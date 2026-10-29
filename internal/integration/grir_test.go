package integration_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// grirSetup is a tenant whose legal entity 2000 has the GR/IR account
// 219000 and the price difference account 590000 (fixture.GRIR), whose
// people may also post invoices, close orders and clear GR/IR, a
// controller who approves clearings, and whose API reads the extract x
// (grir.json).
func grirSetup(t *testing.T, x *sap.Extract) env {
	t.Helper()
	f := fixture.New(t, append(slices.Clone(perms), "payables.invoice.post", "procurement.order.release", "procurement.gr_ir.clear")...)
	dir := t.TempDir()
	f.API = apitest.NewWith(t, func(c *app.Config) { c.ExtractDir = dir })
	f.GRIR(t)
	f.Controller(t)
	write(t, dir, "grir.json", x)
	return env{Env: f, dir: dir}
}

// grirTiny is tiny with its order valued through GR/IR in SAP: the receipt
// of its 2 units (MSEG) posted Dr 520000 / Cr the GR/IR account 219000
// 100.00, both lines naming the order item, and not yet invoiced (its
// invoice is parked). The GR/IR lines are given as received, so a test
// can change them.
func grirTiny(grir ...sap.ACDOCA) *sap.Extract {
	x := tiny()
	x.SKB1 = append(x.SKB1, sap.SKB1{BUKRS: "2000", SAKNR: "0000219000", TXT50: "GR/IR clearing", XBILK: "X"})
	x.ACDOCA = append(x.ACDOCA, sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", RWCUR: "EUR", GJAHR: "2026", BELNR: "0100000005", DOCLN: "000001", RACCT: "0000520000",
		HSL: "100.00", BUDAT: "20260305", BLART: "WE", EBELN: "4500000001", EBELP: "00010"})
	return withGRIR(x, grir...)
}

// grirLine is the receipt's GR/IR line of grirTiny, crediting the order item.
func grirLine() sap.ACDOCA {
	return sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", RWCUR: "EUR", GJAHR: "2026", BELNR: "0100000005", DOCLN: "000002", RACCT: "0000219000", HSL: "-100.00",
		BUDAT: "20260305", BLART: "WE", EBELN: "4500000001", EBELP: "00010"}
}

func withGRIR(x *sap.Extract, grir ...sap.ACDOCA) *sap.Extract {
	if len(grir) == 0 {
		grir = []sap.ACDOCA{grirLine()}
	}
	x.ACDOCA = append(x.ACDOCA, grir...)
	return x
}

// position is the order's single GR/IR position as listed: received,
// invoiced, cleared and migrated quantities and the balance.
func (e env) position(t *testing.T, order string, query string) string {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/gr-ir-balances?legal_entity_id="+e.DE.String()+"&purchase_order_id="+order+query, "", "")
	items := page["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("GR/IR positions of the order: got %d, want 1: %v", len(items), items)
	}
	b := items[0].(map[string]any)
	q := func(f string) any { return b[f].(map[string]any)["value"] }
	return fmt.Sprint("order ", b["purchase_order_number"], " item ", b["po_item_no"], ": received ", q("received_quantity"), ", invoiced ",
		q("invoiced_quantity"), ", cleared ", q("cleared_quantity"), ", migrated ", q("migrated_quantity"), ", balance ", b["balance"].(map[string]any)["amount"])
}

// orderOf is the platform order the load made of an SAP order, and its number.
func (e env) orderOf(t *testing.T, ebeln string) (string, string) {
	t.Helper()
	var id string
	pgtest.Scan(t, `SELECT entity_id::text FROM integration.external_ref WHERE tenant_id = $1 AND object = 'EKKO' AND external_key = $2`,
		[]any{e.Tenant.ID, ebeln}, &id)
	o, _ := e.Must(200, "GET", "/v1/purchase-orders/"+id, "", "")
	if o["valuation"] != "gr_ir" {
		t.Errorf("valuation of the migrated order: got %v, want gr_ir", o["valuation"])
	}
	return id, o["document_number"].(string)
}

