package receivables

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// DocumentType is the journal document type a customer invoice posts as
// (SAP's DR, customer invoice); its entries draw their numbers from the
// journal_dr number range.
const DocumentType = "DR"

// PostInvoice posts a draft invoice once (ADR-0063 §5, T75b): Dr the
// customer's reconciliation account in the company with the customer, the
// invoice's gross; Cr each item's account, its net, with the item's tax
// code and centers; Cr output tax, one line per code and component on the
// component's account, as the codes give it at the invoice date. The debit
// line is the customer's open item, and the ledger keeps the payment terms
// with it: the invoice's term, else the customer's in the company, counted
// from the invoice date.
//
// The invoice's row is read FOR UPDATE and only a draft posts (ADR-0008):
// a second post waits for the first and is then refused, 409
// receivables.invalid_status. The entry, the invoice's new status, its
// audit record and CustomerInvoicePosted are written in the request's
// transaction.
func (e *Endpoints) PostInvoice(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, postingDate *time.Time) (Invoice, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Invoice{}, err
	}
	v, err := get(ctx, tx, id, true)
	if err != nil {
		return Invoice{}, err
	}
	if err := ifMatch(v.Version); err != nil {
		return Invoice{}, err
	}
	if v.Status != Draft {
		return Invoice{}, problem.Conflict("receivables.invalid_status", "Customer invoice "+v.DocumentNumber+" is "+v.Status+"; only a draft is posted.")
	}
	le, _, err := e.LegalEntities.Entity(ctx, v.LegalEntityID) // a stored invoice's legal entity exists
	if err != nil {
		return Invoice{}, fmt.Errorf("receivables: company of customer invoice %s: %w", id, err)
	}
	date, err := e.postingDate(ctx, le, v, postingDate)
	if err != nil {
		return Invoice{}, err
	}
	customer, err := e.postableCustomer(ctx, v)
	if err != nil {
		return Invoice{}, err
	}
	in, err := e.posting(ctx, le, v, customer, date)
	if err != nil {
		return Invoice{}, err
	}
	entry, err := e.Postings.Post(ctx, in)
	if err != nil {
		return Invoice{}, fmt.Errorf("receivables: post customer invoice %s: %w", v.DocumentNumber, err)
	}
	if err := markPosted(ctx, tx, p, v, entry, date, in.Terms); err != nil {
		return Invoice{}, err
	}
	posted, err := load(ctx, tx, id, false)
	if err != nil {
		return Invoice{}, err
	}
	return posted, recordPosted(ctx, posted, le, customer, entry, in.Terms)
}

// postingDate is the date an invoice posts on: the request's, else the
// one entered with the invoice, else the company's date now in its time
// zone (product owner 2026-10-31), never UTC's for a company that sets one.
func (e *Endpoints) postingDate(ctx context.Context, le tenancy.Entity, v Invoice, requested *time.Time) (time.Time, error) {
	switch {
	case requested != nil:
		return *requested, nil
	case v.PostingDate != nil:
		return *v.PostingDate, nil
	}
	day, err := config.DocumentDay(ctx, e.Settings, le.Code, clock.Now(e.Now))
	if err != nil {
		return time.Time{}, fmt.Errorf("receivables: posting date of customer invoice %s: %w", v.DocumentNumber, err)
	}
	return day.Date, nil
}

// postableCustomer is the invoice's customer with its data in the
// company, which a receivables document requires (ADR-0063 §3, plan
// default 2): one that is not active, no longer a customer, without
// company data there or blocked for posting is refused, 422
// receivables.customer_not_usable.
func (e *Endpoints) postableCustomer(ctx context.Context, v Invoice) (partner.Customer, error) {
	c, ok, err := e.CustomerData.Customer(ctx, v.CustomerID, v.LegalEntityID)
	if err != nil {
		return partner.Customer{}, fmt.Errorf("receivables: customer %s of invoice %s: %w", v.CustomerID, v.DocumentNumber, err)
	}
	why := c.Usable()
	if !ok { // a stored invoice's customer exists: business partners are never deleted
		why = "there is no business partner with this id"
	}
	if why != "" {
		return partner.Customer{}, problem.Unprocessable("receivables.customer_not_usable", "The invoice cannot be posted to its customer: "+why+".")
	}
	return c, nil
}

