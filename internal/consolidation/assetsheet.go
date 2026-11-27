package consolidation

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The consolidated asset history sheet (ADR-0057 §9 and As built, the
// consolidated sheet; ADR-0059 §7; the accounting specialist's B14,
// po:2026-11-09): a group's asset history sheet for a fiscal year in the
// group currency, its companies' class totals translated as a run
// translates (ADR-0059 §7, rateOf): the closing values, balances, at the
// closing rate of the year's period 12 and the opening values at that of
// the period before the opening; the profit and loss columns
// (depreciation, impairment losses and their reversals) at their booked
// group amounts; the other movements, of cost and of what retirements and
// transfers took, at their period's closing rate; and the net exchange
// differences on translation the rest, per column, so every row reconciles
// to the cent.

// AssetColumns are an asset class's sums by the history sheet's columns,
// in a company's local currency, signed as the sheet shows them (ADR-0057
// §9): the cost the acquisitions added, retirements took and transfers
// moved, and the accumulated depreciation depreciation, impairment losses
// and their reversals, retirements and transfers changed.
type AssetColumns struct {
	Acquisitions, Retirements, Transfers                money.Minor
	Depreciation, ImpairmentLosses, ImpairmentReversals money.Minor
	RetirementsAccumulated, TransfersAccumulated        money.Minor
}

// AssetBooked are an asset class's profit and loss columns at their
// booked group amounts (the movements' group amounts, each posting's own
// rate): depreciation, impairment losses and their reversals.
type AssetBooked struct {
	Depreciation, ImpairmentLosses, ImpairmentReversals money.Minor
}

// AssetClassTotal is an asset class's sums in a company's leading ledger:
// of every fiscal year before the one read (Period 0), its opening values,
// or of one period of that year (1-16), with its booked profit and loss
// columns (none for Period 0).
type AssetClassTotal struct {
	Class  string
	Period int16
	AssetColumns
	Booked AssetBooked
}

// Assets reads the assets context's class totals (ADR-0057 As built, the
// consolidated sheet) of a company in its leading ledger for a fiscal
// year, by class code and period.
type Assets interface {
	ClassTotals(ctx context.Context, company uuid.UUID, year int16) ([]AssetClassTotal, error)
}

func (a AssetColumns) plus(o AssetColumns) AssetColumns {
	return AssetColumns{Acquisitions: a.Acquisitions + o.Acquisitions, Retirements: a.Retirements + o.Retirements, Transfers: a.Transfers + o.Transfers,
		Depreciation: a.Depreciation + o.Depreciation, ImpairmentLosses: a.ImpairmentLosses + o.ImpairmentLosses,
		ImpairmentReversals: a.ImpairmentReversals + o.ImpairmentReversals, RetirementsAccumulated: a.RetirementsAccumulated + o.RetirementsAccumulated,
		TransfersAccumulated: a.TransfersAccumulated + o.TransfersAccumulated}
}

func (b AssetBooked) plus(o AssetBooked) AssetBooked {
	return AssetBooked{Depreciation: b.Depreciation + o.Depreciation, ImpairmentLosses: b.ImpairmentLosses + o.ImpairmentLosses,
		ImpairmentReversals: b.ImpairmentReversals + o.ImpairmentReversals}
}

// balances are the columns that move the class's balances alone, which
// are translated at a rate: the cost columns and what retirements and
// transfers took of accumulated depreciation, its profit and loss columns
// left out.
func (a AssetColumns) balances() AssetColumns {
	a.Depreciation, a.ImpairmentLosses, a.ImpairmentReversals = 0, 0, 0
	return a
}

// profitAndLoss are the profit and loss columns in local currency.
func (a AssetColumns) profitAndLoss() AssetBooked {
	return AssetBooked{Depreciation: a.Depreciation, ImpairmentLosses: a.ImpairmentLosses, ImpairmentReversals: a.ImpairmentReversals}
}

// cost and accumulated are what the columns change of cost and of
// accumulated depreciation.
func (a AssetColumns) cost() money.Minor { return a.Acquisitions + a.Retirements + a.Transfers }

func (a AssetColumns) accumulated() money.Minor {
	return a.Depreciation + a.ImpairmentLosses + a.ImpairmentReversals + a.RetirementsAccumulated + a.TransfersAccumulated
}

// sheetColumns are a consolidated sheet's columns in the group currency:
// cost and accumulated depreciation, each from its opening through the
// year's movements and the exchange differences to its closing.
type sheetColumns struct {
	OpeningCost, ClosingCost, ExchangeCost               money.Minor
	OpeningAccumulated, ClosingAccumulated, ExchangeAccu money.Minor
	AssetColumns
}

func (s sheetColumns) plus(o sheetColumns) sheetColumns {
	return sheetColumns{OpeningCost: s.OpeningCost + o.OpeningCost, ClosingCost: s.ClosingCost + o.ClosingCost, ExchangeCost: s.ExchangeCost + o.ExchangeCost,
		OpeningAccumulated: s.OpeningAccumulated + o.OpeningAccumulated, ClosingAccumulated: s.ClosingAccumulated + o.ClosingAccumulated,
		ExchangeAccu: s.ExchangeAccu + o.ExchangeAccu, AssetColumns: s.AssetColumns.plus(o.AssetColumns)}
}

// classLocal is a company's class in local currency: its opening values
// (as columns summed) and the year's movements from the opening on, by
// period, with their booked profit and loss columns summed.
type classLocal struct {
	opening AssetColumns
	periods map[int16]AssetColumns
	booked  AssetBooked
}

// opened reports whether the class opens with a value: a cost or an
// accumulated depreciation (as the company's sheet shows a class).
func (l classLocal) opened() bool { return l.opening.cost() != 0 || l.opening.accumulated() != 0 }

// sheetCompany is a company's part of the sheet: whether it is in the
// group currency, the rates it was translated at (opening nil without
// opening values; periods those with movements of its balances, none in
// the group currency), its classes' translated columns, by class code, and
// their totals.
type sheetCompany struct {
	c       includedCompany
	inGroup bool
	opening *Rate
	periods map[int16]Rate
	classes map[string]sheetColumns
	totals  sheetColumns
}

// GetConsolidatedAssetHistorySheet implements GET
// /v1/consolidation-groups/{code}/asset-history-sheet: the companies a
// run of the year's period 12 includes, refused as that run would be
// (targetOf: 404 for a group the caller may not see whole), each
// translated (sheetOf), and the classes summed over them.
func (e *Endpoints) GetConsolidatedAssetHistorySheet(ctx context.Context, params gen.GetConsolidatedAssetHistorySheetParams) (gen.GetConsolidatedAssetHistorySheetRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return nil, fmt.Errorf("consolidation: configuration: %w", err)
	}
	t, err := e.targetOf(ctx, tx, doc, params.Code, params.FiscalYear, 12)
	if err != nil {
		return nil, onFiscalYear(err)
	}
	start, err := e.yearStart(ctx, t)
	if err != nil {
		return nil, err
	}
	expGC, err := currency.Exponent(ctx, tx, t.currency, nil)
	if err != nil {
		return nil, fmt.Errorf("consolidation: currency %s: %w", t.currency, err)
	}
	companies := make([]sheetCompany, len(t.included))
	for i, c := range t.included {
		if companies[i], err = e.sheetOf(ctx, tx, t, c, expGC); err != nil {
			return nil, onFiscalYear(err)
		}
	}
	return assetSheet(doc, t, start, companies, expGC), nil
}

