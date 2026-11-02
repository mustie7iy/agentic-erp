package assets_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// withClasses gives the prototype's document three asset classes: company
// 1000 uses MACHINES and VEHICLES, company 2000 MACHINES, and no company
// BUILDINGS; each company has the accounts and number ranges they need
// (ADR-0057 §3).
func withClasses(doc *config.Config) {
	doc.AssetClasses = []config.AssetClass{
		{Code: "VEHICLES", Name: "Vehicles", Terms: []config.AssetTerms{{Ledger: config.LeadingLedger, Method: config.DecliningBalance, UsefulLifeMonths: 60,
			DecliningFactor: "2.5"}}},
		{Code: "MACHINES", Name: "Machinery and equipment", Terms: []config.AssetTerms{{Ledger: config.LeadingLedger, Method: config.StraightLine,
			UsefulLifeMonths: 120}}},
		{Code: "BUILDINGS", Name: "Buildings", Terms: []config.AssetTerms{{Ledger: config.LeadingLedger, Method: config.StraightLine, UsefulLifeMonths: 600}}},
	}
	for i := range doc.Companies {
		co := &doc.Companies[i]
		co.ChartOfAccounts = append(co.ChartOfAccounts,
			config.Account{Number: "160000", Name: "Machinery", Type: config.Asset, AssetReconciliation: true},
			config.Account{Number: "160900", Name: "Machinery, accumulated depreciation", Type: config.Asset, AssetReconciliation: true},
			config.Account{Number: "620000", Name: "Depreciation", Type: config.Expense},
			config.Account{Number: "169000", Name: "Asset clearing", Type: config.Asset},
			config.Account{Number: "490000", Name: "Gain on asset retirements", Type: config.Revenue},
			config.Account{Number: "690000", Name: "Loss on asset retirements", Type: config.Expense},
			config.Account{Number: "169500", Name: "Asset sale clearing", Type: config.Asset})
		accounts := config.AssetAccounts{Class: "MACHINES", AcquisitionCost: "160000", AccumulatedDepreciation: "160900", Depreciation: "620000",
			Clearing: "169000", Gain: "490000", Loss: "690000", SaleClearing: "169500"}
		co.AssetAccounts = []config.AssetAccounts{accounts}
		if co.Code == "1000" {
			accounts.Class = "VEHICLES"
			co.AssetAccounts = append(co.AssetAccounts, accounts)
		}
		for _, y := range fixture.Years() {
			for _, kind := range config.AssetRanges {
				doc.NumberRanges = append(doc.NumberRanges, config.NumberRange{Company: co.Code, DocumentType: kind, FiscalYear: y, First: 1, Last: 999999})
			}
		}
	}
}

// classes reads the asset classes as one line per class: its code, name
// and terms, then its companies with their acquisition cost account.
func classes(t *testing.T, page map[string]any) []string {
	t.Helper()
	var out []string
	for _, it := range page["items"].([]any) {
		c := it.(map[string]any)
		var terms, companies []string
		for _, tr := range c["terms"].([]any) {
			tr := tr.(map[string]any)
			terms = append(terms, fmt.Sprintf("%s %s %s %v %v", tr["ledger_code"], tr["ledger_name"], tr["method"], tr["useful_life_months"], tr["declining_factor"]))
		}
		for _, co := range c["companies"].([]any) {
			co := co.(map[string]any)
			cost := co["accounts"].(map[string]any)["acquisition_cost"].(map[string]any)
			companies = append(companies, fmt.Sprintf("%s %s %s %s", co["company_code"], co["company_name"], cost["number"], cost["name"]))
		}
		out = append(out, fmt.Sprintf("%s %s [%s] [%s]", c["code"], c["name"], strings.Join(terms, "; "), strings.Join(companies, "; ")))
	}
	return out
}

func wantClasses(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\n got %q\nwant %q", what, got, want)
	}
}

const (
	buildings = "BUILDINGS Buildings [0L Leading ledger straight_line 600 <nil>] []"
	machines  = "MACHINES Machinery and equipment [0L Leading ledger straight_line 120 <nil>] [1000 Acme Corp US 160000 Machinery; 2000 Acme GmbH 160000 Machinery]"
	vehicles  = "VEHICLES Vehicles [0L Leading ledger declining_balance 60 2.5] [1000 Acme Corp US 160000 Machinery]"
)

