package payables

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// Down payment request statuses.
const (
	RequestOpen      = "open"
	RequestPaid      = "paid"    // by a payment run, whose payment posted the down payment (T46c)
	RequestCleared   = "cleared" // against the supplier's final invoice (T46d)
	RequestCancelled = "cancelled"
)

// DownPaymentRequest is a request to pay a supplier in advance of an
// invoice still to come (SAP F-47, special G/L F; ADR-0037). It is a
// payables document, not a ledger posting: it moves no value until a
// payment run pays it, which posts the special G/L item A.
type DownPaymentRequest struct {
	ID              uuid.UUID
	DocumentNumber  string // from the down_payment_request range of the due date's fiscal year
	LegalEntityID   uuid.UUID
	SupplierID      uuid.UUID
	PurchaseOrderID *uuid.UUID
	Currency        string
	Exponent        int
	Amount          money.Minor // what is paid, tax included
	TaxCode         *string
	TaxAmount       money.Minor // the input tax the amount includes
	DueDate         time.Time
	Reference       *string
	Status          string
	CancelReason    *string
	// JournalEntryYear and JournalEntryID are the entry that paid it.
	JournalEntryYear *int16
	JournalEntryID   *uuid.UUID
	// ClearedInvoiceID and ClearingEntryID are the invoice the down
	// payment was cleared against and the transfer entry (T46d).
	ClearedInvoiceID *uuid.UUID
	ClearingEntryID  *uuid.UUID
	Version          int32
	CreatedAt        time.Time
	CreatedBy        uuid.UUID
	UpdatedAt        time.Time
	UpdatedBy        uuid.UUID
}

// DownPaymentInput is a down payment request as a client sends it.
type DownPaymentInput struct {
	LegalEntityID   uuid.UUID
	SupplierID      uuid.UUID
	PurchaseOrderID *uuid.UUID
	Amount          Amount
	TaxCode         *string
	DueDate         time.Time
	Reference       *string
}

const requestSelect = `SELECT r.id, r.document_number, r.legal_entity_id, r.supplier_id, r.purchase_order_id, r.currency, c.exponent, r.amount, r.tax_code,
		r.tax_amount, r.due_date, r.reference, r.status, r.cancel_reason, r.journal_entry_year, r.journal_entry_id, r.cleared_invoice_id, r.clearing_entry_id, r.version, r.created_at, r.created_by, r.updated_at, r.updated_by
	FROM payables.down_payment_request r JOIN ref.currency c ON c.code = r.currency`

func scanRequest(row pgx.Row) (DownPaymentRequest, error) {
	var r DownPaymentRequest
	err := row.Scan(&r.ID, &r.DocumentNumber, &r.LegalEntityID, &r.SupplierID, &r.PurchaseOrderID, &r.Currency, &r.Exponent, &r.Amount, &r.TaxCode,
		&r.TaxAmount, &r.DueDate, &r.Reference, &r.Status, &r.CancelReason, &r.JournalEntryYear, &r.JournalEntryID, &r.ClearedInvoiceID, &r.ClearingEntryID, &r.Version, &r.CreatedAt, &r.CreatedBy, &r.UpdatedAt, &r.UpdatedBy)
	return r, errs.Wrapf(err, "scan down payment request")
}

// getRequest reads a request of a legal entity the caller may act in;
// lock takes the row lock for a change (ADR-0008).
func getRequest(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (DownPaymentRequest, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return DownPaymentRequest{}, err
	}
	r, err := loadRequest(ctx, tx, id, lock)
	if err == nil && !access.Allows(r.LegalEntityID) {
		return DownPaymentRequest{}, problem.NotFoundOf("down payment request")
	}
	return r, err
}

// loadRequest reads a request without company access, for the payments
// context's run (DownPaymentRequests); lock takes the row lock.
func loadRequest(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (DownPaymentRequest, error) {
	q := requestSelect + ` WHERE r.id = $1`
	if lock {
		q += ` FOR UPDATE OF r`
	}
	r, err := scanRequest(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return DownPaymentRequest{}, problem.NotFoundOf("down payment request")
	}
	if err != nil {
		return DownPaymentRequest{}, fmt.Errorf("payables: read down payment request %s: %w", id, err)
	}
	return r, nil
}

// GetDownPaymentRequest returns a down payment request.
func GetDownPaymentRequest(ctx context.Context, id uuid.UUID) (DownPaymentRequest, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return DownPaymentRequest{}, err
	}
	return getRequest(ctx, tx, id, false)
}

