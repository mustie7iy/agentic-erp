package payables

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// MaxDocument is the largest document :capture accepts.
const MaxDocument = 5 << 20

// lowConfidence is the hold reason of documents the extraction is unsure of.
const lowConfidence = "Low extraction confidence; check the invoice against the document."

func unreadable(detail string) error {
	return problem.Unprocessable("payables.unreadable_document", "The document cannot be captured: "+detail+".")
}

// CaptureInvoice extracts an invoice from a document and enters it, as an
// operation that completes within the request (extraction can exceed the
// time budget of a synchronous answer, so the contract makes it an
// operation). The document must print the number of a purchase order of the
// legal entity: supplier, currency and items come from the order; documents
// without one are entered by hand. Low-confidence extractions are put on
// hold. With object storage the document is kept under its hash before the
// extraction, so the invoice refers to what was read; the same bytes
// captured twice (also by a retried request) find the object there already.
func (e *Endpoints) CaptureInvoice(ctx context.Context, legalEntity uuid.UUID, document []byte) (operations.Operation, error) {
	if len(document) > MaxDocument {
		return operations.Operation{}, problem.Unprocessable("payables.document_too_large", "Documents are at most 5 MB in the request; larger ones go through an upload (POST /v1/uploads).")
	}
	return e.capture(ctx, legalEntity, document, nil)
}

// CaptureUpload captures the document of a direct upload (POST /v1/uploads),
// which the capture consumes: used once, the invoice records it.
func (e *Endpoints) CaptureUpload(ctx context.Context, legalEntity, upload uuid.UUID) (operations.Operation, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return operations.Operation{}, err
	}
	document, up, err := e.Uploads.take(ctx, tx, upload)
	if err != nil {
		return operations.Operation{}, err
	}
	return e.capture(ctx, legalEntity, document, &up)
}

