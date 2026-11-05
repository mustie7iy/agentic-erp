package assets

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// CloseChecks is the assets context's published port for a financial
// close's depreciation check (ADR-0058 §2, §6, T64k), run in the
// request's transaction under row-level security.
type CloseChecks struct{}

// PeriodDepreciation is a company's depreciation of a period in a ledger
// as a close's check reads it: its latest posted run of the period, nil
// without one, and, only without one, how many assets a run of the period
// would select and the latest posted run of a later period, nil without
// one. With a later run and assets selected, Late names the first of
// those assets, by asset number, with the catch-up the later run's period
// needs for each (PO 2026-11-04).
type PeriodDepreciation struct {
	Run      *uuid.UUID
	Selected int
	Later    *LaterRun
	Late     []LateAsset
}

// FiscalPeriod is a fiscal year and regular period.
type FiscalPeriod struct{ FiscalYear, Period int16 }

// LaterRun is a posted depreciation run of a later period than the one
// read: its id, period and currency.
type LaterRun struct {
	ID       uuid.UUID
	Of       FiscalPeriod
	Currency string
}

// LateAsset is an asset a run of a period would select after a later
// period's run posted: its number and description; the first period
// whose depreciation it has not booked (the period after the one it is
// booked through, else its depreciation start's), nil when it has
// neither; and its catch-up, what a run of the later run's period posts
// for it now, planned through that period less booked (ADR-0057 §7), in
// the run's currency.
type LateAsset struct {
	ID                  uuid.UUID
	Number, Description string
	MissingFrom         *FiscalPeriod
	CatchUp             money.Minor
}

