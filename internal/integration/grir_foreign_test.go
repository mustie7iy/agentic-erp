package integration_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// usdGRIRLine is a line of SAP's receipt document 0100000005 in USD on 5
// March naming the order item.
func usdGRIRLine(docln, account, hsl, wsl string) sap.ACDOCA {
	l := fxLine("0100000005", docln, account, hsl, wsl, "20260305", "WE")
	l.EBELN, l.EBELP = "4500000001", "00010"
	return l
}

// usdGRIR is tiny with its order in USD, valued through GR/IR in SAP: 3
// units at USD 62.50, of which SAP received 2 on 5 March, the receipt's
// document in USD debiting 520000 and crediting the GR/IR account 219000
// USD 125.00, EUR 100.00 (0.8), both lines naming the order item; and
// the supplier's parked invoice for all 3 in USD, 187.50. The GR/IR lines
// are given as received, so a test can change them.
func usdGRIR(grir ...sap.ACDOCA) *sap.Extract {
	x := tiny()
	x.EKKO[0].WAERS = "USD"
	x.EKPO[0].MENGE, x.EKPO[0].NETPR = "3", "62.50"
	x.RBKP[0].WAERS, x.RBKP[0].RMWWR = "USD", "187.50"
	x.RSEG[0].MENGE, x.RSEG[0].WRBTR = "3", "187.50"
	x.SKB1 = append(x.SKB1, sap.SKB1{BUKRS: "2000", SAKNR: "0000219000", TXT50: "GR/IR clearing", XBILK: "X"})
	if len(grir) == 0 {
		grir = []sap.ACDOCA{usdGRIRLine("000002", "0000219000", "-100.00", "-125.00")}
	}
	x.ACDOCA = append(x.ACDOCA, usdGRIRLine("000001", "0000520000", "100.00", "125.00"))
	x.ACDOCA = append(x.ACDOCA, grir...)
	return x
}

// foreignPosition is the order's single GR/IR position as listed: received,
// invoiced and migrated quantities and the balance in the order's currency
// and in local currency.
func (e env) foreignPosition(t *testing.T, order, query string) string {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/gr-ir-balances?legal_entity_id="+e.DE.String()+"&purchase_order_id="+order+query, "", "")
	items := page["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("GR/IR positions of the order: got %d, want 1: %v", len(items), items)
	}
	b := items[0].(map[string]any)
	q := func(f string) any { return b[f].(map[string]any)["value"] }
	m := func(f string) string { a := b[f].(map[string]any); return fmt.Sprint(a["amount"], " ", a["currency"]) }
	return fmt.Sprint("received ", q("received_quantity"), ", invoiced ", q("invoiced_quantity"), ", migrated ", q("migrated_quantity"),
		", balance ", m("balance"), " = ", m("balance_lc"))
}

// lines are the entry's lines on the accounts, as "amount currency =
// amount_lc currency", by account number; a line of no amount in either,
// an exchange difference in group currency alone, is left out.
func (e env) lines(t *testing.T, entry string, accounts ...string) map[string][]string {
	t.Helper()
	got, _ := e.Must(200, "GET", "/v1/journal-entries/"+entry+"?fiscal_year=2026", "", "")
	byID := map[string]string{}
	for _, a := range accounts {
		byID[e.Account(t, "2000", a).String()] = a
	}
	out := map[string][]string{}
	for _, l := range got["lines"].([]any) {
		m := l.(map[string]any)
		tc, lc := m["amount"].(map[string]any), m["amount_lc"].(map[string]any)
		if a, ok := byID[m["account_id"].(string)]; ok && (tc["amount"] != "0.00" || lc["amount"] != "0.00") {
			out[a] = append(out[a], fmt.Sprint(tc["amount"], " ", tc["currency"], " = ", lc["amount"], " ", lc["currency"]))
		}
	}
	return out
}

