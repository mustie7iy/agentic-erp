package mrp_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/mrp"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

const sops = "/v1/sop-plans"

// issue posts a goods issue of the bracket in EA from S1 to a cost center
// of company 2000 (201), today.
func (e env) issue(t *testing.T, qty string) {
	t.Helper()
	cc := e.CostCenterFrom(t, e.DE, "C"+qty, "2026-01-01", nil)
	e.Must(201, "POST", "/v1/material-documents", mustJSON(t, map[string]any{"movement_type": "201", "items": []any{map[string]any{"material_id": e.bracket,
		"plant_id": e.p1, "storage_location_id": e.s1, "quantity": map[string]any{"value": qty, "unit": "EA"}, "cost_center_id": cc}}}), "")
}

// review starts a review of company 2000 over the months and returns it.
func (e env) review(t *testing.T, months int) (map[string]any, string) {
	t.Helper()
	return e.Must(201, "POST", sops, mustJSON(t, map[string]any{"legal_entity_id": e.DE, "months": months}), "")
}

// sopLine is the plan's line of the key as "forecast orders opening
// purchases transports proposals consensus projected", "none" without it.
func sopLine(t *testing.T, p map[string]any, key string) string {
	t.Helper()
	for _, l := range rows(t, p, "lines") {
		if l["key"] == key {
			q := func(f string) any { return l[f].(map[string]any)["value"] }
			return fmt.Sprintf("%v %v %v %v %v %v %v %v", q("forecast"), q("orders"), q("opening_stock"), q("purchase_orders"),
				q("stock_transports"), q("proposals"), q("consensus"), q("projected_stock"))
		}
	}
	return "none"
}

// months are the plan's months, the first's days and the second's, from
// its as_of.
func months(t *testing.T, p map[string]any) (first, second string, d1, d2 int) {
	t.Helper()
	asOf, err := time.Parse(time.DateOnly, p["as_of"].(string))
	if err != nil {
		t.Fatal(err)
	}
	start := asOf.AddDate(0, 0, 1)
	m1 := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC)
	m2 := m1.AddDate(0, 1, 0)
	return m1.Format("2006-01"), m2.Format("2006-01"), m2.AddDate(0, 0, -1).Day() - start.Day() + 1, m2.AddDate(0, 1, -1).Day()
}