// requestCheck is a validated request: its currency's digits, the amount,
// and the tax it includes.
type requestCheck struct {
	exp           int
	amount, tax   money.Minor
	purchaseOrder *uuid.UUID
}

// validateRequest checks a request: a legal entity the caller may act in,
// a usable supplier, a positive amount with its currency's digits, an
// order of the supplier, and an input tax code valid on the due date.
func (e *Endpoints) validateRequest(ctx context.Context, tx pgx.Tx, in DownPaymentInput) (requestCheck, error) {
	if in.Reference != nil && strings.TrimSpace(*in.Reference) == "" {
		return requestCheck{}, invalid("payables.invalid_reference", "reference", "A reference is not blank; leave it out instead.")
	}
	le, sup, err := e.invoiceParties(ctx, Input{LegalEntityID: in.LegalEntityID, SupplierID: in.SupplierID})
	if err != nil {
		return requestCheck{}, err
	}
	exp, err := currencyExponent(ctx, tx, in.Amount.Currency, "amount.currency")
	if err != nil {
		return requestCheck{}, err
	}
	v := &invoiceCheck{e: e, ctx: ctx, tx: tx, le: le, sup: sup, exp: exp, c: &checked{currency: in.Amount.Currency},
		orders: map[uuid.UUID]procurement.Order{}}
	c := requestCheck{exp: exp, purchaseOrder: in.PurchaseOrderID}
	if c.amount, err = v.parse(in.Amount, "amount"); err != nil {
		return requestCheck{}, err
	}
	if c.amount <= 0 {
		return requestCheck{}, problem.Unprocessable("payables.invalid_amount", "A down payment is more than zero.",
			problem.FieldError{Field: "amount.amount", Code: "not_positive", Message: "an amount above 0"})
	}
	if in.PurchaseOrderID != nil {
		if _, err := v.order(*in.PurchaseOrderID, "purchase_order_id"); err != nil {
			return requestCheck{}, err
		}
	}
	if c.tax, err = e.requestTax(ctx, in, c.amount, exp); err != nil {
		return requestCheck{}, err
	}
	return c, nil
}

// requestTax is the input tax a request's amount includes, amount × rate
// / (100 + rate) rounded half away from zero, or 0 without a tax code.
// The code must be an input tax code of the legal entity on the due date.
func (e *Endpoints) requestTax(ctx context.Context, in DownPaymentInput, amount money.Minor, exp int) (money.Minor, error) {
	if in.TaxCode == nil {
		return 0, nil
	}
	code, ok, err := e.Accounts.TaxCode(ctx, in.LegalEntityID, *in.TaxCode, in.DueDate)
	if err != nil {
		return 0, err
	}
	if !ok || code.Direction != "input" {
		return 0, invalid("payables.unknown_tax_code", "tax_code", "Tax code "+*in.TaxCode+" is not an input tax code of the legal entity on the due date.")
	}
	rate := money.ParseRat(code.RatePct)
	share := new(big.Rat).Quo(rate, new(big.Rat).Add(rate, big.NewRat(100, 1)))
	return money.Round(new(big.Rat).Mul(money.Rat(amount, exp), share), exp)
}

// requestNumber draws a request's document number from the legal entity's
// down_payment_request range of the fiscal year its due date falls in.
func (e *Endpoints) requestNumber(ctx context.Context, in DownPaymentInput) (string, error) {
	year, err := e.Calendar.FiscalYear(ctx, in.LegalEntityID, in.DueDate)
	if err != nil {
		return "", err
	}
	return e.Numbers.Next(ctx, in.LegalEntityID, RequestNumbers, year)
}

// RequestNumbers is the number range object of down payment requests.
const RequestNumbers = "down_payment_request"

