// Package payables is the accounts payable context (03-data-model §3.9):
// supplier invoices entered by hand or captured from documents, the
// three-way match against purchase order and goods receipts, exceptions
// decided through the workflow context, posting to the ledger and
// reversal.
//
// An invoice is parked when entered. PO-based invoices are matched; a
// match with issues puts the invoice on hold and opens an
// invoice_exception approval request, whose approval returns the invoice to
// parked and allows posting despite the issues. Posting books Dr expense
// per item, Dr input tax, Cr the supplier's reconciliation account
// (document type KR); goods receipts do not post (no GR/IR clearing until
// inventory valuation arrives).
package payables

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Invoice statuses.
const (
	Parked    = "parked"
	OnHold    = "on_hold"
	Posted    = "posted"
	Rejected  = "rejected"
	Cancelled = "cancelled"
	Reversed  = "reversed"
)

// DocumentType is the journal document type of invoice postings (SAP KR);
// it draws numbers from the journal_kr number range.
const DocumentType = "KR"

// matchHold starts the hold reason the match sets, so a later clean match
// releases its own holds and leaves manual ones alone.
const matchHold = "Three-way match: "

func init() {
	problem.RegisterConstraint("supplier_invoice_no_duplicates", func() *problem.Problem {
		return problem.Conflict("payables.duplicate_invoice", "This supplier reference is already entered for the supplier.")
	})
	problem.RegisterConstraint("supplier_invoice_capture_once", func() *problem.Problem {
		return problem.Conflict("payables.already_captured", "This document has already been captured.")
	})
	problem.RegisterConstraint("supplier_invoice_item_uom_code_fkey", func() *problem.Problem {
		return problem.Unprocessable("payables.unknown_unit", "An item's unit of measure is not known.",
			problem.FieldError{Field: "items.quantity.unit", Code: "unknown", Message: "a unit of measure code from the reference data"})
	})
}

// LegalEntities is what payables reads about legal entities (tenancy context).
type LegalEntities interface {
	Entity(ctx context.Context, id uuid.UUID) (tenancy.Entity, bool, error)
}

// Suppliers is what payables reads about suppliers (partner context).
type Suppliers interface {
	Supplier(ctx context.Context, partnerID, legalEntity uuid.UUID) (partner.Supplier, bool, error)
}

// Orders is what payables reads about purchase orders (procurement context).
type Orders interface {
	Order(ctx context.Context, id uuid.UUID) (procurement.Order, bool, error)
}

// OrderNumbers finds purchase orders by document number (procurement
// context), for documents that print the order number.
type OrderNumbers interface {
	OrderByNumber(ctx context.Context, legalEntity uuid.UUID, number string) (uuid.UUID, bool, error)
}

// Postings posts and reverses invoice entries (ledger context).
type Postings interface {
	Post(ctx context.Context, in ledger.Posting) (ledger.Entry, error)
	Reverse(ctx context.Context, entryID uuid.UUID, postingDate *time.Time, headerText *string) (ledger.Entry, error)
	IsCleared(ctx context.Context, fiscalYear int16, entryID uuid.UUID) (bool, error)
}

// Accounts is what payables asks the ledger about accounts and tax codes.
type Accounts interface {
	Determination(ctx context.Context, legalEntity uuid.UUID, key string) (uuid.UUID, bool, error)
	IsPostable(ctx context.Context, legalEntity, account uuid.UUID) (bool, error)
	TaxCode(ctx context.Context, legalEntity uuid.UUID, code string, date time.Time) (ledger.TaxCode, bool, error)
}

// Numbers issues document numbers (configuration context).
type Numbers interface {
	Next(ctx context.Context, legalEntity uuid.UUID, documentType string, fiscalYear int16) (string, error)
}

// Calendar maps dates to fiscal years (ledger context).
type Calendar interface {
	FiscalYear(ctx context.Context, legalEntity uuid.UUID, date time.Time) (int16, error)
}

// Settings reads the active configuration: tolerances and limits
// (configuration context).
type Settings interface {
	Document(ctx context.Context) (*config.Config, int32, error)
}

// Opener opens and withdraws approval requests (workflow context).
type Opener interface {
	Open(ctx context.Context, in approvals.NewRequest) (uuid.UUID, bool, error)
	Withdraw(ctx context.Context, objectType string, objectID uuid.UUID, comment string) error
}

