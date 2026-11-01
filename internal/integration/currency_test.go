package integration_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// fxLine is a line of company 2000 of 2026 in USD.
func fxLine(belnr, docln, account, hsl, wsl, budat, blart string) sap.ACDOCA {
	return sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", RWCUR: "USD", GJAHR: "2026", BELNR: belnr, DOCLN: docln, RACCT: account, HSL: hsl, WSL: wsl,
		BUDAT: budat, BLART: blart}
}

// vendorLine is fxLine on the payables account of vendor 0000000001.
func vendorLine(belnr, docln, hsl, wsl, budat, blart, augbl string) sap.ACDOCA {
	l := fxLine(belnr, docln, "0000211000", hsl, wsl, budat, blart)
	l.LIFNR, l.AUGBL = "0000000001", augbl
	return l
}

// fxTiny is dpTiny with documents in USD, company 2000 being in EUR: an
// open invoice of USD 100.00, EUR 80.00 on 2 March (0.8, the fixture's
// average rate then); an open down payment of USD 500.00 with its input
// tax of USD 95.00 on V1 (19%), EUR 400.00 and 76.00; and an invoice of
// USD 50.00, EUR 40.00 that SAP cleared with a payment of USD 50.00 from
// the bank at EUR 41.00, the vendor line at the invoice's EUR 40.00 and
// the exchange difference of EUR 1.00 a line of no amount in USD.
func fxTiny() *sap.Extract {
	x := dpTiny(true)
	x.SKB1 = append(x.SKB1, sap.SKB1{BUKRS: "2000", SAKNR: "0000610000", TXT50: "Exchange losses"})
	dp := fxLine("0100000021", "000001", "0000159000", "400.00", "500.00", "20260305", "KZ")
	dp.LIFNR, dp.UMSKZ, dp.MWSKZ, dp.XBLNR = "0000000001", "A", "V1", "DP-USD"
	tax := fxLine("0100000021", "000002", "0000140000", "76.00", "95.00", "20260305", "KZ")
	tax.MWSKZ = "V1"
	x.ACDOCA = append(x.ACDOCA,
		fxLine("0100000020", "000001", "0000520000", "80.00", "100.00", "20260302", "RE"),
		vendorLine("0100000020", "000002", "-80.00", "-100.00", "20260302", "RE", ""),
		dp, tax, fxLine("0100000021", "000003", "0000100000", "-476.00", "-595.00", "20260305", "KZ"),
		fxLine("0100000022", "000001", "0000520000", "40.00", "50.00", "20260303", "RE"),
		vendorLine("0100000022", "000002", "-40.00", "-50.00", "20260303", "RE", "0100000023"),
		vendorLine("0100000023", "000001", "40.00", "50.00", "20260320", "KZ", "0100000023"),
		fxLine("0100000023", "000002", "0000100000", "-41.00", "-50.00", "20260320", "KZ"),
		fxLine("0100000023", "000003", "0000610000", "1.00", "0.00", "20260320", "KZ"))
	return x
}

