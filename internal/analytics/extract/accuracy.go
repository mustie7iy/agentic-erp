package extract

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// The forecast's accuracy (dataset cash_accuracy v1, ADR-0051 §11; T53j3):
// the cash context's comparison of each compared value date's actuals with
// the forecast of the snapshot the lag before, in cells of a lag, currency
// and liquidity item, three rows per cell (the variance and the absolute
// variance and forecast WAPE adds up cell by cell, product owner
// 2026-11-04). The cash context compares them, through Cash, in the run's
// transaction, as it reads the actuals and the snapshots.

const cashAccuracyDataset = "cash_accuracy"

// AccuracyCell is the cash context's comparison of one value date at one
// lag in one currency and liquidity item: the cell by its id, the item's
// code and name ("" for no item), the actual and forecast amounts and the
// variance, the actual less the forecast, in minor units.
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

// cashAccuracyRow is a row of dataset cash_accuracy v1
// (analytics/datasets/v1/cash_accuracy.yaml).
type cashAccuracyRow struct {
	TenantID          uuid.UUID   `parquet:"tenant_id,uuid"`
	CellID            uuid.UUID   `parquet:"cell_id,uuid"`
	Company           string      `parquet:"company"`
	ValueDate         int32       `parquet:"value_date,date"`
	LagDays           int32       `parquet:"lag_days"`
	Figure            string      `parquet:"figure"`
	LiquidityItem     *string     `parquet:"liquidity_item,optional"`
	LiquidityItemName *string     `parquet:"liquidity_item_name,optional"`
	AmountMinor       money.Minor `parquet:"amount_minor"`
	Currency          string      `parquet:"currency"`
	Exponent          int32       `parquet:"exponent"`
}

// cashAccuracy reads the accuracy's cells of every legal entity with
// snapshots (without one no day is compared), ordered by value date,
// company, lag, currency, item and figure, one file per month of value
// dates. Without a port it reads none.
func (r *run) cashAccuracy(ctx context.Context, tx pgx.Tx) error {
	if r.x.Cash == nil {
		return nil
	}
	all, err := cashOf(ctx, tx, r, cashAccuracyDataset, forecastCompaniesSQL, r.x.Cash.ExtractAccuracyCells)
	if err != nil {
		return err
	}
	slices.SortFunc(all, func(a, b ofCompany[AccuracyCell]) int {
		return cmp.Or(a.v.ValueDate.Compare(b.v.ValueDate), strings.Compare(a.company, b.company), cmp.Compare(a.v.LagDays, b.v.LagDays),
			strings.Compare(a.v.Currency, b.v.Currency), strings.Compare(a.v.Item, b.v.Item))
	})
	months, done := snapshotOf[cashAccuracyRow](ctx, r, cashAccuracyDataset)
	for _, c := range all {
		for _, row := range accuracyRows(r.tenant, c.company, c.v) {
			if err = months.add(c.v.ValueDate.Format(monthLayout), row); err != nil {
				return done(err)
			}
		}
	}
	return done(nil)
}

// accuracyRows are a cell's rows, one per figure: the signed variance, and
// the absolute variance and forecast (the range of an amount is
// symmetric, so each has one); a cell of no item has a null item and name.
func accuracyRows(tenant uuid.UUID, company string, c AccuracyCell) []cashAccuracyRow {
	row := cashAccuracyRow{TenantID: tenant, CellID: c.ID, Company: company, ValueDate: days(c.ValueDate), LagDays: c.LagDays, Currency: c.Currency,
		Exponent: int32(c.Exponent)} //nolint:gosec // a currency's exponent is 0 to 4 (ref.currency)
	if c.Item != "" {
		item, name := c.Item, c.ItemName
		row.LiquidityItem, row.LiquidityItemName = &item, &name
	}
	out := make([]cashAccuracyRow, 0, 3)
	for _, f := range []struct {
		name   string
		amount money.Minor
	}{{"variance", c.Variance}, {"absolute_variance", c.Variance.Abs()}, {"absolute_forecast", c.Forecast.Abs()}} {
		row.Figure, row.AmountMinor = f.name, f.amount
		out = append(out, row)
	}
	return out
}
