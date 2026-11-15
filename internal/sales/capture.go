package sales

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Order capture (ADR-0067 As built T90a): a sales representative uploads a
// customer's purchase order document (a direct upload of purpose
// sales_order, ADR-0020) and capture reads it into a draft sales order
// through the model (internal/capture, the cell's model gateway and its
// budget, ADR-0036). The customer is the business partner whose number or
// tax registration number the document prints; each item takes the price
// printed as entered (manual), or else its item key's price record (the
// price list, T80a), as an order created by hand does. Capture never
// confirms: the order is a draft, which a person checks and confirms. The
// document is data: nothing it says is done. One upload gives one order,
// and the same upload captured again answers that order.

// UploadPurpose is the purpose of the uploads order capture takes.
const UploadPurpose = "sales_order"

// Uploads takes the document of a direct upload for the operation that
// consumes it and closes the upload as used by what it gave (payables
// context, ADR-0020).
type Uploads interface {
	Take(ctx context.Context, id uuid.UUID, purpose string) ([]byte, string, error)
	Used(ctx context.Context, id uuid.UUID, content []byte, usedBy uuid.UUID) error
}

// CustomerFinder finds the business partners a document names by number or
// tax registration number (partner context).
type CustomerFinder interface {
	FindCustomers(ctx context.Context, number, taxNumber string) ([]partner.Found, error)
}

// Captured is what order capture read into an order (sales_order.capture):
// the upload, the customer's own order number, how the customer was found
// and per item what the document printed and where its price came from.
type Captured struct {
	UploadID          uuid.UUID      `json:"upload_id"`
	CustomerReference string         `json:"customer_reference"`
	CustomerMatchedBy []string       `json:"customer_matched_by"`
	Confidence        string         `json:"confidence"`
	Items             []CapturedItem `json:"items"`
}

// CapturedItem is an item as the document printed it, numbered as the
// order's item it gave; UnitPrice is "" when none was printed, and
// PriceSource says whether the item took the printed price (manual) or
// the price list's.
type CapturedItem struct {
	ItemNo      int32  `json:"item_no"`
	ItemKey     string `json:"item_key"`
	Description string `json:"description"`
	Quantity    string `json:"quantity"`
	Unit        string `json:"unit"`
	UnitPrice   string `json:"unit_price"`
	PriceSource string `json:"price_source"`
}

// CaptureInput is an order capture as a person asks for it: the company,
// the upload, and the revenue account and output tax code every item of
// the order takes (a document names neither).
type CaptureInput struct {
	LegalEntityID, UploadID, AccountID uuid.UUID
	TaxCode                            *string
}

// CaptureOrder reads the upload's purchase order into a draft sales order,
// as an operation that completes within the request (extraction can take
// longer than a synchronous answer, as an invoice's capture does). The
// refusals are the upload's (payables.upload_*), the model gateway's
// (agents.*), an unreadable document (422 sales.unreadable_document), a
// customer the document does not name or names twice (422
// sales.capture_customer_unknown, sales.capture_customer_ambiguous) and
// what an order created by hand refuses, an item's field naming the
// document's item by its place (items[i]); each creates nothing.
func (e *Endpoints) CaptureOrder(ctx context.Context, in CaptureInput) (operations.Operation, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return operations.Operation{}, err
	}
	if e.OrderExtractor == nil {
		return operations.Operation{}, problem.Unavailable("Order capture is not configured on this server.", 0)
	}
	le, err := e.company(ctx, in.LegalEntityID)
	if err != nil {
		return operations.Operation{}, err
	}
	op, err := operations.Start(ctx, "sales.capture", map[string]any{"legal_entity_id": le.ID, "upload_id": in.UploadID})
	if err != nil {
		return operations.Operation{}, err
	}
	if o, ok, err := capturedBefore(ctx, tx, in.UploadID, le.ID); err != nil || ok {
		if err != nil {
			return operations.Operation{}, err
		}
		return succeeded(ctx, op, o, nil)
	}
	document, contentType, err := e.Uploads.Take(ctx, in.UploadID, UploadPurpose)
	if err != nil {
		return operations.Operation{}, err
	}
	ext, err := e.OrderExtractor.ExtractOrder(ctx, document, contentType)
	if err != nil {
		return operations.Operation{}, unreadable(err)
	}
	order, captured, err := e.capturedOrder(ctx, in, le.LocalCurrency, ext)
	if err != nil {
		return operations.Operation{}, err
	}
	o, err := e.makeOrder(ctx, order, captured)
	if err != nil {
		return operations.Operation{}, fieldsOfCapture(err)
	}
	if err := e.Uploads.Used(ctx, in.UploadID, document, o.ID); err != nil {
		return operations.Operation{}, err
	}
	var warnings []string
	if ext.Confidence != "high" {
		warnings = append(warnings, "extraction confidence is "+ext.Confidence+"; check the order against the document")
	}
	return succeeded(ctx, op, o.ID, warnings)
}

// succeeded completes the capture's operation with the order it names.
func succeeded(ctx context.Context, op operations.Operation, order uuid.UUID, warnings []string) (operations.Operation, error) {
	return operations.Succeed(ctx, op.ID, "/v1/sales-orders/"+order.String(),
		map[string]any{"sales_order_id": order, "status": Draft, "warnings": warnings})
}