// TestForeignCurrencyItems: open SAP items in a foreign document currency
// load in that currency (ADR-0005 As built, product owner 2026-10-31): the
// AP clerk sees the invoice as an open item of USD 100.00 worth EUR 80.00,
// its entry at the rate SAP's amounts imply, the down payment as a paid
// request in USD, and SAP's clearing of a USD invoice by a USD payment
// with its exchange difference is taken over; the reconciliation still
// balances in EUR and compares the USD items in USD; and a revaluation
// values the migrated item as it values one the platform posted in USD.
func TestForeignCurrencyItems(t *testing.T) {
	e := dpSetup(t)
	write(t, e.dir, "fx.json", fxTiny())
	run := e.run(t, e.connector(t, "fx.json"), "documents", false, e.DE.String())
	if _, got := e.act(t, run, "start"); got["status"] != "loaded" {
		t.Fatalf("start status: got %v, want loaded: %v", got["status"], got["report"])
	}
	_, got := e.act(t, run, "reconcile")
	if got["status"] != "reconciled" {
		t.Fatalf("reconcile: got %v, want reconciled: %v", got["status"], got["report"])
	}
	for _, check := range []string{"2000 Trial balance: accounts with differences true", "2000 Open payables total true",
		"2000 Open down payments total true", "2000 Open items in USD total true", "2000 Open items in USD: vendors with differences true"} {
		if c := checks(got["report"].(map[string]any)); !strings.Contains(c, check) {
			t.Errorf("checks: got %s, want %s among them", c, check)
		}
	}
	supplier := supplierOf(t, e, "0000000001")
	items, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+supplier.String(), "", "")
	var usd []string
	var entry string
	for _, it := range items["items"].([]any) {
		m := it.(map[string]any)
		if a := m["amount"].(map[string]any); a["currency"] == "USD" {
			usd = append(usd, fmt.Sprint(a["amount"], " ", m["amount_lc"].(map[string]any)["amount"], " ", m["special_gl"]))
			if m["special_gl"] == nil {
				entry = m["journal_entry_id"].(string)
			}
		}
	}
	if got, want := strings.Join(usd, ", "), "-100.00 -80.00 <nil>, 500.00 400.00 A"; got != want {
		t.Errorf("the supplier's open USD items (USD EUR special G/L): got %s, want %s", got, want)
	}
	if got, _ := e.Must(200, "GET", "/v1/journal-entries/"+entry+"?fiscal_year=2026", "", ""); got["rate_tc_lc"] != "0.800000000000" {
		t.Errorf("the invoice's entry rate: got %v, want 0.800000000000, SAP's", got["rate_tc_lc"])
	}
	page, _ := e.Must(200, "GET", "/v1/down-payment-requests?status=paid&supplier_id="+supplier.String(), "", "")
	var requests []string
	for _, r := range page["items"].([]any) {
		m := r.(map[string]any)
		requests = append(requests, fmt.Sprint(m["amount"], " ", m["tax_amount"]))
	}
	slices.Sort(requests)
	if got, want := strings.Join(requests, ", "), "map[amount:1190.00 currency:EUR] map[amount:190.00 currency:EUR], "+
		"map[amount:595.00 currency:USD] map[amount:95.00 currency:USD]"; got != want {
		t.Errorf("paid down payment requests: got %s, want %s", got, want)
	}
	e.revaluedLikeNative(t, entry)
}

// revaluedLikeNative: once the company revalues its payables and has a USD
// item of 100.00 posted on 2 March at the average rate (fixture
// Revalues), a revaluation at the end of March values the migrated USD
// item as that one: the same booked amounts, rate and difference.
func (e env) revaluedLikeNative(t *testing.T, migrated string) {
	t.Helper()
	e.Revalues(t, func(doc *config.Config) {
		de := doc.Company("2000")
		de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "159000", Name: "Down payments made", Type: config.Asset, VendorReconciliation: true})
		de.AccountDetermination.DownPayment = "159000"
	})
	got, _ := e.Must(200, "POST", "/v1/revaluation-runs/preview", fmt.Sprintf(`{"legal_entity_id": %q, "key_date": "2026-03-31"}`, e.DE), "")
	valued := map[bool][]string{}
	for _, x := range got["items"].([]any) {
		it := x.(map[string]any)
		valued[it["journal_entry_id"] == migrated] = append(valued[it["journal_entry_id"] == migrated],
			fmt.Sprint(it["amount"], it["amount_lc"], it["rate"], it["valued_lc"], it["difference_lc"], it["direction"]))
	}
	if len(valued[true]) != 1 || len(valued[false]) != 1 || valued[true][0] != valued[false][0] {
		t.Errorf("revaluation: got the migrated item %v, want it valued as the natively posted %v", valued[true], valued[false])
	}
}

