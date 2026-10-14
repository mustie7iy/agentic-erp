package operations

import (
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
