package assets_test

import (
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
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// assetDay is the day the tests create assets on (company time; the
// companies have no time zone of their own, so UTC's).
var assetDay = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

// withLedgers is withClasses with the parallel ledgers 2L and 3L, company
// 2000 keeping 0L and 2L, MACHINES with declining balance terms in 2L,
// and class LAND, whose depreciation account 621000 takes no cost object,
// used by company 2000 alone.
func withLedgers(doc *config.Config) {
	withClasses(doc)
	doc.Ledgers = []config.Ledger{{Code: "0L", Name: "Leading ledger", AccountingPrinciple: "IFRS"}, {Code: "2L", Name: "HGB", AccountingPrinciple: "HGB"},
		{Code: "3L", Name: "Tax", AccountingPrinciple: "TAX"}}
	doc.Company("2000").Ledgers = []string{"0L", "2L"}
	for i := range doc.AssetClasses {
		if doc.AssetClasses[i].Code == "MACHINES" {
			doc.AssetClasses[i].Terms = append(doc.AssetClasses[i].Terms, config.AssetTerms{Ledger: "2L", Method: config.DecliningBalance,
				UsefulLifeMonths: 120, DecliningFactor: "2"})
		}
	}
	doc.AssetClasses = append(doc.AssetClasses, config.AssetClass{Code: "LAND", Name: "Land", Terms: []config.AssetTerms{
		{Ledger: config.LeadingLedger, Method: config.StraightLine, UsefulLifeMonths: 1200}, {Ledger: "2L", Method: config.StraightLine, UsefulLifeMonths: 1200}}})
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "621000", Name: "Land, no depreciation", Type: config.Expense,
		CostElement: config.CostElementNone})
	land := *de.AssetAccountsOf("MACHINES")
	land.Class, land.Depreciation = "LAND", "621000"
	de.AssetAccounts = append(de.AssetAccounts, land)
}

// assetEnv is a tenant with withLedgers, company 2000's centers
// (fixture.CostCenters, cost center accounting on), a profit center PC2
// of company 1000 alone, and a client creating assets on assetDay; its
// people hold the permissions besides reading and changing assets.
type assetEnv struct {
	fixture.Env
	c       fixture.Centers
	pc2     uuid.UUID
	l0, l2  uuid.UUID
	l3      uuid.UUID
	clocked apitest.Client
}

func newAssets(t *testing.T, permissions ...string) assetEnv {
	t.Helper()
	return newAssetsWith(t, withLedgers, permissions...)
}

// newAssetsWith is newAssets with the document doc gives (withLedgers or
// one extending it).
func newAssetsWith(t *testing.T, doc func(*config.Config), permissions ...string) assetEnv {
	t.Helper()
	e := assetEnv{Env: fixture.NewWith(t, nil, doc, append(permissions, "assets.asset.read", "assets.asset.change")...)}
	e.c = e.CostCenters(t, doc)
	pc, _ := e.Must(201, "POST", "/v1/profit-centers", fmt.Sprintf(`{"code": "PC2", "name": "PC 2", "responsible": "Jane Doe",
		"legal_entity_ids": [%q], "valid_from": "2026-01-01", "reason": "r"}`, e.US), "")
	e.Approve(t, pc["pending_change_id"].(string))
	e.pc2 = uuid.MustParse(pc["id"].(string))
	for code, id := range map[string]*uuid.UUID{"0L": &e.l0, "2L": &e.l2, "3L": &e.l3} {
		pgtest.Scan(t, `SELECT id FROM ledger.ledger WHERE tenant_id = $1 AND code = $2`, []any{e.Tenant.ID, code}, id)
	}
	e.clocked = e.Client
	e.clocked.API = apitest.NewWith(t, func(c *app.Config) { c.Now = func() time.Time { return assetDay } })
	return e
}

// create creates an asset on assetDay and returns it with its ETag.
func (e assetEnv) create(t *testing.T, body string) (map[string]any, string) {
	t.Helper()
	return e.clocked.Must(201, "POST", "/v1/assets", body, "")
}

