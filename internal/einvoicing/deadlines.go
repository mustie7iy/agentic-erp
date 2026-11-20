package einvoicing

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/idref"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The worklist of documents not transmitted (As built E3; the mandate's
// 14 business days to issue and transmit an e-invoice from the transaction
// date, MoF Guidelines v2.0 as the coordinator's plan cites them): a
// document's deadline is the transmitDays-th business day of its company
// after its issue date, its calendar's weekend and holidays left out.
const (
	transmitDays = 14
	// issuedLookback is the default first issue date listed, before the
	// key date, and issuedMost the furthest it may be.
	issuedLookback = 90
	issuedMost     = 366
	// maxDays bounds the search for a business day, as closing's and
	// cash's do, so a calendar whose every day is a holiday still answers.
	maxDays = 366
)

// Settings reads the active configuration (configuration context): the
// companies' calendars.
type Settings interface {
	Document(ctx context.Context) (*config.Config, int32, error)
}

// Issued is a posted customer invoice or credit memo as the worklist
// reads it (receivables): number, kind, date, customer and gross amount.
type Issued struct {
	ID         uuid.UUID
	Number     string
	Kind       string
	IssueDate  time.Time
	CustomerID uuid.UUID
	Currency   string
	Exponent   int
	Gross      money.Minor
}

// deadline is a row of the worklist.
type deadline struct {
	Issued
	due      time.Time
	left     int
	einvoice *uuid.UUID
	status   *string
	sending  *string
}

// ListEInvoiceTransmissionDeadlines implements GET
// /v1/e-invoice-transmission-deadlines (As built E3): the UAE company's
// posted customer invoices and credit memos issued in the period whose
// e-invoice is not transmitted (none, invalid, or not completed at the
// provider), each with its deadline and the business days left to it, by
// deadline, number and id, with the totals of every row. Computed at the
// read and kept nowhere; paged in memory.
func (e *Endpoints) ListEInvoiceTransmissionDeadlines(ctx context.Context, params gen.ListEInvoiceTransmissionDeadlinesParams) (
	gen.ListEInvoiceTransmissionDeadlinesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	key := params.KeyDate
	from := key.AddDate(0, 0, -issuedLookback)
	if params.IssuedFrom.Set {
		from = params.IssuedFrom.Value
	}
	if from.After(key) || from.Before(key.AddDate(0, 0, -issuedMost)) {
		return nil, problem.Invalid("einvoicing.invalid_period", "issued_from", fmt.Sprintf("on or before the key date and at most %d days before it", issuedMost))
	}
	co, doc, err := e.deadlineCompany(ctx, params.LegalEntityID)
	if err != nil {
		return nil, err
	}
	scope := fmt.Sprintf("listEInvoiceTransmissionDeadlines|%s|%s|%s", co.ID, key.Format(time.DateOnly), from.Format(time.DateOnly))
	ks, err := e.Pages.Keyset(deadlineSort, scope, apitypes.Page(gen.OptString{}, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	rows, err := e.deadlines(ctx, tx, doc, co, from, key)
	if err != nil {
		return nil, err
	}
	page, next := pagination.InMemory(ks, rows, deadlineKey)
	names, err := idref.Resolve(ctx, e.Sales.Customers, idref.Of(page, func(d deadline) *uuid.UUID { return &d.CustomerID }), customerRef)
	if err != nil {
		return nil, err
	}
	out := &gen.EInvoiceDeadlinePage{LegalEntityID: co.ID, CompanyCode: co.Code, CompanyName: co.Name, KeyDate: key, IssuedFrom: from,
		Count: int32(len(rows)), Items: make([]gen.EInvoiceDeadline, len(page))} // #nosec G115 -- a year of one company's documents
	for _, d := range rows {
		if d.left < 0 {
			out.OverdueCount++
		}
	}
	for i, d := range page {
		out.Items[i] = d.toAPI(names[d.CustomerID])
	}
	if next != "" {
		out.NextPageToken.SetTo(next)
	}
	return out, nil
}

// deadlineSort orders the worklist by its key, deadlineKey.
var deadlineSort = pagination.Sort{Default: "deadline", Fields: []pagination.Field{{Name: "deadline", Column: "deadline", Type: "text"}}}

// deadlineKey is a row's sort key: its deadline, then its number (padded,
// so the text compares as the order reads), then its id.
func deadlineKey(d deadline) (string, uuid.UUID) {
	return d.due.Format(time.DateOnly) + fmt.Sprintf("|%40s", d.Number), d.ID
}

// customerRef is a posted document's customer, the partner context's.
var customerRef = idref.Kind{Name: "customer", Context: "partner"}

// deadlineCompany is the UAE company the caller may act in, with the
// active configuration; refused otherwise.
func (e *Endpoints) deadlineCompany(ctx context.Context, id uuid.UUID) (Company, *config.Config, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Company{}, nil, err
	}
	named, err := e.Companies.Named(ctx, []uuid.UUID{id})
	co, ok := named[id]
	switch {
	case err != nil:
		return Company{}, nil, err
	case !ok || !access.Allows(id):
		return Company{}, nil, problem.ReferenceNotFound("legal_entity_id", "legal entity", "no company with this id that you may act in")
	}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return Company{}, nil, fmt.Errorf("einvoicing: the calendar of company %s: %w", co.Code, err)
	}
	if c := doc.Company(co.Code); c == nil || c.Country != "AE" || c.Currency != accountingCurrency {
		return Company{}, nil, problem.Invalid("einvoicing.not_a_uae_company", "legal_entity_id",
			"company "+co.Code+" is not a UAE company in AED, whose customer documents are e-invoiced")
	}
	return co, doc, nil
}