// TestForeignGRIRPositions: an order item SAP received in USD, company
// 2000 being in EUR, loads with its open GR/IR credit in USD, its EUR
// value as SAP booked it beside it (ADR-0005 As built, product owner
// 2026-10-31), in either mode: the order in USD, the take-over entry in
// USD at the rate SAP's amounts imply, reconciled per order item in both
// currencies. A goods receipt of the last unit on the platform moves the
// position at the platform's rate; the supplier's invoice for all three
// clears it in both currencies, the GR/IR line at the value it was booked
// at and the exchange difference on its own line.
func TestForeignGRIRPositions(t *testing.T) {
	for _, mode := range []string{"documents", "balances"} {
		t.Run(mode, func(t *testing.T) {
			e := grirSetup(t, usdGRIR())
			run := e.run(t, e.connector(t, "grir.json"), mode, false, e.DE.String())
			if _, got := e.act(t, run, "start"); got["status"] != "loaded" {
				t.Fatalf("start status: got %v, want loaded: %v", got["status"], got["report"])
			} else if n := got["report"].(map[string]any)["loaded"].(map[string]any)["gr_ir_positions"]; n != float64(1) {
				t.Errorf("GR/IR positions taken over: got %v, want 1", n)
			}
			_, got := e.act(t, run, "reconcile")
			if got["status"] != "reconciled" {
				t.Fatalf("reconcile: got %v, want reconciled: %v", got["status"], got["report"])
			}
			for _, check := range []string{"2000 Open GR/IR total true", "2000 Open GR/IR in USD total true", "2000 Open GR/IR: order items with differences true",
				"2000 Trial balance: accounts with differences true"} {
				if c := checks(got["report"].(map[string]any)); !strings.Contains(c, check) {
					t.Errorf("checks: got %s, want %s among them", c, check)
				}
			}
			order, _ := e.orderOf(t, "4500000001")
			if o, _ := e.Must(200, "GET", "/v1/purchase-orders/"+order, "", ""); o["currency"] != "USD" {
				t.Errorf("the migrated order's currency: got %v, want USD", o["currency"])
			}
			if got, want := e.foreignPosition(t, order, ""), "received 0, invoiced 0, migrated -2, balance -125.00 USD = -100.00 EUR"; got != want {
				t.Errorf("migrated position: got %s, want %s", got, want)
			}
			e.takeOverEntryInUSD(t)
			e.receiveAndInvoiceInUSD(t, order)
			if got, want := e.grirAccountByCurrency(t), "USD 0 0"; got != want {
				t.Errorf("the GR/IR account by entry currency once the position is cleared (amount, local amount): got %s, want %s", got, want)
			}
		})
	}
}

// grirAccountByCurrency is company 2000's GR/IR account by the currency
// of its entries: what its lines add up to in it and in local currency. In
// either mode SAP's receipt document in USD posts in USD, so the take-over's
// offset nets with it there, not in local currency alone.
func (e env) grirAccountByCurrency(t *testing.T) string {
	t.Helper()
	var got string
	pgtest.Scan(t, `SELECT coalesce(string_agg(c || ' ' || tc || ' ' || lc, ', ' ORDER BY c), '') FROM (
			SELECT e.currency_tc c, sum(l.amount_tc) tc, sum(l.amount_lc) lc FROM ledger.journal_line l
			JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id
			WHERE l.tenant_id = $1 AND l.account_id = $2 GROUP BY 1) s`, []any{e.Tenant.ID, e.Account(t, "2000", "219000")}, &got)
	return got
}

