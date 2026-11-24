package mrp_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// requisition reads a requisition as "status version quantity unit price
// needed_by requester".
func (e env) requisition(t *testing.T, id string) (string, string) {
	t.Helper()
	r, tag := e.Must(200, "GET", "/v1/requisitions/"+id, "", "")
	it := rows(t, r, "items")[0]
	q, p := it["quantity"].(map[string]any), it["unit_price"].(map[string]any)
	return fmt.Sprintf("%v %v %v %v %v %v", r["status"], q["value"], q["unit"], p["amount"], r["needed_by"], r["requester_id"] == e.Tenant.Human.ID.String()), tag
}

// proposals are the tenant's proposals' statuses by requisition number.
func (e env) proposals(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	rows, err := pgtest.Admin(t).Query(context.Background(), `SELECT requisition_number, status FROM mrp.proposal WHERE tenant_id = $1`, e.Tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n, s string
		if err := rows.Scan(&n, &s); err != nil {
			t.Fatal(err)
		}
		out[n] = s
	}
	return out
}

// TestPlanningRun (S2k; ADR-0084 §3): a run of the plant counts the
// bracket's demand (the confirmed orders' ordered quantities less what is
// delivered, the part the availability check could not confirm
// included), its stock and its open receipts (an open purchase order,
// not a closed one, not a person's requisition), and proposes the
// shortage below the reorder point as a draft requisition in the base
// unit at the valuation price; a rerun replaces it; a proposal a person
// changed is theirs, counted as a receipt and never touched, until it is
// cancelled; a person's requisition is never touched.
func TestPlanningRun(t *testing.T) {
	e := setup(t)
	e.plan(t, e.bracket, "50")
	e.confirmed(t, "10 BOX") // 200 EA, all confirmed
	e.confirmed(t, "30 EA")  // none confirmed: the stock is promised
	e.ordered(t, "PO1", "released", "20")
	e.ordered(t, "PO2", "closed", "99")
	own, ownTag := e.Must(201, "POST", "/v1/requisitions", fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "items": [{"material_id": %q,
		"quantity": {"value": "5", "unit": "EA"}, "unit_price": {"amount": "2.00", "currency": "EUR"}, "account_id": %q}]}`, e.DE, e.bracket, e.stock), "")
	first := e.run(t)
	for f, want := range map[string]any{"status": "completed", "materials_planned": float64(1), "proposals_created": float64(1), "proposals_replaced": float64(0)} {
		t.Run(f, func(t *testing.T) {
			if first[f] != want {
				t.Errorf("the first run: got %v, want %v", first[f], want)
			}
		})
	}
	if got := line(t, first, e.bracket); got != "230 200 20 60 PR100001" {
		t.Errorf("the first run's line: got %s, want demand 230, stock 200, receipts 20 and a shortage of 60 proposed", got)
	}
	r1 := rows(t, first, "lines")[0]["requisition_id"].(string)
	if got, _ := e.requisition(t, r1); got != fmt.Sprintf("draft 60 EA 2.00 %v true", first["run_date"]) {
		t.Errorf("the proposal: got %s, want a draft of 60 EA at 2.00 needed by the run's day, raised by who started the run", got)
	}
	second := e.run(t)
	if got := line(t, second, e.bracket); got != "230 200 20 60 PR100002" {
		t.Errorf("the rerun's line: got %s, want the same shortage proposed again", got)
	}
	if replaced := rows(t, second, "replaced"); fmt.Sprint(len(replaced), " ", second["proposals_replaced"]) != "1 1" {
		t.Errorf("the rerun's replaced: got %v, want the first run's proposal alone", replaced)
	} else if got := fmt.Sprint(replaced[0]["requisition_id"], " ", replaced[0]["requisition_number"]); got != r1+" PR100001" {
		t.Errorf("the rerun's replaced proposal: got %s, want %s PR100001", got, r1)
	}
	if got, _ := e.requisition(t, r1); got[:9] != "cancelled" {
		t.Errorf("the first proposal: got %s, want it cancelled", got)
	}
	r2 := rows(t, second, "lines")[0]["requisition_id"].(string)
	_, tag := e.requisition(t, r2)
	e.Must(200, "PATCH", "/v1/requisitions/"+r2, fmt.Sprintf(`{"needed_by": "2026-12-31", "items": [{"material_id": %q, "quantity": {"value": "3", "unit": "BOX"},
		"unit_price": {"amount": "40.00", "currency": "EUR"}, "account_id": %q}, {"material_id": %q, "quantity": {"value": "1", "unit": "EA"},
		"unit_price": {"amount": "9.00", "currency": "EUR"}, "account_id": %q}]}`, e.bracket, e.stock, e.service, e.stock), tag) // 60 EA as 3 BOX, and a service
	third := e.run(t)
	if got := line(t, third, e.bracket); got != "230 200 80 0 -" {
		t.Errorf("after a person changed the proposal: got %s, want it counted as a receipt and nothing proposed", got)
	}
	if got, _ := e.requisition(t, r2); got[:5] != "draft" || len(rows(t, third, "replaced")) != 0 {
		t.Errorf("the changed proposal: got %s, replaced %v; want it left a draft", got, rows(t, third, "replaced"))
	}
	_, tag = e.requisition(t, r2)
	e.Must(200, "POST", "/v1/requisitions/"+r2+":cancel", "", tag)
	fourth := e.run(t)
	if got := line(t, fourth, e.bracket); got != "230 200 20 60 PR100003" {
		t.Errorf("after the changed proposal was cancelled: got %s, want it counted no more", got)
	}
	if got := e.proposals(t); fmt.Sprint(got) != "map[PR100001:replaced PR100002:closed PR100003:open]" {
		t.Errorf("the proposals: got %v, want the first replaced, the changed one closed once cancelled, the last open", got)
	}
	if got, tag := e.Must(200, "GET", "/v1/requisitions/"+own["id"].(string), "", ""); got["status"] != "draft" || tag != ownTag {
		t.Errorf("a person's requisition: got %v at %s, want it untouched at %s", got["status"], tag, ownTag)
	}
}

// TestPlanningRunIntercompany (S2k, S2i; ADR-0084 As built, batch 91,
// review:batch90): a run of P1 counts as demand, beside its confirmed
// sales order (30), the intercompany orders P1 supplies to P9 and has not
// issued, released or not, in the base unit (100 EA and 2 BOX, 40 EA),
// so it proposes what their 140 take below the reorder point; an issued
// order (its stock gone from P1), a closed or cancelled one and a removed
// item demand nothing. Stock transport orders within the company count
// too (As built, stock transport orders; TestPlanningRunTransports): the
// open one from P1 as demand (60), those to P1, open or in transit, as
// receipts (8 and 6). The intercompany orders are no receipts of P1; P9
// counts the open ones as its purchase orders' receipts, the issued one
// until it is received, so each is counted once on each side.
func TestPlanningRunIntercompany(t *testing.T) {
	e := setup(t)
	e.plan(t, e.bracket, "50")
	e.confirmed(t, "30 EA")
	for _, o := range []icOrder{{number: "IC1", status: "released", qty: "100", unit: "EA"}, {number: "IC2", status: "draft", delivery: "2030-01-01", qty: "2",
		unit: "BOX"}, {number: "IC3", status: "released", qty: "70", unit: "EA", issued: true}, {number: "IC4", status: "closed", qty: "9", unit: "EA"},
		{number: "IC5", status: "cancelled", qty: "9", unit: "EA"}, {number: "IC6", status: "released", qty: "9", unit: "EA", removed: true}} {
		e.intercompany(t, o)
	}
	e.transport(t, "ST1", "open", e.p1, uuid.Must(uuid.NewV7()), 60)
	e.transport(t, "ST2", "open", uuid.Must(uuid.NewV7()), e.p1, 8)
	e.transport(t, "ST3", "in_transit", uuid.Must(uuid.NewV7()), e.p1, 6)
	if got := line(t, e.run(t), e.bracket); got != "230 200 14 66 PR100000" {
		t.Errorf("the run's line: got %s, want demand 230 (30 sold, 140 to P9, 60 to another plant), stock 200, receipts 14 and a shortage of 66 proposed",
			got)
	}
	var receipts []procurement.OnOrder
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		var err error
		receipts, err = procurement.Orders{}.OpenReceipts(ctx, e.p9, []uuid.UUID{uuid.MustParse(e.bracket)})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(receipts), fmt.Sprintf("[{%s BOX 2} {%s EA 170}]", e.bracket, e.bracket); got != want {
		t.Errorf("P9's receipts: got %s, want %s (IC1, IC2 and the issued IC3)", got, want)
	}
}

// extend extends the bracket to the plant's storage location through a
// change approved by two approvers.
func (e env) extend(t *testing.T, plant, location uuid.UUID) {
	t.Helper()
	_, tag := e.Must(200, "GET", "/v1/materials/"+e.bracket, "", "")
	c, _ := e.Must(200, "PATCH", "/v1/materials/"+e.bracket, mustJSON(t, map[string]any{"plants": []any{
		map[string]any{"plant_id": e.p1, "storage_location_ids": []any{e.s1}}, map[string]any{"plant_id": plant, "storage_location_ids": []any{location}}}}), tag)
	e.Approve(t, c["pending_change_id"].(string))
}

// TestPlanningRunTransports (S2k, S2h, S2i; ADR-0084 As built, stock
// transport orders, review:batch91): the stock transport orders within
// company 2000, the intercompany orders and the sales orders counted
// together, each order once on each side. P1's run counts as demand its
// confirmed sales order (30), the intercompany order it supplies to P9
// and has not issued (40) and the stock transport order to P2 it has not
// issued (60); the one it issued (25, in transit) and the one P2 received
// (5) demand nothing more at P1. As receipts, P1 counts the stock
// transport orders from P2 not yet received, open (15) or in transit (7).
// P2's run counts those two as its demand only while open (15) and the
// orders from P1 as its receipts while open or in transit (60 and 25),
// never the received one; P9 counts the intercompany order as its
// purchase order's receipt. The orders cancelled before their goods issue
// (review:batch92), one each way, count on neither side.
func TestPlanningRunTransports(t *testing.T) {
	e := setup(t)
	p2 := e.orgUnit(t, e.DE, "plant", "P2", nil)
	e.extend(t, p2, e.orgUnit(t, e.DE, "storage_location", "S2", &p2))
	e.plan(t, e.bracket, "50")
	e.planAt(t, p2, e.bracket, "100")
	e.confirmed(t, "30 EA")
	e.intercompany(t, icOrder{number: "IC1", status: "released", qty: "40", unit: "EA"})
	for _, o := range []struct {
		number, status string
		from, to       uuid.UUID
		qty            int
	}{{"ST1", "open", e.p1, p2, 60}, {"ST2", "in_transit", e.p1, p2, 25}, {"ST3", "received", e.p1, p2, 5}, {"ST4", "open", p2, e.p1, 15},
		{"ST5", "in_transit", p2, e.p1, 7}, {"ST6", "cancelled", e.p1, p2, 90}, {"ST7", "cancelled", p2, e.p1, 80}} {
		e.transport(t, o.number, o.status, o.from, o.to, o.qty)
	}
	if got := line(t, e.run(t), e.bracket); got != "130 200 22 0 -" {
		t.Errorf("P1's line: got %s, want demand 130 (30 sold, 40 to P9, 60 to P2), stock 200, receipts 22 (15 and 7 from P2), nothing short", got)
	}
	if got := line(t, e.runAt(t, p2), e.bracket); got != "15 0 85 30 PR100000" {
		t.Errorf("P2's line: got %s, want demand 15 (to P1, open), no stock, receipts 85 (60 and 25 from P1) and 30 short of 100 proposed", got)
	}
	var receipts []procurement.OnOrder
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		var err error
		receipts, err = procurement.Orders{}.OpenReceipts(ctx, e.p9, []uuid.UUID{uuid.MustParse(e.bracket)})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(receipts), fmt.Sprintf("[{%s EA 40}]", e.bracket); got != want {
		t.Errorf("P9's receipts: got %s, want %s (IC1)", got, want)
	}
}

// TestPlanningRunLeavesOut (ADR-0084 §3): a run plans only what the plant
// plans by reorder point, of materials still kept there; nothing below
// the reorder point proposes nothing; an empty plant completes with
// nothing planned.
func TestPlanningRunLeavesOut(t *testing.T) {
	e := setup(t)
	empty := e.run(t)
	if got := fmt.Sprint(empty["status"], " ", empty["materials_planned"], " ", len(rows(t, empty, "lines"))); got != "completed 0 0" {
		t.Errorf("a plant with nothing planned: got %s, want completed 0 0", got)
	}
	_, tag := e.plan(t, e.bracket, "200")
	if got := line(t, e.run(t), e.bracket); got != "0 200 0 0 -" {
		t.Errorf("at the reorder point: got %s, want nothing proposed", got)
	}
	p, _ := e.Must(200, "GET", plans+"?material_id="+e.bracket, "", "")
	id := rows(t, p, "items")[0]["id"].(string)
	e.Must(200, "PATCH", plans+"/"+id, `{"mrp_type": "no_planning"}`, tag)
	if r := e.run(t); len(rows(t, r, "lines")) != 0 {
		t.Errorf("a material planned no more: got %v, want no line", rows(t, r, "lines"))
	}
	pgtest.Exec(t, `UPDATE mrp.material_plan SET mrp_type = 'reorder_point', version = version + 1 WHERE id = $1`, id)
	pgtest.Exec(t, `UPDATE materials.material SET status = 'blocked', version = version + 1 WHERE id = $1`, e.bracket)
	if r := e.run(t); len(rows(t, r, "lines")) != 0 {
		t.Errorf("a blocked material: got %v, want no line", rows(t, r, "lines"))
	}
}

// TestPlanningRunRefusals (ADR-0084 §3): a run of a plant that is not an
// active plant of the caller's companies is refused; one of a plant with
// a run queued is refused naming it; a run whose Operation was cancelled
// fails with mrp.run_cancelled and proposes nothing; a material with
// demand in a unit it no longer has fails the run.
func TestPlanningRunRefusals(t *testing.T) {
	e := setup(t)
	e.plan(t, e.bracket, "50")
	for name, plant := range map[string]uuid.UUID{"unknown": uuid.New(), "a storage location": e.s1} {
		t.Run(name, func(t *testing.T) {
			p := e.Problem(422, "mrp.plant_invalid", "POST", runs, mustJSON(t, map[string]any{"plant_id": plant}), "")
			if field(p) != "plant_id" {
				t.Errorf("got field %v, want plant_id", field(p))
			}
		})
	}
	in := e.start(t)
	p := e.Problem(409, "mrp.run_in_progress", "POST", runs, mustJSON(t, map[string]any{"plant_id": e.p1}), "")
	if d := fmt.Sprint(p["detail"]); d != fmt.Sprintf("Planning run /v1/mrp-runs/%s of plant P1 is queued; one run of a plant is queued at a time.", in.RunID) {
		t.Errorf("the detail: got %q, want the queued run named", d)
	}
	op, tag := e.Must(200, "GET", "/v1/operations/"+in.OperationID.String(), "", "")
	if op["kind"] != "mrp.run" || op["status"] != "queued" {
		t.Errorf("the operation: got %v %v, want a queued mrp.run", op["kind"], op["status"])
	}
	e.Must(200, "POST", "/v1/operations/"+in.OperationID.String()+":cancel", "", tag)
	if err := planner(t).Plan(context.Background(), in); err != nil {
		t.Fatalf("plan: %v", err)
	}
	r, _ := e.Must(200, "GET", runs+"/"+in.RunID.String(), "", "")
	if got := fmt.Sprint(r["status"], " ", r["error"].(map[string]any)["code"]); got != "failed mrp.run_cancelled" {
		t.Errorf("the cancelled run: got %s, want failed with mrp.run_cancelled", got)
	}
	if got := e.proposals(t); len(got) != 0 {
		t.Errorf("the cancelled run's proposals: got %v, want none", got)
	}
	if op, _ := e.Must(200, "GET", "/v1/operations/"+in.OperationID.String(), "", ""); op["status"] != "cancelled" {
		t.Errorf("the operation: got %v, want it left cancelled", op["status"])
	}
	e.confirmed(t, "1 BOX")
	pgtest.Exec(t, `DELETE FROM materials.unit_conversion WHERE material_id = $1`, e.bracket)
	in = e.start(t)
	if err := planner(t).Plan(context.Background(), in); err != nil {
		t.Fatalf("plan: %v", err)
	}
	r, _ = e.Must(200, "GET", runs+"/"+in.RunID.String(), "", "")
	if got := fmt.Sprint(r["status"], " ", r["error"].(map[string]any)["code"]); got != "failed mrp.unit_not_convertible" {
		t.Errorf("a unit no longer the material's: got %s, want failed mrp.unit_not_convertible", got)
	}
	if op, _ := e.Must(200, "GET", "/v1/operations/"+in.OperationID.String(), "", ""); op["status"] != "failed" {
		t.Errorf("its operation: got %v, want failed", op["status"])
	}
}

// TestTakenOverInARemovedUnit: a proposal a person took over in a unit
// the material no longer has cannot be counted, so the run fails rather
// than plan with less than is on order.
func TestTakenOverInARemovedUnit(t *testing.T) {
	e := setup(t)
	e.plan(t, e.bracket, "250")
	r1 := rows(t, e.run(t), "lines")[0]["requisition_id"].(string)
	_, tag := e.requisition(t, r1)
	e.Must(200, "PATCH", "/v1/requisitions/"+r1, fmt.Sprintf(`{"items": [{"material_id": %q, "quantity": {"value": "3", "unit": "BOX"},
		"unit_price": {"amount": "40.00", "currency": "EUR"}, "account_id": %q}]}`, e.bracket, e.stock), tag)
	pgtest.Exec(t, `DELETE FROM materials.unit_conversion WHERE material_id = $1`, e.bracket)
	r := e.run(t)
	if got := fmt.Sprint(r["status"], " ", r["error"].(map[string]any)["code"]); got != "failed mrp.unit_not_convertible" {
		t.Errorf("got %s, want failed mrp.unit_not_convertible", got)
	}
}