// deadlines are the company's documents issued from through key that are
// not transmitted, each with its deadline and the business days to it.
func (e *Endpoints) deadlines(ctx context.Context, tx pgx.Tx, doc *config.Config, co Company, from, key time.Time) ([]deadline, error) {
	issued, err := e.Sales.Issued(ctx, co.ID, from, key)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(issued))
	for i, d := range issued {
		ids[i] = d.ID
	}
	type state struct {
		einvoice uuid.UUID
		status   string
		sending  *string
	}
	rows, _ := tx.Query(ctx, `SELECT o.customer_invoice_id, o.id, o.status, s.status FROM einvoicing.outgoing_einvoice o
		LEFT JOIN einvoicing.outgoing_submission s ON s.tenant_id = o.tenant_id AND s.outgoing_einvoice_id = o.id
		WHERE o.customer_invoice_id = ANY($1)`, ids)
	states := map[uuid.UUID]state{}
	var doc2 uuid.UUID
	var st state
	if _, err := pgx.ForEachRow(rows, []any{&doc2, &st.einvoice, &st.status, &st.sending}, func() error { states[doc2] = st; return nil }); err != nil {
		return nil, fmt.Errorf("einvoicing: the e-invoices of company %s's documents: %w", co.Code, err)
	}
	var out []deadline
	for _, d := range issued {
		row := deadline{Issued: d}
		if s, ok := states[d.ID]; ok {
			if s.sending != nil && *s.sending == SubmissionCompleted {
				continue
			}
			row.einvoice, row.status, row.sending = &s.einvoice, &s.status, s.sending
		}
		row.due = businessDays(doc, co.Code, d.IssueDate, transmitDays)
		row.left = businessBetween(doc, co.Code, key, row.due)
		out = append(out, row)
	}
	return out, nil
}

// businessDays is the n-th business day of the company after day.
func businessDays(doc *config.Config, company string, day time.Time, n int) time.Time {
	d := day
	for i := 0; n > 0 && i < maxDays; i++ {
		d = d.AddDate(0, 0, 1)
		if doc.BusinessDay(company, d) {
			n--
		}
	}
	return d
}

// businessBetween is the business days from key to due: those after key
// up to and including due, or, with due before key, minus those after due
// up to and including key.
func businessBetween(doc *config.Config, company string, key, due time.Time) int {
	from, to, sign := key, due, 1
	if due.Before(key) {
		from, to, sign = due, key, -1
	}
	n := 0
	for d := from.AddDate(0, 0, 1); !d.After(to); d = d.AddDate(0, 0, 1) {
		if doc.BusinessDay(company, d) {
			n++
		}
	}
	return sign * n
}

func (d deadline) toAPI(c CustomerName) gen.EInvoiceDeadline {
	out := gen.EInvoiceDeadline{CustomerInvoiceID: d.ID, CustomerInvoiceNumber: d.Number, Kind: d.Kind, IssueDate: d.IssueDate, Deadline: d.due,
		Overdue: d.left < 0, BusinessDaysLeft: int32(d.left), CustomerID: d.CustomerID, CustomerNumber: c.Number, CustomerName: c.Name, // #nosec G115 -- at most a year of days
		GrossAmount: apitypes.Money(d.Gross, d.Currency, d.Exponent), OutgoingEinvoiceID: nilUUID(d.einvoice), EinvoiceStatus: nilString(d.status),
		TransmissionStatus: nilString(d.sending)}
	return out
}