// TestOpenGRIRPositions: an order item SAP received and did not invoice
// loads with its open GR/IR credit (T72f), in either mode: the order valued
// through GR/IR, the position against the platform's order item in the
// GR/IR balances, reconciled per order item; the supplier's invoice after
// cutover clears it at the order price without booking the expense again.
func TestOpenGRIRPositions(t *testing.T) {
	for _, mode := range []string{"documents", "balances"} {
		t.Run(mode, func(t *testing.T) {
			e := grirSetup(t, grirTiny())
			run := e.run(t, e.connector(t, "grir.json"), mode, false, e.DE.String())
			if _, got := e.act(t, run, "start"); got["status"] != "loaded" {
				t.Fatalf("start status: got %v, want loaded: %v", got["status"], got["report"])
			} else if n := got["report"].(map[string]any)["loaded"].(map[string]any)["gr_ir_positions"]; n != float64(1) {
				t.Errorf("GR/IR positions taken over: got %v, want 1", n)
			}
			_, got := e.act(t, run, "reconcile")
			if got["status"] != "reconciled" {
				t.Fatalf("reconcile: %v", got["report"])
			}
			for _, check := range []string{"2000 Open GR/IR total true", "2000 Open GR/IR: order items with differences true"} {
				if c := checks(got["report"].(map[string]any)); !strings.Contains(c, check) {
					t.Errorf("checks: got %s, want %s among them", c, check)
				}
			}
			order, number := e.orderOf(t, "4500000001")
			if got, want := e.position(t, order, ""), "order "+number+" item 1: received 0, invoiced 0, cleared 0, migrated -2, balance -100.00"; got != want {
				t.Errorf("migrated position: got %s, want %s", got, want)
			}
			e.postMigratedInvoice(t)
			if got, want := e.position(t, order, "&open_only=false"), "order "+number+" item 1: received 0, invoiced 2, cleared 0, migrated -2, balance 0.00"; got != want {
				t.Errorf("after the invoice: got %s, want %s", got, want)
			}
		})
	}
}

// postMigratedInvoice matches and posts the parked migrated invoice T-1
// and checks its entry: the GR/IR account debited, no expense.
func (e env) postMigratedInvoice(t *testing.T) {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/supplier-invoices?legal_entity_id="+e.DE.String()+"&status=parked", "", "")
	invoice := page["items"].([]any)[0].(map[string]any)["id"].(string)
	if m, _ := e.Must(200, "POST", "/v1/supplier-invoices/"+invoice+":match", "", `"1"`); m["is_clean"] != true {
		t.Fatalf("match of the migrated invoice: %v", m)
	}
	posted, _ := e.Must(200, "POST", "/v1/supplier-invoices/"+invoice+":post", `{"posting_date": "2026-03-25"}`, `"1"`)
	entry, _ := e.Must(200, "GET", "/v1/journal-entries/"+posted["journal_entry_id"].(string), "", "")
	byAccount := map[string]string{}
	for _, l := range entry["lines"].([]any) {
		m := l.(map[string]any)
		byAccount[m["account_id"].(string)] = m["amount"].(map[string]any)["amount"].(string)
	}
	if got := byAccount[e.Account(t, "2000", "219000").String()]; got != "100.00" {
		t.Errorf("the invoice's GR/IR line: got %q, want 100.00", got)
	}
	if got, ok := byAccount[e.Account(t, "2000", "520000").String()]; ok {
		t.Errorf("the invoice's expense line: got %s, want none (booked by SAP's receipt)", got)
	}
}

