// Package drverify checks a restored cell before it serves again
// (ADR-0040, T41d): the schema at the expected migration version, every
// tenant's audit hash chain intact (ADR-0011), every journal entry balanced
// and every legal entity's trial balance at zero (ADR-0005), every tenant's
// per-table counts (ADR-0030) compared with the old database's when it is
// kept, and the restore drill's markers: the one written before the restore
// point present, the one written after it absent. It reads only; the
// restored database is the one under test, so nothing is repaired here.
package drverify

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/offboarding"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Report is a verification's outcome. Problems is empty when the restored
// cell passed; an error from Verify means the checks could not run.
type Report struct {
	SchemaVersion  int                       `json:"schema_version"`
	WantVersion    int                       `json:"want_version"`
	Tenants        int                       `json:"tenants"`
	JournalEntries int64                     `json:"journal_entries"`
	Fingerprints   []offboarding.Fingerprint `json:"fingerprints"`
	Restored       *Mark                     `json:"restored_marker,omitempty"` // the marker the restore kept, as found
	Problems       []string                  `json:"problems"`
}

// Passed reports whether every check held.
func (r Report) Passed() bool { return len(r.Problems) == 0 }

func (r *Report) problem(format string, args ...any) {
	r.Problems = append(r.Problems, fmt.Sprintf(format, args...))
}

// Verifier checks the database behind Pool.
type Verifier struct {
	Pool  *database.Pool
	Begin database.BeginFunc // nil: Pool.Begin; tests inject failures
	// Old is the database the cell ran on before, when it is kept (a
	// restore in place, a promoted replica): every tenant's fingerprint is
	// compared with its own there. Nil skips the comparison.
	Old      *database.Pool
	OldBegin database.BeginFunc
	// AppRole reads a tenant's data in its scope; ControlRole lists the
	// tenants (erp_app and erp_control).
	AppRole, ControlRole string
	Want                 int // the migration version the binary was built for
	// Restored is the drill marker written before the restore point, which
	// must be there with the drill tenant's fingerprint as it was then;
	// Lost the one written after it, which must not. Nil: not a drill.
	Restored, Lost *Mark
	Now            func() time.Time
}

// Verify runs every check and reports what failed.
func (v *Verifier) Verify(ctx context.Context) (Report, error) {
	r := Report{WantVersion: v.Want, Fingerprints: []offboarding.Fingerprint{}, Problems: []string{}}
	tenants, err := v.cell(ctx, &r)
	if err != nil {
		return Report{}, err
	}
	for _, t := range tenants {
		if err := v.tenant(ctx, t, &r); err != nil {
			return Report{}, err
		}
	}
	if err := v.gone(ctx, tenants, &r); err != nil {
		return Report{}, err
	}
	if err := v.markers(ctx, &r); err != nil {
		return Report{}, err
	}
	return r, nil
}

// cell reads the schema version as the login role (which owns the
// migrations table) and the tenants as the control role.
func (v *Verifier) cell(ctx context.Context, r *Report) ([]uuid.UUID, error) {
	err := v.read(ctx, database.Scope{ReadOnly: true}, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM public.erp_schema_migration`).Scan(&r.SchemaVersion)
	})
	if err != nil {
		return nil, fmt.Errorf("drverify: schema version: %w", err)
	}
	if r.SchemaVersion != v.Want {
		r.problem("schema version %d, want %d", r.SchemaVersion, v.Want)
	}
	var ids []uuid.UUID
	err = v.read(ctx, database.Scope{Role: v.ControlRole, ReadOnly: true}, func(tx pgx.Tx) error {
		tenants, err := tenancy.AllTenants(ctx, tx)
		for _, t := range tenants {
			ids = append(ids, t.ID)
		}
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("drverify: list tenants: %w", err)
	}
	r.Tenants = len(ids)
	return ids, nil
}

// gone reports every tenant of the old database the restored one lacks:
// the largest loss a restore can have, which the restored database alone
// cannot show.
func (v *Verifier) gone(ctx context.Context, restored []uuid.UUID, r *Report) error {
	if v.Old == nil {
		return nil
	}
	tx, err := database.BeginOr(ctx, v.Old, v.OldBegin, database.Scope{Role: v.ControlRole, ReadOnly: true})
	if err != nil {
		return fmt.Errorf("drverify: list the old database's tenants: begin: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) // read-only: ending it cannot lose anything
	old, err := tenancy.AllTenants(ctx, tx)
	if err != nil {
		return fmt.Errorf("drverify: list the old database's tenants: %w", err)
	}
	have := map[uuid.UUID]bool{}
	for _, id := range restored {
		have[id] = true
	}
	for _, t := range old {
		if !have[t.ID] {
			r.problem("tenant %s: the restored database has no tenant with this id; the old database has it", t.ID)
		}
	}
	return nil
}

// tenant checks one tenant in its scope. The transaction is read
// committed; a restored database is quiescent while it is verified, so the
// statements see one state.
func (v *Verifier) tenant(ctx context.Context, id uuid.UUID, r *Report) error {
	var fp offboarding.Fingerprint
	err := v.read(ctx, v.scope(id), func(tx pgx.Tx) error {
		res, err := audit.Verify(ctx, tx, id)
		if err != nil {
			return err
		}
		if res.BrokenAt != 0 {
			r.problem("tenant %s: audit chain broken at seq %d", id, res.BrokenAt)
		}
		if err := ledger(ctx, tx, id, r); err != nil {
			return err
		}
		fp, err = offboarding.TakeFingerprint(ctx, tx, id, clock.Now(v.Now))
		return err
	})
	if err != nil {
		return fmt.Errorf("drverify: tenant %s: %w", id, err)
	}
	r.Fingerprints = append(r.Fingerprints, fp)
	if v.Old == nil {
		return nil
	}
	old, err := v.oldFingerprint(ctx, id)
	if err != nil {
		return fmt.Errorf("drverify: tenant %s in the old database: %w", id, err)
	}
	for _, d := range offboarding.Differences("the restored database", &fp, "the old database", old) {
		r.problem("tenant %s: %s", id, d)
	}
	return nil
}

// oldFingerprint is the tenant's fingerprint in the old database; nil when
// the tenant is not there.
func (v *Verifier) oldFingerprint(ctx context.Context, id uuid.UUID) (*offboarding.Fingerprint, error) {
	tx, err := database.BeginOr(ctx, v.Old, v.OldBegin, v.scope(id))
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) // read-only: ending it cannot lose anything
	var present bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM tenancy.tenant WHERE id = $1)`, id).Scan(&present); err != nil {
		return nil, fmt.Errorf("find tenant: %w", err)
	}
	if !present {
		return nil, nil
	}
	fp, err := offboarding.TakeFingerprint(ctx, tx, id, clock.Now(v.Now))
	if err != nil {
		return nil, fmt.Errorf("fingerprint: %w", err)
	}
	return &fp, nil
}

func (v *Verifier) scope(id uuid.UUID) database.Scope {
	return database.Scope{Role: v.AppRole, TenantID: id, ReadOnly: true}
}

// read runs fn in a read-only transaction of the restored database, which
// it rolls back: nothing here writes.
func (v *Verifier) read(ctx context.Context, s database.Scope, fn func(pgx.Tx) error) error {
	tx, err := database.BeginOr(ctx, v.Pool, v.Begin, s)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) // read-only: ending it cannot lose anything
	return fn(tx)
}
