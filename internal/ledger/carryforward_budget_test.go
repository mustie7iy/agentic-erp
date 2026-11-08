package ledger_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/nfr"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestCarryForwardBudget holds a chained carry-forward of one company and
// ledger to its budget (ADR-0059 §13, 01 §1.3: p95 3 s) at the ADR's
// volume: 5,000 accounts, half balance sheet and half expense, with
// projected balances in three currencies in every period, 1-16, of 2025
// and 2026 (480,000 rows). 2025 is carried forward once, from every year;
// each measured run carries 2026 forward chained from it, superseding the
// run before.
func TestCarryForwardBudget(t *testing.T) {
	nfr.Measure(t)
	s := setupCarryForward(t)
	var chart uuid.UUID
	pgtest.Scan(t, `SELECT id FROM ledger.chart_of_accounts WHERE tenant_id = $1 AND code = '2000'`, []any{s.ten.ID}, &chart)
	pgtest.Exec(t, `INSERT INTO ledger.gl_account (tenant_id, id, chart_id, number, name, account_type, status, created_by, updated_by)
		SELECT $1, gen_random_uuid(), $2, 'B' || n, 'Account ' || n, CASE WHEN n % 2 = 0 THEN 'asset' ELSE 'expense' END, 'active', $3, $3
		FROM generate_series(1, 5000) n`, s.ten.ID, chart, s.ten.Human.ID)
	pgtest.Exec(t, `INSERT INTO ledger.company_account (tenant_id, legal_entity_id, account_id, created_by, updated_by)
		SELECT $1, $2, id, $3, $3 FROM ledger.gl_account WHERE chart_id = $4 AND number LIKE 'B%'`, s.ten.ID, s.de, s.ten.Human.ID, chart)
	pgtest.Exec(t, `INSERT INTO ledger.period_balance (tenant_id, ledger_id, legal_entity_id, account_id, fiscal_year, fiscal_period, currency_tc,
			debit_tc, credit_tc, debit_lc, credit_lc, debit_gc, credit_gc)
		SELECT $1, $3, $2, a.id, y, m, cur, 1000, 10, 900, 9, 900, 9
		FROM ledger.gl_account a, generate_series(2025, 2026) y, generate_series(1, 16) m, unnest(ARRAY['EUR', 'USD', 'GBP']) cur
		WHERE a.chart_id = $4 AND a.number LIKE 'B%' ORDER BY y, m`, s.ten.ID, s.de, s.leading, chart)
	pgtest.Exec(t, `ANALYZE ledger.period_balance, ledger.gl_account, ledger.period_status, ledger.carry_forward_balance`)
	s.setPeriods(t, s.de, 2025, 2026, 16, "closed")
	body := func(year int) string {
		return fmt.Sprintf(`{"legal_entity_id": %q, "fiscal_year": %d, "ledger_ids": [%q]}`, s.de, year, s.leading)
	}
	s.Must(201, "POST", "/v1/balance-carry-forwards", body(2025), "")
	pgtest.Exec(t, `ANALYZE ledger.carry_forward_balance`)
	var times []time.Duration
	for range 10 {
		start := time.Now()
		run, _ := s.Must(201, "POST", "/v1/balance-carry-forwards", body(2026), "")
		times = append(times, time.Since(start))
		if r := run["items"].([]any)[0].(map[string]any); r["basis"] != "chained" || r["row_count"] != 7506.0 {
			t.Fatalf("the run: got %v with %v rows, want chained with 7,506 (2,500 balance sheet accounts, retained earnings and a result, each in 3 currencies)",
				r["basis"], r["row_count"])
		}
	}
	slices.Sort(times)
	p95 := times[len(times)*95/100]
	t.Logf("chained carry-forward of 5,000 accounts in 3 currencies: median %v, p95 %v", times[len(times)/2], p95)
	if budget := nfr.Budget(3 * time.Second); p95 > budget {
		t.Fatalf("p95 %v exceeds the %v budget (3 s scaled by ERP_BUDGET_SCALE)", p95, budget)
	}
}