// TestSopReview (S2n; ADR-0084 As built S2n): a review of company 2000
// counts the brackets group at P1 by month: the forecast the greater of
// the moving average of the 91 EA issued in the 13 weeks (1 a day) over
// the month's days and the confirmed order of 2 BOX (40 EA) due now, in
// the first month; the stock, the open purchase order and the planning
// run's proposal in the first month; a second bracket planned but never
// issued counts as a material of the group. The planner sets the second
// month's consensus, which moves its projected stock; a fixed version
// changes no more, and the next review is version 2.
func TestSopReview(t *testing.T) {
	e := setup(t)
	e.plan(t, e.bracket, "250")
	e.plan(t, e.material(t, "Second bracket", "raw_material"), "0")
	e.issue(t, "91")
	e.confirmed(t, "2 BOX")
	e.ordered(t, "PO1", "released", "30")
	run := e.run(t) // 109 in stock, 30 on order, 40 demanded: 151 proposed up to 250
	if got := line(t, run, e.bracket); got != "40 109 30 151 PR100000" {
		t.Fatalf("the run: got %s, want 40 109 30 151 PR100000", got)
	}
	p, tag := e.review(t, 2)
	m1, m2, d1, d2 := months(t, p)
	f1 := max(d1, 40)
	want := map[string]string{
		"BRACKETS/EA/" + m1: fmt.Sprintf("%d 40 109 30 0 151 %d %d", f1, f1, 290-f1),
		"BRACKETS/EA/" + m2: fmt.Sprintf("%d 0 %d 0 0 0 %d %d", d2, 290-f1, d2, 290-f1-d2),
	}
	for key, w := range want {
		if got := sopLine(t, p, key); got != w {
			t.Errorf("%s: got %s, want %s", key, got, w)
		}
	}
	if got := fmt.Sprintf("%v %v %v %v", p["number"], p["status"], len(rows(t, p, "lines")), rows(t, p, "lines")[0]["materials"]); got != "1 draft 2 2" {
		t.Errorf("the version: got %s, want 1 draft 2 2", got)
	}
	key2 := "BRACKETS/EA/" + m2
	p, tag = e.Must(200, "PATCH", sops+"/"+p["id"].(string), fmt.Sprintf(`{"consensus": {%q: "10.5"}}`, key2), tag)
	if got, w := sopLine(t, p, key2), fmt.Sprintf("%d 0 %d 0 0 0 10.5 %d.5", d2, 290-f1, 290-f1-11); got != w {
		t.Errorf("the second month after the change: got %s, want %s", got, w)
	}
	id := p["id"].(string)
	for name, c := range map[string]struct {
		body, code string
	}{
		"unchanged": {fmt.Sprintf(`{"consensus": {%q: "10.50"}}`, key2), "mrp.nothing_to_change"},
		"unknown":   {`{"consensus": {"BOLTS/EA/2026-01": "1"}}`, "mrp.sop_line_unknown"},
	} {
		t.Run(name, func(t *testing.T) {
			if status, got, _ := e.Do("PATCH", sops+"/"+id, c.body, tag); status != 422 || got["code"] != c.code {
				t.Errorf("got %d %v, want 422 %s", status, got["code"], c.code)
			}
		})
	}
	if status, _, _ := e.Do("PATCH", sops+"/"+id, fmt.Sprintf(`{"consensus": {%q: "1"}}`, key2), `"1"`); status != 412 {
		t.Errorf("a stale version: got %d, want 412", status)
	}
	fixed, ftag := e.Must(200, "POST", sops+"/"+id+":fix", "", tag)
	if fixed["status"] != "fixed" || fixed["fixed_by"] != e.Tenant.Human.ID.String() || fixed["fixed_at"] == nil {
		t.Errorf("fixed: got %v by %v at %v, want fixed by the person", fixed["status"], fixed["fixed_by"], fixed["fixed_at"])
	}
	for name, do := range map[string]func() (int, map[string]any, string){
		"changed": func() (int, map[string]any, string) {
			return e.Do("PATCH", sops+"/"+id, fmt.Sprintf(`{"consensus": {%q: "1"}}`, key2), ftag)
		},
		"fixed again": func() (int, map[string]any, string) { return e.Do("POST", sops+"/"+id+":fix", "", ftag) },
	} {
		t.Run(name, func(t *testing.T) {
			if status, got, _ := do(); status != 409 || got["code"] != "mrp.sop_plan_fixed" {
				t.Errorf("got %d %v, want 409 mrp.sop_plan_fixed", status, got["code"])
			}
		})
	}
	next, _ := e.review(t, 1)
	if next["number"] != float64(2) || len(rows(t, next, "lines")) != 1 {
		t.Errorf("the next review: got number %v with %d lines, want 2 with 1", next["number"], len(rows(t, next, "lines")))
	}
	list, _ := e.Must(200, "GET", sops+"?status=fixed&legal_entity_id="+e.DE.String(), "", "")
	if items := rows(t, list, "items"); len(items) != 1 || items[0]["id"] != id || sopLine(t, items[0], key2) != sopLine(t, fixed, key2) {
		t.Errorf("the fixed versions: got %v, want the first, as it was fixed", items)
	}
	var audited int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action IN ('mrp.sop_plan.create', 'mrp.sop_plan.consensus',
		'mrp.sop_plan.fix')`, []any{e.Tenant.ID}, &audited)
	if audited != 4 {
		t.Errorf("audited: got %d, want 4 (two reviews, a change, a fix)", audited)
	}
}

// TestSopSupply (S2n): the first month counts the stock in transit to the
// plant (a stock transport order within the company: 12) and the
// proposals still to be ordered at the plant, a person's changed one with
// its items of materials planned there only; a cancelled one counts
// nothing. An intercompany order P1 supplies and has not issued (5) is an
// order due, which the forecast is never below, not a negative receipt.
func TestSopSupply(t *testing.T) {
	e := setup(t)
	e.plan(t, e.bracket, "250")
	other := e.material(t, "Unplanned bracket", "raw_material")
	pgtest.Exec(t, `INSERT INTO procurement.stock_transport_order (tenant_id, id, legal_entity_id, fiscal_year, number, supplying_plant_id, receiving_plant_id,
		order_date, status, issue_document_id, issue_document_number, issue_date, created_by, updated_by)
		VALUES ($1, $2, $3, 2026, 'ST1', gen_random_uuid(), $4, '2026-11-01', 'in_transit', gen_random_uuid(), 'MD1', '2026-11-01', $5, $5)`,
		e.Tenant.ID, uuid.Must(uuid.NewV7()), e.DE, e.p1, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO procurement.stock_transport_order_item (tenant_id, order_id, item_no, material_id, quantity, uom_code, base_quantity,
		supplying_storage_location_id, receiving_storage_location_id)
		SELECT $1, id, 1, $2, 12, 'EA', 12, gen_random_uuid(), $3 FROM procurement.stock_transport_order WHERE tenant_id = $1`, e.Tenant.ID, e.bracket, e.s1)
	ic := uuid.Must(uuid.NewV7()) // P9's intercompany order from P1, not yet issued
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
		payment_term_code, status, supplying_plant_id, created_by, updated_by)
		VALUES ($1, $2, 'PO2', $3, gen_random_uuid(), 'USD', '2026-11-01', 'NT30', 'draft', $4, $5, $5)`, e.Tenant.ID, ic, e.US, e.p1, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, material_id, description, quantity, uom_code,
		unit_price, price_unit, account_id, plant_id, storage_location_id) VALUES ($1, $2, 1, $3, 'Bracket', 5, 'EA', 200, 1, $4, $5, gen_random_uuid())`,
		e.Tenant.ID, ic, e.bracket, e.stock, e.p9)
	first := rows(t, e.run(t), "lines")[0]["requisition_id"].(string) // 200 in stock: 50 proposed
	_, rtag := e.requisition(t, first)
	e.Must(200, "PATCH", "/v1/requisitions/"+first, fmt.Sprintf(`{"items": [%s, %s]}`, item(e, e.bracket, "50"), item(e, other, "7")), rtag)
	if second := rows(t, e.run(t), "lines")[0]["requisition_id"]; second != nil { // the first taken over (50) on 200: none
		t.Fatalf("the second run proposed %v, want nothing", second)
	}
	p, _ := e.review(t, 1)
	m1, _, _, _ := months(t, p)
	if got := sopLine(t, p, "BRACKETS/EA/"+m1); got != "5 5 200 0 12 50 5 257" {
		t.Errorf("the first month: got %s, want 5 5 200 0 12 50 5 257", got)
	}
	_, rtag = e.requisition(t, first)
	e.Must(200, "POST", "/v1/requisitions/"+first+":cancel", "", rtag)
	p, _ = e.review(t, 1)
	if got := sopLine(t, p, "BRACKETS/EA/"+m1); got != "5 5 200 0 12 0 5 207" {
		t.Errorf("after the cancellation: got %s, want 5 5 200 0 12 0 5 207", got)
	}
}

