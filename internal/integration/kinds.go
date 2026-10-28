package integration

import (
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
	key := h.BELNR + "/" + h.GJAHR
	refuse := func(sev, message string) (string, *Issue) {
		return "", &Issue{Severity: sev, Table: "RBKP", Key: key, Message: message}
	}
	if h.XRECH == nil {
		return refuse(SevError, "XRECH (invoice or credit memo) is missing; the document's kind cannot be told")
	}
	if *h.XRECH != "X" && *h.XRECH != "" {
		return refuse(SevError, `XRECH "`+*h.XRECH+`" is neither "X" (invoice) nor blank (credit memo)`)
	}
	marked := 0
	for _, it := range items {
		switch it.TBTKZ {
		case "X":
			marked++
		case "":
		default:
			return refuse(SevError, `item `+it.BUZEI+`: TBTKZ "`+it.TBTKZ+`" is neither "X" (subsequent debit/credit) nor blank`)
		}
	}
	invoice := *h.XRECH == "X"
	switch {
	case marked > 0 && marked < len(items):
		return refuse(SevWarning, "subsequent debit/credit items beside ordinary ones; a document holds one kind, so it is not migrated")
	case marked > 0 && invoice:
		return payables.KindSubsequentDebit, nil
	case marked > 0:
		return payables.KindSubsequentCredit, nil
	case invoice:
		return payables.KindInvoice, nil
	}
	return payables.KindCreditMemo, nil
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