// TestForeignCurrencyItemsBalances: in balances mode the documents holding
// an open item in USD load on their own beside the opening entry, in USD,
// and the cleared USD invoice and payment are part of the opening
// balances; the reconciliation balances in EUR and in USD.
func TestForeignCurrencyItemsBalances(t *testing.T) {
	e := dpSetup(t)
	write(t, e.dir, "fx.json", fxTiny())
	run := e.run(t, e.connector(t, "fx.json"), "balances", false, e.DE.String())
	if _, got := e.act(t, run, "start"); got["status"] != "loaded" {
		t.Fatalf("start status: got %v, want loaded: %v", got["status"], got["report"])
	}
	if _, got := e.act(t, run, "reconcile"); got["status"] != "reconciled" {
		t.Fatalf("reconcile: got %v, want reconciled: %v", got["status"], got["report"])
	}
	items, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+supplierOf(t, e, "0000000001").String(), "", "")
	var usd []string
	for _, it := range items["items"].([]any) {
		m := it.(map[string]any)
		if a := m["amount"].(map[string]any); a["currency"] == "USD" {
			usd = append(usd, fmt.Sprint(a["amount"], " ", m["amount_lc"].(map[string]any)["amount"]))
		}
	}
	slices.Sort(usd)
	if got, want := strings.Join(usd, ", "), "-100.00 -80.00, 500.00 400.00"; got != want {
		t.Errorf("the supplier's open USD items (USD EUR): got %s, want %s", got, want)
	}
}

// usdInvoice is tiny with an open USD invoice of 100.00, EUR 80.00, and,
// when currency is given, its payment in that currency on 20 March: in
// USD the bank at EUR 82.00 and an exchange difference of EUR 2.00 in
// company currency alone, in EUR at EUR 80.00.
func usdInvoice(currency string) *sap.Extract {
	x := tiny()
	x.SKB1 = append(x.SKB1, sap.SKB1{BUKRS: "2000", SAKNR: "0000610000", TXT50: "Exchange losses"})
	augbl := ""
	if currency != "" {
		augbl = "0100000030"
	}
	x.ACDOCA = append(x.ACDOCA, fxLine("0100000020", "000001", "0000520000", "80.00", "100.00", "20260302", "RE"),
		vendorLine("0100000020", "000002", "-80.00", "-100.00", "20260302", "RE", augbl))
	switch currency {
	case "USD":
		x.ACDOCA = append(x.ACDOCA, vendorLine("0100000030", "000001", "80.00", "100.00", "20260320", "KZ", augbl),
			fxLine("0100000030", "000002", "0000100000", "-82.00", "-100.00", "20260320", "KZ"),
			fxLine("0100000030", "000003", "0000610000", "2.00", "0.00", "20260320", "KZ"))
	case "EUR":
		pay, bank := vendorLine("0100000030", "000001", "80.00", "80.00", "20260320", "KZ", augbl),
			fxLine("0100000030", "000002", "0000100000", "-80.00", "-80.00", "20260320", "KZ")
		pay.RWCUR, bank.RWCUR = "EUR", "EUR"
		x.ACDOCA = append(x.ACDOCA, pay, bank)
	}
	return x
}

// TestForeignItemAcrossRuns: a USD item an earlier run loaded open is
// cleared by the final delta run that loads SAP's payment of it in USD,
// with its exchange difference; paid in EUR instead, the delta run's
// quality gate refuses the payment, which could not clear the USD item,
// rather than leave the clearing to fail in the load.
func TestForeignItemAcrossRuns(t *testing.T) {
	for _, c := range []struct{ currency, status string }{{"USD", "loaded"}, {"EUR", "failed"}} {
		t.Run("paid in "+c.currency, func(t *testing.T) {
			e := dpSetup(t)
			write(t, e.dir, "fx.json", usdInvoice(""))
			conn := e.connector(t, "fx.json")
			if _, got := e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start"); got["status"] != "loaded" {
				t.Fatalf("first run: got %v, want loaded: %v", got["status"], got["report"])
			}
			write(t, e.dir, "fx.json", usdInvoice(c.currency))
			_, got := e.act(t, e.run(t, conn, "documents", false, e.DE.String()), "start")
			if got["status"] != c.status {
				t.Fatalf("delta run: got %v, want %s: %v", got["status"], c.status, got["report"])
			}
			report := got["report"].(map[string]any)
			if c.status == "failed" {
				if q := fmt.Sprint(report["quality"]); !strings.Contains(q, "SAP cleared it together with documents an earlier run loaded in USD") {
					t.Errorf("quality: got %s, want the EUR payment refused", q)
				}
				return
			}
			if n := report["loaded"].(map[string]any)["clearings"]; n != float64(1) {
				t.Errorf("clearings of the delta run: got %v, want the USD item's", n)
			}
			items, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+supplierOf(t, e, "0000000001").String(), "", "")
			for _, it := range items["items"].([]any) {
				if a := it.(map[string]any)["amount"].(map[string]any); a["currency"] == "USD" {
					t.Errorf("open USD items after the payment: got %v, want none", a)
				}
			}
		})
	}
}

