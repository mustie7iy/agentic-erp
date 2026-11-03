// Package assets is the asset accounting context (ADR-0057; SAP FI-AA):
// asset classes, assets, their movements, acquisitions, retirements,
// transfers and depreciation runs. It is a leaf (ADR-0057 §2): it reads
// the other contexts only through ports it declares, which the
// composition root (internal/app) adapts and which run in the request's
// transaction under row-level security (ADR-0003), and no context imports
// it (internal/layout).
//
// T57a builds its classes: the asset classes and each company's asset
// accounts are configuration (ADR-0057 §3, internal/config), validated
// there and in ledger.Setup, which makes the acquisition cost and
// accumulated depreciation accounts asset reconciliation accounts that
// only this context posts to. ListAssetClasses reads them from the active
// document.
//
// T57b builds the asset master (ADR-0057 §5, §7, §8): assets numbered from
// their company's asset range, with depreciation terms per ledger the
// company keeps, copied from their class and changeable until depreciation
// is posted, and their cost and profit center as insert-only assignment
// rows, the profit center derived from the cost center (ADR-0050 §5).
// Every change is audited (ADR-0011) and published as AssetChanged through
// the context's outbox (ADR-0007); a change reads the asset FOR UPDATE and
// checks the caller's version (ADR-0008); amounts are minor units
// (ADR-0005); the schema is the tenant's under RLS (ADR-0003). The ports
// below are the leaf's to the ledger, tenancy, configuration and
// operations contexts, assets.Runs (a depreciation run's Operation, T57f)
// among them.
//
// T57c builds acquisitions from posted lines on an asset clearing account
// (ADR-0057 §4, §5, §6): a line, read under its clearing_use lock, is
// capitalised in full or in parts onto assets through the ledger's
// posting API in every ledger the company keeps, each part keeping its
// local and group amounts (ledger.PostingLine.GC), with the assets'
// movements, AssetMovementPosted and the worklist of the lines.
//
// T57d builds acquisitions by manual entry against an offset account and
// the reversal of acquisitions (ADR-0057 §6): a reversal reverses the
// acquisition's entries through the ledger's reversal path, negates its
// movements, gives the amount back to its line and derives the asset's
// status again, refused while a retirement or transfer posted after it
// stands (last posted, first reversed); a reversed line's reversal line
// is capitalised as a reduction onto the assets holding the reversed line,
// each at most what it holds, and published as AssetMovementReversed.
//
// T57e builds the depreciation schedule (ADR-0057 §7): Walk computes an
// asset's planned depreciation per period from its terms and movements in
// a ledger, in minor units, so the schedule read, a run's preview and the
// run (T57f) never disagree with the movements; the movements are listed
// as the asset's subledger.
//
// T57f builds the depreciation run (ADR-0057 §4, §7): a run of a company,
// ledger and regular period is queued with its Operation and carried out
// by the workflow DepreciationRun through the workflows.Depreciation
// port, which these endpoints implement: the plan writes the selected
// assets in batches of 500, each batch posts its assets' differences as
// one entry in the run's ledger alone (ledger.Posting.LedgerID), with a
// depreciation movement per asset, under the assets' locks, and the run
// ends posted or failed, published as DepreciationRunCompleted.
package assets

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Settings reads the active configuration (configuration context), whose
// asset classes and asset accounts the context reads.
type Settings interface {
	Document(ctx context.Context) (*config.Config, int32, error)
}

// Company is a legal entity as the assets context reads it: its company
// code, name and local currency.
type Company struct {
	ID                   uuid.UUID
	Code, Name, Currency string
}

// Companies reads legal entities (tenancy context): the one of a company
// code, and the one of an id, ok false when there is none; and the
// tenant's group currency.
type Companies interface {
	ByCode(ctx context.Context, code string) (Company, bool, error)
	Company(ctx context.Context, id uuid.UUID) (Company, bool, error)
	GroupCurrency(ctx context.Context) (string, error)
}

// LedgerRef is a ledger by its code and name.
type LedgerRef struct {
	ID         uuid.UUID
	Code, Name string
}

