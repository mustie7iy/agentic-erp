package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/consolidation"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Consolidation is the consolidation context as the API wires it
// (ADR-0059 §2): a leaf that reads the configuration, tenancy, ledger and
// operations contexts through the adapters below, so none of them imports
// it and it imports none of them.
func Consolidation(cfg Config) *consolidation.Endpoints {
	return &consolidation.Endpoints{Settings: configuration.Settings{}, Companies: consolidationCompanies{}, Ledger: consolidationLedger{},
		People: consolidationPeople{}, Runs: runOperations{kind: consolidation.KindRun}, Pages: cfg.Pages}
}

// consolidationCompanies gives the consolidation context the tenant's
// legal entities and its group currency.
type consolidationCompanies struct{}

func (consolidationCompanies) Entities(ctx context.Context) ([]consolidation.Company, error) {
	es, err := tenancy.Directory{}.Entities(ctx)
	out := make([]consolidation.Company, len(es))
	for i, e := range es {
		out[i] = consolidation.Company{ID: e.ID, Code: e.Code, Name: e.Name, LocalCurrency: e.LocalCurrency, FiscalCalendarID: e.FiscalCalendarID}
	}
	return out, err
}

func (consolidationCompanies) GroupCurrency(ctx context.Context) (string, error) {
	return tenancy.Directory{}.GroupCurrency(ctx)
}

// consolidationLedger gives the consolidation context the ledger's
// figures in the leading ledger (ADR-0059 §4a), over the ledger's
// published ConsolidationReads.
type consolidationLedger struct{}

func (consolidationLedger) PeriodOf(ctx context.Context, company uuid.UUID, year, period int16) (consolidation.Period, bool, error) {
	leading, err := ledger.ConsolidationReads{}.LeadingLedger(ctx)
	if err != nil {
		return consolidation.Period{}, false, err
	}
	p, ok, err := ledger.ConsolidationReads{}.PeriodOf(ctx, company, leading, year, period)
	return consolidation.Period{Start: p.Start, End: p.End, Status: p.Status, Version: p.Version, UnlockRequestID: p.UnlockRequestID}, ok, err
}

func (consolidationLedger) PeriodsOf(ctx context.Context, companies []uuid.UUID, year, period int16) (map[uuid.UUID]consolidation.Period, error) {
	leading, err := ledger.ConsolidationReads{}.LeadingLedger(ctx)
	if err != nil {
		return nil, err
	}
	ps, err := ledger.ConsolidationReads{}.PeriodsOf(ctx, companies, leading, year, period)
	out := make(map[uuid.UUID]consolidation.Period, len(ps))
	for id, p := range ps {
		out[id] = consolidation.Period{Start: p.Start, End: p.End, Status: p.Status, Version: p.Version, UnlockRequestID: p.UnlockRequestID}
	}
	return out, err
}

func (consolidationLedger) Accounts(ctx context.Context, company uuid.UUID) ([]consolidation.Account, error) {
	as, err := ledger.ConsolidationReads{}.AccountsOf(ctx, company)
	out := make([]consolidation.Account, len(as))
	for i, a := range as {
		out[i] = consolidation.Account{ID: a.ID, Number: a.Number, Name: a.Name, Type: config.AccountType(a.Type)}
	}
	return out, err
}

func (consolidationLedger) BalancesThrough(ctx context.Context, company uuid.UUID, year, period int16) ([]consolidation.Balance, error) {
	leading, err := ledger.ConsolidationReads{}.LeadingLedger(ctx)
	if err != nil {
		return nil, err
	}
	bs, err := ledger.ConsolidationReads{}.BalancesThrough(ctx, company, leading, year, period)
	out := make([]consolidation.Balance, len(bs))
	for i, b := range bs {
		out[i] = consolidation.Balance{AccountID: b.AccountID, TradingPartnerID: b.TradingPartnerID, FiscalYear: b.FiscalYear, Period: b.Period,
			LC: b.LC, GC: b.GC}
	}
	return out, err
}

func (consolidationLedger) RateOn(ctx context.Context, rateType, from, to string, date time.Time) (consolidation.Rate, bool, error) {
	r, ok, err := ledger.ConsolidationReads{}.RateOn(ctx, rateType, from, to, date)
	return consolidation.Rate{Rate: r.Rate, Shown: r.Shown, ValidFrom: r.ValidFrom}, ok, err
}

// consolidationPeople gives the consolidation context principals' names:
// who finalized a run and who accepted a difference (ADR-0059 §2).
type consolidationPeople struct{}

func (consolidationPeople) Named(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	ns, err := identity.People{}.Named(ctx, ids)
	out := make(map[uuid.UUID]string, len(ns))
	for id, n := range ns {
		out[id] = n.Name
	}
	return out, err
}
