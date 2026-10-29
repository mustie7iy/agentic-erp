package integration

import (
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// TestDocumentKind: RBKP-XRECH and RSEG-TBTKZ give a parked document's
// kind, and anything else is refused rather than guessed (T72b).
func TestDocumentKind(t *testing.T) {
	item := func(tbtkz string) sap.RSEG { return sap.RSEG{BUZEI: "000001", TBTKZ: tbtkz} }
	cases := []struct {
		name     string
		xrech    *string
		items    []sap.RSEG
		kind     string
		severity string
	}{
		{"invoice", sap.Indicator("X"), []sap.RSEG{item("")}, "invoice", ""},
		{"invoice without items", sap.Indicator("X"), nil, "invoice", ""},
		{"credit memo", sap.Indicator(""), []sap.RSEG{item(""), item("")}, "credit_memo", ""},
		{"subsequent debit", sap.Indicator("X"), []sap.RSEG{item("X"), item("X")}, "subsequent_debit", ""},
		{"subsequent credit", sap.Indicator(""), []sap.RSEG{item("X")}, "subsequent_credit", ""},
		{"mixed items", sap.Indicator("X"), []sap.RSEG{item("X"), item("")}, "", SevWarning},
		{"no XRECH", nil, []sap.RSEG{item("")}, "", SevError},
		{"unknown XRECH", sap.Indicator("."), []sap.RSEG{item("")}, "", SevError},
		{"unknown TBTKZ", sap.Indicator("X"), []sap.RSEG{item("Y")}, "", SevError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, issue := documentKind(sap.RBKP{BELNR: "5100000001", GJAHR: "2026", XRECH: c.xrech}, c.items)
			if kind != c.kind {
				t.Errorf("kind: got %q, want %q", kind, c.kind)
			}
			severity := ""
			if issue != nil {
				severity = issue.Severity
				if issue.Table != "RBKP" || issue.Key != "5100000001/2026" {
					t.Errorf("issue row: got %s %s, want RBKP 5100000001/2026", issue.Table, issue.Key)
				}
			}
			if severity != c.severity {
				t.Errorf("issue severity: got %q, want %q", severity, c.severity)
			}
		})
	}
}

// TestPostedInvoiced: a posted document takes over its ordinary items'
// quantities by its direction, numbered by their place in the document,
// and is refused when its direction, an indicator or a quantity cannot be
// told (T72b2).
func TestPostedInvoiced(t *testing.T) {
	item := func(buzei, qty, tbtkz string) sap.RSEG {
		return sap.RSEG{BUZEI: buzei, EBELN: "4500000001", EBELP: "00010", MENGE: qty, TBTKZ: tbtkz}
	}
	cases := []struct {
		name  string
		xrech *string
		items []sap.RSEG
		want  string
	}{
		{"invoice", sap.Indicator("X"), []sap.RSEG{item("000001", "2", "")}, "invoice [1:2]"},
		{"credit memo", sap.Indicator(""), []sap.RSEG{item("000002", "1.5", ""), item("000001", "1", "")}, "credit_memo [1:1 2:1.5]"},
		{"beside a subsequent debit item", sap.Indicator("X"), []sap.RSEG{item("000001", "2", "X"), item("000002", "1", "")}, "invoice [2:1]"},
		{"a subsequent credit alone", sap.Indicator(""), []sap.RSEG{item("000001", "2", "X")}, "credit_memo []"},
		{"no XRECH", nil, []sap.RSEG{item("000001", "2", "")}, "XRECH (invoice or credit memo) is missing; the document's kind cannot be told"},
		{"unknown TBTKZ", sap.Indicator("X"), []sap.RSEG{item("000001", "2", "Y")}, `item 000001: TBTKZ "Y" is neither "X" (subsequent debit/credit) nor blank`},
		{"no quantity", sap.Indicator("X"), []sap.RSEG{item("000001", "", "")}, `item 000001: quantity "" is not a plain decimal; the quantity it invoiced cannot be taken over`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, issue := postedInvoiced(sap.RBKP{BELNR: "5100000001", GJAHR: "2026", XRECH: c.xrech}, c.items)
			got := ""
			if issue != nil {
				got = issue.Message
				if issue.Severity != SevError || issue.Key != "5100000001/2026" {
					t.Errorf("issue: got %s %s, want an error of 5100000001/2026", issue.Severity, issue.Key)
				}
			} else {
				var items []string
				for _, it := range d.items {
					items = append(items, fmt.Sprintf("%d:%s", it.no, it.MENGE))
				}
				got = fmt.Sprintf("%s %v", d.kind, items)
			}
			if got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}

// TestMigratedInvoiceWaits: a posted document is taken over once every
// order of the extract it names is loaded, without its items on orders
// outside the extract (T72b2).
func TestMigratedInvoiceWaits(t *testing.T) {
	loaded := uuid.New()
	l := &loader{refs: refs{"EKKO/4500000001": loaded}}
	inExtract := map[string]bool{"4500000001": true, "4500000002": true}
	nos := map[string]int32{"4500000001/00010": 1}
	doc := func(orders ...string) postedDocument {
		d := postedDocument{key: "5100000001/2026", kind: "invoice"}
		for i, o := range orders {
			d.items = append(d.items, postedItem{no: int32(i + 1), RSEG: sap.RSEG{EBELN: o, EBELP: "00010", MENGE: "1"}})
		}
		return d
	}
	in, ready := l.migratedInvoice(doc("4500000001", "4599999999"), nos, inExtract)
	if !ready {
		t.Error("document on a loaded order: got waiting, want ready")
	}
	if in.Reference != "SAP 5100000001/2026" {
		t.Errorf("reference: got %s, want SAP 5100000001/2026", in.Reference)
	}
	want := []payables.MigratedItem{{ItemNo: 1, PurchaseOrderID: loaded, POItemNo: 1, Quantity: "1"}}
	if !slices.Equal(in.Items, want) {
		t.Errorf("items: got %+v, want %+v (the item outside the extract left out)", in.Items, want)
	}
	if _, ready := l.migratedInvoice(doc("4500000001", "4500000002"), nos, inExtract); ready {
		t.Error("document on an order not loaded yet: got ready, want it to wait")
	}
}
