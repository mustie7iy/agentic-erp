package leases_test

import (
	"slices"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/nfr"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestScheduleBudget holds a read of a lease contract's schedule to the
// budget of a report read (p95 3 s; plan-breadth-week3, what every slice
// reuses) at the longest term a contract may have, 600 monthly payments:
// a draft's, computed on read in exact rational arithmetic, and an active
// contract's, read from its 600 stored lines. No requirement names a
// count of lease contracts (01 §1.3 has no figure for them); the read is
// of one contract by id, and 2,000 others of 36 payments each, 500 of
// them active with their lines, stand beside it in the tables.
func TestScheduleBudget(t *testing.T) {
	nfr.Measure(t)
	e := setup(t)
	longest := lease{commencement: "2026-01-01", term: 600, amount: "12345.67", frequency: "monthly", timing: "advance", rate: "4.123456"}
	draft := e.create(t, longest)["id"].(string)
	active := e.create(t, longest)["id"].(string)
	e.Must(200, "POST", contracts+"/"+active+":activate", "", `"1"`)
	pgtest.Exec(t, `INSERT INTO leases.contract (tenant_id, id, document_number, legal_entity_id, lessor_id, commencement_date, term_months, payment_amount,
			currency, payment_frequency, payment_timing, annual_rate, cost_center_id, status, created_by, updated_by)
		SELECT $1, gen_random_uuid(), 'B' || lpad(n::text, 6, '0'), $2, $3, '2026-01-01', 36, 100000, 'EUR', 'monthly', 'arrears', 5, $4, 'draft', $5, $5
		FROM generate_series(1, 2000) n`, e.Tenant.ID, e.DE, e.Supplier, e.center, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO leases.payment (tenant_id, contract_id, payment_no, due_date, amount)
		SELECT k.tenant_id, k.id, p, ('2026-01-01'::date + (p || ' months')::interval - interval '1 day')::date, 100000
		FROM leases.contract k CROSS JOIN generate_series(1, 36) p WHERE k.tenant_id = $1 AND k.document_number LIKE 'B%'`, e.Tenant.ID)
	pgtest.Exec(t, `INSERT INTO leases.schedule_line (tenant_id, contract_id, line_no, period_start, period_end, opening_liability, payment, interest,
			closing_liability, depreciation, right_of_use_closing)
		SELECT k.tenant_id, k.id, p, '2026-01-01', '2026-01-31', 100000, 100000, 0, 0, 1000, 0
		FROM leases.contract k CROSS JOIN generate_series(1, 36) p WHERE k.tenant_id = $1 AND k.document_number BETWEEN 'B000001' AND 'B000500'`, e.Tenant.ID)
	for _, table := range []string{"leases.contract", "leases.payment", "leases.schedule_line"} {
		pgtest.Exec(t, `ANALYZE `+table)
	}
	for _, c := range []struct{ name, id, status string }{{"a draft's, computed", draft, "draft"}, {"an active contract's, stored", active, "active"}} {
		t.Run(c.name, func(t *testing.T) {
			e := e.In(t)
			var times []time.Duration
			for range 20 {
				start := time.Now()
				got, _ := e.Must(200, "GET", contracts+"/"+c.id+"/schedule", "", "")
				times = append(times, time.Since(start))
				if n := len(rows(t, got, "lines")); n != 600 || got["status"] != c.status {
					t.Fatalf("got %d lines of a %v contract, want 600 of a %s one", n, got["status"], c.status)
				}
			}
			slices.Sort(times)
			p95 := times[len(times)*95/100]
			t.Logf("the schedule of 600 monthly payments, %s: median %v, p95 %v", c.status, times[len(times)/2], p95)
			if budget := nfr.Budget(3 * time.Second); p95 > budget {
				t.Fatalf("p95 %v exceeds the %v budget (3 s scaled by ERP_BUDGET_SCALE)", p95, budget)
			}
		})
	}
}