// TestMigratedGRIRClearing: a migrated residual of a closed order is
// cleared by a GR/IR clearing (SAP MR11, T72e) as the platform's own.
func TestMigratedGRIRClearing(t *testing.T) {
	e := grirSetup(t, grirTiny())
	run := e.run(t, e.connector(t, "grir.json"), "documents", false, e.DE.String())
	if _, got := e.act(t, run, "start"); got["status"] != "loaded" {
		t.Fatalf("start status: got %v, want loaded: %v", got["status"], got["report"])
	}
	order, number := e.orderOf(t, "4500000001")
	_, tag := e.Must(200, "GET", "/v1/purchase-orders/"+order, "", "")
	e.Must(200, "POST", "/v1/purchase-orders/"+order+":close", "", tag)
	c, _ := e.Must(201, "POST", "/v1/purchase-orders/"+order+"/gr-ir-clearings", `{"reason": "SAP's receipt will not be invoiced", "posting_date": "2026-03-20"}`, "")
	if got := fmt.Sprint(c["total"]); got != "map[amount:-100.00 currency:EUR]" {
		t.Errorf("clearing proposed: got %s, want -100.00 EUR", got)
	}
	e.Approve(t, c["id"].(string))
	if got, want := e.position(t, order, "&open_only=false"), "order "+number+" item 1: received 0, invoiced 0, cleared 2, migrated -2, balance 0.00"; got != want {
		t.Errorf("after the clearing: got %s, want %s", got, want)
	}
	// Reconciled after the clearing, the position SAP still holds open differs.
	_, got := e.act(t, run, "reconcile")
	if c := checks(got["report"].(map[string]any)); !strings.Contains(c, "2000 Open GR/IR: order items with differences false") {
		t.Errorf("checks: got %s, want the GR/IR difference", c)
	}
	diffs := fmt.Sprint(got["report"].(map[string]any)["diffs"])
	if want := "key:GR/IR of order item 4500000001/00010 platform: sap:-100.00 EUR, quantity -2"; !strings.Contains(diffs, want) {
		t.Errorf("diffs: got %s, want %s", diffs, want)
	}
}

// partlyInvoiced is grirTiny with three more items received through GR/IR
// and an invoice SAP posted on all four: item 10 invoiced 1 of 2, item 20
// (2 at 5.00) 1 of 2, item 30 (1 at 7.00) in full and then credited by a
// posted credit memo, and item 40 (1 at 3.00) in full, so items 10 to 30
// are open, item 30 for its 1 unit, and 40 is not.
func partlyInvoiced() *sap.Extract {
	x := grirTiny()
	line := func(belnr, docln, account, hsl, item, blart string) sap.ACDOCA {
		return sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", RWCUR: "EUR", GJAHR: "2026", BELNR: belnr, DOCLN: docln, RACCT: account, HSL: hsl, BUDAT: "20260306",
			BLART: blart, EBELN: "4500000001", EBELP: item}
	}
	for _, it := range []struct{ item, qty, price, value string }{{"00020", "2", "5.00", "10.00"}, {"00030", "1", "7.00", "7.00"}, {"00040", "1", "3.00", "3.00"}} {
		x.EKPO = append(x.EKPO, sap.EKPO{EBELN: "4500000001", EBELP: it.item, MENGE: it.qty, NETPR: it.price, PEINH: "1", SAKTO: "0000520000"})
		x.MSEG = append(x.MSEG, sap.MSEG{MBLNR: "50000000" + it.item[2:4], MJAHR: "2026", ZEILE: "0001", BWART: "101", EBELN: "4500000001", EBELP: it.item,
			MENGE: it.qty, BUDAT: "20260306"})
		belnr := "01000001" + it.item[2:4]
		x.ACDOCA = append(x.ACDOCA, line(belnr, "000001", "0000520000", it.value, it.item, "WE"), line(belnr, "000002", "0000219000", "-"+it.value, it.item, "WE"))
	}
	x.RBKP = append(x.RBKP, sap.RBKP{BELNR: "5100000030", GJAHR: "2026", BUKRS: "2000", LIFNR: "0000000001", XBLNR: "P-30", BLDAT: "20260307", WAERS: "EUR",
		RMWWR: "65.00", WMWST1: "0.00", RBSTAT: "5", XRECH: sap.Indicator("X")})
	vendor := line("0100000030", "000005", "0000211000", "-65.00", "", "RE")
	vendor.EBELN, vendor.LIFNR = "", "0000000001"
	for i, it := range []struct{ item, qty, value string }{{"00010", "1", "50.00"}, {"00020", "1", "5.00"}, {"00030", "1", "7.00"}, {"00040", "1", "3.00"}} {
		x.RSEG = append(x.RSEG, sap.RSEG{BELNR: "5100000030", GJAHR: "2026", BUZEI: fmt.Sprintf("%06d", i+1), EBELN: "4500000001", EBELP: it.item,
			MENGE: it.qty, WRBTR: it.value})
		x.ACDOCA = append(x.ACDOCA, line("0100000030", fmt.Sprintf("%06d", i+1), "0000219000", it.value, it.item, "RE"))
	}
	x.ACDOCA = append(x.ACDOCA, vendor)
	x.RBKP = append(x.RBKP, sap.RBKP{BELNR: "5100000031", GJAHR: "2026", BUKRS: "2000", LIFNR: "0000000001", XBLNR: "CM-31", BLDAT: "20260308", WAERS: "EUR",
		RMWWR: "7.00", WMWST1: "0.00", RBSTAT: "5", XRECH: sap.Indicator("")})
	x.RSEG = append(x.RSEG, sap.RSEG{BELNR: "5100000031", GJAHR: "2026", BUZEI: "000001", EBELN: "4500000001", EBELP: "00030", MENGE: "1", WRBTR: "7.00"})
	credit := line("0100000031", "000002", "0000211000", "7.00", "", "RE")
	credit.EBELN, credit.LIFNR = "", "0000000001"
	x.ACDOCA = append(x.ACDOCA, line("0100000031", "000001", "0000219000", "-7.00", "00030", "RE"), credit)
	return x
}

