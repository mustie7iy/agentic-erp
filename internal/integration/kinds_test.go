package integration_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// corrections is tiny with a parked credit memo, subsequent debit and
// subsequent credit of its order item, and a document mixing a subsequent
// debit item with an ordinary one, which is not migrated.
func corrections() *sap.Extract {
	x := tiny()
	head := func(belnr, ref, xrech, gross string) sap.RBKP {
		return sap.RBKP{BELNR: belnr, GJAHR: "2026", BUKRS: "2000", LIFNR: "0000000001", XBLNR: ref, BLDAT: "20260312", WAERS: "EUR",
			RMWWR: gross, WMWST1: "0.00", RBSTAT: "A", XRECH: sap.Indicator(xrech)}
	}
	item := func(belnr, buzei, qty, amount, tbtkz string) sap.RSEG {
		return sap.RSEG{BELNR: belnr, GJAHR: "2026", BUZEI: buzei, EBELN: "4500000001", EBELP: "00010", MENGE: qty, WRBTR: amount, TBTKZ: tbtkz}
	}
	x.RBKP = append(x.RBKP, head("5100000010", "CM-1", "", "50.00"), head("5100000011", "SD-1", "X", "5.00"),
		head("5100000012", "SC-1", "", "3.00"), head("5100000013", "MX-1", "X", "7.00"))
	x.RSEG = append(x.RSEG, item("5100000010", "000001", "1", "50.00", ""), item("5100000011", "000001", "2", "5.00", "X"),
		item("5100000012", "000001", "2", "3.00", "X"), item("5100000013", "000001", "2", "5.00", "X"), item("5100000013", "000002", "1", "2.00", ""))
	return x
}

