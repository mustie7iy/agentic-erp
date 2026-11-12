package receivables

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Amount is an API amount: a decimal string and its currency.
type Amount struct{ Value, Currency string }

// ItemInput is an invoice item as a client sends it.
type ItemInput struct {
	Description                  *string
	AccountID                    uuid.UUID
	Net                          Amount
	TaxCode                      *string
	CostCenterID, ProfitCenterID *uuid.UUID
}

// Input is a customer invoice as a client sends it.
type Input struct {
	LegalEntityID uuid.UUID
	CustomerID    uuid.UUID
	InvoiceDate   time.Time
	PostingDate   *time.Time
	Currency      string
	// PaymentTermCode is the payment term the invoice is to post with; nil:
	// the customer's in the company when it posts.
	PaymentTermCode *string
	Items           []ItemInput
	// Source is the document of another context the invoice is entered
	// from (a billing document of sales, T79b); nil for an invoice a person
	// enters.
	Source *Source
}

// Source names the document an invoice came from, by id and by the number
// its own context gave it (ADR-0063 §2; ADR-0067 §9). A context that
// enters invoices in process sets it; the API's request cannot.
type Source struct {
	ID     uuid.UUID
	Number string
}

// columns are the source as the invoice's row keeps it: both, or neither
// for an invoice without one (customer_invoice_source).
func (s *Source) columns() (*uuid.UUID, *string) {
	if s == nil {
		return nil, nil
	}
	return &s.ID, &s.Number
}

// checked is an input that passed validation: its company, its customer
// as the partner context names it, and its items with their tax.
type checked struct {
	le              tenancy.Entity
	customer        partner.Named
	exp             int
	items           []Item
	net, tax, gross money.Minor
}

// errTooLarge refuses an invoice whose amounts are out of range.
func errTooLarge() *problem.Problem {
	return problem.Unprocessable("receivables.amount_too_large", "The invoice's amounts, or the tax on them, are too large.")
}

// CreateInvoice enters a customer invoice as a draft, numbered from the
// company's customer_invoice range of the invoice date's fiscal year, and
// publishes CustomerInvoiceChanged with its audit record. Nothing is
// posted. An invoice another context enters in process names the
// document it came from (Input.Source), once per document
// (customer_invoice_source_once).
func (e *Endpoints) CreateInvoice(ctx context.Context, in Input) (Invoice, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Invoice{}, err
	}
	c, err := e.validate(ctx, tx, in)
	if err != nil {
		return Invoice{}, err
	}
	year, err := e.Calendar.FiscalYear(ctx, in.LegalEntityID, in.InvoiceDate)
	if err != nil {
		return Invoice{}, fmt.Errorf("receivables: fiscal year of %s: %w", in.InvoiceDate.Format(time.DateOnly), err)
	}
	number, err := e.Numbers.Next(ctx, in.LegalEntityID, NumberRange, year)
	if err != nil {
		return Invoice{}, fmt.Errorf("receivables: number a customer invoice: %w", err)
	}
	id := uuid.Must(uuid.NewV7())
	sourceID, sourceNumber := in.Source.columns()
	if _, err := tx.Exec(ctx, `INSERT INTO receivables.customer_invoice (tenant_id, id, document_number, legal_entity_id, customer_id, kind, invoice_date,
			posting_date, currency, net_amount, tax_amount, gross_amount, status, created_by, updated_by, payment_term_code,
			source_document_id, source_document_number)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $14, $15, $16, $17)`,
		p.TenantID, id, number, in.LegalEntityID, in.CustomerID, KindInvoice, in.InvoiceDate, in.PostingDate, in.Currency, c.net, c.tax, c.gross,
		Draft, p.ID, in.PaymentTermCode, sourceID, sourceNumber); err != nil {
		return Invoice{}, fmt.Errorf("receivables: insert customer invoice %s: %w", id, err)
	}
	if err := insertItems(ctx, tx, p.TenantID, id, c.items); err != nil {
		return Invoice{}, err
	}
	v, err := load(ctx, tx, id, false)
	if err != nil {
		return Invoice{}, err
	}
	return v, recordEntry(ctx, v, c)
}