// TestPartlyInvoicedGRIR: what SAP invoiced counts in the open quantity,
// credit memos negatively, and an order's open positions are taken over
// in one entry.
func TestPartlyInvoicedGRIR(t *testing.T) {
	e := grirSetup(t, partlyInvoiced())
	run := e.run(t, e.connector(t, "grir.json"), "documents", false, e.DE.String())
	if _, got := e.act(t, run, "start"); got["status"] != "loaded" {
		t.Fatalf("start status: got %v, want loaded: %v", got["status"], got["report"])
	} else if n := got["report"].(map[string]any)["loaded"].(map[string]any)["gr_ir_positions"]; n != float64(3) {
		t.Errorf("GR/IR positions taken over: got %v, want 3", n)
	}
	if _, got := e.act(t, run, "reconcile"); got["status"] != "reconciled" {
		t.Fatalf("reconcile: %v", got["report"])
	}
	order, _ := e.orderOf(t, "4500000001")
	page, _ := e.Must(200, "GET", "/v1/gr-ir-balances?legal_entity_id="+e.DE.String()+"&purchase_order_id="+order, "", "")
	var got []string
	for _, it := range page["items"].([]any) {
		b := it.(map[string]any)
		got = append(got, fmt.Sprint(b["po_item_no"], " ", b["migrated_quantity"].(map[string]any)["value"], " ", b["balance"].(map[string]any)["amount"]))
	}
	slices.Sort(got)
	if want := []string{"1 -1 -50.00", "2 -1 -5.00", "3 -1 -7.00"}; !slices.Equal(got, want) {
		t.Errorf("open positions: got %v, want %v", got, want)
	}
}

