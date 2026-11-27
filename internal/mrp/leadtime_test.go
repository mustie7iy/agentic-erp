package mrp_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/mrp"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestLeadTimes (S2j2; ADR-0082 As built S2j2): a material's planning
// data at a plant may name its replenishment lead time, 1 to 999 days,
// set on creation, changed and removed by a merge patch, audited, and
// read by sales through mrp.Plans for the materials of a plant that have
// one, whatever their MRP type.
func TestLeadTimes(t *testing.T) {
	e := setup(t)
	p, tag := e.Must(201, "POST", plans, mustJSON(t, map[string]any{"plant_id": e.p1, "material_id": e.bracket, "mrp_type": "no_planning",
		"reorder_point": "0", "account_id": e.stock, "replenishment_lead_time_days": 10}), "")
	if p["replenishment_lead_time_days"] != float64(10) {
		t.Errorf("created: got %v, want 10 days", p["replenishment_lead_time_days"])
	}
	id := p["id"].(string)
	other := e.material(t, "Bracket without a lead time", "raw_material")
	e.plan(t, other, "5")
	leads := func() string {
		t.Helper()
		var got map[uuid.UUID]int32
		if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			var err error
			got, err = mrp.Plans{}.LeadTimes(ctx, e.p1, []uuid.UUID{uuid.MustParse(e.bracket), uuid.MustParse(other), uuid.New()})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(got)
	}
	if got, want := leads(), fmt.Sprintf("map[%s:10]", e.bracket); got != want {
		t.Errorf("the plant's lead times: got %s, want %s", got, want)
	}
	e.Problem(400, "request.invalid_body", "POST", plans, mustJSON(t, map[string]any{"plant_id": e.p1, "material_id": other, "mrp_type": "no_planning",
		"reorder_point": "0", "account_id": e.stock, "replenishment_lead_time_days": 0}), "")
	got, tag := e.Must(200, "PATCH", plans+"/"+id, `{"replenishment_lead_time_days": 20}`, tag)
	if got["replenishment_lead_time_days"] != float64(20) {
		t.Errorf("changed: got %v, want 20 days", got["replenishment_lead_time_days"])
	}
	e.Problem(422, "mrp.nothing_to_change", "PATCH", plans+"/"+id, `{"replenishment_lead_time_days": 20}`, tag)
	got, tag = e.Must(200, "PATCH", plans+"/"+id, `{"replenishment_lead_time_days": null}`, tag)
	if v, ok := got["replenishment_lead_time_days"]; ok {
		t.Errorf("removed: got %v, want none", v)
	}
	e.Problem(422, "mrp.nothing_to_change", "PATCH", plans+"/"+id, `{"replenishment_lead_time_days": null}`, tag)
	if got := leads(); got != "map[]" {
		t.Errorf("the plant's lead times once removed: got %s, want none", got)
	}
	var detail string
	pgtest.Scan(t, `SELECT string_agg(coalesce(detail->'before'->>'replenishment_lead_time_days', 'none') || '>' ||
			coalesce(detail->'after'->>'replenishment_lead_time_days', 'none'), ',' ORDER BY seq) FROM audit.audit_event
		WHERE tenant_id = $1 AND action = 'mrp.material_plan.change'`, []any{e.Tenant.ID}, &detail)
	if detail != "10>20,20>none" {
		t.Errorf("the changes' audit: got %q, want 10>20, then 20>none", detail)
	}
	pgtest.Scan(t, `SELECT detail->>'replenishment_lead_time_days' FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2
		AND action = 'mrp.material_plan.create'`, []any{e.Tenant.ID, id}, &detail)
	if detail != "10" {
		t.Errorf("the creation's audit: got %q, want 10", detail)
	}
}