// Depreciation reads the legal entity's depreciation of the fiscal year
// and period, ending on end, in the ledger. Only a posted run counts: a
// failed one (a cancelled one ends failed, T57f) or one still queued or
// running does not. Without a posted run it counts the assets a run would
// select, through the run's and the preview's own selection
// (selectDepreciable), so "nothing to depreciate" means what a run would
// find (ADR-0057 §7 step 1); when it selects some and a later period's
// run has posted, a run of this period is refused as out of order
// (runsAllow), so it names the first limit of them with their catch-up
// instead. A legal entity the caller may not act in is an error, not
// "nothing to depreciate": a check must not pass on what it could not
// read.
func (CloseChecks) Depreciation(ctx context.Context, legalEntity, ledger uuid.UUID, fiscalYear, period int16, end time.Time,
	limit int) (PeriodDepreciation, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return PeriodDepreciation{}, fmt.Errorf("assets: depreciation of %s: %w", legalEntity, err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return PeriodDepreciation{}, fmt.Errorf("assets: depreciation of %s: %w", legalEntity, err)
	}
	if !access.Allows(legalEntity) {
		return PeriodDepreciation{}, fmt.Errorf("assets: depreciation of %s: not a company the caller may act in", legalEntity)
	}
	var run uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM assets.depreciation_run WHERE legal_entity_id = $1 AND ledger_id = $2 AND fiscal_year = $3
		AND fiscal_period = $4 AND status = 'posted' ORDER BY created_at DESC, id DESC LIMIT 1`, legalEntity, ledger, fiscalYear, period).Scan(&run)
	switch {
	case err == nil:
		return PeriodDepreciation{Run: &run}, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return PeriodDepreciation{}, fmt.Errorf("assets: posted depreciation run of %s in ledger %s, %d/%d: %w", legalEntity, ledger, fiscalYear, period, err)
	}
	cs, err := selectDepreciable(ctx, tx, legalEntity, ledger, Period{FiscalYear: fiscalYear, Number: period, End: end}, nil, nil)
	if err != nil {
		return PeriodDepreciation{}, err
	}
	out := PeriodDepreciation{Selected: len(cs)}
	if len(cs) == 0 {
		return out, nil
	}
	if out.Later, err = laterRun(ctx, tx, legalEntity, ledger, FiscalPeriod{fiscalYear, period}); err != nil || out.Later == nil {
		return out, err
	}
	out.Late, err = lateAssets(ctx, tx, ledger, calendar{FiscalPeriod{fiscalYear, period}, MonthOf(end)}, *out.Later, cs[:min(len(cs), limit)])
	return out, err
}

// laterRun is the legal entity's latest posted depreciation run in the
// ledger of a period after p, nil without one.
func laterRun(ctx context.Context, tx pgx.Tx, legalEntity, ledger uuid.UUID, p FiscalPeriod) (*LaterRun, error) {
	var r LaterRun
	err := tx.QueryRow(ctx, `SELECT id, fiscal_year, fiscal_period, currency FROM assets.depreciation_run
		WHERE legal_entity_id = $1 AND ledger_id = $2 AND status = 'posted' AND (fiscal_year, fiscal_period) > ($3::smallint, $4::smallint)
		ORDER BY fiscal_year DESC, fiscal_period DESC, created_at DESC, id DESC LIMIT 1`, legalEntity, ledger, p.FiscalYear, p.Period).
		Scan(&r.ID, &r.Of.FiscalYear, &r.Of.Period, &r.Currency)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("assets: later depreciation run of %s in ledger %s after %d/%d: %w", legalEntity, ledger, p.FiscalYear, p.Period, err)
	}
	return &r, nil
}

// calendar places regular periods in months from one period whose month
// is known: a fiscal year's twelve regular periods are consecutive
// calendar months (Month), so periods and months advance together.
type calendar struct {
	at    FiscalPeriod
	month Month
}

// monthOf is the month of a regular period.
func (c calendar) monthOf(p FiscalPeriod) Month {
	return c.month + Month(index(p)-index(c.at))
}

// periodOf is the regular period of a month.
func (c calendar) periodOf(m Month) FiscalPeriod {
	i := index(c.at) + int(m-c.month)
	return FiscalPeriod{FiscalYear: int16(i / 12), Period: int16(i%12 + 1)} // a fiscal year of four digits and a period of 1 to 12
}

// index counts regular periods: twelve to a fiscal year.
func index(p FiscalPeriod) int { return int(p.FiscalYear)*12 + int(p.Period) - 1 }

// lateAssets reads the assets' numbers, descriptions, the period each is
// booked through and its moves in the ledger, and gives each its first
// missing period and its catch-up: the difference a run of the later
// run's period posts for it (differences, ADR-0057 §7).
func lateAssets(ctx context.Context, tx pgx.Tx, ledger uuid.UUID, cal calendar, later LaterRun, cs []*candidate) ([]LateAsset, error) {
	if err := pageNames(ctx, tx, cs); err != nil {
		return nil, err
	}
	m := cal.monthOf(later.Of)
	through := Period{FiscalYear: later.Of.FiscalYear, Number: later.Of.Period, Start: time.Date(int(m)/12, time.Month(int(m)%12+1), 1, 0, 0, 0, 0, time.UTC)}
	if err := differences(ctx, tx, ledger, through, cs); err != nil {
		return nil, err
	}
	booked, err := bookedThrough(ctx, tx, ledger, cs)
	if err != nil {
		return nil, err
	}
	out := make([]LateAsset, len(cs))
	for i, c := range cs {
		out[i] = LateAsset{ID: c.asset, Number: c.number, Description: c.description, CatchUp: c.difference}
		switch b, ok := booked[c.asset]; {
		case ok:
			next := cal.periodOf(cal.monthOf(b) + 1)
			out[i].MissingFrom = &next
		case c.start() != nil:
			from := cal.periodOf(MonthOf(*c.start()))
			out[i].MissingFrom = &from
		}
	}
	return out, nil
}

// bookedThrough is the period each asset's depreciation is booked through
// in the ledger, absent for one with none booked.
func bookedThrough(ctx context.Context, tx pgx.Tx, ledger uuid.UUID, cs []*candidate) (map[uuid.UUID]FiscalPeriod, error) {
	ids := make([]pgtype.UUID, len(cs))
	for i, c := range cs {
		ids[i] = pgtype.UUID{Bytes: c.asset, Valid: true}
	}
	rows, _ := tx.Query(ctx, `SELECT asset_id, booked_through_year, booked_through_period FROM assets.asset_terms
		WHERE asset_id = ANY($1) AND ledger_id = $2 AND booked_through_year IS NOT NULL`, ids, ledger)
	out := make(map[uuid.UUID]FiscalPeriod, len(cs))
	var asset uuid.UUID
	var p FiscalPeriod
	_, err := pgx.ForEachRow(rows, []any{&asset, &p.FiscalYear, &p.Period}, func() error {
		out[asset] = p
		return nil
	})
	return out, errs.Wrapf(err, "assets: booked through of %d assets in ledger %s", len(cs), ledger)
}
