package integration_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// stockDay is the day the stock tests take stock over on: in 2026, the
// fiscal year their runs migrate.
var stockDay = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// stockSetup is materialSetup with company 2000's valuation class 3000
// (stock 150000, initial entries 391000, price differences 517000) and
// class 3100 without an initial entry account, on stockDay.
func stockSetup(t *testing.T) env {
	t.Helper()
	f := fixture.NewWith(t, nil, func(c *config.Config) {
		de := c.Company("2000")
		de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "150000", Name: "Inventory", Type: config.Asset},
			config.Account{Number: "391000", Name: "Initial entries of stock", Type: config.Equity},
			config.Account{Number: "517000", Name: "Price differences", Type: config.Expense})
		de.AccountDetermination.Inventory = []config.InventoryAccounts{{ValuationClass: "3000", Stock: "150000", InitialEntry: "391000",
			PriceDifference: "517000"}, {ValuationClass: "3100", Stock: "150000", PriceDifference: "517000"}}
	}, append(slices.Clone(perms), "materials.material.read", "materials.material.write", "tenancy.org.read", "tenancy.org.manage",
		"inventory.stock.read", "inventory.valuation.read")...)
	dir := t.TempDir()
	f.API = apitest.NewWith(t, func(c *app.Config) { c.ExtractDir, c.Now = dir, func() time.Time { return stockDay } })
	return env{Env: f, dir: dir}
}

// withStock is withMaterials with stock and valuations (S2p): steel sheet
// M-1 at a moving price, 60 KG at P1/S1 and 40 KG at P1/S2 worth 250.01
// EUR in all; shelving M-2 at a standard price of 10.00, 4 at P1/S2 and 6
// at P2/S3; and 3 pieces of M-3, which is held back, worth 7.50.
func withStock() *sap.Extract {
	x := withMaterials()
	x.MARD = append(x.MARD, sap.MARD{MATNR: "M-1", WERKS: "P1", LGORT: "S2"})
	labst := map[string]string{"M-1/P1/S1": "60.000", "M-1/P1/S2": "40.000", "M-2/P1/S2": "4.000", "M-2/P2/S3": "6.000", "M-3/P2/S3": "3.000"}
	for i, d := range x.MARD {
		x.MARD[i].LABST = labst[d.MATNR+"/"+d.WERKS+"/"+d.LGORT]
	}
	x.MBEW = []sap.MBEW{
		{MATNR: "M-1", BWKEY: "P1", VPRSV: "V", VERPR: "2.50", STPRS: "0.00", PEINH: "1", BKLAS: "3000", LBKUM: "100.000", SALK3: "250.01"},
		{MATNR: "M-2", BWKEY: "P1", VPRSV: "S", VERPR: "9.80", STPRS: "10.00", PEINH: "1", BKLAS: "3000", LBKUM: "4.000", SALK3: "40.00"},
		{MATNR: "M-2", BWKEY: "P2", VPRSV: "S", VERPR: "10.00", STPRS: "10.00", PEINH: "1", BKLAS: "3000", LBKUM: "6.000", SALK3: "60.00"},
		{MATNR: "M-3", BWKEY: "P2", VPRSV: "V", VERPR: "2.50", STPRS: "0.00", PEINH: "1", BKLAS: "3000", LBKUM: "3.000", SALK3: "7.50"},
	}
	return x
}