// TestGRIRQuality: the quality gate refuses GR/IR data it cannot read
// rather than guess (T72f).
func TestGRIRQuality(t *testing.T) {
	noItem := grirLine()
	noItem.EBELP = ""
	outside := grirLine()
	outside.EBELN = "4500000099"
	foreign := grirTiny()
	foreign.EKKO[0].WAERS = "USD"
	reversed := grirTiny()
	reversed.MSEG = append(reversed.MSEG, sap.MSEG{MBLNR: "5000000002", MJAHR: "2026", ZEILE: "0001", BWART: "102", EBELN: "4500000001", EBELP: "00010",
		MENGE: "1", BUDAT: "20260306"})
	unread := grirTiny()
	unread.MSEG[0].MENGE = "two"
	noBalance := grirTiny(grirLine(), grirLine())
	noBalance.ACDOCA[len(noBalance.ACDOCA)-1].DOCLN, noBalance.ACDOCA[len(noBalance.ACDOCA)-1].HSL = "000003", "100.00"
	noBalance.ACDOCA[len(noBalance.ACDOCA)-3].HSL = "0.00" // the document still balances
	unvalued := grirTiny()
	unvalued.EKPO = append(unvalued.EKPO, sap.EKPO{EBELN: "4500000001", EBELP: "00020", MENGE: "1", NETPR: "5.00", PEINH: "1", SAKTO: "0000520000"})
	unvalued.MSEG = append(unvalued.MSEG, sap.MSEG{MBLNR: "5000000003", MJAHR: "2026", ZEILE: "0001", BWART: "101", EBELN: "4500000001", EBELP: "00020",
		MENGE: "1", BUDAT: "20260306"})
	// An order of company 1000 in EUR, named by 2000's GR/IR line: outside
	// a scope of 2000 alone, and in a scope of both, of the other company.
	otherCompany := grirTiny()
	otherCompany.T001 = append(otherCompany.T001, sap.T001{BUKRS: "1000", BUTXT: "Acme Corp US", WAERS: "USD", LAND1: "US"})
	otherCompany.EKKO = append(otherCompany.EKKO, sap.EKKO{EBELN: "4500000002", BUKRS: "1000", LIFNR: "0000000001", WAERS: "EUR", BEDAT: "20260301"})
	otherCompany.EKPO = append(otherCompany.EKPO, sap.EKPO{EBELN: "4500000002", EBELP: "00010", MENGE: "1", NETPR: "5.00", PEINH: "1", SAKTO: "0000520000"})
	otherCompany.ACDOCA[len(otherCompany.ACDOCA)-1].EBELN = "4500000002"
	otherAccount := tiny()
	otherAccount.T030 = []sap.T030{{BUKRS: "2000", KTOSL: "WRX", KONTS: "0000520000"}}
	for _, c := range []struct {
		name, want string
		x          *sap.Extract
		both       bool // scoped to 1000 and 2000, not 2000 alone
	}{
		{"a GR/IR line without its order item", "without its order item (EBELN/EBELP)", grirTiny(noItem), false},
		{"an order outside the scope", "names order item 4500000002/00010, which is not in the extract", otherCompany, false},
		{"an order of another company", "line on the GR/IR account 219000 of company 2000 names order 4500000002 of company 1000", otherCompany, true},
		{"no T030 to tell SAP's GR/IR account by", "", grirTiny(), false},
		{"an order item outside the extract", "names order item 4500000099/00010, which is not in the extract", grirTiny(outside), false},
		{"an order in another currency", "order 4500000001 is in USD and has GR/IR positions", foreign, false},
		{"a reversed receipt", "movement type 102 on order item 4500000001/00010", reversed, false},
		{"a receipt quantity that does not parse", "the received quantity of order item 4500000001/00010 cannot be read", unread, false},
		{"an open quantity without a balance", "open GR/IR quantity of -2 and no balance", noBalance, false},
		{"an item received without GR/IR", "item 4500000001/00020 was received in SAP without a GR/IR line", unvalued, false},
		{"SAP's GR/IR account not the platform's", "SAP's GR/IR account 520000 has lines but is not the legal entity's gr_ir account", otherAccount, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := grirSetup(t, c.x)
			scope := []string{e.DE.String()}
			if c.both {
				scope = append(scope, e.US.String())
			}
			run := e.run(t, e.connector(t, "grir.json"), "documents", true, scope...)
			_, got := e.act(t, run, "start")
			severity, want := "error", c.want
			if want == "" {
				severity, want = "warning", "no T030 to tell SAP's GR/IR account (WRX) by"
			}
			var messages []string
			for _, q := range got["report"].(map[string]any)["quality"].([]any) {
				if m := q.(map[string]any); m["severity"] == severity {
					messages = append(messages, m["message"].(string))
				}
			}
			if !slices.ContainsFunc(messages, func(m string) bool { return strings.Contains(m, want) }) {
				t.Errorf("%ss: got %v, want one with %q", severity, messages, want)
			}
		})
	}
}

// TestGRIRDatabaseFailures: a load that takes GR/IR positions over, and its
// reconciliation, report each failing statement.
func TestGRIRDatabaseFailures(t *testing.T) {
	e := grirSetup(t, partlyInvoiced())
	h := endpoints(e.dir)
	yes := func(int32) error { return nil }
	c := e.connector(t, "grir.json")
	first := uuid.MustParse(e.run(t, c, "documents", false, e.DE.String())["id"].(string))
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "load", func(ctx context.Context) error {
		_, err := h.StartRun(ctx, first, yes)
		return err
	})
	loaded := e.run(t, c, "documents", false, e.DE.String())
	e.act(t, loaded, "start")
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "reconcile", func(ctx context.Context) error {
		_, err := h.ReconcileRun(ctx, uuid.MustParse(loaded["id"].(string)), yes)
		return err
	})
	// A later run tops item 20's position up by a document SAP posted since.
	later := partlyInvoiced()
	line := sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", RWCUR: "EUR", GJAHR: "2026", BELNR: "0100000040", DOCLN: "000001", RACCT: "0000520000", HSL: "1.00",
		BUDAT: "20260310", BLART: "WE", EBELN: "4500000001", EBELP: "00020"}
	grir := line
	grir.DOCLN, grir.RACCT, grir.HSL = "000002", "0000219000", "-1.00"
	later.ACDOCA = append(later.ACDOCA, line, grir)
	write(t, e.dir, "grir.json", later)
	delta := uuid.MustParse(e.run(t, c, "documents", false, e.DE.String())["id"].(string))
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "top-up", func(ctx context.Context) error {
		_, err := h.StartRun(ctx, delta, yes)
		return err
	})
}

