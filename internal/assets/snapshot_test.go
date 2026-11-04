package assets_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
)

// interleaved is a request's transaction that runs change, once, before
// the first query whose SQL holds marker: a posting committed between
// the request's statements, with no wait. Only Query is watched, so the
// marker must name a statement read by Query, not QueryRow or Exec.
type interleaved struct {
	pgx.Tx
	marker string
	once   *sync.Once
	change func()
}

func (x interleaved) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, x.marker) {
		x.once.Do(x.change)
	}
	return x.Tx.Query(ctx, sql, args...)
}

// between is a client of an API whose request transactions run change,
// once, before the first query holding marker; it reports whether change
// ran. The change posts through e's own clients, which it does not reach.
func (e assetEnv) between(t *testing.T, marker string, change func()) (apitest.Client, func() bool) {
	t.Helper()
	var once sync.Once
	var ran bool
	c := e.Client
	c.API = apitest.NewWith(t, func(cfg *app.Config) {
		pool := cfg.Pool
		cfg.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			if err != nil {
				return nil, err
			}
			return interleaved{Tx: tx, marker: marker, once: &once, change: func() { change(); ran = true }}, nil
		}
	})
	return c, func() bool { return ran }
}

// TestPreviewInOneSnapshot: a sale posted while a preview reads, after its
// selection read what the assets have booked and before it reads their
// moves, is not seen by it: the preview's totals are those before the
// sale, not the sale's moves against the depreciation booked before its
// catch-up (x-snapshot: one REPEATABLE READ transaction, ADR-0057 As
// built, review:T57f3).
func TestPreviewInOneSnapshot(t *testing.T) {
	e := newRuns(t)
	mill, _ := e.depreciable(t, "Mill")
	_, run := e.start(t, e.l0, 6)
	e.carry(t, run)
	before := previewTotals(e.preview(t, e.Client, e.l0, 6, ""))
	want(t, "June's preview after its run", before, "1 10.00 10.00 0.00")

	c, sold := e.between(t, "kind <> 'depreciation'", func() {
		e.retire(t, mill, `"kind": "sale", "proceeds": {"amount": "700.00", "currency": "EUR"}, "share": {"percent": "50"}`)
	})
	during := previewTotals(e.preview(t, c, e.l0, 6, ""))
	if !sold() {
		t.Fatal("the sale: got none posted during the preview, want one")
	}
	want(t, "the preview read while half the mill was sold", during, before)
	want(t, "the preview after the sale", previewTotals(e.preview(t, e.Client, e.l0, 6, "")),
		"1 5.00 0.00 5.00; 620000 Depreciation 1000 PC1 5.00; 160900 Machinery, accumulated depreciation <nil> PC1 -5.00")
}

// TestAssetHistorySheetInOneSnapshot: an asset scrapped while the sheet is
// read by asset, after it summed the classes' totals and before it read
// the assets' rows, is not seen by it: the rows add up to the totals
// (x-snapshot, review:T57f3).
func TestAssetHistorySheetInOneSnapshot(t *testing.T) {
	e := newRuns(t)
	mill, _ := e.depreciable(t, "Mill")
	e.depreciable(t, "Lathe")
	query := "/v1/asset-history-sheet?legal_entity_id=" + e.DE.String() + "&fiscal_year=2026&group_by=asset"
	before := e.sheet(t, "&fiscal_year=2026&group_by=asset")

	c, scrapped := e.between(t, "FROM assets.year_total t", func() { e.retire(t, mill, `"kind": "scrapping"`) })
	during, _ := c.Must(200, "GET", query, "", "")
	if !scrapped() {
		t.Fatal("the scrapping: got none posted during the sheet's read, want it")
	}
	want(t, "the rows read while the mill was scrapped", strings.Join(sheetRows(during), "; "), strings.Join(sheetRows(before), "; "))
	want(t, "the totals read while the mill was scrapped", sheetLine(during["totals"].(map[string]any)), sheetLine(before["totals"].(map[string]any)))
	after := e.sheet(t, "&fiscal_year=2026&group_by=asset")
	if got := strings.Join(sheetRows(after), "; "); got == strings.Join(sheetRows(before), "; ") {
		t.Errorf("the rows after the scrapping: got %s, want the mill's retirement", got)
	}
}
