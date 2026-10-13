package audit

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
)

// Anchor is a chain head written to write-once storage (ADR-0011): the
// object and the row of audit.anchor say the same thing, and a verification
// compares both with the chain.
type Anchor struct {
	TenantID   uuid.UUID `json:"tenant_id"`
	Seq        int64     `json:"seq"`
	Hash       string    `json:"hash"` // hex of the chain head's hash
	AnchoredAt time.Time `json:"anchored_at"`
}

// Anchorer is the worker that anchors every tenant's chain head whenever it
// has moved since the last anchor, like erp-server runs it. Finding the
// tenants runs as WorkerRole across tenants; each anchor is written in the
// tenant's scope as AppRole: object first (locked for Retention), then the
// row, then commit. A crash between the two leaves an object without a row;
// the next round finds it, checks it says what the chain says, and records
// it.
type Anchorer struct {
	Pool       *database.Pool
	Store      blobstore.Store
	AppRole    string        // erp_app
	WorkerRole string        // erp_worker
	Interval   time.Duration // between rounds; default 1 minute
	Retention  time.Duration // how long anchors are locked; 0 leaves them unlocked
	Log        *slog.Logger
	Now        func() time.Time
	// Begin starts transactions; nil means Pool.Begin. Tests replace it to
	// inject database failures.
	Begin database.BeginFunc
}

func (a *Anchorer) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	return database.BeginOr(ctx, a.Pool, a.Begin, s)
}

