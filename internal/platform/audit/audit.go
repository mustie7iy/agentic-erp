// Package audit writes the tamper-evident audit trail (ADR-0011): every
// event extends its tenant's SHA-256 hash chain, in the same transaction as
// the change it records. Handlers add entries during the request; the unit of
// work flushes them last, so the tenant's chain-head lock is held only for
// the end of the transaction (ADR-0014). Record pairs a change's outbox
// events (ADR-0007) with its entry, so audit depends on outbox; outbox must
// never import audit.
package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
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
// refusals are entries that outlive the request's work: a write refused
// rolls back, and its refusal is still recorded (Refuse).
type Buffer struct{ entries, refusals []Entry }

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

// Record publishes a change's events through the outbox (ADR-0007) and adds
// its succeeded audit entry, both in the request's transaction: the record
// every context's application services write for a state change.
func Record(ctx context.Context, action, objectType string, id uuid.UUID, detail map[string]any, events ...outbox.Event) error {
	for _, e := range events {
		if _, err := outbox.Add(ctx, e); err != nil {
			return fmt.Errorf("audit: record %s: %w", action, err)
		}
	}
	return Add(ctx, Entry{Action: action, ObjectType: objectType, ObjectID: id, Outcome: "succeeded", Detail: detail})
}

// ObjectTypeOf is the object type an action names, the action without its
// verb: payments.payment_run.release is about payments.payment_run. An
// action without a verb names its own type.
func ObjectTypeOf(action string) string {
	if i := strings.LastIndex(action, "."); i >= 0 {
		return action[:i]
	}
	return action
}

// Refuse records the refusal of a write, such as a validation rule's
// (ADR-0046): an entry the unit of work keeps when it rolls the request's
// work back for its business outcome, as well as when it succeeds.
func Refuse(ctx context.Context, e Entry) error {
	b, ok := ctx.Value(bufferKey{}).(*Buffer)
	if !ok {
		return ErrNoBuffer
	}
	b.refusals = append(b.refusals, e)
	return nil
}

// Discard drops the entries buffered so far, refusals included, for work
// a caller rolls back to its own savepoint as though it never ran (a
// rehearsal); the unit of work keeps the refusals of a request it rolls
// back for its business outcome (Buffer.FlushRefusals).
func Discard(ctx context.Context) {
	if b, ok := ctx.Value(bufferKey{}).(*Buffer); ok {
		b.entries, b.refusals = nil, nil
	}
}

// Len is the number of buffered entries, refusals included.
func (b *Buffer) Len() int { return len(b.entries) + len(b.refusals) }

// Flush writes the buffered entries and refusals as the given actor and
// empties the buffer.
func (b *Buffer) Flush(ctx context.Context, tx pgx.Tx, actor reqctx.Principal, traceID string, now time.Time) error {
	all := slices.Concat(b.entries, b.refusals)
	b.entries, b.refusals = nil, nil
	if len(all) == 0 {
		return nil
	}
	return Write(ctx, tx, actor, traceID, now, all...)
}

// FlushRefusals writes only the refusals, for a request whose work was
// rolled back, and empties the buffer.
func (b *Buffer) FlushRefusals(ctx context.Context, tx pgx.Tx, actor reqctx.Principal, traceID string, now time.Time) error {
	b.entries = nil
	return b.Flush(ctx, tx, actor, traceID, now)
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
		return nil, fmt.Errorf("encode: %w", err)
	}
	return canonical(b), nil
}

// canonical re-encodes valid JSON (from json.Marshal or jsonb) through Go
// values, so numbers and key order always come out the same way.
func canonical(valid []byte) json.RawMessage {
	var v any
	_ = json.Unmarshal(valid, &v) // callers pass valid JSON: json.Marshal output or jsonb
	out, _ := json.Marshal(v)     // values decoded from JSON always encode
	return out
}

// Personal data never belongs in an audit detail (ADR-0026): details carry
// identifiers, statuses and amounts, never what the identifiers point to.
// The convention is checked at write time: a detail whose key names
// personal data, or whose value looks like a bank account number or an
// email address, is refused, so the rule cannot drift unnoticed.
var personalKeys = map[string]bool{"iban": true, "account_number": true, "email": true, "phone": true, "holder_name": true,
	"tax_id": true, "vat_id": true, "date_of_birth": true, "street": true, "address": true}

var (
	ibanLike  = regexp.MustCompile(`^[A-Z]{2}[0-9]{2}[A-Z0-9]{11,30}$`)
	emailLike = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)
)

