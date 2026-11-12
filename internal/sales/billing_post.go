package sales

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/receivables"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// ItemBilling is one posted billing of an order item as Orders gives it:
// the billing document, the date it posted on and the amount it billed of
// the item, with the customer invoice it posted as and that invoice's
// journal entry. Revenue recognition reads it (T81d). A billing document
// not yet posted is not among them.
type ItemBilling struct {
	BillingDocumentID uuid.UUID   `json:"billing_document_id"`
	DocumentNumber    string      `json:"document_number"`
	PostingDate       time.Time   `json:"posting_date"`
	Net               money.Minor `json:"net_amount"`
	InvoiceID         uuid.UUID   `json:"customer_invoice_id"`
	EntryID           uuid.UUID   `json:"journal_entry_id"`
}

// itemBillings is the key an order item's posted billings are read under,
// for itemsJSON: by posting date, then by the documents' numbers, which
// are unique in the order's company. The date is written as an instant at
// midnight UTC, as itemDeliveries writes one.
const itemBillings = `, 'billings', coalesce((SELECT json_agg(json_build_object('billing_document_id', b.id, 'document_number', b.document_number,
		'posting_date', to_char(b.posting_date, 'YYYY-MM-DD"T00:00:00Z"'), 'net_amount', i.net_amount,
		'customer_invoice_id', b.customer_invoice_id, 'journal_entry_id', b.journal_entry_id) ORDER BY b.posting_date, b.document_number)
	FROM sales.billing_document_item i JOIN sales.billing_document b ON b.tenant_id = i.tenant_id AND b.id = i.billing_document_id
	WHERE i.tenant_id = t.tenant_id AND i.sales_order_id = t.sales_order_id AND i.order_item_no = t.item_no AND b.status = 'posted'), '[]')`

