package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// CenterMigration creates the segments, profit centers and cost centers of
// an SAP migration run for the integration context (ADR-0050 §10, T56h).
// The run itself is the governed act (a reconciled load started by a
// person with integration.migration.run), as it is for the suppliers
// partner.Migration creates, so each creation is a center change recorded
// as applied at once, naming the run in its reason, with no approval
// request of its own (§10: "approved by the migration run's approval, not
// one by one"). It is checked as a requested create is when it is
// applied: an active category, a profit center that is active, assigned
// to the cost center's company and valid over each period, and a place at
// a leaf of the standard hierarchy: SAP's, which the run loads first
// (T56h2, hierarchymigration.go), else the UNASSIGNED leaf.
type CenterMigration struct{}

// MigratedPeriod is a migrated center's period: from From to To
// (exclusive; zero open-ended) with what it says.
type MigratedPeriod[A comparable] struct {
	From, To time.Time
	Attr     A
}

// MigratedCostCenter is a cost center to create with its periods, in date
// order and adjoining.
type MigratedCostCenter struct {
	LegalEntityID uuid.UUID
	Code          string
	Texts         Texts
	Periods       []MigratedPeriod[CostCenterAttrs]
	// StandardNodeID is its leaf of the standard hierarchy (SAP's KHINR);
	// nil places it at the UNASSIGNED leaf.
	StandardNodeID *uuid.UUID
	Reason         string
}

// MigratedProfitCenter is a profit center to create with its companies
// and its periods, in date order and adjoining.
type MigratedProfitCenter struct {
	Code           string
	Texts          Texts
	SegmentID      *uuid.UUID
	LegalEntityIDs []uuid.UUID
	Periods        []MigratedPeriod[ProfitCenterAttrs]
	StandardNodeID *uuid.UUID // as a cost center's
	Reason         string
}

// migratedPayload is what a migrated create's change records: the periods
// and companies it created, as a create's payload names its validity.
type migratedPayload struct {
	Periods        []map[string]any `json:"periods"`
	LegalEntityIDs []uuid.UUID      `json:"legal_entity_ids,omitempty"`
}

// migratedSpans checks that the periods adjoin in date order, each ending
// after it starts and only the last open-ended, and joins neighbours alike.
func migratedSpans[A comparable](periods []MigratedPeriod[A]) ([]span[A], error) {
	if len(periods) == 0 {
		return nil, invalidValidity("valid_from", "a migrated center needs at least one period")
	}
	spans := make([]span[A], len(periods))
	for i, p := range periods {
		spans[i] = span[A]{From: day(p.From), To: dayOrOpen(p.To), Attr: p.Attr}
		if err := checkDates(spans[i].From, spans[i].To); err != nil {
			return nil, err
		}
		if i > 0 && !spans[i-1].To.Equal(spans[i].From) {
			return nil, invalidValidity("valid_from", "the periods of a migrated center must adjoin: one ends ("+dateText(spans[i-1].To)+
				", open-ended if empty) where the next starts ("+dateText(spans[i].From)+")")
		}
	}
	return joined(spans), nil
}

// periodMaps are the spans as a change's payload and events name periods.
func periodMaps[A comparable](spans []span[A], attrs func(A) map[string]any) []map[string]any {
	out := make([]map[string]any, len(spans))
	for i, s := range spans {
		m := attrs(s.Attr)
		m["valid_from"], m["valid_to"] = dateText(s.From), datePtr(s.To)
		out[i] = m
	}
	return out
}

// appliedChange records a migrated create, assignment, node or place as
// a center change applied at once (CenterMigration): of the center ref
// names, of the hierarchy it names, or of both.
func appliedChange(ctx context.Context, tx pgx.Tx, ref centerRef, kind string, payload any, reason string) (uuid.UUID, error) {
	p := principal(ctx)
	var cc, pc *uuid.UUID
	switch {
	case ref.id == uuid.Nil: // a node, which names no center
	case ref.kind == costCenters:
		cc = &ref.id
	default:
		pc = &ref.id
	}
	b, _ := json.Marshal(payload) // periods of dates, strings, ids and flags, or a hierarchy's payload
	id := uuid.Must(uuid.NewV7())
	_, err := tx.Exec(ctx, `INSERT INTO ledger.center_change (tenant_id, id, cost_center_id, profit_center_id, hierarchy_id, kind, payload, reason, status,
			applied_at, requested_by, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'applied', now(), $9, $9, $9)`,
		p.TenantID, id, cc, pc, ref.hierarchy, kind, b, reason, p.ID)
	if ref.id == uuid.Nil {
		return id, errs.Wrapf(err, "ledger: record the migrated %s change of hierarchy %s", kind, *ref.hierarchy)
	}
	return id, errs.Wrapf(err, "ledger: record the migrated %s change of %s %s", kind, ref.kind.table, ref.id)
}

