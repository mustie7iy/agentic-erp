package mrp_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// lotLine is a run's line of the material as "lot_size proposed lots
// requisition", "-" for none.
func lotLine(t *testing.T, r map[string]any, material string) string {
	t.Helper()
	for _, l := range rows(t, r, "lines") {
		if l["material_id"] == material {
			lots, req := "-", "-"
			if n, ok := l["lots"].(float64); ok {
				lots = fmt.Sprint(n)
			}
			if n, ok := l["requisition_number"].(string); ok {
				req = n
			}
			return fmt.Sprintf("%v %v %s %s", l["lot_size"], l["proposed"].(map[string]any)["value"], lots, req)
		}
	}
	return "none"
}

// items are a requisition's items' quantities as "quantity unit", in
// item order.
func (e env) items(t *testing.T, r map[string]any, material string) string {
	t.Helper()
	for _, l := range rows(t, r, "lines") {
		if l["material_id"] == material {
			req, _ := e.Must(200, "GET", "/v1/requisitions/"+l["requisition_id"].(string), "", "")
			var out []string
			for _, it := range rows(t, req, "items") {
				q := it["quantity"].(map[string]any)
				out = append(out, fmt.Sprint(q["value"], " ", q["unit"]))
			}
			return strings.Join(out, ", ")
		}
	}
	return "none"
}

// TestPlanningRunLotSizing (ADR-0084 As built, lot sizing): the bracket,
// reorder point 250 at P1, its 200 EA in stock against a confirmed order
// of 10 BOX (200 EA), is 250 EA short. Planned with a fixed lot of 70 EA
// a run proposes four lots of 70 (280 EA), the items of one requisition;
// changed to replenish up to 400 EA in lots of 50 to 150 EA rounded to
// 25, a rerun replaces it with 400 EA in lots of 150, 150 and 100; a
// fixed lot of 1 EA would need 250 lots, so the run fails with
// mrp.too_many_lots, proposing and cancelling nothing.
func TestPlanningRunLotSizing(t *testing.T) {
	e := setup(t)
	e.confirmed(t, "10 BOX")
	plan, tag := e.Must(201, "POST", plans, mustJSON(t, map[string]any{"plant_id": e.p1, "material_id": e.bracket, "mrp_type": "reorder_point",
		"reorder_point": "250", "account_id": e.stock, "lot_size": "fixed", "fixed_lot_size": "70.000"}), "")
	if got := fmt.Sprint(plan["lot_size"], " ", plan["fixed_lot_size"], " ", plan["maximum_stock_level"]); got != "fixed map[unit:EA value:70] <nil>" {
		t.Errorf("the plan: got %s, want a fixed lot of 70 EA and no maximum stock level", got)
	}
	id := plans + "/" + plan["id"].(string)
	first := e.run(t)
	if got := lotLine(t, first, e.bracket); got != "fixed 280 4 PR100000" {
		t.Errorf("the fixed lot's line: got %s, want four lots of 70 proposed", got)
	}
	if got := e.items(t, first, e.bracket); got != "70 EA, 70 EA, 70 EA, 70 EA" {
		t.Errorf("the fixed lot's requisition: got %s, want four items of 70 EA", got)
	}
	e.Problem(422, "mrp.invalid_lot_size", "PATCH", id, `{"lot_size": "replenish_to_maximum", "maximum_stock_level": "400"}`, tag)
	plan, tag = e.Must(200, "PATCH", id, `{"lot_size": "replenish_to_maximum", "fixed_lot_size": null, "maximum_stock_level": "400",
		"minimum_lot_size": "50", "maximum_lot_size": "150", "rounding_value": "25"}`, tag)
	if got := fmt.Sprint(plan["lot_size"], " ", plan["fixed_lot_size"], " ", plan["rounding_value"], " ", plan["version"]); got !=
		"replenish_to_maximum <nil> map[unit:EA value:25] 2" {
		t.Errorf("the changed plan: got %s, want replenishing to the maximum, rounded to 25 EA, at version 2", got)
	}
	var detail string
	pgtest.Scan(t, `SELECT detail::text FROM audit.audit_event WHERE tenant_id = $1 AND action = 'mrp.material_plan.change'`, []any{e.Tenant.ID}, &detail)
	if want := `"after": {"lot_size": "replenish_to_maximum", "mrp_type": "reorder_point", "account_id": "` + e.stock.String() +
		`", "reorder_point": "250", "rounding_value": "25", "maximum_lot_size": "150", "minimum_lot_size": "50", "maximum_stock_level": "400"}`; !strings.Contains(detail, want) ||
		!strings.Contains(detail, `"fixed_lot_size": "70"`) {
		t.Errorf("the change's audit: got %s, want the lot sizing after (%s) and the fixed lot before", detail, want)
	}
	second := e.run(t)
	if got := lotLine(t, second, e.bracket); got != "replenish_to_maximum 400 3 PR100001" {
		t.Errorf("the rerun's line: got %s, want 400 EA in three lots", got)
	}
	if got := e.items(t, second, e.bracket); got != "150 EA, 150 EA, 100 EA" {
		t.Errorf("the rerun's requisition: got %s, want lots of 150, 150 and 100 EA", got)
	}
	if got := e.proposals(t); fmt.Sprint(got) != "map[PR100000:replaced PR100001:open]" {
		t.Errorf("the proposals: got %v, want the fixed lots replaced", got)
	}
	e.Must(200, "PATCH", id, `{"lot_size": "fixed", "fixed_lot_size": "1", "maximum_stock_level": null, "minimum_lot_size": null, "maximum_lot_size": null,
		"rounding_value": null}`, tag)
	failed := e.run(t)
	errObj, _ := failed["error"].(map[string]any)
	if got := fmt.Sprint(failed["status"], " ", errObj["code"]); got != "failed mrp.too_many_lots" {
		t.Errorf("a fixed lot of 1 EA: got %s, want the run failed with mrp.too_many_lots", got)
	}
	if got := e.proposals(t); fmt.Sprint(got) != "map[PR100000:replaced PR100001:open]" {
		t.Errorf("the failed run's proposals: got %v, want nothing proposed or cancelled", got)
	}
}

// TestRunLinesBeforeLotSizing: a line a server before lot sizing wrote
// (000482: no proposed quantity, no lots) reads as lot for lot, its
// shortage proposed in one lot.
func TestRunLinesBeforeLotSizing(t *testing.T) {
	e := setup(t)
	e.plan(t, e.bracket, "0")
	run := e.run(t)["id"].(string)
	pgtest.Exec(t, `INSERT INTO mrp.run_line (tenant_id, run_id, material_id, uom_code, reorder_point, demand, stock, receipts, shortage, requisition_id,
		requisition_number) VALUES ($1, $2, $3, 'EA', 50, 0, 0, 0, 50, gen_random_uuid(), 'PR9')`, e.Tenant.ID, run, e.service)
	got, _ := e.Must(200, "GET", runs+"/"+run, "", "")
	if l := lotLine(t, got, e.service); l != "lot_for_lot 50 1 PR9" {
		t.Errorf("got %s, want the shortage of 50 in one lot, lot for lot", l)
	}
	if l := lotLine(t, got, e.bracket); l != "lot_for_lot 0 - -" {
		t.Errorf("a line without a shortage: got %s, want nothing proposed", l)
	}
}