// capturedBefore is the order the upload was captured into before, if
// any: the same upload captured again into the same company answers it.
// One captured into another company is refused as a used upload (422
// payables.upload_used), naming no order: the caller may not act there,
// and this company got none.
func capturedBefore(ctx context.Context, tx pgx.Tx, upload, company uuid.UUID) (uuid.UUID, bool, error) {
	var id, le uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id, legal_entity_id FROM sales.sales_order WHERE capture_upload_id = $1`, upload).Scan(&id, &le)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return uuid.Nil, false, nil
	case err != nil:
		return uuid.Nil, false, fmt.Errorf("sales: the order captured from upload %s: %w", upload, err)
	case le != company:
		return uuid.Nil, false, problem.Unprocessable("payables.upload_used", "This upload has already been captured, into another company's order.")
	}
	return id, true, nil
}

// unreadable is the refusal of a document the model did not read: the
// model gateway's refusals as they are (capture.Refusal), anything else
// 422 sales.unreadable_document.
func unreadable(err error) error {
	if refused := capture.Refusal(err); refused != nil {
		return refused
	}
	return problem.Unprocessable("sales.unreadable_document", "The document cannot be captured: "+err.Error()+".")
}

// capturedOrder is the order the extraction gives, in the currency it
// prints or else the company's, and what capture read.
func (e *Endpoints) capturedOrder(ctx context.Context, in CaptureInput, local string, ext *capture.OrderExtraction) (OrderInput, *Captured, error) {
	if len(ext.Items) == 0 {
		return OrderInput{}, nil, problem.Unprocessable("sales.unreadable_document", "The document cannot be captured: it prints no items.")
	}
	customer, matched, err := e.capturedCustomer(ctx, ext)
	if err != nil {
		return OrderInput{}, nil, err
	}
	currency := ext.Currency
	if currency == "" {
		currency = local
	}
	captured := &Captured{UploadID: in.UploadID, CustomerReference: ext.OrderNumber, CustomerMatchedBy: matched, Confidence: ext.Confidence,
		Items: make([]CapturedItem, len(ext.Items))}
	items := make([]ItemInput, len(ext.Items))
	for i, it := range ext.Items {
		items[i] = ItemInput{Description: it.Description, Quantity: it.Quantity, Unit: it.Unit, AccountID: in.AccountID, TaxCode: in.TaxCode}
		source := PriceList
		if it.ItemKey != "" {
			items[i].ItemKey = &it.ItemKey
		}
		if it.UnitPrice != "" {
			items[i].UnitPrice, source = &Amount{Value: it.UnitPrice, Currency: currency}, Manual
		}
		captured.Items[i] = CapturedItem{ItemNo: int32(i + 1), ItemKey: it.ItemKey, Description: it.Description, Quantity: it.Quantity, Unit: it.Unit,
			UnitPrice: it.UnitPrice, PriceSource: source}
	}
	return OrderInput{LegalEntityID: &in.LegalEntityID, CustomerID: &customer, Currency: &currency, Items: items}, captured, nil
}

// capturedCustomer is the one business partner the document names, by the
// customer number or the tax registration number it prints, and which of
// the two found it; none (422 sales.capture_customer_unknown) or more
// than one (422 sales.capture_customer_ambiguous) is refused. Whether it
// may be sold to in the company is the order's check.
func (e *Endpoints) capturedCustomer(ctx context.Context, ext *capture.OrderExtraction) (uuid.UUID, []string, error) {
	number, tax := strings.TrimSpace(ext.CustomerNumber), strings.TrimSpace(ext.CustomerTaxNumber)
	var found []partner.Found
	var matched []string
	for _, by := range []struct{ kind, number, tax string }{{"customer_number", number, ""}, {"tax_number", "", tax}} {
		if by.number == "" && by.tax == "" {
			continue
		}
		f, err := e.Finder.FindCustomers(ctx, by.number, by.tax)
		if err != nil {
			return uuid.Nil, nil, fmt.Errorf("sales: the customer a document names: %w", err)
		}
		if len(f) > 0 {
			matched = append(matched, by.kind)
		}
		for _, c := range f {
			if !containsPartner(found, c.ID) {
				found = append(found, c)
			}
		}
	}
	switch len(found) {
	case 0:
		return uuid.Nil, nil, problem.Unprocessable("sales.capture_customer_unknown", fmt.Sprintf("The document names its customer %q by number %q "+
			"and tax registration number %q, and no business partner has either; enter the order by hand.", ext.CustomerName, number, tax))
	case 1:
		return found[0].ID, matched, nil
	}
	names := make([]string, len(found))
	for i, c := range found {
		names[i] = c.Number + " " + c.Name
	}
	return uuid.Nil, nil, problem.Unprocessable("sales.capture_customer_ambiguous", "The document's customer number and tax registration number name "+
		"more than one business partner ("+strings.Join(names, ", ")+"); enter the order by hand for the right one.")
}

func containsPartner(found []partner.Found, id uuid.UUID) bool {
	for _, f := range found {
		if f.ID == id {
			return true
		}
	}
	return false
}

// capturedItemField matches an order item's account or tax code field, which a
// capture request gives once for every item.
var capturedItemField = regexp.MustCompile(`^items\[\d+\]\.(account_id|tax_code)$`)

// fieldsOfCapture names, in a refusal of the captured order, the capture
// request's account_id and tax_code where the order's item fields stand
// for them; an item's other fields name the document's item by its place.
func fieldsOfCapture(err error) error {
	var p *problem.Problem
	if !errors.As(err, &p) {
		return err
	}
	for i, f := range p.Errors {
		if m := capturedItemField.FindStringSubmatch(f.Field); m != nil {
			p.Errors[i].Field = m[1]
		}
	}
	return err
}
