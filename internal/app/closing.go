package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/agents"
	"github.com/mustie7iy/agentic-erp/internal/assets"
	"github.com/mustie7iy/agentic-erp/internal/closing"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/controlling"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Closing is the financial close context as the API wires it (ADR-0058
// §2): a leaf that reads the configuration, tenancy, ledger, identity,
// controlling, agents, assets, payables, procurement and partner contexts
// through the adapters below, so none of them imports it and it imports
// none of them. It parks accrual proposals' entries through the ledger's
// endpoints as the API wires them.
func Closing(cfg Config) *closing.Endpoints {
	return &closing.Endpoints{Settings: configuration.Settings{}, Companies: closingCompanies{},
		Ledger: closingLedger{parked: ledger.ParkedEntries{Endpoints: Ledger(cfg)}}, People: closingPeople{}, Plans: closingPlans{},
		Sessions: agents.Sessions{}, Depreciation: closingDepreciation{}, Evidence: closingEvidence{}, Pages: cfg.Pages, Now: cfg.Now}
}

// closingCompanies gives the closing context a legal entity's code, name
// and local currency.
type closingCompanies struct{}

func (closingCompanies) Company(ctx context.Context, id uuid.UUID) (closing.Company, bool, error) {
	le, ok, err := tenancy.Directory{}.Entity(ctx, id)
	return closing.Company{ID: le.ID, Code: le.Code, Name: le.Name, LocalCurrency: le.LocalCurrency}, ok, err
}

// closingLedger gives the closing context the ledger's posting periods
// (a year's by ledger for a year close), what a close's checks read
// (parked entries, revaluation runs and a year's carry-forward), the
// period lock a sign-off takes, the variance report's profit and loss and
// the journal entries its comments cite, and an accrual proposal's
// accounts, centers and parked entry, that entry's status and its
// cancellation.
type closingLedger struct {
	parked ledger.ParkedEntries
}

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

func (closingLedger) YearPeriods(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16) ([]closing.LedgerYear, error) {
	ks, err := ledger.ClosePeriods{}.OfYear(ctx, legalEntity, fiscalYear)
	out := make([]closing.LedgerYear, len(ks))
	for i, k := range ks {
		out[i] = closing.LedgerYear{LedgerID: k.LedgerID, Code: k.LedgerCode, Name: k.LedgerName, Leading: k.IsLeading, Periods: make([]closing.Period, len(k.Periods))}
		for j, p := range k.Periods {
			out[i].Periods[j] = closingPeriod(p)
		}
	}
	return out, err
}

func (closingLedger) Parked(ctx context.Context, legalEntity uuid.UUID, from, to time.Time, statuses []string, special bool,
	limit int) (int, []closing.ParkedEntry, error) {
	n, ps, err := ledger.CloseChecks{}.Parked(ctx, legalEntity, from, to, statuses, special, limit)
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

func (closingLedger) CarryForward(ctx context.Context, legalEntity, ledgerID uuid.UUID, fiscalYear int16) (closing.CarryForward, error) {
	cf, err := ledger.CloseChecks{}.CarryForward(ctx, legalEntity, ledgerID, fiscalYear)
	out := closing.CarryForward{Run: cf.Run}
	for _, p := range cf.OpenedSince {
		out.OpenedSince = append(out.OpenedSince, closing.FiscalPeriod{FiscalYear: p.FiscalYear, Period: p.Period})
	}
	return out, err
}

func (closingLedger) Lock(ctx context.Context, period uuid.UUID, version int32) error {
	_, err := ledger.ClosePeriods{}.Lock(ctx, period, version)
	return err
}

func (closingLedger) ProfitAndLoss(ctx context.Context, legalEntity uuid.UUID, fiscalYear, period int16) ([]closing.AccountNet, error) {
	ns, err := ledger.CloseAccounts{}.ProfitAndLoss(ctx, legalEntity, fiscalYear, period)
	out := make([]closing.AccountNet, len(ns))
	for i, n := range ns {
		out[i] = closing.AccountNet{AccountID: n.AccountID, Number: n.Number, Name: n.Name, Net: n.Net}
	}
	return out, err
}

func (closingLedger) Cited(ctx context.Context, legalEntity, account uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]closing.CitedEntry, error) {
	cs, err := ledger.CloseAccounts{}.Cited(ctx, legalEntity, account, ids)
	out := make(map[uuid.UUID]closing.CitedEntry, len(cs))
	for id, c := range cs {
		out[id] = closing.CitedEntry{ID: c.ID, FiscalYear: c.FiscalYear, DocumentNumber: c.DocumentNumber, PostingDate: c.PostingDate, Periods: c.Periods}
	}
	return out, err
}

func (closingLedger) Accounts(ctx context.Context, legalEntity uuid.UUID, ids []uuid.UUID, numbers []string) ([]closing.Account, error) {
	as, err := ledger.CloseAccounts{}.Accounts(ctx, legalEntity, ids, numbers)
	out := make([]closing.Account, len(as))
	for i, a := range as {
		out[i] = closing.Account{ID: a.ID, Number: a.Number, Name: a.Name, BalanceSheet: a.BalanceSheet, Postable: a.Postable}
	}
	return out, err
}

// Centers names centers through the ledger's published names of cost and
// profit centers (the assets context reads them too).
func (closingLedger) Centers(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]closing.Center, error) {
	ns, err := ledger.AssetReads{}.CenterNames(ctx, ids)
	out := make(map[uuid.UUID]closing.Center, len(ns))
	for id, n := range ns {
		out[id] = closing.Center{Code: n.Code, Name: n.Name}
	}
	return out, err
}