// item is a requisition item of the material in EA at 2.00 EUR.
func item(e env, material, qty string) string {
	return fmt.Sprintf(`{"material_id": %q, "quantity": {"value": %q, "unit": "EA"}, "unit_price": {"amount": "2.00", "currency": "EUR"}, "account_id": %q}`,
		material, qty, e.stock)
}

// TestSopRefusals (S2n): a company the caller may not act in or none, a
// horizon outside 1 to 12 months and a company planning nothing are
// refused naming the field; a version of another company is not found.
func TestSopRefusals(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	for name, c := range map[string]struct {
		in   mrp.SopInput
		code string
	}{
		"no company":     {mrp.SopInput{LegalEntityID: uuid.New()}, "mrp.company_invalid"},
		"13 months":      {mrp.SopInput{LegalEntityID: e.DE, Months: 13}, "mrp.invalid_horizon"},
		"no months":      {mrp.SopInput{LegalEntityID: e.DE, Months: -1}, "mrp.invalid_horizon"},
		"nothing to see": {mrp.SopInput{LegalEntityID: e.US}, "mrp.sop_plan_empty"},
	} {
		t.Run(name, func(t *testing.T) {
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { _, err := h.StartSop(ctx, c.in); return err })
			if codeOf(err) != c.code {
				t.Errorf("got %v, want %s", err, c.code)
			}
		})
	}
	e.plan(t, e.bracket, "0")
	p, tag := e.review(t, 1)
	us := e.As(e.Reader(t, "Una", e.US))
	if status, _, _ := us.Do("GET", sops+"/"+p["id"].(string), "", ""); status != 404 {
		t.Errorf("another company's version: got %d, want 404", status)
	}
	if status, got, _ := us.Do("POST", sops, mustJSON(t, map[string]any{"legal_entity_id": e.DE}), ""); status != 422 || got["code"] != "mrp.company_invalid" {
		t.Errorf("a review of another company: got %d %v, want 422 mrp.company_invalid", status, got["code"])
	}
	if list, _ := us.Must(200, "GET", sops, "", ""); len(rows(t, list, "items")) != 0 {
		t.Errorf("another company's list: got %v, want none", list["items"])
	}
	m1, _, _, _ := months(t, p)
	for _, q := range []string{"-1", "1.0000001"} {
		t.Run(q, func(t *testing.T) {
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				_, err := h.SetConsensus(ctx, uuid.MustParse(p["id"].(string)), etag.IfMatch(tag), map[string]string{"BRACKETS/EA/" + m1: q})
				return err
			})
			if codeOf(err) != "mrp.invalid_quantity" {
				t.Errorf("got %v, want mrp.invalid_quantity", err)
			}
		})
	}
	if p := problem.From(&pgconn.PgError{Code: "23505", ConstraintName: "sop_plan_number_once"}); p.Status != 409 || p.Code != "mrp.sop_plan_concurrent" {
		t.Errorf("two reviews at once: got %d %s, want 409 mrp.sop_plan_concurrent", p.Status, p.Code)
	}
}

