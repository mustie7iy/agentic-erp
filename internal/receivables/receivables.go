// Package receivables is the accounts receivable context (ADR-0063;
// 02-architecture §2.2, 03-data-model §3.16 and §3.26): customer invoices
// entered without a sales order, as a header and its items by account,
// each item's output tax calculated at the invoice date (ADR-0049 §4, §7).
//
// A customer invoice is the accounting record of an invoice the company
// issued: not the legal tax invoice, and no e-invoice is sent. It is a
// draft when entered and nothing is posted; posting it (T75b, ADR-0063
// §5) debits the customer's reconciliation account, which is the
// customer's open item with its payment terms, and credits revenue and
// output tax. An incoming payment waiting on a house bank's incoming
// payments clearing account is applied to the customer's open items in
// full and exactly (T75c, ADR-0063 §6; incoming.go). A credit memo is the
// same document with the kind credit_memo, posted with the signs turned;
// one naming an invoice may not, with the other posted memos naming it,
// exceed that invoice's gross amount (T75d, ADR-0063 §7; kinds.go). The
// AR aging is computed on read from the ledger's customer open items
// (T75e, ADR-0063 §10; aging.go). A document's amounts are never
// negative: the kind gives their direction.
//
// The context reads the ledger (accounts, tax codes, assignments), the
// partner context (the customer by role and its company data), tenancy
// and the configuration through the ports below, as payables does; its
// financial effects go only through the ledger's posting port (03 §3.16).
package receivables

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// The statuses of a customer invoice: entered as a draft, posted once
// (ADR-0063 §2, §5).
const (
	Draft  = "draft"
	Posted = "posted"
)

// NumberRange is the document type customer invoices draw their numbers
// from (configuration number_ranges).
const NumberRange = "customer_invoice"

// customerRole is the partner role a customer plays (03 §3.6).
const customerRole = "customer"

// LegalEntities is what receivables reads about legal entities (tenancy
// context).
type LegalEntities interface {
	Entity(ctx context.Context, id uuid.UUID) (tenancy.Entity, bool, error)
}

// Customers says whether a business partner may be invoiced as a customer
// (partner context): the check by role the ledger makes of a line on a
// customer reconciliation account.
type Customers interface {
	PostingBlock(ctx context.Context, partnerID, legalEntity uuid.UUID, kind string) (string, error)
}

// CustomerData reads a customer's data in a company: the reconciliation
// account and payment term a receivables document posts with (partner
// context, T74a).
type CustomerData interface {
	Customer(ctx context.Context, partnerID, legalEntity uuid.UUID) (partner.Customer, bool, error)
}

// Terms reads a configured payment term (partner context).
type Terms interface {
	PaymentTerm(ctx context.Context, code string) (partner.PaymentTerm, bool, error)
}

// Postings is the ledger's posting port (03 §3.16): a customer invoice's
// entry, and the entry that applies a receipt to a customer's open items.
type Postings interface {
	Post(ctx context.Context, in ledger.Posting) (ledger.Entry, error)
	ApplyReceipt(ctx context.Context, in ledger.ReceiptApplication) (ledger.AppliedReceipt, error)
}

// Entries names journal entries by their document numbers and by their
// references (ledger context).
type Entries interface {
	Numbers(ctx context.Context, entries []uuid.UUID) (map[uuid.UUID]string, error)
	References(ctx context.Context, entries []uuid.UUID) (map[uuid.UUID]string, error)
}

// Receipts reads the receipts not yet applied on a company's incoming
// payments clearing accounts (ledger context).
type Receipts interface {
	Receipts(ctx context.Context, page *pagination.Keyset, legalEntity uuid.UUID, accounts []uuid.UUID, f ledger.ReceiptFilter) ([]ledger.Receipt, error)
}

// StatementLine is the bank statement line that posted a receipt: the
// statement's id and reference and the line's number, where a person
// reads who paid and what the remittance says (payments context).
type StatementLine struct {
	StatementID  uuid.UUID
	StatementRef string
	LineNo       int32
}