func (closingLedger) ProfitCenter(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16, date time.Time, account uuid.UUID,
	costCenter, profitCenter *uuid.UUID) (*uuid.UUID, error) {
	return ledger.CloseAccounts{}.ProfitCenter(ctx, legalEntity, fiscalYear, date, account, costCenter, profitCenter)
}

func (l closingLedger) Park(ctx context.Context, entry closing.ParkEntry) (uuid.UUID, error) {
	in := ledger.NewEntry{LegalEntityID: entry.LegalEntityID, DocumentType: "SA", DocumentDate: entry.Date, PostingDate: entry.Date,
		Currency: entry.Currency, HeaderText: &entry.HeaderText, Lines: make([]ledger.NewLine, len(entry.Lines))}
	for i, line := range entry.Lines {
		in.Lines[i] = ledger.NewLine{AccountID: line.AccountID, Amount: line.Amount, Currency: entry.Currency, CostCenterID: line.CostCenterID,
			ProfitCenterID: line.ProfitCenterID, Text: &line.Text}
	}
	p, err := l.parked.Park(ctx, in)
	return p.ID, err
}

func (l closingLedger) Statuses(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]closing.EntryStatus, error) {
	ss, err := l.parked.Statuses(ctx, ids)
	out := make(map[uuid.UUID]closing.EntryStatus, len(ss))
	for id, s := range ss {
		out[id] = closing.EntryStatus{Status: s.Status, Debits: s.Debits, Currency: s.Currency, JournalEntryID: s.JournalEntryID,
			DocumentNumber: s.DocumentNumber, ReversalID: s.ReversalEntryID, ReversalNumber: s.ReversalNumber}
	}
	return out, err
}

func (l closingLedger) Cancel(ctx context.Context, id uuid.UUID, reason string) error {
	return l.parked.Cancel(ctx, id, reason)
}

func closingPeriod(p ledger.Period) closing.Period {
	return closing.Period{ID: p.ID, Number: p.Period, StartDate: p.StartDate, EndDate: p.EndDate, Special: p.IsSpecial, Status: p.Status, Version: p.Version}
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

// closingPlans gives the closing context the controlling context's plan
// categories and plans, the variance report's plan basis.
type closingPlans struct{}

func (closingPlans) Category(ctx context.Context, code string) (closing.PlanCategory, bool, error) {
	c, ok, err := controlling.Plans{}.Category(ctx, code)
	return closing.PlanCategory{ID: c.ID, Code: c.Code, Name: c.Name}, ok, err
}

func (closingPlans) Amounts(ctx context.Context, legalEntity, category uuid.UUID, fiscalYear, period int16, profit bool) (map[uuid.UUID]money.Minor, error) {
	return controlling.Plans{}.Amounts(ctx, legalEntity, category, fiscalYear, period, profit)
}

// closingDepreciation gives the closing context the assets context's
// depreciation of a period, the depreciation check's (T64k).
type closingDepreciation struct{}

func (closingDepreciation) Posted(ctx context.Context, legalEntity, ledgerID uuid.UUID, fiscalYear, period int16, end time.Time,
	limit int) (closing.PeriodDepreciation, error) {
	d, err := assets.CloseChecks{}.Depreciation(ctx, legalEntity, ledgerID, fiscalYear, period, end, limit)
	out := closing.PeriodDepreciation{Run: d.Run, Selected: d.Selected, Late: make([]closing.LateAsset, len(d.Late))}
	if d.Later != nil {
		out.Later = &closing.LaterDepreciation{ID: d.Later.ID, Of: closing.FiscalPeriod(d.Later.Of), Currency: d.Later.Currency}
	}
	for i, a := range d.Late {
		out.Late[i] = closing.LateAsset{ID: a.ID, Number: a.Number, Description: a.Description, CatchUp: a.CatchUp}
		if a.MissingFrom != nil {
			from := closing.FiscalPeriod(*a.MissingFrom)
			out.Late[i].MissingFrom = &from
		}
	}
	return out, err
}

// closingEvidence gives the closing context what an accrual proposal
// rests on: supplier invoices (payables), purchase orders (procurement)
// and the suppliers' numbers and names (partner).
type closingEvidence struct{}

func (closingEvidence) Invoices(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]closing.Document, error) {
	ss, err := payables.Invoices{}.Sources(ctx, ids)
	out := make(map[uuid.UUID]closing.Document, len(ss))
	for id, s := range ss {
		out[id] = closing.Document{LegalEntityID: s.LegalEntityID, Number: s.DocumentNumber, SupplierID: s.SupplierID, Counts: s.Status == payables.Posted}
	}
	return out, err
}

// Orders reads each order on its own: an accrual cites at most ten
// documents.
func (closingEvidence) Orders(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]closing.Document, error) {
	out := make(map[uuid.UUID]closing.Document, len(ids))
	for _, id := range ids {
		o, ok, err := procurement.Orders{}.Order(ctx, id)
		if err != nil {
			return nil, err
		}
		if ok {
			out[id] = closing.Document{LegalEntityID: o.LegalEntityID, Number: o.DocumentNumber, SupplierID: o.SupplierID, Counts: o.Status == procurement.Released}
		}
	}
	return out, nil
}

func (closingEvidence) Suppliers(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]closing.Supplier, error) {
	ns, err := partner.Payees{}.Names(ctx, ids)
	out := make(map[uuid.UUID]closing.Supplier, len(ns))
	for id, n := range ns {
		out[id] = closing.Supplier{Number: n.Number, Name: n.Name}
	}
	return out, err
}
