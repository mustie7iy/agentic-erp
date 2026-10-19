package drverify

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
)

// tenantChain is what the restored database says of a tenant's anchors.
type tenantChain struct {
	head int64           // the chain head's seq; 0 without events
	refs map[string]bool // the anchors' objects it records
	torn []int64         // anchors whose hash is not the event's
}

// anchors checks the tenant's restored chain against its anchors in the
// object store (ADR-0011, ADR-0025): every anchor the database records
// agrees with its event and has its object, and every object the database
// does not record (anchored after the restore point) agrees with the event
// at its seq when the restore kept that event. The objects are locked
// (ADR-0016), so one the database records says what the database's row
// says when it was written: the row is compared with the event, and the
// object needs only to exist. The objects the database does not record are
// read.
func (rc *Reconciler) anchors(ctx context.Context, store blobstore.Eraser, tenant uuid.UUID, rep *Reconciled) error {
	keys, err := store.List(ctx, blobstore.Key(tenant, blobstore.KindAnchors, ""))
	if err != nil {
		return fmt.Errorf("drverify: anchors of tenant %s: %w", tenant, err)
	}
	c, err := rc.chain(ctx, tenant)
	if err != nil {
		return err
	}
	for _, seq := range c.torn {
		rep.problem("tenant %s: anchor at seq %d disagrees with the restored chain", tenant, seq)
	}
	for ref := range c.refs {
		if !slices.Contains(keys, ref) {
			rep.problem("tenant %s: anchor %s is not in the object store", tenant, ref)
		}
	}
	for _, key := range keys {
		if c.refs[key] {
			rep.AnchorsChecked++
			continue
		}
		if err := rc.anchorObject(ctx, tenant, key, c, rep); err != nil {
			return err
		}
	}
	return nil
}

// anchorObject compares an anchor object the database does not record with
// the restored chain.
func (rc *Reconciler) anchorObject(ctx context.Context, tenant uuid.UUID, key string, c tenantChain, rep *Reconciled) error {
	raw, _, err := rc.Store.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("drverify: anchor %s: %w", key, err)
	}
	a, ok := anchorOf(raw, tenant)
	if !ok {
		rep.problem("tenant %s: %s is not an anchor of the tenant", tenant, key)
		return nil
	}
	if a.Seq > c.head {
		rep.AnchorsAfter++ // of events the restore lost: expected after a point in time
		return nil
	}
	hash, err := rc.eventHash(ctx, tenant, a.Seq)
	if err != nil {
		return err
	}
	if want, _ := hex.DecodeString(a.Hash); !bytes.Equal(hash, want) || hash == nil { // a hash that is not hex decodes short: a mismatch
		rep.problem("tenant %s: the object store's anchor at seq %d disagrees with the restored chain", tenant, a.Seq)
		return nil
	}
	rep.AnchorsChecked++
	return nil
}

// anchorOf reads an anchor object of the tenant; an unreadable one, or
// another tenant's, is not one: a finding, not an error.
func anchorOf(raw []byte, tenant uuid.UUID) (audit.Anchor, bool) {
	var a audit.Anchor
	return a, json.Unmarshal(raw, &a) == nil && a.TenantID == tenant
}

// chain reads the tenant's chain head and recorded anchors, and the
// anchors whose hash is not their event's.
func (rc *Reconciler) chain(ctx context.Context, tenant uuid.UUID) (tenantChain, error) {
	c := tenantChain{refs: map[string]bool{}}
	err := rc.Verifier.read(ctx, rc.Verifier.scope(tenant), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT seq FROM audit.chain_head WHERE tenant_id = $1), 0)`, tenant).Scan(&c.head); err != nil {
			return fmt.Errorf("chain head: %w", err)
		}
		rows, _ := tx.Query(ctx, `SELECT a.object_ref, a.seq, e.hash IS DISTINCT FROM a.hash FROM audit.anchor a
			LEFT JOIN audit.audit_event e ON e.tenant_id = a.tenant_id AND e.seq = a.seq WHERE a.tenant_id = $1 ORDER BY a.seq`, tenant)
		var (
			ref  string
			seq  int64
			torn bool
		)
		_, err := pgx.ForEachRow(rows, []any{&ref, &seq, &torn}, func() error {
			c.refs[ref] = true
			if torn {
				c.torn = append(c.torn, seq)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("anchors: %w", err)
		}
		return nil
	})
	if err != nil {
		return tenantChain{}, fmt.Errorf("drverify: chain of tenant %s: %w", tenant, err)
	}
	return c, nil
}

// eventHash is the restored chain's hash at seq; nil when the event is gone.
func (rc *Reconciler) eventHash(ctx context.Context, tenant uuid.UUID, seq int64) ([]byte, error) {
	var hash []byte
	err := rc.Verifier.read(ctx, rc.Verifier.scope(tenant), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT (SELECT hash FROM audit.audit_event WHERE tenant_id = $1 AND seq = $2)`, tenant, seq).Scan(&hash)
	})
	if err != nil {
		return nil, fmt.Errorf("drverify: event %d of tenant %s: %w", seq, tenant, err)
	}
	return hash, nil
}