// LockProfitCenters takes the existing profit centers a run assigns or
// names for update, in id order, before the run creates anything: creating
// a center locks its standard hierarchy, and the API locks a center before
// its hierarchy (a place change, a decision), so the run must hold the
// centers it touches first (ADR-0008).
func (CenterMigration) LockProfitCenters(ctx context.Context, ids []uuid.UUID) error {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `SELECT FROM ledger.profit_center WHERE id = ANY($1) ORDER BY id FOR UPDATE`, ids)
	return errs.Wrapf(err, "ledger: lock the profit centers a migration run touches")
}

// CreateSegment creates a segment, as the API does (CreateSegment).
func (CenterMigration) CreateSegment(ctx context.Context, in NewSegment) (uuid.UUID, error) {
	s, err := CreateSegment(ctx, in)
	return s.ID, err
}

// CreateProfitCenter creates an active profit center with its companies
// and periods.
func (CenterMigration) CreateProfitCenter(ctx context.Context, in MigratedProfitCenter) (uuid.UUID, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	if err := checkSegment(ctx, tx, in.SegmentID); err != nil {
		return uuid.Nil, err
	}
	spans, err := migratedSpans(in.Periods)
	if err != nil {
		return uuid.Nil, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.profit_center (tenant_id, id, code, segment_id, name, description, responsible, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending', $8, $8)`, p.TenantID, id, in.Code, in.SegmentID, in.Texts.Name, in.Texts.Description, in.Texts.Responsible, p.ID); err != nil {
		return uuid.Nil, fmt.Errorf("ledger: insert migrated profit center %s: %w", in.Code, err)
	}
	payload := migratedPayload{LegalEntityIDs: in.LegalEntityIDs, Periods: periodMaps(spans, func(a ProfitCenterAttrs) map[string]any {
		return map[string]any{"locked": a.Locked}
	})}
	change, err := appliedChange(ctx, tx, profitCenterRef(id, in.Code), KindCreate, payload, in.Reason)
	if err != nil {
		return uuid.Nil, err
	}
	c := ProfitCenter{centerHead: centerHead{ID: id}}
	if err := writeProfitCenter(ctx, tx, c, CenterChange{ID: change}, profitCenterPayload{LegalEntityIDs: in.LegalEntityIDs}, spans); err != nil {
		return uuid.Nil, err
	}
	if err := activate(ctx, tx, profitCenters, id, change, in.StandardNodeID); err != nil {
		return uuid.Nil, err
	}
	return id, recordProfitCenter(ctx, tx, id, KindCreate, &change)
}

// AssignProfitCenter assigns a profit center an earlier run created to
// the legal entities it is not yet assigned to; it refuses one that is
// assigned to them all (ledger.nothing_to_change).
func (CenterMigration) AssignProfitCenter(ctx context.Context, id uuid.UUID, legalEntities []uuid.UUID, reason string) error {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	c, err := getProfitCenter(ctx, tx, id, forUpdate)
	if err != nil {
		return err
	}
	if err := changeable(c.centerHead, func(int32) error { return nil }); err != nil {
		return err
	}
	if c.PendingChangeID != nil {
		// Applied around a pending change, the change could no longer be
		// approved (ADR-0050 §11: one pending change per center).
		return problem.Conflict("ledger.center_change_pending", "Profit center "+c.Code+" has a pending change; it must be decided or withdrawn first.")
	}
	in := profitCenterPayload{LegalEntityIDs: legalEntities}
	spans, err := planProfitCenter(ctx, tx, c, KindAssign, in, companyDays{})
	if err != nil {
		return err
	}
	change, err := appliedChange(ctx, tx, profitCenterRef(id, c.Code), KindAssign, migratedPayload{LegalEntityIDs: newCompanies(c, legalEntities),
		Periods: []map[string]any{}}, reason)
	if err != nil {
		return err
	}
	if err := writeProfitCenter(ctx, tx, c, CenterChange{ID: change}, in, spans); err != nil {
		return err
	}
	if err := profitCenters.touch(ctx, tx, id, ""); err != nil {
		return err
	}
	return recordProfitCenter(ctx, tx, id, KindAssign, &change)
}

// CreateCostCenter creates an active cost center with its periods. Each
// period's category must be an active one of the configuration; its locks
// are the migrated center's own, not the category's defaults for a new one.
func (CenterMigration) CreateCostCenter(ctx context.Context, in MigratedCostCenter) (uuid.UUID, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	spans, err := migratedSpans(in.Periods)
	if err != nil {
		return uuid.Nil, err
	}
	for _, s := range spans {
		if _, err := costCenterCategory(ctx, tx, s.Attr.Category); err != nil {
			return uuid.Nil, err
		}
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.cost_center (tenant_id, id, legal_entity_id, code, name, description, responsible, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending', $8, $8)`, p.TenantID, id, in.LegalEntityID, in.Code, in.Texts.Name, in.Texts.Description, in.Texts.Responsible,
		p.ID); err != nil {
		return uuid.Nil, fmt.Errorf("ledger: insert migrated cost center %s: %w", in.Code, err)
	}
	c := CostCenter{centerHead: centerHead{ID: id}, LegalEntityID: in.LegalEntityID}
	if err := checkProfitCenters(ctx, tx, c.LegalEntityID, spans); err != nil {
		return uuid.Nil, err
	}
	payload := migratedPayload{Periods: periodMaps(spans, func(a CostCenterAttrs) map[string]any {
		return map[string]any{"category": a.Category, "profit_center_id": optionalID(a.ProfitCenterID), "lock_actual_cost": a.LockCost,
			"lock_actual_revenue": a.LockRevenue, "lock_plan": a.LockPlan}
	})}
	change, err := appliedChange(ctx, tx, centerRef{kind: costCenters, id: id}, KindCreate, payload, in.Reason)
	if err != nil {
		return uuid.Nil, err
	}
	if err := writeCostCenterPeriods(ctx, tx, c, change, nil, spans); err != nil {
		return uuid.Nil, err
	}
	if err := activate(ctx, tx, costCenters, id, change, in.StandardNodeID); err != nil {
		return uuid.Nil, err
	}
	return id, recordCostCenter(ctx, tx, id, KindCreate, &change)
}

func optionalID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// MigratedCostCenterState is a cost center as the reconciliation reads it.
type MigratedCostCenterState struct {
	LegalEntityID uuid.UUID
	Code, Status  string
	Periods       []MigratedPeriod[CostCenterAttrs]
}

// MigratedProfitCenterState is a profit center as the reconciliation
// reads it.
type MigratedProfitCenterState struct {
	Code, Status   string
	SegmentID      *uuid.UUID
	LegalEntityIDs []uuid.UUID
	Periods        []MigratedPeriod[ProfitCenterAttrs]
}

// CostCenters reads the cost centers by id, with their active periods, for
// the reconciliation of a migration run; an id that names none is left
// out.
func (CenterMigration) CostCenters(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]MigratedCostCenterState, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, costCenterSelect+` WHERE c.id = ANY($1)`, ids)
	centers, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CostCenter, error) { return scanCostCenter(r) })
	if err != nil {
		return nil, fmt.Errorf("ledger: read the migrated cost centers: %w", err)
	}
	periods, err := costCenterPeriods(ctx, tx, ids, noLock)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]MigratedCostCenterState, len(centers))
	for _, c := range centers {
		out[c.ID] = MigratedCostCenterState{LegalEntityID: c.LegalEntityID, Code: c.Code, Status: c.Status, Periods: fromSpans(periods[c.ID])}
	}
	return out, nil
}

