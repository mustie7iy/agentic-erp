package extract_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/analytics/extract"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// forecastRow is what a reader sees of a cash_snapshots file.
type forecastRow struct {
	TenantID          uuid.UUID `parquet:"tenant_id,uuid"`
	SnapshotID        uuid.UUID `parquet:"snapshot_id,uuid"`
	LineNo            int32     `parquet:"line_no"`
	Company           string    `parquet:"company"`
	ValueDate         int32     `parquet:"value_date,date"`
	LagDays           int32     `parquet:"lag_days"`
	HouseBank         *string   `parquet:"house_bank,optional"`
	FlowSource        string    `parquet:"flow_source"`
	Certainty         string    `parquet:"certainty"`
	Blocked           bool      `parquet:"blocked"`
	LiquidityItem     *string   `parquet:"liquidity_item,optional"`
	LiquidityItemName *string   `parquet:"liquidity_item_name,optional"`
	AmountMinor       int64     `parquet:"amount_minor"`
	Currency          string    `parquet:"currency"`
	Exponent          int32     `parquet:"exponent"`
}

// String is the row as the test compares it: its snapshot (by name), line,
// company, value date, lag, bank, source, certainty, blocked, item and
// amount.
func (r forecastRow) String(names map[uuid.UUID]string) string {
	bank, item, name := "-", "-", "-"
	if r.HouseBank != nil {
		bank = *r.HouseBank
	}
	if r.LiquidityItem != nil {
		item, name = *r.LiquidityItem, *r.LiquidityItemName
	}
	day := time.Unix(int64(r.ValueDate)*86400, 0).UTC().Format(time.DateOnly)
	return fmt.Sprintf("%s/%d %s %s +%d %s %s/%s blocked=%v %s %s %d %s/%d", names[r.SnapshotID], r.LineNo, r.Company, day, r.LagDays, bank, r.FlowSource,
		r.Certainty, r.Blocked, item, name, r.AmountMinor, r.Currency, r.Exponent)
}

// forecastRows are the dataset's rows of the manifest, each named by
// String.
func forecastRows(t *testing.T, e env, m extract.Manifest, names map[uuid.UUID]string) []string {
	t.Helper()
	rows := read[forecastRow](t, e, m, "cash_snapshots")
	out := make([]string, len(rows))
	for i, r := range rows {
		if r.TenantID != e.Tenant.ID {
			t.Errorf("row %d: tenant got %s, want %s", i, r.TenantID, e.Tenant.ID)
		}
		out[i] = r.String(names)
	}
	return out
}

// seedForecast gives company 2000 a daily snapshot of 1 April with one
// taken on demand the same day, which the daily one is preferred to; two
// taken on demand on 2 April, the later preferred; and company 1000 a
// daily snapshot of 1 April. Each preferred one has lines at lags 0, 7 and
// 30, a blocked supplier's, one of no item and no bank, and a bucket's sum
// after the document level.
func seedForecast(t *testing.T, e env) map[uuid.UUID]string {
	t.Helper()
	names := map[uuid.UUID]string{}
	snap := func(name string, le uuid.UUID, kind, date string, lines ...fixture.SnapshotLine) {
		names[e.Snapshot(t, le, kind, date, lines...)] = name
	}
	pay := func(day string, amount int64) fixture.SnapshotLine {
		return fixture.SnapshotLine{ValueDate: day, Source: "payable", Certainty: "expected", Item: "OP_SUPPLIERS", Amount: amount, Bank: true}
	}
	snap("de1", e.DE, "daily", "2026-04-01", pay("2026-04-01", -100), pay("2026-04-08", -700),
		fixture.SnapshotLine{ValueDate: "2026-04-08", Source: "payable_blocked", Certainty: "expected", Item: "OP_SUPPLIERS", Amount: -50},
		fixture.SnapshotLine{ValueDate: "2026-04-20", Source: "memo", Certainty: "planned", Amount: 2000},
		pay("2026-05-01", -3000), fixture.SnapshotLine{ValueDate: "2026-05-04", BucketEnd: "2026-05-10", Source: "payable", Certainty: "expected", Amount: -9})
	snap("de1 on demand", e.DE, "on_demand", "2026-04-01", pay("2026-04-01", -1))
	snap("de2 early", e.DE, "on_demand", "2026-04-02", pay("2026-04-02", -2))
	snap("de2", e.DE, "on_demand", "2026-04-02", pay("2026-04-09", -800))
	snap("us1", e.US, "daily", "2026-04-01", pay("2026-04-08", -70))
	return names
}