// Statements names the statement lines behind the journal entries
// (fiscal year and id) their postings posted, keyed by entry (payments context, which receivables
// does not import: the composition root wires it).
type Statements interface {
	Lines(ctx context.Context, entries []ledger.ItemRef) (map[uuid.UUID]StatementLine, error)
}

// Partners names business partners by number and name (partner context).
type Partners interface {
	Names(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]partner.Named, error)
}

// Accounts is what receivables asks the ledger about accounts, tax codes
// and assignments.
type Accounts interface {
	IsPostable(ctx context.Context, legalEntity, account uuid.UUID) (bool, error)
	TaxCode(ctx context.Context, legalEntity uuid.UUID, code string, date time.Time) (ledger.TaxCode, bool, error)
	CheckAssignments(ctx context.Context, legalEntity uuid.UUID, date time.Time, items []ledger.ItemAssignment, defaults bool) error
}

// References names the accounts and centers a document's items carry by
// id, and finds a company's accounts by the numbers the configuration
// names them by (ledger context).
type References interface {
	Accounts(ctx context.Context, legalEntity uuid.UUID, numbers []string) (map[string]uuid.UUID, error)
	AccountNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]ledger.AccountRef, error)
	CenterNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]ledger.CenterName, error)
}

// Numbers issues document numbers (configuration context).
type Numbers interface {
	Next(ctx context.Context, legalEntity uuid.UUID, documentType string, fiscalYear int16) (string, error)
}

// Calendar maps dates to fiscal years (ledger context).
type Calendar interface {
	FiscalYear(ctx context.Context, legalEntity uuid.UUID, date time.Time) (int16, error)
}

// Settings reads the active configuration: a company's tax rounding, its
// time zone and its house banks' incoming payments clearing accounts
// (configuration context).
type Settings interface {
	Document(ctx context.Context) (*config.Config, int32, error)
}

// Endpoints implements the receivables part of the generated API.
type Endpoints struct {
	Pages         *pagination.Codec
	LegalEntities LegalEntities
	Customers     Customers
	CustomerData  CustomerData
	Terms         Terms
	Partners      Partners
	Postings      Postings
	Entries       Entries
	Receipts      Receipts
	OpenItems     OpenItems
	Statements    Statements
	Accounts      Accounts
	References    References
	Numbers       Numbers
	Calendar      Calendar
	Settings      Settings
	// Now is the clock a posting without a date is dated by, in the
	// company's time zone; nil: the wall clock.
	Now func() time.Time
}

// Item is an item of a customer invoice: a net amount on an account and
// the tax its code gave when the invoice was entered. TaxJurisdiction is
// the subdivision that tax is levied in (the emirate of a UAE sale,
// AE-DU), named exactly where the code's component levies its lines'
// subdivision (ADR-0049 §7; ADR-0063 §4 As built): the item's lines in
// the ledger carry it, so the tax return reads the sale by emirate.
type Item struct {
	ItemNo          int32       `json:"item_no"`
	Description     *string     `json:"description"`
	AccountID       uuid.UUID   `json:"account_id"`
	Net             money.Minor `json:"net_amount"`
	TaxCode         *string     `json:"tax_code"`
	TaxJurisdiction *string     `json:"tax_jurisdiction"`
	Tax             money.Minor `json:"tax_amount"`
	CostCenterID    *uuid.UUID  `json:"cost_center_id"`
	ProfitCenterID  *uuid.UUID  `json:"profit_center_id"`
}

// Invoice is a row of receivables.customer_invoice with its items.
// Amounts are minor units of Currency, whose minor-unit digits are
// Exponent; Gross is Net and Tax.
type Invoice struct {
	ID             uuid.UUID
	DocumentNumber string
	LegalEntityID  uuid.UUID
	CustomerID     uuid.UUID
	Kind           string
	// ReferenceInvoiceID is the posted invoice a credit memo credits and
	// ReferenceInvoiceNumber its document number: nil on an invoice and on
	// a memo naming none.
	ReferenceInvoiceID     *uuid.UUID
	ReferenceInvoiceNumber *string
	InvoiceDate            time.Time
	PostingDate            *time.Time
	Currency               string
	Exponent               int
	Net, Tax, Gross        money.Minor
	Status                 string
	// PaymentTermCode is the payment term entered, nil for none; once
	// posted, the term the invoice posted with. NetDueDate is its open
	// item's net due date, and EntryYear and EntryID its journal entry:
	// nil until posted.
	PaymentTermCode *string
	NetDueDate      *time.Time
	EntryYear       *int16
	EntryID         *uuid.UUID
	Items           []Item
	Version         int32
	CreatedAt       time.Time
	CreatedBy       uuid.UUID
	UpdatedAt       time.Time
	UpdatedBy       uuid.UUID
}

