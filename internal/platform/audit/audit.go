// Package audit writes the tamper-evident audit trail (ADR-0011): every
// event extends its tenant's SHA-256 hash chain, in the same transaction as
// the change it records. Handlers add entries during the request; the unit of
// work flushes them last, so the tenant's chain-head lock is held only for
// the end of the transaction (ADR-0014).
package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Entry is one audited action.
type Entry struct {
	Action     string    // dotted verb, e.g. platform.operation.cancel
	ObjectType string    // e.g. platform.operation
	ObjectID   uuid.UUID // uuid.Nil when the action has no single object
	Outcome    string    // e.g. succeeded
	Detail     map[string]any
}

// Buffer collects a request's entries until the unit of work flushes them.
type Buffer struct{ entries []Entry }

type bufferKey struct{}

// WithBuffer attaches an empty buffer to the context.
func WithBuffer(ctx context.Context) (context.Context, *Buffer) {
	b := &Buffer{}
	return context.WithValue(ctx, bufferKey{}, b), b
}

// ErrNoBuffer means Add was called outside a unit of work.
var ErrNoBuffer = errors.New("audit: no buffer in context (outside a unit of work)")

// Add records an entry for the current request.
func Add(ctx context.Context, e Entry) error {
	b, ok := ctx.Value(bufferKey{}).(*Buffer)
	if !ok {
		return ErrNoBuffer
	}
	b.entries = append(b.entries, e)
	return nil
}

// Len is the number of buffered entries.
func (b *Buffer) Len() int { return len(b.entries) }

// Flush writes the buffered entries as the given actor and empties the buffer.
func (b *Buffer) Flush(ctx context.Context, tx pgx.Tx, actor reqctx.Principal, traceID string, now time.Time) error {
	if len(b.entries) == 0 {
		return nil
	}
	err := Write(ctx, tx, actor, traceID, now, b.entries...)
	b.entries = nil
	return err
}

// record is the hashed form of an event; field order is fixed.
type record struct {
	TenantID   uuid.UUID       `json:"tenant_id"`
	Seq        int64           `json:"seq"`
	OccurredAt string          `json:"occurred_at"`
	ActorID    uuid.UUID       `json:"actor_id"`
	ActorKind  string          `json:"actor_kind"`
	Action     string          `json:"action"`
	ObjectType string          `json:"object_type"`
	ObjectID   string          `json:"object_id"`
	Outcome    string          `json:"outcome"`
	Detail     json.RawMessage `json:"detail"`
	TraceID    string          `json:"trace_id"`
}

const timeLayout = "2006-01-02T15:04:05.000000Z"

func (r record) hash(prev []byte) []byte {
	b, _ := json.Marshal(r) // only strings, ids, numbers and normalized JSON
	h := sha256.New()
	h.Write(prev)
	h.Write(b)
	return h.Sum(nil)
}

// normalize gives detail one canonical encoding, so the hash computed now
// matches the one recomputed from the stored jsonb.
func normalize(detail map[string]any) (json.RawMessage, error) {
	if detail == nil {
		return json.RawMessage("{}"), nil
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return nil, err
	}
	return canonical(b), nil
}

// canonical re-encodes valid JSON (from json.Marshal or jsonb) through Go
// values, so numbers and key order always come out the same way.
func canonical(valid []byte) json.RawMessage {
	var v any
	_ = json.Unmarshal(valid, &v)
	out, _ := json.Marshal(v)
	return out
}

