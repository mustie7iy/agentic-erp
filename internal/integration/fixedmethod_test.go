package integration_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// fixedSetup is a tenant whose company 2000 pays by the methods T and S.
func fixedSetup(t *testing.T) env {
	t.Helper()
	f := fixture.NewWith(t, nil, func(c *config.Config) {
		c.Company("2000").PaymentMethods = []config.PaymentMethod{
			{Code: "T", Description: "Bank transfer", Ranking: []config.BankRanking{{Currency: "EUR", HouseBanks: []string{"MAIN"}}}},
			{Code: "S", Description: "SEPA credit transfer", Ranking: []config.BankRanking{{Currency: "EUR", HouseBanks: []string{"MAIN"}}}}}
	}, perms...)
	dir := t.TempDir()
	f.API = apitest.NewWith(t, func(c *app.Config) { c.ExtractDir = dir })
	return env{Env: f, dir: dir}
}

// fixedTiny is tiny with two open invoices of its vendor, whose company
// data lists no payment method: one of 60.00 that fixes the payment method
// (ZLSCH) and one of 25.00 that fixes the other, or none for "". The
// invoice SAP cleared names a method the company does not have, which a
// cleared item may.
func fixedTiny(method, other string) *sap.Extract {
	x := tiny()
	x.ACDOCA[1].ZLSCH = "X"
	line := func(belnr, docln, account, hsl, lifnr, zlsch string) sap.ACDOCA {
		return sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", RWCUR: "EUR", GJAHR: "2026", BELNR: belnr, DOCLN: docln, RACCT: account, HSL: hsl, BUDAT: "20260304",
			BLART: "RE", LIFNR: lifnr, XBLNR: "F-" + belnr[8:], ZLSCH: zlsch}
	}
	x.ACDOCA = append(x.ACDOCA, line("0100000030", "000001", "0000520000", "60.00", "", ""),
		line("0100000030", "000002", "0000211000", "-60.00", "0000000001", method),
		line("0100000031", "000001", "0000520000", "25.00", "", ""),
		line("0100000031", "000002", "0000211000", "-25.00", "0000000001", other))
	return x
}

// openMethods are the supplier's open items as the API lists them: each
// item's amount and the payment method it fixes, by amount.
func openMethods(t *testing.T, e env) string {
	t.Helper()
	items, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+supplierOf(t, e, "0000000001").String(), "", "")
	var out []string
	for _, it := range items["items"].([]any) {
		m := it.(map[string]any)
		out = append(out, fmt.Sprint(m["amount"].(map[string]any)["amount"], " ", m["payment_method"]))
	}
	slices.Sort(out)
	return strings.Join(out, ", ")
}

