package mrp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/mrp"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/workflows"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"materials.material.read", "materials.material.write", "tenancy.org.manage", "inventory.movement.post", "inventory.stock.read",
	"inventory.valuation.read", "inventory.valuation.write", "sales.order.read", "sales.order.write", "sales.order.confirm", "procurement.requisition.read",
	"procurement.requisition.create", "mrp.plan.read", "mrp.plan.write", "mrp.run.start", "platform.operation.read", "platform.operation.cancel",
	"mrp.sop.read", "mrp.sop.write"}

const (
	plans = "/v1/material-plans"
	runs  = "/v1/mrp-runs"
)

// env is a configured tenant whose company 2000 has plant P1 (storage
// location S1) and company 1000 plant P9; the bracket, a trading good kept
// at S1 by the EA or the box of 20, valued at a moving price of 2.00 EUR
// with 200 EA at S1; a service material; Northwind, a customer of 2000;
// its revenue account and its inventory account, which plans name.
type env struct {
	fixture.Env
	p1, s1, p9       uuid.UUID
	bracket, service string
	customer         uuid.UUID
	revenue, stock   uuid.UUID
}

func setup(t *testing.T) env {
	t.Helper()
	e := env{Env: fixture.NewWith(t, nil, valued, perms...)}
	e.p1 = e.orgUnit(t, e.DE, "plant", "P1", nil)
	e.s1 = e.orgUnit(t, e.DE, "storage_location", "S1", &e.p1)
	e.p9 = e.orgUnit(t, e.US, "plant", "P9", nil)
	e.bracket = e.material(t, "Machined bracket M10", "trading_good")
	e.service = e.material(t, "Installation service", "service")
	e.Must(201, "POST", "/v1/material-valuations", mustJSON(t, map[string]any{"material_id": e.bracket, "plant_id": e.p1, "valuation_class": "3000",
		"price_control": "moving_average", "price": map[string]any{"amount": "2.00", "currency": "EUR"}}), "")
	e.receive(t, "200")
	e.revenue, e.stock = e.Account(t, "2000", "400000"), e.Account(t, "2000", "150000")
	e.customer = e.Customer(t, "BP70000001", "Northwind Traders")
	pgtest.Exec(t, `INSERT INTO partner.customer_company (tenant_id, partner_id, legal_entity_id, reconciliation_account_id, payment_term_code, created_by,
		updated_by) VALUES ($1, $2, $3, $4, 'NT30', $5, $5)`, e.Tenant.ID, e.customer, e.DE, e.Account(t, "2000", "120000"), e.Tenant.Human.ID)
	return e
}

// valued gives company 2000 a customer reconciliation account and the
// inventory accounts of valuation class 3000.
func valued(c *config.Config) {
	de := c.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "120000", Name: "Trade receivables", Type: config.Asset, CustomerReconciliation: true},
		config.Account{Number: "150000", Name: "Inventory", Type: config.Asset}, config.Account{Number: "391000", Name: "Initial entries", Type: config.Equity},
		config.Account{Number: "515000", Name: "Consumption", Type: config.Expense})
	de.AccountDetermination.Inventory = []config.InventoryAccounts{{ValuationClass: "3000", Stock: "150000", InitialEntry: "391000", Consumption: "515000"}}
}

// orgUnit creates an active org unit of the company through the API.
func (e env) orgUnit(t *testing.T, le uuid.UUID, kind, code string, parent *uuid.UUID) uuid.UUID {
	t.Helper()
	body := map[string]any{"legal_entity_id": le, "kind": kind, "code": code, "name": "Unit " + code}
	if parent != nil {
		body["parent_id"] = *parent
	}
	got, _ := e.Must(201, "POST", "/v1/org-units", mustJSON(t, body), "")
	id := got["id"].(string)
	e.Must(200, "POST", "/v1/org-units/"+id+":activate", "", `"1"`)
	return uuid.MustParse(id)
}

// material creates an approved material of company 2000 kept at S1 of P1
// and returns its id.
func (e env) material(t *testing.T, description, kind string) string {
	t.Helper()
	m, _ := e.Must(201, "POST", "/v1/materials", mustJSON(t, map[string]any{"legal_entity_id": e.DE, "description": description, "base_unit": "EA",
		"material_type": kind, "material_group": "BRACKETS", "confirm_duplicate": true,
		"plants":            []any{map[string]any{"plant_id": e.p1, "storage_location_ids": []any{e.s1}}},
		"alternative_units": []any{map[string]any{"unit": "BOX", "numerator": 20, "denominator": 1}}}), "")
	e.Approve(t, m["pending_change_id"].(string))
	return m["id"].(string)
}

// receive enters an initial stock of the bracket in EA at S1.
func (e env) receive(t *testing.T, qty string) {
	t.Helper()
	e.Must(201, "POST", "/v1/material-documents", mustJSON(t, map[string]any{"movement_type": "561", "items": []any{map[string]any{"material_id": e.bracket,
		"plant_id": e.p1, "storage_location_id": e.s1, "quantity": map[string]any{"value": qty, "unit": "EA"}}}}), "")
}

