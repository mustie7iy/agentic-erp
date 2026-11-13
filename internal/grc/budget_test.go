package grc_test

import (
	"slices"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/nfr"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestMatrixBudget holds a read of the control matrix to the budget of
// a report read (p95 3 s; plan-breadth-week3, what every slice reuses):
// a library of 2,000 active controls in 20 processes, each with three
// links (a permission, a segregation-of-duties rule, a configuration
// path) and ten tests, so 6,000 links and 20,000 tests. No requirement
// names a library's size (01 §1.3 has no figure for controls); controls
// are recorded one at a time through the API in the first version, so
// 2,000 is far above a library of it. Every read judges all the links
// the filters select before it pages, so the first page costs what the
// last does; a read without filters is the worst case and is measured
// with the filters by state and by process.
func TestMatrixBudget(t *testing.T) {
	nfr.Measure(t)
	e := setup(t)
	inForce := e.rule(t, "credit.limit.read", "credit.limit.write", -1, nil)
	ended := e.rule(t, "sales.price.read", "sales.price.write", -9, days(0))
	pgtest.Exec(t, `INSERT INTO grc.control (tenant_id, id, code, objective, process, owner_id, frequency, nature, framework_references, created_by, updated_by)
		SELECT $1, gen_random_uuid(), 'C-' || lpad(n::text, 5, '0'), 'Objective ' || n, 'Process ' || (n % 20), $2, 'monthly', 'manual',
			ARRAY['REF ' || (n % 50)], $2, $2
		FROM generate_series(1, 2000) n`, e.Tenant.ID, e.Tenant.Human.ID)
	// One control in ten is linked to the rule that ended: 200 broken links.
	pgtest.Exec(t, `INSERT INTO grc.control_link (tenant_id, id, control_id, kind, target, created_by, updated_by)
		SELECT $1, gen_random_uuid(), c.id, k.kind, CASE k.kind
				WHEN 'permission' THEN (SELECT code FROM ref.permission ORDER BY code OFFSET (c.n % 100) LIMIT 1)
				WHEN 'sod_rule' THEN CASE WHEN c.n % 10 = 0 THEN $3::text ELSE $4::text END
				ELSE CASE c.n % 3 WHEN 0 THEN 'companies[2000].currency' WHEN 1 THEN 'companies[1000]' ELSE 'payments' END END, $2, $2
		FROM (SELECT id, row_number() OVER (ORDER BY code) AS n FROM grc.control WHERE tenant_id = $1) c
		CROSS JOIN (VALUES ('permission'), ('sod_rule'), ('configuration_path')) k(kind)`, e.Tenant.ID, e.Tenant.Human.ID, ended, inForce)
	pgtest.Exec(t, `INSERT INTO grc.control_test (tenant_id, id, control_id, tested_on, result, note, tested_by)
		SELECT $1, gen_random_uuid(), c.id, current_date - d, CASE WHEN d % 4 = 0 THEN 'failed' ELSE 'passed' END, 'Seen.', $2
		FROM grc.control c CROSS JOIN generate_series(1, 10) d WHERE c.tenant_id = $1`, e.Tenant.ID, e.Tenant.Human.ID)
	pgtest.Exec(t, `ANALYZE grc.control`)
	pgtest.Exec(t, `ANALYZE grc.control_link`)
	pgtest.Exec(t, `ANALYZE grc.control_test`)
	for _, c := range []struct {
		name, query string
		rows        int
		totals      string
	}{
		{"the first page of the whole matrix", "", 50, "2000 controls 1800/200/0, 6000 links 5800/200"},
		{"a page of 500", "?page_size=500", 500, "2000 controls 1800/200/0, 6000 links 5800/200"},
		{"the controls with a broken link", "?link_state=broken&page_size=500", 200, "200 controls 0/200/0, 600 links 400/200"},
		{"one process", "?process=Process%207&page_size=500", 100, "100 controls 100/0/0, 300 links 300/0"},
		{"one framework reference", "?framework_reference=REF%2010&page_size=500", 40, "40 controls 0/40/0, 120 links 80/40"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := e.in(t)
			var times []time.Duration
			for range 20 {
				start := time.Now()
				m := e.matrix(t, c.query)
				times = append(times, time.Since(start))
				if n := len(apitest.List(t, m, "items")); n != c.rows {
					t.Fatalf("got %d rows, want %d", n, c.rows)
				}
				if got := totalsOf(t, m); got != c.totals {
					t.Fatalf("totals: got %q, want %q", got, c.totals)
				}
			}
			slices.Sort(times)
			p95 := times[len(times)*95/100]
			t.Logf("the matrix of 2,000 controls with 6,000 links and 20,000 tests: median %v, p95 %v", times[len(times)/2], p95)
			if budget := nfr.Budget(3 * time.Second); p95 > budget {
				t.Fatalf("p95 %v exceeds the %v budget (3 s scaled by ERP_BUDGET_SCALE)", p95, budget)
			}
		})
	}
}