// onFiscalYear puts a run's refusal of a period on the sheet's parameter:
// the sheet has a fiscal year, and the run's period it stands for is the
// year's 12th or one of its months.
func onFiscalYear(err error) error {
	var p *problem.Problem
	if errors.As(err, &p) {
		for i := range p.Errors {
			if p.Errors[i].Field == "period" {
				p.Errors[i].Field = "fiscal_year"
			}
		}
	}
	return err
}

// yearStart is the first day of the parent's fiscal year: its period 1's
// (422 consolidation.period_unknown for a year without one).
func (e *Endpoints) yearStart(ctx context.Context, t target) (time.Time, error) {
	parent := t.companies[t.group.Parent]
	p, ok, err := e.Ledger.PeriodOf(ctx, parent.ID, t.year, 1)
	switch {
	case err != nil:
		return time.Time{}, fmt.Errorf("consolidation: period %d/1 of %s: %w", t.year, parent.Code, err)
	case !ok:
		return time.Time{}, problem.Unprocessable("consolidation.period_unknown", fmt.Sprintf("Company %s, the parent of group %s, has no period %d/1 "+
			"in its leading ledger.", parent.Code, t.group.Code, t.year), problem.FieldError{Field: "fiscal_year", Code: "period_unknown",
			Message: "a fiscal year of the parent's calendar"})
	}
	return p.Start, nil
}

