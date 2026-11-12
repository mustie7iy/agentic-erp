package sales

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// The statuses of a quotation: open when made, then the customer's
// answer, recorded once (ADR-0067 §2).
const (
	Open     = "open"
	Accepted = "accepted"
	Rejected = "rejected"
)

// QuotationRange is the document type quotations draw their numbers from
// (configuration number_ranges).
const QuotationRange = "quotation"

// Quotation is a row of sales.quotation with its items. Amounts are minor
// units of Currency, whose minor-unit digits are Exponent; Net is the sum
// of the items' net values.
type Quotation struct {
	ID             uuid.UUID
	DocumentNumber string
	LegalEntityID  uuid.UUID
	CustomerID     uuid.UUID
	QuotationDate  time.Time
	ValidUntil     time.Time
	Currency       string
	Exponent       int
	Net            money.Minor
	Status         string
	Items          []Item
	Version        int32
	CreatedAt      time.Time
	CreatedBy      uuid.UUID
	UpdatedAt      time.Time
	UpdatedBy      uuid.UUID
}

// QuotationInput is a quotation as a client sends it.
type QuotationInput struct {
	LegalEntityID uuid.UUID
	CustomerID    uuid.UUID
	ValidUntil    time.Time
	Currency      string
	Items         []ItemInput
}

var quotationSelect = `SELECT q.id, q.document_number, q.legal_entity_id, q.customer_id, q.quotation_date, q.valid_until, q.currency, c.exponent,
		q.net_amount, q.status, ` + itemsJSON("quotation_item", "quotation_id", "q", "") + `,
		q.version, q.created_at, q.created_by, q.updated_at, q.updated_by
	FROM sales.quotation q JOIN ref.currency c ON c.code = q.currency`

func scanQuotation(row pgx.Row) (Quotation, error) {
	var q Quotation
	err := row.Scan(&q.ID, &q.DocumentNumber, &q.LegalEntityID, &q.CustomerID, &q.QuotationDate, &q.ValidUntil, &q.Currency, &q.Exponent,
		&q.Net, &q.Status, &q.Items, &q.Version, &q.CreatedAt, &q.CreatedBy, &q.UpdatedAt, &q.UpdatedBy)
	return q, errs.Wrapf(err, "scan quotation")
}

// loadQuotation reads a quotation whatever its company, forUpdate under
// its row lock (ADR-0008); quotation applies company access.
func loadQuotation(ctx context.Context, tx pgx.Tx, id uuid.UUID, forUpdate bool) (Quotation, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE OF q"
	}
	q, err := scanQuotation(tx.QueryRow(ctx, quotationSelect+` WHERE q.id = $1`+lock, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Quotation{}, problem.NotFoundOf("quotation")
	}
	if err != nil {
		return Quotation{}, fmt.Errorf("sales: read quotation %s: %w", id, err)
	}
	return q, nil
}

// quotation reads a quotation of a legal entity the caller may act in,
// forUpdate under its row lock; another's is not found, so ids cannot be
// probed (04 §4.9).
func quotation(ctx context.Context, tx pgx.Tx, id uuid.UUID, forUpdate bool) (Quotation, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Quotation{}, fmt.Errorf("sales: company access for quotation %s: %w", id, err)
	}
	q, err := loadQuotation(ctx, tx, id, forUpdate)
	if err == nil && !access.Allows(q.LegalEntityID) {
		return Quotation{}, problem.NotFoundOf("quotation")
	}
	return q, err
}

// GetQuotation returns a quotation of a legal entity the caller may act
// in.
func GetQuotation(ctx context.Context, id uuid.UUID) (Quotation, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Quotation{}, err
	}
	return quotation(ctx, tx, id, false)
}

