package assets_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/nfr"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestAssetHistorySheetBudget checks 01-requirements §1.3's budget for the
// asset history sheet of one company, ledger and fiscal year (p95 1 s,
// ADR-0057 §9) at its design scale: 100,000 assets of two classes, each
// with year totals in the leading ledger for ten years (2017 to 2026,
// a million rows), read for 2026 by class, by asset (the first page and
// pages further on) and by asset of one class. The rows are written
// straight into the tables, without the triggers and checks a posting
// runs (as TestClearingItemsBudget seeds its lines), within a deadline of
// their own.
func TestAssetHistorySheetBudget(t *testing.T) {
	if testing.Short() || raceDetector {
		t.Skip("performance budget: runs without -short and without the race detector (make perf, CI)")
	}
	e := newRuns(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	admin := pgtest.Admin(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	start := time.Now()
	exec(`SET session_replication_role = replica`)
	exec(`CREATE TEMP TABLE seed AS SELECT n, gen_random_uuid() AS id, CASE WHEN n % 4 = 0 THEN 'LAND' ELSE 'MACHINES' END AS class
		FROM generate_series(1, 100000) n`)
	exec(`INSERT INTO assets.asset (tenant_id, id, legal_entity_id, asset_number, class_code, description, status, capitalised_on, last_seq, created_by,
			updated_by)
		SELECT $1, s.id, $2, s.n::text, s.class, 'Asset ' || s.n, 'active', make_date(2017 + s.n % 10, 1, 1), 20, $3, $3 FROM seed s`,
		e.Tenant.ID, e.DE, e.Tenant.Human.ID)
	// From its first year on each asset is acquired, depreciated every year,
	// and one in five partly retired or transferred in its last year.
	exec(`INSERT INTO assets.year_total (tenant_id, ledger_id, asset_id, fiscal_year, legal_entity_id, class_code, acquisitions, retired_cost,
			transferred_cost, depreciation, retired_accumulated, transferred_accumulated)
		SELECT $1, $2, s.id, y, $3, s.class, CASE WHEN y = 2017 + s.n % 10 THEN 1200000 ELSE 0 END,
			CASE WHEN y = 2026 AND s.n % 5 = 0 THEN -300000 ELSE 0 END, CASE WHEN y = 2026 AND s.n % 5 = 1 THEN -300000 ELSE 0 END,
			CASE WHEN y >= 2017 + s.n % 10 THEN -120000 ELSE 0 END, CASE WHEN y = 2026 AND s.n % 5 = 0 THEN 30000 ELSE 0 END,
			CASE WHEN y = 2026 AND s.n % 5 = 1 THEN 30000 ELSE 0 END
		FROM seed s, generate_series(2017, 2026) y`, e.Tenant.ID, e.l0, e.DE)
	exec(`INSERT INTO assets.class_year_total (tenant_id, legal_entity_id, ledger_id, fiscal_year, class_code, acquisitions, retired_cost,
			transferred_cost, depreciation, retired_accumulated, transferred_accumulated)
		SELECT tenant_id, legal_entity_id, ledger_id, fiscal_year, class_code, sum(acquisitions), sum(retired_cost), sum(transferred_cost),
			sum(depreciation), sum(retired_accumulated), sum(transferred_accumulated)
		FROM assets.year_total WHERE tenant_id = $1 GROUP BY 1, 2, 3, 4, 5`, e.Tenant.ID)
	exec(`RESET session_replication_role`)
	exec(`ANALYZE assets.asset, assets.year_total, assets.class_year_total`)
	t.Logf("seeded 100,000 assets with 1,000,000 year totals in %v", time.Since(start))
	path := "/v1/asset-history-sheet?legal_entity_id=" + e.DE.String() + "&fiscal_year=2026"
	later, _ := e.Must(200, "GET", path+"&group_by=asset&page_size=500", "", "")
	for range 20 { // the 21st page of 500: assets 10,001 on by number
		later, _ = e.Must(200, "GET", path+"&group_by=asset&page_size=500&page_token="+later["next_page_token"].(string), "", "")
	}
	for name, query := range map[string]string{"by class": "", "by asset": "&group_by=asset", "by asset of a class": "&group_by=asset&class=LAND",
		"by asset, a later page": "&group_by=asset&page_size=500&page_token=" + later["next_page_token"].(string)} {
		var times []time.Duration
		for range 10 {
			start := time.Now()
			sheet, _ := e.Must(200, "GET", path+query, "", "")
			times = append(times, time.Since(start))
			if n := len(sheet["items"].([]any)); n == 0 {
				t.Fatalf("%s: got no rows", name)
			}
		}
		slices.Sort(times)
		p95 := times[len(times)*95/100]
		t.Logf("history sheet %s over 100,000 assets: median %v, p95 %v", name, times[len(times)/2], p95)
		if budget := nfr.Budget(time.Second); p95 > budget {
			t.Errorf("history sheet %s: p95 %v exceeds the %v budget (1 s scaled by ERP_BUDGET_SCALE)", name, p95, budget)
		}
	}
	sheet, _ := e.Must(200, "GET", path, "", "")
	want(t, "the closing cost of 100,000 assets", fmt.Sprint(sheet["totals"].(map[string]any)["closing_cost"].(map[string]any)["amount"]), "1080000000.00")
}
