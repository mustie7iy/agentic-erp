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
	for _, want := range []string{"5100000010/2026 message:XRECH (invoice or credit memo) is missing", `5100000011/2026 message:XRECH "Y"`, `item 000001: TBTKZ "Y"`} {
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
