// Package closing is the financial close context (ADR-0058; SAP's
// Financial Closing cockpit): the close template, each company's close
// calendar, and, as later slices add them, its period closes with their
// checklists, sign-offs, accrual proposals and variance comments. It is
// named closing, not close, Go's builtin. It is a leaf (ADR-0058 §2): it
// reads the other contexts only through ports it declares, which the
// composition root (internal/app) adapts and which run in the request's
// transaction under row-level security (ADR-0003), and no context imports
// it (internal/layout).
//
// T64a builds the calendar: the close template (close.tasks) and each
// company's weekend are configuration (ADR-0058 §3, internal/config), and
// GetCloseCalendar plans each regular period's due dates from them on the
// company's business days (config.Config.BusinessDay, the rule cash
// counts working days by). T64b builds the period close (ADR-0058 §5,
// §6): one per company and regular period, its checklist copied from the
// template when it is opened, its manual tasks assigned, completed and
// reopened under a lock on the close (ADR-0008), each change audited and
// published through the closing outbox (ADR-0007, ADR-0011). T64c runs a
// check task's check on demand (ADR-0058 §6): no parked entry left in the
// period, and each covered ledger's revaluation posted, read through the
// ledger's port, the result recorded on the task with what it found. T64d
// signs a close off (ADR-0058 §7): every manual task done, every check
// passing when run again, each covered period closed by a person; it then
// locks each covered period through the ledger's period lock (T32) in the
// order postings take them, and records the sign-off. A covered period
// unlocked later reopens the close on its next write. T64h reports a
// close's variances (ADR-0058 §9): each profit and loss account's actual
// of the period against the prior period, the prior year's period or a
// plan category (read through controlling's port), flagged by the
// company's thresholds. T64i explains them (ADR-0058 §10): the close agent
// proposes a comment on a row in its session, citing the journal entries
// the row's figures are made of (checked through the ledger's port), and a
// person accepts or rejects it, or writes one; a comment keeps the figures
// it was written against and is stale once they change, and the check
// variances_explained passes when every row requiring a comment has an
// accepted one that is current. T64k adds the depreciation check (ADR-0058
// §6): each covered ledger has a posted depreciation run of the period, or
// nothing a run of it would select, read through the assets context's
// port (ADR-0057 §7). T64e lets the close agent propose accruals
// (ADR-0058 §8): one expense of the close's period the ledger does not
// show yet, resting on posted supplier invoices, released purchase orders
// or posted journal entries (read through the payables, procurement and
// ledger contexts' ports), parked through the ledger as the agent, debiting
// the expense and crediting the company's accrual account on the period's
// last day, for a person to post in the ledger.
package closing

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
)

// Settings reads the active configuration (configuration context), whose
// close template and calendars the context reads.
type Settings interface {
	Document(ctx context.Context) (*config.Config, int32, error)
}

// Company is a legal entity as the closing context reads it, with its
// local currency, the variance report's.
type Company struct {
	ID            uuid.UUID
	Code          string
	Name          string
	LocalCurrency string
}

// Companies reads legal entities (tenancy context): the one of an id, ok
// false when there is none.
type Companies interface {
	Company(ctx context.Context, id uuid.UUID) (Company, bool, error)
}

// Period is a posting period of a company's ledger as the closing
// context reads it.
type Period struct {
	ID        uuid.UUID
	Number    int16
	StartDate time.Time
	EndDate   time.Time
	Status    string
	Version   int32
}

// LedgerPeriod is a ledger a legal entity keeps and its posting period
// of one fiscal year and period, nil when it has none.
type LedgerPeriod struct {
	LedgerID   uuid.UUID
	Code, Name string
	Leading    bool
	Period     *Period
}

// AccountNet is a profit and loss account with its net movement of a
// period in local currency, debit positive.
type AccountNet struct {
	AccountID    uuid.UUID
	Number, Name string
	Net          money.Minor
}

// CitedEntry is a posted journal entry a variance comment cites: its
// fiscal year, document number and posting date, and the periods of its
// fiscal year in which it has a line on the account asked about.
type CitedEntry struct {
	ID             uuid.UUID
	FiscalYear     int16
	DocumentNumber string
	PostingDate    time.Time
	Periods        []int16
}

// ParkedEntry is a parked entry of the ledger context as a close's check
// names it: what identifies it to a person, and who parked it.
type ParkedEntry struct {
	ID                    uuid.UUID
	DocumentType          string
	PostingDate           time.Time
	Status                string
	Reference, HeaderText *string
	CreatedBy             uuid.UUID
	CreatedByKind         string
}

// Account is an account as an accrual proposal reads it: whether it is a
// balance sheet account and whether it is postable by hand in the company
// read for (active, set up and not blocked there, no reconciliation
// account, not its GR/IR account).
type Account struct {
	ID           uuid.UUID
	Number, Name string
	BalanceSheet bool
	Postable     bool
}

// Center is a cost or profit center by its code and name.
type Center struct{ Code, Name string }

// ParkLine is a line of an entry the closing context parks: an amount in
// the entry's currency (a decimal string, debit positive), its centers and
// its text.
type ParkLine struct {
	AccountID                    uuid.UUID
	Amount                       string
	CostCenterID, ProfitCenterID *uuid.UUID
	Text                         string
}

