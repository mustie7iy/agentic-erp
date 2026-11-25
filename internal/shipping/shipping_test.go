package shipping_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/shipping"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"materials.material.read", "materials.material.write", "tenancy.org.manage", "inventory.movement.post", "inventory.stock.read",
	"inventory.valuation.read", "inventory.valuation.write", "ledger.entry.read", "sales.order.read", "sales.order.write", "sales.order.confirm",
	"sales.delivery.confirm", "sales.billing.read", "sales.billing.write", "shipping.delivery.read", "shipping.delivery.write", "shipping.goods_issue.post",
	"inventory.count.write", "shipping.trade_compliance.release", "sales.billing.post",
	"inventory.count_plan.write", "inventory.batch.write"}

const deliveries = "/v1/outbound-deliveries"

// env is a configured tenant whose company 2000 has plant P1 (storage
// locations S1 and S2) and company 1000 plant P9 (S9); the bracket, a
// trading good kept at S1 of P1 alone, by the box of 20 or by the KG (a
// third of an EA), valued at a moving price of 2.00 EUR with 200 EA at S1;
// a service material; and Northwind, a customer of company 2000.
type env struct {
	fixture.Env
	p1, s1, s2, p9, s9 uuid.UUID
	bracket, service   string
	customer           uuid.UUID
	revenue            uuid.UUID
}

func setup(t *testing.T) env {
	t.Helper()
	e := env{Env: fixture.NewWith(t, nil, valued, perms...)}
	e.p1 = e.orgUnit(t, e.DE, "plant", "P1", nil)
	e.s1 = e.orgUnit(t, e.DE, "storage_location", "S1", &e.p1)
	e.s2 = e.orgUnit(t, e.DE, "storage_location", "S2", &e.p1)
	e.p9 = e.orgUnit(t, e.US, "plant", "P9", nil)
	e.s9 = e.orgUnit(t, e.US, "storage_location", "S9", &e.p9)
	e.bracket = e.material(t, "Machined bracket M10", "trading_good")
	e.service = e.material(t, "Installation service", "service")
	e.value(t, e.bracket, "3000", "moving_average", "2.00")
	e.receive(t, "200")
	e.revenue = e.Account(t, "2000", "400000")
	e.customer = e.Customer(t, "BP70000001", "Northwind Traders")
	pgtest.Exec(t, `INSERT INTO partner.customer_company (tenant_id, partner_id, legal_entity_id, reconciliation_account_id, payment_term_code, created_by,
		updated_by) VALUES ($1, $2, $3, $4, 'NT30', $5, $5)`, e.Tenant.ID, e.customer, e.DE, e.Account(t, "2000", "120000"), e.Tenant.Human.ID)
	return e
}

// valued gives company 2000 a customer reconciliation account, the
// inventory accounts of valuation class 3000, of class 3100, which names
// no cost of goods sold account, and of class 3200, which names price
// difference and revaluation accounts too.
func valued(c *config.Config) {
	de := c.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "120000", Name: "Trade receivables", Type: config.Asset, CustomerReconciliation: true},
		config.Account{Number: "150000", Name: "Inventory", Type: config.Asset}, config.Account{Number: "391000", Name: "Initial entries", Type: config.Equity},
		config.Account{Number: "515000", Name: "Consumption", Type: config.Expense}, config.Account{Number: "519000", Name: "Cost of goods sold", Type: config.Expense},
		config.Account{Number: "231000", Name: "Price differences", Type: config.Expense}, config.Account{Number: "232000", Name: "Revaluation", Type: config.Expense})
	de.AccountDetermination.Inventory = []config.InventoryAccounts{{ValuationClass: "3000", Stock: "150000", InitialEntry: "391000", Consumption: "515000",
		CostOfGoodsSold: "519000"}, {ValuationClass: "3100", Stock: "150000", InitialEntry: "391000"}, {ValuationClass: "3200", Stock: "150000",
		InitialEntry: "391000", CostOfGoodsSold: "519000", PriceDifference: "231000", Revaluation: "232000"}}
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
		"alternative_units": []any{map[string]any{"unit": "BOX", "numerator": 20, "denominator": 1}, map[string]any{"unit": "KG", "numerator": 1, "denominator": 3}}}), "")
	e.Approve(t, m["pending_change_id"].(string))
	return m["id"].(string)
}

// receive enters an initial stock of the bracket in EA at S1.
func (e env) receive(t *testing.T, qty string) {
	t.Helper()
	e.receiveOf(t, e.bracket, qty)
}

// receiveOf enters an initial stock of the material in EA at S1.
func (e env) receiveOf(t *testing.T, material, qty string) {
	t.Helper()
	e.Must(201, "POST", "/v1/material-documents", mustJSON(t, map[string]any{"movement_type": "561", "items": []any{map[string]any{"material_id": material,
		"plant_id": e.p1, "storage_location_id": e.s1, "quantity": map[string]any{"value": qty, "unit": "EA"}}}}), "")
}

// value values the material at P1 under the class at the price control
// and price in EUR per EA.
func (e env) value(t *testing.T, material, class, control, price string) {
	t.Helper()
	e.Must(201, "POST", "/v1/material-valuations", mustJSON(t, map[string]any{"material_id": material, "plant_id": e.p1, "valuation_class": class,
		"price_control": control, "price": map[string]any{"amount": price, "currency": "EUR"}}), "")
}

