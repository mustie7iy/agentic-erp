package extract

import (
	"bytes"
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// The forecast (dataset cash_snapshots v1, ADR-0051 §6, §11; T53j2): the
// lines at document level of each company's snapshot of each date, the
// one the variance compares against, each with its lag in days. The cash
// context reads them (with its house banks' codes and its items' names),
// through Cash, in the run's transaction, as it reads the actuals.

const cashSnapshotsDataset = "cash_snapshots"

// ForecastShare is a snapshot's line at document level as the cash
// context reads it: the line by its snapshot and number, the snapshot's
// date, the line's value date and lag in days, its house bank's code (""
// for none), its currency and that currency's exponent, its source and
// certainty, whether it is a blocked supplier's, its item's code and name
// ("" for no item) and the signed amount in minor units.
type ForecastShare struct {
	SnapshotID    uuid.UUID
	LineNo        int32
	SnapshotDate  time.Time
	ValueDate     time.Time
	LagDays       int32
	HouseBankCode string
	Currency      string
	Exponent      int
	Source        string
	Certainty     string
	Blocked       bool
	Item          string
	ItemName      string
	Amount        money.Minor
}

// cashSnapshot is a row of dataset cash_snapshots v1
// (analytics/datasets/v1/cash_snapshots.yaml).
type cashSnapshot struct {
	TenantID          uuid.UUID   `parquet:"tenant_id,uuid"`
	SnapshotID        uuid.UUID   `parquet:"snapshot_id,uuid"`
	LineNo            int32       `parquet:"line_no"`
	Company           string      `parquet:"company"`
	ValueDate         int32       `parquet:"value_date,date"`
	LagDays           int32       `parquet:"lag_days"`
	HouseBank         *string     `parquet:"house_bank,optional"`
	Source            string      `parquet:"source"`
	Certainty         string      `parquet:"certainty"`
	Blocked           bool        `parquet:"blocked"`
	LiquidityItem     *string     `parquet:"liquidity_item,optional"`
	LiquidityItemName *string     `parquet:"liquidity_item_name,optional"`
	AmountMinor       money.Minor `parquet:"amount_minor"`
	Currency          string      `parquet:"currency"`
	Exponent          int32       `parquet:"exponent"`
}

// forecastCompaniesSQL lists the legal entities that have snapshots, by
// company code: the others have no forecast kept.
const forecastCompaniesSQL = `SELECT le.id, le.code FROM tenancy.legal_entity le
	WHERE EXISTS (SELECT FROM cash.snapshot s WHERE s.tenant_id = le.tenant_id AND s.legal_entity_id = le.id) ORDER BY le.code`

// cashForecast reads the forecast of every legal entity with snapshots,
// ordered by value date, company, lag, snapshot and line, one file per
// month of value dates. Without a port it reads none.
func (r *run) cashForecast(ctx context.Context, tx pgx.Tx) error {
	if r.x.Cash == nil {
		return nil
	}
	all, err := cashOf(ctx, tx, r, cashSnapshotsDataset, forecastCompaniesSQL, r.x.Cash.ExtractForecastShares)
	if err != nil {
		return err
	}
	slices.SortFunc(all, func(a, b ofCompany[ForecastShare]) int {
		return cmp.Or(a.v.ValueDate.Compare(b.v.ValueDate), strings.Compare(a.company, b.company), cmp.Compare(a.v.LagDays, b.v.LagDays),
			bytes.Compare(a.v.SnapshotID[:], b.v.SnapshotID[:]), cmp.Compare(a.v.LineNo, b.v.LineNo))
	})
	months, done := snapshotOf[cashSnapshot](ctx, r, cashSnapshotsDataset)
	for _, sh := range all {
		if err = months.add(sh.v.ValueDate.Format(monthLayout), forecastRow(r.tenant, sh.company, sh.v)); err != nil {
			break
		}
	}
	return done(err)
}

// forecastRow is a line as the dataset holds it: a line of no bank has a
// null bank, one of no item a null item and name.
func forecastRow(tenant uuid.UUID, company string, sh ForecastShare) cashSnapshot {
	f := cashSnapshot{TenantID: tenant, SnapshotID: sh.SnapshotID, LineNo: sh.LineNo, Company: company, ValueDate: days(sh.ValueDate), LagDays: sh.LagDays,
		Source: sh.Source, Certainty: sh.Certainty, Blocked: sh.Blocked, AmountMinor: sh.Amount, Currency: sh.Currency,
		Exponent: int32(sh.Exponent)} //nolint:gosec // a currency's exponent is 0 to 4 (ref.currency)
	if sh.HouseBankCode != "" {
		bank := sh.HouseBankCode
		f.HouseBank = &bank
	}
	if sh.Item != "" {
		item, name := sh.Item, sh.ItemName
		f.LiquidityItem, f.LiquidityItemName = &item, &name
	}
	return f
}