// capture extracts and enters a document, from the request or from upload.
func (e *Endpoints) capture(ctx context.Context, legalEntity uuid.UUID, document []byte, upload *Upload) (operations.Operation, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return operations.Operation{}, err
	}
	if e.Extractor == nil {
		return operations.Operation{}, problem.Unavailable("Invoice capture is not configured on this server.", 0)
	}
	sum := sha256.Sum256(document)
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM payables.supplier_invoice WHERE capture_sha256 = $1)`, sum[:]).Scan(&exists); err != nil {
		return operations.Operation{}, fmt.Errorf("payables: check capture of document %x: %w", sum, err)
	}
	if exists {
		return operations.Operation{}, problem.Conflict("payables.already_captured", "This document has already been captured.")
	}
	op, err := operations.Start(ctx, "payables.capture", map[string]any{"legal_entity_id": legalEntity, "sha256": hex.EncodeToString(sum[:])})
	if err != nil {
		return operations.Operation{}, err
	}
	objectRef, err := e.storeDocument(ctx, p.TenantID, sum, document)
	if err != nil {
		return operations.Operation{}, err
	}
	ext, err := e.Extractor.Extract(ctx, document)
	if err != nil {
		return operations.Operation{}, unreadable(err.Error())
	}
	in, held, err := e.capturedInput(ctx, tx, legalEntity, ext)
	if err != nil {
		return operations.Operation{}, err
	}
	status, reason, warnings := captureStatus(ext.Confidence, held)
	v, err := e.insert(ctx, tx, p, newInvoice{Input: in, source: "capture", status: status, holdReason: reason, sha: sum[:], objectRef: objectRef})
	if err != nil {
		return operations.Operation{}, err
	}
	if upload != nil {
		if err := e.Uploads.mark(ctx, tx, *upload, document, v.ID); err != nil {
			return operations.Operation{}, err
		}
	}
	return operations.Succeed(ctx, op.ID, "/v1/supplier-invoices/"+v.ID.String(),
		map[string]any{"supplier_invoice_id": v.ID, "status": v.Status, "warnings": warnings})
}

// storeDocument keeps the document in object storage under its hash and
// returns its key, or "" without object storage.
func (e *Endpoints) storeDocument(ctx context.Context, tenant uuid.UUID, sum [sha256.Size]byte, document []byte) (string, error) {
	if e.Objects == nil {
		return "", nil
	}
	objectRef := blobstore.Key(tenant, blobstore.KindDocuments, hex.EncodeToString(sum[:]))
	err := e.Objects.Put(ctx, objectRef, document, blobstore.PutOptions{ContentType: http.DetectContentType(document)})
	if err != nil && !errors.Is(err, blobstore.ErrExists) { // the same bytes are the same object
		return "", problem.Unavailable("The document store did not take the document; capture it again.", 30).WithCause(err)
	}
	return objectRef, nil
}

// capturedInput turns an extraction into invoice input: supplier and items
// come from the purchase order the document prints, whose currency the
// document must match; the kind is the one read (invoice or credit memo).
// It returns, as the reason to hold the document, a credited invoice that
// cannot be named (creditedInvoice).
func (e *Endpoints) capturedInput(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID, ext *capture.Extraction) (Input, string, error) {
	kind, ok := capturedKinds[ext.DocumentKind]
	if !ok {
		return Input{}, "", unreadable("the document is a " + ext.DocumentKind + ", not an invoice or a credit memo")
	}
	po, order, err := e.capturedOrder(ctx, legalEntity, ext.PurchaseOrder)
	if err != nil {
		return Input{}, "", err
	}
	date, err := time.Parse(time.DateOnly, ext.InvoiceDate)
	if err != nil {
		return Input{}, "", unreadable("the document date " + ext.InvoiceDate + " is not a date")
	}
	if ext.InvoiceNumber == "" {
		return Input{}, "", unreadable("there is no document number")
	}
	if ext.Currency != order.Currency {
		return Input{}, "", unreadable("the document is in " + ext.Currency + " but the order in " + order.Currency)
	}
	exp, err := currency.Exponent(ctx, tx, order.Currency, nil) // the order's currency is in the reference data
	if err != nil {
		return Input{}, "", fmt.Errorf("payables: capture: %w", err)
	}
	a := &printed{exp: exp, currency: order.Currency, credit: kind == KindCreditMemo}
	in := Input{LegalEntityID: legalEntity, SupplierID: order.SupplierID, SupplierReference: ext.InvoiceNumber, InvoiceDate: date, PurchaseOrderID: &po, Kind: kind}
	if in.Gross, err = a.amount(ext.GrossAmount, "gross amount"); err != nil {
		return Input{}, "", err
	}
	if in.Tax, err = a.amount(ext.TaxAmount, "tax amount"); err != nil {
		return Input{}, "", err
	}
	if in.Items, err = capturedItems(ext.Lines, order, a); err != nil {
		return Input{}, "", err
	}
	if a.negative && a.positive {
		return Input{}, "", unreadable("the credit memo prints amounts of both signs")
	}
	held, err := creditedInvoice(ctx, tx, &in, ext.CreditedInvoiceNumber)
	return in, held, err
}

// capturedKinds are the kinds a document is read as. Subsequent debits and
// credits are entered by hand: a document does not say reliably that it
// revalues without quantity.
var capturedKinds = map[string]string{"invoice": KindInvoice, "credit_memo": KindCreditMemo}

// capturedOrder is the purchase order the document prints the number of.
func (e *Endpoints) capturedOrder(ctx context.Context, legalEntity uuid.UUID, number string) (uuid.UUID, procurement.Order, error) {
	if number == "" {
		return uuid.Nil, procurement.Order{}, problem.Unprocessable("payables.capture_needs_order",
			"The document prints no purchase order number; enter invoices without an order by hand.")
	}
	po, ok, err := e.OrderNumbers.OrderByNumber(ctx, legalEntity, number)
	if err != nil {
		return uuid.Nil, procurement.Order{}, err
	}
	if !ok {
		return uuid.Nil, procurement.Order{}, unreadable("the legal entity has no purchase order " + number)
	}
	order, _, err := e.Orders.Order(ctx, po)
	return po, order, err
}

// printed reads a document's amounts in the order's currency as they are
// entered: not negative, the kind giving the direction. A credit memo may
// print its amounts negative, as many do; an invoice printing a negative
// amount, or a credit memo printing both signs, is not read, since which
// amounts credit is then a guess.
type printed struct {
	exp                int
	currency           string
	credit             bool
	negative, positive bool
}

func (p *printed) amount(s, what string) (Amount, error) {
	m, err := money.ParseDecimal(s, p.exp)
	if err != nil {
		return Amount{}, unreadable(what + ": " + err.Error())
	}
	switch {
	case m < 0 && !p.credit:
		return Amount{}, unreadable("the " + what + " is negative on an invoice")
	case m < 0:
		p.negative, m = true, -m
	case m > 0:
		p.positive = true
	}
	return Amount{Value: m.Plain(p.exp), Currency: p.currency}, nil
}

// quantity is a line's quantity as entered: a credit memo's printed
// negative is its magnitude, whether or not the memo prints its amounts
// negative too (both layouts are common, so quantities do not count
// towards the signs of a memo); an invoice's is left for entry to refuse.
func (p *printed) quantity(s string) string {
	if p.credit {
		return strings.TrimPrefix(s, "-")
	}
	return s
}

// creditedInvoice names, as the reference of a credit memo, the posted
// invoice of the supplier whose number the memo prints (supplier
// references are unique per supplier). A memo printing no number corrects
// none, as one entered by hand may; one whose number is not a posted
// invoice of the supplier is held, with the reason returned, for the
// reference to be entered by hand rather than dropped. An invoice printing
// a credited invoice is not read.
func creditedInvoice(ctx context.Context, tx pgx.Tx, in *Input, number string) (string, error) {
	if number == "" {
		return "", nil
	}
	if in.Kind != KindCreditMemo {
		return "", unreadable("an invoice credits no invoice, but the document names " + number)
	}
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM payables.supplier_invoice WHERE supplier_id = $1 AND supplier_reference_norm = platform.normalize_ref($2)
		AND legal_entity_id = $3 AND currency = $4 AND kind = 'invoice' AND status = 'posted'`, in.SupplierID, number, in.LegalEntityID, in.Gross.Currency).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "The credit memo names the invoice " + number + ", which is not a posted invoice of the supplier; enter its reference by hand.", nil
	}
	if err != nil {
		return "", fmt.Errorf("payables: capture: find the credited invoice %s: %w", number, err)
	}
	in.ReferenceInvoiceID = &id
	return "", nil
}