// odd is the mrp context's supply answering each read with a quantity in
// a unit no material has, the read named by which.
type odd struct {
	mrp.Supply
	which string
}

var xx = mrp.Open{Unit: "XX", Quantity: "1"}

func (o odd) with(id uuid.UUID) []mrp.Open { x := xx; x.MaterialID = id; return []mrp.Open{x} }

func (o odd) DemandDue(ctx context.Context, plant uuid.UUID, ms []uuid.UUID) ([]mrp.Due, error) {
	if o.which == "demand" {
		return []mrp.Due{{Open: o.with(ms[0])[0], Day: time.Time{}}}, nil
	}
	return o.Supply.DemandDue(ctx, plant, ms)
}

func (o odd) Receipts(ctx context.Context, plant uuid.UUID, ms []uuid.UUID) ([]mrp.Open, error) {
	if o.which == "receipts" {
		return o.with(ms[0]), nil
	}
	return o.Supply.Receipts(ctx, plant, ms)
}

func (o odd) Requisitions(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]mrp.Requisition, error) {
	got, err := o.Supply.Requisitions(ctx, ids)
	if o.which == "proposals" {
		for id, r := range got {
			for i := range r.Items {
				r.Items[i].Unit = "XX"
			}
			got[id] = r
		}
	}
	return got, err
}

// TestSopUnitNotConvertible (S2n): a quantity in a unit the material no
// longer has, in its orders, receipts or proposals, refuses the review
// (409 mrp.unit_not_convertible) rather than count less than is there.
func TestSopUnitNotConvertible(t *testing.T) {
	e := setup(t)
	e.plan(t, e.bracket, "250")
	e.run(t)
	for _, which := range []string{"demand", "receipts", "proposals"} {
		t.Run(which, func(t *testing.T) {
			h := endpoints(t)
			h.Supply = odd{Supply: h.Supply, which: which}
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				_, err := h.StartSop(ctx, mrp.SopInput{LegalEntityID: e.DE})
				return err
			})
			var p *problem.Problem
			if !errors.As(err, &p) || p.Code != "mrp.unit_not_convertible" || p.Status != 409 {
				t.Errorf("got %v, want 409 mrp.unit_not_convertible", err)
			}
		})
	}
}

