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
// counts working days by).
package closing

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
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

// Ledger reads the ledger context's posting periods: the leading
// ledger's regular periods of a legal entity in a fiscal year, by period;
// none for one the caller may not act in.
type Ledger interface {
	LeadingPeriods(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16) ([]Period, error)
}

// Endpoints implements the closing context's operations of the public API.
type Endpoints struct {
	Settings  Settings
	Companies Companies
	Ledger    Ledger
}