func (e assetEnv) body(le uuid.UUID, class, extra string) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "class_code": %q, "description": "CNC milling machine"%s}`, le, class, extra)
}

// terms is an asset's terms, one line per ledger.
func terms(a map[string]any) string {
	var out []string
	for _, t := range a["terms"].([]any) {
		t := t.(map[string]any)
		r := t["residual_value"].(map[string]any)
		out = append(out, fmt.Sprintf("%s %s %s %v %v %s %s %v", t["ledger_code"], t["ledger_name"], t["method"], t["useful_life_months"],
			t["declining_factor"], r["amount"], r["currency"], t["depreciation_start"]))
	}
	return strings.Join(out, "; ")
}

func want(t *testing.T, what string, got, wanted any) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(wanted) {
		t.Errorf("%s: got %v, want %v", what, got, wanted)
	}
}

// TestCreateAsset: an asset of company 2000 with a cost center is
// planned, numbered from the asset range, has its class's terms in the
// ledgers the company keeps, and its profit center derived from the cost
// center; it reads back the same, is audited and published.
func TestCreateAsset(t *testing.T) {
	e := newAssets(t)
	a, tag := e.create(t, e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)))
	want(t, "status", a["status"], "planned")
	want(t, "number", a["asset_number"], "1")
	want(t, "class", fmt.Sprint(a["class_code"], " ", a["class_name"]), "MACHINES Machinery and equipment")
	want(t, "company", a["company_code"], "2000")
	want(t, "cost center", fmt.Sprint(a["cost_center_id"], " ", a["cost_center_code"], " ", a["cost_center_name"]), e.c.Admin.String()+" 1000 CC 1000")
	want(t, "profit center", fmt.Sprint(a["profit_center_id"], " ", a["profit_center_code"], " ", a["profit_center_name"]), e.c.Profit.String()+" PC1 PC 1")
	want(t, "terms", terms(a), "0L Leading ledger straight_line 120 <nil> 0.00 EUR <nil>; 2L HGB declining_balance 120 2 0.00 EUR <nil>")
	want(t, "ETag", tag, `"1"`)
	if _, ok := a["capitalised_on"]; ok {
		t.Errorf("capitalised_on: got %v, want none", a["capitalised_on"])
	}
	got, getTag := e.Must(200, "GET", "/v1/assets/"+a["id"].(string), "", "")
	want(t, "read back", fmt.Sprint(got), fmt.Sprint(a))
	want(t, "ETag read", getTag, tag)

	b, _ := e.create(t, e.body(e.US, "VEHICLES", ""))
	want(t, "the second number", b["asset_number"], "1")
	want(t, "an asset without centers", fmt.Sprint(b["cost_center_id"], b["profit_center_id"]), "<nil> <nil>")
	want(t, "company 1000's terms", terms(b), "0L Leading ledger declining_balance 60 2.5 0.00 USD <nil>")
	c, _ := e.create(t, e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q, "profit_center_id": %q`, e.c.Sales, e.c.Profit)))
	want(t, "a cost center without a profit center takes the one given", fmt.Sprint(c["asset_number"], " ", c["profit_center_code"]), "2 PC1")

	evs := apitest.Published(t, e.Tenant, outbox.Event{Context: "assets", Name: "asset_changed"})
	want(t, "events", len(evs), 3)
	want(t, "the first event", fmt.Sprint(evs[0]["asset_id"], " ", evs[0]["asset_number"], " ", evs[0]["change"], " ", evs[0]["status"]),
		fmt.Sprint(a["id"], " 1 created planned"))
	var audits int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'assets.asset.create'`, []any{e.Tenant.ID}, &audits)
	want(t, "audit records", audits, 3)
	var seq, last int
	var from string
	pgtest.Scan(t, `SELECT r.seq, a.last_seq, r.valid_from::text FROM assets.asset a JOIN assets.asset_assignment r ON r.asset_id = a.id WHERE a.id = $1`,
		[]any{a["id"]}, &seq, &last, &from)
	want(t, "the first assignment", fmt.Sprint(seq, " ", last, " ", from), "1 1 -infinity")
}

// TestCreateAssetRefused: classes the company cannot hold, centers a
// posting could not take on the day, and a company the caller cannot act
// in are refused.
func TestCreateAssetRefused(t *testing.T) {
	e := newAssets(t)
	for name, c := range map[string]struct {
		body           string
		status         int
		code, field    string
		wantFieldError bool
	}{
		"an unknown class":                {e.body(e.DE, "NOPE", ""), 422, "assets.class_unknown", "class_code", true},
		"a class no company uses":         {e.body(e.DE, "BUILDINGS", ""), 422, "assets.class_not_in_company", "class_code", true},
		"a class of another company":      {e.body(e.DE, "VEHICLES", ""), 422, "assets.class_not_in_company", "class_code", true},
		"no cost center where needed":     {e.body(e.DE, "MACHINES", ""), 422, "assets.cost_center_required", "cost_center_id", true},
		"only a profit center":            {e.body(e.DE, "MACHINES", fmt.Sprintf(`, "profit_center_id": %q`, e.c.Profit)), 422, "assets.cost_center_required", "cost_center_id", true},
		"another profit center":           {e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q, "profit_center_id": %q`, e.c.Admin, e.pc2)), 422, "ledger.profit_center_mismatch", "profit_center_id", true},
		"a cost center locked":            {e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Locked)), 422, "ledger.cost_center_invalid", "cost_center_id", true},
		"another company's center":        {e.body(e.US, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)), 422, "ledger.cost_center_invalid", "cost_center_id", true},
		"a center on no cost object":      {e.body(e.DE, "LAND", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)), 422, "ledger.cost_center_invalid", "cost_center_id", true},
		"another company's profit center": {e.body(e.US, "MACHINES", fmt.Sprintf(`, "profit_center_id": %q`, e.c.Profit)), 422, "ledger.profit_center_invalid", "profit_center_id", true},
		"a profit center of another company with a cost center": {e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q, "profit_center_id": %q`, e.c.Sales, e.pc2)), 422,
			"ledger.profit_center_invalid", "profit_center_id", true},
		"a description of spaces": {fmt.Sprintf(`{"legal_entity_id": %q, "class_code": "MACHINES", "description": "   "}`, e.DE), 422,
			"request.validation_failed", "description", true},
		"an unknown company": {e.body(uuid.Must(uuid.NewV7()), "MACHINES", ""), 404, "request.not_found", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			p := e.clocked.Problem(c.status, c.code, "POST", "/v1/assets", c.body, "")
			if !c.wantFieldError {
				return
			}
			errs, _ := p["errors"].([]any)
			if len(errs) != 1 {
				t.Fatalf("errors: got %v, want one on %s", p["errors"], c.field)
			}
			want(t, "field", errs[0].(map[string]any)["field"], c.field)
		})
	}
	land, _ := e.create(t, e.body(e.DE, "LAND", ""))
	want(t, "an asset whose depreciation takes no cost object needs no cost center", land["status"], "planned")

	reader := e.clocked
	reader.As = e.Reader(t, "reader", e.US)
	reader.Problem(404, "request.not_found", "POST", "/v1/assets", e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)), "")
	asset, _ := e.create(t, e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)))
	reader.Problem(404, "request.not_found", "GET", "/v1/assets/"+asset["id"].(string), "", "")
	pgtest.Exec(t, `UPDATE config.number_range SET next_value = last_value + 1, version = version + 1 WHERE tenant_id = $1 AND document_type = 'asset'
		AND legal_entity_id = $2`, e.Tenant.ID, e.US)
	e.clocked.Problem(409, "config.number_range_exhausted", "POST", "/v1/assets", e.body(e.US, "MACHINES", ""), "")
}