// ParkEntry is a journal entry the closing context parks for a person to
// post: dated (document and posting date) on Date, in Currency, its
// header text naming what it was parked for.
type ParkEntry struct {
	LegalEntityID uuid.UUID
	Date          time.Time
	Currency      string
	HeaderText    string
	Lines         []ParkLine
}

// Ledger reads the ledger context: the leading ledger's regular periods
// of a legal entity in a fiscal year, by period; the ledgers it keeps with
// their period of a fiscal year and period, the leading one first, then by
// code; how many of its parked entries with a posting date from from to to
// are in one of the statuses, and the first limit of them by posting date;
// its ledger's posted revaluation run at a key date, ok false when there
// is none; every profit and loss account of it with its net movement of a
// fiscal year and period in the leading ledger, by account number; and
// those of the ids that are its posted journal entries in the leading
// ledger, each with the periods it has a line on the account in; the
// accounts among ids, and those set up in the legal entity among numbers,
// by number. Each reads nothing of a legal entity the caller may not act
// in. Centers names the cost and profit centers among ids; an id of
// neither is absent. Lock locks a closed period as the period lock does
// (T32), refusing one whose version is no longer the one read. Park parks
// an entry as the request's principal, as parkJournalEntry does (its
// checks and problems), and returns its id.
type Ledger interface {
	LeadingPeriods(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16) ([]Period, error)
	PeriodsOf(ctx context.Context, legalEntity uuid.UUID, fiscalYear, period int16) ([]LedgerPeriod, error)
	Parked(ctx context.Context, legalEntity uuid.UUID, from, to time.Time, statuses []string, limit int) (int, []ParkedEntry, error)
	PostedRevaluation(ctx context.Context, legalEntity, ledger uuid.UUID, keyDate time.Time) (run uuid.UUID, ok bool, err error)
	Lock(ctx context.Context, period uuid.UUID, version int32) error
	ProfitAndLoss(ctx context.Context, legalEntity uuid.UUID, fiscalYear, period int16) ([]AccountNet, error)
	Cited(ctx context.Context, legalEntity, account uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]CitedEntry, error)
	Accounts(ctx context.Context, legalEntity uuid.UUID, ids []uuid.UUID, numbers []string) ([]Account, error)
	Centers(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Center, error)
	Park(ctx context.Context, entry ParkEntry) (uuid.UUID, error)
}

// Document is a supplier invoice or a purchase order as an accrual's
// evidence: its legal entity, document number and supplier, and whether
// it counts as evidence (an invoice posted, an order released).
type Document struct {
	LegalEntityID uuid.UUID
	Number        string
	SupplierID    uuid.UUID
	Counts        bool
}

// Supplier is a business partner by its number and name.
type Supplier struct{ Number, Name string }

// Evidence reads what an accrual proposal rests on (payables, procurement
// and partner contexts): the supplier invoices and the purchase orders
// among ids, and the business partners among ids, by id; an id of none is
// absent.
type Evidence interface {
	Invoices(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Document, error)
	Orders(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Document, error)
	Suppliers(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Supplier, error)
}

// Sessions reads the agents context's sessions: that a session is the
// calling agent's and running, refused with the agents context's problem
// when it is not (agents.not_your_session, agents.session_ended).
type Sessions interface {
	Running(ctx context.Context, id uuid.UUID) error
}

// PlanCategory is a plan category of the controlling context.
type PlanCategory struct {
	ID         uuid.UUID
	Code, Name string
}

// Plans reads the controlling context's plans: a plan category by its
// code, ok false when there is none, and a category's plan of a legal
// entity's fiscal year and period per account, in local currency, debit
// positive, summed over its lines on profit centers when profit is true,
// else on cost centers.
type Plans interface {
	Category(ctx context.Context, code string) (PlanCategory, bool, error)
	Amounts(ctx context.Context, legalEntity, category uuid.UUID, fiscalYear, period int16, profit bool) (map[uuid.UUID]money.Minor, error)
}

// Depreciation reads the assets context: a legal entity's latest posted
// depreciation run of a fiscal year and period, ending on end, in a
// ledger, nil without one; and, only without one, how many of its assets
// a run of that period in that ledger would select (ADR-0057 §7).
type Depreciation interface {
	Posted(ctx context.Context, legalEntity, ledger uuid.UUID, fiscalYear, period int16, end time.Time) (run *uuid.UUID, selected int, err error)
}

// Person is a principal as the closing context names it: its name, kind
// (human, agent, service) and status (active, blocked, deprovisioned).
type Person struct {
	Name, Kind, Status string
}

// People reads principals (identity context), keyed by id; an id the
// tenant has no principal of is absent.
type People interface {
	Named(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Person, error)
}

// Endpoints implements the closing context's operations of the public
// API. Now is the clock overdue tasks are judged by (nil: the system
// clock); Pages signs the lists' page tokens.
type Endpoints struct {
	Settings     Settings
	Companies    Companies
	Ledger       Ledger
	People       People
	Plans        Plans
	Sessions     Sessions
	Depreciation Depreciation
	Evidence     Evidence
	Pages        *pagination.Codec
	Now          func() time.Time
}
