// Package integration is the integration context (03-data-model §3.13):
// connectors to external systems and migration runs that move SAP data
// onto the platform one scope (legal entities and a fiscal year) at a time,
// proven by reconciliation (docs/migration.md).
//
// A migration run reads the scope from its connector (an SAP extract file
// or SAP's OData APIs, internal/sap), checks it, and loads it through the
// other contexts' application services in the run's transaction, never into
// their tables: suppliers through partner, purchase orders and goods
// receipts through procurement, parked invoices through payables, journal
// entries and clearings through the ledger. Every loaded record is mapped
// to its SAP key in integration.external_ref, so a later run of an
// overlapping scope skips what is already there. Reconciliation recomputes
// the controls from the SAP data and from the platform independently. A
// rehearsal loads and reconciles in a savepoint that is rolled back, so
// only its report remains.
package integration

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/egress"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// LegalEntities is what integration reads about legal entities (tenancy).
type LegalEntities interface {
	Entity(ctx context.Context, id uuid.UUID) (tenancy.Entity, bool, error)
}

// Accounts reads a legal entity's chart of accounts (ledger).
type Accounts interface {
	Chart(ctx context.Context, legalEntity uuid.UUID) (map[string]ledger.ChartAccount, error)
}

// Settings reads the active configuration: payment terms and number
// ranges (configuration). Hold keeps it from changing until the run's
// transaction ends.
type Settings interface {
	Document(ctx context.Context) (*config.Config, int32, error)
	Hold(ctx context.Context) error
}

// Partners creates and changes suppliers (partner).
type Partners interface {
	Create(ctx context.Context, in partner.NewPartner) (uuid.UUID, error)
	AddCompanies(ctx context.Context, id uuid.UUID, companies []partner.SupplierCompanyInput, reason string) error
	Block(ctx context.Context, id uuid.UUID, reason string) error
}

// Suppliers reads suppliers and, for the reconciliation, their payments
// bank accounts (partner).
type Suppliers interface {
	Supplier(ctx context.Context, partnerID, legalEntity uuid.UUID) (partner.Supplier, bool, error)
	PaymentAccounts(ctx context.Context, partners []uuid.UUID) (map[uuid.UUID]partner.PaymentAccount, error)
}

// Orders creates purchase orders and records goods receipts, whose value
// the migrated journal carries (procurement).
type Orders interface {
	CreateOrder(ctx context.Context, in procurement.NewOrder) (procurement.PurchaseOrder, error)
	ReleaseLoadedOrder(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, valuation string) (procurement.PurchaseOrder, error)
	PostLoadedReceipt(ctx context.Context, in procurement.ReceiptInput) (procurement.GoodsReceipt, error)
}

// OrderLocks locks a purchase order's items until the transaction ends,
// in item order, as a goods movement locks them (procurement): the load
// takes every item lock its receipts need before the first of them posts.
type OrderLocks interface {
	LockItems(ctx context.Context, id uuid.UUID, items []int32) error
}

// OrderValues reads purchase order totals, and an order an earlier run
// loaded, whose valuation says whether its GR/IR positions are topped up
// (procurement).
type OrderValues interface {
	Value(ctx context.Context, ids []uuid.UUID) (int, money.Minor, error)
	Order(ctx context.Context, id uuid.UUID) (procurement.Order, bool, error)
}

// Invoices enters supplier invoices, and takes over what SAP's posted
// invoice documents invoiced on order items and SAP's open down payments
// (payables).
type Invoices interface {
	CreateInvoice(ctx context.Context, in payables.Input, source string) (payables.Invoice, error)
	TakeOverInvoiced(ctx context.Context, in payables.MigratedInvoice) (uuid.UUID, error)
	TakeOverDownPayment(ctx context.Context, in payables.MigratedDownPayment) (payables.DownPaymentRequest, error)
}