// TestLeadTimeChecks (S2j2): a lead time the contract bounds is still
// checked by the context for its callers, on creation and on a change.
func TestLeadTimeChecks(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	p, tag := e.plan(t, e.bracket, "5")
	id := uuid.MustParse(p["id"].(string))
	for _, days := range []int32{0, -1, mrp.MaxLeadDays + 1} {
		t.Run(fmt.Sprint(days), func(t *testing.T) {
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				_, err := h.CreatePlan(ctx, mrp.PlanInput{PlantID: e.p1, MaterialID: uuid.MustParse(e.material(t, fmt.Sprint("Bracket ", days), "raw_material")),
					AccountID: e.stock, MRPType: mrp.NoPlanning, ReorderPoint: "0", LeadDays: &days})
				return err
			})
			if codeOf(err) != "mrp.invalid_lead_time" {
				t.Errorf("create: got %v, want mrp.invalid_lead_time", err)
			}
			err = apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				d := &days
				_, err := h.ChangePlan(ctx, id, etag.IfMatch(tag), mrp.PlanChange{LeadDays: &d})
				return err
			})
			if codeOf(err) != "mrp.invalid_lead_time" {
				t.Errorf("change: got %v, want mrp.invalid_lead_time", err)
			}
		})
	}
}

// TestLeadTimesFailures: the published read needs a unit of work and
// reports its statement's failure.
func TestLeadTimesFailures(t *testing.T) {
	e := setup(t)
	read := func(ctx context.Context) error {
		_, err := mrp.Plans{}.LeadTimes(ctx, e.p1, []uuid.UUID{uuid.MustParse(e.bracket)})
		return err
	}
	if err := read(context.Background()); err == nil {
		t.Error("outside a unit of work: got nil error, want an error")
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "lead times", read)
}

// TestPlanLotSizingAndLeadTime (batch 96S integration; ADR-0084 As built,
// replenishment lead time with lot sizing): a plan carries its lot sizing
// and its lead time, each its own fields of the merge patch, so a change
// naming one leaves the other; a run with both sizes by the lot sizing
// alone. The bracket, reorder point 250, 200 EA in stock against a
// confirmed order of 200 EA, planned in fixed lots of 70 EA with a lead
// time of 10 days, is proposed four lots of 70.
func TestPlanLotSizingAndLeadTime(t *testing.T) {
	e := setup(t)
	e.confirmed(t, "10 BOX")
	plan, tag := e.Must(201, "POST", plans, mustJSON(t, map[string]any{"plant_id": e.p1, "material_id": e.bracket, "mrp_type": "reorder_point",
		"reorder_point": "250", "account_id": e.stock, "lot_size": "fixed", "fixed_lot_size": "70", "replenishment_lead_time_days": 10}), "")
	id := plans + "/" + plan["id"].(string)
	both := func(p map[string]any) string {
		t.Helper()
		return fmt.Sprint(p["lot_size"], " ", p["fixed_lot_size"], " ", p["replenishment_lead_time_days"])
	}
	if got, want := both(plan), "fixed map[unit:EA value:70] 10"; got != want {
		t.Errorf("created: got %s, want %s", got, want)
	}
	if got, want := lotLine(t, e.run(t), e.bracket), "fixed 280 4 PR100000"; got != want {
		t.Errorf("the run with a lead time: got %s, want %s", got, want)
	}
	plan, tag = e.Must(200, "PATCH", id, `{"fixed_lot_size": "50"}`, tag)
	if got, want := both(plan), "fixed map[unit:EA value:50] 10"; got != want {
		t.Errorf("the lot sizing changed: got %s, want %s (the lead time kept)", got, want)
	}
	plan, _ = e.Must(200, "PATCH", id, `{"replenishment_lead_time_days": 20}`, tag)
	if got, want := both(plan), "fixed map[unit:EA value:50] 20"; got != want {
		t.Errorf("the lead time changed: got %s, want %s (the lot sizing kept)", got, want)
	}
	var detail string
	pgtest.Scan(t, `SELECT string_agg((detail->'before'->>'fixed_lot_size') || '/' || (detail->'before'->>'replenishment_lead_time_days') || '>' ||
			(detail->'after'->>'fixed_lot_size') || '/' || (detail->'after'->>'replenishment_lead_time_days'), ',' ORDER BY seq) FROM audit.audit_event
		WHERE tenant_id = $1 AND action = 'mrp.material_plan.change'`, []any{e.Tenant.ID}, &detail)
	if want := "70/10>50/10,50/10>50/20"; detail != want {
		t.Errorf("the changes' audit: got %q, want %q", detail, want)
	}
}
