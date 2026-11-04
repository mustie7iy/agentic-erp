package assets_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/assets"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// capitalised is a capitalised asset of company 2000 of the class (body
// as e.body's extra fields) whose 1,200.00 EUR clearing line, posted on
// 10 June 2026, was capitalised, with the depreciation start given, if
// any.
func (e assetEnv) capitalised(t *testing.T, class, extra, start string) string {
	t.Helper()
	entry := e.post(t, "2000", "2026-06-10", "EUR", nil, line{"169000", "1200.00"}, line{"100000", "-1200.00"})
	a, tag := e.create(t, e.body(e.DE, class, extra))
	if start != "" {
		e.Must(200, "PATCH", "/v1/assets/"+a["id"].(string), fmt.Sprintf(`{"depreciation_start": %q}`, start), tag)
	}
	e.acquire(t, a["id"], entry, 1, "")
	return a["id"].(string)
}

// TestYearTotalsInKeyOrder: two postings touching the same two classes
// in opposite orders, a depreciation run's batch writing a MACHINES
// asset's movement before a LAND asset's (by asset id) and an asset
// transfer from a LAND asset to a MACHINES asset, both wait behind a
// transaction holding the LAND class row, the first in key order; neither
// holds the MACHINES row meanwhile, as it would if it added to the totals
// in the order it wrote its movements and the two could deadlock; once
// the LAND row is free both commit, and the totals are the movements'
// sums.
func TestYearTotalsInKeyOrder(t *testing.T) {
	e := newRuns(t)
	mill := e.capitalised(t, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin), "")
	field := e.capitalised(t, "LAND", fmt.Sprintf(`, "profit_center_id": %q`, e.c.Profit), "")
	// Not depreciated in June, so the run leaves them to the transfer.
	plot := e.capitalised(t, "LAND", fmt.Sprintf(`, "profit_center_id": %q`, e.c.Profit), "2026-12-01")
	press := e.capitalised(t, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin), "2026-12-01")
	if mill > field {
		t.Fatalf("the mill's id %s sorts after the field's %s", mill, field)
	}
	_, run := e.start(t, e.l0, 6)
	var batches []int32
	if err := e.step(t, func(ctx context.Context, h *assets.Endpoints) error {
		var err error
		batches, err = h.PlanDepreciationRun(ctx, run)
		return err
	}); err != nil || len(batches) != 1 {
		t.Fatalf("plan: got %v %v, want one batch", batches, err)
	}
	ctx := context.Background()
	row := `SELECT FROM assets.class_year_total WHERE tenant_id = $1 AND legal_entity_id = $2 AND ledger_id = $3 AND fiscal_year = 2026
		AND class_code = $4 FOR UPDATE`
	holder, err := pgtest.Admin(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }() // released below; a second rollback has nothing to undo
	if _, err := holder.Exec(ctx, row, e.Tenant.ID, e.DE, e.l0, "LAND"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var batchErr error
	var transferred int
	wg.Go(func() {
		batchErr = e.step(t, func(ctx context.Context, h *assets.Endpoints) error {
			return h.PostDepreciationBatch(ctx, run, batches[0])
		})
	})
	wg.Go(func() {
		transferred, _, _ = e.clocked.Do("POST", "/v1/asset-transfers", fmt.Sprintf(`{"asset_id": %q, "kind": "asset", "to_asset_id": %q,
			"share": {"percent": "50"}}`, plot, press), "")
	})
	pgtest.Eventually(t, 4*time.Second, func() bool {
		var waiting int
		pgtest.Scan(t, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'
			AND query LIKE '%INSERT INTO assets.class_year_total%'`, nil, &waiting)
		return waiting == 2
	}, "both postings waiting for the LAND class row")
	probe, err := pgtest.Admin(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := probe.Exec(ctx, strings.Replace(row, "FOR UPDATE", "FOR UPDATE NOWAIT", 1), e.Tenant.ID, e.DE, e.l0, "MACHINES"); err != nil {
		t.Errorf("the MACHINES class row while both wait for LAND's: got %v, want it free", err)
	}
	_ = probe.Rollback(ctx)  // it only read; nothing to undo
	_ = holder.Rollback(ctx) // frees the LAND row; it changed nothing
	wg.Wait()
	if batchErr != nil {
		t.Errorf("the batch: got %v, want it posted", batchErr)
	}
	want(t, "the transfer", transferred, 201)
	want(t, "year totals that differ from their movements' sums", totalsDiffer(t, e.Tenant.ID), 0)
}

// TestAssetHistorySheetDatabaseFailures calls the sheet by class and by
// asset with each of its database statements failing in turn.
func TestAssetHistorySheetDatabaseFailures(t *testing.T) {
	e := newRuns(t)
	e.depreciable(t, "Mill")
	pages, err := pagination.NewCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	h := app.Assets(app.Config{Pages: pages})
	for name, by := range map[string]string{"by class": "class", "by asset": "asset"} {
		fn := func(ctx context.Context) error {
			_, err := h.GetAssetHistorySheet(ctx, gen.GetAssetHistorySheetParams{LegalEntityID: e.DE, FiscalYear: 2026,
				GroupBy: gen.NewOptString(by)})
			return err
		}
		t.Run(name, func(t *testing.T) {
			if err := fn(context.Background()); err == nil || !strings.Contains(err.Error(), "outside a unit of work") {
				t.Errorf("outside a unit of work: got %v, want it refused", err)
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, fn)
		})
	}
}

// TestAssetHistorySheetSettingsLost: a class the active configuration no
// longer has keeps its row, without a name, and a class whose earlier
// years net to nothing has none; a configuration that does not load fails
// the sheet.
func TestAssetHistorySheetSettingsLost(t *testing.T) {
	e := newRuns(t)
	e.depreciable(t, "Mill")
	// Bought and scrapped in 2025, so it opens 2026 with nothing.
	pgtest.Exec(t, `INSERT INTO assets.class_year_total (tenant_id, legal_entity_id, ledger_id, fiscal_year, class_code, acquisitions, retired_cost)
		VALUES ($1, $2, $3, 2025, 'LAND', 1000, -1000)`, e.Tenant.ID, e.DE, e.l0)
	h := app.Assets(app.Config{})
	h.Settings = classGone{configuration.Settings{}}
	var sheet gen.GetAssetHistorySheetRes
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		var err error
		sheet, err = h.GetAssetHistorySheet(ctx, gen.GetAssetHistorySheetParams{LegalEntityID: e.DE, FiscalYear: 2026})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	items := sheet.(*gen.AssetHistorySheet).Items
	want(t, "the one class with values, the configuration lost", fmt.Sprint(len(items), " ", items[0].ClassCode, " ", items[0].ClassName.Set), "1 MACHINES false")
	h.Settings = settingsLost{}
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.GetAssetHistorySheet(ctx, gen.GetAssetHistorySheetParams{LegalEntityID: e.DE, FiscalYear: 2026})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "configuration lost") {
		t.Errorf("a configuration that does not load: got %v, want it to fail the sheet", err)
	}
}