// MakeQuotation makes a quotation to a customer (ADR-0067 §2-§4): open,
// dated the company's date today, numbered from the company's quotation
// range of that date's fiscal year, its items checked and priced and its
// net value their sum. It publishes QuotationChanged with its audit
// record. Nothing is posted.
func (e *Endpoints) MakeQuotation(ctx context.Context, in QuotationInput) (Quotation, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Quotation{}, err
	}
	le, customer, err := e.parties(ctx, in.LegalEntityID, in.CustomerID)
	if err != nil {
		return Quotation{}, err
	}
	doc, err := e.document(ctx, tx, le, in.CustomerID, in.Currency)
	if err != nil {
		return Quotation{}, err
	}
	if in.ValidUntil.Before(doc.date) {
		return Quotation{}, problem.Invalid("sales.invalid_validity", "valid_until",
			"A quotation is valid until a day not before the day it is made, "+doc.date.Format(time.DateOnly)+", the company's date in "+doc.zone.String()+".")
	}
	items, net, err := e.items(ctx, tx, doc, in.Items)
	if err != nil {
		return Quotation{}, err
	}
	number, err := e.number(ctx, le.ID, QuotationRange, doc.date)
	if err != nil {
		return Quotation{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO sales.quotation (tenant_id, id, document_number, legal_entity_id, customer_id, quotation_date, valid_until,
			currency, net_amount, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11)`,
		p.TenantID, id, number, le.ID, in.CustomerID, doc.date, in.ValidUntil, in.Currency, net, Open, p.ID); err != nil {
		return Quotation{}, fmt.Errorf("sales: insert quotation %s: %w", id, err)
	}
	if err := insertItems(ctx, tx, "quotation_item", "quotation_id", p.TenantID, id, items); err != nil {
		return Quotation{}, err
	}
	q, err := loadQuotation(ctx, tx, id, false)
	if err != nil {
		return Quotation{}, err
	}
	return q, recordQuotation(ctx, "sales.quotation.create", q, le, customer, nil)
}

// parties resolves the legal entity, which the caller must be allowed to
// act in, and the customer: a business partner that may be posted as a
// customer there (active, with the customer role and not blocked for
// posting in the company), as receivables checks one at entry (ADR-0063
// §3). Its company data is required from the sales order, not for a
// quotation (ADR-0067 §4).
func (e *Endpoints) parties(ctx context.Context, legalEntity, customer uuid.UUID) (tenancy.Entity, partner.Named, error) {
	le, err := e.company(ctx, legalEntity)
	if err != nil {
		return tenancy.Entity{}, partner.Named{}, err
	}
	named, err := e.customer(ctx, le, customer)
	return le, named, err
}

// company is the request's legal entity (legal_entity_id), one the caller
// may act in.
func (e *Endpoints) company(ctx context.Context, legalEntity uuid.UUID) (tenancy.Entity, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return tenancy.Entity{}, fmt.Errorf("sales: company access: %w", err)
	}
	le, err := tenancy.Accessible(ctx, e.LegalEntities, access, legalEntity, "legal_entity_id")
	if err != nil {
		return tenancy.Entity{}, fmt.Errorf("sales: legal entity %s: %w", legalEntity, err)
	}
	return le, nil
}

// customer is the request's customer (customer_id) by number and name: a
// business partner that may be posted as a customer in the company, else
// 422 naming customer_id (request.reference_not_found for an unknown
// one, sales.customer_not_usable otherwise).
func (e *Endpoints) customer(ctx context.Context, le tenancy.Entity, customer uuid.UUID) (partner.Named, error) {
	names, err := e.Partners.Names(ctx, []uuid.UUID{customer})
	if err != nil {
		return partner.Named{}, fmt.Errorf("sales: customer %s: %w", customer, err)
	}
	named, ok := names[customer]
	if !ok {
		return partner.Named{}, problem.ReferenceNotFound("customer_id", "business partner", "no business partner with this id")
	}
	why, err := e.Customers.PostingBlock(ctx, customer, le.ID, customerRole)
	if err != nil {
		return partner.Named{}, fmt.Errorf("sales: may customer %s be sold to: %w", customer, err)
	}
	if why != "" {
		return partner.Named{}, problem.Invalid("sales.customer_not_usable", "customer_id", "The customer cannot be sold to: "+why+".")
	}
	return named, nil
}

// document is what a new document of the company to the customer in the
// currency is checked and priced against: the currency's minor-unit
// digits (422 sales.unknown_currency naming currency) and the company's
// date today.
func (e *Endpoints) document(ctx context.Context, tx pgx.Tx, le tenancy.Entity, customer uuid.UUID, code string) (document, error) {
	exp, err := currency.Exponent(ctx, tx, code, problem.Invalid("sales.unknown_currency", "currency", "The currency is not known."))
	if err != nil {
		return document{}, fmt.Errorf("sales: currency %s: %w", code, err)
	}
	day, err := e.today(ctx, le.Code)
	if err != nil {
		return document{}, err
	}
	return document{le: le, customer: customer, currency: code, exp: exp, date: day.Date, zone: day.Zone}, nil
}

// MarkAccepted records that the customer accepted an open quotation,
// once (ADR-0067 §2). A quotation whose validity ended before the
// company's date today is not accepted (409 sales.quotation_expired).
func (e *Endpoints) MarkAccepted(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Quotation, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Quotation{}, err
	}
	q, le, err := e.openQuotation(ctx, tx, id, ifMatch)
	if err != nil {
		return Quotation{}, err
	}
	if err := e.stillValid(ctx, q); err != nil {
		return Quotation{}, err
	}
	return e.answer(ctx, tx, p, q, le, Accepted, "sales.quotation.accept")
}

// MarkRejected records that the customer rejected an open quotation,
// once (ADR-0067 §2), whether or not it is still valid: it only records
// the customer's answer.
func (e *Endpoints) MarkRejected(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (Quotation, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Quotation{}, err
	}
	q, le, err := e.openQuotation(ctx, tx, id, ifMatch)
	if err != nil {
		return Quotation{}, err
	}
	return e.answer(ctx, tx, p, q, le, Rejected, "sales.quotation.decline")
}

// openQuotation reads the quotation to answer FOR UPDATE, at the version
// the caller read, with its company: only an open one is answered
// (ADR-0008), so a second answer waits for the first and is then refused,
// 409 sales.invalid_status.
func (e *Endpoints) openQuotation(ctx context.Context, tx pgx.Tx, id uuid.UUID, ifMatch func(int32) error) (Quotation, tenancy.Entity, error) {
	q, err := quotation(ctx, tx, id, true)
	if err != nil {
		return Quotation{}, tenancy.Entity{}, err
	}
	if err := ifMatch(q.Version); err != nil {
		return Quotation{}, tenancy.Entity{}, err
	}
	if q.Status != Open {
		return Quotation{}, tenancy.Entity{}, problem.Conflict("sales.invalid_status",
			"Quotation "+q.DocumentNumber+" is "+q.Status+"; only an open quotation is accepted or declined.")
	}
	le, _, err := e.LegalEntities.Entity(ctx, q.LegalEntityID) // a stored quotation's legal entity exists
	if err != nil {
		return Quotation{}, tenancy.Entity{}, fmt.Errorf("sales: company of quotation %s: %w", id, err)
	}
	return q, le, nil
}

// answer gives the open quotation q, read under its row lock, the status
// of the customer's answer, and writes the audit record of the action and
// QuotationChanged in the request's transaction.
func (e *Endpoints) answer(ctx context.Context, tx pgx.Tx, p reqctx.Principal, q Quotation, le tenancy.Entity, status, action string) (Quotation, error) {
	names, err := e.Partners.Names(ctx, []uuid.UUID{q.CustomerID})
	if err != nil {
		return Quotation{}, fmt.Errorf("sales: customer of quotation %s: %w", q.ID, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sales.quotation SET status = $2, version = version + 1, updated_by = $3 WHERE id = $1`, q.ID, status, p.ID); err != nil {
		return Quotation{}, fmt.Errorf("sales: mark quotation %s %s: %w", q.ID, status, err)
	}
	answered, err := loadQuotation(ctx, tx, q.ID, false)
	if err != nil {
		return Quotation{}, err
	}
	before := Open
	return answered, recordQuotation(ctx, action, answered, le, names[q.CustomerID], &before)
}

