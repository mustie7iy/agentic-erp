package sales

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/receivables"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// BillingCancellation is a row of sales.billing_cancellation (ADR-0067,
// As built T79c): the cancellation of a billing document, a document of
// its own numbered from the billing_document range, its date and reason
// and, for a document that was posted, the customer credit memo that
// reversed its invoice with that credit memo's journal entry (nil for one
// cancelled before it was posted).
type BillingCancellation struct {
	ID             uuid.UUID  `json:"id"`
	DocumentNumber string     `json:"document_number"`
	Date           time.Time  `json:"cancellation_date"`
	Reason         string     `json:"reason"`
	InvoiceID      *uuid.UUID `json:"customer_invoice_id"`
	InvoiceNumber  *string    `json:"customer_invoice_number"`
	EntryYear      *int16     `json:"journal_entry_year"`
	EntryID        *uuid.UUID `json:"journal_entry_id"`
	CreatedAt      time.Time  `json:"created_at"`
	CreatedBy      uuid.UUID  `json:"created_by"`
}

// CancellationInput is a cancellation as a client sends it: its reason
// and, optionally, its date.
type CancellationInput struct {
	Reason string
	Date   *time.Time
}

// cancelling is a cancellation being made: its id and number, its date,
// the document's company and customer's number, and the credit memo that
// reversed a posted document's invoice (nil for a created document).
type cancelling struct {
	id             uuid.UUID
	number         string
	date           time.Time
	le             tenancy.Entity
	customerNumber string
	memo           *receivables.Invoice
	entryNumber    string
}

// CancelBilling cancels a created or posted billing document, once
// (ADR-0067, As built T79c), as SAP's billing cancellation does: a
// cancellation numbered from the company's billing_document range is
// written, dated the date given, else the company's date today. A posted
// document's invoice is reversed in the same transaction by a customer
// credit memo naming it, with its items one for one and the tax they bore
// (reversal); a created one posted nothing. The database then cancels the
// document, takes its quantities off what is billed of its order's items,
// so they are billed again, and moves the order's version (migration
// 000356).
//
// The document's row is read FOR UPDATE, then its order's (ADR-0008),
// the order PostCredit and returns take after their own documents: a
// second cancellation waits and meets another version (412) or the new
// status (409 sales.invalid_status), and a returns order or credit memo
// request for the document, made under the order's lock, is seen here or
// refused there. A document a returns order or a credit memo request that
// is not rejected names is refused (409 sales.billing_document_referenced).
// A refusal by receivables or the ledger leaves everything as it was. The
// audit record and BillingDocumentCancelled are written with the rest.
func (e *Endpoints) CancelBilling(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, in CancellationInput) (BillingDocument, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return BillingDocument{}, err
	}
	if err := problem.RequireText("reason", in.Reason); err != nil {
		return BillingDocument{}, fmt.Errorf("sales: reason of a billing cancellation: %w", err)
	}
	b, err := cancellable(ctx, tx, id, ifMatch)
	if err != nil {
		return BillingDocument{}, err
	}
	c, err := e.cancellation(ctx, b, in.Date)
	if err != nil {
		return BillingDocument{}, err
	}
	if b.Status == Posted {
		if err := e.reversal(ctx, &c, b); err != nil {
			return BillingDocument{}, err
		}
	}
	if err := insertCancellation(ctx, tx, p, b, c, in.Reason); err != nil {
		return BillingDocument{}, err
	}
	cancelled, err := scanBilling(tx.QueryRow(ctx, billingSelect+` WHERE b.id = $1`, id))
	if err != nil {
		return BillingDocument{}, fmt.Errorf("sales: read billing document %s: %w", id, err)
	}
	return cancelled, recordCancellation(ctx, cancelled, b.Status, c)
}

