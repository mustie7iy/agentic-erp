package payables

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// SourceEInvoice is the source of an invoice entered from a received
// e-invoice (ADR-0061 §6).
const SourceEInvoice = "einvoice"

// EInvoice is a received e-invoice's tax invoice as the einvoicing context
// read and validated it (ADR-0061 §5, §6), to enter as a supplier
// invoice: amounts in minor units of Currency, whose digits are Exponent.
// Its bytes are kept already, under ObjectRef with their SHA256, and
// become the invoice's document.
type EInvoice struct {
	LegalEntityID  uuid.UUID
	Number         string    // IBT-001, the supplier reference
	IssueDate      time.Time // IBT-002
	Currency       string    // IBT-005
	Exponent       int
	OrderReference string // IBT-013, "" when none
	SellerTRN      string // IBT-031
	Lines          []EInvoiceLine
	Tax, Gross     money.Minor // IBT-110, IBT-112
	SHA256         []byte
	ObjectRef      string
}

// EInvoiceLine is a line of a received e-invoice: its order line (IBT-132,
// "" when none), description (IBT-154), quantity (IBT-129), net amount
// (IBT-131) and VAT category and rate (IBT-151, IBT-152).
type EInvoiceLine struct {
	OrderLine, Description, Quantity string
	Net                              money.Minor
	Category, Percent                string
}

// EnterEInvoice enters a received tax invoice as a supplier invoice with
// capture's code and no model (ADR-0061 §6): the order from its order
// reference, whose supplier must hold the seller's TRN as a tax
// identifier (compared by fingerprint); its items by order line or by
// position with the order items' units and codes; codes from each line's
// category and rate by capturedTax's rule, an ambiguity holding it; the
// document's bytes its document. It is parked, or on hold with capture's
// reasons, as a captured invoice is, and published as
// SupplierInvoiceCaptured with source einvoice. Any refusal is a problem
// (no order: payables.capture_needs_order), for the caller to hold the
// e-invoice with; the caller runs it in a savepoint, since several are
// raised by the database (supplier_invoice_no_duplicates,
// supplier_invoice_capture_once).
func (e *Endpoints) EnterEInvoice(ctx context.Context, d EInvoice) (Invoice, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Invoice{}, err
	}
	ext, err := extractionOf(d)
	if err != nil {
		return Invoice{}, err
	}
	in, held, err := e.capturedInput(ctx, tx, d.LegalEntityID, ext)
	if err != nil {
		return Invoice{}, err
	}
	if err := e.sellerOf(ctx, in.SupplierID, d.SellerTRN); err != nil {
		return Invoice{}, err
	}
	rates := make([]printedTax, len(d.Lines))
	for i, l := range d.Lines {
		rates[i] = printedTax{rate: l.Percent, category: l.Category}
	}
	// A code proposed for a line is on its item. Its warning has no reader
	// here: only capture's Operation shows warnings.
	_, taxHeld, err := e.capturedTax(ctx, tx, &in, rates)
	if err != nil {
		return Invoice{}, err
	}
	held = strings.TrimSpace(held + " " + taxHeld)
	if held, err = e.capturedExtensions(ctx, &in, held); err != nil {
		return Invoice{}, err
	}
	status, reason, _ := captureStatus("high", held) // read, not extracted: no confidence to doubt, so no warning
	return e.insert(ctx, tx, p, newInvoice{Input: in, source: SourceEInvoice, status: status, holdReason: reason, sha: d.SHA256, objectRef: d.ObjectRef})
}

// extractionOf is the e-invoice as capture's code reads a document: a tax
// invoice, its amounts as decimals of its currency, its lines' order line
// numbers as item numbers. An order line that is no item number is
// refused, as capture refuses a line naming no item of the order.
func extractionOf(d EInvoice) (*capture.Extraction, error) {
	ext := &capture.Extraction{DocumentKind: "invoice", InvoiceNumber: d.Number, InvoiceDate: d.IssueDate.Format(time.DateOnly),
		PurchaseOrder: d.OrderReference, Currency: d.Currency, TaxAmount: d.Tax.Plain(d.Exponent), GrossAmount: d.Gross.Plain(d.Exponent),
		Confidence: "high"}
	for i, l := range d.Lines {
		var no int
		if l.OrderLine != "" {
			n, err := strconv.Atoi(l.OrderLine)
			if err != nil || n <= 0 {
				return nil, unreadable(fmt.Sprintf("line %d names the order line %q, which is no item number", i+1, clipped(l.OrderLine)))
			}
			no = n
		}
		ext.Lines = append(ext.Lines, capture.Line{Description: l.Description, Quantity: l.Quantity, Amount: l.Net.Plain(d.Exponent), POLine: no,
			TaxRate: l.Percent})
	}
	return ext, nil
}

// clipped is at most 40 characters of a value a document gives, for a
// detail.
func clipped(s string) string {
	if r := []rune(s); len(r) > 40 {
		return string(r[:40]) + "…"
	}
	return s
}

// sellerOf refuses an e-invoice whose seller's TRN is not a tax identifier
// of the order's supplier, compared by its keyed fingerprint without
// opening one; the detail names the TRN's last four only.
func (e *Endpoints) sellerOf(ctx context.Context, supplier uuid.UUID, trn string) error {
	ok, err := e.TaxNumbers.HasTaxNumber(ctx, supplier, trn)
	if err != nil {
		return err
	}
	if !ok {
		last := trn
		if len(last) > 4 {
			last = last[len(last)-4:]
		}
		return problem.Unprocessable("payables.einvoice_seller_unknown", "The seller's TRN (IBT-031), ending "+last+
			", is not a tax identifier of the order's supplier; correct the supplier's tax identifiers or the order.")
	}
	return nil
}