// TestListAssetClasses: the classes in code order with their terms per
// ledger and, per company that uses each and that the caller may act in,
// its accounts by number and name; filtered by legal entity, with that
// company's accounts alone; paged by code.
func TestListAssetClasses(t *testing.T) {
	e := fixture.NewWith(t, nil, withClasses, "assets.asset.read")
	page, _ := e.Must(200, "GET", "/v1/asset-classes", "", "")
	wantClasses(t, "all classes", classes(t, page), buildings, machines, vehicles)
	accounts := page["items"].([]any)[1].(map[string]any)["companies"].([]any)[1].(map[string]any)["accounts"].(map[string]any)
	var got []string
	for _, role := range []string{"acquisition_cost", "accumulated_depreciation", "depreciation", "clearing", "gain", "loss", "sale_clearing"} {
		a := accounts[role].(map[string]any)
		got = append(got, fmt.Sprintf("%s %s %s", role, a["number"], a["name"]))
	}
	wantClasses(t, "company 2000's accounts of MACHINES", got, "acquisition_cost 160000 Machinery",
		"accumulated_depreciation 160900 Machinery, accumulated depreciation", "depreciation 620000 Depreciation", "clearing 169000 Asset clearing",
		"gain 490000 Gain on asset retirements", "loss 690000 Loss on asset retirements", "sale_clearing 169500 Asset sale clearing")
	page, _ = e.Must(200, "GET", "/v1/asset-classes?legal_entity_id="+e.DE.String(), "", "")
	wantClasses(t, "company 2000's classes", classes(t, page), "MACHINES Machinery and equipment [0L Leading ledger straight_line 120 <nil>] [2000 Acme GmbH 160000 Machinery]")

	var paged []string
	path := "/v1/asset-classes?page_size=1"
	for range 4 {
		page, _ = e.Must(200, "GET", path, "", "")
		paged = append(paged, classes(t, page)...)
		next, ok := page["next_page_token"].(string)
		if !ok {
			break
		}
		path = "/v1/asset-classes?page_size=1&page_token=" + next
	}
	wantClasses(t, "classes one page at a time", paged, buildings, machines, vehicles)
	e.Problem(400, "request.invalid_page_token", "GET", "/v1/asset-classes?legal_entity_id="+e.DE.String()+"&page_token="+
		strings.TrimPrefix(path, "/v1/asset-classes?page_size=1&page_token="), "", "")

	reader := e.Client
	reader.As = e.Reader(t, "reader", e.US)
	page, _ = reader.Must(200, "GET", "/v1/asset-classes", "", "")
	wantClasses(t, "classes as a reader of company 1000", classes(t, page), buildings,
		"MACHINES Machinery and equipment [0L Leading ledger straight_line 120 <nil>] [1000 Acme Corp US 160000 Machinery]", vehicles)
	page, _ = reader.Must(200, "GET", "/v1/asset-classes?legal_entity_id="+e.DE.String(), "", "")
	wantClasses(t, "company 2000's classes as a reader of company 1000", classes(t, page))
}

// TestListAssetClassesWithoutConfiguration: a tenant without a
// configuration has no classes to list (409 config.not_bootstrapped).
func TestListAssetClassesWithoutConfiguration(t *testing.T) {
	ten := pgtest.NewTenant(t, "assets.asset.read")
	c := apitest.Client{T: t, API: apitest.New(t), Tenant: ten, As: ten.Human}
	c.Problem(409, "config.not_bootstrapped", "GET", "/v1/asset-classes", "", "")
}

// TestDatabaseFailures calls the handler with each of its database
// statements failing in turn.
func TestDatabaseFailures(t *testing.T) {
	e := fixture.NewWith(t, nil, withClasses, "assets.asset.read")
	pages, err := pagination.NewCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h := app.Assets(app.Config{Pages: pages})
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "list asset classes", func(ctx context.Context) error {
		_, err := h.ListAssetClasses(ctx, gen.ListAssetClassesParams{})
		return err
	})
	// The contract refuses a page size of 0 before the handler; called
	// directly, the handler refuses it itself.
	err = apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.ListAssetClasses(ctx, gen.ListAssetClassesParams{PageSize: gen.NewOptInt32(0)})
		return err
	})
	pgtest.WantErr(t, err, "request.invalid_page_size")
}
