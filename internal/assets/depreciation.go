package assets

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Movement kinds (ADR-0057 §5).
const kindDepreciation = "depreciation"

// postingPeriod is a fiscal year and period a movement was posted in.
type postingPeriod struct{ year, period int16 }

// depreciable is an asset's terms and movements in one ledger as the
// schedule reads them (ADR-0057 §7): the moves it applies, the
// depreciation booked (the sum of its depreciation movements, a debit of
// expense, so positive) and, for the schedule alone, that by posting
// period.
type depreciable struct {
	asset       uuid.UUID
	terms       Terms
	capitalised *time.Time
	moves       []Move
	booked      money.Minor
	bookedIn    map[postingPeriod]money.Minor
}

// start is the day depreciation starts from: the terms' own, else the
// capitalisation date (ADR-0057 §6); nil while the asset has neither.
func (d depreciable) start() *time.Time {
	if d.terms.DepreciationStart != nil {
		return d.terms.DepreciationStart
	}
	return d.capitalised
}

// plan is the terms as the walk reads them.
func (d depreciable) plan() Plan {
	p := Plan{Method: d.terms.Method, Life: d.terms.UsefulLifeMonths, Residual: d.terms.Residual}
	if d.terms.DecliningFactor != nil {
		p.Factor = *d.terms.DecliningFactor
	}
	if s := d.start(); s != nil {
		m := MonthOf(*s)
		p.Start = &m
	}
	return p
}

// planned walks the schedule through the period and returns it with
// planned(P), the depreciation planned through it.
func (d depreciable) planned(through Month) ([]Step, money.Minor, error) {
	steps, err := Walk(d.plan(), d.moves, through)
	if err != nil {
		return nil, 0, fmt.Errorf("assets: schedule of asset %s in ledger %s: %w", d.asset, d.terms.LedgerID, err)
	}
	return steps, Planned(steps, through), nil
}

// readMovements reads the movements of the assets in the ledger into
// them: every one but depreciation as a move at its value date's period,
// and the depreciation booked. With byPeriod (the schedule) it reads the
// depreciation movements themselves, into what is booked and what is
// booked by posting period. Otherwise (a run's batch, the preview, a
// catch-up) only the booked total is wanted, and it is read from the
// assets' year totals, which every posting keeps with its movements in
// its own transaction (keepTotals, ADR-0057 §5, T57i): an asset depreciated
// for ten years has some 120 depreciation movements and at most ten
// totals rows per ledger, and its other movements are read through the
// partial index movement_moves_by_asset (T57f2: the preview of 100,000
// assets read 5.8 million rows).
func readMovements(ctx context.Context, tx pgx.Tx, ledger uuid.UUID, ds map[uuid.UUID]*depreciable, byPeriod bool) error {
	ids := make([]uuid.UUID, 0, len(ds))
	for id := range ds {
		ids = append(ids, id)
	}
	sql := `SELECT asset_id, kind, value_date, fiscal_year, fiscal_period, cost, accumulated, residual
		FROM assets.movement WHERE asset_id = ANY($1) AND ledger_id = $2 ORDER BY asset_id, seq`
	if !byPeriod {
		// A totals row carries the asset's depreciation (accumulated, a
		// credit) as a depreciation row with no date or period. Unordered:
		// the walk applies a period's moves in any order, their effects
		// adding (Walk), and sorting 100,000 assets' rows spilled to disk.
		sql = `SELECT asset_id, kind, value_date, fiscal_year, fiscal_period, cost, accumulated, residual
			FROM (SELECT asset_id, kind, value_date, fiscal_year, fiscal_period, cost, accumulated, residual
				FROM assets.movement WHERE asset_id = ANY($1) AND ledger_id = $2 AND kind <> 'depreciation'
			UNION ALL SELECT asset_id, 'depreciation', NULL, NULL, NULL, 0, sum(depreciation)::bigint, 0
				FROM assets.year_total WHERE asset_id = ANY($1) AND ledger_id = $2 GROUP BY asset_id) m`
	}
	rows, _ := tx.Query(ctx, sql, ids, ledger)
	var asset uuid.UUID
	var kind string
	var value *time.Time
	var year, period *int16
	var cost, accumulated, residual money.Minor
	_, err := pgx.ForEachRow(rows, []any{&asset, &kind, &value, &year, &period, &cost, &accumulated, &residual}, func() error {
		d := ds[asset]
		if kind == kindDepreciation {
			d.booked -= accumulated
			if byPeriod {
				d.bookedIn[postingPeriod{*year, *period}] -= accumulated
			}
			return nil
		}
		d.moves = append(d.moves, Move{Month: MonthOf(*value), Cost: cost, Accumulated: accumulated, Residual: residual})
		return nil
	})
	return errs.Wrapf(err, "assets: movements of %d assets in ledger %s", len(ids), ledger)
}

// periodOf is the regular period of the company's fiscal year with the
// number, refusing a special one (422 assets.special_period), and a year
// that is not set up or lacks the period (422 ledger.no_fiscal_period).
func (e *Endpoints) periodOf(ctx context.Context, le uuid.UUID, year int16, number int16) (Period, error) {
	if number > 12 {
		return Period{}, invalid("assets.special_period", fmt.Sprintf("Period %d is a special period, which takes no depreciation; "+
			"depreciation is per regular period (1-12).", number), "period", "a regular period, 1 to 12")
	}
	periods, err := e.Ledger.Periods(ctx, le, year)
	if err != nil {
		return Period{}, fmt.Errorf("assets: periods of %s in %d: %w", le, year, err)
	}
	i := slices.IndexFunc(periods, func(p Period) bool { return p.Number == number })
	if i < 0 {
		return Period{}, problem.Unprocessable("ledger.no_fiscal_period", fmt.Sprintf("Fiscal year %d has no regular period %d.", year, number))
	}
	return periods[i], nil
}

// ledgerOf is the ledger of a company by id, or its leading ledger
// (the first it keeps) when none is given; ok false for a ledger the
// company does not keep.
func (e *Endpoints) ledgerOf(ctx context.Context, le uuid.UUID, id *uuid.UUID) (LedgerRef, bool, error) {
	kept, err := e.Ledger.Ledgers(ctx, &le)
	if err != nil {
		return LedgerRef{}, false, fmt.Errorf("assets: ledgers of %s: %w", le, err)
	}
	// A company keeps the leading ledger (ADR-0045), which comes first,
	// once its configuration is applied; before, it keeps none.
	if id == nil {
		return firstOf(kept)
	}
	i := slices.IndexFunc(kept, func(l LedgerRef) bool { return l.ID == *id })
	if i < 0 {
		return LedgerRef{}, false, nil
	}
	return kept[i], true, nil
}

// firstOf is the first of the ledgers, ok false when there is none.
func firstOf(ledgers []LedgerRef) (LedgerRef, bool, error) {
	if len(ledgers) == 0 {
		return LedgerRef{}, false, nil
	}
	return ledgers[0], true, nil
}