// Payments says whether an entry's open item is in a live payment run
// (payments context).
type Payments interface {
	RunOf(ctx context.Context, fiscalYear int16, entryID uuid.UUID) (string, bool, error)
}

// Endpoints implements the payables part of the generated API.
type Endpoints struct {
	Pages         *pagination.Codec
	LegalEntities LegalEntities
	Suppliers     Suppliers
	Orders        Orders
	Postings      Postings
	Accounts      Accounts
	Numbers       Numbers
	Calendar      Calendar
	Settings      Settings
	Approvals     Opener
	Payments      Payments
	OrderNumbers  OrderNumbers      // resolves the order number a captured document prints
	Extractor     capture.Extractor // reads captured documents; nil: capture is not available
}

// Item is a live invoice item.
type Item struct {
	ItemNo          int32      `json:"item_no"`
	PurchaseOrderID *uuid.UUID `json:"purchase_order_id"`
	POItemNo        *int32     `json:"po_item_no"`
	Description     *string    `json:"description"`
	Quantity        string     `json:"quantity"`
	Unit            string     `json:"unit"`
	Amount          int64      `json:"amount"`
	TaxCode         *string    `json:"tax_code"`
	AccountID       *uuid.UUID `json:"account_id"`
}

// Invoice is a row of payables.supplier_invoice with its live items.
// Amounts are minor units of Currency, whose minor-unit digits are Exponent.
type Invoice struct {
	ID                uuid.UUID
	DocumentNumber    string
	LegalEntityID     uuid.UUID
	SupplierID        uuid.UUID
	SupplierReference string
	InvoiceDate       time.Time
	PostingDate       *time.Time
	Currency          string
	Exponent          int
	Gross, Tax        int64
	PurchaseOrderID   *uuid.UUID
	Status            string
	HoldReason        *string
	Source            string
	CaptureSHA256     []byte
	JournalEntryYear  *int16
	JournalEntryID    *uuid.UUID
	ReversalEntryYear *int16
	ReversalEntryID   *uuid.UUID
	WorkflowRequestID *uuid.UUID
	Extensions        map[string]any
	Items             []Item
	Version           int32
	CreatedAt         time.Time
	CreatedBy         uuid.UUID
	UpdatedAt         time.Time
	UpdatedBy         uuid.UUID
}

const invoiceSelect = `SELECT i.id, i.document_number, i.legal_entity_id, i.supplier_id, i.supplier_reference, i.invoice_date, i.posting_date,
		i.currency, c.exponent, i.gross_amount, i.tax_amount, i.purchase_order_id, i.status, i.hold_reason, i.source, i.capture_sha256,
		i.journal_entry_year, i.journal_entry_id, i.reversal_entry_year, i.reversal_entry_id, i.workflow_request_id, i.extensions,
		coalesce((SELECT json_agg(json_build_object('item_no', t.item_no, 'purchase_order_id', t.purchase_order_id, 'po_item_no', t.po_item_no,
			'description', t.description, 'quantity', trim_scale(t.quantity)::text, 'unit', t.uom_code, 'amount', t.amount, 'tax_code', t.tax_code,
			'account_id', t.account_id) ORDER BY t.item_no)
		 FROM payables.supplier_invoice_item t WHERE t.tenant_id = i.tenant_id AND t.supplier_invoice_id = i.id AND NOT t.is_removed), '[]'),
		i.version, i.created_at, i.created_by, i.updated_at, i.updated_by
	FROM payables.supplier_invoice i JOIN ref.currency c ON c.code = i.currency`

func scanInvoice(row pgx.Row) (Invoice, error) {
	var v Invoice
	err := row.Scan(&v.ID, &v.DocumentNumber, &v.LegalEntityID, &v.SupplierID, &v.SupplierReference, &v.InvoiceDate, &v.PostingDate,
		&v.Currency, &v.Exponent, &v.Gross, &v.Tax, &v.PurchaseOrderID, &v.Status, &v.HoldReason, &v.Source, &v.CaptureSHA256,
		&v.JournalEntryYear, &v.JournalEntryID, &v.ReversalEntryYear, &v.ReversalEntryID, &v.WorkflowRequestID, &v.Extensions,
		&v.Items, &v.Version, &v.CreatedAt, &v.CreatedBy, &v.UpdatedAt, &v.UpdatedBy)
	return v, err
}

