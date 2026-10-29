package integration

import (
	"cmp"
	"slices"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// invoiceItems are the extract's supplier invoice items by document
// (BELNR/GJAHR).
func invoiceItems(e *sap.Extract) map[string][]sap.RSEG {
	out := map[string][]sap.RSEG{}
	for _, it := range e.RSEG {
		out[it.BELNR+"/"+it.GJAHR] = append(out[it.BELNR+"/"+it.GJAHR], it)
	}
	return out
}

// documentKind is the kind a parked SAP invoice document loads as (T72b):
// RBKP-XRECH gives the direction ("X" an invoice, "" a credit memo) and
// RSEG-TBTKZ marks subsequent debit/credit items, so a document whose
// items all carry it is a subsequent debit or credit, one with none an
// invoice or credit memo (sap.RBKP, sap.RSEG). It is never guessed: a
// document without the indicator, or with a value other than "X" or "",
// is an error the extract must fix; one that mixes subsequent and ordinary
// items, which a platform document of one kind cannot hold, is not
// migrated (a warning: it is entered as two documents). The kind is ""
// with the issue in either case.
func documentKind(h sap.RBKP, items []sap.RSEG) (string, *Issue) {
	invoice, issue := direction(h)
	if issue != nil {
		return "", issue
	}
	marked, issue := subsequentItems(h, items)
	if issue != nil {
		return "", issue
	}
	switch {
	case marked > 0 && marked < len(items):
		return "", documentIssue(h, SevWarning, "subsequent debit/credit items beside ordinary ones; a document holds one kind, so it is not migrated")
	case marked > 0 && invoice:
		return payables.KindSubsequentDebit, nil
	case marked > 0:
		return payables.KindSubsequentCredit, nil
	case invoice:
		return payables.KindInvoice, nil
	}
	return payables.KindCreditMemo, nil
}

func documentIssue(h sap.RBKP, severity, message string) *Issue {
	return &Issue{Severity: severity, Table: "RBKP", Key: h.BELNR + "/" + h.GJAHR, Message: message}
}

// direction is whether a document invoices (RBKP-XRECH "X") or credits
// (blank); an absent or other indicator is an error, never a guess.
func direction(h sap.RBKP) (bool, *Issue) {
	switch {
	case h.XRECH == nil:
		return false, documentIssue(h, SevError, "XRECH (invoice or credit memo) is missing; the document's kind cannot be told")
	case *h.XRECH != "X" && *h.XRECH != "":
		return false, documentIssue(h, SevError, `XRECH "`+*h.XRECH+`" is neither "X" (invoice) nor blank (credit memo)`)
	}
	return *h.XRECH == "X", nil
}

// subsequentItems counts the document's items RSEG-TBTKZ marks as
// subsequent debit/credit items; a value other than "X" or "" is an error.
func subsequentItems(h sap.RBKP, items []sap.RSEG) (int, *Issue) {
	marked := 0
	for _, it := range items {
		switch it.TBTKZ {
		case "X":
			marked++
		case "":
		default:
			return 0, documentIssue(h, SevError, `item `+it.BUZEI+`: TBTKZ "`+it.TBTKZ+`" is neither "X" (subsequent debit/credit) nor blank`)
		}
	}
	return marked, nil
}

// postedDocument is what a posted SAP invoice document (RBKP RBSTAT 5)
// invoiced or credited on order items, which the load takes over for
// matching (T72b2): its direction, and its ordinary items numbered by
// their place in the document (BUZEI order). Subsequent debit/credit items
// move no quantity and are left out; unlike a parked document, a posted
// one may mix them with ordinary items, as nothing is entered for it.
type postedDocument struct {
	key   string
	kind  string // payables.KindInvoice or payables.KindCreditMemo
	items []postedItem
}

type postedItem struct {
	no int32
	sap.RSEG
}

// postedInvoiced reads a posted document, or the error that refuses it:
// its direction or an item's indicator cannot be told (direction,
// subsequentItems), or an ordinary item's quantity is not a positive
// quantity.
func postedInvoiced(h sap.RBKP, items []sap.RSEG) (postedDocument, *Issue) {
	invoice, issue := direction(h)
	if issue != nil {
		return postedDocument{}, issue
	}
	if _, issue := subsequentItems(h, items); issue != nil {
		return postedDocument{}, issue
	}
	d := postedDocument{key: h.BELNR + "/" + h.GJAHR, kind: payables.KindCreditMemo}
	if invoice {
		d.kind = payables.KindInvoice
	}
	lines := slices.Clone(items)
	slices.SortFunc(lines, func(a, b sap.RSEG) int { return cmp.Compare(a.BUZEI, b.BUZEI) })
	for i, it := range lines {
		if it.TBTKZ == "X" {
			continue
		}
		if err := money.PositiveQuantity(it.MENGE); err != nil {
			return postedDocument{}, documentIssue(h, SevError, "item "+it.BUZEI+": "+err.Error()+"; the quantity it invoiced cannot be taken over")
		}
		d.items = append(d.items, postedItem{no: int32(i + 1), RSEG: it})
	}
	return d, nil
}

// postedDocuments are the extract's posted invoice documents of the
// company ("" every company) in key order, and the issues of those refused
// (postedInvoiced), which are left out.
func postedDocuments(e *sap.Extract, company string) ([]postedDocument, []Issue) {
	items := invoiceItems(e)
	heads := slices.Clone(e.RBKP)
	slices.SortFunc(heads, func(a, b sap.RBKP) int { return cmp.Compare(a.BELNR+"/"+a.GJAHR, b.BELNR+"/"+b.GJAHR) })
	var docs []postedDocument
	var issues []Issue
	for _, h := range heads {
		if h.RBSTAT != "5" || (company != "" && h.BUKRS != company) {
			continue
		}
		d, issue := postedInvoiced(h, items[h.BELNR+"/"+h.GJAHR])
		if issue != nil {
			issues = append(issues, *issue)
			continue
		}
		docs = append(docs, d)
	}
	return docs, issues
}

// parkedKinds are the kinds the parked documents of the extract load as,
// by BELNR/GJAHR: those payables accepts as one kind, without duplicates
// (duplicateInvoices). The load, and reconciliation on the SAP side, take
// the same documents.
func parkedKinds(e *sap.Extract) map[string]string {
	items := invoiceItems(e)
	dups := duplicateInvoices(e)
	out := map[string]string{}
	for _, h := range e.RBKP {
		key := h.BELNR + "/" + h.GJAHR
		if h.RBSTAT != "A" || dups[key] != "" {
			continue
		}
		if kind, issue := documentKind(h, items[key]); issue == nil {
			out[key] = kind
		}
	}
	return out
}