// Personal reports the path of the first detail field that carries
// personal data, by key or by the look of its value.
func Personal(detail map[string]any) (string, bool) {
	return personal("", detail)
}

func personal(path string, v any) (string, bool) {
	switch x := v.(type) {
	case map[string]any:
		return personalField(path, x)
	case []any:
		for i, child := range x {
			if found, ok := personal(fmt.Sprintf("%s[%d]", path, i), child); ok {
				return found, true
			}
		}
	case string:
		s := strings.ReplaceAll(x, " ", "")
		if ibanLike.MatchString(s) || emailLike.MatchString(x) {
			return path, true
		}
	}
	return "", false
}

// personalField is personal for an object: a key that names personal
// data, or a value that carries it.
func personalField(path string, x map[string]any) (string, bool) {
	for k, child := range x {
		p := k
		if path != "" {
			p = path + "." + k
		}
		if personalKeys[strings.ToLower(k)] {
			return p, true
		}
		if found, ok := personal(p, child); ok {
			return found, true
		}
	}
	return "", false
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
		if field, found := Personal(e.Detail); found {
			return fmt.Errorf("audit: %s: detail %s carries personal data (ADR-0026)", e.Action, field)
		}
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
	LastSeq  int64 // last sequence number checked
	BrokenAt int64 // first sequence number that does not verify; 0 when intact
}

// Verify recomputes the tenant's chain from the first event and checks it
// against the chain head. One query returns the head with the events, so a
// chain whose events were deleted, even all of them, is reported broken.
func Verify(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) (Result, error) {
	return VerifyRange(ctx, tx, tenant, 1, 0)
}

// VerifyRange recomputes the events from..to (to 0: up to the head). The
// first event's stored predecessor hash is trusted, so a range starts from
// a point verified before; a range that reaches the head is checked
// against it as well.
func VerifyRange(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, from, to int64) (Result, error) {
	rows, _ := tx.Query(ctx, `SELECT coalesce(h.seq, 0), h.hash, e.seq, e.occurred_at, e.actor_id, e.actor_kind, e.action,
		e.object_type, coalesce(e.object_id::text, ''), e.outcome, e.detail::text, coalesce(e.trace_id, ''), e.prev_hash, e.hash
		FROM (SELECT 1) one
		LEFT JOIN audit.chain_head h ON h.tenant_id = $1
		LEFT JOIN audit.audit_event e ON e.tenant_id = $1 AND e.seq >= $2 AND ($3 = 0 OR e.seq <= $3)
		ORDER BY e.seq`, tenant, from, to)
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
		if res.Checked == 0 && from > 1 {
			prev = storedPrev // the range starts from a verified point
		}
		res.Checked++
		d := canonical([]byte(*detail))
		r := record{TenantID: tenant, Seq: *seq, OccurredAt: at.UTC().Format(timeLayout), ActorID: *actorID, ActorKind: *kind,
			Action: *action, ObjectType: *objType, ObjectID: *objID, Outcome: *outcome, Detail: d, TraceID: *traceID}
		if r.Seq != from+res.Checked-1 || !bytes.Equal(storedPrev, prev) || !bytes.Equal(storedHash, r.hash(prev)) {
			res.BrokenAt = r.Seq
			return nil
		}
		prev = storedHash
		return nil
	})
	if err != nil {
		return Result{}, fmt.Errorf("audit: verify: %w", err)
	}
	res.LastSeq = from + res.Checked - 1
	last := to
	if last == 0 {
		last = headSeq
	}
	switch {
	case res.BrokenAt != 0:
	case res.LastSeq != last, to == 0 && res.Checked > 0 && !bytes.Equal(headHash, prev):
		// Events after the last verified one are missing (deleted or never written).
		res.BrokenAt = res.LastSeq + 1
	}
	return res, nil
}

// Head is the head of a tenant's chain: its last sequence number and hash.
type Head struct {
	Seq  int64
	Hash []byte
}

// ReadHead reads the tenant's chain head; ok is false when the tenant has
// no chain yet.
func ReadHead(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) (Head, bool, error) {
	var h Head
	switch err := tx.QueryRow(ctx, `SELECT seq, hash FROM audit.chain_head WHERE tenant_id = $1`, tenant).Scan(&h.Seq, &h.Hash); {
	case errors.Is(err, pgx.ErrNoRows):
		return Head{}, false, nil
	case err != nil:
		return Head{}, false, fmt.Errorf("audit: read the chain head of tenant %s: %w", tenant, err)
	}
	return h, true, nil
}