// stillValid refuses to accept a quotation whose validity ended before
// its company's date today in its time zone (409 sales.quotation_expired).
// The date is the legal entity's through config.EntityDay, which refuses
// a legal entity the directory does not know rather than dating it in
// UTC.
func (e *Endpoints) stillValid(ctx context.Context, q Quotation) error {
	today, err := config.EntityDay(ctx, e.Settings, tenancy.CompanyCode(e.LegalEntities), q.LegalEntityID, clock.Now(e.Now))
	if err != nil {
		return fmt.Errorf("sales: today for quotation %s: %w", q.DocumentNumber, err)
	}
	if q.ValidUntil.Before(today.Date) {
		return problem.Conflict("sales.quotation_expired", "Quotation "+q.DocumentNumber+" was valid until "+q.ValidUntil.Format(time.DateOnly)+
			"; today is "+today.Date.Format(time.DateOnly)+", the company's date in "+today.Zone.String()+". It is made again.")
	}
	return nil
}

// recordQuotation adds the audit record of a quotation's change and
// publishes QuotationChanged in the same transaction (ADR-0007,
// ADR-0011); before is its status before, nil when it had none. The event
// names the customer by id and number, never by name (product owner,
// 2026-11-05), and carries the net value, not the items.
func recordQuotation(ctx context.Context, action string, q Quotation, le tenancy.Entity, customer partner.Named, before *string) error {
	net := money.EventAmount(q.Net, q.Currency, q.Exponent)
	payload := map[string]any{"quotation_id": q.ID, "document_number": q.DocumentNumber, "legal_entity_id": q.LegalEntityID,
		"legal_entity_code": le.Code, "customer_id": q.CustomerID, "customer_number": customer.Number,
		"quotation_date": q.QuotationDate.Format(time.DateOnly), "valid_until": q.ValidUntil.Format(time.DateOnly), "net_amount": net,
		"status": q.Status, "status_before": before, "version": q.Version}
	detail := map[string]any{"document_number": q.DocumentNumber, "customer_id": q.CustomerID, "status": q.Status, "status_before": before,
		"valid_until": q.ValidUntil.Format(time.DateOnly), "net_amount": net, "items": len(q.Items)}
	return record(ctx, action, "quotation", "quotation_changed", q.ID, detail, payload)
}