func notFound() error { return problem.NotFound("No supplier invoice with this id.") }

// load reads an invoice without company access, for the workflow context's
// callbacks; lock takes the row lock for a change.
func load(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Invoice, error) {
	if lock {
		if _, err := tx.Exec(ctx, `SELECT FROM payables.supplier_invoice WHERE id = $1 FOR UPDATE`, id); err != nil {
			return Invoice{}, err
		}
	}
	v, err := scanInvoice(tx.QueryRow(ctx, invoiceSelect+` WHERE i.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Invoice{}, notFound()
	}
	return v, err
}

// get reads an invoice of a legal entity the caller may act in.
func get(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Invoice, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Invoice{}, err
	}
	v, err := load(ctx, tx, id, lock)
	if err == nil && !access.Allows(v.LegalEntityID) {
		return Invoice{}, notFound()
	}
	return v, err
}

// GetInvoice returns a supplier invoice.
func GetInvoice(ctx context.Context, id uuid.UUID) (Invoice, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Invoice{}, err
	}
	return get(ctx, tx, id, false)
}

// Amount is an API amount: a decimal string and its currency.
type Amount struct{ Value, Currency string }

// ItemInput is an invoice item as a client sends it.
type ItemInput struct {
	PurchaseOrderID *uuid.UUID
	POItemNo        *int32
	Description     *string
	Quantity, Unit  string
	Amount          Amount
	TaxCode         *string
	AccountID       *uuid.UUID
}

// Input is an invoice as a client sends it, for entry and edits.
type Input struct {
	LegalEntityID     uuid.UUID
	SupplierID        uuid.UUID
	SupplierReference string
	InvoiceDate       time.Time
	PostingDate       *time.Time
	Gross, Tax        Amount
	PurchaseOrderID   *uuid.UUID
	Items             []ItemInput
	Extensions        map[string]any
}

// checked is an input that passed validation.
type checked struct {
	currency      string
	gross, tax    int64
	purchaseOrder *uuid.UUID
	items         []Item
}

func invalid(code, field, message string) error {
	return problem.Unprocessable(code, message, problem.FieldError{Field: field, Code: "invalid", Message: message})
}

var extensionKey = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// validate checks an invoice for entry: a legal entity the caller may act
// in, a usable supplier, amounts in one currency with its minor-unit
// digits, items on the order or on postable accounts, known tax codes.
func (e *Endpoints) validate(ctx context.Context, tx pgx.Tx, in Input) (checked, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return checked{}, err
	}
	le, ok, err := e.LegalEntities.Entity(ctx, in.LegalEntityID)
	if err != nil {
		return checked{}, err
	}
	if !ok || !access.Allows(le.ID) {
		return checked{}, problem.Unprocessable("request.reference_not_found", "No legal entity with this id.",
			problem.FieldError{Field: "legal_entity_id", Code: "not_found", Message: "no legal entity with this id that you can access"})
	}
	sup, ok, err := e.Suppliers.Supplier(ctx, in.SupplierID, le.ID)
	if err != nil {
		return checked{}, err
	}
	if !ok {
		return checked{}, problem.Unprocessable("request.reference_not_found", "No business partner with this id.",
			problem.FieldError{Field: "supplier_id", Code: "not_found", Message: "no business partner with this id"})
	}
	if why := sup.Usable(); why != "" {
		return checked{}, invalid("payables.supplier_not_usable", "supplier_id", "The supplier cannot be invoiced: "+why+".")
	}
	for k := range in.Extensions {
		if !extensionKey.MatchString(k) {
			return checked{}, problem.Unprocessable("request.invalid_extension", "Custom field keys are snake_case.",
				problem.FieldError{Field: "extensions." + k, Code: "invalid_key", Message: "snake_case, starting with a letter"})
		}
	}
	c := checked{currency: in.Gross.Currency, purchaseOrder: in.PurchaseOrderID}
	var exp int
	err = tx.QueryRow(ctx, `SELECT exponent FROM ref.currency WHERE code = $1`, c.currency).Scan(&exp)
	if errors.Is(err, pgx.ErrNoRows) {
		return checked{}, invalid("payables.unknown_currency", "gross_amount.currency", "The currency is not known.")
	}
	if err != nil {
		return checked{}, err
	}
	parse := func(a Amount, field string) (int64, error) {
		if a.Currency != c.currency {
			return 0, invalid("payables.currency_mismatch", field+".currency", "Every amount of an invoice is in one currency, "+c.currency+".")
		}
		v, err := money.ParseAmount(a.Value, exp)
		if err != nil {
			return 0, problem.BadRequest("request.invalid_amount", fmt.Sprintf("Amounts in %s have exactly %d decimals.", c.currency, exp),
				problem.FieldError{Field: field + ".amount", Code: "invalid", Message: err.Error()})
		}
		return int64(v), nil
	}
	if c.gross, err = parse(in.Gross, "gross_amount"); err != nil {
		return checked{}, err
	}
	if c.tax, err = parse(in.Tax, "tax_amount"); err != nil {
		return checked{}, err
	}
	orders := map[uuid.UUID]procurement.Order{}
	order := func(id uuid.UUID, field string) (procurement.Order, error) {
		if o, ok := orders[id]; ok {
			return o, nil
		}
		o, ok, err := e.Orders.Order(ctx, id)
		if err != nil {
			return o, err
		}
		if !ok || o.Status != "released" || o.SupplierID != sup.ID || o.LegalEntityID != le.ID || o.Currency != c.currency {
			return o, invalid("payables.invalid_purchase_order", field,
				"The purchase order must be a released order of this supplier and legal entity in the invoice currency.")
		}
		orders[id] = o
		return o, nil
	}
	if c.purchaseOrder != nil {
		if _, err := order(*c.purchaseOrder, "purchase_order_id"); err != nil {
			return checked{}, err
		}
	}
	for i, it := range in.Items {
		field := fmt.Sprintf("items[%d]", i)
		item := Item{ItemNo: int32(i + 1), Description: it.Description, Quantity: it.Quantity, Unit: it.Unit, TaxCode: it.TaxCode, AccountID: it.AccountID}
		if q, ok := new(big.Rat).SetString(it.Quantity); !ok || q.Sign() <= 0 {
			return checked{}, invalid("payables.invalid_quantity", field+".quantity.value", "Quantities are positive.")
		}
		if item.Amount, err = parse(it.Amount, field+".amount"); err != nil {
			return checked{}, err
		}
		po := it.PurchaseOrderID
		if po == nil && it.POItemNo != nil {
			po = c.purchaseOrder
		}
		switch {
		case po != nil && it.POItemNo == nil:
			return checked{}, invalid("payables.po_item_required", field+".po_item_no", "An item on a purchase order names the order item.")
		case po == nil && it.POItemNo != nil:
			return checked{}, invalid("payables.purchase_order_required", field+".purchase_order_id", "An order item needs its purchase order.")
		case po != nil:
			if c.purchaseOrder != nil && *c.purchaseOrder != *po {
				return checked{}, invalid("payables.mixed_purchase_orders", field+".purchase_order_id", "An invoice is for one purchase order.")
			}
			c.purchaseOrder = po
			o, err := order(*po, field+".purchase_order_id")
			if err != nil {
				return checked{}, err
			}
			var oi *procurement.OrderItem
			for j := range o.Items {
				if o.Items[j].ItemNo == *it.POItemNo {
					oi = &o.Items[j]
				}
			}
			if oi == nil {
				return checked{}, invalid("payables.unknown_po_item", field+".po_item_no", fmt.Sprintf("Order %s has no item %d.", o.DocumentNumber, *it.POItemNo))
			}
			if oi.Unit != it.Unit {
				return checked{}, invalid("payables.unit_mismatch", field+".quantity.unit", "The order item is in "+oi.Unit+".")
			}
			item.PurchaseOrderID, item.POItemNo = po, it.POItemNo
			if item.AccountID == nil {
				item.AccountID = &oi.AccountID
			}
			if item.TaxCode == nil {
				item.TaxCode = oi.TaxCode
			}
		case it.AccountID == nil:
			return checked{}, invalid("payables.account_required", field+".account_id", "An item without a purchase order names its account.")
		default:
			ok, err := e.Accounts.IsPostable(ctx, le.ID, *it.AccountID)
			if err != nil {
				return checked{}, err
			}
			if !ok {
				return checked{}, invalid("payables.account_not_postable", field+".account_id", "The account cannot be posted in this legal entity.")
			}
		}
		if item.TaxCode != nil {
			_, ok, err := e.Accounts.TaxCode(ctx, le.ID, *item.TaxCode, in.InvoiceDate)
			if err != nil {
				return checked{}, err
			}
			if !ok {
				return checked{}, invalid("payables.unknown_tax_code", field+".tax_code", "Tax code "+*item.TaxCode+" is not valid in the legal entity on the invoice date.")
			}
		}
		c.items = append(c.items, item)
	}
	return c, nil
}

// putItems makes the invoice's live items the given ones: existing item
// numbers are updated, new ones inserted and the rest marked removed.
func putItems(ctx context.Context, tx pgx.Tx, tenant, invoice uuid.UUID, items []Item) error {
	n := len(items)
	nos, pos, poItems, accounts := make([]int32, n), make([]*uuid.UUID, n), make([]*int32, n), make([]*uuid.UUID, n)
	descriptions, quantities, units, taxes := make([]*string, n), make([]string, n), make([]string, n), make([]*string, n)
	amounts := make([]int64, n)
	for i, it := range items {
		nos[i], pos[i], poItems[i], accounts[i] = it.ItemNo, it.PurchaseOrderID, it.POItemNo, it.AccountID
		descriptions[i], quantities[i], units[i], taxes[i], amounts[i] = it.Description, it.Quantity, it.Unit, it.TaxCode, it.Amount
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payables.supplier_invoice_item AS t (tenant_id, supplier_invoice_id, item_no, purchase_order_id, po_item_no,
			description, quantity, uom_code, amount, tax_code, account_id)
		SELECT $1, $2, n, po, pi, d, q::numeric, u, a, tc, ac
		FROM unnest($3::int[], $4::uuid[], $5::int[], $6::text[], $7::text[], $8::text[], $9::bigint[], $10::text[], $11::uuid[])
			AS x(n, po, pi, d, q, u, a, tc, ac)
		ON CONFLICT (tenant_id, supplier_invoice_id, item_no) DO UPDATE SET purchase_order_id = EXCLUDED.purchase_order_id,
			po_item_no = EXCLUDED.po_item_no, description = EXCLUDED.description, quantity = EXCLUDED.quantity, uom_code = EXCLUDED.uom_code,
			amount = EXCLUDED.amount, tax_code = EXCLUDED.tax_code, account_id = EXCLUDED.account_id, is_removed = false`,
		tenant, invoice, nos, pos, poItems, descriptions, quantities, units, amounts, taxes, accounts); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE payables.supplier_invoice_item SET is_removed = true WHERE supplier_invoice_id = $1 AND item_no > $2 AND NOT is_removed`,
		invoice, n)
	return err
}

// record publishes the events and adds the audit entry of a change.
func record(ctx context.Context, action string, id uuid.UUID, detail map[string]any, events ...outbox.Event) error {
	for _, ev := range events {
		if _, err := outbox.Add(ctx, ev); err != nil {
			return err
		}
	}
	return audit.Add(ctx, audit.Entry{Action: action, ObjectType: "payables.supplier_invoice", ObjectID: id, Outcome: "succeeded", Detail: detail})
}

func event(v Invoice, name string, payload map[string]any) outbox.Event {
	payload["supplier_invoice_id"], payload["supplier_id"], payload["legal_entity_id"] = v.ID, v.SupplierID, v.LegalEntityID
	return outbox.Event{Context: "payables", Name: name, AggregateType: "payables.supplier_invoice", AggregateID: v.ID, Payload: payload}
}

func amount(minor int64, currency string, exp int) map[string]string {
	return map[string]string{"amount": money.Minor(minor).Plain(exp), "currency": currency}
}

func today(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var d time.Time
	err := tx.QueryRow(ctx, `SELECT current_date`).Scan(&d)
	return d, err
}

// Count is how many of the invoices exist.
func (Invoices) Count(ctx context.Context, ids []uuid.UUID) (int, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return 0, err
	}
	var n int
	err = tx.QueryRow(ctx, `SELECT count(*) FROM payables.supplier_invoice WHERE id = ANY($1)`, ids).Scan(&n)
	return n, err
}