// Ledger reads and posts in the ledger context: the tenant's ledgers
// (leading first, then by code), those a company keeps when legalEntity is
// given; the fiscal year and the regular period a company's date falls in
// (422 ledger.no_fiscal_period outside every one) and the regular periods
// of a fiscal year in order (the same problem for a year not set up); a
// company's accounts by
// number; a journal line and the leading ledger's lines on accounts in a
// year (ADR-0057 §6); an entry posted to every ledger the company keeps
// through the ledger's posting API, and the reversal of such an entry
// (its posting group's first) on a date, their entries returned per
// ledger; an account a manual acquisition may credit in a company (ok
// false for any other); accounts' numbers and names by id; whether a
// regular period is open in a ledger (409 ledger.period_not_open
// otherwise); and amounts of a local currency in the group currency on a
// date, as a posting dated then translates them (ADR-0057 §7, T57f).
type Ledger interface {
	Ledgers(ctx context.Context, legalEntity *uuid.UUID) ([]LedgerRef, error)
	FiscalYear(ctx context.Context, legalEntity uuid.UUID, date time.Time) (int16, error)
	Period(ctx context.Context, legalEntity uuid.UUID, date time.Time) (Period, error)
	Periods(ctx context.Context, legalEntity uuid.UUID, year int16) ([]Period, error)
	Accounts(ctx context.Context, legalEntity uuid.UUID, numbers []string) (map[string]uuid.UUID, error)
	Line(ctx context.Context, entry uuid.UUID, lineNo int32) (Line, bool, error)
	ClearingLines(ctx context.Context, q ClearingQuery) ([]Line, error)
	Post(ctx context.Context, p Posting) ([]PostedEntry, error)
	Reverse(ctx context.Context, entry uuid.UUID, postingDate time.Time) ([]PostedEntry, error)
	OffsetAccount(ctx context.Context, legalEntity, account uuid.UUID) (AccountRef, bool, error)
	AccountNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]AccountRef, error)
	PeriodOpen(ctx context.Context, legalEntity, ledger uuid.UUID, year, period int16) error
	GroupAmounts(ctx context.Context, from, to string, date time.Time, amounts []money.Minor) ([]money.Minor, error)
}

// AccountRef is a G/L account by its number and name.
type AccountRef struct {
	ID           uuid.UUID
	Number, Name string
}

// Period is a regular posting period with its dates.
type Period struct {
	FiscalYear, Number int16
	Start, End         time.Time
}

// LineRef names a journal line.
type LineRef struct {
	FiscalYear int16
	EntryID    uuid.UUID
	LineNo     int32
}

// Line is a journal line as an acquisition reads its source (ADR-0057
// §6): amounts debit positive in its entry's currency, local and group
// currency, whether its entry is the leading ledger's and has a copy in
// every ledger the company keeps, the entry it reverses and the entry
// that reversed it. A reversal keeps its original's lines in order, so
// line n of a reversal reverses line n of its original.
type Line struct {
	LineRef
	LegalEntityID, AccountID         uuid.UUID
	Currency, CurrencyLC, CurrencyGC string
	Amount, LC, GC                   money.Minor
	PostingDate                      time.Time
	DocumentNumber, SourceContext    string
	SourceDocumentID, ProfitCenterID *uuid.UUID
	Leading, InEveryLedger           bool
	Reverses, ReversedBy             *ReversalEntry
}

// ReversalEntry is the other entry of a reversal pair: the entry a
// reversal reverses, or the reversal of a reversed entry.
type ReversalEntry struct {
	FiscalYear     int16
	EntryID        uuid.UUID
	DocumentNumber string
	PostingDate    time.Time
}

// ClearingQuery selects the leading ledger's lines on Accounts of a
// company in a fiscal year by (posting date, entry, line) after After, at
// most Limit, leaving out the lines of entries ExceptContext posted, of
// the other lines those Skip names, and the lines of reversed entries and
// reversals that Keep does not name (Skip binds only the others, Keep
// only these).
type ClearingQuery struct {
	LegalEntityID uuid.UUID
	Accounts      []uuid.UUID
	ExceptContext string
	FiscalYear    int16
	After         *ClearingKey
	Limit         int
	Skip, Keep    []LineRef
}