// posting is the entry the invoice posts on the date, with the terms its
// open item keeps.
func (e *Endpoints) posting(ctx context.Context, le tenancy.Entity, v Invoice, customer partner.Customer, date time.Time) (ledger.Posting, error) {
	terms, err := e.invoiceTerms(ctx, v, customer)
	if err != nil {
		return ledger.Posting{}, err
	}
	taxLines, err := e.taxLines(ctx, le, v)
	if err != nil {
		return ledger.Posting{}, err
	}
	text := "Customer invoice " + v.DocumentNumber
	lines := make([]ledger.PostingLine, 0, 1+len(v.Items)+len(taxLines))
	lines = append(lines, ledger.PostingLine{AccountID: customer.ReconciliationID, Amount: v.Gross, PartnerID: &v.CustomerID, Text: &text})
	for _, it := range v.Items {
		lines = append(lines, ledger.PostingLine{AccountID: it.AccountID, Amount: -it.Net, TaxCode: it.TaxCode, Text: it.Description,
			CostCenterID: it.CostCenterID, ProfitCenterID: it.ProfitCenterID})
	}
	// The invoice date is the tax date (ADR-0063 §4): the ledger derives the
	// entry's tax items from its lines at that date's rates and holds the
	// tax lines to them (ADR-0049 §7).
	return ledger.Posting{LegalEntityID: v.LegalEntityID, DocumentType: DocumentType, DocumentDate: v.InvoiceDate, PostingDate: date,
		TaxDate: &v.InvoiceDate, Currency: v.Currency, Reference: &v.DocumentNumber, HeaderText: &text, SourceContext: "receivables",
		SourceDocumentID: v.ID, Lines: append(lines, taxLines...), Terms: terms}, nil
}

// invoiceTerms are the payment terms the invoice's open item keeps: the
// term entered with the invoice, else the customer's in the company,
// counted from the invoice date (ADR-0063 §5). Both name a configured
// term: one entered was checked, the customer's is held by a foreign key,
// and terms are never removed.
func (e *Endpoints) invoiceTerms(ctx context.Context, v Invoice, customer partner.Customer) (*ledger.ItemTerms, error) {
	code := customer.PaymentTermCode
	if v.PaymentTermCode != nil {
		code = *v.PaymentTermCode
	}
	term, ok, err := e.Terms.PaymentTerm(ctx, code)
	if err == nil && !ok {
		err = fmt.Errorf("payment term %s is not configured", code)
	}
	if err != nil {
		return nil, fmt.Errorf("receivables: payment terms of customer invoice %s: %w", v.DocumentNumber, err)
	}
	t := &ledger.ItemTerms{PaymentTermCode: code, BaselineDate: v.InvoiceDate, NetDays: term.NetDays}
	for _, d := range term.Discounts {
		t.Discounts = append(t.Discounts, ledger.Discount{Days: d.Days, Percent: d.Percent})
	}
	return t, nil
}

// taxLines credit the invoice's output tax: one line per code and
// component with tax, on the component's account with the code, in the
// calculator's order: the codes as they first appear among the items, each
// code's components in theirs. The tax is calculated again as
// it was when the invoice was entered, on the invoice date; when the
// codes no longer give the tax entered (a rate recorded or withdrawn for
// the date since), the invoice is refused, 409 receivables.tax_changed: a
// draft is not changed in the first version, so it is entered again.
func (e *Endpoints) taxLines(ctx context.Context, le tenancy.Entity, v Invoice) ([]ledger.PostingLine, error) {
	c, err := e.calculate(ctx, le, v.Currency, v.InvoiceDate, v.Items)
	if err != nil {
		return nil, err
	}
	for j, i := range c.coded {
		tax, err := c.itemTax(j)
		if err != nil {
			return nil, err
		}
		if tax != v.Items[i].Tax {
			return nil, problem.Conflict("receivables.tax_changed", fmt.Sprintf("Item %d's tax code %s now gives %s %s of tax on the invoice date; "+
				"the invoice was entered with %s. Enter the invoice again.", v.Items[i].ItemNo, *v.Items[i].TaxCode, tax.Plain(v.Exponent), v.Currency,
				v.Items[i].Tax.Plain(v.Exponent)))
		}
	}
	var lines []ledger.PostingLine
	for _, t := range c.res.Items {
		if t.Tax == 0 { // a zero-rated or exempt code: its base carries the code, and no tax posts
			continue
		}
		account := componentAccount(c.codes[t.Code], t.Key)
		if account == nil {
			return nil, problem.Unprocessable("receivables.no_tax_account", "Component "+t.Key+" of tax code "+t.Code+
				" has no account its tax posts to in the company's configuration.")
		}
		code := t.Code
		lines = append(lines, ledger.PostingLine{AccountID: *account, Amount: -t.Tax, TaxCode: &code})
	}
	return lines, nil
}

