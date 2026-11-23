package payables

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The receiving company's supplier invoice from an intercompany billing
// (S2i3; ADR-0082 As built S2i3): the supplying company's posted billing
// of an intercompany stock transport order's goods issue (S2i2, sales) is
// entered as the receiving company's invoice for the order's supplier, at
// the billing's amounts and tax, its items the order's, so that posting
// it clears the order's GR/IR. It is read as capture reads a document
// (capturedInput, capturedTax), the billing stating what a document
// prints, and lands parked or held for a person to post.

func init() {
	// The handler reads first; the key is the rule for two requests racing
	// for one billing, and answers alike.
	problem.RegisterConstraint("supplier_invoice_billing_once", func() *problem.Problem {
		return problem.Conflict("payables.intercompany_billing_entered", "The intercompany billing is entered already; a billing is entered once.")
	})
}

// IntercompanyBillings reads the supplying companies' intercompany
// billings (sales context, through the composition root, as payables does
// not import sales): one the caller may read, or a 404 problem.
type IntercompanyBillings interface {
	IntercompanyBilling(ctx context.Context, id uuid.UUID) (IntercompanyBilling, error)
}

// IntercompanyBilling is a posted intercompany billing as the receiving
// company's invoice reads it: the supplying company (LegalEntityID) bills
// the receiving company (ReceivingID) the goods issue of the latter's
// order. Amounts are minor units of Currency, whose minor-unit digits are
// Exponent; Net is the items' sum and Tax the invoice's.
type IntercompanyBilling struct {
	ID                         uuid.UUID
	DocumentNumber             string
	LegalEntityID, ReceivingID uuid.UUID
	OrderID                    uuid.UUID
	OrderNumber                string
	BillingDate                time.Time
	Currency                   string
	Exponent                   int
	Net, Tax                   money.Minor
	Items                      []IntercompanyBillingItem
}

// IntercompanyBillingItem is an item of an intercompany billing: the order
// item it bills, what the order calls it, its quantity (in the order
// item's unit), its net amount and the supplying company's output tax code
// it was billed on (nil for none).
type IntercompanyBillingItem struct {
	POItemNo              int32
	Description, Quantity string
	Net                   money.Minor
	TaxCode               *string
}

// EnterIntercompanyBilling enters an intercompany billing as the receiving
// company's supplier invoice, in the request's transaction (S2i3): a
// billing billed to a company the caller may act in (404 otherwise, so
// ids cannot be probed)
// and entered once (409 payables.intercompany_billing_entered, and the
// key supplier_invoice_billing_once for two racing). It is read as a
// captured document is (billingExtraction): the order's supplier and
// items, the billing's number as supplier reference, its date, currency,
// gross and tax; each item's code from its billing item's rate
// (billingRates, capturedTax), a code to choose, a stated tax the codes
// do not bear out, the company's rules or missing custom fields holding
// it. Source edi; it names the billing.
func (e *Endpoints) EnterIntercompanyBilling(ctx context.Context, id uuid.UUID) (Invoice, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Invoice{}, err
	}
	b, err := e.enteredBilling(ctx, tx, id)
	if err != nil {
		return Invoice{}, err
	}
	in, held, err := e.capturedInput(ctx, tx, b.ReceivingID, billingExtraction(b))
	if err != nil {
		return Invoice{}, fmt.Errorf("payables: intercompany billing %s as an invoice: %w", b.DocumentNumber, err)
	}
	rates, unknown, err := e.billingRates(ctx, b, in.Items)
	if err != nil {
		return Invoice{}, err
	}
	_, taxHeld, err := e.capturedTax(ctx, tx, &in, rates)
	if err != nil {
		return Invoice{}, err
	}
	if held, err = e.capturedExtensions(ctx, &in, strings.Join(appendNonEmpty(appendNonEmpty(appendNonEmpty(nil, held), unknown), taxHeld), " ")); err != nil {
		return Invoice{}, err
	}
	status, reason, _ := captureStatus("high", held) // stated by the billing, not extracted: no confidence to doubt
	return e.insert(ctx, tx, p, newInvoice{Input: in, source: "edi", status: status, holdReason: reason, billing: &b.ID})
}