// openingPeriod is the period the company's sheet opens at: a member's
// first, in the fiscal year of its first period, else the year's first.
func (c includedCompany) openingPeriod(year int16) int16 {
	if c.member != nil && c.firstYear == year {
		return c.firstP
	}
	return 1
}

// localOf groups a company's class totals by class: the years before and
// the periods before its opening period as its opening, the others by
// period.
func localOf(totals []AssetClassTotal, open int16) map[string]*classLocal {
	out := map[string]*classLocal{}
	for _, x := range totals {
		l := out[x.Class]
		if l == nil {
			l = &classLocal{periods: map[int16]AssetColumns{}}
			out[x.Class] = l
		}
		if x.Period < open {
			l.opening = l.opening.plus(x.AssetColumns)
		} else {
			l.periods[x.Period], l.booked = l.periods[x.Period].plus(x.AssetColumns), l.booked.plus(x.Booked)
		}
	}
	return out
}

// sheetOf reads the company's class totals, the rates they need (ratesOf)
// and translates each class (translate).
func (e *Endpoints) sheetOf(ctx context.Context, tx pgx.Tx, t target, c includedCompany, expGC int) (sheetCompany, error) {
	totals, err := e.Assets.ClassTotals(ctx, c.co.ID, t.year)
	if err != nil {
		return sheetCompany{}, fmt.Errorf("consolidation: asset class totals of %s in %d: %w", c.co.Code, t.year, err)
	}
	expLC, err := currency.Exponent(ctx, tx, c.co.LocalCurrency, nil)
	if err != nil {
		return sheetCompany{}, fmt.Errorf("consolidation: currency %s: %w", c.co.LocalCurrency, err)
	}
	open := c.openingPeriod(t.year)
	local := localOf(totals, open)
	s := sheetCompany{c: c, inGroup: c.co.LocalCurrency == t.currency, periods: map[int16]Rate{}, classes: map[string]sheetColumns{}}
	if err := e.ratesOf(ctx, t, &s, local, open); err != nil {
		return sheetCompany{}, err
	}
	for _, class := range slices.Sorted(maps.Keys(local)) {
		l := local[class]
		v, err := s.translate(*l, expLC, expGC)
		if err != nil {
			return sheetCompany{}, fmt.Errorf("consolidation: translate the asset class %s of %s: %w", class, c.co.Code, err)
		}
		if l.opened() || len(l.periods) > 0 {
			s.classes[class], s.totals = v, s.totals.plus(v)
		}
	}
	return s, nil
}

// ratesOf reads the rates the company's classes need, each only where an
// amount needs it: the opening rate when a class has opening values (the
// closing rate of the period before the opening period; for a member first
// consolidated in the year that is where a run measures its acquisition,
// measurePeriod), and each period's own closing rate for its movements of
// the balances (AssetColumns.balances), period 12's and a special
// period's the run's closing rate. A company in the group currency is at
// rate 1 throughout and lists no period rates.
func (e *Endpoints) ratesOf(ctx context.Context, t target, s *sheetCompany, local map[string]*classLocal, open int16) error {
	co := s.c.co
	opening, periods := false, map[int16]bool{}
	for _, l := range local {
		opening = opening || l.opened()
		for p, m := range l.periods {
			periods[p] = periods[p] || m.balances() != (AssetColumns{})
		}
	}
	if opening {
		year, per := previousPeriod(t.year, open)
		_, r, err := e.periodRate(ctx, t.rateType, t.currency, co, year, per, fmt.Sprintf("the period before the opening of its %d asset history sheet, "+
			"whose closing rate translates its opening values", t.year), "the period before company "+co.Code+"'s opening")
		if err != nil {
			return err
		}
		s.opening = &r
	}
	if s.inGroup {
		return nil
	}
	for _, p := range slices.Sorted(maps.Keys(periods)) {
		if !periods[p] {
			continue
		}
		if p >= 12 {
			s.periods[p] = s.c.rate
			continue
		}
		_, r, err := e.periodRate(ctx, t.rateType, t.currency, co, t.year, p, "whose closing rate translates its asset movements of the period",
			fmt.Sprintf("period %d of company %s", p, co.Code))
		if err != nil {
			return err
		}
		s.periods[p] = r
	}
	return nil
}