// RequestDownPayment enters an open down payment request and publishes
// DownPaymentRequested.
func (e *Endpoints) RequestDownPayment(ctx context.Context, in DownPaymentInput) (DownPaymentRequest, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return DownPaymentRequest{}, err
	}
	c, err := e.validateRequest(ctx, tx, in)
	if err != nil {
		return DownPaymentRequest{}, err
	}
	number, err := e.requestNumber(ctx, in)
	if err != nil {
		return DownPaymentRequest{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO payables.down_payment_request (tenant_id, id, document_number, legal_entity_id, supplier_id, purchase_order_id,
			currency, amount, tax_code, tax_amount, due_date, reference, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $13)`,
		p.TenantID, id, number, in.LegalEntityID, in.SupplierID, in.PurchaseOrderID, in.Amount.Currency, c.amount, in.TaxCode, c.tax, in.DueDate,
		in.Reference, p.ID); err != nil {
		return DownPaymentRequest{}, fmt.Errorf("payables: insert down payment request %s: %w", id, err)
	}
	r, err := getRequest(ctx, tx, id, false)
	if err != nil {
		return DownPaymentRequest{}, err
	}
	return r, recordRequest(ctx, "payables.down_payment_request.create", r, map[string]any{"document_number": r.DocumentNumber,
		"amount": r.Amount.Plain(r.Exponent), "currency": r.Currency},
		requestEvent(r, "down_payment_requested", map[string]any{"document_number": r.DocumentNumber, "purchase_order_id": r.PurchaseOrderID,
			"amount": money.EventAmount(r.Amount, r.Currency, r.Exponent), "tax_code": r.TaxCode,
			"tax_amount": money.EventAmount(r.TaxAmount, r.Currency, r.Exponent), "due_date": r.DueDate.Format(time.DateOnly),
			"reference": r.Reference, "status": r.Status}))
}

// CancelRequest cancels an open request, which no payment run
// then pays, and publishes DownPaymentRequestCancelled. A request a live
// run pays is not cancelled (409 payables.request_in_payment_run): asking
// the run holds off the legal entity's proposals until this commits.
func (e *Endpoints) CancelRequest(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, reason string) (DownPaymentRequest, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return DownPaymentRequest{}, err
	}
	r, err := getRequest(ctx, tx, id, true)
	if err != nil {
		return DownPaymentRequest{}, err
	}
	if err := ifMatch(r.Version); err != nil {
		return DownPaymentRequest{}, err
	}
	if r.Status != RequestOpen {
		return DownPaymentRequest{}, problem.Conflict("payables.invalid_status", "The down payment request is "+r.Status+"; cancel requires status [\"open\"].")
	}
	if run, held, err := e.Payments.RequestRunOf(ctx, r.LegalEntityID, id); err != nil || held {
		if err != nil {
			return DownPaymentRequest{}, err
		}
		return DownPaymentRequest{}, problem.Conflict("payables.request_in_payment_run", "Payment run "+run+" pays the down payment request; reject the run first.")
	}
	if _, err := tx.Exec(ctx, `UPDATE payables.down_payment_request SET status = $2, cancel_reason = $3, version = version + 1, updated_by = $4
		WHERE id = $1 AND status = $5`, id, RequestCancelled, reason, p.ID, RequestOpen); err != nil {
		return DownPaymentRequest{}, fmt.Errorf("payables: cancel down payment request %s: %w", id, err)
	}
	cancelled, err := getRequest(ctx, tx, id, false)
	if err != nil {
		return DownPaymentRequest{}, err
	}
	return cancelled, recordRequest(ctx, "payables.down_payment_request.cancel", cancelled, map[string]any{"reason": reason},
		requestEvent(cancelled, "down_payment_request_cancelled", map[string]any{"previous_status": r.Status, "status": cancelled.Status}))
}

// recordRequest publishes the events and adds the audit entry of a
// change to a request.
func recordRequest(ctx context.Context, action string, r DownPaymentRequest, detail map[string]any, events ...outbox.Event) error {
	return audit.Record(ctx, action, "payables.down_payment_request", r.ID, detail, events...)
}

func requestEvent(r DownPaymentRequest, name string, payload map[string]any) outbox.Event {
	payload["down_payment_request_id"], payload["supplier_id"], payload["legal_entity_id"] = r.ID, r.SupplierID, r.LegalEntityID
	return outbox.Event{Context: "payables", Name: name, AggregateType: "payables.down_payment_request", AggregateID: r.ID, Payload: payload}
}
