package operations

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
)

// Waiting is a queued operation that runs as a workflow (ADR-0012), as the
// context that owns the resource it works on describes it to the workflows
// dispatcher: the business key the workflow's id derives from. Each owning
// context lists its own (payments.Queued, configuration.QueuedSimulations,
// integration.QueuedRuns), so the dispatcher reads no context's tables.
type Waiting struct {
	Kind        string
	TenantID    uuid.UUID
	ID          uuid.UUID // the operation
	Resource    uuid.UUID // what it works on: a payment or migration run; for a simulation, the operation itself
	LegalEntity uuid.UUID // the resource's legal entity; zero when its key has none
	Number      string    // the resource's document number; empty when it has none
	Attempt     int       // its number among the operations of its kind on the resource; 0 for a proposal
}

// ScanWaiting reads a row of kind, tenant, operation, resource, legal
// entity, number and attempt, in that order: what an owning context's
// listing of its waiting operations selects (for pgx.CollectRows).
func ScanWaiting(r pgx.CollectableRow) (Waiting, error) {
	var w Waiting
	err := r.Scan(&w.Kind, &w.TenantID, &w.ID, &w.Resource, &w.LegalEntity, &w.Number, &w.Attempt)
	return w, errs.Wrapf(err, "scan waiting operation")
}

// QueuedOf lists, across active tenants (ADR-0015) and in the order they were queued, the
// operations of a kind waiting for a workflow whose resource is the path
// of what they work on (/v1/depreciation-runs/{id}), the resource being
// the path's last part: queued, or cancelled before any workflow took
// them, since the work still has to end what it holds (a depreciation run
// cancelled while queued holds its company and ledger until its workflow's
// first step finds the cancellation and fails it, ADR-0057 §7). It serves
// a leaf context (ADR-0057 §2), whose tables the workflows dispatcher may
// not read: the operation alone is the hand-off.
func QueuedOf(ctx context.Context, tx pgx.Tx, kind string) ([]Waiting, error) {
	rows, _ := tx.Query(ctx, `SELECT kind, tenant_id, id, substring(resource from '[^/]+$')::uuid, '00000000-0000-0000-0000-000000000000'::uuid, '', 0
		FROM platform.operation WHERE kind = $1 AND status IN ('queued', 'cancelled') AND workflow_id IS NULL AND resource IS NOT NULL
		AND tenant_id IN `+database.ActiveTenants+` ORDER BY id`, kind)
	waiting, err := pgx.CollectRows(rows, ScanWaiting)
	return waiting, errs.Wrapf(err, "operations: list the %s operations waiting for a workflow", kind)
}

// TenantsOf lists the active tenants (ADR-0015) with operations of a kind queued, running or
// cancelled: those an in-process worker of a cell without a workflow
// engine asks, in each tenant's scope, for the work still to do. A
// cancelled operation's work may still be queued or running, and the
// worker's next step ends it (a depreciation run failed with
// assets.run_cancelled, ADR-0057 §7).
func TenantsOf(ctx context.Context, tx pgx.Tx, kind string) ([]uuid.UUID, error) {
	rows, _ := tx.Query(ctx, `SELECT DISTINCT tenant_id FROM platform.operation WHERE kind = $1 AND status IN ('queued', 'running', 'cancelled')
		AND tenant_id IN `+database.ActiveTenants+` ORDER BY tenant_id`, kind)
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	return tenants, errs.Wrapf(err, "operations: tenants with %s operations", kind)
}

// StillQueuedOf lists, across tenants and in the order they were queued,
// the operations of a kind still queued whose resource is the path of
// what they work on, as QueuedOf reads them: the work of an in-process
// worker of a cell without a workflow engine, for a kind whose work holds
// nothing a cancellation must end (an e-invoice fetch, ADR-0061 §4), of
// active tenants only, as QueuedOf's (ADR-0015, po:2026-11-09). An
// operation leaves the list when its work finishes it; one cancelled, or
// one a workflow took, is not taken up.
func StillQueuedOf(ctx context.Context, tx pgx.Tx, kind string) ([]Waiting, error) {
	rows, _ := tx.Query(ctx, `SELECT kind, tenant_id, id, substring(resource from '[^/]+$')::uuid, '00000000-0000-0000-0000-000000000000'::uuid, '', 0
		FROM platform.operation WHERE kind = $1 AND status = 'queued' AND workflow_id IS NULL AND resource IS NOT NULL
		AND tenant_id IN `+database.ActiveTenants+` ORDER BY id`, kind)
	waiting, err := pgx.CollectRows(rows, ScanWaiting)
	return waiting, errs.Wrapf(err, "operations: list the %s operations still queued", kind)
}