// TestSopDatabaseFailures calls every S&OP handler outside a unit of work
// and with each of its database statements failing in turn.
func TestSopDatabaseFailures(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	e.plan(t, e.bracket, "250")
	e.confirmed(t, "1 BOX")
	e.ordered(t, "PO1", "released", "5")
	e.run(t)
	p, tag := e.review(t, 2)
	id := uuid.MustParse(p["id"].(string))
	m1, _, _, _ := months(t, p)
	calls := map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreateSopPlan(ctx, &gen.SopPlanCreate{LegalEntityID: e.DE}, gen.CreateSopPlanParams{})
			return err
		},
		"get": func(ctx context.Context) error { _, err := h.GetSopPlan(ctx, gen.GetSopPlanParams{ID: id}); return err },
		"update": func(ctx context.Context) error {
			_, err := h.UpdateSopPlan(ctx, &gen.SopPlanUpdate{Consensus: gen.SopPlanUpdateConsensus{"BRACKETS/EA/" + m1: "3"}},
				gen.UpdateSopPlanParams{ID: id, IfMatch: tag})
			return err
		},
		"fix": func(ctx context.Context) error {
			_, err := h.FixSopPlan(ctx, gen.FixSopPlanParams{ID: id, IfMatch: tag})
			return err
		},
		"list": func(ctx context.Context) error { _, err := h.ListSopPlans(ctx, gen.ListSopPlansParams{}); return err },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
	ctx, ids := context.Background(), []uuid.UUID{uuid.New()}
	for name, call := range map[string]func() error{
		"issues": func() error {
			_, err := app.Inventory(app.Config{}).PlantIssues(ctx, ids[0], ids, time.Now(), time.Now())
			return err
		},
		"demand due": func() error { _, err := sales.Orders{}.PlantRequirementsDue(ctx, ids[0], ids); return err },
		"transports":   func() error { _, err := procurement.Orders{}.TransportReceipts(ctx, ids[0], ids); return err },
		"intercompany": func() error { _, err := procurement.Orders{}.IntercompanyDue(ctx, ids[0], ids); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
		})
	}
}

// TestSopScope (S2n): a review counts the planned materials still active,
// by group (BOLTS before BRACKETS) and unit, and leaves out an order due
// after the horizon; versions list in the order they were started, page
// by page.
func TestSopScope(t *testing.T) {
	e := setup(t)
	e.plan(t, e.bracket, "0")
	bolt, _ := e.Must(201, "POST", "/v1/materials", mustJSON(t, map[string]any{"legal_entity_id": e.DE, "description": "Bolt M10", "base_unit": "EA",
		"material_type": "trading_good", "material_group": "BOLTS", "plants": []any{map[string]any{"plant_id": e.p1, "storage_location_ids": []any{e.s1}}}}), "")
	e.Approve(t, bolt["pending_change_id"].(string))
	e.plan(t, bolt["id"].(string), "0")
	blocked := e.material(t, "Blocked bracket", "raw_material")
	e.plan(t, blocked, "0")
	pgtest.Exec(t, `UPDATE materials.material SET status = 'blocked', version = version + 1 WHERE id = $1`, blocked)
	o, _ := e.Must(201, "POST", "/v1/sales-orders", fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "currency": "EUR", "plant_id": %q,
		"requested_delivery_date": "2030-01-01", "items": [{"material_id": %q, "quantity": {"value": "1", "unit": "BOX"},
		"unit_price": {"amount": "1.00", "currency": "EUR"}, "account_id": %q}]}`, e.DE, e.customer, e.p1, e.bracket, e.revenue), "")
	e.Must(200, "POST", "/v1/sales-orders/"+o["id"].(string)+":confirm", "", `"1"`)
	p, _ := e.review(t, 1)
	m1, _, _, _ := months(t, p)
	var keys []string
	for _, l := range rows(t, p, "lines") {
		keys = append(keys, fmt.Sprintf("%v:%v", l["key"], l["materials"]))
	}
	if got, want := fmt.Sprint(keys), fmt.Sprintf("[BOLTS/EA/%s:1 BRACKETS/EA/%s:1]", m1, m1); got != want {
		t.Errorf("the lines: got %s, want %s", got, want)
	}
	if got := sopLine(t, p, "BRACKETS/EA/"+m1); got != "0 0 200 0 0 0 0 200" {
		t.Errorf("the brackets: got %s, want 0 0 200 0 0 0 0 200 (the order due after the horizon left out)", got)
	}
	second, _ := e.review(t, 1)
	both, _ := e.Must(200, "GET", sops+"?page_size=2", "", "")
	if items := rows(t, both, "items"); len(items) != 2 || items[0]["company_code"] != "2000" || items[1]["company_code"] != "2000" {
		t.Errorf("both versions: got %v, want two of company 2000", items)
	}
	page, _ := e.Must(200, "GET", sops+"?page_size=1", "", "")
	next, _ := e.Must(200, "GET", sops+"?page_size=1&page_token="+page["next_page_token"].(string), "", "")
	if items := rows(t, next, "items"); len(items) != 1 || items[0]["id"] != second["id"] {
		t.Errorf("the second page: got %v, want the second version", items)
	}
	if status, _, _ := e.Do("GET", sops+"?page_token=x", "", ""); status != 400 {
		t.Errorf("a bad page token: got %d, want 400", status)
	}
}
