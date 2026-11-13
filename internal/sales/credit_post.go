package sales

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/receivables"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// ItemCredit is one posted credit of an order item as Orders gives it:
// the credit memo request, the billing document whose item it credited,
// the date it posted on and the net amount credited, with the customer
// credit memo it posted as and that memo's journal entry (T85d). Net
// revenue reads it (T92a). A request not yet posted is not among them.
type ItemCredit struct {
	RequestID         uuid.UUID   `json:"credit_memo_request_id"`
	DocumentNumber    string      `json:"document_number"`
	BillingDocumentID uuid.UUID   `json:"billing_document_id"`
	PostingDate       time.Time   `json:"posting_date"`
	Net               money.Minor `json:"net_amount"`
	InvoiceID         uuid.UUID   `json:"customer_invoice_id"`
	EntryID           uuid.UUID   `json:"journal_entry_id"`
}

// itemPostedMemos is the key an order item's posted credits are read under,
// for itemsJSON, by posting date, then by the requests' numbers, which
// are unique in the order's company. The date is written as an instant at
// midnight UTC, as itemBillings writes one.
const itemPostedMemos = `, 'credits', coalesce((SELECT json_agg(json_build_object('credit_memo_request_id', r.id, 'document_number', r.document_number,
		'billing_document_id', r.billing_document_id, 'posting_date', to_char(r.posting_date, 'YYYY-MM-DD"T00:00:00Z"'), 'net_amount', c.net_amount,
		'customer_invoice_id', r.customer_invoice_id, 'journal_entry_id', r.journal_entry_id) ORDER BY r.posting_date, r.document_number)
	FROM sales.credit_memo_request_item c
	JOIN sales.credit_memo_request r ON r.tenant_id = c.tenant_id AND r.id = c.request_id
	JOIN sales.billing_document_item b ON b.tenant_id = c.tenant_id AND b.billing_document_id = c.billing_document_id AND b.item_no = c.billing_item_no
	WHERE b.tenant_id = t.tenant_id AND b.sales_order_id = t.sales_order_id AND b.order_item_no = t.item_no AND r.status = 'posted'), '[]')`

// PostCredit posts an approved credit memo request, once (ADR-0067, As
// built T85d): it enters a customer credit memo in receivables naming the
// billing document's invoice, with the request's items one for one, and
// posts it, in the request's transaction (creditMemo). Receivables posts
// Cr the customer's reconciliation account, Dr each item's revenue
// account and output tax: the credit line is the customer's open credit
// item. The credit memo is dated, and posts on, the date given, else the
// company's date today in its time zone.
//
// The request's row is read FOR UPDATE and only an approved one posts
// (ADR-0008): a second post waits for the first and then meets another
// version (412) or, at the new version, another status (409
// sales.invalid_status). The billing document's sales order is then read
// FOR UPDATE, so the credits of one order are counted one post after the
// other (creditTax). A refusal by receivables or the ledger leaves the
// request as it was and names the request and its own fields
// (asPosting). The request's new status with the credit memo and its
// entry, its audit record and CreditMemoRequestPosted are written with the
// credit memo and the entry, or nothing is. Revenue is not touched.
func (e *Endpoints) PostCredit(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, postingDate *time.Time) (CreditMemoRequest, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return CreditMemoRequest{}, err
	}
	r, err := creditRequest(ctx, tx, id, true)
	if err != nil {
		return CreditMemoRequest{}, err
	}
	if err := ifMatch(r.Version); err != nil {
		return CreditMemoRequest{}, err
	}
	if r.Status != Approved {
		return CreditMemoRequest{}, problem.Conflict("sales.invalid_status",
			"Credit memo request "+r.DocumentNumber+" is "+r.Status+"; only an approved one is posted.")
	}
	b, err := billing(ctx, tx, r.BillingDocumentID, false) // the request's billing document is of its company
	if err != nil {
		return CreditMemoRequest{}, err
	}
	if _, err := loadOrder(ctx, tx, b.OrderID, true); err != nil { // a billing document's order exists (billing_document_sales_order_fkey)
		return CreditMemoRequest{}, err
	}
	le, _, err := e.LegalEntities.Entity(ctx, r.LegalEntityID) // a stored document's legal entity exists
	if err != nil {
		return CreditMemoRequest{}, fmt.Errorf("sales: company of credit memo request %s: %w", id, err)
	}
	date, err := e.billingDate(ctx, le.Code, postingDate)
	if err != nil {
		return CreditMemoRequest{}, err
	}
	v, err := e.creditMemo(ctx, tx, r, b, date)
	if err != nil {
		return CreditMemoRequest{}, err
	}
	return e.markCredited(ctx, tx, p, r, b, v, le)
}