// translator converts local amounts at a rate into the group currency,
// each rounded once, half away from zero (ADR-0005), keeping the first
// failure (an amount beyond the minor unit's range).
type translator struct {
	expLC, expGC int
	err          error
}

func (x *translator) at(a money.Minor, r *big.Rat) money.Minor {
	if a == 0 || x.err != nil {
		return 0
	}
	v, err := money.ConvertAt(a, r, x.expLC, x.expGC)
	x.err = err
	return v
}

// columnsAt are the balances' columns at the rate, each translated once.
func (x *translator) columnsAt(a AssetColumns, r *big.Rat) AssetColumns {
	return AssetColumns{Acquisitions: x.at(a.Acquisitions, r), Retirements: x.at(a.Retirements, r), Transfers: x.at(a.Transfers, r),
		RetirementsAccumulated: x.at(a.RetirementsAccumulated, r), TransfersAccumulated: x.at(a.TransfersAccumulated, r)}
}

// translate is a class's columns in the group currency: the opening cost
// and accumulated depreciation at the opening rate, each period's
// movements of the balances at the period's rate, the profit and loss
// columns at their booked group amounts, the closing values at the closing
// rate, and the exchange differences what is left of the closing values
// after the opening and the movements. A company in the group currency is
// at rate 1 throughout, its profit and loss columns its local amounts (its
// booked group amounts are those, at rate 1), so it has no differences.
func (s sheetCompany) translate(l classLocal, expLC, expGC int) (sheetColumns, error) {
	x := &translator{expLC: expLC, expGC: expGC}
	closing := l.opening
	var v sheetColumns
	if s.opening != nil {
		v.OpeningCost, v.OpeningAccumulated = x.at(l.opening.cost(), s.opening.Rate), x.at(l.opening.accumulated(), s.opening.Rate)
	}
	var local AssetBooked // the profit and loss columns in local currency
	for _, p := range slices.Sorted(maps.Keys(l.periods)) {
		rate := one()
		if r, ok := s.periods[p]; ok {
			rate = r.Rate
		}
		m := l.periods[p]
		v.AssetColumns = v.AssetColumns.plus(x.columnsAt(m.balances(), rate))
		local, closing = local.plus(m.profitAndLoss()), closing.plus(m)
	}
	booked := l.booked
	if s.inGroup {
		booked = local
	}
	v.Depreciation, v.ImpairmentLosses, v.ImpairmentReversals = booked.Depreciation, booked.ImpairmentLosses, booked.ImpairmentReversals
	v.ClosingCost, v.ClosingAccumulated = x.at(closing.cost(), s.c.rate.Rate), x.at(closing.accumulated(), s.c.rate.Rate)
	v.ExchangeCost = v.ClosingCost - v.OpeningCost - v.cost()
	v.ExchangeAccu = v.ClosingAccumulated - v.OpeningAccumulated - v.accumulated()
	return v, x.err
}

