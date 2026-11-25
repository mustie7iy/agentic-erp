package mrp

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Materials' planning data at plants (ADR-0084 §4; SAP MARC's MRP view):
// how a material is planned at a plant, its reorder point and how its
// proposals are sized (lotsize.go) in its base unit, maintained by a
// planner at its version, read by the plant's runs.

// Plan is a material's planning data at a plant.
type Plan struct {
	ID, LegalEntityID, PlantID, MaterialID, AccountID uuid.UUID
	MRPType, ReorderPoint                             string
	Lots                                              LotSizing
	Version                                           int32
	CreatedAt, UpdatedAt                              time.Time
	CreatedBy, UpdatedBy                              uuid.UUID
}

const planSelect = `SELECT id, legal_entity_id, plant_id, material_id, account_id, mrp_type, trim_scale(reorder_point)::text, lot_size,
		trim_scale(fixed_lot_size)::text, trim_scale(maximum_stock_level)::text, trim_scale(minimum_lot_size)::text,
		trim_scale(maximum_lot_size)::text, trim_scale(rounding_value)::text, version, created_at, updated_at, created_by, updated_by
	FROM mrp.material_plan`

func scanPlan(row pgx.Row) (Plan, error) {
	var p Plan
	l := &p.Lots
	err := row.Scan(&p.ID, &p.LegalEntityID, &p.PlantID, &p.MaterialID, &p.AccountID, &p.MRPType, &p.ReorderPoint, &l.Procedure, &l.Fixed,
		&l.MaximumLevel, &l.Minimum, &l.Maximum, &l.Rounding, &p.Version, &p.CreatedAt, &p.UpdatedAt, &p.CreatedBy, &p.UpdatedBy)
	return p, errs.Wrapf(err, "scan material plan")
}