// TestMigratedFixedPaymentMethod: the payment method an open SAP vendor
// item fixes (ZLSCH) comes over with the item in either mode (ADR-0038 As
// built), though its vendor's own methods (LFB1-ZWELS) do not list it, so
// the AP clerk sees it on the open item and a payment run pays the item by
// it alone; an item that fixes none has none, and a cleared item's is not
// asked about. The reconciliation counts and sums the items fixing each
// method on both sides. The same extract loaded again changes nothing; a
// method SAP fixes on an item after it was loaded is not taken over, and
// the reconciliation shows it.
func TestMigratedFixedPaymentMethod(t *testing.T) {
	passing := []string{"2000 Open items fixing payment method S true", "2000 Open items fixing payment method S total true",
		"2000 Open items fixing payment method S: vendors with differences true", "2000 Open payables total true"}
	for _, mode := range []string{"documents", "balances"} {
		t.Run(mode, func(t *testing.T) {
			e := fixedSetup(t)
			write(t, e.dir, "fixed.json", fixedTiny("S", ""))
			c := e.connector(t, "fixed.json")
			run := e.run(t, c, mode, false, e.DE.String())
			if _, got := e.act(t, run, "start"); got["status"] != "loaded" {
				t.Fatalf("start status: got %v, want loaded: %v", got["status"], got["report"])
			}
			if got, want := openMethods(t, e), "-25.00 <nil>, -60.00 S"; got != want {
				t.Errorf("the supplier's open items (amount, fixed method): got %s, want %s", got, want)
			}
			_, got := e.act(t, run, "reconcile")
			if got["status"] != "reconciled" {
				t.Fatalf("reconcile: got %v, want reconciled: %v", got["status"], got["report"])
			}
			for _, check := range passing {
				if c := checks(report(got)); !strings.Contains(c, check) {
					t.Errorf("checks: got %s, want %s among them", c, check)
				}
			}
			if c := checkOf(t, got, "2000", "Open items fixing payment method S total"); c["sap"] != "-60.00 EUR" || c["platform"] != "-60.00 EUR" {
				t.Errorf("the total of the items fixing S: got SAP %v, platform %v, want -60.00 EUR on both", c["sap"], c["platform"])
			}
			kept := `SELECT count(*) FROM ledger.open_item_payment_method WHERE tenant_id = $1`
			entries := `SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1`
			before := count(t, entries, e.Tenant.ID)
			if _, got := e.act(t, e.run(t, c, mode, false, e.DE.String()), "start"); got["status"] != "loaded" {
				t.Fatalf("the same extract again: got %v, want loaded: %v", got["status"], got["report"])
			}
			if n := count(t, kept, e.Tenant.ID); n != 1 {
				t.Errorf("fixed methods kept after the same extract again: got %d, want 1", n)
			}
			if n := count(t, entries, e.Tenant.ID); n != before {
				t.Errorf("journal entries after the same extract again: got %d, want %d as before", n, before)
			}
			// SAP fixes T on the other invoice after it was loaded: an open item's
			// data is not changed, and the reconciliation says so.
			write(t, e.dir, "fixed.json", fixedTiny("S", "T"))
			later := e.run(t, c, mode, false, e.DE.String())
			if _, got := e.act(t, later, "start"); got["status"] != "loaded" {
				t.Fatalf("the extract with the method SAP fixed later: got %v, want loaded: %v", got["status"], got["report"])
			}
			_, got = e.act(t, later, "reconcile")
			if got["status"] != "failed" {
				t.Fatalf("reconcile after SAP fixed a method on a loaded item: got %v, want failed: %v", got["status"], got["report"])
			}
			if want, diffs := "key:payment method T items of vendor 1 platform:0 items, 0.00 EUR sap:1 items, -25.00 EUR", fmt.Sprint(report(got)["diffs"]); !strings.Contains(diffs, want) {
				t.Errorf("differences: got %s, want %s among them", diffs, want)
			}
		})
	}
}

// TestMigratedFixedMethodRefused: the quality gate refuses a run whose
// open vendor item fixes a payment method the company's configuration
// does not have, before anything loads.
func TestMigratedFixedMethodRefused(t *testing.T) {
	e := fixedSetup(t)
	write(t, e.dir, "unknown.json", fixedTiny("Q", ""))
	_, got := e.act(t, e.run(t, e.connector(t, "unknown.json"), "documents", false, e.DE.String()), "start")
	if got["status"] != "failed" {
		t.Fatalf("run status: got %v, want failed: %v", got["status"], got["report"])
	}
	want := `key:2000/2026/0100000030/000002 message:payment method "Q" the open item of vendor 0000000001 fixes (ZLSCH) is not a payment method of company 2000 ` +
		`in the configuration severity:error table:ACDOCA`
	if quality := fmt.Sprint(report(got)["quality"]); !strings.Contains(quality, want) {
		t.Errorf("quality gate: got %s, want it to report %q", quality, want)
	}
	if n := count(t, `SELECT count(*) FROM integration.external_ref WHERE tenant_id = $1`, e.Tenant.ID); n != 0 {
		t.Errorf("records loaded by the refused run: got %d, want 0", n)
	}
}