// PostBilling posts a created billing document, once (ADR-0067 §9, T79b):
// it enters a customer invoice in receivables with the document's items,
// one for one, and posts it, in the request's transaction. The invoice is
// dated the billing date, which is its tax date (ADR-0063 §4), carries
// the order's payment term and names the billing document as its source;
// receivables calculates the output tax and posts Dr the customer's
// reconciliation account, Cr each item's account, Cr output tax, so the
// invoice's debit line is the customer's open item. Revenue is recognized
// as billed. The posting date is the one given, else the company's date
// today in its time zone, as for a customer invoice a person posts.
//
// The document's row is read FOR UPDATE and only a created one posts
// (ADR-0008): a second post waits for the first and then meets another
// version (412) or, at the new version, another status (409
// sales.invalid_status). A refusal by receivables or the ledger leaves
// the document as it was and names the document and its own fields
// (asBilling): what a post can still meet after T79a's checks when the
// document was created is a customer blocked since, an account or a tax
// code that lapsed since, a closed period. The document's new status with the invoice and its entry,
// its audit record and BillingDocumentPosted are written with the invoice
// and the entry, or nothing is.
func (e *Endpoints) PostBilling(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, postingDate *time.Time) (BillingDocument, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return BillingDocument{}, err
	}
	b, err := billing(ctx, tx, id, true)
	if err != nil {
		return BillingDocument{}, err
	}
	if err := ifMatch(b.Version); err != nil {
		return BillingDocument{}, err
	}
	if b.Status != Created {
		return BillingDocument{}, problem.Conflict("sales.invalid_status",
			"Billing document "+b.DocumentNumber+" is "+b.Status+"; only a created one is posted.")
	}
	le, _, err := e.LegalEntities.Entity(ctx, b.LegalEntityID) // a stored document's legal entity exists
	if err != nil {
		return BillingDocument{}, fmt.Errorf("sales: company of billing document %s: %w", id, err)
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{b.CustomerID})
	if err != nil {
		return BillingDocument{}, fmt.Errorf("sales: customer of billing document %s: %w", id, err)
	}
	v, err := e.invoice(ctx, b, postingDate)
	if err != nil {
		return BillingDocument{}, err
	}
	// A posted invoice names its entry, the entry's year and its posting date (customer_invoice_posted).
	entries, err := e.Entries.Numbers(ctx, []uuid.UUID{*v.EntryID})
	if err != nil {
		return BillingDocument{}, fmt.Errorf("sales: journal entry of billing document %s: %w", id, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sales.billing_document SET status = $2, posting_date = $3, customer_invoice_id = $4, customer_invoice_number = $5,
			journal_entry_year = $6, journal_entry_id = $7, version = version + 1, updated_by = $8
		WHERE id = $1`, id, Posted, v.PostingDate, v.ID, v.DocumentNumber, v.EntryYear, v.EntryID, p.ID); err != nil {
		return BillingDocument{}, fmt.Errorf("sales: mark billing document %s posted: %w", id, err)
	}
	posted, err := scanBilling(tx.QueryRow(ctx, billingSelect+` WHERE b.id = $1`, id))
	if err != nil {
		return BillingDocument{}, fmt.Errorf("sales: read billing document %s: %w", id, err)
	}
	return posted, recordBillingPosted(ctx, posted, v, le, names[b.CustomerID].Number, entries[*v.EntryID])
}

// invoice enters the billing document as a customer invoice in
// receivables and posts it (ADR-0067 §9): the document's company,
// customer and currency, the billing date as invoice date, the order's
// payment term, an item per item of the document with its description,
// account, amount and tax code, and the document as its source. An
// amount goes as the decimal string of its minor units, which receivables
// reads back exactly, so the invoice's net amounts are the document's to
// the minor unit. The invoice posts at the version it was entered at: no
// one else can have read it, it being of this transaction. It is returned
// posted, with its entry.
func (e *Endpoints) invoice(ctx context.Context, b BillingDocument, postingDate *time.Time) (receivables.Invoice, error) {
	in := receivables.Input{LegalEntityID: b.LegalEntityID, CustomerID: b.CustomerID, InvoiceDate: b.BillingDate, PostingDate: postingDate,
		Currency: b.Currency, PaymentTermCode: &b.PaymentTermCode, Items: make([]receivables.ItemInput, len(b.Items)),
		Source: &receivables.Source{ID: b.ID, Number: b.DocumentNumber}}
	for i, it := range b.Items {
		in.Items[i] = receivables.ItemInput{Description: &it.Description, AccountID: it.AccountID,
			Net: receivables.Amount{Value: it.Net.Plain(b.Exponent), Currency: b.Currency}, TaxCode: it.TaxCode}
	}
	draft, err := e.Invoices.CreateInvoice(ctx, in)
	if err != nil {
		return receivables.Invoice{}, asBilling(fmt.Errorf("sales: enter billing document %s as a customer invoice: %w", b.DocumentNumber, err), b)
	}
	v, err := e.Invoices.PostInvoice(ctx, draft.ID, func(int32) error { return nil }, nil)
	if err != nil {
		return receivables.Invoice{}, asBilling(fmt.Errorf("sales: post customer invoice %s of billing document %s: %w", draft.DocumentNumber, b.DocumentNumber, err), b)
	}
	return v, nil
}

// asBilling makes a refusal by receivables or the ledger one of the
// billing document's post: its detail leads with the document, and its
// field errors name the document's own fields. The invoice's fields are
// the document's, its items one for one (customer_id, payment_term_code,
// items[i].account_id, items[i].tax_code), so they stay, but for an
// item's centers (itemField); the ledger names the entry's lines, which
// the document does not have, in its fields and its detail ("Line 2"),
// and those become what the lines are to the document (billingLine). The
// code stays the refusing context's, which a consumer branches on. The
// problem is copied, never changed: its maker may keep it. Other errors
// pass unchanged.
func asBilling(err error, b BillingDocument) error {
	var p *problem.Problem
	if !errors.As(err, &p) {
		return err
	}
	refused, detail := *p, p.Detail
	refused.Errors = slices.Clone(p.Errors)
	for k := range refused.Errors {
		f := &refused.Errors[k]
		var line int
		// Sscanf's error says only that the field names no line, which n tells too.
		if n, _ := fmt.Sscanf(f.Field, "lines[%d]", &line); n != 1 {
			f.Field = itemField(f.Field)
			continue
		}
		_, rest, _ := strings.Cut(f.Field, "]") // what of the line was refused: ".account_id", or nothing
		var name string
		f.Field, name = billingLine(line, len(b.Items), rest)
		if rest, ok := strings.CutPrefix(detail, fmt.Sprintf("Line %d", line+1)); ok {
			detail = name + rest
		}
	}
	refused.Detail = "Billing document " + b.DocumentNumber + " cannot be posted as a customer invoice: " + detail
	return &refused
}

// billingLine is what line k of the invoice's entry is to a billing
// document of so many items, as its field and as a detail names it: line
// 0 debits the customer on the reconciliation account, lines 1 to items
// credit the items in their order, the lines after them the output tax
// on the items (receivables.PostInvoice). rest is what of the line was
// refused (".account_id"), which an item has too, but for its centers
// (itemField).
func billingLine(k, items int, rest string) (field, name string) {
	switch {
	case k == 0:
		return "customer_id", "The customer's line"
	case k <= items:
		return itemField(fmt.Sprintf("items[%d]%s", k-1, rest)), fmt.Sprintf("Item %d", k)
	}
	return "items", "The output tax on the items"
}

// itemField is the billing document's field for a field of the invoice:
// the same, but for an item's cost center or profit center, which a
// billing document's item does not carry: its account asked for one.
func itemField(field string) string {
	for _, center := range []string{".cost_center_id", ".profit_center_id"} {
		if item, ok := strings.CutSuffix(field, center); ok && strings.HasPrefix(item, "items[") {
			return item + ".account_id"
		}
	}
	return field
}

// recordBillingPosted adds the audit record of a billing document's
// posting and publishes BillingDocumentPosted in the same transaction
// (ADR-0007, ADR-0011). The event names the customer by id and number,
// never by name (product owner, 2026-11-05), and carries the totals, not
// the items: the document's net value and the tax and gross of the
// customer invoice it posted as, with the invoice and its journal entry.
func recordBillingPosted(ctx context.Context, b BillingDocument, v receivables.Invoice, le tenancy.Entity, customerNumber, entryNumber string) error {
	amount := func(m money.Minor) any { return money.EventAmount(m, b.Currency, b.Exponent) }
	billed, on := b.BillingDate.Format(time.DateOnly), b.PostingDate.Format(time.DateOnly)
	payload := map[string]any{"billing_document_id": b.ID, "document_number": b.DocumentNumber, "legal_entity_id": b.LegalEntityID,
		"legal_entity_code": le.Code, "customer_id": b.CustomerID, "customer_number": customerNumber, "sales_order_id": b.OrderID,
		"sales_order_number": b.OrderNumber, "billing_date": billed, "posting_date": on, "net_amount": amount(b.Net), "tax_amount": amount(v.Tax),
		"gross_amount": amount(v.Gross), "customer_invoice_id": v.ID, "customer_invoice_number": v.DocumentNumber, "journal_entry_id": v.EntryID,
		"journal_entry_year": v.EntryYear, "journal_entry_number": entryNumber, "status": b.Status, "status_before": Created, "version": b.Version}
	detail := map[string]any{"document_number": b.DocumentNumber, "customer_id": b.CustomerID, "sales_order_id": b.OrderID,
		"sales_order_number": b.OrderNumber, "status": b.Status, "status_before": Created, "posting_date": on, "net_amount": amount(b.Net),
		"gross_amount": amount(v.Gross), "customer_invoice_id": v.ID, "customer_invoice_number": v.DocumentNumber, "journal_entry_id": v.EntryID,
		"journal_entry_number": entryNumber}
	return record(ctx, "sales.billing_document.post", "billing_document", "billing_document_posted", b.ID, detail, payload)
}
