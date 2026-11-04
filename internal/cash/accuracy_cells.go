package cash

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// The forecast's accuracy as the analytical store holds it (ADR-0044,
// ADR-0051 §11; T53j3, product owner 2026-11-04): the actuals against
// the forecast in cells of a value date, a lag (0, 7 or 30), a currency
// and a liquidity item, as the variance compares them. The accuracy is
// 100 less WAPE, Σ|actual − forecast| / Σ|forecast| over the cells, so
// each cell's absolute value is taken before the cells are added up: the
// semantic layer only sums, so the cells are rows of their own
// (cash_accuracy), built here from the same shares as cash_actuals and
// cash_snapshots and read through the same ports.

// AccuracyCell is the actuals against the forecast of one value date at
// one lag, in one currency and liquidity item: the actual flows of the
// day (ActualShares'), the flows the company's snapshot taken LagDays
// before forecast for it (ForecastShares', blocked suppliers' left out),
// and Variance, the actual less the forecast. ID names the cell (its
// legal entity, day, lag, currency and item); the item's code and today's
// name are "" for no item.
type AccuracyCell struct {
	ID        uuid.UUID
	ValueDate time.Time
	LagDays   int32
	Currency  string
	Exponent  int
	Item      string
	ItemName  string
	Actual    money.Minor
	Forecast  money.Minor
	Variance  money.Minor
}

// AccuracyCells are the legal entity's cells, read as the caller in a
// consistent read of their own: the semantic layer's database path, whose
// endpoint has checked cash.forecast.read. A legal entity the caller
// cannot act in is not found.
func (e *Endpoints) AccuracyCells(ctx context.Context, legalEntity uuid.UUID) ([]AccuracyCell, error) {
	return asCaller(ctx, e, legalEntity, e.accuracyCells)
}

// ExtractAccuracyCells are the legal entity's cells, read in the context's
// transaction: the analytical store's extract (ADR-0044). A legal entity
// the directory does not know has none.
func (e *Endpoints) ExtractAccuracyCells(ctx context.Context, legalEntity uuid.UUID) ([]AccuracyCell, error) {
	return asExtract(ctx, e, legalEntity, e.accuracyCells)
}

// comparedSQL is the horizon of the legal entity's preferred snapshot of
// each date, the one ForecastShares reads.
const comparedSQL = `SELECT DISTINCT ON (s.snapshot_date) s.snapshot_date, s.horizon_end FROM cash.snapshot s WHERE s.legal_entity_id = $1
	ORDER BY s.snapshot_date, ` + snapshotPreferred

// accuracyCells reads, in tx, the legal entity's actuals and forecast and
// compares them in cells, ordered by value date, lag, currency and item.
func (e *Endpoints) accuracyCells(ctx context.Context, tx pgx.Tx, co Company, legalEntity uuid.UUID) ([]AccuracyCell, error) {
	ar, err := e.classified(ctx, tx, co, legalEntity, firstValueDate, lastValueDate)
	if err != nil {
		return nil, err
	}
	actuals, err := ar.shares(ctx)
	if err != nil {
		return nil, err
	}
	forecast, err := e.forecastShares(ctx, tx, co, legalEntity)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, comparedSQL, legalEntity)
	horizons := map[string]time.Time{}
	var date, horizon time.Time
	_, err = pgx.ForEachRow(rows, []any{&date, &horizon}, func() error {
		horizons[date.Format(time.DateOnly)] = horizon
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("cash: snapshots of %s compared: %w", legalEntity, err)
	}
	c := comparison{legalEntity: legalEntity, read: ar.r.read, horizons: horizons, cells: map[cellKey]*cellTerms{}}
	for _, a := range actuals {
		for _, lag := range varianceLags {
			c.add(cellFlow{a.ValueDate, a.Currency, a.Exponent, a.Item, a.ItemName, a.Amount}, int32(lag), true) //nolint:gosec // a lag is 0, 7 or 30
		}
	}
	for _, f := range forecast {
		if !f.Blocked && slices.Contains(varianceLags, int(f.LagDays)) {
			c.add(cellFlow{f.ValueDate, f.Currency, f.Exponent, f.Item, f.ItemName, f.Amount}, f.LagDays, false)
		}
	}
	return c.result()
}

// cellKey is what a cell compares: a value date (ISO, so keys compare as
// dates do whatever a time's location) at a lag, in a currency and an
// item.
type cellKey struct {
	day      string
	lag      int32
	currency string
	item     string
}

// cellTerms are a cell's amounts, summed once all are read, so the sums
// are exact whatever their order (money.Sum).
type cellTerms struct {
	day              time.Time
	exponent         int
	name             string
	actual, forecast []money.Minor
}

// comparison gathers the cells of a legal entity: a value date is compared
// at a lag when it has passed (on or before the company's date) and the
// company's snapshot of the date the lag before was taken and forecast as
// far (its horizon), the variance's rules (checkPassed, snapshotOn,
// checkHorizon). Lags 0 to 30 are within a snapshot's document level.
type comparison struct {
	legalEntity uuid.UUID
	read        time.Time
	horizons    map[string]time.Time // by snapshot date (ISO)
	cells       map[cellKey]*cellTerms
}

// cellFlow is an actual share or a forecast line as a cell counts it: its
// value date, currency and exponent, item's code and name, and amount.
type cellFlow struct {
	day        time.Time
	currency   string
	exponent   int
	item, name string
	amount     money.Minor
}

// add is an actual or forecast flow in its cell at the lag, when its day
// is compared at it.
func (c comparison) add(f cellFlow, lag int32, actual bool) {
	horizon, taken := c.horizons[f.day.AddDate(0, 0, -int(lag)).Format(time.DateOnly)]
	if !taken || f.day.After(c.read) || f.day.After(horizon) {
		return
	}
	k := cellKey{day: f.day.Format(time.DateOnly), lag: lag, currency: f.currency, item: f.item}
	t := c.cells[k]
	if t == nil {
		t = &cellTerms{day: f.day, exponent: f.exponent, name: f.name}
		c.cells[k] = t
	}
	if actual {
		t.actual = append(t.actual, f.amount)
	} else {
		t.forecast = append(t.forecast, f.amount)
	}
}

// result is the cells in order; sums out of the range of an amount are
// refused, never given saturated.
func (c comparison) result() ([]AccuracyCell, error) {
	keys := slices.SortedFunc(maps.Keys(c.cells), func(a, b cellKey) int {
		return cmp.Or(strings.Compare(a.day, b.day), cmp.Compare(a.lag, b.lag), strings.Compare(a.currency, b.currency), strings.Compare(a.item, b.item))
	})
	out := make([]AccuracyCell, 0, len(keys))
	for _, k := range keys {
		t := c.cells[k]
		actual, errA := money.Sum(t.actual...)
		forecast, errF := money.Sum(t.forecast...)
		variance, errV := money.Diff(actual, forecast)
		if errA != nil || errF != nil || errV != nil {
			return nil, errVarianceTooLarge()
		}
		id := uuid.NewSHA1(uuid.NameSpaceURL, []byte("urn:agentic-erp:cash-accuracy-cell:"+c.legalEntity.String()+"/"+k.day+"/"+
			strconv.Itoa(int(k.lag))+"/"+k.currency+"/"+k.item))
		out = append(out, AccuracyCell{ID: id, ValueDate: t.day, LagDays: k.lag, Currency: k.currency, Exponent: t.exponent, Item: k.item, ItemName: t.name,
			Actual: actual, Forecast: forecast, Variance: variance})
	}
	return out, nil
}
