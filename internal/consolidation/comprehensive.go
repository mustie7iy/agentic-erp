package consolidation

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
)

// Other comprehensive income (ADR-0059 §7, §11, As built: other
// comprehensive income; the accounting specialist's answer B3,
// po:2026-11-09). A company's translation difference is recognised in
// other comprehensive income, not in the profit (IAS 21.39(c), 21.41),
// and accumulated in a separate component of equity (IAS 21.52(b)), the
// translation reserve: its cta row, cumulative, on the balance sheet. The
// profit and loss therefore shows, below the profit, the fiscal year's
// change of each company's reserve through the period, the span its
// profit covers: the reserve at the run's period less its opening, the
// reserve in the group's final run of the previous fiscal year's last
// period as the run found it when it started (run.opening_run_id; zero
// without one, product owner 2026-11-05: the reserve starts at zero at
// go-live). The reserve stays per company, as its reclassification on a
// disposal needs (IAS 21.48).
//
// Each member's difference is attributed to the non-controlling interest
// by the part the parent does not hold and to the owners of the parent
// for the rest (IAS 21.41, IAS 1.81B). A run consolidates only members
// held whole (As built T68h), so the interest's share is zero until the
// step that writes the nci entries, which also moves its accumulated
// share from the reserve to the nci account on the balance sheet (§9).

// reserve is a company's translation reserve in a run's statement, in
// the group currency's minor units, debit positive: at the end of the
// previous fiscal year and at the run's period, with the part of the
// change between them attributable to non-controlling interests. The
// closing reserve holds what the run moved out of the company's profit of
// the year, the translation part of its group-currency revaluation
// differences (member.RevaluationTranslation; translate's
// revaluationPart) and the rest of the exchange differences on its
// designated net investments (member.NetInvestment; netInvestment).
type reserve struct {
	member           Member
	opening, closing money.Minor
	nci              money.Minor
}

// difference is the fiscal year's translation difference through the
// period: the company's part of other comprehensive income.
func (v reserve) difference() money.Minor { return v.closing - v.opening }

// comprehensive is what a run's profit and loss shows below the profit:
// the run its reserves open from, if any, each company's reserve by
// company code, and the year's change of the elimination entries' lines
// on the cta account.
type comprehensive struct {
	openingRun   *uuid.UUID
	reserves     []reserve
	eliminations money.Minor
	names        map[uuid.UUID]string // the run's companies' names
}