// Write appends entries to the actor's tenant chain.
func Write(ctx context.Context, tx pgx.Tx, actor reqctx.Principal, traceID string, now time.Time, entries ...Entry) error {
	// Create the head on first use and lock it: concurrent writers in the
	// tenant queue here until this transaction ends.
	var seq int64
	var prev []byte
	if err := tx.QueryRow(ctx, `INSERT INTO audit.chain_head AS h (tenant_id, seq, hash) VALUES ($1, 0, $2)
		ON CONFLICT (tenant_id) DO UPDATE SET tenant_id = h.tenant_id RETURNING h.seq, h.hash`,
		actor.TenantID, make([]byte, sha256.Size)).Scan(&seq, &prev); err != nil {
		return fmt.Errorf("audit: lock chain head: %w", err)
	}
	if seq == 0 {
		prev = nil // the first event has no predecessor
	}
	at := now.UTC().Truncate(time.Microsecond)
	for _, e := range entries {
		detail, err := normalize(e.Detail)
		if err != nil {
			return fmt.Errorf("audit: %s: detail: %w", e.Action, err)
		}
		seq++
		r := record{TenantID: actor.TenantID, Seq: seq, OccurredAt: at.Format(timeLayout), ActorID: actor.ID, ActorKind: actor.Kind,
			Action: e.Action, ObjectType: e.ObjectType, Outcome: e.Outcome, Detail: detail, TraceID: traceID}
		var objectID *uuid.UUID
		if e.ObjectID != uuid.Nil {
			r.ObjectID = e.ObjectID.String()
			objectID = &e.ObjectID
		}
		h := r.hash(prev)
		if _, err := tx.Exec(ctx, `INSERT INTO audit.audit_event (tenant_id, occurred_at, seq, actor_id, actor_kind, action, object_type,
			object_id, outcome, detail, trace_id, prev_hash, hash) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,nullif($11, ''),$12,$13)`,
			r.TenantID, at, seq, r.ActorID, r.ActorKind, r.Action, r.ObjectType, objectID, r.Outcome, string(detail), traceID, prev, h); err != nil {
			return fmt.Errorf("audit: %s: %w", e.Action, err)
		}
		prev = h
	}
	if _, err := tx.Exec(ctx, `UPDATE audit.chain_head SET seq = $2, hash = $3, updated_at = now() WHERE tenant_id = $1`,
		actor.TenantID, seq, prev); err != nil {
		return fmt.Errorf("audit: advance chain head: %w", err)
	}
	return nil
}

// Result reports a chain verification.
type Result struct {
	Checked  int64 // events checked
	BrokenAt int64 // first sequence number that does not verify; 0 when intact
}

// Verify recomputes the tenant's chain from the first event and checks it
// against the chain head. One query returns the head with the events, so a
// chain whose events were deleted, even all of them, is reported broken.
func Verify(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) (Result, error) {
	rows, _ := tx.Query(ctx, `SELECT coalesce(h.seq, 0), h.hash, e.seq, e.occurred_at, e.actor_id, e.actor_kind, e.action,
		e.object_type, coalesce(e.object_id::text, ''), e.outcome, e.detail::text, coalesce(e.trace_id, ''), e.prev_hash, e.hash
		FROM (SELECT 1) one
		LEFT JOIN audit.chain_head h ON h.tenant_id = $1
		LEFT JOIN audit.audit_event e ON e.tenant_id = $1
		ORDER BY e.seq`, tenant)
	var (
		res                    Result
		prev                   []byte
		headSeq                int64
		headHash               []byte
		seq                    *int64
		at                     *time.Time
		actorID                *uuid.UUID
		kind, action, objType  *string
		objID, outcome, detail *string
		traceID                *string
		storedPrev, storedHash []byte
	)
	_, err := pgx.ForEachRow(rows, []any{&headSeq, &headHash, &seq, &at, &actorID, &kind, &action, &objType, &objID,
		&outcome, &detail, &traceID, &storedPrev, &storedHash}, func() error {
		if seq == nil || res.BrokenAt != 0 {
			return nil // no events, or already broken
		}
		res.Checked++
		d := canonical([]byte(*detail))
		r := record{TenantID: tenant, Seq: *seq, OccurredAt: at.UTC().Format(timeLayout), ActorID: *actorID, ActorKind: *kind,
			Action: *action, ObjectType: *objType, ObjectID: *objID, Outcome: *outcome, Detail: d, TraceID: *traceID}
		if r.Seq != res.Checked || !bytes.Equal(storedPrev, prev) || !bytes.Equal(storedHash, r.hash(prev)) {
			res.BrokenAt = r.Seq
			return nil
		}
		prev = storedHash
		return nil
	})
	if err != nil {
		return Result{}, fmt.Errorf("audit: verify: %w", err)
	}
	if res.BrokenAt == 0 && (headSeq != res.Checked || (res.Checked > 0 && !bytes.Equal(headHash, prev))) {
		// Events after the last verified one are missing (deleted or never written).
		res.BrokenAt = res.Checked + 1
	}
	return res, nil
}