// TestCorrectionsLoadByKind: a parked SAP credit memo or subsequent
// debit/credit loads as its kind, its amounts signed by it in the events,
// never as a positive invoice; reconciliation compares the kinds (T72b).
func TestCorrectionsLoadByKind(t *testing.T) {
	e := setup(t)

	// An indicator the extract lacks or holds an unknown value of stops the load.
	bad := corrections()
	bad.RBKP[1].XRECH, bad.RBKP[2].XRECH, bad.RSEG[3].TBTKZ = nil, sap.Indicator("Y"), "Y"
	write(t, e.dir, "bad.json", bad)
	_, got := e.act(t, e.run(t, e.connector(t, "bad.json"), "documents", false, e.DE.String()), "start")
	if got["status"] != "failed" {
		t.Fatalf("bad indicators status: got %v, want failed", got["status"])
	}
	quality := fmt.Sprint(got["report"].(map[string]any)["quality"])
	for _, want := range []string{"5100000010/2026 message:XRECH (invoice or credit memo) is missing", `5100000011/2026 message:XRECH "Y"`, `5100000012/2026 message:item 000001: TBTKZ "Y"`} {
		if !strings.Contains(quality, want) {
			t.Errorf("bad indicators quality: got %s, want it to report %q", quality, want)
		}
	}

	write(t, e.dir, "corrections.json", corrections())
	c := e.connector(t, "corrections.json")
	run := e.run(t, c, "documents", false, e.DE.String())
	_, got = e.act(t, run, "start")
	report := got["report"].(map[string]any)
	if got["status"] != "loaded" {
		t.Fatalf("start status: got %v, want loaded: %v", got["status"], report)
	}
	if n := report["loaded"].(map[string]any)["supplier_invoices"]; n != float64(4) {
		t.Errorf("loaded supplier invoices: got %v, want 4 (the mixed document is not migrated)", n)
	}
	if q := fmt.Sprint(report["quality"]); !strings.Contains(q, "5100000013/2026 message:subsequent debit/credit items beside ordinary ones") {
		t.Errorf("quality: got %s, want the mixed document reported", q)
	}

	captured := map[any]string{}
	for _, ev := range apitest.Published(t, e.Tenant, outbox.Event{Context: "payables", Name: "supplier_invoice_captured"}) {
		captured[ev["supplier_invoice_id"]] = fmt.Sprint(ev["gross_amount"])
	}
	for _, want := range []struct{ kind, ref, quantity, gross, event string }{
		{"invoice", "T-1", "2", "100.00", "100.00"},
		{"credit_memo", "CM-1", "1", "50.00", "-50.00"},
		{"subsequent_debit", "SD-1", "0", "5.00", "5.00"},
		{"subsequent_credit", "SC-1", "0", "3.00", "-3.00"},
	} {
		t.Run(want.kind, func(t *testing.T) {
			page, _ := e.Must(200, "GET", fmt.Sprintf("/v1/supplier-invoices?legal_entity_id=%s&kind=%s", e.DE, want.kind), "", "")
			items := page["items"].([]any)
			if len(items) != 1 {
				t.Fatalf("documents of kind %s: got %d, want 1", want.kind, len(items))
			}
			v, _ := e.Must(200, "GET", "/v1/supplier-invoices/"+items[0].(map[string]any)["id"].(string), "", "")
			got := fmt.Sprintf("%v %v %v %v %v", v["supplier_reference"], v["status"], v["source"], v["gross_amount"].(map[string]any)["amount"],
				v["items"].([]any)[0].(map[string]any)["quantity"].(map[string]any)["value"])
			if wantDoc := fmt.Sprintf("%s parked migration %s %s", want.ref, want.gross, want.quantity); got != wantDoc {
				t.Errorf("document: got %s, want %s", got, wantDoc)
			}
			if ev := captured[v["id"]]; !strings.Contains(ev, "amount:"+want.event+" ") {
				t.Errorf("captured event gross: got %s, want amount %s", ev, want.event)
			}
		})
	}

	_, got = e.act(t, run, "reconcile")
	checked := checks(got["report"].(map[string]any))
	if got["status"] != "reconciled" {
		t.Fatalf("reconcile status: got %v, want reconciled: %s", got["status"], checked)
	}
	for _, want := range []string{"2000 Parked supplier invoices true", "2000 Parked supplier credit memos true", "2000 Parked subsequent debits true",
		"2000 Parked subsequent credits true"} {
		if !strings.Contains(checked, want) {
			t.Errorf("reconcile checks: got %s, want %q among them", checked, want)
		}
	}

	// Had the credit memo been an invoice, the kinds would not agree.
	flipped := corrections()
	flipped.RBKP[1].XRECH = sap.Indicator("X")
	write(t, e.dir, "corrections.json", flipped)
	recheck := e.run(t, c, "documents", false, e.DE.String())
	e.act(t, recheck, "start")
	_, got = e.act(t, recheck, "reconcile")
	checked = checks(got["report"].(map[string]any))
	for _, want := range []string{"2000 Parked supplier invoices false", "2000 Parked supplier credit memos false"} {
		if !strings.Contains(checked, want) {
			t.Errorf("reconcile checks of the flipped extract: got %s, want %q among them", checked, want)
		}
	}
}

// postedSAP is corrections with the documents SAP posted on its order item:
// an invoice of 2, a credit memo of 1, a document invoicing 1 beside a
// subsequent debit item, a subsequent credit alone (no quantity, not taken
// over) and an invoice item on an order outside the extract (left out).
func postedSAP() *sap.Extract {
	x := corrections()
	head := func(belnr, xrech string) sap.RBKP {
		return sap.RBKP{BELNR: belnr, GJAHR: "2026", BUKRS: "2000", LIFNR: "0000000001", XBLNR: "P-" + belnr, BLDAT: "20260305", WAERS: "EUR",
			RMWWR: "1.00", WMWST1: "0.00", RBSTAT: "5", XRECH: sap.Indicator(xrech)}
	}
	item := func(belnr, buzei, ebeln, qty, tbtkz string) sap.RSEG {
		return sap.RSEG{BELNR: belnr, GJAHR: "2026", BUZEI: buzei, EBELN: ebeln, EBELP: "00010", MENGE: qty, WRBTR: "1.00", TBTKZ: tbtkz}
	}
	x.RBKP = append(x.RBKP, head("5100000020", "X"), head("5100000021", ""), head("5100000022", "X"), head("5100000023", ""))
	x.RSEG = append(x.RSEG, item("5100000020", "000001", "4500000001", "2", ""), item("5100000020", "000002", "4599999999", "5", ""),
		item("5100000021", "000001", "4500000001", "1", ""),
		item("5100000022", "000001", "4500000001", "2", "X"), item("5100000022", "000002", "4500000001", "1", ""),
		item("5100000023", "000001", "4500000001", "2", "X"))
	return x
}

