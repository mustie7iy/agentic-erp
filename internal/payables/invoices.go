package payables

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/mergepatch"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Sources an invoice can be entered with; capture is :capture's own.
var Sources = map[string]bool{"manual": true, "edi": true, "migration": true}

// newInvoice is an invoice to insert.
type newInvoice struct {
	Input
	source     string
	status     string
	holdReason *string
	sha        []byte
	objectRef  string // where the captured document is kept; "" records sha256:<hex>
}

// insert numbers and inserts a validated invoice and publishes
// SupplierInvoiceCaptured.
func (e *Endpoints) insert(ctx context.Context, tx pgx.Tx, p reqctx.Principal, in newInvoice) (Invoice, error) {
	c, err := e.validate(ctx, tx, in.Input)
	if err != nil {
		return Invoice{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if err := e.insertRules(ctx, &in, id, c); err != nil {
		return Invoice{}, err
	}
	year, err := e.Calendar.FiscalYear(ctx, in.LegalEntityID, in.InvoiceDate)
	if err != nil {
		return Invoice{}, err
	}
	number, err := e.Numbers.Next(ctx, in.LegalEntityID, "supplier_invoice", year)
	if err != nil {
		return Invoice{}, err
	}
	if in.Extensions == nil {
		in.Extensions = map[string]any{}
	}
	var ref *string
	if in.sha != nil {
		r := "sha256:" + hex.EncodeToString(in.sha)
		if in.objectRef != "" {
			r = in.objectRef
		}
		ref = &r
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payables.supplier_invoice (tenant_id, id, document_number, legal_entity_id, supplier_id, supplier_reference,
			invoice_date, posting_date, currency, gross_amount, tax_amount, purchase_order_id, status, hold_reason, source, capture_object_ref,
			capture_sha256, extensions, created_by, updated_by, kind, reference_invoice_id, extensions_schema, tax_date)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $19, $20, $21, $22, $23)`,
		p.TenantID, id, number, in.LegalEntityID, in.SupplierID, in.SupplierReference, in.InvoiceDate, in.PostingDate, c.currency, c.gross, c.tax,
		c.purchaseOrder, in.status, in.holdReason, in.source, ref, in.sha, in.Extensions, p.ID, c.kind, in.ReferenceInvoiceID, c.extensionsSchema,
		in.TaxDate); err != nil {
		return Invoice{}, fmt.Errorf("payables: insert invoice %s: %w", id, err)
	}
	if err := putItems(ctx, tx, p.TenantID, id, c.items); err != nil {
		return Invoice{}, err
	}
	v, err := load(ctx, tx, id, false)
	if err != nil {
		return Invoice{}, err
	}
	var sha *string
	if in.sha != nil {
		s := hex.EncodeToString(in.sha)
		sha = &s
	}
	captured := map[string]any{"document_number": v.DocumentNumber, "supplier_reference": v.SupplierReference,
		"invoice_date": v.InvoiceDate.Format(time.DateOnly), "gross_amount": money.EventAmount(signed(v, v.Gross), v.Currency, v.Exponent),
		"tax_amount": money.EventAmount(signed(v, v.Tax), v.Currency, v.Exponent), "purchase_order_id": v.PurchaseOrderID, "source": v.Source,
		"capture_sha256": sha, "status": v.Status, "kind": v.Kind, "reference_invoice_id": v.ReferenceInvoiceID}
	if err := e.extensions().Event(ctx, captured, v.Extensions, v.ExtensionsSchema); err != nil {
		return Invoice{}, err
	}
	events := []outbox.Event{event(v, "supplier_invoice_captured", captured)}
	if v.Status == OnHold {
		events = append(events, heldEvent(v))
	}
	return v, record(ctx, "payables.supplier_invoice.create", id, map[string]any{"document_number": v.DocumentNumber, "source": v.Source}, events...)
}

// insertRules evaluates the tenant's rules on a new invoice. A refusal is
// 422, except for a captured invoice, which lands held naming the rules,
// as it does without a required custom field, for a person to complete;
// its audit names the invoice id. An invoice held for a missing required
// field is not judged until it is completed: the rules may read the field.
func (e *Endpoints) insertRules(ctx context.Context, in *newInvoice, id uuid.UUID, c checked) error {
	if in.incomplete {
		return nil
	}
	if !in.held {
		return e.ruleWrites().Check(ctx, ruleWrite("payables.supplier_invoice.create", uuid.Nil, in.Input, c, in.source))
	}
	reason, err := e.ruleWrites().Hold(ctx, ruleWrite("payables.supplier_invoice.create", id, in.Input, c, in.source))
	if err != nil || reason == "" {
		return err
	}
	if in.holdReason != nil {
		reason = *in.holdReason + " " + reason
	}
	in.status, in.holdReason = OnHold, &reason
	return nil
}

// CreateInvoice enters a parked invoice.
func (e *Endpoints) CreateInvoice(ctx context.Context, in Input, source string) (Invoice, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Invoice{}, err
	}
	if !Sources[source] {
		return Invoice{}, invalid("payables.unknown_source", "source", "Invoices are entered as manual, edi or migration; documents use :capture.")
	}
	return e.insert(ctx, tx, p, newInvoice{Input: in, source: source, status: Parked})
}

// Change is a merge patch of a parked invoice; nil fields are unchanged.
type Change struct {
	SupplierReference *string
	InvoiceDate       *time.Time
	PostingDate       **time.Time // set to nil to clear
	TaxDate           **time.Time // set to nil to clear: the invoice date
	Gross, Tax        *Amount
	PurchaseOrderID   **uuid.UUID // set to nil to clear
	// ReferenceInvoiceID names the invoice a correction corrects; set to nil
	// to clear. The kind is fixed once entered.
	ReferenceInvoiceID **uuid.UUID
	Items              []ItemInput // nil: unchanged; the complete list otherwise
	Extensions         map[string]any
}

func amountInput(minor money.Minor, currency string, exp int) Amount {
	return Amount{Value: minor.Plain(exp), Currency: currency}
}

// UpdateInvoice changes a parked invoice and validates it again. The edit
// needs a new match before the invoice posts (F13): it changes the version
// the latest match was recorded at, and it unlinks an approved exception
// request, which was decided on the invoice as it was.
func (e *Endpoints) UpdateInvoice(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, ch Change) (Invoice, error) {
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
	if err := requireStatus(v, "change", Parked); err != nil {
		return Invoice{}, err
	}
	in := changed(v, ch)
	c, err := e.validate(ctx, tx, in)
	if err != nil {
		return Invoice{}, err
	}
	if err := e.ruleWrites().Check(ctx, ruleWrite("payables.supplier_invoice.update", id, in, c, v.Source)); err != nil {
		return Invoice{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payables.supplier_invoice SET supplier_reference = $2, invoice_date = $3, posting_date = $4, currency = $5,
			gross_amount = $6, tax_amount = $7, purchase_order_id = $8, extensions = $9, workflow_request_id = NULL, version = version + 1, updated_by = $10,
			reference_invoice_id = $11, extensions_schema = $12, tax_date = $13
		WHERE id = $1`,
		id, in.SupplierReference, in.InvoiceDate, in.PostingDate, c.currency, c.gross, c.tax, c.purchaseOrder, in.Extensions, p.ID, in.ReferenceInvoiceID,
		c.extensionsSchema, in.TaxDate); err != nil {
		return Invoice{}, fmt.Errorf("payables: update invoice %s: %w", id, err)
	}
	if err := putItems(ctx, tx, p.TenantID, id, c.items); err != nil {
		return Invoice{}, err
	}
	updated, err := load(ctx, tx, id, false)
	if err != nil {
		return Invoice{}, err
	}
	detail := map[string]any{"items": len(c.items)}
	if v.WorkflowRequestID != nil {
		detail["exception_unlinked"] = *v.WorkflowRequestID
	}
	return updated, record(ctx, "payables.supplier_invoice.update", id, detail)
}

// changed is the input of a parked invoice with a change applied.
func changed(v Invoice, ch Change) Input {
	in := Input{LegalEntityID: v.LegalEntityID, SupplierID: v.SupplierID, SupplierReference: v.SupplierReference, InvoiceDate: v.InvoiceDate,
		PostingDate: v.PostingDate, TaxDate: v.TaxDate, Gross: amountInput(v.Gross, v.Currency, v.Exponent), Tax: amountInput(v.Tax, v.Currency, v.Exponent),
		PurchaseOrderID: v.PurchaseOrderID, Extensions: mergepatch.Apply(v.Extensions, ch.Extensions), Kind: v.Kind, ReferenceInvoiceID: v.ReferenceInvoiceID}
	for _, it := range v.Items {
		in.Items = append(in.Items, ItemInput{PurchaseOrderID: it.PurchaseOrderID, POItemNo: it.POItemNo, Description: it.Description,
			Quantity: it.Quantity, Unit: it.Unit, Amount: amountInput(it.Amount, v.Currency, v.Exponent), TaxCode: it.TaxCode, AccountID: it.AccountID,
			CostCenterID: it.CostCenterID, ProfitCenterID: it.ProfitCenterID})
	}
	if ch.SupplierReference != nil {
		in.SupplierReference = *ch.SupplierReference
	}
	if ch.InvoiceDate != nil {
		in.InvoiceDate = *ch.InvoiceDate
	}
	if ch.PostingDate != nil {
		in.PostingDate = *ch.PostingDate
	}
	in.change, in.storedTaxDate = true, v.TaxDate
	if ch.TaxDate != nil {
		in.TaxDate = *ch.TaxDate
	}
	if ch.Gross != nil {
		in.Gross = *ch.Gross
	}
	if ch.Tax != nil {
		in.Tax = *ch.Tax
	}
	if ch.PurchaseOrderID != nil {
		in.PurchaseOrderID = *ch.PurchaseOrderID
	}
	if ch.ReferenceInvoiceID != nil {
		in.ReferenceInvoiceID = *ch.ReferenceInvoiceID
	}
	if ch.Items != nil {
		in.Items = ch.Items
	}
	return in
}

func requireStatus(v Invoice, action string, allowed ...string) error {
	for _, s := range allowed {
		if v.Status == s {
			return nil
		}
	}
	return problem.Conflict("payables.invalid_status", "The invoice is "+v.Status+"; "+action+" requires status "+joinStatuses(allowed)+".")
}

func joinStatuses(s []string) string {
	b, _ := json.Marshal(s) // strings always encode
	return string(b)
}

// transition moves an invoice to another status. holdReason is set with
// on_hold and cleared otherwise; request replaces workflow_request_id.
func transition(ctx context.Context, tx pgx.Tx, p reqctx.Principal, v Invoice, status string, holdReason *string, request *uuid.UUID) (Invoice, error) {
	if _, err := tx.Exec(ctx, `UPDATE payables.supplier_invoice SET status = $2, hold_reason = $3, workflow_request_id = $4, version = version + 1,
		updated_by = $5 WHERE id = $1`, v.ID, status, holdReason, request, p.ID); err != nil {
		return Invoice{}, fmt.Errorf("payables: move invoice %s to %s: %w", v.ID, status, err)
	}
	return load(ctx, tx, v.ID, false)
}

// HoldInvoice puts a parked invoice on hold.
func HoldInvoice(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, reason string) (Invoice, error) {
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
	if err := requireStatus(v, "hold", Parked); err != nil {
		return Invoice{}, err
	}
	held, err := transition(ctx, tx, p, v, OnHold, &reason, v.WorkflowRequestID)
	if err != nil {
		return Invoice{}, err
	}
	return held, record(ctx, "payables.supplier_invoice.hold", id, map[string]any{"reason": reason}, heldEvent(held))
}

func heldEvent(v Invoice) outbox.Event {
	return event(v, "invoice_held", map[string]any{"hold_reason": *v.HoldReason, "status": OnHold, "workflow_request_id": v.WorkflowRequestID})
}

// ReleaseInvoice returns an invoice on hold to parked; a pending exception
// request is withdrawn.
func (e *Endpoints) ReleaseInvoice(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, comment string) (Invoice, error) {
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
	if err := requireStatus(v, "release", OnHold); err != nil {
		return Invoice{}, err
	}
	if err := e.Approvals.Withdraw(ctx, approvals.InvoiceException, id, "Released by hand: "+comment); err != nil {
		return Invoice{}, err
	}
	released, err := transition(ctx, tx, p, v, Parked, nil, nil)
	if err != nil {
		return Invoice{}, err
	}
	return released, record(ctx, "payables.supplier_invoice.release", id, map[string]any{"comment": comment}, releasedEvent(released, nil))
}

func releasedEvent(v Invoice, request *uuid.UUID) outbox.Event {
	return event(v, "invoice_hold_released", map[string]any{"workflow_request_id": request, "previous_status": OnHold, "status": Parked})
}

// CancelInvoice cancels an unposted invoice; its supplier reference can be
// entered again.
func (e *Endpoints) CancelInvoice(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, reason string) (Invoice, error) {
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
	if err := requireStatus(v, "cancel", Parked, OnHold); err != nil {
		return Invoice{}, err
	}
	if err := e.Approvals.Withdraw(ctx, approvals.InvoiceException, id, "Invoice cancelled: "+reason); err != nil {
		return Invoice{}, err
	}
	cancelled, err := transition(ctx, tx, p, v, Cancelled, nil, v.WorkflowRequestID)
	if err != nil {
		return Invoice{}, err
	}
	return cancelled, record(ctx, "payables.supplier_invoice.cancel", id, map[string]any{"reason": reason},
		event(cancelled, "supplier_invoice_cancelled", map[string]any{"previous_status": v.Status, "status": Cancelled}))
}

// Approvals moves invoices on when their exception approval requests complete.
type Approvals struct{}

// decided loads the invoice an exception request was for; it must still be
// on hold.
func decided(ctx context.Context, id uuid.UUID) (pgx.Tx, reqctx.Principal, Invoice, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, reqctx.Principal{}, Invoice{}, err
	}
	v, err := load(ctx, tx, id, true)
	if err == nil {
		err = requireStatus(v, "an exception decision", OnHold)
	}
	return tx, p, v, err
}

