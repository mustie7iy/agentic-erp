package core

import (
	"fmt"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// MatchTolerance bounds acceptable price variance per invoice line: the
// larger of PercentOfLine (of the expected line value) and Absolute.
type MatchTolerance struct {
	PercentOfLine int64       `json:"percentOfLine"`
	Absolute      money.Minor `json:"absolute"`
}

var DefaultTolerance = MatchTolerance{PercentOfLine: 2, Absolute: 500}

type IssueKind string

const (
	IssueNoPO             IssueKind = "no_po"
	IssuePONotFound       IssueKind = "po_not_found"
	IssueVendorMismatch   IssueKind = "vendor_mismatch"
	IssueVendorBlocked    IssueKind = "vendor_blocked"
	IssueCurrencyMismatch IssueKind = "currency_mismatch"
	IssueUnknownPOLine    IssueKind = "unknown_po_line"
	IssuePriceVariance    IssueKind = "price_variance"
	IssueQtyOverReceived  IssueKind = "quantity_exceeds_received"
	IssueTotalMismatch    IssueKind = "tax_or_total_mismatch"
	IssueDuplicate        IssueKind = "possible_duplicate"
)

type MatchIssue struct {
	Kind    IssueKind `json:"kind"`
	LineNo  int       `json:"lineNo,omitempty"`
	Message string    `json:"message"`
}

type MatchResult struct {
	InvoiceID string       `json:"invoiceId"`
	Clean     bool         `json:"clean"`
	Issues    []MatchIssue `json:"issues"`
	NetAmount money.Minor  `json:"netAmount"`
}

func (r MatchResult) Summary() string {
	if r.Clean {
		return "clean match"
	}
	msgs := make([]string, len(r.Issues))
	for i, is := range r.Issues {
		msgs[i] = is.Message
	}
	return strings.Join(msgs, "; ")
}

// ThreeWayMatch checks an invoice against its PO and goods receipts. It is
// pure and deterministic; agents explain and resolve the issues it reports
// but never do this arithmetic themselves.
func (s *Store) ThreeWayMatch(inv *SupplierInvoice, tol MatchTolerance) MatchResult {
	res := MatchResult{InvoiceID: inv.ID}
	add := func(kind IssueKind, line int, format string, args ...any) {
		res.Issues = append(res.Issues, MatchIssue{Kind: kind, LineNo: line, Message: fmt.Sprintf(format, args...)})
	}
	for _, l := range inv.Lines {
		res.NetAmount += l.Amount
	}
	if v := s.Vendors[inv.VendorID]; v != nil && v.Blocked {
		add(IssueVendorBlocked, 0, "vendor %s is blocked for posting", v.ID)
	}
	if expected := res.NetAmount + inv.TaxAmount; expected != inv.GrossAmount {
		add(IssueTotalMismatch, 0, "gross %s does not equal lines %s + tax %s", inv.GrossAmount, res.NetAmount, inv.TaxAmount)
	}
	for _, id := range SortedKeys(s.Invoices) {
		other := s.Invoices[id]
		if other.ID != inv.ID && other.VendorID == inv.VendorID && other.Status != InvoiceRejected &&
			normalizeRef(other.VendorReference) == normalizeRef(inv.VendorReference) {
			add(IssueDuplicate, 0, "vendor reference %q already used by invoice %s (%s)", inv.VendorReference, other.ID, other.Status)
		}
	}

	if inv.POID == "" {
		add(IssueNoPO, 0, "invoice has no purchase order")
		return res
	}
	po := s.PurchaseOrders[inv.POID]
	if po == nil {
		add(IssuePONotFound, 0, "purchase order %s not found", inv.POID)
		return res
	}
	if po.VendorID != inv.VendorID {
		add(IssueVendorMismatch, 0, "invoice vendor %s differs from PO vendor %s", inv.VendorID, po.VendorID)
	}
	if po.Currency != inv.Currency {
		add(IssueCurrencyMismatch, 0, "invoice currency %s differs from PO currency %s", inv.Currency, po.Currency)
	}

	received := map[int]money.Qty{}
	for _, g := range s.ReceiptsFor(po.ID) {
		received[g.POLineNo] += g.Quantity
	}
	invoiced := s.invoicedQty(po.ID, inv.ID)
	for _, l := range inv.Lines {
		pl := po.line(l.POLineNo)
		if pl == nil {
			add(IssueUnknownPOLine, l.POLineNo, "line references unknown PO item %d", l.POLineNo)
			continue
		}
		expected := money.Extend(l.Quantity, pl.UnitPrice, pl.PriceUnit)
		allowed := max(tol.Absolute, money.Percent(expected.Abs(), tol.PercentOfLine))
		if diff := l.Amount - expected; diff.Abs() > allowed {
			add(IssuePriceVariance, pl.LineNo, "PO item %d: invoiced %s but PO price gives %s (variance %s, tolerance %s)",
				pl.LineNo, l.Amount, expected, diff, allowed)
		}
		if total := invoiced[pl.LineNo] + l.Quantity; total > received[pl.LineNo] {
			add(IssueQtyOverReceived, pl.LineNo, "PO item %d: invoiced quantity %s exceeds received %s",
				pl.LineNo, total, received[pl.LineNo])
		}
	}
	res.Clean = len(res.Issues) == 0
	return res
}

func (po *PurchaseOrder) line(no int) *POLine {
	for i := range po.Lines {
		if po.Lines[i].LineNo == no {
			return &po.Lines[i]
		}
	}
	return nil
}

// invoicedQty sums quantities already posted against each PO line.
func (s *Store) invoicedQty(poID, excludeID string) map[int]money.Qty {
	out := map[int]money.Qty{}
	for _, inv := range s.Invoices {
		if inv.ID == excludeID || inv.POID != poID || inv.Status != InvoicePosted {
			continue
		}
		for _, l := range inv.Lines {
			out[l.POLineNo] += l.Quantity
		}
	}
	return out
}

func normalizeRef(ref string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(ref) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// PostInvoice books an approved invoice: Dr expense per line, Dr input tax,
// Cr the vendor's AP reconciliation account. Ledger validation still applies,
// so an invoice whose gross does not equal net + tax cannot be posted.
func (s *Store) PostInvoice(invoiceID, inputTaxAccount, postingDate string) (*JournalEntry, error) {
	inv := s.Invoices[invoiceID]
	if inv == nil {
		return nil, fmt.Errorf("invoice %s not found", invoiceID)
	}
	if inv.Status != InvoiceParked && inv.Status != InvoiceOnHold {
		return nil, fmt.Errorf("invoice %s is %s and cannot be posted", invoiceID, inv.Status)
	}
	vendor := s.Vendors[inv.VendorID]
	if vendor == nil {
		return nil, fmt.Errorf("vendor %s not found", inv.VendorID)
	}
	if vendor.Blocked {
		return nil, fmt.Errorf("vendor %s is blocked for posting", vendor.ID)
	}
	apAccount := vendor.APAccount[inv.CompanyCode]
	if apAccount == "" {
		return nil, fmt.Errorf("vendor %s has no AP account in company %s", vendor.ID, inv.CompanyCode)
	}
	var po *PurchaseOrder
	if inv.POID != "" {
		po = s.PurchaseOrders[inv.POID]
	}
	je := &JournalEntry{
		ID:           s.NextID("JE"),
		CompanyCode:  inv.CompanyCode,
		PostingDate:  postingDate,
		DocumentType: "RE",
		Reference:    inv.VendorReference,
		Source:       "native:invoice:" + inv.ID,
	}
	for _, l := range inv.Lines {
		acct := l.GLAccount
		if acct == "" && po != nil {
			if pl := po.line(l.POLineNo); pl != nil {
				acct = pl.GLAccount
			}
		}
		if acct == "" {
			return nil, fmt.Errorf("invoice %s: line for PO item %d has no G/L account", inv.ID, l.POLineNo)
		}
		je.Lines = append(je.Lines, JournalLine{Account: acct, Amount: l.Amount, Text: l.Description})
	}
	if inv.TaxAmount != 0 {
		je.Lines = append(je.Lines, JournalLine{Account: inputTaxAccount, Amount: inv.TaxAmount, Text: "Input tax"})
	}
	je.Lines = append(je.Lines, JournalLine{Account: apAccount, Amount: -inv.GrossAmount, VendorID: vendor.ID})
	if err := s.Post(je); err != nil {
		return nil, err
	}
	inv.Status = InvoicePosted
	inv.JournalEntryID = je.ID
	inv.HoldReason = ""
	return je, nil
}

// HoldInvoice parks an invoice with a reason for human follow-up.
func (s *Store) HoldInvoice(invoiceID, reason string) error {
	inv := s.Invoices[invoiceID]
	if inv == nil {
		return fmt.Errorf("invoice %s not found", invoiceID)
	}
	if inv.Status != InvoiceParked && inv.Status != InvoiceOnHold {
		return fmt.Errorf("invoice %s is %s and cannot be put on hold", invoiceID, inv.Status)
	}
	inv.Status = InvoiceOnHold
	inv.HoldReason = reason
	return nil
}