// assetSheet is the response: the classes summed over the companies, by
// class code, each with its name from the configuration (none when it no
// longer has the class), the companies with their rates and totals, and
// the totals.
func assetSheet(doc *config.Config, t target, start time.Time, companies []sheetCompany, expGC int) *gen.ConsolidatedAssetHistorySheet {
	byClass := map[string]sheetColumns{}
	var totals sheetColumns
	out := &gen.ConsolidatedAssetHistorySheet{GroupCode: t.group.Code, GroupName: t.group.Name, FiscalYear: int32(t.year), StartDate: start,
		EndDate: t.period.End, Currency: t.currency, RateType: t.rateType, Items: []gen.ConsolidatedAssetHistorySheetRow{}}
	for _, s := range companies {
		for class, v := range s.classes {
			byClass[class] = byClass[class].plus(v)
		}
		totals = totals.plus(s.totals)
		out.Companies = append(out.Companies, s.response(t.currency, expGC))
	}
	for _, class := range slices.Sorted(maps.Keys(byClass)) {
		v := sheetValues(byClass[class], t.currency, expGC)
		row := gen.ConsolidatedAssetHistorySheetRow{ClassCode: class, OpeningCost: v.OpeningCost, Acquisitions: v.Acquisitions, Retirements: v.Retirements,
			Transfers: v.Transfers, ExchangeDifferences: v.ExchangeDifferences, ClosingCost: v.ClosingCost, OpeningAccumulated: v.OpeningAccumulated,
			Depreciation: v.Depreciation, ImpairmentLosses: v.ImpairmentLosses, ImpairmentReversals: v.ImpairmentReversals,
			RetirementsAccumulated: v.RetirementsAccumulated, TransfersAccumulated: v.TransfersAccumulated,
			ExchangeDifferencesAccumulated: v.ExchangeDifferencesAccumulated, ClosingAccumulated: v.ClosingAccumulated,
			OpeningNetBookValue: v.OpeningNetBookValue, ClosingNetBookValue: v.ClosingNetBookValue}
		if c := doc.AssetClass(class); c != nil {
			row.ClassName = gen.NewOptString(c.Name)
		}
		out.Items = append(out.Items, row)
	}
	out.Totals = sheetValues(totals, t.currency, expGC)
	return out
}

// response is the company's part of the response: who it is, its rates
// and its totals.
func (s sheetCompany) response(group string, expGC int) gen.ConsolidatedAssetHistorySheetCompany {
	role := "parent"
	if s.c.member != nil {
		role = "member"
	}
	out := gen.ConsolidatedAssetHistorySheetCompany{LegalEntityID: s.c.co.ID, CompanyCode: s.c.co.Code, CompanyName: s.c.co.Name, Role: role,
		LocalCurrency: s.c.co.LocalCurrency, ClosingRate: money.PlainQuantity(s.c.rate.Rate), ClosingRateValidFrom: validFrom(s.c.rate),
		PeriodRates: []gen.ConsolidatedAssetHistorySheetRate{}, Totals: sheetValues(s.totals, group, expGC)}
	if s.opening != nil {
		out.OpeningRate, out.OpeningRateValidFrom = gen.NewOptString(money.PlainQuantity(s.opening.Rate)), validFrom(*s.opening)
	}
	for _, p := range slices.Sorted(maps.Keys(s.periods)) {
		r := s.periods[p]
		out.PeriodRates = append(out.PeriodRates, gen.ConsolidatedAssetHistorySheetRate{Period: int32(p), Rate: money.PlainQuantity(r.Rate), ValidFrom: r.ValidFrom})
	}
	return out
}

// validFrom is the day a rate is valid from; none for rate 1 in the group
// currency, which no recorded rate gives.
func validFrom(r Rate) gen.OptDate {
	if r.ValidFrom.IsZero() {
		return gen.OptDate{}
	}
	return gen.NewOptDate(r.ValidFrom)
}

// sheetValues shows the columns with the net book values they give.
func sheetValues(v sheetColumns, group string, exp int) gen.ConsolidatedAssetHistorySheetValues {
	m := func(a money.Minor) gen.Money { return apitypes.Money(a, group, exp) }
	return gen.ConsolidatedAssetHistorySheetValues{OpeningCost: m(v.OpeningCost), Acquisitions: m(v.Acquisitions), Retirements: m(v.Retirements),
		Transfers: m(v.Transfers), ExchangeDifferences: m(v.ExchangeCost), ClosingCost: m(v.ClosingCost), OpeningAccumulated: m(v.OpeningAccumulated),
		Depreciation: m(v.Depreciation), ImpairmentLosses: m(v.ImpairmentLosses), ImpairmentReversals: m(v.ImpairmentReversals),
		RetirementsAccumulated: m(v.RetirementsAccumulated), TransfersAccumulated: m(v.TransfersAccumulated),
		ExchangeDifferencesAccumulated: m(v.ExchangeAccu), ClosingAccumulated: m(v.ClosingAccumulated),
		OpeningNetBookValue: m(v.OpeningCost + v.OpeningAccumulated), ClosingNetBookValue: m(v.ClosingCost + v.ClosingAccumulated)}
}