// halfReceived is grirTiny with one of its 2 units received: the receipt
// and its GR/IR line for 50.00, beside an order not received.
func halfReceived() *sap.Extract {
	x := grirTiny()
	x.EKKO = append(x.EKKO, sap.EKKO{EBELN: "4500000002", BUKRS: "2000", LIFNR: "0000000001", WAERS: "EUR", BEDAT: "20260301"})
	x.EKPO = append(x.EKPO, sap.EKPO{EBELN: "4500000002", EBELP: "00010", MATNR: "MAT-1", MENGE: "1", NETPR: "5.00", PEINH: "1", SAKTO: "0000520000"})
	x.MSEG[0].MENGE = "1"
	for i, l := range x.ACDOCA {
		if l.BELNR == "0100000005" {
			x.ACDOCA[i].HSL = strings.Replace(l.HSL, "100.00", "50.00", 1)
		}
	}
	return x
}

// laterReceipt is halfReceived after SAP received the second unit: its
// receipt (MSEG 5000000002) and the receipt's document crediting the GR/IR
// account 50.00, both lines naming the order item.
func laterReceipt() *sap.Extract {
	x := halfReceived()
	x.MSEG = append(x.MSEG, sap.MSEG{MBLNR: "5000000002", MJAHR: "2026", ZEILE: "0001", BWART: "101", EBELN: "4500000001", EBELP: "00010",
		MENGE: "1", BUDAT: "20260306"})
	for _, l := range x.ACDOCA {
		if l.BELNR == "0100000005" {
			l.BELNR, l.BUDAT = "0100000006", "20260306"
			x.ACDOCA = append(x.ACDOCA, l)
		}
	}
	return x
}

// grirReconciled reconciles the run and reports whether the open GR/IR
// check passed, with the run's differences.
func (e env) grirReconciled(t *testing.T, run map[string]any) (bool, string) {
	t.Helper()
	_, got := e.act(t, run, "reconcile")
	report := got["report"].(map[string]any)
	return strings.Contains(checks(report), "2000 Open GR/IR: order items with differences true"), fmt.Sprint(report["diffs"])
}