// enteredBilling reads the billing an invoice is entered from and refuses
// one billed to a company the caller may not act in, or entered already as
// an invoice that is not rejected, cancelled or reversed.
func (e *Endpoints) enteredBilling(ctx context.Context, tx pgx.Tx, id uuid.UUID) (IntercompanyBilling, error) {
	b, err := e.Billings.IntercompanyBilling(ctx, id) // one the caller may read, else 404
	if err != nil {
		return IntercompanyBilling{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return IntercompanyBilling{}, fmt.Errorf("payables: company access for intercompany billing %s: %w", b.DocumentNumber, err)
	}
	if !access.Allows(b.ReceivingID) {
		return IntercompanyBilling{}, problem.NotFoundOf("intercompany billing")
	}
	var number string
	switch err := tx.QueryRow(ctx, `SELECT document_number FROM payables.supplier_invoice
		WHERE intercompany_billing_id = $1 AND status NOT IN ('rejected', 'cancelled', 'reversed')`, b.ID).Scan(&number); {
	case err == nil:
		return IntercompanyBilling{}, problem.Conflict("payables.intercompany_billing_entered", "Intercompany billing "+b.DocumentNumber+
			" is entered already, as supplier invoice "+number+"; a billing is entered once.")
	case !errors.Is(err, pgx.ErrNoRows):
		return IntercompanyBilling{}, fmt.Errorf("payables: invoice of intercompany billing %s: %w", b.DocumentNumber, err)
	}
	return b, nil
}

// billingExtraction is the billing as capture's code reads a document: an
// invoice numbered as the billing, of its date, its order and currency,
// its tax and gross amounts, a line per item naming its order item, with
// its quantity and net amount. Rates are billingRates'.
func billingExtraction(b IntercompanyBilling) *capture.Extraction {
	ext := &capture.Extraction{DocumentKind: "invoice", InvoiceNumber: b.DocumentNumber, InvoiceDate: b.BillingDate.Format(time.DateOnly),
		PurchaseOrder: b.OrderNumber, Currency: b.Currency, TaxAmount: b.Tax.Plain(b.Exponent), GrossAmount: (b.Net + b.Tax).Plain(b.Exponent),
		Confidence: "high"} // net and tax are the posted invoice's, whose gross receivables checked: in range
	for _, it := range b.Items {
		ext.Lines = append(ext.Lines, capture.Line{Description: it.Description, Quantity: it.Quantity, Amount: it.Net.Plain(b.Exponent),
			POLine: int(it.POItemNo)})
	}
	return ext
}

// billingRates are the rates the billing's items state, for capturedTax
// to propose the receiving company's input codes from: each the rate its
// output code charges in the supplying company on the billing date, and
// 0% for an item billed without a code. An item billed at 0% (with a code
// or without) whose order item has no code (items, capturedItems')
// states none: nothing is charged and there is no tax to recover, so it
// stays without a code; one whose order item has a code states 0%, so
// that code is kept only where it charges nothing. A code that is
// no longer the supplying company's on that date (its configuration
// changed) is a reason to hold the invoice, returned, for a person to
// choose the item's code.
func (e *Endpoints) billingRates(ctx context.Context, b IntercompanyBilling, items []ItemInput) ([]printedTax, string, error) {
	out := make([]printedTax, len(b.Items))
	var held []string
	for i, it := range b.Items {
		if it.TaxCode == nil {
			if items[i].TaxCode != nil {
				out[i] = printedTax{rate: "0"}
			}
			continue
		}
		code, ok, err := e.Accounts.TaxCode(ctx, b.LegalEntityID, *it.TaxCode, b.BillingDate)
		if err != nil {
			return nil, "", fmt.Errorf("payables: tax code %s of intercompany billing %s: %w", *it.TaxCode, b.DocumentNumber, err)
		}
		if !ok {
			held = append(held, fmt.Sprintf("Line %d is billed on tax code %s, which the supplying company no longer has on %s; choose its tax code.",
				i+1, *it.TaxCode, b.BillingDate.Format(time.DateOnly)))
			continue
		}
		if rate := charged(code); rate.Sign() != 0 || items[i].TaxCode != nil {
			out[i] = printedTax{rate: money.PlainQuantity(rate)} // a rate is a numeric(9,6), a quantity's scale
		}
	}
	return out, strings.Join(held, " "), nil
}