// TestDocumentAmountAloneRun (review:batch31): in either mode, a run whose
// extract holds an open vendor item with an amount in its document
// currency and none in company currency, in a document that posts in
// company currency, fails at the quality gate naming its document and
// line and posts nothing, where the journal posting (documents) or the
// opening entry (balances) would have left it out; an open item in
// company currency of a company code amount alone loads from it.
func TestDocumentAmountAloneRun(t *testing.T) {
	small := func(x *sap.Extract, belnr, debit, credit, hsl, wsl string) {
		line := fxLine(belnr, "000001", "0000520000", strings.TrimPrefix(hsl, "-"), strings.TrimPrefix(wsl, "-"), "20260310", "RE")
		line.RWCUR = debit
		vendor := vendorLine(belnr, "000002", hsl, wsl, "20260310", "RE", "")
		vendor.RWCUR = credit
		x.ACDOCA = append(x.ACDOCA, line, vendor)
	}
	for _, mode := range []string{"documents", "balances"} {
		t.Run(mode, func(t *testing.T) {
			e := dpSetup(t)
			x := fxTiny()
			small(x, "0100000024", "EUR", "USD", "0.00", "-0.01")
			small(x, "0100000025", "EUR", "EUR", "0.00", "-0.01")
			write(t, e.dir, "alone.json", x)
			before := e.entries(t)
			_, got := e.act(t, e.run(t, e.connector(t, "alone.json"), mode, false, e.DE.String()), "start")
			if got["status"] != "failed" {
				t.Fatalf("status: got %v, want failed: %v", got["status"], got["report"])
			}
			var refused []string
			for _, i := range got["report"].(map[string]any)["quality"].([]any) {
				if m := i.(map[string]any); m["severity"] == "error" {
					refused = append(refused, fmt.Sprint(m["key"]))
				}
			}
			if got, want := strings.Join(refused, ", "), "2000/2026/0100000024/000002, 2000/2026/0100000025/000002"; got != want {
				t.Errorf("lines refused: got %s, want %s", got, want)
			}
			if n := e.entries(t); n != before {
				t.Errorf("journal entries: got %d, want %d, nothing posted", n, before)
			}

			x = fxTiny()
			small(x, "0100000025", "EUR", "EUR", "-50.00", "0.00")
			write(t, e.dir, "local.json", x)
			if _, got := e.act(t, e.run(t, e.connector(t, "local.json"), mode, false, e.DE.String()), "start"); got["status"] != "loaded" {
				t.Fatalf("company code amount alone: got %v, want loaded: %v", got["status"], got["report"])
			}
			items, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+supplierOf(t, e, "0000000001").String(), "", "")
			var eur []string
			for _, it := range items["items"].([]any) {
				if a := it.(map[string]any)["amount"].(map[string]any); a["currency"] == "EUR" && a["amount"] == "-50.00" {
					eur = append(eur, fmt.Sprint(a["amount"]))
				}
			}
			if len(eur) != 1 {
				t.Errorf("the open EUR item of a company code amount alone: got %v, want one of -50.00", eur)
			}
		})
	}
}