// loadPlan reads a plan of a company the caller may act in, under its
// row lock with lock; another company's is not found.
func loadPlan(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Plan, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Plan{}, err
	}
	q := planSelect + ` WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	p, err := scanPlan(tx.QueryRow(ctx, q, id))
	if err = authz.Visible(err, access, p.LegalEntityID, problem.NotFoundOf("material plan"), "mrp: material plan %s", id); err != nil {
		return Plan{}, err
	}
	return p, nil
}

// PlanInput is a material's planning data as a request creates it.
type PlanInput struct {
	PlantID, MaterialID, AccountID uuid.UUID
	MRPType, ReorderPoint          string
	Lots                           LotSizing // its Procedure empty: lot for lot
}

// plant reads an active plant of a company the caller may act in, with
// its company (422 mrp.plant_invalid naming field otherwise).
func (e *Endpoints) plant(ctx context.Context, id uuid.UUID, field string) (tenancy.Unit, tenancy.Entity, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return tenancy.Unit{}, tenancy.Entity{}, err
	}
	units, err := e.Plants.OrgUnits(ctx, []uuid.UUID{id})
	if err != nil {
		return tenancy.Unit{}, tenancy.Entity{}, fmt.Errorf("mrp: plant %s: %w", id, err)
	}
	u, ok := units[id]
	if !ok || u.Kind != "plant" || u.Status != tenancy.Active || !access.Allows(u.LegalEntityID) {
		return tenancy.Unit{}, tenancy.Entity{}, problem.Invalid("mrp.plant_invalid", field, "No active plant with this id in a company you may act in.")
	}
	// An org unit's legal entity is there: tenancy's foreign key holds it.
	le, _, err := e.Plants.Entity(ctx, u.LegalEntityID)
	if err != nil {
		return tenancy.Unit{}, tenancy.Entity{}, fmt.Errorf("mrp: company of plant %s: %w", u.Code, err)
	}
	return u, le, nil
}

// plannable checks the material a plan is created for: an active material
// kept in stock and extended to the plant (422 mrp.material_invalid).
func (e *Endpoints) plannable(ctx context.Context, id uuid.UUID, plant tenancy.Unit) (materials.Material, error) {
	ms, err := e.Materials.Materials(ctx, []uuid.UUID{id})
	if err != nil {
		return materials.Material{}, fmt.Errorf("mrp: material %s: %w", id, err)
	}
	m, ok := ms[id]
	if _, extended := m.Plant(plant.ID); !ok || m.Status != "active" || !m.Stocked() || !extended {
		return materials.Material{}, problem.Invalid("mrp.material_invalid", "material_id", "No active material with this id kept in stock at plant "+
			plant.Code+".")
	}
	return m, nil
}

// checkPlan checks the MRP type, reorder point, lot sizing and account of
// a plan of the company.
func (e *Endpoints) checkPlan(ctx context.Context, le uuid.UUID, p Plan) error {
	if p.MRPType != ReorderPoint && p.MRPType != NoPlanning {
		return problem.Invalid("mrp.invalid_mrp_type", "mrp_type", "reorder_point or no_planning")
	}
	if money.QuantityOrZero(p.ReorderPoint) != nil {
		return problem.Invalid("mrp.invalid_quantity", "reorder_point", fmt.Sprintf("zero or a plain decimal of at most %d integer digits and %d decimals",
			money.QuantityIntDigits, money.QuantityDecimals))
	}
	if err := p.Lots.check(p.ReorderPoint); err != nil {
		return err
	}
	ok, err := e.Accounts.IsPostable(ctx, le, p.AccountID)
	if err != nil {
		return fmt.Errorf("mrp: account %s: %w", p.AccountID, err)
	}
	if !ok {
		return problem.Invalid("mrp.account_not_postable", "account_id", "An active, unblocked account of the plant's company that is not a reconciliation account.")
	}
	return nil
}

// CreatePlan creates a material's planning data at a plant: an active
// plant of a company the caller may act in, an active material kept in
// stock there, a lot sizing procedure (lot for lot unless named) with
// the quantities it uses, a postable account of the company; once per
// material and plant (409 mrp.material_plan_exists). Audited.
func (e *Endpoints) CreatePlan(ctx context.Context, in PlanInput) (Plan, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Plan{}, err
	}
	plant, le, err := e.plant(ctx, in.PlantID, "plant_id")
	if err != nil {
		return Plan{}, err
	}
	m, err := e.plannable(ctx, in.MaterialID, plant)
	if err != nil {
		return Plan{}, err
	}
	if in.Lots.Procedure == "" {
		in.Lots.Procedure = LotForLot
	}
	if err := e.checkPlan(ctx, le.ID, Plan{MRPType: in.MRPType, ReorderPoint: in.ReorderPoint, Lots: in.Lots, AccountID: in.AccountID}); err != nil {
		return Plan{}, err
	}
	id, l := uuid.Must(uuid.NewV7()), in.Lots
	if _, err := tx.Exec(ctx, `INSERT INTO mrp.material_plan (tenant_id, id, legal_entity_id, plant_id, material_id, mrp_type, reorder_point, account_id,
			lot_size, fixed_lot_size, maximum_stock_level, minimum_lot_size, maximum_lot_size, rounding_value, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8, $9, $10::numeric, $11::numeric, $12::numeric, $13::numeric, $14::numeric, $15, $15)`,
		p.TenantID, id, le.ID, plant.ID, m.ID, in.MRPType, in.ReorderPoint, in.AccountID, l.Procedure, l.Fixed, l.MaximumLevel, l.Minimum, l.Maximum,
		l.Rounding, p.ID); err != nil {
		return Plan{}, fmt.Errorf("mrp: plan material %s at plant %s: %w", m.Number, plant.Code, err)
	}
	out, err := loadPlan(ctx, tx, id, false)
	if err != nil {
		return Plan{}, err
	}
	detail := planAudit(out)
	detail["plant_id"], detail["plant_code"], detail["material_id"], detail["material_number"] = plant.ID, plant.Code, m.ID, m.Number
	return out, audit.Record(ctx, "mrp.material_plan.create", "mrp.material_plan", id, detail)
}

// PlanChange is a merge patch of a plan; nil leaves a field as it is.
// A lot sizing quantity is set to its value, or removed when it points
// to nil.
type PlanChange struct {
	MRPType, ReorderPoint, LotSize                                            *string
	AccountID                                                                 *uuid.UUID
	FixedLotSize, MaximumStockLevel, MinimumLotSize, MaximumLotSize, Rounding **string
}

// apply is the plan with the change, and whether it changes anything.
func (c PlanChange) apply(p Plan) (Plan, bool) {
	before := p
	if c.MRPType != nil {
		p.MRPType = *c.MRPType
	}
	if c.ReorderPoint != nil {
		p.ReorderPoint = *c.ReorderPoint
	}
	if c.AccountID != nil {
		p.AccountID = *c.AccountID
	}
	if c.LotSize != nil {
		p.Lots.Procedure = *c.LotSize
	}
	l := &p.Lots
	for _, f := range []struct{ to, from **string }{{&l.Fixed, c.FixedLotSize}, {&l.MaximumLevel, c.MaximumStockLevel}, {&l.Minimum, c.MinimumLotSize},
		{&l.Maximum, c.MaximumLotSize}, {&l.Rounding, c.Rounding}} {
		if f.from != nil {
			*f.to = *f.from
		}
	}
	same := p.MRPType == before.MRPType && p.AccountID == before.AccountID && sameQuantity(p.ReorderPoint, before.ReorderPoint) && p.Lots.same(before.Lots)
	return p, !same
}

// sameQuantity reports whether two decimals are one quantity ("30" and
// "30.0"); one that is not a decimal is never the same.
func sameQuantity(a, b string) bool {
	x, y := money.ParseRat(a), money.ParseRat(b)
	return x != nil && y != nil && x.Cmp(y) == 0
}

// ChangePlan changes a plan at its version (412 otherwise): its MRP type,
// reorder point, lot sizing or account, checked as on creation, the
// plan as it is after the change; a change to what it is is 422
// mrp.nothing_to_change. Audited with the values before and after.
func (e *Endpoints) ChangePlan(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, ch PlanChange) (Plan, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Plan{}, err
	}
	before, err := loadPlan(ctx, tx, id, true)
	if err != nil {
		return Plan{}, err
	}
	if err := ifMatch(before.Version); err != nil {
		return Plan{}, err
	}
	after, changed := ch.apply(before)
	if !changed {
		return Plan{}, problem.Unprocessable("mrp.nothing_to_change", "The material's planning data is so already.")
	}
	if err := e.checkPlan(ctx, before.LegalEntityID, after); err != nil {
		return Plan{}, err
	}
	l := after.Lots
	if _, err := tx.Exec(ctx, `UPDATE mrp.material_plan SET mrp_type = $2, reorder_point = $3::numeric, account_id = $4, lot_size = $5,
			fixed_lot_size = $6::numeric, maximum_stock_level = $7::numeric, minimum_lot_size = $8::numeric, maximum_lot_size = $9::numeric,
			rounding_value = $10::numeric, version = version + 1, updated_by = $11
		WHERE id = $1`, id, after.MRPType, after.ReorderPoint, after.AccountID, l.Procedure, l.Fixed, l.MaximumLevel, l.Minimum, l.Maximum, l.Rounding,
		p.ID); err != nil {
		return Plan{}, fmt.Errorf("mrp: change material plan %s: %w", id, err)
	}
	out, err := loadPlan(ctx, tx, id, false)
	if err != nil {
		return Plan{}, err
	}
	return out, audit.Record(ctx, "mrp.material_plan.change", "mrp.material_plan", id, map[string]any{"before": planAudit(before), "after": planAudit(out)})
}

// planAudit is what a plan's audit records hold of it: on creation,
// and before and after a change.
func planAudit(p Plan) map[string]any {
	out := p.Lots.auditOf()
	out["mrp_type"], out["reorder_point"], out["account_id"] = p.MRPType, p.ReorderPoint, p.AccountID
	return out
}

// planned reads the plant's plans by reorder point, by material, under
// a share lock, so a change waits for the run that plans by them.
func planned(ctx context.Context, tx pgx.Tx, plant uuid.UUID) ([]Plan, error) {
	rows, _ := tx.Query(ctx, planSelect+` WHERE plant_id = $1 AND mrp_type = $2 ORDER BY material_id FOR SHARE`, plant, ReorderPoint)
	ps, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Plan, error) { return scanPlan(r) })
	if err != nil {
		return nil, fmt.Errorf("mrp: plans of plant %s: %w", plant, err)
	}
	return ps, nil
}
