package inventory_test

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
	"github.com/mustie7iy/agentic-erp/internal/inventory"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"materials.material.read", "materials.material.write", "tenancy.org.manage", "inventory.movement.post", "inventory.stock.read",
	"inventory.valuation.read", "inventory.valuation.write", "ledger.entry.read"}

const docs = "/v1/material-documents"

// env is a configured tenant with plants P1 (storage locations S1 and S2)
// and P2 (S3) of company 2000 and P3 (S4) of company 1000, the bracket,
// an active material kept at S1, S2 and S3 and bought by the box of 20,
// and cost center C1 of company 2000 and C9 of company 1000.
type env struct {
	fixture.Env
	p1, p2, p3     uuid.UUID
	s1, s2, s3, s4 uuid.UUID
	bracket        string
	c1, c9         uuid.UUID
}

func setup(t *testing.T) env {
	t.Helper()
	return setupWith(t, perms...)
}

// setupWith is setup with the tenant holding the permissions.
func setupWith(t *testing.T, permissions ...string) env {
	t.Helper()
	e := env{Env: fixture.NewWith(t, nil, valued, permissions...)}
	e.p1 = e.orgUnit(t, e.DE, "plant", "P1", nil)
	e.s1 = e.orgUnit(t, e.DE, "storage_location", "S1", &e.p1)
	e.s2 = e.orgUnit(t, e.DE, "storage_location", "S2", &e.p1)
	e.p2 = e.orgUnit(t, e.DE, "plant", "P2", nil)
	e.s3 = e.orgUnit(t, e.DE, "storage_location", "S3", &e.p2)
	e.p3 = e.orgUnit(t, e.US, "plant", "P3", nil)
	e.s4 = e.orgUnit(t, e.US, "storage_location", "S4", &e.p3)
	e.bracket = e.material(t, "Machined bracket M10", "trading_good", true)
	e.value(t, e.bracket, e.p1, "moving_average", "2.00")
	e.value(t, e.bracket, e.p2, "moving_average", "2.00")
	e.c1 = e.CostCenterFrom(t, e.DE, "C1", "2026-01-01", nil)
	e.c9 = e.CostCenterFrom(t, e.US, "C9", "2026-01-01", nil)
	return e
}

// valued gives company 2000 the accounts of valuation class 3000, the
// stock account alone of class 3100 and the stock and initial entry
// accounts of class 3200.
func valued(c *config.Config) {
	de := c.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "150000", Name: "Inventory", Type: config.Asset},
		config.Account{Number: "219000", Name: "Receipts without order", Type: config.Liability},
		config.Account{Number: "391000", Name: "Initial entries of stock", Type: config.Equity},
		config.Account{Number: "515000", Name: "Consumption", Type: config.Expense}, config.Account{Number: "516000", Name: "Scrapping", Type: config.Expense},
		config.Account{Number: "517000", Name: "Price differences", Type: config.Expense},
		config.Account{Number: "518000", Name: "Revaluation", Type: config.Expense})
	de.AccountDetermination.Inventory = []config.InventoryAccounts{{ValuationClass: "3000", Stock: "150000", Receipt: "219000", InitialEntry: "391000",
		Consumption: "515000", Scrapping: "516000", PriceDifference: "517000", Revaluation: "518000"}, {ValuationClass: "3100", Stock: "150000"},
		{ValuationClass: "3200", Stock: "150000", InitialEntry: "391000"}}
}

// value values the material in the plant at the price in EUR per base
// unit, under class 3000, and returns the valuation.
func (e env) value(t *testing.T, material string, plant uuid.UUID, control, price string) map[string]any {
	t.Helper()
	got, _ := e.Must(201, "POST", "/v1/material-valuations", mustJSON(t, map[string]any{"material_id": material, "plant_id": plant,
		"valuation_class": "3000", "price_control": control, "price": map[string]any{"amount": price, "currency": "EUR"}}), "")
	return got
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

// material creates a material of company 2000 kept at S1 and S2 of P1 and
// S3 of P2, with a box of 20, and has it approved, active, when approve.
func (e env) material(t *testing.T, description, kind string, approve bool) string {
	t.Helper()
	body := mustJSON(t, map[string]any{"legal_entity_id": e.DE, "description": description, "base_unit": "EA", "material_type": kind,
		"material_group": "BRACKETS", "confirm_duplicate": true, "plants": []any{
			map[string]any{"plant_id": e.p1, "storage_location_ids": []any{e.s1, e.s2}},
			map[string]any{"plant_id": e.p2, "storage_location_ids": []any{e.s3}}},
		"alternative_units": []any{map[string]any{"unit": "BOX", "numerator": 20, "denominator": 1}, map[string]any{"unit": "KG", "numerator": 1, "denominator": 3}}})
	m, _ := e.Must(201, "POST", "/v1/materials", body, "")
	if approve {
		e.Approve(t, m["pending_change_id"].(string))
	}
	return m["id"].(string)
}

// item is a request item of the bracket at S1 of P1, with fields
// replaced, added or (nil) taken out by over.
func (e env) item(qty, unit string, over map[string]any) map[string]any {
	it := map[string]any{"material_id": e.bracket, "plant_id": e.p1, "storage_location_id": e.s1, "quantity": map[string]any{"value": qty, "unit": unit}}
	for k, v := range over {
		if v == nil {
			delete(it, k)
			continue
		}
		it[k] = v
	}
	return it
}

// body is a request posting the movement type's items.
func body(t *testing.T, movementType string, items ...map[string]any) string {
	t.Helper()
	list := make([]any, len(items))
	for i, it := range items {
		list[i] = it
	}
	return mustJSON(t, map[string]any{"movement_type": movementType, "items": list})
}

// post posts the movement and returns the document.
func (e env) post(t *testing.T, movementType string, items ...map[string]any) map[string]any {
	t.Helper()
	got, _ := e.Must(201, "POST", docs, body(t, movementType, items...), "")
	return got
}

// stock is the bracket's stock at the storage location now, "" when
// there is none.
func (e env) stock(t *testing.T, location uuid.UUID, query string) string {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/stock?material_id="+e.bracket+"&storage_location_id="+location.String()+query, "", "")
	items := rows(t, page, "items")
	if len(items) == 0 {
		return ""
	}
	q, _ := items[0]["quantity"].(map[string]any)
	return fmt.Sprintf("%v %v", q["value"], q["unit"])
}

// lines are a document's lines as "plant:location quantity unit (entered
// unit)" each.
func (e env) lines(t *testing.T, d map[string]any) string {
	t.Helper()
	var out []string
	for _, l := range rows(t, d, "lines") {
		q, entry := l["quantity"].(map[string]any), l["entry_quantity"].(map[string]any)
		out = append(out, fmt.Sprintf("%v:%v %v %v (%v %v)", l["plant_code"], l["storage_location_code"], q["value"], q["unit"], entry["value"], entry["unit"]))
	}
	return strings.Join(out, "; ")
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
func endpoints(t *testing.T) *inventory.Endpoints {
	t.Helper()
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return app.Inventory(app.Config{Pages: pages, Now: time.Now})
}

// events are the MaterialDocumentPosted payloads of the document, each
// conforming to the event contract.
func (e env) events(t *testing.T, id string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, p := range apitest.Published(t, e.Tenant, outbox.Event{Context: "inventory", Name: "material_document_posted"}) {
		if p["material_document_id"] == id {
			out = append(out, p)
		}
	}
	return out
}

// audits are the details of the document's audit records of the action.
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
