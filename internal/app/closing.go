package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/closing"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Closing is the financial close context as the API wires it (ADR-0058
// §2): a leaf that reads the configuration, tenancy, ledger and identity
// contexts through the adapters below, so none of them imports it and it
// imports none of them.
func Closing(cfg Config) *closing.Endpoints {
	return &closing.Endpoints{Settings: configuration.Settings{}, Companies: closingCompanies{}, Ledger: closingLedger{}, People: closingPeople{},
		Pages: cfg.Pages, Now: cfg.Now}
}

// closingCompanies gives the closing context a legal entity's code and
// name.
type closingCompanies struct{}

func (closingCompanies) Company(ctx context.Context, id uuid.UUID) (closing.Company, bool, error) {
	le, ok, err := tenancy.Directory{}.Entity(ctx, id)
	return closing.Company{ID: le.ID, Code: le.Code, Name: le.Name}, ok, err
}

// closingLedger gives the closing context the ledger's posting periods,
// and what a close's checks read: parked entries and revaluation runs.
type closingLedger struct{}

func (closingLedger) LeadingPeriods(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16) ([]closing.Period, error) {
	ps, err := ledger.ClosePeriods{}.Leading(ctx, legalEntity, fiscalYear)
	out := make([]closing.Period, len(ps))
	for i, p := range ps {
		out[i] = closingPeriod(p)
	}
	return out, err
}

func (closingLedger) PeriodsOf(ctx context.Context, legalEntity uuid.UUID, fiscalYear, period int16) ([]closing.LedgerPeriod, error) {
	ks, err := ledger.ClosePeriods{}.Of(ctx, legalEntity, fiscalYear, period)
	out := make([]closing.LedgerPeriod, len(ks))
	for i, k := range ks {
		out[i] = closing.LedgerPeriod{LedgerID: k.LedgerID, Code: k.LedgerCode, Name: k.LedgerName, Leading: k.IsLeading}
		if k.Period != nil {
			p := closingPeriod(*k.Period)
			out[i].Period = &p
		}
	}
	return out, err
}

func (closingLedger) Parked(ctx context.Context, legalEntity uuid.UUID, from, to time.Time, statuses []string, limit int) (int, []closing.ParkedEntry, error) {
	n, ps, err := ledger.CloseChecks{}.Parked(ctx, legalEntity, from, to, statuses, limit)
	out := make([]closing.ParkedEntry, len(ps))
	for i, p := range ps {
		out[i] = closing.ParkedEntry{ID: p.ID, DocumentType: p.DocumentType, PostingDate: p.PostingDate, Status: p.Status, Reference: p.Reference,
			HeaderText: p.HeaderText, CreatedBy: p.CreatedBy, CreatedByKind: p.CreatedByKind}
	}
	return n, out, err
}

func (closingLedger) PostedRevaluation(ctx context.Context, legalEntity, ledgerID uuid.UUID, keyDate time.Time) (uuid.UUID, bool, error) {
	return ledger.CloseChecks{}.PostedRevaluation(ctx, legalEntity, ledgerID, keyDate)
}

func closingPeriod(p ledger.Period) closing.Period {
	return closing.Period{ID: p.ID, Number: p.Period, StartDate: p.StartDate, EndDate: p.EndDate, Status: p.Status}
}

// closingPeople gives the closing context principals' names, kinds and
// statuses.
type closingPeople struct{}

func (closingPeople) Named(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]closing.Person, error) {
	ns, err := identity.People{}.Named(ctx, ids)
	out := make(map[uuid.UUID]closing.Person, len(ns))
	for id, n := range ns {
		out[id] = closing.Person{Name: n.Name, Kind: n.Kind, Status: n.Status}
	}
	return out, err
}