// TestPostedInvoicesTakenOver: the quantities SAP's posted invoice
// documents invoiced and credited on an order item are taken over at
// migration, once, so the migrated credit memo and subsequent debit and
// credit match clean without a release; reconciliation compares them per
// order item; a posted document whose direction or quantity cannot be told
// stops the load (T72b2).
func TestPostedInvoicesTakenOver(t *testing.T) {
	e := setup(t)

	bad := postedSAP()
	bad.RBKP[5].XRECH, bad.RSEG[8].MENGE = nil, "0"
	write(t, e.dir, "bad.json", bad)
	_, got := e.act(t, e.run(t, e.connector(t, "bad.json"), "documents", false, e.DE.String()), "start")
	if got["status"] != "failed" {
		t.Fatalf("bad posted documents status: got %v, want failed", got["status"])
	}
	quality := fmt.Sprint(got["report"].(map[string]any)["quality"])
	for _, want := range []string{"5100000020/2026 message:XRECH (invoice or credit memo) is missing",
		`5100000021/2026 message:item 000001: quantity "0" is not positive; the quantity it invoiced cannot be taken over`} {
		if !strings.Contains(quality, want) {
			t.Errorf("bad posted documents quality: got %s, want it to report %q", quality, want)
		}
	}

	write(t, e.dir, "posted.json", postedSAP())
	c := e.connector(t, "posted.json")
	run := e.run(t, c, "documents", false, e.DE.String())
	_, got = e.act(t, run, "start")
	report := got["report"].(map[string]any)
	if got["status"] != "loaded" {
		t.Fatalf("start status: got %v, want loaded: %v", got["status"], report)
	}
	if n := report["loaded"].(map[string]any)["posted_invoices_invoiced"]; n != float64(3) {
		t.Errorf("posted documents taken over: got %v, want 3 (the subsequent credit moves no quantity)", n)
	}
	rows := `SELECT count(*) FROM payables.migrated_invoice_item WHERE tenant_id = $1`
	if n := count(t, rows, e.Tenant.ID); n != 3 {
		t.Errorf("migrated items: got %d, want 3 (the item on an order outside the extract is left out)", n)
	}
	_, got = e.act(t, run, "reconcile")
	if checked := checks(got["report"].(map[string]any)); !strings.Contains(checked, "2000 Quantities invoiced in SAP: order items with differences true") {
		t.Errorf("reconcile checks: got %s, want the invoiced quantities agreeing", checked)
	}

	// SAP invoiced 2 + 1 - 1 = 2 of the order item: the migrated
	// corrections match clean, where nothing invoiced held each of them.
	for _, kind := range []string{"credit_memo", "subsequent_debit", "subsequent_credit"} {
		t.Run(kind, func(t *testing.T) {
			page, _ := e.Must(200, "GET", fmt.Sprintf("/v1/supplier-invoices?legal_entity_id=%s&kind=%s", e.DE, kind), "", "")
			v := page["items"].([]any)[0].(map[string]any)
			_, tag := e.Must(200, "GET", "/v1/supplier-invoices/"+v["id"].(string), "", "")
			m, _ := e.Must(200, "POST", "/v1/supplier-invoices/"+v["id"].(string)+":match", "", tag)
			if m["is_clean"] != true {
				t.Errorf("match of the migrated %s: got issues %v, want clean", kind, m["issues"])
			}
		})
	}

	// A second run takes nothing over again.
	again := e.run(t, c, "documents", false, e.DE.String())
	_, got = e.act(t, again, "start")
	if n, ok := got["report"].(map[string]any)["loaded"].(map[string]any)["posted_invoices_invoiced"]; ok {
		t.Errorf("posted documents taken over again: got %v, want none", n)
	}
	if n := count(t, rows, e.Tenant.ID); n != 3 {
		t.Errorf("migrated items after the second run: got %d, want 3", n)
	}

	// Had SAP's credit memo been an invoice, the quantities would differ.
	flipped := postedSAP()
	flipped.RBKP[6].XRECH = sap.Indicator("X")
	write(t, e.dir, "posted.json", flipped)
	recheck := e.run(t, c, "documents", false, e.DE.String())
	e.act(t, recheck, "start")
	_, got = e.act(t, recheck, "reconcile")
	report = got["report"].(map[string]any)
	if checked := checks(report); !strings.Contains(checked, "2000 Quantities invoiced in SAP: order items with differences false") {
		t.Errorf("reconcile checks of the flipped extract: got %s, want the invoiced quantities differing", checked)
	}
	if diffs := fmt.Sprint(report["diffs"]); !strings.Contains(diffs, "key:order item 4500000001/00010 platform:2 sap:4") {
		t.Errorf("reconcile diffs of the flipped extract: got %s, want order item 4500000001/00010 at 4 in SAP and 2 here", diffs)
	}
}