// componentAccount is the account the code's component posts its tax to,
// nil when the configuration gives it none.
func componentAccount(code ledger.TaxCode, component string) *uuid.UUID {
	var account *uuid.UUID
	for _, c := range code.Components {
		if c.Key == component {
			account = c.AccountID
		}
	}
	return account
}

// markPosted moves the invoice from draft to posted with its entry, the
// date it posted on, the term it posted with and its net due date.
// PostInvoice holds the row's lock and read it as a draft; the database
// refuses a change of any other (customer_invoice_posted_once).
func markPosted(ctx context.Context, tx pgx.Tx, p reqctx.Principal, v Invoice, entry ledger.Entry, date time.Time, terms *ledger.ItemTerms) error {
	if _, err := tx.Exec(ctx, `UPDATE receivables.customer_invoice SET status = 'posted', posting_date = $2, payment_term_code = $3, net_due_date = $4,
			journal_entry_year = $5, journal_entry_id = $6, version = version + 1, updated_by = $7
		WHERE id = $1`, v.ID, date, terms.PaymentTermCode, terms.NetDueDate(), entry.FiscalYear, entry.ID, p.ID); err != nil {
		return fmt.Errorf("receivables: mark customer invoice %s posted: %w", v.ID, err)
	}
	return nil
}

// recordPosted adds the audit record of the posting and publishes
// CustomerInvoicePosted in the same transaction (ADR-0007, ADR-0011). The
// event names the customer by id and number, never by name (product
// owner, 2026-11-05), and carries the totals, not the items.
func recordPosted(ctx context.Context, v Invoice, le tenancy.Entity, customer partner.Customer, entry ledger.Entry, terms *ledger.ItemTerms) error {
	const action = "receivables.customer_invoice.post"
	amount := func(m money.Minor) any { return money.EventAmount(m, v.Currency, v.Exponent) }
	payload := map[string]any{"customer_invoice_id": v.ID, "document_number": v.DocumentNumber, "legal_entity_id": v.LegalEntityID,
		"legal_entity_code": le.Code, "customer_id": v.CustomerID, "customer_number": customer.Number, "kind": v.Kind,
		"invoice_date": v.InvoiceDate.Format(time.DateOnly), "posting_date": v.PostingDate.Format(time.DateOnly),
		"net_amount": amount(v.Net), "tax_amount": amount(v.Tax), "gross_amount": amount(v.Gross),
		"payment_term_code": terms.PaymentTermCode, "net_due_date": terms.NetDueDate().Format(time.DateOnly),
		"journal_entry_id": entry.ID, "journal_entry_year": entry.FiscalYear, "journal_entry_number": entry.DocumentNumber,
		"status": v.Status, "status_before": Draft, "version": v.Version}
	detail := map[string]any{"document_number": v.DocumentNumber, "customer_id": v.CustomerID, "status": v.Status, "status_before": Draft,
		"posting_date": v.PostingDate.Format(time.DateOnly), "gross_amount": amount(v.Gross), "journal_entry_id": entry.ID,
		"journal_entry_number": entry.DocumentNumber}
	if err := audit.Record(ctx, action, "receivables.customer_invoice", v.ID, detail,
		outbox.Event{Context: "receivables", Name: "customer_invoice_posted", AggregateType: "receivables.customer_invoice", AggregateID: v.ID,
			Payload: payload}); err != nil {
		return fmt.Errorf("receivables: record %s of customer invoice %s: %w", action, v.ID, err)
	}
	return nil
}
