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
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// cashRow is what a reader sees of a cash_actuals file.
type cashRow struct {
	TenantID          uuid.UUID `parquet:"tenant_id,uuid"`
	StatementID       uuid.UUID `parquet:"statement_id,uuid"`
	LineNo            int32     `parquet:"line_no"`
	Company           string    `parquet:"company"`
	ValueDate         int32     `parquet:"value_date,date"`
	HouseBank         string    `parquet:"house_bank"`
	Match             string    `parquet:"match"`
	LiquidityItem     *string   `parquet:"liquidity_item,optional"`
	LiquidityItemName *string   `parquet:"liquidity_item_name,optional"`
	AmountMinor       int64     `parquet:"amount_minor"`
	Currency          string    `parquet:"currency"`
	Exponent          int32     `parquet:"exponent"`
}

// String is the row as the test compares it: its line, company, value
// date, bank, match, item and amount.
func (r cashRow) String() string {
	item, name := "-", "-"
	if r.LiquidityItem != nil {
		item, name = *r.LiquidityItem, *r.LiquidityItemName
	}
	day := time.Unix(int64(r.ValueDate)*86400, 0).UTC().Format(time.DateOnly)
	return fmt.Sprintf("%d %s %s %s %s %s %s %d %s/%d", r.LineNo, r.Company, day, r.HouseBank, r.Match, item, name, r.AmountMinor, r.Currency, r.Exponent)
}

// cashRows are the dataset's rows of the manifest, each named by String,
// with the statement each is of.
func cashRows(t *testing.T, e env, m extract.Manifest) ([]string, []cashRow) {
	t.Helper()
	rows := read[cashRow](t, e, m, "cash_actuals")
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.String()
	}
	return out, rows
}

// TestCashActuals: the run reads every company's bank statement lines as
// the cash actuals classify them, one row per line and liquidity item, the
// tenant's alone, in the order of value date, company, statement, line and
// item, one file per month of value dates; a line reclassified changes its
// month's file alone, and a run with nothing new writes nothing.
func TestCashActuals(t *testing.T) {
	e := setupWith(t, fixture.CashItems)
	de := e.Statement(t, e.DE, fixture.StatementLine{ValueDate: "2026-03-31", Amount: 50000},
		fixture.StatementLine{ValueDate: "2026-04-02", Amount: -1500, Rule: "NCHG"}, fixture.StatementLine{ValueDate: "2026-04-02", Amount: -20000})
	e.Statement(t, e.US, fixture.StatementLine{ValueDate: "2026-04-01", Amount: 10000})
	other := setup(t) // another tenant's line, which row-level security keeps out
	other.Statement(t, other.DE, fixture.StatementLine{ValueDate: "2026-03-30", Amount: 777})

	first := e.run(t)
	m := e.manifest(t, first.ManifestKey)
	got, rows := cashRows(t, e, m)
	want := []string{
		"1 2000 2026-03-31 MAIN unmatched REC_CUSTOMERS Customer receipts 50000 EUR/2",
		"1 1000 2026-04-01 MAIN unmatched REC_CUSTOMERS Customer receipts 10000 USD/2",
		"2 2000 2026-04-02 MAIN posting_rule BANK_CHARGES Bank charges -1500 EUR/2",
		"3 2000 2026-04-02 MAIN unmatched OP_SUPPLIERS Supplier payments -20000 EUR/2",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("rows: got %q, want %q", got, want)
	}
	for _, r := range rows {
		if r.TenantID != e.Tenant.ID {
			t.Errorf("row %s: tenant got %s, want %s", r, r.TenantID, e.Tenant.ID)
		}
	}
	files := m.Datasets["cash_actuals"].Files
	if len(files) != 2 || !strings.Contains(files[0].Key, "/cash_actuals/month=2026-03/") || !strings.Contains(files[1].Key, "/cash_actuals/month=2026-04/") {
		t.Fatalf("files: got %+v, want one of March and one of April", files)
	}

	t.Run("run twice", func(t *testing.T) {
		if again := e.run(t); again.Published || again.ManifestKey != first.ManifestKey {
			t.Errorf("run: got %+v, want nothing new over %s", again, first.ManifestKey)
		}
	})
	t.Run("reclassified", func(t *testing.T) {
		match := uuid.Must(uuid.NewV7())
		pgtest.Exec(t, `INSERT INTO payments.statement_match (tenant_id, id, statement_id, line_no, kind, rule, reason, status, created_by, updated_by)
			VALUES ($1, $2, $3, 3, 'manual', NULL, 'Not a supplier payment', 'excluded', $4, $4)`, e.Tenant.ID, match, de, e.Tenant.Human.ID)
		next := e.run(t)
		if !next.Published {
			t.Fatalf("run: got %+v, want a publication", next)
		}
		n := e.manifest(t, next.ManifestKey)
		if got, _ := cashRows(t, e, n); got[3] != "3 2000 2026-04-02 MAIN excluded OP_SUPPLIERS Supplier payments -20000 EUR/2" {
			t.Errorf("the line excluded: got %q, want it classified by excluded", got[3])
		}
		was, is := m.Datasets["cash_actuals"].Files, n.Datasets["cash_actuals"].Files
		if was[0] != is[0] || was[1].Key == is[1].Key {
			t.Errorf("files: got %+v after %+v, want March's kept and April's new", is, was)
		}
	})
}

