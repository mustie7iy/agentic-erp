package payables

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// SourceEInvoice is the source of an invoice entered from a received
// e-invoice (ADR-0061 §6).
const SourceEInvoice = "einvoice"

// EInvoice is a received e-invoice's tax invoice or tax credit note as the
// einvoicing context read and validated it (ADR-0061 §5, §6), to enter as
// a supplier invoice or credit memo (Kind: KindInvoice or KindCreditMemo):
// amounts in minor units of Currency, whose digits are Exponent. Its
// bytes are kept already, under ObjectRef with their SHA256, and become
// the invoice's document.
type EInvoice struct {
	LegalEntityID  uuid.UUID
	Kind           string
	Number         string     // IBT-001, the supplier reference
	Cited          []Citation // IBG-03, the invoices a credit note corrects, each number once (their InvoiceID nil)
	Reason         *CreditReason
	IssueDate      time.Time // IBT-002
	Currency       string    // IBT-005
	Exponent       int
	OrderReference string // IBT-013, "" when none
	SellerTRN      string // IBT-031
	Lines          []EInvoiceLine
	Tax, Gross     money.Minor // IBT-110, IBT-112
	// LocalTax is the VAT a document not in the company's currency states
	// in LocalCurrency (AED: the sum of its lines' BTAE-08), kept as its
	// tax's local amount when it posts; nil for a document in AED.
	LocalTax      *money.Minor
	LocalCurrency string
	SHA256        []byte
	ObjectRef     string
}

// EInvoiceLine is a line of a received e-invoice: its order line (IBT-132,
// "" when none), description (IBT-154), quantity (IBT-129), net amount
// (IBT-131) and VAT category and rate (IBT-151, IBT-152).
type EInvoiceLine struct {
	OrderLine, Description, Quantity string
	Net                              money.Minor
	Category, Percent                string
}

// EnterEInvoice enters a received tax invoice as a supplier invoice, or a
// tax credit note as a credit memo, with capture's code and no model
// (ADR-0061 §6): the order from its order reference, whose supplier must
// hold the seller's TRN as a tax identifier (compared by fingerprint); a
// credit memo's reference the posted invoice of the supplier whose number
// it cites, by capture's rule (creditedInvoice: one citing a number that
// is no posted invoice of the supplier is on hold naming it); its items by
// order line or by position with the order items' units and codes; codes
// from each line's category and rate by capturedTax's rule, an ambiguity
// holding it; the document's bytes its document. A credit memo keeps
// its reason (BTAE-03) and every invoice it cites (citedInvoices): one
// citing one names it as its reference, one citing several names none and
// one for a volume discount may cite none (ADR-0061 As built batch 88).
// A credit memo for a volume discount naming no order is entered without
// one (rebateInput, As built batch 89). It is parked, or on hold
// with capture's reasons, as a captured invoice is, and published as
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
	facts, err := statedOf(d)
	if err != nil {
		return Invoice{}, err
	}
	in, held, err := e.enteredInput(ctx, tx, d, ext)
	if err != nil {
		return Invoice{}, err
	}
	if d.Kind == KindCreditMemo {
		var unmatched string
		if facts.citations, unmatched, err = citedInvoices(ctx, tx, &in, d.Cited); err != nil {
			return Invoice{}, err
		}
		held = strings.TrimSpace(held + " " + unmatched)
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
	return e.insert(ctx, tx, p, newInvoice{Input: in, source: SourceEInvoice, status: status, holdReason: reason, sha: d.SHA256, objectRef: d.ObjectRef,
		stated: facts})
}

// extractionOf is the e-invoice as capture's code reads a document: an
// invoice or a credit memo (capture's kinds are payables' own), the
// invoices a memo cites matched by citedInvoices rather than capture's
// single number, its amounts as decimals of its currency, its lines'
// order line numbers as item numbers. An order line that is no item number
// is refused, as capture refuses a line naming no item of the order.
func extractionOf(d EInvoice) (*capture.Extraction, error) {
	ext := &capture.Extraction{DocumentKind: d.Kind, InvoiceNumber: d.Number,
		InvoiceDate: d.IssueDate.Format(time.DateOnly), PurchaseOrder: d.OrderReference, Currency: d.Currency, TaxAmount: d.Tax.Plain(d.Exponent),
		GrossAmount: d.Gross.Plain(d.Exponent), Confidence: "high"}
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
		return problem.Unprocessable("payables.einvoice_seller_unknown", "The seller's TRN (IBT-031), ending "+trnEnding(trn)+
			", is not a tax identifier of the order's supplier; correct the supplier's tax identifiers or the order.")
	}
	return nil
}

// trnEnding is the last four characters of a TRN, all a detail names of it.
func trnEnding(trn string) string {
	if len(trn) > 4 {
		return trn[len(trn)-4:]
	}
	return trn
}

// reasonVolumeDiscount is the credit note reason (BTAE-03) of a volume
// discount, which may name no invoice (IBR-055-AE) and is entered without
// an order when it names none (ADR-0061 As built batch 89). It mirrors
// einvoicing.ReasonVolumeDiscount, which payables cannot import (the
// einvoicing leaf imports no context): change both together.
const reasonVolumeDiscount = "VD"

