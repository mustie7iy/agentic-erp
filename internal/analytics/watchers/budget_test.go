package watchers_test

import (
	"slices"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/nfr"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestAlertListBudget holds one page of the alerts to 01-requirements'
// budget for a list with filters (p95 150 ms): 12,000 alerts of 200
// watchers, five years of months each, in two companies, half of them
// acknowledged. No requirement names how many alerts a tenant keeps; a
// watcher raises at most one a month, so 12,000 is five years of 200
// watchers. The list reads the alerts the latest first (metric_alert_latest)
// and keeps those the caller may see, so a reader of one company reads
// past the other's to fill a page: measured too.
func TestAlertListBudget(t *testing.T) {
	nfr.Measure(t)
	e := setup(t, alertPerms...)
	pgtest.Exec(t, `INSERT INTO analytics.metric_watcher (tenant_id, id, name, metric, grain, condition, threshold, currency, created_by, updated_by)
		SELECT $1, gen_random_uuid(), 'Watcher ' || n, 'revenue', 'month', 'above', '1', 'EUR', $2, $2 FROM generate_series(1, 200) n`,
		e.Tenant.ID, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO analytics.metric_alert (tenant_id, id, watcher_id, fiscal_year, fiscal_period, companies, permissions, findings, status,
			acknowledged_at, acknowledged_by, created_at, created_by)
		SELECT $1, gen_random_uuid(), w.id, 2022 + m / 12, m % 12 + 1, ARRAY[CASE WHEN w.n % 2 = 0 THEN '1000' ELSE '2000' END],
			'{ledger.account.read,ledger.entry.read,partner.supplier.read,tenancy.org.read}',
			'[{"company": "2000", "currency": "EUR", "figure": "500.00", "compared_with": "1"}]',
			CASE WHEN m % 2 = 0 THEN 'open' ELSE 'acknowledged' END, CASE WHEN m % 2 = 0 THEN NULL ELSE now() END,
			CASE WHEN m % 2 = 0 THEN NULL ELSE $2::uuid END, now() - (m * interval '1 day') - (w.n * interval '1 second'), $2
		FROM (SELECT id, row_number() OVER (ORDER BY id) AS n FROM analytics.metric_watcher WHERE tenant_id = $1) w
		CROSS JOIN generate_series(0, 59) m`, e.Tenant.ID, e.Tenant.Human.ID)
	pgtest.Exec(t, `ANALYZE analytics.metric_alert`)
	pgtest.Exec(t, `ANALYZE analytics.metric_watcher`)
	var watcher string
	pgtest.Scan(t, `SELECT id::text FROM analytics.metric_watcher WHERE tenant_id = $1 ORDER BY id LIMIT 1`, []any{e.Tenant.ID}, &watcher)
	reader := e.As(e.Reader(t, "Rhea Reader", e.US))
	for _, c := range []struct {
		name   string
		client apitest.Client
		query  string
		want   int
	}{
		{"the first page", e.Client, "", 50},
		{"a page of 500", e.Client, "?page_size=500", 500},
		{"the open alerts", e.Client, "?status=open&page_size=100", 100},
		{"one watcher's", e.Client, "?watcher_id=" + watcher, 50},
		{"a reader of one company", reader, "?page_size=100", 100},
	} {
		t.Run(c.name, func(t *testing.T) {
			client := c.client.In(t)
			var times []time.Duration
			for range 20 {
				start := time.Now()
				page, _ := client.Must(200, "GET", "/v1/metric-alerts"+c.query, "", "")
				times = append(times, time.Since(start))
				if n := len(apitest.List(t, page, "items")); n != c.want {
					t.Fatalf("got %d alerts, want %d", n, c.want)
				}
			}
			slices.Sort(times)
			p95 := times[len(times)*95/100]
			t.Logf("one page of 12,000 alerts: median %v, p95 %v", times[len(times)/2], p95)
			if budget := nfr.Budget(150 * time.Millisecond); p95 > budget {
				t.Fatalf("p95 %v exceeds the %v budget (150 ms scaled by ERP_BUDGET_SCALE)", p95, budget)
			}
		})
	}
}