// confirmed creates and confirms Northwind's order of the bracket from P1,
// "quantity unit", and returns its id.
func (e env) confirmed(t *testing.T, quantity string) string {
	t.Helper()
	f := strings.Fields(quantity)
	o, _ := e.Must(201, "POST", "/v1/sales-orders", fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "currency": "EUR", "plant_id": %q,
		"items": [{"material_id": %q, "quantity": {"value": %q, "unit": %q}, "unit_price": {"amount": "1.00", "currency": "EUR"}, "account_id": %q}]}`,
		e.DE, e.customer, e.p1, e.bracket, f[0], f[1], e.revenue), "")
	id := o["id"].(string)
	e.Must(200, "POST", "/v1/sales-orders/"+id+":confirm", "", `"1"`)
	return id
}

// ordered writes a purchase order of the bracket for P1/S1 in the status,
// of the quantity in EA, straight into procurement, and returns its id.
func (e env) ordered(t *testing.T, number, status, qty string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
		payment_term_code, status, created_by, updated_by) VALUES ($1, $2, $3, $4, gen_random_uuid(), 'EUR', '2026-11-01', 'NT30', 'draft', $5, $5)`,
		e.Tenant.ID, id, number, e.DE, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, material_id, description, quantity, uom_code,
		unit_price, price_unit, account_id, plant_id, storage_location_id) VALUES ($1, $2, 1, $3, 'Bracket', $4::numeric, 'EA', 200, 1, $5, $6, $7)`,
		e.Tenant.ID, id, e.bracket, qty, e.stock, e.p1, e.s1)
	for _, s := range map[string][]string{"released": {"released"}, "closed": {"released", "closed"}}[status] {
		pgtest.Exec(t, `UPDATE procurement.purchase_order SET status = $2, version = version + 1 WHERE id = $1`, id, s)
	}
	return id
}

// plan plans the bracket at P1 by its reorder point and returns the plan.
func (e env) plan(t *testing.T, material, reorderPoint string) (map[string]any, string) {
	t.Helper()
	return e.Must(201, "POST", plans, mustJSON(t, map[string]any{"plant_id": e.p1, "material_id": material, "mrp_type": "reorder_point",
		"reorder_point": reorderPoint, "account_id": e.stock}), "")
}

// planner is the in-process planning run worker over the package
// database, the mrp context wired as the API wires it.
func planner(t *testing.T) *workflows.Planner {
	t.Helper()
	return &workflows.Planner{Mrps: &workflows.Mrps{Pool: pgtest.Pool(t), AppRole: "erp_app", WorkerRole: "erp_worker", Runs: app.MRP(app.Config{}),
		Actors: identity.Actors{}}}
}

// start starts a run of P1 and returns the workflow's input.
func (e env) start(t *testing.T) workflows.MrpInput {
	t.Helper()
	op, _ := e.Must(202, "POST", runs, mustJSON(t, map[string]any{"plant_id": e.p1}), "")
	run := uuid.MustParse(strings.TrimPrefix(op["resource"].(string), runs+"/"))
	return workflows.MrpInput{TenantID: e.Tenant.ID, RunID: run, OperationID: uuid.MustParse(op["id"].(string))}
}

// run starts a run of P1, carries it out and returns it.
func (e env) run(t *testing.T) map[string]any {
	t.Helper()
	in := e.start(t)
	if err := planner(t).Plan(context.Background(), in); err != nil {
		t.Fatalf("plan: %v", err)
	}
	r, _ := e.Must(200, "GET", runs+"/"+in.RunID.String(), "", "")
	return r
}

// line is a run's line of the material as "demand stock receipts
// shortage requisition", "-" for no requisition.
func line(t *testing.T, r map[string]any, material string) string {
	t.Helper()
	for _, l := range rows(t, r, "lines") {
		if l["material_id"] == material {
			req, _ := l["requisition_number"].(string)
			if req == "" {
				req = "-"
			}
			q := func(f string) any { return l[f].(map[string]any)["value"] }
			return fmt.Sprintf("%v %v %v %v %s", q("demand"), q("stock"), q("receipts"), q("shortage"), req)
		}
	}
	return "none"
}

// rows are the objects of the array field of m.
func rows(t *testing.T, m map[string]any, field string) []map[string]any {
	t.Helper()
	raw, _ := m[field].([]any)
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		out[i], _ = r.(map[string]any)
	}
	return out
}

// field is the field the problem's first field error names.
func field(m map[string]any) any {
	errs, _ := m["errors"].([]any)
	if len(errs) == 0 {
		return nil
	}
	e, _ := errs[0].(map[string]any)
	return e["field"]
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// endpoints are the context's endpoints as the API wires them.
func endpoints(t *testing.T) *mrp.Endpoints {
	t.Helper()
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return app.MRP(app.Config{Pages: pages})
}
