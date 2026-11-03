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
// published through the closing outbox (ADR-0007, ADR-0011).
package closing

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
)

// Settings reads the active configuration (configuration context), whose
// close template and calendars the context reads.
type Settings interface {
	Document(ctx context.Context) (*config.Config, int32, error)
}

// Company is a legal entity as the closing context reads it.
type Company struct {
	ID   uuid.UUID
	Code string
	Name string
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
}

// LedgerPeriod is a ledger a legal entity keeps and its posting period
// of one fiscal year and period, nil when it has none.
type LedgerPeriod struct {
	LedgerID   uuid.UUID
	Code, Name string
	Leading    bool
	Period     *Period
}

// Ledger reads the ledger context's posting periods: the leading
// ledger's regular periods of a legal entity in a fiscal year, by period,
// and the ledgers it keeps with their period of a fiscal year and period,
// the leading one first, then by code; none for a legal entity the caller
// may not act in.
type Ledger interface {
	LeadingPeriods(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16) ([]Period, error)
	PeriodsOf(ctx context.Context, legalEntity uuid.UUID, fiscalYear, period int16) ([]LedgerPeriod, error)
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
	Settings  Settings
	Companies Companies
	Ledger    Ledger
	People    People
	Pages     *pagination.Codec
	Now       func() time.Time
}
