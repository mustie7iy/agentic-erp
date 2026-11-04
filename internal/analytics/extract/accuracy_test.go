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

// accuracyRow is what a reader sees of a cash_accuracy file.
type accuracyRow struct {
	TenantID          uuid.UUID `parquet:"tenant_id,uuid"`
	CellID            uuid.UUID `parquet:"cell_id,uuid"`
	Company           string    `parquet:"company"`
	ValueDate         int32     `parquet:"value_date,date"`
	LagDays           int32     `parquet:"lag_days"`
	Figure            string    `parquet:"figure"`
	LiquidityItem     *string   `parquet:"liquidity_item,optional"`
	LiquidityItemName *string   `parquet:"liquidity_item_name,optional"`
	AmountMinor       int64     `parquet:"amount_minor"`
	Currency          string    `parquet:"currency"`
	Exponent          int32     `parquet:"exponent"`
}

// accuracyRows are the dataset's rows of the manifest as "company value
// date +lag item name figure amount currency/exponent", and the rows.
func accuracyRows(t *testing.T, e env, m extract.Manifest) ([]string, []accuracyRow) {
	t.Helper()
	rows := read[accuracyRow](t, e, m, "cash_accuracy")
	out := make([]string, len(rows))
	for i, r := range rows {
		if r.TenantID != e.Tenant.ID {
			t.Errorf("row %d: tenant got %s, want %s", i, r.TenantID, e.Tenant.ID)
		}
		item, name := "-", "-"
		if r.LiquidityItem != nil {
			item, name = *r.LiquidityItem, *r.LiquidityItemName
		}
		day := time.Unix(int64(r.ValueDate)*86400, 0).UTC().Format(time.DateOnly)
		out[i] = fmt.Sprintf("%s %s +%d %s %s %s %d %s/%d", r.Company, day, r.LagDays, item, name, r.Figure, r.AmountMinor, r.Currency, r.Exponent)
	}
	return out, rows
}

// TestCashAccuracy: the run reads every company's accuracy cells (seedForecast's
// snapshots against statement lines of 1 and 8 April and 1 May), three
// rows per cell, the tenant's alone, in the order of value date, company,
// lag, item and figure, one file per month of value dates; a cell's rows
// share its id. A run with nothing new writes nothing.
func TestCashAccuracy(t *testing.T) {
	e := setupWith(t, fixture.CashItems)
	seedForecast(t, e)
	e.Statement(t, e.DE, fixture.StatementLine{ValueDate: "2026-04-01", Amount: -100}, fixture.StatementLine{ValueDate: "2026-04-08", Amount: -650},
		fixture.StatementLine{ValueDate: "2026-05-01", Amount: 40})
	other := setup(t) // another tenant's snapshot, which row-level security keeps out
	other.Snapshot(t, other.DE, "daily", "2026-04-01", fixture.SnapshotLine{ValueDate: "2026-04-01", Source: "memo", Certainty: "planned", Amount: 777})

	first := e.run(t)
	m := e.manifest(t, first.ManifestKey)
	var want []string
	for _, c := range []struct {
		cell                       string
		variance, actual, forecast int64
	}{
		{"2000 2026-04-01 +0 OP_SUPPLIERS Supplier payments", 0, -100, -100},
		{"1000 2026-04-08 +7 OP_SUPPLIERS Supplier payments", 70, 0, -70},
		{"2000 2026-04-08 +7 OP_SUPPLIERS Supplier payments", 50, -650, -700},
		{"2000 2026-04-09 +7 OP_SUPPLIERS Supplier payments", 800, 0, -800},
		{"2000 2026-05-01 +30 OP_SUPPLIERS Supplier payments", 3000, 0, -3000},
		{"2000 2026-05-01 +30 REC_CUSTOMERS Customer receipts", 40, 40, 0},
	} {
		cur := map[bool]string{true: "USD/2", false: "EUR/2"}[strings.HasPrefix(c.cell, "1000")]
		want = append(want, fmt.Sprintf("%s variance %d %s", c.cell, c.variance, cur), fmt.Sprintf("%s absolute_variance %d %s", c.cell, max(c.variance, -c.variance), cur),
			fmt.Sprintf("%s absolute_forecast %d %s", c.cell, max(c.forecast, -c.forecast), cur))
	}
	got, rows := accuracyRows(t, e, m)
	if !slices.Equal(got, want) {
		t.Fatalf("rows: got %q, want %q", got, want)
	}
	if rows[0].CellID != rows[2].CellID || rows[0].CellID == rows[3].CellID {
		t.Errorf("cell ids: got %s, %s and %s, want a cell's rows to share its own", rows[0].CellID, rows[2].CellID, rows[3].CellID)
	}
	files := m.Datasets["cash_accuracy"].Files
	if len(files) != 2 || !strings.Contains(files[0].Key, "/cash_accuracy/month=2026-04/") || !strings.Contains(files[1].Key, "/cash_accuracy/month=2026-05/") {
		t.Fatalf("files: got %+v, want one of April and one of May", files)
	}
	t.Run("run twice", func(t *testing.T) {
		if again := e.run(t); again.Published || again.ManifestKey != first.ManifestKey {
			t.Errorf("run: got %+v, want nothing new over %s", again, first.ManifestKey)
		}
	})
}

// TestCashAccuracyFailures: every statement of the run failing, the cash
// context's reads among them, stops it; the cash context failing on the
// accuracy alone stops it naming the dataset; a file not stored stops it.
func TestCashAccuracyFailures(t *testing.T) {
	e := setupWith(t, fixture.CashItems)
	seedForecast(t, e)
	e.Statement(t, e.DE, fixture.StatementLine{ValueDate: "2026-04-01", Amount: -100})
	if res := failEach(t, e); !res.Published {
		t.Errorf("run: got %+v, want published", res)
	}
	t.Run("cash context", func(t *testing.T) {
		x := e.x
		x.Cash = failingAccuracy{x.Cash}
		if _, err := x.Run(context.Background(), e.Tenant.ID); !errors.Is(err, errCash) || !strings.Contains(err.Error(), "cash_accuracy of 1000") {
			t.Errorf("run: got %v, want the cash context's error for company 1000", err)
		}
	})
	t.Run("a file not stored", func(t *testing.T) {
		pgtest.Exec(t, `DELETE FROM analytics.snapshot WHERE tenant_id = $1`, e.Tenant.ID) // a first run, which writes every month
		x := e.x
		x.Store = failingStore{Store: e.server.Store(), method: "put", part: "cash_accuracy/month=2026-04"}
		if _, err := x.Run(context.Background(), e.Tenant.ID); err == nil || !strings.Contains(err.Error(), "cash_accuracy") {
			t.Errorf("run: got %v, want the April file's write failed", err)
		}
	})
}

// failingAccuracy is the cash context failing on the accuracy alone.
type failingAccuracy struct{ extract.Cash }

func (failingAccuracy) ExtractAccuracyCells(context.Context, uuid.UUID) ([]extract.AccuracyCell, error) {
	return nil, errCash
}