func (a *Anchorer) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// pending lists the tenants whose chain head is past their last anchor.
func (a *Anchorer) pending(ctx context.Context) ([]uuid.UUID, error) {
	tx, err := a.begin(ctx, database.Scope{Role: a.WorkerRole, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	rows, _ := tx.Query(ctx, `SELECT h.tenant_id FROM audit.chain_head h
		WHERE h.seq > coalesce((SELECT max(x.seq) FROM audit.anchor x WHERE x.tenant_id = h.tenant_id), 0) ORDER BY h.tenant_id`)
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// ObjectName is the name of a tenant's anchor at seq within KindAnchors.
func ObjectName(seq int64) string { return fmt.Sprintf("%012d.json", seq) }

// anchor writes one tenant's current head.
func (a *Anchorer) anchor(ctx context.Context, tenant uuid.UUID) (Anchor, error) {
	tx, err := a.begin(ctx, database.Scope{Role: a.AppRole, TenantID: tenant})
	if err != nil {
		return Anchor{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var head Anchor
	var hash []byte
	// The head is locked so a round never races a second replica on the same tenant.
	if err := tx.QueryRow(ctx, `SELECT tenant_id, seq, hash FROM audit.chain_head WHERE tenant_id = $1 FOR UPDATE`, tenant).Scan(&head.TenantID, &head.Seq, &hash); err != nil {
		return Anchor{}, err
	}
	head.Hash, head.AnchoredAt = hex.EncodeToString(hash), a.now().UTC().Truncate(time.Second)
	var last int64
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(seq), 0) FROM audit.anchor WHERE tenant_id = $1`, tenant).Scan(&last); err != nil {
		return Anchor{}, err
	}
	if head.Seq <= last {
		return head, nil // anchored by another replica meanwhile
	}
	key := blobstore.Key(tenant, blobstore.KindAnchors, ObjectName(head.Seq))
	body, _ := json.Marshal(head) // ids, numbers and strings
	opts := blobstore.PutOptions{ContentType: "application/json"}
	if a.Retention > 0 {
		opts.RetainUntil = head.AnchoredAt.Add(a.Retention)
	}
	switch err := a.Store.Put(ctx, key, body, opts); {
	case errors.Is(err, blobstore.ErrExists):
		// Written by a round that did not get to commit: it must say what the chain says.
		stored, _, err := a.Store.Get(ctx, key)
		if err != nil {
			return Anchor{}, err
		}
		var prior Anchor
		if json.Unmarshal(stored, &prior) != nil || prior.Hash != head.Hash || prior.Seq != head.Seq {
			return Anchor{}, fmt.Errorf("audit: anchor %s exists with another hash: the chain or the anchor was tampered with", key)
		}
		head.AnchoredAt = prior.AnchoredAt
	case err != nil:
		return Anchor{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit.anchor (tenant_id, seq, hash, object_ref, anchored_at) VALUES ($1, $2, $3, $4, $5)`,
		tenant, head.Seq, hash, key, head.AnchoredAt); err != nil {
		return Anchor{}, err
	}
	return head, tx.Commit(ctx)
}

// RunOnce anchors every tenant whose head moved and returns how many.
func (a *Anchorer) RunOnce(ctx context.Context) (int, error) {
	tenants, err := a.pending(ctx)
	if err != nil {
		return 0, fmt.Errorf("audit anchorer: %w", err)
	}
	n := 0
	var errs []error
	for _, t := range tenants {
		if _, err := a.anchor(ctx, t); err != nil {
			errs = append(errs, fmt.Errorf("audit anchorer: tenant %s: %w", t, err))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// Run anchors until ctx ends; errors are logged and retried next round.
func (a *Anchorer) Run(ctx context.Context) {
	interval := a.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	for {
		if _, err := a.RunOnce(ctx); err != nil && ctx.Err() == nil {
			a.Log.ErrorContext(ctx, "audit anchoring failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// anchorsIn lists a tenant's anchors with seq in [from, to] (to 0: the head).
func anchorsIn(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, from, to int64) ([]storedAnchor, error) {
	rows, _ := tx.Query(ctx, `SELECT a.seq, a.hash, a.object_ref, a.anchored_at, e.hash FROM audit.anchor a
		LEFT JOIN audit.audit_event e ON e.tenant_id = a.tenant_id AND e.seq = a.seq
		WHERE a.tenant_id = $1 AND a.seq >= $2 AND ($3 = 0 OR a.seq <= $3) ORDER BY a.seq`, tenant, from, to)
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (storedAnchor, error) {
		var s storedAnchor
		err := r.Scan(&s.Seq, &s.Hash, &s.ObjectRef, &s.AnchoredAt, &s.EventHash)
		return s, err
	})
}

type storedAnchor struct {
	Seq        int64
	Hash       []byte
	ObjectRef  string
	AnchoredAt time.Time
	EventHash  []byte // the chain's hash at seq; nil when the event is gone
}

// checkAnchors compares every anchor in the range with the chain and, with
// a store, with its object; it returns how many were checked and the first
// sequence that disagrees (0 when all agree).
func checkAnchors(ctx context.Context, tx pgx.Tx, store blobstore.Store, tenant uuid.UUID, from, to int64) (int, int64, error) {
	anchors, err := anchorsIn(ctx, tx, tenant, from, to)
	if err != nil {
		return 0, 0, err
	}
	for i, s := range anchors {
		if !bytes.Equal(s.Hash, s.EventHash) {
			return i, s.Seq, nil
		}
		if store == nil {
			continue
		}
		stored, _, err := store.Get(ctx, s.ObjectRef)
		if errors.Is(err, blobstore.ErrNotFound) {
			return i, s.Seq, nil // a locked object that is gone was removed behind the platform's back: the anchor does not verify
		}
		if err != nil {
			return i, 0, fmt.Errorf("audit: anchor %s: %w", s.ObjectRef, err)
		}
		if !holds(stored, s.Seq, s.Hash) {
			return i, s.Seq, nil
		}
	}
	return len(anchors), 0, nil
}

// holds reports whether a stored anchor object records the sequence and
// hash; an unreadable object does not, which is a finding, not an error.
func holds(stored []byte, seq int64, hash []byte) bool {
	var obj Anchor
	return json.Unmarshal(stored, &obj) == nil && obj.Seq == seq && obj.Hash == hex.EncodeToString(hash)
}

// lastAnchor is the sequence of the tenant's newest anchor, 0 without one.
func lastAnchor(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) (int64, error) {
	var seq int64
	err := tx.QueryRow(ctx, `SELECT coalesce(max(seq), 0) FROM audit.anchor WHERE tenant_id = $1`, tenant).Scan(&seq)
	return seq, err
}