// Approved releases the invoice from hold; the approved exception lets it
// be posted despite its match issues, as it now stands: the invoice records
// the version the release leaves it at (exception_version, F16), and any
// later change, even one that keeps the request linked, needs a new match
// or approval.
func (Approvals) Approved(ctx context.Context, invoice, request uuid.UUID) error {
	tx, p, v, err := decided(ctx, invoice)
	if err != nil {
		return err
	}
	// transition's release, recording the version it leaves the invoice at
	// in the same statement: every update moves the version on by one (the
	// optimistic lock trigger), and v is locked since decided.
	if _, err := tx.Exec(ctx, `UPDATE payables.supplier_invoice SET status = 'parked', hold_reason = NULL, workflow_request_id = $2,
		exception_version = $4, version = $4, updated_by = $3 WHERE id = $1`, invoice, request, p.ID, v.Version+1); err != nil {
		return fmt.Errorf("payables: release invoice %s on approved exception %s: %w", invoice, request, err)
	}
	released, err := load(ctx, tx, invoice, false)
	if err != nil {
		return err
	}
	return record(ctx, "payables.supplier_invoice.exception_approved", invoice, map[string]any{"workflow_request_id": request},
		releasedEvent(released, &request))
}

// Rejected rejects the invoice; its supplier reference can be entered again.
func (Approvals) Rejected(ctx context.Context, invoice, request uuid.UUID, comment string) error {
	tx, p, v, err := decided(ctx, invoice)
	if err != nil {
		return err
	}
	rejected, err := transition(ctx, tx, p, v, Rejected, nil, &request)
	if err != nil {
		return err
	}
	return record(ctx, "payables.supplier_invoice.reject", invoice, map[string]any{"workflow_request_id": request, "comment": comment},
		event(rejected, "supplier_invoice_rejected", map[string]any{"workflow_request_id": request, "previous_status": OnHold, "status": Rejected}))
}

// Cancelled leaves the invoice on hold without a pending exception request.
func (Approvals) Cancelled(ctx context.Context, invoice, request uuid.UUID, comment string) error {
	tx, p, v, err := decided(ctx, invoice)
	if err != nil {
		return err
	}
	if _, err := transition(ctx, tx, p, v, OnHold, v.HoldReason, nil); err != nil {
		return err
	}
	return record(ctx, "payables.supplier_invoice.exception_withdrawn", invoice, map[string]any{"workflow_request_id": request, "comment": comment})
}

// Invoices answers procurement's question whether a purchase order has
// invoices, in the request's transaction.
type Invoices struct{}

// HasInvoices reports whether an invoice that is not cancelled or rejected
// refers to the purchase order.
func (Invoices) HasInvoices(ctx context.Context, purchaseOrder uuid.UUID) (bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return false, err
	}
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM payables.supplier_invoice WHERE purchase_order_id = $1 AND status NOT IN ('cancelled', 'rejected'))`,
		purchaseOrder).Scan(&ok); err != nil {
		return false, fmt.Errorf("payables: check invoices of purchase order %s: %w", purchaseOrder, err)
	}
	return ok, nil
}