// capturedItems maps the document's lines to items of the order: by the
// order line printed on them, or by position when the counts agree.
func capturedItems(lines []capture.Line, order procurement.Order, p *printed) ([]ItemInput, error) {
	if len(lines) == 0 {
		return nil, unreadable("there are no invoice lines")
	}
	var items []ItemInput
	for i, l := range lines {
		no := int32(l.POLine)
		if no == 0 && len(lines) == len(order.Items) {
			no = order.Items[i].ItemNo // same number of lines: match by position
		}
		unit := ""
		for _, oi := range order.Items {
			if oi.ItemNo == no {
				unit = oi.Unit
			}
		}
		if unit == "" {
			return nil, unreadable("a line names no item of the order")
		}
		a, err := p.amount(l.Amount, "a line amount")
		if err != nil {
			return nil, err
		}
		description := l.Description
		items = append(items, ItemInput{POItemNo: &no, Description: &description, Quantity: p.quantity(l.Quantity), Unit: unit, Amount: a})
	}
	return items, nil
}

// captureStatus parks a captured invoice, or holds it when the extraction
// is unsure or a credited invoice is to be named by hand (held); anything
// below high confidence is a warning.
func captureStatus(confidence, held string) (status string, reason *string, warnings []string) {
	if confidence != "high" {
		warnings = append(warnings, "extraction confidence is "+confidence)
	}
	var reasons []string
	if confidence == "low" {
		reasons = append(reasons, lowConfidence)
	}
	if held != "" {
		reasons = append(reasons, held)
	}
	if len(reasons) == 0 {
		return Parked, nil, warnings
	}
	r := strings.Join(reasons, " ")
	return OnHold, &r, warnings
}

// Document returns the document an invoice was captured from, with its
// content type, from object storage.
func (e *Endpoints) Document(ctx context.Context, id uuid.UUID) ([]byte, string, error) {
	v, err := GetInvoice(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if v.CaptureObjectRef == nil || !blobstore.IsKey(*v.CaptureObjectRef) {
		return nil, "", problem.NotFound("The invoice has no stored document.")
	}
	if e.Objects == nil {
		return nil, "", problem.Unavailable("Object storage is not configured on this server.", 0)
	}
	content, ct, err := e.Objects.Get(ctx, *v.CaptureObjectRef)
	if errors.Is(err, blobstore.ErrNotFound) {
		return nil, "", problem.NotFound("The invoice's document is not in the store.")
	}
	if err != nil {
		return nil, "", problem.Unavailable("The document store did not answer; try again.", 30).WithCause(err)
	}
	if sum := sha256.Sum256(content); v.CaptureSHA256 == nil || !bytes.Equal(sum[:], v.CaptureSHA256) {
		return nil, "", problem.Conflict("payables.document_changed", "The stored document does not match the hash recorded at capture.")
	}
	return content, ct, nil
}