// TestMigratedStock (S2p): a run values the materials it loads at their
// plants as SAP does and posts their unrestricted stock as initial
// entries (561) at SAP's value, on the class's stock account against its
// initial entry account; the stock of a held-back material is a warning.
// The reconciliation compares each valuation and the stock accounts, and
// the trial balance leaves the take-over out. A run of the same extract
// takes nothing over again.
func TestMigratedStock(t *testing.T) {
	e := stockSetup(t)
	write(t, e.dir, "stock.json", withStock())
	conn := e.connector(t, "stock.json")
	_, got := e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
	if got["status"] != "loaded" {
		t.Fatalf("status: got %v (%s %v), want loaded", got["status"], quality(got), loadIssues(got))
	}
	expectFields(t, "loaded", report(got)["loaded"].(map[string]any), map[string]any{"material_valuations": 3.0, "initial_entries": 3.0})
	if q := quality(got); !strings.Contains(q, "warning MBEW P2/M-3: the stock of 3.000 M-3 (value 7.50, as extracted) at plant P2 is not taken over") {
		t.Errorf("quality: got\n%s\nwant M-3's stock named as not taken over", q)
	}
	steel := e.materialID(t, "Material M-1")
	vals, _ := e.Must(200, "GET", "/v1/material-valuations?material_id="+steel, "", "")
	v := vals["items"].([]any)[0].(map[string]any)
	if got := fmt.Sprintf("%v %v %v %v", v["price_control"], v["valuation_class"], v["quantity"].(map[string]any)["value"], v["value"].(map[string]any)["amount"]); got != "moving_average 3000 100 250.01" {
		t.Errorf("M-1's valuation: got %s, want moving_average 3000 100 250.01", got)
	}
	stock, _ := e.Must(200, "GET", "/v1/stock?material_id="+steel, "", "")
	var at []string
	for _, s := range stock["items"].([]any) {
		m := s.(map[string]any)
		at = append(at, fmt.Sprintf("%v/%v %v", m["plant_code"], m["storage_location_code"], m["quantity"].(map[string]any)["value"]))
	}
	if slices.Sort(at); strings.Join(at, ", ") != "P1/S1 60, P1/S2 40" {
		t.Errorf("M-1's stock: got %v, want P1/S1 60, P1/S2 40", at)
	}
	docs, _ := e.Must(200, "GET", "/v1/material-documents?material_id="+steel, "", "")
	d := docs["items"].([]any)[0].(map[string]any)
	if d["movement_type"] != "561" || !strings.HasSuffix(fmt.Sprint(d["header_text"]), ": SAP stock of material M-1 at plant P1") {
		t.Errorf("M-1's material document: got %v %v, want a 561 naming SAP's material and plant", d["movement_type"], d["header_text"])
	}
	entry, _ := e.Must(200, "GET", fmt.Sprintf("/v1/journal-entries/%v?fiscal_year=2026", d["journal_entry_id"]), "", "")
	var lines []string
	accounts := map[bool]map[any]bool{true: {}, false: {}}
	for _, l := range entry["lines"].([]any) {
		m := l.(map[string]any)
		amount := m["amount"].(map[string]any)["amount"].(string)
		lines = append(lines, amount)
		accounts[!strings.HasPrefix(amount, "-")][m["account_id"]] = true
	}
	if slices.Sort(lines); strings.Join(lines, ", ") != "-100.00, -150.01, 100.00, 150.01" {
		t.Errorf("M-1's initial entry: got %v, want each storage location's value against its counter line", lines)
	}
	if len(accounts[true]) != 1 || len(accounts[false]) != 1 {
		t.Errorf("M-1's initial entry accounts: got debits on %v and credits on %v, want one stock and one initial entry account", accounts[true], accounts[false])
	}
	_, got = e.act(t, got, "reconcile")
	if got["status"] != "reconciled" {
		t.Fatalf("reconciled: got %v (%v %v)", got["status"], report(got)["diffs"], report(got)["issues"])
	}
	for name, want := range map[string]string{"Stock": "3", "Stock: valuations with differences": "0", "Stock value total": "350.01 EUR",
		"Stock accounts: accounts with differences": "0", "Trial balance: accounts with differences": "0"} {
		expectFields(t, "check "+name, checkOf(t, got, "2000", name), map[string]any{"sap": want, "platform": want, "pass": true})
	}
	_, got = e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
	if got["status"] != "loaded" {
		t.Fatalf("a second run: got %v (%s %v), want loaded", got["status"], quality(got), loadIssues(got))
	}
	if n := report(got)["loaded"].(map[string]any)["material_valuations"]; n != nil {
		t.Errorf("a second run's valuations: got %v, want none taken over again", n)
	}
	if _, got = e.act(t, got, "reconcile"); got["status"] != "reconciled" {
		t.Errorf("the second run reconciled: got %v (%v)", got["status"], report(got)["diffs"])
	}
}

