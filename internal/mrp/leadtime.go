package mrp

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Replenishment lead times (S2j2; ADR-0082 As built S2j2, ADR-0084 As
// built): a material's planning data at a plant may name how many
// calendar days the plant takes to have more of it (SAP MARC-WZEIT, from
// memory), whatever its MRP type. The sales context's availability check
// reads them through a port the composition root wires (LeadTimes), so
// mrp stays a leaf; planning runs do not read them.

// MaxLeadDays is the longest replenishment lead time, in days: SAP's
// field has three digits (from memory), and no plant takes years.
const MaxLeadDays = 999

// checkLeadDays checks a lead time a plan is given, nil for none: one to
// MaxLeadDays days (422 mrp.invalid_lead_time). The contract bounds it
// too; this answers the context's other callers.
func checkLeadDays(days *int32) error {
	if days != nil && (*days < 1 || *days > MaxLeadDays) {
		return problem.Invalid("mrp.invalid_lead_time", "replenishment_lead_time_days", fmt.Sprintf("1 to %d days", MaxLeadDays))
	}
	return nil
}

// withLead is a plan's audit detail with its lead time when it has one:
// a plan without one is recorded as before lead times.
func withLead(detail map[string]any, days *int32) map[string]any {
	if days != nil {
		detail["replenishment_lead_time_days"] = *days
	}
	return detail
}

// Plans is the mrp context's published read of materials' planning data,
// in the request's transaction.
type Plans struct{}

// LeadTimes are the replenishment lead times in days of those of the
// materials that the plant plans with one. Company access is not applied:
// the caller reads for a document of the plant's company it may act in.
// The plans are read without a lock: a lead time is one input of a check
// and no rule spans it and what the check writes, so a change committed
// after the read orders after the check, as one committed after the
// check's commit would.
func (Plans) LeadTimes(ctx context.Context, plant uuid.UUID, materials []uuid.UUID) (map[uuid.UUID]int32, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	type lead struct {
		Material uuid.UUID
		Days     int32
	}
	rows, _ := tx.Query(ctx, `SELECT material_id, replenishment_lead_time_days FROM mrp.material_plan
		WHERE plant_id = $1 AND material_id = ANY($2) AND replenishment_lead_time_days IS NOT NULL ORDER BY material_id`, plant, materials)
	leads, err := pgx.CollectRows(rows, pgx.RowToStructByPos[lead])
	if err != nil {
		return nil, fmt.Errorf("mrp: lead times at plant %s: %w", plant, err)
	}
	out := make(map[uuid.UUID]int32, len(leads))
	for _, l := range leads {
		out[l.Material] = l.Days
	}
	return out, nil
}
