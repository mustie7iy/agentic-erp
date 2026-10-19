package drverify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/controlplane"
	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/offboarding"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Actor is the reconciliation's id in audit trails: a system actor.
var Actor = uuid.NewSHA1(uuid.NameSpaceURL, []byte("urn:agentic-erp:dr-reconcile"))

// Reconciled is a reconciliation's outcome. Problems is empty when the
// restored cell agrees with the object store and the directory.
type Reconciled struct {
	Tenants        int         `json:"tenants"`                     // in the restored cell
	AnchorsChecked int         `json:"anchors_checked"`             // anchors that agree with the restored chains
	AnchorsAfter   int         `json:"anchors_after_restore_point"` // anchors of events the restore lost
	Directory      int         `json:"directory_entries"`           // entries the directory places in the cell; 0 without a directory
	Offboarded     []uuid.UUID `json:"offboarded"`                  // offboarded again: the directory had them offboarded or erased
	Erased         []uuid.UUID `json:"erased"`                      // erased again: the directory had them erased
	Problems       []string    `json:"problems"`
}

// Passed reports whether the cell agrees with the store and the directory.
func (r Reconciled) Passed() bool { return len(r.Problems) == 0 }

func (r *Reconciled) problem(format string, args ...any) {
	r.Problems = append(r.Problems, fmt.Sprintf(format, args...))
}

// CellDirectory lists the entries the directory places in a cell
// (directory.Postgres).
type CellDirectory interface {
	CellTenants(ctx context.Context, cell string) ([]directory.Entry, error)
}

// Reconciler brings a restored cell in line with what outlives a restore
// (ADR-0040, T41f), after Verify and before the cell serves: it checks the
// chains against their anchors in the object store, checks that every
// tenant the directory places in the cell is in it, and offboards and
// erases again the tenants the directory has offboarded or erased since
// the restore point. Unlike Verify it writes, through the control plane
// and the offboarding worker, so each change is audited and published.
type Reconciler struct {
	Verifier    *Verifier       // reads the restored database: Pool, roles, Begin
	Store       blobstore.Store // the cell's object store; it must list (blobstore.Eraser)
	Directory   CellDirectory   // nil: a single cell, no directory
	Cell        string          // the cell's name in the directory
	Control     *controlplane.Service
	Offboarding *offboarding.Worker
}

// operator is who the control plane records the reconciliation as.
var operator = controlplane.Operator{ID: Actor, Issuer: "erp-server", Subject: "dr reconcile"}

// Reconcile checks and repairs the restored cell and reports what it did.
func (rc *Reconciler) Reconcile(ctx context.Context) (Reconciled, error) {
	store, ok := rc.Store.(blobstore.Eraser)
	if !ok {
		return Reconciled{}, errors.New("drverify: reconcile needs an object store that lists (a directory or S3)")
	}
	rep := Reconciled{Offboarded: []uuid.UUID{}, Erased: []uuid.UUID{}, Problems: []string{}}
	tenants, err := rc.tenants(ctx)
	if err != nil {
		return Reconciled{}, err
	}
	rep.Tenants = len(tenants)
	// In id order, so two runs report alike.
	for _, id := range slices.SortedFunc(maps.Keys(tenants), func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) }) {
		if err := rc.anchors(ctx, store, id, &rep); err != nil {
			return Reconciled{}, err
		}
	}
	if rc.Directory == nil {
		return rep, nil
	}
	entries, err := rc.Directory.CellTenants(ctx, rc.Cell)
	if err != nil {
		return Reconciled{}, fmt.Errorf("drverify: %w", err)
	}
	rep.Directory = len(entries)
	for _, e := range entries {
		t, ok := tenants[e.TenantID]
		if !ok {
			rep.problem("tenant %s: the directory places it in cell %s, which lacks it (provisioned after the restore point?)", e.TenantID, rc.Cell)
			continue
		}
		rc.follow(ctx, e.Status, t, &rep)
	}
	return rep, nil
}

// tenants reads the restored cell's tenants by id.
func (rc *Reconciler) tenants(ctx context.Context) (map[uuid.UUID]tenancy.Tenant, error) {
	byID := map[uuid.UUID]tenancy.Tenant{}
	err := rc.Verifier.read(ctx, database.Scope{Role: rc.Verifier.ControlRole, ReadOnly: true}, func(tx pgx.Tx) error {
		tenants, err := tenancy.AllTenants(ctx, tx)
		for _, t := range tenants {
			byID[t.ID] = t
		}
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("drverify: list tenants: %w", err)
	}
	return byID, nil
}

// follow offboards and erases the tenant again as far as the directory's
// status says; what fails is a problem, and the other tenants go on.
func (rc *Reconciler) follow(ctx context.Context, status string, t tenancy.Tenant, rep *Reconciled) {
	if status != directory.Offboarded && status != directory.Erased {
		return
	}
	if t.Status != controlplane.Offboarded {
		if _, err := rc.Control.Transition(ctx, operator, t.ID, "offboard", etag.Format(t.Version)); err != nil {
			rep.problem("tenant %s: offboard again: %v", t.ID, err)
			return
		}
		rep.Offboarded = append(rep.Offboarded, t.ID)
	}
	if status != directory.Erased || t.ErasedAt != nil {
		return
	}
	if t.ExportedAt == nil {
		if _, err := rc.Offboarding.Adopt(ctx, t.ID); err != nil {
			rep.problem("tenant %s: erase again: %v", t.ID, err)
			return
		}
	}
	if _, err := rc.Offboarding.Erase(ctx, t.ID, Actor, map[string]any{"reason": "the directory had it erased after the restore point (ADR-0040)"}); err != nil {
		rep.problem("tenant %s: erase again: %v", t.ID, err)
		return
	}
	rep.Erased = append(rep.Erased, t.ID)
}