// markCredited moves the request to posted with the credit memo v and its
// entry, reads it back and records its posting.
func (e *Endpoints) markCredited(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r CreditMemoRequest, b BillingDocument, v receivables.Invoice,
	le tenancy.Entity) (CreditMemoRequest, error) {
	// A posted credit memo names its entry, the entry's year and its posting date (customer_invoice_posted).
	entries, err := e.Entries.Numbers(ctx, []uuid.UUID{*v.EntryID})
	if err != nil {
		return CreditMemoRequest{}, fmt.Errorf("sales: journal entry of credit memo request %s: %w", r.ID, err)
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{r.CustomerID})
	if err != nil {
		return CreditMemoRequest{}, fmt.Errorf("sales: customer of credit memo request %s: %w", r.ID, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sales.credit_memo_request SET status = $2, posting_date = $3, customer_invoice_id = $4, customer_invoice_number = $5,
			journal_entry_year = $6, journal_entry_id = $7, version = version + 1, updated_by = $8
		WHERE id = $1 AND status = $9`, r.ID, Posted, v.PostingDate, v.ID, v.DocumentNumber, v.EntryYear, v.EntryID, p.ID, Approved); err != nil {
		return CreditMemoRequest{}, fmt.Errorf("sales: mark credit memo request %s posted: %w", r.ID, err)
	}
	posted, err := scanCredit(tx.QueryRow(ctx, requestSelect+` WHERE r.id = $1`, r.ID))
	if err != nil {
		return CreditMemoRequest{}, fmt.Errorf("sales: read credit memo request %s: %w", r.ID, err)
	}
	return posted, recordCreditPosted(ctx, posted, b, v, le, names[r.CustomerID].Number, entries[*v.EntryID])
}

// creditMemo enters the request as a customer credit memo in receivables
// naming the billing document's invoice and posts it (ADR-0067, As built
// T85d): the request's company, customer and currency, the date as its
// date and posting date, no payment term of its own (it keeps the
// invoice's, ADR-0063 §7), an item per item of the request with the
// billing item's description, revenue account and tax code, its net
// amount and its share of the tax the invoice's item bore (creditTax),
// and the request as its source. It posts at the version it was entered
// at: no one else can have read it, it being of this transaction. It is
// returned posted, with its entry.
func (e *Endpoints) creditMemo(ctx context.Context, tx pgx.Tx, r CreditMemoRequest, b BillingDocument, date time.Time) (receivables.Invoice, error) {
	taxes, err := e.creditTax(ctx, tx, r, b)
	if err != nil {
		return receivables.Invoice{}, err
	}
	in := receivables.Input{LegalEntityID: r.LegalEntityID, CustomerID: r.CustomerID, Kind: receivables.KindCreditMemo, ReferenceInvoiceID: b.InvoiceID,
		InvoiceDate: date, PostingDate: &date, Currency: r.Currency, Items: make([]receivables.ItemInput, len(r.Items)),
		Source: &receivables.Source{ID: r.ID, Number: r.DocumentNumber}}
	for i, it := range r.Items {
		in.Items[i] = receivables.ItemInput{Description: &it.Description, AccountID: it.AccountID,
			Net: receivables.Amount{Value: it.Net.Plain(r.Exponent), Currency: r.Currency}, TaxCode: it.TaxCode, Tax: &taxes[i]}
	}
	lead := "Credit memo request " + r.DocumentNumber + " cannot be posted as a customer credit memo: "
	draft, err := e.Invoices.CreateInvoice(ctx, in)
	if err != nil {
		return receivables.Invoice{}, asPosting(fmt.Errorf("sales: enter credit memo request %s as a customer credit memo: %w", r.DocumentNumber, err), lead, len(r.Items))
	}
	v, err := e.Invoices.PostInvoice(ctx, draft.ID, func(int32) error { return nil }, nil)
	if err != nil {
		return receivables.Invoice{}, asPosting(fmt.Errorf("sales: post customer credit memo %s of credit memo request %s: %w", draft.DocumentNumber,
			r.DocumentNumber, err), lead, len(r.Items))
	}
	return v, nil
}

// creditTax is the tax each item of the request gives back, in item
// order: of its billing item, the share of the tax the invoice's item
// bore that the net amounts posted so far, this item's included, are of
// the billing item's amount (share, money.Allocate), less the share of
// what the earlier posted requests credited, so the credits of a billing
// item's whole amount give back exactly the tax it bore (product owner
// 2026-11-12: what was billed is what is credited). The invoice's items
// are the billing document's one for one (T79b). The sales order's row
// lock is held, under which the requests of its billing documents post.
func (e *Endpoints) creditTax(ctx context.Context, tx pgx.Tx, r CreditMemoRequest, b BillingDocument) ([]money.Minor, error) {
	invoice, err := e.Invoices.Invoice(ctx, *b.InvoiceID) // a posted billing document names its invoice (billing_document_posted)
	if err != nil {
		return nil, fmt.Errorf("sales: customer invoice of billing document %s: %w", b.DocumentNumber, err)
	}
	earlier, err := creditedNets(ctx, tx, b.ID)
	if err != nil {
		return nil, err
	}
	out := make([]money.Minor, len(r.Items))
	for i, it := range r.Items {
		billed, before := b.Items[it.BillingItemNo-1], earlier[it.BillingItemNo] // items are numbered from 1 (billable)
		borne, whole := invoice.Items[it.BillingItemNo-1].Tax, big.NewRat(int64(billed.Net), 1)
		out[i] = share(borne, big.NewRat(int64(before+it.Net), 1), whole) - share(borne, big.NewRat(int64(before), 1), whole)
	}
	return out, nil
}

// creditedNets are the net amounts the billing document's posted requests
// credited of each of its items, by the billing item's number.
func creditedNets(ctx context.Context, tx pgx.Tx, billingDocument uuid.UUID) (map[int32]money.Minor, error) {
	rows, _ := tx.Query(ctx, `SELECT c.billing_item_no, sum(c.net_amount)::bigint FROM sales.credit_memo_request_item c
		JOIN sales.credit_memo_request r ON r.tenant_id = c.tenant_id AND r.id = c.request_id
		WHERE c.billing_document_id = $1 AND r.status = $2 GROUP BY c.billing_item_no ORDER BY c.billing_item_no`, billingDocument, Posted)
	out := map[int32]money.Minor{}
	var no int32
	var amount money.Minor
	_, err := pgx.ForEachRow(rows, []any{&no, &amount}, func() error { out[no] = amount; return nil })
	if err != nil {
		return nil, fmt.Errorf("sales: read what is credited of billing document %s: %w", billingDocument, err)
	}
	return out, nil
}

// recordCreditPosted adds the audit record of a request's posting and
// publishes CreditMemoRequestPosted in the same transaction (ADR-0007,
// ADR-0011). The event names the customer by id and number, never by
// name (product owner, 2026-11-05), and carries the totals, positive: the
// request's net value and the credit memo's tax and gross, with the
// credit memo, the invoice it credits and its journal entry.
func recordCreditPosted(ctx context.Context, r CreditMemoRequest, b BillingDocument, v receivables.Invoice, le tenancy.Entity, customerNumber, entryNumber string) error {
	amount := func(m money.Minor) any { return money.EventAmount(m, r.Currency, r.Exponent) }
	on := r.PostingDate.Format(time.DateOnly)
	payload := map[string]any{"credit_memo_request_id": r.ID, "document_number": r.DocumentNumber, "legal_entity_id": r.LegalEntityID,
		"legal_entity_code": le.Code, "customer_id": r.CustomerID, "customer_number": customerNumber, "billing_document_id": r.BillingDocumentID,
		"billing_document_number": r.BillingDocumentNumber, "sales_order_id": r.OrderID, "sales_order_number": r.OrderNumber,
		"returns_order_id": r.ReturnsOrderID, "returns_order_number": r.ReturnsOrderNumber, "posting_date": on, "net_amount": amount(r.Net),
		"tax_amount": amount(v.Tax), "gross_amount": amount(v.Gross), "customer_invoice_id": v.ID, "customer_invoice_number": v.DocumentNumber,
		"reference_invoice_id": b.InvoiceID, "reference_invoice_number": b.InvoiceNumber, "journal_entry_id": v.EntryID,
		"journal_entry_year": v.EntryYear, "journal_entry_number": entryNumber, "status": r.Status, "status_before": Approved, "version": r.Version}
	detail := map[string]any{"document_number": r.DocumentNumber, "customer_id": r.CustomerID, "billing_document_id": r.BillingDocumentID,
		"status": r.Status, "status_before": Approved, "posting_date": on, "net_amount": amount(r.Net), "tax_amount": amount(v.Tax),
		"gross_amount": amount(v.Gross), "customer_invoice_id": v.ID, "customer_invoice_number": v.DocumentNumber, "reference_invoice_id": b.InvoiceID,
		"journal_entry_id": v.EntryID, "journal_entry_number": entryNumber}
	return record(ctx, "sales.credit_memo_request.post", "credit_memo_request", "credit_memo_request_posted", r.ID, detail, payload)
}