// TestCashForecast: the run reads every company's preferred snapshot of
// each date (its daily one, else the last taken on demand) at document
// level, each line with its lag, the tenant's alone, in the order of value
// date, company, lag, snapshot and line, one file per month of value
// dates; a run with nothing new writes nothing, and a snapshot taken
// changes the months it forecast alone.
func TestCashForecast(t *testing.T) {
	e := setupWith(t, fixture.CashItems)
	names := seedForecast(t, e)
	other := setup(t) // another tenant's snapshot, which row-level security keeps out
	other.Snapshot(t, other.DE, "daily", "2026-04-01", fixture.SnapshotLine{ValueDate: "2026-04-01", Source: "memo", Certainty: "planned", Amount: 777})

	first := e.run(t)
	m := e.manifest(t, first.ManifestKey)
	want := []string{
		"de1/1 2000 2026-04-01 +0 MAIN payable/expected blocked=false OP_SUPPLIERS Supplier payments -100 EUR/2",
		"us1/1 1000 2026-04-08 +7 MAIN payable/expected blocked=false OP_SUPPLIERS Supplier payments -70 USD/2",
		"de1/2 2000 2026-04-08 +7 MAIN payable/expected blocked=false OP_SUPPLIERS Supplier payments -700 EUR/2",
		"de1/3 2000 2026-04-08 +7 - payable_blocked/expected blocked=true OP_SUPPLIERS Supplier payments -50 EUR/2",
		"de2/1 2000 2026-04-09 +7 MAIN payable/expected blocked=false OP_SUPPLIERS Supplier payments -800 EUR/2",
		"de1/4 2000 2026-04-20 +19 - memo/planned blocked=false - - 2000 EUR/2",
		"de1/5 2000 2026-05-01 +30 MAIN payable/expected blocked=false OP_SUPPLIERS Supplier payments -3000 EUR/2",
	}
	if got := forecastRows(t, e, m, names); !slices.Equal(got, want) {
		t.Fatalf("rows: got %q, want %q", got, want)
	}
	files := m.Datasets["cash_snapshots"].Files
	if len(files) != 2 || !strings.Contains(files[0].Key, "/cash_snapshots/month=2026-04/") || !strings.Contains(files[1].Key, "/cash_snapshots/month=2026-05/") {
		t.Fatalf("files: got %+v, want one of April and one of May", files)
	}

	t.Run("run twice", func(t *testing.T) {
		if again := e.run(t); again.Published || again.ManifestKey != first.ManifestKey {
			t.Errorf("run: got %+v, want nothing new over %s", again, first.ManifestKey)
		}
	})
	t.Run("a snapshot taken", func(t *testing.T) {
		e.Snapshot(t, e.DE, "daily", "2026-05-02", fixture.SnapshotLine{ValueDate: "2026-05-09", Source: "memo", Certainty: "planned", Amount: 5})
		next := e.run(t)
		if !next.Published {
			t.Fatalf("run: got %+v, want a publication", next)
		}
		was, is := m.Datasets["cash_snapshots"].Files, e.manifest(t, next.ManifestKey).Datasets["cash_snapshots"].Files
		if len(is) != 2 || was[0] != is[0] || was[1].Key == is[1].Key || is[1].Rows != 2 {
			t.Errorf("files: got %+v after %+v, want April's kept and May's new with two rows", is, was)
		}
	})
}

// TestCashForecastFailures: every statement of the run failing, the cash
// context's reads among them, stops it; the cash context failing on the
// forecast alone stops it naming the dataset.
func TestCashForecastFailures(t *testing.T) {
	e := setupWith(t, fixture.CashItems)
	seedForecast(t, e)
	if res := failEach(t, e); !res.Published {
		t.Errorf("run: got %+v, want published", res)
	}
	t.Run("cash context", func(t *testing.T) {
		x := e.x
		x.Cash = failingForecast{x.Cash}
		if _, err := x.Run(context.Background(), e.Tenant.ID); !errors.Is(err, errCash) || !strings.Contains(err.Error(), "cash_snapshots of 1000") {
			t.Errorf("run: got %v, want the cash context's error for company 1000", err)
		}
	})
	t.Run("a file not stored", func(t *testing.T) {
		pgtest.Exec(t, `DELETE FROM analytics.snapshot WHERE tenant_id = $1`, e.Tenant.ID) // a first run, which writes every month
		x := e.x
		x.Store = failingStore{Store: e.server.Store(), method: "put", part: "cash_snapshots/month=2026-04"}
		if _, err := x.Run(context.Background(), e.Tenant.ID); err == nil || !strings.Contains(err.Error(), "cash_snapshots") {
			t.Errorf("run: got %v, want the April file's write failed", err)
		}
	})
}

// failingForecast is the cash context failing on the forecast alone.
type failingForecast struct{ extract.Cash }

func (failingForecast) ExtractForecastShares(context.Context, uuid.UUID) ([]extract.ForecastShare, error) {
	return nil, errCash
}