// ProfitCenters reads the profit centers by id, as CostCenters.
func (CenterMigration) ProfitCenters(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]MigratedProfitCenterState, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, profitCenterSelect+` WHERE c.id = ANY($1)`, ids)
	centers, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ProfitCenter, error) { return scanProfitCenter(r) })
	if err != nil {
		return nil, fmt.Errorf("ledger: read the migrated profit centers: %w", err)
	}
	periods, err := profitCenterPeriods(ctx, tx, ids, noLock)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]MigratedProfitCenterState, len(centers))
	for _, c := range centers {
		out[c.ID] = MigratedProfitCenterState{Code: c.Code, Status: c.Status, SegmentID: c.SegmentID, LegalEntityIDs: slices.Clone(c.LegalEntityIDs),
			Periods: fromSpans(periods[c.ID])}
	}
	return out, nil
}

// Segments reads the codes of the segments by id; an id that names none is
// left out.
func (CenterMigration) Segments(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT id, code FROM ledger.segment WHERE id = ANY($1)`, ids)
	out := map[uuid.UUID]string{}
	var id uuid.UUID
	var code string
	_, err = pgx.ForEachRow(rows, []any{&id, &code}, func() error { out[id] = code; return nil })
	return out, errs.Wrapf(err, "ledger: read the migrated segments")
}

// fromSpans are a center's active periods as the reconciliation compares
// them with SAP's.
func fromSpans[A comparable](spans []span[A]) []MigratedPeriod[A] {
	out := make([]MigratedPeriod[A], len(spans))
	for i, s := range spans {
		out[i] = MigratedPeriod[A]{From: s.From, To: s.To, Attr: s.Attr}
	}
	return out
}