// TestCashActualsOfNoItem: without liquidity items a line's share is of
// no item, whose code and name the dataset holds as null.
func TestCashActualsOfNoItem(t *testing.T) {
	e := setup(t)
	e.Statement(t, e.DE, fixture.StatementLine{ValueDate: "2026-05-04", Amount: -300})
	got, rows := cashRows(t, e, e.manifest(t, e.run(t).ManifestKey))
	if want := []string{"1 2000 2026-05-04 MAIN unmatched - - -300 EUR/2"}; !slices.Equal(got, want) || rows[0].LiquidityItemName != nil {
		t.Errorf("rows: got %q, want %q with a null name", got, want)
	}
}

// TestCashActualsFailures: every statement of the run failing, the cash
// context's reads among them, stops it.
func TestCashActualsFailures(t *testing.T) {
	e := setupWith(t, fixture.CashItems)
	e.Statement(t, e.DE, fixture.StatementLine{ValueDate: "2026-03-31", Amount: 50000}, fixture.StatementLine{ValueDate: "2026-04-02", Amount: -1500, Rule: "NCHG"})
	if res := failEach(t, e); !res.Published {
		t.Errorf("run: got %+v, want published", res)
	}
	t.Run("a file not stored", func(t *testing.T) {
		pgtest.Exec(t, `DELETE FROM analytics.snapshot WHERE tenant_id = $1`, e.Tenant.ID) // a first run, which writes every month
		x := e.x
		x.Store = failingStore{Store: e.server.Store(), method: "put", part: "cash_actuals/month=2026-03"}
		if _, err := x.Run(context.Background(), e.Tenant.ID); !errors.Is(err, blobstore.ErrLocked) || !strings.Contains(err.Error(), "cash_actuals") {
			t.Errorf("run: got %v, want the March file's write failed", err)
		}
	})
	t.Run("without the cash context", func(t *testing.T) {
		x := e.x
		x.Cash = nil
		res, err := x.Run(context.Background(), e.Tenant.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := e.manifest(t, res.ManifestKey).Datasets["cash_actuals"]; ok {
			t.Error("manifest: got cash_actuals, want none without the cash context")
		}
	})
	t.Run("cash context", func(t *testing.T) {
		x := e.x
		x.Cash = failingCash{}
		if _, err := x.Run(context.Background(), e.Tenant.ID); !errors.Is(err, errCash) || !strings.Contains(err.Error(), "cash_actuals of 2000") {
			t.Errorf("run: got %v, want the cash context's error for company 2000", err)
		}
	})
}

var errCash = errors.New("cash context failed")

// failingCash is a cash context that fails.
type failingCash struct{}

func (failingCash) ExtractActualShares(context.Context, uuid.UUID) ([]extract.CashShare, error) {
	return nil, errCash
}

func (failingCash) ExtractForecastShares(context.Context, uuid.UUID) ([]extract.ForecastShare, error) {
	return nil, errCash
}

func (failingCash) ExtractAccuracyCells(context.Context, uuid.UUID) ([]extract.AccuracyCell, error) {
	return nil, errCash
}