// invoiceSelect reads invoices with their items by item number, so every
// read lists them in the order they were entered.
const invoiceSelect = `SELECT i.id, i.document_number, i.legal_entity_id, i.customer_id, i.kind, i.reference_invoice_id,
		(SELECT r.document_number FROM receivables.customer_invoice r WHERE r.tenant_id = i.tenant_id AND r.id = i.reference_invoice_id), i.invoice_date, i.posting_date, i.currency, c.exponent,
		i.net_amount, i.tax_amount, i.gross_amount, i.status, i.payment_term_code, i.net_due_date, i.journal_entry_year, i.journal_entry_id,
		coalesce((SELECT json_agg(json_build_object('item_no', t.item_no, 'description', t.description, 'account_id', t.account_id,
			'net_amount', t.net_amount, 'tax_code', t.tax_code, 'tax_jurisdiction', t.tax_jurisdiction, 'tax_amount', t.tax_amount,
			'cost_center_id', t.cost_center_id, 'profit_center_id', t.profit_center_id) ORDER BY t.item_no)
		 FROM receivables.customer_invoice_item t WHERE t.tenant_id = i.tenant_id AND t.customer_invoice_id = i.id), '[]'),
		i.version, i.created_at, i.created_by, i.updated_at, i.updated_by
	FROM receivables.customer_invoice i JOIN ref.currency c ON c.code = i.currency`

func scanInvoice(row pgx.Row) (Invoice, error) {
	var v Invoice
	err := row.Scan(&v.ID, &v.DocumentNumber, &v.LegalEntityID, &v.CustomerID, &v.Kind, &v.ReferenceInvoiceID, &v.ReferenceInvoiceNumber, &v.InvoiceDate, &v.PostingDate, &v.Currency, &v.Exponent,
		&v.Net, &v.Tax, &v.Gross, &v.Status, &v.PaymentTermCode, &v.NetDueDate, &v.EntryYear, &v.EntryID, &v.Items, &v.Version, &v.CreatedAt, &v.CreatedBy, &v.UpdatedAt, &v.UpdatedBy)
	return v, errs.Wrapf(err, "scan customer invoice")
}

// load reads an invoice whatever its company, forUpdate under its row lock
// (ADR-0008); get applies company access.
func load(ctx context.Context, tx pgx.Tx, id uuid.UUID, forUpdate bool) (Invoice, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE OF i"
	}
	v, err := scanInvoice(tx.QueryRow(ctx, invoiceSelect+` WHERE i.id = $1`+lock, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Invoice{}, problem.NotFoundOf("customer invoice")
	}
	if err != nil {
		return Invoice{}, fmt.Errorf("receivables: read customer invoice %s: %w", id, err)
	}
	return v, nil
}

// GetInvoice returns a customer invoice of a legal entity the caller may
// act in; another's is not found, so ids cannot be probed (04 §4.9).
func GetInvoice(ctx context.Context, id uuid.UUID) (Invoice, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Invoice{}, err
	}
	return get(ctx, tx, id, false)
}

// get reads an invoice of a legal entity the caller may act in, forUpdate
// under its row lock.
func get(ctx context.Context, tx pgx.Tx, id uuid.UUID, forUpdate bool) (Invoice, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Invoice{}, fmt.Errorf("receivables: company access for customer invoice %s: %w", id, err)
	}
	v, err := load(ctx, tx, id, forUpdate)
	if err == nil && !access.Allows(v.LegalEntityID) {
		return Invoice{}, problem.NotFoundOf("customer invoice")
	}
	return v, err
}