// cancellable reads the document and then its order under their row
// locks, at the version the caller read, and refuses one already
// cancelled or named by a returns order or a credit memo request that is
// not rejected.
func cancellable(ctx context.Context, tx pgx.Tx, id uuid.UUID, ifMatch func(int32) error) (BillingDocument, error) {
	b, err := billing(ctx, tx, id, true)
	if err != nil {
		return BillingDocument{}, err
	}
	if err := ifMatch(b.Version); err != nil {
		return BillingDocument{}, err
	}
	if b.Status == Cancelled {
		return BillingDocument{}, problem.Conflict("sales.invalid_status", "Billing document "+b.DocumentNumber+" is cancelled; it is cancelled once.")
	}
	if _, err := loadOrder(ctx, tx, b.OrderID, true); err != nil { // a billing document's order exists (billing_document_sales_order_fkey)
		return BillingDocument{}, err
	}
	var returns, requests int
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM sales.returns_order WHERE billing_document_id = $1),
			(SELECT count(*) FROM sales.credit_memo_request WHERE billing_document_id = $1 AND status <> $2)`, id, Rejected).Scan(&returns, &requests); err != nil {
		return BillingDocument{}, fmt.Errorf("sales: documents that name billing document %s: %w", id, err)
	}
	if returns+requests > 0 {
		return BillingDocument{}, problem.Conflict("sales.billing_document_referenced", fmt.Sprintf(
			"Billing document %s is named by %d returns order(s) and %d credit memo request(s) not rejected; it is cancelled once none names it.",
			b.DocumentNumber, returns, requests))
	}
	return b, nil
}

// cancellation dates and numbers the document's cancellation: the date
// given, else the company's date today in its time zone, which is not
// before the billing date nor, for a posted document, its posting date
// (422 sales.invalid_cancellation_date naming cancellation_date).
func (e *Endpoints) cancellation(ctx context.Context, b BillingDocument, given *time.Time) (cancelling, error) {
	le, _, err := e.LegalEntities.Entity(ctx, b.LegalEntityID) // a stored document's legal entity exists
	if err != nil {
		return cancelling{}, fmt.Errorf("sales: company of billing document %s: %w", b.ID, err)
	}
	date, err := e.billingDate(ctx, le.Code, given)
	if err != nil {
		return cancelling{}, err
	}
	earliest := b.BillingDate
	if b.PostingDate != nil && b.PostingDate.After(earliest) {
		earliest = *b.PostingDate
	}
	if date.Before(earliest) {
		return cancelling{}, problem.Invalid("sales.invalid_cancellation_date", "cancellation_date", fmt.Sprintf(
			"Billing document %s is cancelled on %s or later, its billing or posting date.", b.DocumentNumber, earliest.Format(time.DateOnly)))
	}
	number, err := e.number(ctx, le.ID, BillingRange, date)
	if err != nil {
		return cancelling{}, err
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{b.CustomerID})
	if err != nil {
		return cancelling{}, fmt.Errorf("sales: customer of billing document %s: %w", b.ID, err)
	}
	return cancelling{id: uuid.Must(uuid.NewV7()), number: number, date: date, le: le, customerNumber: names[b.CustomerID].Number}, nil
}

// reversal enters and posts the customer credit memo that reverses the
// posted document's invoice (ADR-0063 §7): the document's company,
// customer and currency, the cancellation's date as its date and posting
// date, the invoice as the one it credits (it keeps the invoice's payment
// term), the invoice's items one for one, each with the billing item's
// description, account, amount and tax code and the tax and jurisdiction
// the invoice's item bore, so the credit memo's net, tax and gross are the invoice's to
// the minor unit; the cancellation is its source. Receivables posts Cr the
// customer's reconciliation account, Dr each item's account and output
// tax. A refusal names the document and its fields (asPosting).
func (e *Endpoints) reversal(ctx context.Context, c *cancelling, b BillingDocument) error {
	invoice, err := e.Invoices.Invoice(ctx, *b.InvoiceID) // a posted billing document names its invoice (billing_document_posted)
	if err != nil {
		return fmt.Errorf("sales: customer invoice of billing document %s: %w", b.DocumentNumber, err)
	}
	in := receivables.Input{LegalEntityID: b.LegalEntityID, CustomerID: b.CustomerID, Kind: receivables.KindCreditMemo, ReferenceInvoiceID: b.InvoiceID,
		InvoiceDate: c.date, PostingDate: &c.date, Currency: b.Currency, Items: make([]receivables.ItemInput, len(b.Items)),
		Source: &receivables.Source{ID: c.id, Number: c.number}}
	for i, it := range b.Items {
		tax := invoice.Items[i].Tax // the invoice's items are the document's one for one (T79b)
		in.Items[i] = receivables.ItemInput{Description: &it.Description, AccountID: it.AccountID,
			Net: receivables.Amount{Value: it.Net.Plain(b.Exponent), Currency: b.Currency}, TaxCode: it.TaxCode,
			TaxJurisdiction: invoice.Items[i].TaxJurisdiction, Tax: &tax}
	}
	lead := "Billing document " + b.DocumentNumber + " cannot be cancelled by a customer credit memo: "
	draft, err := e.Invoices.CreateInvoice(ctx, in)
	if err != nil {
		return asPosting(fmt.Errorf("sales: enter the cancellation of billing document %s as a customer credit memo: %w", b.DocumentNumber, err), lead, len(b.Items))
	}
	v, err := e.Invoices.PostInvoice(ctx, draft.ID, func(int32) error { return nil }, nil)
	if err != nil {
		return asPosting(fmt.Errorf("sales: post customer credit memo %s cancelling billing document %s: %w", draft.DocumentNumber, b.DocumentNumber, err),
			lead, len(b.Items))
	}
	// A posted credit memo names its entry (customer_invoice_posted).
	entries, err := e.Entries.Numbers(ctx, []uuid.UUID{*v.EntryID})
	if err != nil {
		return fmt.Errorf("sales: journal entry of the cancellation of billing document %s: %w", b.DocumentNumber, err)
	}
	c.memo, c.entryNumber = &v, entries[*v.EntryID]
	return nil
}

// insertCancellation writes the cancellation; its trigger cancels the
// document, takes its quantities off what is billed of the order's items
// and moves the order's version (migration 000356).
func insertCancellation(ctx context.Context, tx pgx.Tx, p reqctx.Principal, b BillingDocument, c cancelling, reason string) error {
	var invoice, entry *uuid.UUID
	var number *string
	var year *int16
	if c.memo != nil {
		invoice, number, year, entry = &c.memo.ID, &c.memo.DocumentNumber, c.memo.EntryYear, c.memo.EntryID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sales.billing_cancellation (tenant_id, id, document_number, legal_entity_id, billing_document_id,
			sales_order_id, cancellation_date, reason, customer_invoice_id, customer_invoice_number, journal_entry_year, journal_entry_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		p.TenantID, c.id, c.number, b.LegalEntityID, b.ID, b.OrderID, c.date, reason, invoice, number, year, entry, p.ID); err != nil {
		return fmt.Errorf("sales: cancel billing document %s: %w", b.ID, err)
	}
	return nil
}

// recordCancellation adds the audit record of a document's cancellation
// and publishes BillingDocumentCancelled in the same transaction
// (ADR-0007, ADR-0011). The event names the customer by id and number,
// never by name (product owner, 2026-11-05), and carries the totals: the
// document's net value and, for a posted one, the credit memo's tax and
// gross, with the credit memo, the invoice it credits and its entry,
// null for a document cancelled before it was posted.
func recordCancellation(ctx context.Context, b BillingDocument, before string, c cancelling) error {
	amount := func(m money.Minor) any { return money.EventAmount(m, b.Currency, b.Exponent) }
	var tax, gross, memo, memoNumber, year, entry, entryNumber, reference, referenceNumber any // null for a document cancelled before it was posted
	if v := c.memo; v != nil {
		tax, gross, memo, memoNumber, year, entry, entryNumber, reference, referenceNumber = amount(v.Tax), amount(v.Gross), v.ID, v.DocumentNumber,
			v.EntryYear, v.EntryID, c.entryNumber, b.InvoiceID, b.InvoiceNumber
	}
	on := c.date.Format(time.DateOnly)
	payload := map[string]any{"billing_document_id": b.ID, "document_number": b.DocumentNumber, "legal_entity_id": b.LegalEntityID,
		"legal_entity_code": c.le.Code, "customer_id": b.CustomerID, "customer_number": c.customerNumber, "sales_order_id": b.OrderID,
		"sales_order_number": b.OrderNumber, "cancellation_id": c.id, "cancellation_number": c.number, "cancellation_date": on,
		"reason": b.Cancellation.Reason, "net_amount": amount(b.Net), "tax_amount": tax, "gross_amount": gross, "customer_invoice_id": memo,
		"customer_invoice_number": memoNumber, "reference_invoice_id": reference, "reference_invoice_number": referenceNumber, "journal_entry_id": entry, "journal_entry_year": year,
		"journal_entry_number": entryNumber, "status": b.Status, "status_before": before, "version": b.Version}
	detail := map[string]any{"document_number": b.DocumentNumber, "customer_id": b.CustomerID, "sales_order_id": b.OrderID, "status": b.Status,
		"status_before": before, "cancellation_id": c.id, "cancellation_number": c.number, "cancellation_date": on, "reason": b.Cancellation.Reason,
		"net_amount": amount(b.Net), "gross_amount": gross, "customer_invoice_id": memo, "journal_entry_number": entryNumber}
	return record(ctx, "sales.billing_document.cancel", "billing_document", "billing_document_cancelled", b.ID, detail, payload)
}