// TestStockAccountTwice: SAP's G/L balance on the stock account, loaded
// with the journal, beside the take-over's stock on it is twice the
// stock's value, which the stock accounts' control shows while the trial
// balance, without the take-over, agrees.
func TestStockAccountTwice(t *testing.T) {
	e := stockSetup(t)
	x := withStock()
	x.SKB1 = append(x.SKB1, sap.SKB1{BUKRS: "2000", SAKNR: "0000150000", TXT50: "Inventory", XBILK: "X"})
	x.ACDOCA = append(x.ACDOCA,
		sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", RWCUR: "EUR", GJAHR: "2026", BELNR: "0100000009", DOCLN: "000001", RACCT: "0000150000", HSL: "350.01", BUDAT: "20260305", BLART: "WE"},
		sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", RWCUR: "EUR", GJAHR: "2026", BELNR: "0100000009", DOCLN: "000002", RACCT: "0000100000", HSL: "-350.01", BUDAT: "20260305", BLART: "WE"})
	write(t, e.dir, "twice.json", x)
	_, got := e.act(t, e.run(t, e.connector(t, "twice.json"), "documents", false, e.DE.String()), "start")
	if got["status"] != "loaded" {
		t.Fatalf("status: got %v (%s %v), want loaded", got["status"], quality(got), loadIssues(got))
	}
	_, got = e.act(t, got, "reconcile")
	expectFields(t, "the trial balance", checkOf(t, got, "2000", "Trial balance: accounts with differences"), map[string]any{"pass": true})
	expectFields(t, "the stock accounts", checkOf(t, got, "2000", "Stock accounts: accounts with differences"), map[string]any{"platform": "1", "pass": false})
	if diffs := fmt.Sprint(report(got)["diffs"]); !strings.Contains(diffs, "stock account 150000") || !strings.Contains(diffs, "700.02 EUR") {
		t.Errorf("diffs: got %s, want stock account 150000 at 700.02 EUR against SAP's 350.01", diffs)
	}
}

// TestStockTakeOverRefused: a valuation the services refuse (a class
// without an initial entry account) is a load refusal, and so is stock
// taken over on a day of another fiscal year than the run's; a storage
// location or plant that did not load leaves its stock untaken.
func TestStockTakeOverRefused(t *testing.T) {
	e := stockSetup(t)
	x := withStock()
	x.MBEW[0].BKLAS = "3100"
	write(t, e.dir, "refused.json", x)
	_, got := e.act(t, e.run(t, e.connector(t, "refused.json"), "documents", false, e.DE.String()), "start")
	if load := loadIssues(got); !slices.ContainsFunc(load, func(s string) bool {
		return strings.HasPrefix(s, "MBEW P1/M-1: inventory.no_inventory_account")
	}) {
		t.Errorf("load: got %v, want M-1's take-over refused for its class's missing initial entry account", load)
	}
	other := stockSetup(t)
	p1 := other.unit(t, "plant", "P1", "", false)
	other.unit(t, "storage_location", "S9", p1, false)
	write(t, other.dir, "stock.json", withStock())
	_, got = other.act(t, other.run(t, other.connector(t, "stock.json"), "documents", false, other.DE.String()), "start")
	if load := loadIssues(got); !slices.Contains(load, "MBEW P1/M-1: material M-1 or plant P1 was not loaded") {
		t.Errorf("load: got %v, want M-1's stock at the refused plant P1 left untaken", load)
	}
}

// TestStockAtANewStorageLocationRefused: stock at a storage location a
// later run could not load (the company's is inactive) is not taken over
// for a material an earlier run loaded.
func TestStockAtANewStorageLocationRefused(t *testing.T) {
	e := stockSetup(t)
	write(t, e.dir, "first.json", withMaterials())
	conn := e.connector(t, "first.json")
	if _, got := e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start"); got["status"] != "loaded" {
		t.Fatalf("the first run: got %v (%s), want loaded", got["status"], quality(got))
	}
	units, _ := e.Must(200, "GET", "/v1/org-units?kind=plant&page_size=50", "", "")
	var p1 string
	for _, u := range units["items"].([]any) {
		if m := u.(map[string]any); m["code"] == "P1" {
			p1 = m["id"].(string)
		}
	}
	e.unit(t, "storage_location", "S7", p1, false)
	x := withMaterials()
	x.T001L = append(x.T001L, sap.T001L{WERKS: "P1", LGORT: "S7", LGOBE: "Store seven"})
	x.MARD = append(x.MARD, sap.MARD{MATNR: "M-1", WERKS: "P1", LGORT: "S7", LABST: "5"})
	x.MBEW = []sap.MBEW{{MATNR: "M-1", BWKEY: "P1", VPRSV: "V", VERPR: "2.00", STPRS: "0", PEINH: "1", BKLAS: "3000", LBKUM: "5", SALK3: "10.00"}}
	write(t, e.dir, "second.json", x)
	_, got := e.act(t, e.run(t, e.connector(t, "second.json"), "documents", false, e.DE.String()), "start")
	if load := loadIssues(got); !slices.Contains(load, "MARD M-1/P1/S7: storage location S7 of plant P1 was not loaded") {
		t.Errorf("load: got %v, want M-1's stock at S7 left untaken", load)
	}
}