// insertItems writes a new invoice's items.
func insertItems(ctx context.Context, tx pgx.Tx, tenant, invoice uuid.UUID, items []Item) error {
	n := len(items)
	nos, descriptions, accounts, nets := make([]int32, n), make([]*string, n), make([]uuid.UUID, n), make([]money.Minor, n)
	codes, taxes, costCenters, profitCenters := make([]*string, n), make([]money.Minor, n), make([]*uuid.UUID, n), make([]*uuid.UUID, n)
	for i, it := range items {
		nos[i], descriptions[i], accounts[i], nets[i] = it.ItemNo, it.Description, it.AccountID, it.Net
		codes[i], taxes[i], costCenters[i], profitCenters[i] = it.TaxCode, it.Tax, it.CostCenterID, it.ProfitCenterID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO receivables.customer_invoice_item (tenant_id, customer_invoice_id, item_no, description, account_id,
			net_amount, tax_code, tax_amount, cost_center_id, profit_center_id)
		SELECT $1, $2, n, d, ac, na, tc, ta, cc, pc
		FROM unnest($3::int[], $4::text[], $5::uuid[], $6::bigint[], $7::text[], $8::bigint[], $9::uuid[], $10::uuid[])
			AS x(n, d, ac, na, tc, ta, cc, pc)`,
		tenant, invoice, nos, descriptions, accounts, nets, codes, taxes, costCenters, profitCenters); err != nil {
		return fmt.Errorf("receivables: write items of customer invoice %s: %w", invoice, err)
	}
	return nil
}

// recordEntry adds the audit record of an invoice's entry and publishes
// CustomerInvoiceChanged in the same transaction (ADR-0007, ADR-0011):
// its status before is null, as it had none. The event names the customer
// by id and number, never by name (product owner, 2026-11-05), and
// carries the totals, not the items.
func recordEntry(ctx context.Context, v Invoice, c checked) error {
	const action = "receivables.customer_invoice.create"
	payload := map[string]any{"customer_invoice_id": v.ID, "document_number": v.DocumentNumber, "legal_entity_id": v.LegalEntityID,
		"legal_entity_code": c.le.Code, "customer_id": v.CustomerID, "customer_number": c.customer.Number, "kind": v.Kind,
		"invoice_date": v.InvoiceDate.Format(time.DateOnly), "net_amount": money.EventAmount(v.Net, v.Currency, v.Exponent),
		"tax_amount": money.EventAmount(v.Tax, v.Currency, v.Exponent), "gross_amount": money.EventAmount(v.Gross, v.Currency, v.Exponent),
		"status": v.Status, "status_before": nil, "version": v.Version}
	detail := map[string]any{"document_number": v.DocumentNumber, "customer_id": v.CustomerID, "status": v.Status,
		"gross_amount": money.EventAmount(v.Gross, v.Currency, v.Exponent), "items": len(v.Items)}
	if err := audit.Record(ctx, action, "receivables.customer_invoice", v.ID, detail,
		outbox.Event{Context: "receivables", Name: "customer_invoice_changed", AggregateType: "receivables.customer_invoice", AggregateID: v.ID,
			Payload: payload}); err != nil {
		return fmt.Errorf("receivables: record %s of customer invoice %s: %w", action, v.ID, err)
	}
	return nil
}

// validate checks an invoice for entry: a legal entity the caller may act
// in, a customer that may be invoiced there, a known currency, at least
// one item, each a positive net amount in that currency on a postable
// account with the centers the ledger asks for, and output tax codes in
// force on the invoice date, whose tax it calculates; the totals add up.
func (e *Endpoints) validate(ctx context.Context, tx pgx.Tx, in Input) (checked, error) {
	var c checked
	var err error
	if c.le, c.customer, err = e.parties(ctx, in); err != nil {
		return checked{}, err
	}
	if c.exp, err = currency.Exponent(ctx, tx, in.Currency, problem.Invalid("receivables.unknown_currency", "currency", "The currency is not known.")); err != nil {
		return checked{}, fmt.Errorf("receivables: currency %s: %w", in.Currency, err)
	}
	if err := e.knownTerm(ctx, in.PaymentTermCode); err != nil {
		return checked{}, err
	}
	if len(in.Items) == 0 {
		return checked{}, problem.Invalid("receivables.items_required", "items", "A customer invoice has at least one item.")
	}
	for i, it := range in.Items {
		item, err := e.item(ctx, c.le, in.Currency, c.exp, i, it)
		if err != nil {
			return checked{}, err
		}
		c.items = append(c.items, item)
	}
	if err := e.assignments(ctx, in, c); err != nil {
		return checked{}, err
	}
	if err := e.outputTax(ctx, c.le, in.Currency, in.InvoiceDate, c.items); err != nil {
		return checked{}, err
	}
	return c, c.total()
}

// knownTerm refuses a payment term entered that is not configured (422
// receivables.unknown_payment_term naming payment_term_code); nil: none
// entered, the customer's applies when the invoice posts.
func (e *Endpoints) knownTerm(ctx context.Context, code *string) error {
	if code == nil {
		return nil
	}
	_, ok, err := e.Terms.PaymentTerm(ctx, *code)
	if err != nil {
		return fmt.Errorf("receivables: payment term %s: %w", *code, err)
	}
	if !ok {
		return problem.Invalid("receivables.unknown_payment_term", "payment_term_code", "Payment term "+*code+" is not configured.")
	}
	return nil
}

// total adds the items' net and tax up to the invoice's, and these to its
// gross, exactly in minor units; out of range, 422.
func (c *checked) total() error {
	nets, taxes := make([]money.Minor, len(c.items)), make([]money.Minor, len(c.items))
	for i, it := range c.items {
		nets[i], taxes[i] = it.Net, it.Tax
	}
	var err error
	if c.net, err = money.Sum(nets...); err == nil {
		if c.tax, err = money.Sum(taxes...); err == nil {
			c.gross, err = money.Sum(c.net, c.tax)
		}
	}
	if err != nil { // money.Sum's only error is the range
		return errTooLarge()
	}
	return nil
}

// parties resolves the legal entity, which the caller must be allowed to
// act in, and the customer: a business partner that may be posted as a
// customer there (active, with the customer role and, since T74a, not
// blocked for posting in the company), as the ledger checks a line on a
// customer reconciliation account. Its company data is required when the
// invoice posts, not when it is entered (ADR-0063 §3).
func (e *Endpoints) parties(ctx context.Context, in Input) (tenancy.Entity, partner.Named, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return tenancy.Entity{}, partner.Named{}, fmt.Errorf("receivables: company access: %w", err)
	}
	le, err := tenancy.Accessible(ctx, e.LegalEntities, access, in.LegalEntityID, "legal_entity_id")
	if err != nil {
		return tenancy.Entity{}, partner.Named{}, fmt.Errorf("receivables: legal entity of a customer invoice: %w", err)
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{in.CustomerID})
	if err != nil {
		return tenancy.Entity{}, partner.Named{}, fmt.Errorf("receivables: customer %s: %w", in.CustomerID, err)
	}
	named, ok := names[in.CustomerID]
	if !ok {
		return tenancy.Entity{}, partner.Named{}, problem.ReferenceNotFound("customer_id", "business partner", "no business partner with this id")
	}
	why, err := e.Customers.PostingBlock(ctx, in.CustomerID, le.ID, customerRole)
	if err != nil {
		return tenancy.Entity{}, partner.Named{}, fmt.Errorf("receivables: may customer %s be invoiced: %w", in.CustomerID, err)
	}
	if why != "" {
		return tenancy.Entity{}, partner.Named{}, problem.Invalid("receivables.customer_not_usable", "customer_id", "The customer cannot be invoiced: "+why+".")
	}
	return le, named, nil
}

// item checks one item: a net amount in the invoice's currency with its
// minor-unit digits, more than zero, on an account postable in the legal
// entity. Its tax code is checked with the tax (outputTax).
func (e *Endpoints) item(ctx context.Context, le tenancy.Entity, code string, exp, i int, it ItemInput) (Item, error) {
	field := fmt.Sprintf("items[%d]", i)
	if it.Net.Currency != code {
		return Item{}, problem.Invalid("receivables.currency_mismatch", field+".net_amount.currency", "Every amount of an invoice is in its currency, "+code+".")
	}
	net, err := currency.ParseAmount(it.Net.Value, code, exp, field+".net_amount.amount")
	if err != nil {
		return Item{}, fmt.Errorf("receivables: net amount of item %d: %w", i+1, err)
	}
	if net <= 0 {
		return Item{}, problem.Invalid("receivables.amount_not_positive", field+".net_amount.amount",
			"An item's net amount is more than zero: the kind of the document gives its direction.")
	}
	ok, err := e.Accounts.IsPostable(ctx, le.ID, it.AccountID)
	if err != nil {
		return Item{}, fmt.Errorf("receivables: account of item %d: %w", i+1, err)
	}
	if !ok {
		return Item{}, problem.Invalid("receivables.account_not_postable", field+".account_id", "The account cannot be posted in this legal entity.")
	}
	return Item{ItemNo: int32(i + 1), Description: it.Description, AccountID: it.AccountID, Net: net, TaxCode: it.TaxCode,
		CostCenterID: it.CostCenterID, ProfitCenterID: it.ProfitCenterID}, nil
}

// assignments checks the items' centers against the ledger as the posting
// will on the invoice's posting date, else its invoice date (ADR-0050 §6):
// an item without a cost center takes its account's default. The posting
// checks them again.
func (e *Endpoints) assignments(ctx context.Context, in Input, c checked) error {
	items := make([]ledger.ItemAssignment, len(c.items))
	for i, it := range c.items {
		items[i] = ledger.ItemAssignment{Index: i, AccountID: it.AccountID, CostCenterID: it.CostCenterID, ProfitCenterID: it.ProfitCenterID}
	}
	date := in.InvoiceDate
	if in.PostingDate != nil {
		date = *in.PostingDate
	}
	// The ledger's refusal names the items' own fields (items[i].…).
	if err := e.Accounts.CheckAssignments(ctx, c.le.ID, date, items, true); err != nil {
		return fmt.Errorf("receivables: check the items' assignments: %w", err)
	}
	return nil
}