// TestGRIRTopUp: a later run whose extract carries a receipt SAP posted on
// an order an earlier run loaded tops its migrated position up to SAP's
// (review:batch21), and the open GR/IR check passes; without the top-up it
// fails. Running the extract again changes nothing and publishes nothing,
// and what the platform posts on the item stays beside what was taken over.
func TestGRIRTopUp(t *testing.T) {
	e := grirSetup(t, halfReceived())
	c := e.connector(t, "grir.json")
	first := e.run(t, c, "documents", false, e.DE.String())
	if _, got := e.act(t, first, "start"); got["status"] != "loaded" {
		t.Fatalf("first run: got %v, want loaded: %v", got["status"], got["report"])
	}
	order, number := e.orderOf(t, "4500000001")
	write(t, e.dir, "grir.json", laterReceipt())
	delta := e.run(t, c, "documents", false, e.DE.String())
	_, got := e.act(t, delta, "start")
	if got["status"] != "loaded" {
		t.Fatalf("delta run: got %v, want loaded: %v", got["status"], got["report"])
	}
	if loaded := fmt.Sprint(got["report"].(map[string]any)["loaded"]); loaded != "map[goods_receipts:1 gr_ir_top_ups:1 journal_entries:1]" {
		t.Errorf("loaded by the delta run: got %s, want the receipt, its document and the top-up", loaded)
	}
	if got, want := e.position(t, order, ""), "order "+number+" item 1: received 0, invoiced 0, cleared 0, migrated -2, balance -100.00"; got != want {
		t.Errorf("topped up: got %s, want %s", got, want)
	}

	// Without the top-up (reversed), SAP's receipt loaded and the position
	// not moved, the check fails; the next run tops it up again, and passes.
	var topUp string
	pgtest.Scan(t, `SELECT entity_id::text FROM integration.external_ref WHERE tenant_id = $1 AND object = 'GRIR' AND external_key = $2`,
		[]any{e.Tenant.ID, "4500000001/" + delta["id"].(string)}, &topUp)
	_, tag := e.Must(200, "GET", "/v1/journal-entries/"+topUp, "", "")
	e.Must(200, "POST", "/v1/journal-entries/"+topUp+":reverse", `{}`, tag)
	ok, diffs := e.grirReconciled(t, delta)
	if want := "key:GR/IR of order item 4500000001/00010 platform:-50.00 EUR, quantity -1 sap:-100.00 EUR, quantity -2"; ok || !strings.Contains(diffs, want) {
		t.Errorf("open GR/IR without the top-up: passed %t, differences %s, want %s", ok, diffs, want)
	}
	again := e.run(t, c, "documents", false, e.DE.String())
	if _, got := e.act(t, again, "start"); fmt.Sprint(got["report"].(map[string]any)["loaded"]) != "map[gr_ir_top_ups:1]" {
		t.Errorf("the run after the reversal: got %v, want the top-up alone", got["report"])
	}
	if ok, diffs := e.grirReconciled(t, again); !ok {
		t.Errorf("open GR/IR topped up again: differences %s, want none", diffs)
	}

	// The same extract again: nothing to load, no entry, no event.
	events := count(t, `SELECT count(*) FROM ledger.outbox WHERE tenant_id = $1`, e.Tenant.ID)
	same := e.run(t, c, "documents", false, e.DE.String())
	if _, got := e.act(t, same, "start"); got["status"] != "loaded" || len(got["report"].(map[string]any)["loaded"].(map[string]any)) != 0 {
		t.Errorf("the same extract again: got %v, want nothing loaded", got["report"])
	}
	if n := count(t, `SELECT count(*) FROM ledger.outbox WHERE tenant_id = $1`, e.Tenant.ID); n != events {
		t.Errorf("ledger events after the same extract again: got %d, want %d", n, events)
	}

	// The platform's invoice moves the position; a later run leaves it.
	e.postMigratedInvoice(t)
	after := e.run(t, c, "documents", false, e.DE.String())
	if _, got := e.act(t, after, "start"); len(got["report"].(map[string]any)["loaded"].(map[string]any)) != 0 {
		t.Errorf("a run after the platform's invoice: got %v, want nothing loaded", got["report"])
	}
	if got, want := e.position(t, order, "&open_only=false"), "order "+number+" item 1: received 0, invoiced 2, cleared 0, migrated -2, balance 0.00"; got != want {
		t.Errorf("after the platform's invoice and a later run: got %s, want %s", got, want)
	}
}

// TestGRIRTopUpDirectOrder: an order an earlier run loaded valued directly,
// before SAP received it, is not moved onto GR/IR by a later run (the
// optional tracker leaf): nothing is topped up, and its position stays a
// difference in the reconciliation.
func TestGRIRTopUpDirectOrder(t *testing.T) {
	unreceived := tiny()
	unreceived.MSEG = nil
	e := grirSetup(t, unreceived)
	c := e.connector(t, "grir.json")
	if _, got := e.act(t, e.run(t, c, "documents", false, e.DE.String()), "start"); got["status"] != "loaded" {
		t.Fatalf("first run: got %v, want loaded: %v", got["status"], got["report"])
	}
	write(t, e.dir, "grir.json", grirTiny())
	delta := e.run(t, c, "documents", false, e.DE.String())
	_, got := e.act(t, delta, "start")
	if loaded := fmt.Sprint(got["report"].(map[string]any)["loaded"]); got["status"] != "loaded" || loaded != "map[goods_receipts:1 journal_entries:1]" {
		t.Errorf("delta run: got %v %s, want the receipt and its document, no take-over", got["status"], loaded)
	}
	if ok, diffs := e.grirReconciled(t, delta); ok || !strings.Contains(diffs, "GR/IR of order item 4500000001/00010") {
		t.Errorf("open GR/IR of the direct order: passed %t, differences %s, want the item's", ok, diffs)
	}
}