// takeOverEntryInUSD checks the take-over entry of order 4500000001: in
// USD at SAP's 0.8, its GR/IR lines USD 125.00 at EUR 100.00 each way.
func (e env) takeOverEntryInUSD(t *testing.T) {
	t.Helper()
	var entry string
	pgtest.Scan(t, `SELECT entity_id::text FROM integration.external_ref WHERE tenant_id = $1 AND object = 'GRIR' AND external_key = '4500000001'`,
		[]any{e.Tenant.ID}, &entry)
	got, _ := e.Must(200, "GET", "/v1/journal-entries/"+entry+"?fiscal_year=2026", "", "")
	if got["currency"] != "USD" || got["rate_tc_lc"] != "0.800000000000" {
		t.Errorf("the take-over entry: got %v at %v, want USD at SAP's 0.800000000000", got["currency"], got["rate_tc_lc"])
	}
	grir := e.lines(t, entry, "219000")["219000"]
	slices.Sort(grir)
	if want := []string{"-125.00 USD = -100.00 EUR", "125.00 USD = 100.00 EUR"}; !slices.Equal(grir, want) {
		t.Errorf("the take-over's GR/IR lines: got %v, want %v", grir, want)
	}
}

// receiveAndInvoiceInUSD receives the order's third unit on 15 March at
// the platform's average rate of 0.84 and posts the migrated invoice for
// all three, USD 187.50, on 25 March at that rate (EUR 157.50).
func (e env) receiveAndInvoiceInUSD(t *testing.T, order string) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-03-10', 0.84, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	e.Must(201, "POST", "/v1/goods-receipts", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-15", "posting_date": "2026-03-15",
		"items": [{"purchase_order_id": %q, "po_item_no": 1, "quantity": {"value": "1", "unit": "EA"}}]}`, e.DE, order), "")
	if got, want := e.foreignPosition(t, order, ""), "received 1, invoiced 0, migrated -2, balance -187.50 USD = -152.50 EUR"; got != want {
		t.Errorf("after the receipt: got %s, want %s", got, want)
	}
	page, _ := e.Must(200, "GET", "/v1/supplier-invoices?legal_entity_id="+e.DE.String()+"&status=parked", "", "")
	invoice := page["items"].([]any)[0].(map[string]any)["id"].(string)
	if m, _ := e.Must(200, "POST", "/v1/supplier-invoices/"+invoice+":match", "", `"1"`); m["is_clean"] != true {
		t.Fatalf("match of the migrated invoice: %v", m)
	}
	posted, _ := e.Must(200, "POST", "/v1/supplier-invoices/"+invoice+":post", `{"posting_date": "2026-03-25"}`, `"1"`)
	got := e.lines(t, posted["journal_entry_id"].(string), "219000", "520000", "211000", "690000", "590000")
	want := map[string][]string{"219000": {"187.50 USD = 152.50 EUR"}, "211000": {"-187.50 USD = -157.50 EUR"}, "690000": {"0.00 USD = 5.00 EUR"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the invoice's lines by account: got %v, want %v (no expense, no price difference)", got, want)
	}
	if got, want := e.foreignPosition(t, order, "&open_only=false"), "received 1, invoiced 3, migrated -2, balance 0.00 USD = 0.00 EUR"; got != want {
		t.Errorf("after the invoice: got %s, want %s", got, want)
	}
}

// TestForeignGRIRTopUp: a later run whose extract carries SAP's receipt of
// the third unit in USD tops the migrated position up in both currencies
// (review:batch21), reconciled; the same extract again posts nothing.
func TestForeignGRIRTopUp(t *testing.T) {
	e := grirSetup(t, usdGRIR())
	c := e.connector(t, "grir.json")
	if _, got := e.act(t, e.run(t, c, "documents", false, e.DE.String()), "start"); got["status"] != "loaded" {
		t.Fatalf("first run: got %v, want loaded: %v", got["status"], got["report"])
	}
	order, _ := e.orderOf(t, "4500000001")
	x := usdGRIR()
	x.MSEG = append(x.MSEG, sap.MSEG{MBLNR: "5000000002", MJAHR: "2026", ZEILE: "0001", BWART: "101", EBELN: "4500000001", EBELP: "00010", MENGE: "1",
		BUDAT: "20260312"})
	for _, l := range []sap.ACDOCA{usdGRIRLine("000001", "0000520000", "51.00", "62.50"), usdGRIRLine("000002", "0000219000", "-51.00", "-62.50")} {
		l.BELNR, l.BUDAT = "0100000006", "20260312"
		x.ACDOCA = append(x.ACDOCA, l)
	}
	write(t, e.dir, "grir.json", x)
	delta := e.run(t, c, "documents", false, e.DE.String())
	if _, got := e.act(t, delta, "start"); fmt.Sprint(got["report"].(map[string]any)["loaded"]) != "map[goods_receipts:1 gr_ir_top_ups:1 journal_entries:1]" {
		t.Errorf("loaded by the delta run: got %v, want the receipt, its document and the top-up", got["report"])
	}
	if got, want := e.foreignPosition(t, order, ""), "received 0, invoiced 0, migrated -3, balance -187.50 USD = -151.00 EUR"; got != want {
		t.Errorf("topped up: got %s, want %s", got, want)
	}
	if ok, diffs := e.grirReconciled(t, delta); !ok {
		t.Errorf("open GR/IR after the top-up: differences %s, want none", diffs)
	}
	same := e.run(t, c, "documents", false, e.DE.String())
	if _, got := e.act(t, same, "start"); got["status"] != "loaded" || len(got["report"].(map[string]any)["loaded"].(map[string]any)) != 0 {
		t.Errorf("the same extract again: got %v, want nothing loaded", got["report"])
	}
}

// TestForeignGRIRQuality: the quality gate refuses what the take-over of a
// position in its order's foreign currency cannot read rather than guess.
func TestForeignGRIRQuality(t *testing.T) {
	line := func(hsl, wsl string) sap.ACDOCA { return usdGRIRLine("000002", "0000219000", hsl, wsl) }
	inEUR := line("-100.00", "-100.00")
	inEUR.RWCUR = "EUR"
	second := line("99.00", "125.00")
	second.DOCLN = "000003"
	huge := line("-100.00", "-92233720368547758.07")
	hugeToo := huge
	hugeToo.DOCLN = "000003"
	eurOrder := grirTiny(func() sap.ACDOCA { l := grirLine(); l.RWCUR, l.WSL = "USD", "-125.00"; return l }())
	for _, c := range []struct {
		name, want string
		x          *sap.Extract
	}{
		{"a line in another currency than its order's", `is in document currency "EUR" (RWCUR), but its order 4500000001 is in USD`, usdGRIR(inEUR)},
		{"a document amount that does not parse", "document amount (WSL)", usdGRIR(line("-100.00", "-1x"))},
		{"a company code amount alone", "GR/IR line of order item 4500000001/00010 has no amount in its order's currency USD (WSL)", usdGRIR(line("-100.00", "0.00"))},
		{"amounts of opposite signs", "have opposite signs", usdGRIR(line("-100.00", "125.00"))},
		{"a balance in company currency alone", "position is -1.00 EUR in company currency (HSL) and 0.00 USD in its order's currency (WSL)",
			usdGRIR(line("-100.00", "-125.00"), second)},
		{"document amounts out of range together", "the document amounts (WSL) of order item 4500000001/00010's GR/IR lines add up to more",
			usdGRIR(huge, hugeToo)},
		{"a rate a journal entry cannot record", "the rate from USD to EUR that the amounts of order 4500000001's GR/IR lines imply",
			usdGRIR(line("-0.01", "-92233720368547758.07"))},
		{"a USD line of an order in EUR", `is in document currency USD, not the currency of its order "4500000001"`, eurOrder},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := grirSetup(t, c.x)
			run := e.run(t, e.connector(t, "grir.json"), "documents", true, e.DE.String())
			_, got := e.act(t, run, "start")
			var errors []string
			for _, q := range got["report"].(map[string]any)["quality"].([]any) {
				if m := q.(map[string]any); m["severity"] == "error" {
					errors = append(errors, m["message"].(string))
				}
			}
			if !slices.ContainsFunc(errors, func(m string) bool { return strings.Contains(m, c.want) }) {
				t.Errorf("errors: got %v, want one with %q", errors, c.want)
			}
		})
	}
}