// TestStockInAnotherYearRefused: a run of fiscal year 2025 cannot take
// over stock, which is posted on the company's date today, in 2026.
func TestStockInAnotherYearRefused(t *testing.T) {
	e := stockSetup(t)
	x := withStock()
	x.LFA1, x.LFB1, x.LFBK, x.EKKO, x.EKPO, x.MSEG, x.RBKP, x.RSEG, x.ACDOCA = nil, nil, nil, nil, nil, nil, nil, nil, nil
	write(t, e.dir, "earlier.json", x)
	r, _ := e.Must(201, "POST", "/v1/migration-runs", fmt.Sprintf(`{"connector_id": %q, "scope": {"legal_entity_ids": [%q], "fiscal_year": 2025}, "mode": "documents", "is_rehearsal": false}`,
		e.connector(t, "earlier.json"), e.DE), "")
	_, got := e.act(t, r, "start")
	if got["status"] != "failed" || report(got)["load"] == nil {
		t.Fatalf("status: got %v (%s), want the load failed", got["status"], quality(got))
	}
	if load := loadIssues(got); !slices.Contains(load, "MBEW P1/M-1: the stock is taken over on the company's date today, in fiscal year 2026; take it over in a run of that fiscal year") {
		t.Errorf("load: got %v (%s), want the take-over refused for its fiscal year", load, quality(got))
	}
}

// TestMigratedStockDatabaseFailures: a stock take-over's load and its
// reconciliation fail with each of their statements in turn.
func TestMigratedStockDatabaseFailures(t *testing.T) {
	e := stockSetup(t)
	write(t, e.dir, "stock.json", withStock())
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	h := app.Integration(app.Config{Pages: pages, FieldKeys: apitest.FieldKeys(), ExtractDir: e.dir, Now: func() time.Time { return stockDay }})
	conn := e.connector(t, "stock.json")
	queued := uuid.MustParse(e.run(t, conn, "documents", false, e.DE.String())["id"].(string))
	yes := func(int32) error { return nil }
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "load", func(ctx context.Context) error {
		_, err := h.StartRun(ctx, queued, yes)
		return err
	})
	_, loaded := e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
	id := uuid.MustParse(loaded["id"].(string))
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "reconcile", func(ctx context.Context) error {
		_, err := h.ReconcileRun(ctx, id, yes)
		return err
	})
}

// TestMigratedValuationWithoutStock: a valuation SAP holds no stock of is
// taken over as a valuation alone, with no initial entry; a reconciliation
// of an extract that changed since and holds a valuation the quality gate
// refuses reports the refusal, as the gate does, and fails.
func TestMigratedValuationWithoutStock(t *testing.T) {
	e := stockSetup(t)
	x := withMaterials()
	x.MBEW = []sap.MBEW{{MATNR: "M-1", BWKEY: "P1", VPRSV: "V", VERPR: "2.50", STPRS: "0.00", PEINH: "1", BKLAS: "3000", LBKUM: "0.000", SALK3: "0.00"}}
	write(t, e.dir, "stock.json", x)
	_, got := e.act(t, e.run(t, e.connector(t, "stock.json"), "documents", false, e.DE.String()), "start")
	if got["status"] != "loaded" {
		t.Fatalf("status: got %v (%s %v), want loaded", got["status"], quality(got), loadIssues(got))
	}
	loaded := report(got)["loaded"].(map[string]any)
	if loaded["material_valuations"] != 1.0 {
		t.Errorf("valuations taken over: got %v, want 1", loaded["material_valuations"])
	}
	if n, ok := loaded["initial_entries"]; ok {
		t.Errorf("initial entries: got %v, want none", n)
	}
	x.MBEW[0].VPRSV = "X"
	write(t, e.dir, "stock.json", x)
	_, got = e.act(t, got, "reconcile")
	if issues := fmt.Sprint(report(got)["issues"]); got["status"] == "reconciled" || !strings.Contains(issues, "MBEW") {
		t.Errorf("reconciled with a refused valuation: got %v with issues %s, want it failed naming the MBEW row", got["status"], issues)
	}
}