// rebateKey is the account determination key a volume discount naming no
// order is credited to: the discounts received from suppliers (SAP SKE),
// the account its credit is of the same nature as; the clerk who posts the
// memo may change it while it is parked (ADR-0061 As built batch 89).
const rebateKey = "cash_discount"

// enteredInput is the invoice input of a received e-invoice: a credit memo
// for a volume discount naming no purchase order is entered without one
// (rebateInput), every other document by capture's code from the order its
// order reference names, whose supplier must hold the seller's TRN. It
// returns capture's reason to hold the document, if any.
func (e *Endpoints) enteredInput(ctx context.Context, tx pgx.Tx, d EInvoice, ext *capture.Extraction) (Input, string, error) {
	if d.Kind == KindCreditMemo && d.OrderReference == "" && d.Reason != nil && d.Reason.Code == reasonVolumeDiscount {
		in, err := e.rebateInput(ctx, d, ext)
		return in, "", err
	}
	in, held, err := e.capturedInput(ctx, tx, d.LegalEntityID, ext)
	if err != nil {
		return Input{}, "", err
	}
	return in, held, e.sellerOf(ctx, in.SupplierID, d.SellerTRN)
}

// rebateInput is the input of a volume discount's credit memo naming no
// order (the product owner, 2026-11-21: entered as a credit memo without a
// reference; a credit memo needs no order): the supplier of the company
// holding the seller's TRN (sellerSupplier); its amounts in the document's
// currency as stated; each line an item of quantity 1 EA and the line's
// net on the account of discounts received (rebateKey), as a discount is
// credited by amount and the document's units are not mapped to the ERP's
// (ADR-0061 §6), its tax code by capturedTax's rule from the line's
// category and rate. A company without that account holds the document
// (payables.capture_needs_account).
func (e *Endpoints) rebateInput(ctx context.Context, d EInvoice, ext *capture.Extraction) (Input, error) {
	supplier, err := e.sellerSupplier(ctx, d.LegalEntityID, d.SellerTRN)
	if err != nil {
		return Input{}, err
	}
	account, ok, err := e.Accounts.Determination(ctx, d.LegalEntityID, rebateKey)
	if err != nil {
		return Input{}, err
	}
	if !ok {
		return Input{}, problem.Unprocessable("payables.capture_needs_account", "The volume discount names no purchase order and the company has "+
			"no account for discounts received (account determination "+rebateKey+") to credit it to; set that account in the company's "+
			"account determination, or enter the credit memo by hand.")
	}
	date, err := documentDate(ext)
	if err != nil {
		return Input{}, err
	}
	a := &printed{exp: d.Exponent, currency: d.Currency, credit: true}
	in := Input{LegalEntityID: d.LegalEntityID, SupplierID: supplier, SupplierReference: ext.InvoiceNumber, InvoiceDate: date, Kind: KindCreditMemo}
	if err := a.totals(&in, ext); err != nil {
		return Input{}, err
	}
	if len(ext.Lines) == 0 {
		return Input{}, unreadable("there are no invoice lines")
	}
	for _, l := range ext.Lines {
		amount, err := a.amount(l.Amount, "a line amount")
		if err != nil {
			return Input{}, err
		}
		description := l.Description
		in.Items = append(in.Items, ItemInput{Description: &description, Quantity: "1", Unit: "EA", Amount: amount, AccountID: &account})
	}
	return in, a.oneSign()
}

// sellerSupplier is the one supplier of the company holding the seller's
// TRN as a tax identifier, found by its keyed fingerprint without opening
// one: none is payables.einvoice_seller_unknown, several (whose numbers
// the detail names, for a person to merge or correct)
// payables.einvoice_seller_ambiguous. The details name the TRN's last
// four only.
func (e *Endpoints) sellerSupplier(ctx context.Context, legalEntity uuid.UUID, trn string) (uuid.UUID, error) {
	holders, err := e.TaxNumbers.HoldersOfTaxNumber(ctx, trn)
	if err != nil {
		return uuid.Nil, err
	}
	var ids []uuid.UUID
	var numbers []string
	for _, h := range holders {
		s, ok, err := e.Suppliers.Supplier(ctx, h.ID, legalEntity)
		if err != nil {
			return uuid.Nil, err
		}
		if ok && s.IsSupplier && s.SetUp {
			ids, numbers = append(ids, h.ID), append(numbers, h.Number)
		}
	}
	switch len(ids) {
	case 1:
		return ids[0], nil
	case 0:
		return uuid.Nil, problem.Unprocessable("payables.einvoice_seller_unknown", "The seller's TRN (IBT-031), ending "+trnEnding(trn)+
			", is not a tax identifier of a supplier of the company, and the document names no order; add it to the supplier's tax identifiers.")
	default:
		return uuid.Nil, problem.Unprocessable("payables.einvoice_seller_ambiguous", "The seller's TRN (IBT-031), ending "+trnEnding(trn)+
			", is a tax identifier of several suppliers of the company ("+strings.Join(numbers, ", ")+"), and the document names no order; "+
			"keep it on one of them.")
	}
}