// openingRun finds the run a run's translation reserve opens from: the
// group's final run of the previous fiscal year's last period, if any,
// kept from being superseded until the run that names it has recorded it
// (FOR SHARE; a finalization locks its period's runs FOR UPDATE). A
// finalization of that period after this read makes the run stale
// (reopened).
func openingRun(ctx context.Context, tx pgx.Tx, r Run) (*uuid.UUID, error) {
	year, period := previousPeriod(r.FiscalYear, 1)
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM consolidation.run WHERE group_code = $1 AND fiscal_year = $2 AND fiscal_period = $3 AND status = 'final'
		FOR SHARE`, r.GroupCode, year, period).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &id, errs.Wrapf(err, "consolidation: the opening run of run %s", r.ID)
}

// openingsNow are, by run, the group's final run of the previous fiscal
// year's last period as it is now, for the runs that have one: what a
// run started now would open from (openingRun).
func openingsNow(ctx context.Context, tx pgx.Tx, rs []Run) (map[uuid.UUID]*uuid.UUID, error) {
	ids := make([]uuid.UUID, len(rs))
	for i, r := range rs {
		ids[i] = r.ID
	}
	rows, _ := tx.Query(ctx, `SELECT r.id, o.id FROM consolidation.run r
		JOIN consolidation.run o ON o.tenant_id = r.tenant_id AND o.group_code = r.group_code AND o.fiscal_year = r.fiscal_year - 1
			AND o.fiscal_period = 12 AND o.status = 'final'
		WHERE r.id = ANY($1)`, ids)
	out := map[uuid.UUID]*uuid.UUID{}
	var run, opening uuid.UUID
	_, err := pgx.ForEachRow(rows, []any{&run, &opening}, func() error {
		id := opening
		out[run] = &id
		return nil
	})
	return out, errs.Wrapf(err, "consolidation: the opening runs of %d runs now", len(rs))
}

// reopened reports whether the run a run's reserve opens from is no
// longer the one a run started now would open from (ADR-0059 §10): the
// previous fiscal year's last period was finalized, or finalized again,
// after the run started, so its other comprehensive income is the change
// from a reserve that is not the group's opening any more.
func reopened(r Run, now map[uuid.UUID]*uuid.UUID) bool {
	current := now[r.ID]
	if r.OpeningRunID == nil || current == nil {
		return r.OpeningRunID != current
	}
	return *r.OpeningRunID != *current
}

// runOrNone names a run, or none.
func runOrNone(id *uuid.UUID) string {
	if id == nil {
		return "none"
	}
	return id.String()
}

// reserveFigures are a run's translation reserve as its rows hold it,
// read by the cta account of the configuration version it recorded
// (run_balance and run_entry_line are indexed by run and group account):
// the translation differences per company, its cta rows; and its
// elimination entries' lines on the account summed (an elimination's
// difference account may be it, §8; an acquired member's reserve at
// acquisition is reversed there, §9). The nci entries' lines are left
// out: they attribute the reserve to the interest and do not change it.
func (e *Endpoints) reserveFigures(ctx context.Context, tx pgx.Tx, r Run) (map[uuid.UUID]money.Minor, money.Minor, error) {
	doc, err := e.version(ctx, r.ConfigVersion)
	if err != nil {
		return nil, 0, fmt.Errorf("consolidation: configuration version %d of run %s: %w", r.ConfigVersion, r.ID, err)
	}
	account := roleAccount(doc.Consolidation, config.RoleCTA)
	rows, _ := tx.Query(ctx, `SELECT legal_entity_id, sum(amount_gc)::bigint FROM consolidation.run_balance
		WHERE run_id = $1 AND group_account = $2 AND kind = 'cta' GROUP BY legal_entity_id`, r.ID, account)
	differences := map[uuid.UUID]money.Minor{}
	var company uuid.UUID
	var amount money.Minor
	if _, err := pgx.ForEachRow(rows, []any{&company, &amount}, func() error { differences[company] = amount; return nil }); err != nil {
		return nil, 0, fmt.Errorf("consolidation: translation differences of run %s: %w", r.ID, err)
	}
	var eliminations money.Minor
	err = tx.QueryRow(ctx, `SELECT coalesce(sum(l.amount_gc), 0)::bigint FROM consolidation.run_entry en
		JOIN consolidation.run_entry_line l ON l.tenant_id = en.tenant_id AND l.run_id = en.run_id AND l.entry_no = en.entry_no
		WHERE en.run_id = $1 AND en.kind <> 'nci' AND l.group_account = $2`, r.ID, account).Scan(&eliminations)
	return differences, eliminations, errs.Wrapf(err, "consolidation: the eliminations on the translation reserve of run %s", r.ID)
}

// opening is what a run's reserves open from: the reserve (reserveFigures)
// of the run it named when it started; nothing for a run that named none.
func (e *Endpoints) opening(ctx context.Context, tx pgx.Tx, r Run) (map[uuid.UUID]money.Minor, money.Minor, error) {
	if r.OpeningRunID == nil {
		return nil, 0, nil
	}
	// Read without readRun's access check (§12): it is a run of the same
	// group, and only the figures of this run's companies, which the
	// caller may all act in, are shown from it.
	o, err := scanRun(tx.QueryRow(ctx, runSelect+` WHERE id = $1`, *r.OpeningRunID))
	if err != nil {
		return nil, 0, fmt.Errorf("consolidation: the opening run %s of run %s: %w", *r.OpeningRunID, r.ID, err)
	}
	return e.reserveFigures(ctx, tx, o)
}

// comprehensiveOf reads what a run's profit and loss shows below the
// profit (comprehensive), from the run's and its opening run's stored
// rows alone (§14).
func (e *Endpoints) comprehensiveOf(ctx context.Context, tx pgx.Tx, r Run) (comprehensive, error) {
	c := comprehensive{openingRun: r.OpeningRunID, names: map[uuid.UUID]string{}}
	opening, openingEliminations, err := e.opening(ctx, tx, r)
	if err != nil {
		return c, err
	}
	closing, eliminations, err := e.reserveFigures(ctx, tx, r)
	if err != nil {
		return c, err
	}
	c.eliminations = eliminations - openingEliminations
	entities, err := e.Companies.Entities(ctx)
	if err != nil {
		return c, fmt.Errorf("consolidation: companies: %w", err)
	}
	for _, co := range entities {
		c.names[co.ID] = co.Name
	}
	rows, _ := tx.Query(ctx, memberSelect+` WHERE run_id = $1 ORDER BY company_code`, r.ID)
	ms, err := pgx.CollectRows(rows, scanMember)
	if err != nil {
		return c, fmt.Errorf("consolidation: companies of run %s: %w", r.ID, err)
	}
	c.reserves, err = reservesOf(ms, opening, closing)
	return c, err
}

// reservesOf are the companies' reserves in their order, each from its
// opening and closing translation difference (none is zero), with the
// interest's share of the change.
func reservesOf(ms []Member, opening, closing map[uuid.UUID]money.Minor) ([]reserve, error) {
	out := make([]reserve, len(ms))
	for i, m := range ms {
		v := reserve{member: m, opening: opening[m.LegalEntityID], closing: closing[m.LegalEntityID]}
		share, err := interestShare(v.difference(), m.OwnershipPercent)
		if err != nil {
			return nil, fmt.Errorf("consolidation: the non-controlling interest's share of %s's translation difference: %w", m.CompanyCode, err)
		}
		v.nci = share
		out[i] = v
	}
	return out, nil
}

// interestShare is the part of a member's translation difference
// attributable to the non-controlling interest (IAS 21.41; ADR-0059 §9's
// translation share): the difference times the part the parent does not
// hold, (100 - ownership) / 100, in exact rational arithmetic, rounded
// once to the minor unit, half away from zero (ADR-0005). The owners of
// the parent have the rest, so the two shares sum to the difference and
// the rounding remainder is the owners'. The parent has no ownership and
// no interest.
func interestShare(difference money.Minor, ownership *string) (money.Minor, error) {
	if ownership == nil {
		return 0, nil
	}
	// A numeric(7,4) above zero and at most 100, always a decimal.
	outside := new(big.Rat).Sub(big.NewRat(100, 1), money.ParseRat(*ownership))
	return money.ConvertAt(difference, outside.Quo(outside, big.NewRat(100, 1)), 0, 0)
}

// total is other comprehensive income: the companies' differences summed
// and the year's change of the eliminations on the reserve.
func (c comprehensive) total() columns {
	f := columns{eliminations: c.eliminations}
	for _, v := range c.reserves {
		f.reported += v.difference()
	}
	return f
}

// revaluation is the part of other comprehensive income the run moved
// out of the companies' profit of the year: the translation part of
// their group-currency revaluation differences, reported by them, so no
// elimination.
func (c comprehensive) revaluation() columns {
	var f columns
	for _, v := range c.reserves {
		f.reported += v.member.RevaluationTranslation
	}
	return f
}

// netInvestment is the part of other comprehensive income that is the
// companies' exchange differences on their designated net investments,
// moved out of their profit of the year (IAS 21.32): reported by them,
// so no elimination.
func (c comprehensive) netInvestment() columns {
	var f columns
	for _, v := range c.reserves {
		f.reported += v.member.NetInvestment
	}
	return f
}

// interest is other comprehensive income's share attributable to
// non-controlling interests: an attribution, so not reported by any
// company, as the profit's share is shown (nciProfit).
func (c comprehensive) interest() columns {
	var f columns
	for _, v := range c.reserves {
		f.eliminations += v.nci
	}
	return f
}

// reservesAPI shows the companies' reserves.
func (s statement) reservesAPI() []gen.ConsolidatedTranslationReserve {
	out := make([]gen.ConsolidatedTranslationReserve, len(s.oci.reserves))
	for i, v := range s.oci.reserves {
		out[i] = gen.ConsolidatedTranslationReserve{LegalEntityID: v.member.LegalEntityID, CompanyCode: v.member.CompanyCode,
			CompanyName: s.oci.names[v.member.LegalEntityID], Role: v.member.Role, Opening: s.money(v.opening), Difference: s.money(v.difference()),
			Closing: s.money(v.closing), RevaluationTranslation: s.money(v.member.RevaluationTranslation),
			NetInvestment: s.money(v.member.NetInvestment), NonControllingInterests: s.money(v.nci), OwnersOfTheParent: s.money(v.difference() - v.nci)}
		if v.member.OwnershipPercent != nil {
			out[i].OwnershipPercent = gen.NewOptString(plainRate(*v.member.OwnershipPercent))
		}
	}
	return out
}

// comprehensiveTotals adds to the profit and loss's totals what follows
// the profit: other comprehensive income, total comprehensive income and
// its attribution to non-controlling interests (their share of the profit
// and of the translation differences) and to the owners of the parent
// (the rest), IAS 1.81B; and, of other comprehensive income, what the run
// moved out of the profit (revaluation, netInvestment).
func (s statement) comprehensiveTotals(out *gen.ConsolidatedStatementTotals, profit, nciProfit columns) {
	oci := s.oci.total()
	total, interest := profit.add(oci), nciProfit.add(s.oci.interest())
	out.OtherComprehensiveIncome = gen.NewOptConsolidatedFigures(s.figuresAPI(oci))
	out.RevaluationTranslation = gen.NewOptConsolidatedFigures(s.figuresAPI(s.oci.revaluation()))
	out.NetInvestment = gen.NewOptConsolidatedFigures(s.figuresAPI(s.oci.netInvestment()))
	out.TotalComprehensiveIncome = gen.NewOptConsolidatedFigures(s.figuresAPI(total))
	out.ComprehensiveIncomeNonControllingInterests = gen.NewOptConsolidatedFigures(s.figuresAPI(interest))
	out.ComprehensiveIncomeOwnersOfTheParent = gen.NewOptConsolidatedFigures(s.figuresAPI(columns{reported: total.reported - interest.reported,
		eliminations: total.eliminations - interest.eliminations}))
}