// InvoiceCounts counts supplier invoices by kind (payables).
type InvoiceCounts interface {
	Count(ctx context.Context, ids []uuid.UUID) (map[string]int, error)
}

// InvoicedQuantities reads the quantities migrated documents invoiced on
// order items, by order and item (payables).
type InvoicedQuantities interface {
	MigratedQuantities(ctx context.Context, orders []uuid.UUID) (map[uuid.UUID]map[int32]string, error)
}

// Postings posts journal entries, clears migrated open items and reads
// what earlier runs took over of GR/IR positions (ledger).
type Postings interface {
	Post(ctx context.Context, in ledger.Posting) (ledger.Entry, error)
	TakenOverGRIR(ctx context.Context, legalEntity uuid.UUID, orders []uuid.UUID) (map[uuid.UUID]ledger.GRIRTakenOverOrder, error)
	Clear(ctx context.Context, legalEntity uuid.UUID, date time.Time, by ledger.ItemRef, items []ledger.ItemRef) (uuid.UUID, error)
	EntryYears(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]int16, error)
	EntryCurrencies(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error)
	ClearedTogether(ctx context.Context, items []ledger.ItemRef) (bool, error)
	LockAllEntries(ctx context.Context) error
}

// Reports reads ledger totals for reconciliation (ledger).
type Reports interface {
	Totals(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16) (ledger.Totals, error)
	OpenPayables(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16) (map[uuid.UUID]money.Minor, error)
	OpenPayableItems(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16) (map[uuid.UUID]int, error)
	OpenDownPayments(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16) (map[uuid.UUID]money.Minor, error)
	OpenForeignItems(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16) (map[uuid.UUID]map[string]money.Minor, error)
	OpenGRIR(ctx context.Context, legalEntity uuid.UUID, orders []uuid.UUID) (map[uuid.UUID]map[int32]ledger.OpenGRIR, error)
}

// Secrets resolves a connector's secret reference (env:, file: or vault:;
// internal/platform/secrets).
type Secrets interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// Requesters refuses a run's deferred work acting as a requester no longer
// active (identity, PO 2026-11-03).
type Requesters interface {
	MayAct(ctx context.Context, tx pgx.Tx, id uuid.UUID) error
}

// Endpoints implements the integration part of the generated API.
type Endpoints struct {
	Pages         *pagination.Codec
	LegalEntities LegalEntities
	Accounts      Accounts
	Settings      Settings
	Partners      Partners
	Suppliers     Suppliers
	Orders        Orders
	OrderLocks    OrderLocks
	OrderValues   OrderValues
	Invoices      Invoices
	InvoiceCounts InvoiceCounts
	Invoiced      InvoicedQuantities
	Postings      Postings
	Reports       Reports
	Centers       Centers
	Secrets       Secrets
	Requesters    Requesters       // the workflow's Execute refuses a requester no longer active
	Keys          Sealer           // seals webhook secrets with the tenant's key
	Resolver      egress.Resolver  // resolves webhook hosts when a URL is written
	ExtractDir    string           // sap_file connectors read extracts below it; empty: they cannot be used
	HTTP          *http.Client     // for sap_odata connectors; nil: the reader's default
	Now           func() time.Time // the clock replay and ping read, the dispatcher's where both are wired; nil: the wall clock
	Workflows     bool             // runs leave the request and run as workflows (ADR-0012); false: they complete within it
	RefuseEBICS   bool             // production: bank_ebics connectors cannot be created or enabled (ADR-0038, T44d)
}

// record publishes the events and adds the audit entry of a change; the
// action names the object type (integration.connector.create).
func record(ctx context.Context, action string, id uuid.UUID, detail map[string]any, events ...outbox.Event) error {
	return audit.Record(ctx, action, audit.ObjectTypeOf(action), id, detail, events...)
}

func invalid(code, field, message string) error {
	return problem.Unprocessable(code, message, problem.FieldError{Field: field, Code: "invalid", Message: message})
}

func noRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
