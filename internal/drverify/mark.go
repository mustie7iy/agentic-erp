package drverify

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/offboarding"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// A restore drill's markers are audit events of its tenant: written in the
// tenant's chain, they are restored, exported and verified with it, and
// need no table of their own. The drill writes one before the restore
// point and one after; the restored cell must hold exactly the first.

// MarkAction is the audit action of a drill marker.
const MarkAction = "platform.restore_drill.mark"

// DrillActor is the system actor that writes markers.
var DrillActor = uuid.MustParse("0d5c7a1e-41d0-4e7b-9a0d-d2111e5e0041")

// Mark is a written marker: the tenant, its name, when it was written and
// the tenant's fingerprint (ADR-0030) as the marker's transaction left it.
type Mark struct {
	TenantID    uuid.UUID                `json:"tenant_id"`
	Name        string                   `json:"name"`
	At          time.Time                `json:"at"`
	Fingerprint *offboarding.Fingerprint `json:"fingerprint,omitempty"`
}

// Mark writes a marker in the tenant's audit chain and returns it with the
// tenant's fingerprint taken in the same transaction. The transaction is
// read committed, each count its own snapshot: the drill tenant must be
// idle while it is marked, or the fingerprint matches no state.
func (v *Verifier) Mark(ctx context.Context, tenant uuid.UUID, name string) (Mark, error) {
	if name == "" {
		return Mark{}, errors.New("drverify: a marker needs a name")
	}
	tx, err := database.BeginOr(ctx, v.Pool, v.Begin, database.Scope{Role: v.AppRole, TenantID: tenant})
	if err != nil {
		return Mark{}, fmt.Errorf("drverify: mark %s: %w", name, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) // after Commit a no-op; before it, the marker is not written
	at := clock.Now(v.Now).UTC().Truncate(time.Microsecond)
	actor := reqctx.Principal{ID: DrillActor, Kind: reqctx.KindSystem, TenantID: tenant}
	if err := audit.Write(ctx, tx, actor, "", at, audit.Entry{Action: MarkAction, ObjectType: "platform.restore_drill",
		Outcome: "succeeded", Detail: map[string]any{"name": name}}); err != nil {
		return Mark{}, fmt.Errorf("drverify: mark %s: %w", name, err)
	}
	fp, err := offboarding.TakeFingerprint(ctx, tx, tenant, at)
	if err != nil {
		return Mark{}, fmt.Errorf("drverify: mark %s: %w", name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Mark{}, fmt.Errorf("drverify: mark %s: commit: %w", name, err)
	}
	return Mark{TenantID: tenant, Name: name, At: at, Fingerprint: &fp}, nil
}

// markers checks the drill's markers: Restored present, with the drill
// tenant's fingerprint as it was written; Lost absent.
func (v *Verifier) markers(ctx context.Context, r *Report) error {
	if v.Restored != nil {
		at, found, err := v.find(ctx, *v.Restored)
		if err != nil {
			return err
		}
		if !found {
			r.problem("marker %q, written before the restore point, is missing: the restore lost data it should hold", v.Restored.Name)
		} else {
			r.Restored = &Mark{TenantID: v.Restored.TenantID, Name: v.Restored.Name, At: at}
			v.compare(*v.Restored, r)
		}
	}
	if v.Lost != nil {
		_, found, err := v.find(ctx, *v.Lost)
		if err != nil {
			return err
		}
		if found {
			r.problem("marker %q, written after the restore point, is present: the restore went past its point", v.Lost.Name)
		}
	}
	return nil
}

// compare names the differences between the drill tenant's fingerprint now
// and the one written with the marker.
func (v *Verifier) compare(m Mark, r *Report) {
	if m.Fingerprint == nil {
		return
	}
	var now *offboarding.Fingerprint
	for i := range r.Fingerprints {
		if r.Fingerprints[i].TenantID == m.TenantID {
			now = &r.Fingerprints[i]
		}
	}
	for _, d := range offboarding.Differences("the restored database", now, "marker "+m.Name, m.Fingerprint) {
		r.problem("drill tenant %s: %s", m.TenantID, d)
	}
}

// find reads when the marker was written; found is false when the restored
// chain does not hold it. A marker with its time is that very event: a
// drill tenant holds the markers of every earlier drill, names repeated.
func (v *Verifier) find(ctx context.Context, m Mark) (time.Time, bool, error) {
	var (
		at      time.Time
		written *time.Time
	)
	if !m.At.IsZero() {
		written = &m.At
	}
	err := v.read(ctx, v.scope(m.TenantID), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT occurred_at FROM audit.audit_event
			WHERE tenant_id = $1 AND action = $2 AND detail->>'name' = $3 AND ($4::timestamptz IS NULL OR occurred_at = $4)
			ORDER BY seq LIMIT 1`, m.TenantID, MarkAction, m.Name, written).Scan(&at)
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return time.Time{}, false, nil
	case err != nil:
		return time.Time{}, false, fmt.Errorf("drverify: find marker %s: %w", m.Name, err)
	}
	return at.UTC(), true, nil
}