// ofMaterial is an order item of the material: "quantity unit" at 1.00 EUR.
func (e env) ofMaterial(material, quantity string) string {
	f := strings.Fields(quantity)
	return fmt.Sprintf(`{"material_id": %q, "quantity": {"value": %q, "unit": %q}, "unit_price": {"amount": "1.00", "currency": "EUR"}, "account_id": %q}`,
		material, f[0], f[1], e.revenue)
}

// freeText is an order item without a material.
func (e env) freeText() string {
	return fmt.Sprintf(`{"description": "Installation", "quantity": {"value": "1", "unit": "EA"}, "unit_price": {"amount": "50.00", "currency": "EUR"},
		"account_id": %q}`, e.revenue)
}

// order creates Northwind's order of the items and returns its id: a
// draft, delivering from the company's only plant P1, or one confirmed
// without a plant, as an order was confirmed before a plant was required
// (product owner 2026-11-21; ADR-0082 As built S2j, the plant required),
// so that its items, unchecked for availability, can be delivered beyond
// the stock. It is written as such an order was: its confirmation, with
// no plant, by the owner, since the application confirms none now.
func (e env) order(t *testing.T, draft bool, items ...string) string {
	t.Helper()
	o, _ := e.Must(201, "POST", "/v1/sales-orders", fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "currency": "EUR", "items": [%s]}`,
		e.DE, e.customer, strings.Join(items, ", ")), "")
	id := o["id"].(string)
	if !draft {
		pgtest.Exec(t, `UPDATE sales.sales_order SET status = 'confirmed', plant_id = NULL, version = version + 1 WHERE tenant_id = $1 AND id = $2`,
			e.Tenant.ID, id)
	}
	return id
}

// create is a request creating a delivery of the order from P1/S1 with
// the items, "order item quantity unit" each; none: without items.
func (e env) create(order string, items ...string) string {
	return e.createAt(order, e.p1, e.s1, items...)
}

func (e env) createAt(order string, plant, location uuid.UUID, items ...string) string {
	body := map[string]any{"sales_order_id": order, "plant_id": plant, "storage_location_id": location}
	if len(items) > 0 {
		var list []any
		for _, it := range items {
			f := strings.Fields(it)
			var no int
			fmt.Sscan(f[0], &no) //nolint:errcheck // the tests' own literal
			list = append(list, map[string]any{"order_item_no": no, "quantity": map[string]any{"value": f[1], "unit": f[2]}})
		}
		body["items"] = list
	}
	b, _ := json.Marshal(body) // a map of plain values always marshals
	return string(b)
}

// pick is a request picking "item quantity unit" each.
func pick(items ...string) string {
	var list []any
	for _, it := range items {
		f := strings.Fields(it)
		var no int
		fmt.Sscan(f[0], &no) //nolint:errcheck // the tests' own literal
		list = append(list, map[string]any{"item_no": no, "quantity": map[string]any{"value": f[1], "unit": f[2]}})
	}
	b, _ := json.Marshal(map[string]any{"items": list}) // a map of plain values always marshals
	return string(b)
}

// items are a delivery's items as "order item: quantity unit, picked" each.
func items(t *testing.T, d map[string]any) string {
	t.Helper()
	var out []string
	for _, it := range rows(t, d, "items") {
		q, p := it["quantity"].(map[string]any), it["picked_quantity"].(map[string]any)
		out = append(out, fmt.Sprintf("%v: %v %v, picked %v", it["order_item_no"], q["value"], q["unit"], p["value"]))
	}
	return strings.Join(out, "; ")
}

// inDelivery is the order's items' quantities in delivery, by item.
func (e env) inDelivery(t *testing.T, order string) string {
	t.Helper()
	o, _ := e.Must(200, "GET", "/v1/sales-orders/"+order, "", "")
	var out []string
	for _, it := range rows(t, o, "items") {
		q := it["in_delivery_quantity"].(map[string]any)
		out = append(out, fmt.Sprintf("%v %v", q["value"], q["unit"]))
	}
	return strings.Join(out, ", ")
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
func endpoints(t *testing.T) *shipping.Endpoints {
	t.Helper()
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return app.Shipping(app.Config{Pages: pages, Now: time.Now})
}

// events are the OutboundDeliveryChanged payloads of the delivery, each
// conforming to the event contract.
func (e env) events(t *testing.T, id string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, p := range apitest.Published(t, e.Tenant, outbox.Event{Context: "shipping", Name: "outbound_delivery_changed"}) {
		if p["outbound_delivery_id"] == id {
			out = append(out, p)
		}
	}
	return out
}

// audits are the details of the object's audit records of the action.
func (e env) audits(t *testing.T, id, action string) []map[string]any {
	t.Helper()
	var raw string
	pgtest.Scan(t, `SELECT coalesce(json_agg(detail ORDER BY seq), '[]')::text FROM audit.audit_event
		WHERE tenant_id = $1 AND object_id = $2 AND action = $3`, []any{e.Tenant.ID, id, action}, &raw)
	var out []map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (e env) inRequest(t *testing.T, fn func(ctx context.Context) error) error {
	t.Helper()
	return apitest.InRequest(t, e.Tenant, e.Tenant.Human, fn)
}