// TestPostedInvoicesRefused: a posted document the reconciliation cannot
// read, as the quality gate could not, fails it; one naming an item its
// order does not have is refused by payables and fails the load (T72b2).
func TestPostedInvoicesRefused(t *testing.T) {
	e := setup(t)
	write(t, e.dir, "posted.json", postedSAP())
	c := e.connector(t, "posted.json")
	run := e.run(t, c, "documents", false, e.DE.String())
	e.act(t, run, "start")
	unknown := postedSAP()
	unknown.RBKP[5].XRECH = nil
	write(t, e.dir, "posted.json", unknown)
	_, got := e.act(t, run, "reconcile")
	if got["status"] == "reconciled" {
		t.Errorf("reconcile status with a posted document of no kind: got reconciled, want it to fail")
	}
	if issues := fmt.Sprint(got["report"].(map[string]any)["issues"]); !strings.Contains(issues, "key:5100000020/2026 message:XRECH (invoice or credit memo) is missing") {
		t.Errorf("reconcile issues: got %s, want the posted document without XRECH", issues)
	}

	other := tiny()
	other.RBKP = append(other.RBKP, sap.RBKP{BELNR: "5100000030", GJAHR: "2026", BUKRS: "2000", LIFNR: "0000000001", XBLNR: "P-30", BLDAT: "20260305",
		WAERS: "EUR", RMWWR: "1.00", WMWST1: "0.00", RBSTAT: "5", XRECH: sap.Indicator("X")})
	other.RSEG = append(other.RSEG, sap.RSEG{BELNR: "5100000030", GJAHR: "2026", BUZEI: "000001", EBELN: "4500000001", EBELP: "00020", MENGE: "1", WRBTR: "1.00"})
	f := setup(t)
	write(t, f.dir, "unordered.json", other)
	_, got = f.act(t, f.run(t, f.connector(t, "unordered.json"), "documents", false, f.DE.String()), "start")
	if got["status"] != "failed" {
		t.Errorf("load of a posted document on an unknown order item: got %v, want failed", got["status"])
	}
	if load := fmt.Sprint(got["report"].(map[string]any)["load"]); !strings.Contains(load, "key:5100000030/2026 message:payables.invalid_purchase_order") {
		t.Errorf("load issues: got %s, want the take-over refused", load)
	}
}

// TestPostedInvoicesOfTwoSystems: two SAP systems of one tenant may both
// have posted a document 5100000020/2026; each is taken over under its
// system (T72b2).
func TestPostedInvoicesOfTwoSystems(t *testing.T) {
	e := setup(t)
	write(t, e.dir, "posted.json", postedSAP())
	for _, system := range []string{"SAP_EU", "SAP_US"} {
		t.Run(system, func(t *testing.T) {
			c, _ := e.Must(201, "POST", "/v1/connectors", fmt.Sprintf(`{"kind": "sap_file", "name": %q, "settings": {"path": "posted.json", "system": %q}}`,
				system, system), "")
			_, got := e.act(t, e.run(t, c["id"].(string), "documents", false, e.DE.String()), "start")
			report := got["report"].(map[string]any)
			if got["status"] != "loaded" {
				t.Fatalf("start status: got %v, want loaded: %v", got["status"], report)
			}
			if n := report["loaded"].(map[string]any)["posted_invoices_invoiced"]; n != float64(3) {
				t.Errorf("posted documents taken over: got %v, want 3", n)
			}
			if n := count(t, `SELECT count(*) FROM payables.migrated_invoice_item WHERE tenant_id = $1 AND reference = $2`, e.Tenant.ID,
				system+" 5100000020/2026"); n != 1 {
				t.Errorf("items of %s 5100000020/2026: got %d, want 1", system, n)
			}
		})
	}
}