// ClearingKey is a line's place in the worklist's order.
type ClearingKey struct {
	PostingDate time.Time `json:"d"`
	EntryID     uuid.UUID `json:"e"`
	LineNo      int32     `json:"l"`
}

// Posting is an entry an assets document posts to every ledger its
// company keeps (ADR-0057 §4, §6), or to the one ledger LedgerID names (a
// depreciation run's, §7).
type Posting struct {
	LegalEntityID             uuid.UUID
	LedgerID                  *uuid.UUID
	DocumentType              string
	PostingDate, DocumentDate time.Time
	Currency                  string
	HeaderText                *string
	DocumentID                uuid.UUID
	Lines                     []PostingLine
}

// PostingLine is a line in the posting's currency, debit positive, with
// the local and group amounts it keeps (PostingLine.LC and GC; both nil:
// converted at the posting date's rate, as a manual acquisition's lines
// are) and its cost and profit center.
type PostingLine struct {
	AccountID                    uuid.UUID
	Amount                       money.Minor
	LC, GC                       *money.Minor
	CostCenterID, ProfitCenterID *uuid.UUID
	Text                         *string
}

// PostedEntry is one ledger's entry of a posting.
type PostedEntry struct {
	ID, LedgerID   uuid.UUID
	FiscalYear     int16
	Period         int16
	DocumentNumber string
}

// InvoiceSource is a supplier invoice as an acquisition names its source
// (ADR-0057 §2): its number, the supplier's reference and the supplier.
type InvoiceSource struct {
	DocumentNumber, SupplierReference string
	SupplierID                        uuid.UUID
	SupplierName                      string
}

// Invoices reads supplier invoices (payables and partner contexts) by id;
// an id of none is left out.
type Invoices interface {
	Sources(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]InvoiceSource, error)
}

// Assignment is an asset's centers as Centers derives them: its profit
// center, and whether it needs a cost center it was not given.
type Assignment struct {
	ProfitCenterID     *uuid.UUID
	CostCenterRequired bool
}

// CenterName is a cost or profit center by its code and name.
type CenterName struct{ Code, Name string }

// Centers reads the ledger's cost and profit centers (ADR-0057 §2, §8):
// an asset's centers derived and checked on a date as a posting on its
// depreciation account (by number) would (ledger.AssetReads.Assignment),
// and centers' codes and names by id.
type Centers interface {
	Assignment(ctx context.Context, legalEntity uuid.UUID, date time.Time, year int16, account string, costCenter, profitCenter *uuid.UUID) (Assignment, error)
	Names(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]CenterName, error)
}

// Numbers issues document numbers from the company's number ranges
// (configuration context), in the request's transaction.
type Numbers interface {
	Next(ctx context.Context, legalEntity uuid.UUID, documentType string, fiscalYear int16) (string, error)
}

// KindDepreciate is the kind of a depreciation run's Operation (ADR-0057 §7).
const KindDepreciate = "assets.depreciate"

// Runs keeps a depreciation run's Operation (operations context, ADR-0057
// §2, §7): Start writes it queued, for the run's resource path, in the
// request's transaction, which the workflows dispatcher then starts;
// Report records the batches posted of all; Cancelled says whether it
// was cancelled, which stops the run at its next step, read under the
// Operation's lock with lock and as it stands without; Succeed and Fail
// finish it from the run's activities. assets imports neither operations
// nor the workflow engine.
type Runs interface {
	Start(ctx context.Context, resource string, request any) (gen.Operation, error)
	Report(ctx context.Context, id uuid.UUID, done, total int64) error
	Cancelled(ctx context.Context, id uuid.UUID, lock bool) (bool, error)
	Succeed(ctx context.Context, id uuid.UUID, resource string, result any) error
	Fail(ctx context.Context, id uuid.UUID, p *problem.Problem) error
}

// Endpoints implements the assets context's operations of the public API.
type Endpoints struct {
	Settings  Settings
	Companies Companies
	Ledger    Ledger
	Centers   Centers
	Numbers   Numbers
	Runs      Runs
	Invoices  Invoices
	Pages     *pagination.Codec
	Now       func() time.Time // the clock of an asset's creation date; nil: the wall clock
	BatchSize int              // the most assets a depreciation run's batch takes; 0: 500
}
