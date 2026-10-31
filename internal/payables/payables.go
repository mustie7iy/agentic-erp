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
// (document type KR). On an order valued through GR/IR (T33), whose goods
// receipts post Cr GR/IR, an item on an order item debits GR/IR at the
// order price instead, with the price difference on its own line.
//
// Down payment requests (ADR-0037) are payables documents outside the
// ledger: a payment run pays them, which posts the special G/L item.
//
// Simulations of configuration changes replay the invoices of every
// company and are recorded by company; each reader of the change request is
// shown those of the companies they may access alone (Disclosure, ADR-0010).
package payables

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/rules"
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
// it draws numbers from the journal_kr number range. CreditDocumentType is
// that of credit memos and subsequent credits (SAP KG, journal_kg; T34).
const (
	DocumentType       = "KR"
	CreditDocumentType = "KG"
)

// matchHold starts the hold reason the match sets, so a later clean match
// releases its own holds and leaves manual ones alone.
const matchHold = "Three-way match: "

func init() {
	problem.RegisterConstraint("supplier_invoice_no_duplicates", func() *problem.Problem {
		return problem.Conflict("payables.duplicate_invoice", "This supplier reference is already entered for the supplier.")
	})
	problem.RegisterConstraint("supplier_invoice_amounts_not_negative", func() *problem.Problem {
		return problem.Unprocessable("payables.negative_amount", "Gross and tax amounts are not negative; a credit memo is entered as one (kind credit_memo).")
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
	PaymentTerm(ctx context.Context, code string) (partner.PaymentTerm, bool, error)
}

// Orders is what payables reads about purchase orders (procurement context).
type Orders interface {
	Order(ctx context.Context, id uuid.UUID) (procurement.Order, bool, error)
	// LockItems locks the order's items until the transaction ends: an
	// invoice posts under them (lockedOrder).
	LockItems(ctx context.Context, id uuid.UUID, items []int32) error
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
	ClearDownPayments(ctx context.Context, in ledger.DownPaymentClearing) (ledger.Entry, error)
}

// Accounts is what payables asks the ledger about accounts and tax codes.
type Accounts interface {
	Determination(ctx context.Context, legalEntity uuid.UUID, key string) (uuid.UUID, bool, error)
	IsPostable(ctx context.Context, legalEntity, account uuid.UUID) (bool, error)
	TaxCode(ctx context.Context, legalEntity uuid.UUID, code string, date time.Time) (ledger.TaxCode, bool, error)
	TaxCodes(ctx context.Context, legalEntity uuid.UUID, date time.Time) ([]ledger.TaxCode, error)
	WithholdingTaxCode(ctx context.Context, legalEntity uuid.UUID, code string) (ledger.WithholdingTaxCode, bool, error)
	CheckAssignments(ctx context.Context, legalEntity uuid.UUID, date time.Time, items []ledger.ItemAssignment, defaults bool) error
}

// Numbers issues document numbers (configuration context).
type Numbers interface {
	Next(ctx context.Context, legalEntity uuid.UUID, documentType string, fiscalYear int16) (string, error)
}

// Calendar maps dates to fiscal years (ledger context).
type Calendar interface {
	FiscalYear(ctx context.Context, legalEntity uuid.UUID, date time.Time) (int16, error)
}

// Rates converts an invoice's amounts into its company's currency, the
// currency of the configured tolerances and limits (ledger context, F12).
type Rates interface {
	Conversion(ctx context.Context, from, to string, date time.Time) (ledger.Conversion, error)
}

// Settings reads the active configuration: tolerances and limits, and the
// time zone a company's date is told in (configuration context).
type Settings interface {
	Document(ctx context.Context) (*config.Config, int32, error)
}

// Opener opens and withdraws approval requests (workflow context).
type Opener interface {
	Open(ctx context.Context, in approvals.NewRequest) (uuid.UUID, bool, error)
	Withdraw(ctx context.Context, objectType string, objectID uuid.UUID, comment string) error
}

// Payments says whether an entry's open item is in a live payment run
// (payments context); asking holds off the legal entity's proposals until
// the transaction ends.
type Payments interface {
	RunOf(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16, entryID uuid.UUID) (string, bool, error)
	// RequestRunOf is the live run that pays a down payment request.
	RequestRunOf(ctx context.Context, legalEntity, request uuid.UUID) (string, bool, error)
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
	Schemas       extensions.Schemas    // the tenant's custom field schemas (configuration context)
	References    extensions.References // what custom fields refer to (partner context)
	Rules         rules.Source          // the tenant's validation rules (configuration context)
	Rates         Rates
	Approvals     Opener
	Payments      Payments
	OrderNumbers  OrderNumbers      // resolves the order number a captured document prints
	Extractor     capture.Extractor // reads captured documents; nil: capture is not available
	Objects       blobstore.Store   // keeps captured documents; nil: only their hash is recorded, and downloads answer 503
	Uploads       *Uploads          // direct uploads of large documents; nil or without a store: uploads answer 503
	// Now is the clock a posting date the caller leaves out is read from,
	// as the company's date in its time zone (companyDay); nil is the
	// system clock.
	Now func() time.Time
}

// Item is a live invoice item.
type Item struct {
	ItemNo          int32       `json:"item_no"`
	PurchaseOrderID *uuid.UUID  `json:"purchase_order_id"`
	POItemNo        *int32      `json:"po_item_no"`
	Description     *string     `json:"description"`
	Quantity        string      `json:"quantity"`
	Unit            string      `json:"unit"`
	Amount          money.Minor `json:"amount"`
	TaxCode         *string     `json:"tax_code"`
	AccountID       *uuid.UUID  `json:"account_id"`
	// The item's assignment (ADR-0050 §6, T56e): as entered on an item
	// without an order, its order item's cost center on one with.
	CostCenterID   *uuid.UUID `json:"cost_center_id"`
	ProfitCenterID *uuid.UUID `json:"profit_center_id"`
}

// Invoice is a row of payables.supplier_invoice with its live items.
// Amounts are minor units of Currency, whose minor-unit digits are Exponent.
type Invoice struct {
	ID                 uuid.UUID
	DocumentNumber     string
	LegalEntityID      uuid.UUID
	SupplierID         uuid.UUID
	SupplierReference  string
	InvoiceDate        time.Time
	PostingDate        *time.Time
	TaxDate            *time.Time // set by a person (P6); nil: the invoice date, or the corrected invoice's tax date (taxDateOf)
	Currency           string
	Exponent           int
	Gross, Tax         money.Minor
	PurchaseOrderID    *uuid.UUID
	Status             string
	HoldReason         *string
	Source             string
	CaptureSHA256      []byte
	CaptureObjectRef   *string // where the captured document is kept, or sha256:<hex> for captures before object storage
	JournalEntryYear   *int16
	JournalEntryID     *uuid.UUID
	ReversalEntryYear  *int16
	ReversalEntryID    *uuid.UUID
	WorkflowRequestID  *uuid.UUID
	Extensions         map[string]any
	ExtensionsSchema   *int       // the custom field schema version the extensions were written under (ADR-0046)
	Kind               string     // KindInvoice, KindCreditMemo, KindSubsequentDebit or KindSubsequentCredit (T34)
	ReferenceInvoiceID *uuid.UUID // the posted invoice a correction names
	// Terms are the payment terms the invoice posted with (T36b), or nil.
	Terms     *ledger.ItemTerms
	Items     []Item
	Version   int32
	CreatedAt time.Time
	CreatedBy uuid.UUID
	UpdatedAt time.Time
	UpdatedBy uuid.UUID
}

const invoiceSelect = `SELECT i.id, i.document_number, i.legal_entity_id, i.supplier_id, i.supplier_reference, i.invoice_date, i.posting_date,
		i.currency, c.exponent, i.gross_amount, i.tax_amount, i.purchase_order_id, i.status, i.hold_reason, i.source, i.capture_sha256, i.capture_object_ref,
		i.journal_entry_year, i.journal_entry_id, i.reversal_entry_year, i.reversal_entry_id, i.workflow_request_id, i.extensions, i.extensions_schema, i.kind, i.reference_invoice_id,
		i.tax_date,
		coalesce((SELECT json_agg(json_build_object('item_no', t.item_no, 'purchase_order_id', t.purchase_order_id, 'po_item_no', t.po_item_no,
			'description', t.description, 'quantity', trim_scale(t.quantity)::text, 'unit', t.uom_code, 'amount', t.amount, 'tax_code', t.tax_code,
			'account_id', t.account_id, 'cost_center_id', t.cost_center_id, 'profit_center_id', t.profit_center_id) ORDER BY t.item_no)
		 FROM payables.supplier_invoice_item t WHERE t.tenant_id = i.tenant_id AND t.supplier_invoice_id = i.id AND NOT t.is_removed), '[]'),
		` + termsColumns + `, i.version, i.created_at, i.created_by, i.updated_at, i.updated_by
	FROM payables.supplier_invoice i JOIN ref.currency c ON c.code = i.currency
	LEFT JOIN payables.supplier_invoice_terms st ON st.tenant_id = i.tenant_id AND st.supplier_invoice_id = i.id`

func scanInvoice(row pgx.Row) (Invoice, error) {
	var v Invoice
	var terms storedTerms
	err := row.Scan(append(append([]any{&v.ID, &v.DocumentNumber, &v.LegalEntityID, &v.SupplierID, &v.SupplierReference, &v.InvoiceDate,
		&v.PostingDate, &v.Currency, &v.Exponent, &v.Gross, &v.Tax, &v.PurchaseOrderID, &v.Status, &v.HoldReason, &v.Source, &v.CaptureSHA256,
		&v.CaptureObjectRef, &v.JournalEntryYear, &v.JournalEntryID, &v.ReversalEntryYear, &v.ReversalEntryID, &v.WorkflowRequestID, &v.Extensions,
		&v.ExtensionsSchema, &v.Kind, &v.ReferenceInvoiceID, &v.TaxDate, &v.Items}, terms.fields()...), &v.Version, &v.CreatedAt, &v.CreatedBy, &v.UpdatedAt, &v.UpdatedBy)...)
	v.Terms = terms.terms()
	return v, errs.Wrapf(err, "scan invoice")
}

func notFound() error { return problem.NotFoundOf("supplier invoice") }

// load reads an invoice without company access, for the workflow context's
// callbacks; lock takes the row lock for a change.
func load(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Invoice, error) {
	if lock {
		if _, err := tx.Exec(ctx, `SELECT FROM payables.supplier_invoice WHERE id = $1 FOR UPDATE`, id); err != nil {
			return Invoice{}, fmt.Errorf("payables: lock invoice %s: %w", id, err)
		}
	}
	v, err := scanInvoice(tx.QueryRow(ctx, invoiceSelect+` WHERE i.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Invoice{}, notFound()
	}
	if err != nil {
		return Invoice{}, fmt.Errorf("payables: read invoice %s: %w", id, err)
	}
	return v, nil
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
	// CostCenterID and ProfitCenterID are the assignment of an item
	// without an order; one on an order item is its order item's.
	CostCenterID, ProfitCenterID *uuid.UUID
}

// Input is an invoice as a client sends it, for entry and edits.
type Input struct {
	LegalEntityID     uuid.UUID
	SupplierID        uuid.UUID
	SupplierReference string
	InvoiceDate       time.Time
	PostingDate       *time.Time
	// TaxDate is set by a person only (decision P6): an agent's is refused,
	// but for one a change keeps (taxDateSet).
	TaxDate         *time.Time
	change          bool       // the input is a parked invoice's with a change applied
	storedTaxDate   *time.Time // a change's invoice's tax date
	Gross, Tax      Amount
	PurchaseOrderID *uuid.UUID
	Items           []ItemInput
	Extensions      map[string]any
	// Kind is KindInvoice (also when empty), KindCreditMemo,
	// KindSubsequentDebit or KindSubsequentCredit; ReferenceInvoiceID the
	// posted invoice a correction names (T34).
	Kind               string
	ReferenceInvoiceID *uuid.UUID
	// held: the extensions were checked already, as a captured document's
	// are (CheckHeld), against the schema version extensionsSchema.
	held             bool
	extensionsSchema *int
	// incomplete: a captured invoice lacks required custom fields, which
	// its rules read; they run when a person completes it (ADR-0046).
	incomplete bool
}

// checked is an input that passed validation.
type checked struct {
	currency      string
	gross, tax    money.Minor
	purchaseOrder *uuid.UUID
	items         []Item
	kind          string
	// extensionsSchema is the custom field schema version the extensions
	// were checked against, written with the invoice.
	extensionsSchema *int
}

// extensions checks a supplier invoice's custom fields (ADR-0046).
func (e *Endpoints) extensions() extensions.Writes {
	return extensions.Writes{Entity: "supplier_invoice", Schemas: e.Schemas, References: e.References}
}

// ruleWrites evaluates a supplier invoice's validation rules (ADR-0046).
func (e *Endpoints) ruleWrites() rules.Writes {
	return rules.Writes{Entity: "supplier_invoice", Source: e.Rules}
}

// ruleWrite is a checked invoice as its rules read it: the header of its
// create request, amounts in minor units too.
func ruleWrite(action string, id uuid.UUID, in Input, c checked, source string) rules.Write {
	row := map[string]any{"legal_entity_id": in.LegalEntityID.String(), "supplier_id": in.SupplierID.String(),
		"supplier_reference": in.SupplierReference, "invoice_date": in.InvoiceDate.Format(time.DateOnly),
		"gross_amount": rules.Money{Amount: in.Gross.Value, Currency: c.currency, Minor: c.gross},
		"tax_amount":   rules.Money{Amount: in.Tax.Value, Currency: c.currency, Minor: c.tax},
		"posting_date": rules.Date(in.PostingDate), "purchase_order_id": rules.ID(c.purchaseOrder), "kind": c.kind,
		"reference_invoice_id": rules.ID(in.ReferenceInvoiceID), "source": source}
	return rules.Write{Action: action, ID: id, Row: row, Extensions: in.Extensions}
}

// storedRuleWrite is a stored invoice's header as its rules read it, with
// a change applied.
func storedRuleWrite(action string, v Invoice, ch Change) rules.Write {
	return ruleWrite(action, v.ID, changed(v, ch),
		checked{currency: v.Currency, gross: v.Gross, tax: v.Tax, purchaseOrder: v.PurchaseOrderID, kind: v.Kind}, v.Source)
}

// RuleSamples reads the recent supplier invoices on which a simulation
// replays proposed rules (rules.Sampler, ADR-0046): those changed since
// the time given, newest first, read as their rules read them.
type RuleSamples struct{}

// Sample implements rules.Sampler.
func (RuleSamples) Sample(ctx context.Context, since time.Time, limit int) ([]rules.Sample, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, invoiceSelect+` WHERE i.updated_at >= $1 ORDER BY i.updated_at DESC, i.id LIMIT $2`, since, limit)
	vs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Invoice, error) { return scanInvoice(r) })
	if err != nil {
		return nil, fmt.Errorf("payables: sample invoices for rules: %w", err)
	}
	out := make([]rules.Sample, len(vs))
	for i, v := range vs {
		w := storedRuleWrite("", v, Change{})
		// Only a parked or held invoice is written again (changed, released, posted).
		historical := v.Status != Parked && v.Status != OnHold
		out[i] = rules.Sample{ID: v.ID, Reference: v.DocumentNumber, Row: w.Row, Extensions: w.Extensions, Historical: historical}
	}
	return out, nil
}

func invalid(code, field, message string) error {
	return problem.Unprocessable(code, message, problem.FieldError{Field: field, Code: "invalid", Message: message})
}

// validate checks an invoice for entry: a legal entity the caller may act
// in, a usable supplier, amounts in one currency with its minor-unit
// digits, items on the order or on postable accounts, known tax codes.
func (e *Endpoints) validate(ctx context.Context, tx pgx.Tx, in Input) (checked, error) {
	le, sup, err := e.invoiceParties(ctx, in)
	if err != nil {
		return checked{}, err
	}
	schema := in.extensionsSchema
	if !in.held {
		if schema, err = e.extensions().Check(ctx, in.Extensions); err != nil {
			return checked{}, err
		}
	}
	c := checked{currency: in.Gross.Currency, purchaseOrder: in.PurchaseOrderID, extensionsSchema: schema}
	exp, err := currencyExponent(ctx, tx, c.currency, "gross_amount.currency")
	if err != nil {
		return checked{}, err
	}
	v := &invoiceCheck{e: e, ctx: ctx, tx: tx, in: in, le: le, sup: sup, exp: exp, c: &c, orders: map[uuid.UUID]procurement.Order{}}
	if c.gross, err = v.parse(in.Gross, "gross_amount"); err != nil {
		return checked{}, err
	}
	if c.tax, err = v.parse(in.Tax, "tax_amount"); err != nil {
		return checked{}, err
	}
	if err := notNegative(c.gross, c.tax); err != nil {
		return checked{}, err
	}
	if err := v.kind(); err != nil {
		return checked{}, err
	}
	if v.taxDate, err = e.validTaxDate(ctx, tx, in); err != nil {
		return checked{}, err
	}
	if c.purchaseOrder != nil {
		if _, err := v.order(*c.purchaseOrder, "purchase_order_id"); err != nil {
			return checked{}, err
		}
	}
	for i, it := range in.Items {
		item, err := v.item(i, it)
		if err != nil {
			return checked{}, err
		}
		c.items = append(c.items, item)
	}
	return c, v.assignments()
}

// assignments checks the centers of the items without an order against
// the ledger as the posting will on the invoice's posting date, else its
// invoice date (ADR-0050 §6): an item without a cost center takes its
// account's default. The posting checks them again; an item on an order
// has its order item's, checked as the order was released.
func (v *invoiceCheck) assignments() error {
	var items []ledger.ItemAssignment
	for i, it := range v.c.items {
		if it.PurchaseOrderID == nil {
			items = append(items, ledger.ItemAssignment{Index: i, AccountID: *it.AccountID, CostCenterID: it.CostCenterID, ProfitCenterID: it.ProfitCenterID})
		}
	}
	if len(items) == 0 {
		return nil
	}
	date := v.in.InvoiceDate
	if v.in.PostingDate != nil {
		date = *v.in.PostingDate
	}
	return v.e.Accounts.CheckAssignments(v.ctx, v.le.ID, date, items, true)
}

// invoiceParties resolves the legal entity, which the caller must be allowed
// to act in, and the supplier, which must be usable in it.
func (e *Endpoints) invoiceParties(ctx context.Context, in Input) (tenancy.Entity, partner.Supplier, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return tenancy.Entity{}, partner.Supplier{}, err
	}
	le, err := tenancy.Accessible(ctx, e.LegalEntities, access, in.LegalEntityID, "legal_entity_id")
	if err != nil {
		return tenancy.Entity{}, partner.Supplier{}, err
	}
	sup, ok, err := e.Suppliers.Supplier(ctx, in.SupplierID, le.ID)
	if err != nil {
		return tenancy.Entity{}, partner.Supplier{}, err
	}
	if !ok {
		return tenancy.Entity{}, partner.Supplier{}, problem.ReferenceNotFound("supplier_id", "business partner", "no business partner with this id")
	}
	if why := sup.Usable(); why != "" {
		return tenancy.Entity{}, partner.Supplier{}, invalid("payables.supplier_not_usable", "supplier_id", "The supplier cannot be invoiced: "+why+".")
	}
	return le, sup, nil
}

// notNegative refuses a negative gross or tax amount: the amounts are not
// negative whatever the kind, which gives the direction (T34).
func notNegative(gross, tax money.Minor) error {
	for _, a := range []struct {
		field  string
		amount money.Minor
	}{{"gross_amount", gross}, {"tax_amount", tax}} {
		if field := a.field; a.amount < 0 {
			return problem.Unprocessable("payables.negative_amount", "Gross and tax amounts are not negative; a credit memo is entered as one (kind credit_memo).",
				problem.FieldError{Field: field + ".amount", Code: "negative", Message: "an amount of 0 or more"})
		}
	}
	return nil
}

// currencyExponent reads the minor-unit digits of a document's currency;
// field names it in the refusal of an unknown one.
func currencyExponent(ctx context.Context, tx pgx.Tx, code, field string) (int, error) {
	exp, err := currency.Exponent(ctx, tx, code, invalid("payables.unknown_currency", field, "The currency is not known."))
	if err != nil {
		return 0, fmt.Errorf("payables: currency %s: %w", code, err)
	}
	return exp, nil
}

// invoiceCheck carries what validate has established so far into the
// per-amount and per-item checks. c.purchaseOrder is fixed by the header or
// the first item on an order, so later items are checked against it, and
// orders caches each order so it is read once per validation.
type invoiceCheck struct {
	e      *Endpoints
	ctx    context.Context
	tx     pgx.Tx
	in     Input
	le     tenancy.Entity
	sup    partner.Supplier
	exp    int
	c      *checked
	orders map[uuid.UUID]procurement.Order
	// taxDate is the date the items' tax codes are checked on (taxDateOf).
	taxDate time.Time
}

// parse reads an amount, which must be in the invoice currency with its
// minor-unit digits.
func (v *invoiceCheck) parse(a Amount, field string) (money.Minor, error) {
	if a.Currency != v.c.currency {
		return 0, invalid("payables.currency_mismatch", field+".currency", "Every amount of an invoice is in one currency, "+v.c.currency+".")
	}
	n, err := money.ParseAmount(a.Value, v.exp)
	if err != nil {
		return 0, problem.BadRequest("request.invalid_amount", fmt.Sprintf("Amounts in %s have exactly %d decimals.", v.c.currency, v.exp),
			problem.FieldError{Field: field + ".amount", Code: "invalid", Message: err.Error()})
	}
	return n, nil
}

// order reads a purchase order and checks that it is a released order of
// this supplier and legal entity in the invoice currency.
func (v *invoiceCheck) order(id uuid.UUID, field string) (procurement.Order, error) {
	if o, ok := v.orders[id]; ok {
		return o, nil
	}
	o, ok, err := v.e.Orders.Order(v.ctx, id)
	if err != nil {
		return o, err
	}
	if !ok || o.Status != "released" || o.SupplierID != v.sup.ID || o.LegalEntityID != v.le.ID || o.Currency != v.c.currency {
		return o, invalid("payables.invalid_purchase_order", field,
			"The purchase order must be a released order of this supplier and legal entity in the invoice currency.")
	}
	v.orders[id] = o
	return o, nil
}

// item checks one invoice item: a positive quantity, an amount in the
// invoice currency, an order item or a postable account, a known tax code.
func (v *invoiceCheck) item(i int, it ItemInput) (Item, error) {
	field := fmt.Sprintf("items[%d]", i)
	item := Item{ItemNo: int32(i + 1), Description: it.Description, Quantity: it.Quantity, Unit: it.Unit, TaxCode: it.TaxCode, AccountID: it.AccountID,
		CostCenterID: it.CostCenterID, ProfitCenterID: it.ProfitCenterID}
	// A subsequent debit's or credit's item names its order item first:
	// what it is decides what its quantity may be.
	if subsequent(v.c.kind) && it.POItemNo == nil {
		return Item{}, invalid("payables.po_item_required", field+".po_item_no", "A subsequent debit or credit revalues order items: each item names one.")
	}
	if err := v.quantity(field, it.Quantity); err != nil {
		return Item{}, err
	}
	var err error
	if item.Amount, err = v.parse(it.Amount, field+".amount"); err != nil {
		return Item{}, err
	}
	if err := v.itemTarget(field, it, &item); err != nil {
		return Item{}, err
	}
	if err := v.taxCode(field, item.TaxCode); err != nil {
		return Item{}, err
	}
	return item, nil
}

// itemTarget checks what an item is booked against: an item of the
// invoice's one purchase order, or else an account postable in the legal
// entity.
func (v *invoiceCheck) itemTarget(field string, it ItemInput, item *Item) error {
	po := it.PurchaseOrderID
	if po == nil && it.POItemNo != nil {
		po = v.c.purchaseOrder
	}
	switch {
	case po != nil && it.POItemNo == nil:
		return invalid("payables.po_item_required", field+".po_item_no", "An item on a purchase order names the order item.")
	case po == nil && it.POItemNo != nil:
		return invalid("payables.purchase_order_required", field+".purchase_order_id", "An order item needs its purchase order.")
	case po != nil:
		return v.orderItem(field, po, it, item)
	case it.AccountID == nil:
		return invalid("payables.account_required", field+".account_id", "An item without a purchase order names its account.")
	default:
		ok, err := v.e.Accounts.IsPostable(v.ctx, v.le.ID, *it.AccountID)
		if err != nil {
			return err
		}
		if !ok {
			return invalid("payables.account_not_postable", field+".account_id", "The account cannot be posted in this legal entity.")
		}
		return nil
	}
}

// orderItem links an item to its purchase order item, which must exist in
// the same unit; the order item's account and tax code fill those the
// invoice item leaves open.
func (v *invoiceCheck) orderItem(field string, po *uuid.UUID, it ItemInput, item *Item) error {
	if v.c.purchaseOrder != nil && *v.c.purchaseOrder != *po {
		return invalid("payables.mixed_purchase_orders", field+".purchase_order_id", "An invoice is for one purchase order.")
	}
	v.c.purchaseOrder = po
	o, err := v.order(*po, field+".purchase_order_id")
	if err != nil {
		return err
	}
	var oi *procurement.OrderItem
	for j := range o.Items {
		if o.Items[j].ItemNo == *it.POItemNo {
			oi = &o.Items[j]
		}
	}
	if oi == nil {
		return invalid("payables.unknown_po_item", field+".po_item_no", fmt.Sprintf("Order %s has no item %d.", o.DocumentNumber, *it.POItemNo))
	}
	if oi.Unit != it.Unit {
		return invalid("payables.unit_mismatch", field+".quantity.unit", "The order item is in "+oi.Unit+".")
	}
	item.PurchaseOrderID, item.POItemNo = po, it.POItemNo
	if item.AccountID == nil {
		item.AccountID = &oi.AccountID
	}
	if err := fromOrder(field, o, *oi, it); err != nil {
		return err
	}
	item.CostCenterID = oi.CostCenterID
	if item.TaxCode == nil {
		item.TaxCode = oi.TaxCode
	}
	return nil
}

// fromOrder refuses an assignment an item on order item oi gives: it
// takes the order item's, checked as its order was released (T56e). A
// stored item's copy of the order item's cost center passes; an order
// item has no profit center of its own.
func fromOrder(field string, o procurement.Order, oi procurement.OrderItem, it ItemInput) error {
	switch {
	case it.CostCenterID != nil && (oi.CostCenterID == nil || *it.CostCenterID != *oi.CostCenterID):
		field += ".cost_center_id"
	case it.ProfitCenterID != nil:
		field += ".profit_center_id"
	default:
		return nil
	}
	return invalid("payables.assignment_from_order", field,
		fmt.Sprintf("An item on order %s takes its order item's account assignment; leave its cost and profit center out.", o.DocumentNumber))
}

// taxCode checks that an item's tax code, if any, is an input tax code
// valid in the legal entity on the invoice date, of components an invoice
// can post.
func (v *invoiceCheck) taxCode(field string, code *string) error {
	if code == nil {
		return nil
	}
	t, ok, err := v.e.Accounts.TaxCode(v.ctx, v.le.ID, *code, v.taxDate)
	if err != nil {
		return err
	}
	if !ok || t.Direction != config.TaxInput {
		return invalid("payables.unknown_tax_code", field+".tax_code", "Tax code "+*code+" is not an input tax code of the legal entity on the tax date "+
			v.taxDate.Format(time.DateOnly)+".")
	}
	return nil
}

// componentsUnsupported refuses a tax code with several components, a
// non-deductible one or a partly recoverable one: down payments post one
// tax line on one account (their tax is a share of the amount paid).
func componentsUnsupported(field, code string) error {
	return invalid("payables.tax_code_components", field, "Tax code "+code+" has components an invoice or down payment cannot post yet "+
		"(several, non-deductible or partly recoverable; T61d).")
}

// putItems makes the invoice's live items the given ones: existing item
// numbers are updated, new ones inserted and the rest marked removed.
func putItems(ctx context.Context, tx pgx.Tx, tenant, invoice uuid.UUID, items []Item) error {
	n := len(items)
	nos, pos, poItems, accounts := make([]int32, n), make([]*uuid.UUID, n), make([]*int32, n), make([]*uuid.UUID, n)
	descriptions, quantities, units, taxes := make([]*string, n), make([]string, n), make([]string, n), make([]*string, n)
	amounts, costCenters, profitCenters := make([]money.Minor, n), make([]*uuid.UUID, n), make([]*uuid.UUID, n)
	for i, it := range items {
		nos[i], pos[i], poItems[i], accounts[i] = it.ItemNo, it.PurchaseOrderID, it.POItemNo, it.AccountID
		descriptions[i], quantities[i], units[i], taxes[i], amounts[i] = it.Description, it.Quantity, it.Unit, it.TaxCode, it.Amount
		costCenters[i], profitCenters[i] = it.CostCenterID, it.ProfitCenterID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO payables.supplier_invoice_item AS t (tenant_id, supplier_invoice_id, item_no, purchase_order_id, po_item_no,
			description, quantity, uom_code, amount, tax_code, account_id, cost_center_id, profit_center_id)
		SELECT $1, $2, n, po, pi, d, q::numeric, u, a, tc, ac, cc, pc
		FROM unnest($3::int[], $4::uuid[], $5::int[], $6::text[], $7::text[], $8::text[], $9::bigint[], $10::text[], $11::uuid[], $12::uuid[], $13::uuid[])
			AS x(n, po, pi, d, q, u, a, tc, ac, cc, pc)
		ON CONFLICT (tenant_id, supplier_invoice_id, item_no) DO UPDATE SET purchase_order_id = EXCLUDED.purchase_order_id,
			po_item_no = EXCLUDED.po_item_no, description = EXCLUDED.description, quantity = EXCLUDED.quantity, uom_code = EXCLUDED.uom_code,
			amount = EXCLUDED.amount, tax_code = EXCLUDED.tax_code, account_id = EXCLUDED.account_id, cost_center_id = EXCLUDED.cost_center_id,
			profit_center_id = EXCLUDED.profit_center_id, is_removed = false`,
		tenant, invoice, nos, pos, poItems, descriptions, quantities, units, amounts, taxes, accounts, costCenters, profitCenters); err != nil {
		return fmt.Errorf("payables: write items of invoice %s: %w", invoice, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE payables.supplier_invoice_item SET is_removed = true WHERE supplier_invoice_id = $1 AND item_no > $2 AND NOT is_removed`,
		invoice, n); err != nil {
		return fmt.Errorf("payables: remove items of invoice %s: %w", invoice, err)
	}
	return nil
}

// record publishes the events and adds the audit entry of a change.
func record(ctx context.Context, action string, id uuid.UUID, detail map[string]any, events ...outbox.Event) error {
	return audit.Record(ctx, action, "payables.supplier_invoice", id, detail, events...)
}

func event(v Invoice, name string, payload map[string]any) outbox.Event {
	payload["supplier_invoice_id"], payload["supplier_id"], payload["legal_entity_id"] = v.ID, v.SupplierID, v.LegalEntityID
	return outbox.Event{Context: "payables", Name: name, AggregateType: "payables.supplier_invoice", AggregateID: v.ID, Payload: payload}
}

// Count is how many of the invoices exist, by kind; a kind none of them
// is has no entry.
func (Invoices) Count(ctx context.Context, ids []uuid.UUID) (map[string]int, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT kind, count(*) FROM payables.supplier_invoice WHERE id = ANY($1) GROUP BY kind`, ids)
	out := map[string]int{}
	var kind string
	var n int
	if _, err := pgx.ForEachRow(rows, []any{&kind, &n}, func() error { out[kind] = n; return nil }); err != nil {
		return nil, fmt.Errorf("payables: count %d invoices by kind: %w", len(ids), err)
	}
	return out, nil
}
