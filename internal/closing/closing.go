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
// company's thresholds. T64k adds the depreciation check (ADR-0058 §6):
// each covered ledger has a posted depreciation run of the period, or
// nothing a run of it would select, read through the assets context's
// port (ADR-0057 §7).
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

// Ledger reads the ledger context: the leading ledger's regular periods
// of a legal entity in a fiscal year, by period; the ledgers it keeps with
// their period of a fiscal year and period, the leading one first, then by
// code; how many of its parked entries with a posting date from from to to
// are in one of the statuses, and the first limit of them by posting date;
// its ledger's posted revaluation run at a key date, ok false when there
// is none; and every profit and loss account of it with its net movement
// of a fiscal year and period in the leading ledger, by account number.
// Each reads nothing of a legal entity the caller may not act in. Lock
// locks a closed period as the period lock does (T32), refusing
// one whose version is no longer the one read.
type Ledger interface {
	LeadingPeriods(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16) ([]Period, error)
	PeriodsOf(ctx context.Context, legalEntity uuid.UUID, fiscalYear, period int16) ([]LedgerPeriod, error)
	Parked(ctx context.Context, legalEntity uuid.UUID, from, to time.Time, statuses []string, limit int) (int, []ParkedEntry, error)
	PostedRevaluation(ctx context.Context, legalEntity, ledger uuid.UUID, keyDate time.Time) (run uuid.UUID, ok bool, err error)
	Lock(ctx context.Context, period uuid.UUID, version int32) error
	ProfitAndLoss(ctx context.Context, legalEntity uuid.UUID, fiscalYear, period int16) ([]AccountNet, error)
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
	Depreciation Depreciation
	Pages        *pagination.Codec
	Now          func() time.Time
}
