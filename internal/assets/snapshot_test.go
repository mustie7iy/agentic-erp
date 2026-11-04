package assets_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/assets"
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

// snapshotRead is a read of the assets context and a change that alters
// what it returns: the read's path, the SQL of the statement before which
// the change is committed, and the change.
type snapshotRead struct {
	path, marker string
	change       func()
}

// clearingLine posts company 2000's 1,200.00 EUR line on the clearing
// account and creates a planned asset that may be capitalised from it.
func (e assetEnv) clearingLine(t *testing.T) (uuid.UUID, string) {
	t.Helper()
	entry := e.post(t, "2000", "2026-06-10", "EUR", nil, line{"169000", "1200.00"}, line{"100000", "-1200.00"})
	a, _ := e.create(t, fmt.Sprintf(`{"legal_entity_id": %q, "class_code": "MACHINES", "description": "Mill", "cost_center_id": %q}`, e.DE, e.c.Admin))
	return entry, a["id"].(string)
}

// TestReadsInOneSnapshot: every read of an asset, its schedule, its
// documents, the depreciation runs and the clearing worklist that issues
// more than one statement returns one database state: a posting
// committed between its statements is not seen by it, whose response is
// the one before the posting, which the posting does alter (x-snapshot:
// one REPEATABLE READ transaction, ADR-0057 As built, review:T57f). With
// an operation's mark removed its case fails.
func TestReadsInOneSnapshot(t *testing.T) {
	const values, entries, batches = "sum(residual)", "FROM assets.document_entry WHERE document_kind", "cardinality(asset_ids)"
	run := func(path string) func(t *testing.T, e assetEnv) snapshotRead {
		return func(t *testing.T, e assetEnv) snapshotRead {
			t.Helper()
			mill, _ := e.depreciable(t, "Mill")
			_, r := e.start(t, e.l0, 6)
			return snapshotRead{strings.NewReplacer("{asset}", mill, "{run}", r.String()).Replace(path), values, func() { e.carry(t, r) }}
		}
	}
	planned := func(path string) func(t *testing.T, e assetEnv) snapshotRead {
		return func(t *testing.T, e assetEnv) snapshotRead {
			t.Helper()
			e.depreciable(t, "Mill")
			_, r := e.start(t, e.l0, 6)
			return snapshotRead{strings.ReplaceAll(path, "{run}", r.String()), batches, func() {
				if err := e.step(t, func(ctx context.Context, h *assets.Endpoints) error {
					_, err := h.PlanDepreciationRun(ctx, r)
					return err
				}); err != nil {
					t.Errorf("plan the run: %v", err)
				}
			}}
		}
	}
	acquired := func(path, marker string) func(t *testing.T, e assetEnv) snapshotRead {
		return func(t *testing.T, e assetEnv) snapshotRead {
			t.Helper()
			entry, a := e.clearingLine(t)
			return snapshotRead{strings.NewReplacer("{asset}", a, "{company}", e.DE.String()).Replace(path), marker, func() { e.acquire(t, a, entry, 1, "") }}
		}
	}
	document := func(path string, post func(t *testing.T, e assetEnv, mill string) (map[string]any, func(map[string]any))) func(t *testing.T, e assetEnv) snapshotRead {
		return func(t *testing.T, e assetEnv) snapshotRead {
			t.Helper()
			mill, _ := e.depreciable(t, "Mill")
			q, reverse := post(t, e, mill)
			return snapshotRead{strings.NewReplacer("{asset}", mill, "{id}", q["id"].(string)).Replace(path), entries, func() { reverse(q) }}
		}
	}
	acquisition := func(t *testing.T, e assetEnv, mill string) (map[string]any, func(map[string]any)) {
		t.Helper()
		q, _ := e.Must(200, "GET", "/v1/asset-acquisitions?asset_id="+mill, "", "")
		return q["items"].([]any)[0].(map[string]any), func(q map[string]any) { e.reverse(t, q, "") }
	}
	retirement := func(t *testing.T, e assetEnv, mill string) (map[string]any, func(map[string]any)) {
		t.Helper()
		return e.retire(t, mill, `"kind": "scrapping"`), func(q map[string]any) { e.unretire(t, q, `{"reason": "r"}`) }
	}
	transfer := func(t *testing.T, e assetEnv, mill string) (map[string]any, func(map[string]any)) {
		t.Helper()
		other := e.otherCenter(t)
		return e.transfer(t, mill, fmt.Sprintf(`"kind": "center", "cost_center_id": %q`, other)), func(q map[string]any) { e.untransfer(t, q, `{"reason": "r"}`) }
	}
	for name, read := range map[string]func(t *testing.T, e assetEnv) snapshotRead{
		"getAsset":               run("/v1/assets/{asset}"),
		"listAssets":             run("/v1/assets?class_code=MACHINES"),
		"getAssetSchedule":       acquired("/v1/assets/{asset}/schedule?fiscal_year=2026", "ORDER BY asset_id, seq"),
		"listAssetClearingItems": acquired("/v1/asset-clearing-items?legal_entity_id={company}&fiscal_year=2026", "q.source_line_no, q.id, q.asset_id"),
		"getAssetAcquisition":    document("/v1/asset-acquisitions/{id}", acquisition),
		"listAssetAcquisitions":  document("/v1/asset-acquisitions?asset_id={asset}", acquisition),
		"getAssetRetirement":     document("/v1/asset-retirements/{id}", retirement),
		"listAssetRetirements":   document("/v1/asset-retirements?asset_id={asset}", retirement),
		"getAssetTransfer":       document("/v1/asset-transfers/{id}", transfer),
		"listAssetTransfers":     document("/v1/asset-transfers?asset_id={asset}", transfer),
		"getDepreciationRun":     planned("/v1/depreciation-runs/{run}"),
		"listDepreciationRuns":   planned("/v1/depreciation-runs?fiscal_year=2026"),
	} {
		t.Run(name, func(t *testing.T) {
			e := newRuns(t)
			r := read(t, e)
			before, _ := e.Must(200, "GET", r.path, "", "")
			c, changed := e.between(t, r.marker, r.change)
			during, _ := c.Must(200, "GET", r.path, "", "")
			if !changed() {
				t.Fatal("the posting: got none committed during the read, want one")
			}
			want(t, "the response read while the posting was committed", during, before)
			if after, _ := e.Must(200, "GET", r.path, "", ""); fmt.Sprint(after) == fmt.Sprint(before) {
				t.Errorf("the response after the posting: got %v, the one before it, want the posting's", after)
			}
		})
	}
}
