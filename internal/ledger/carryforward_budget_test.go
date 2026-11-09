package ledger_test

import (
	"fmt"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/nfr"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// budgetCompany gives company 2000 the ADR's volume (ADR-0060 §13): 5,000
// accounts, half balance sheet and half expense, with projected balances
// in three currencies in every period, 1-16, of the years from to to
// (240,000 rows a year) in the leading ledger, every period of those
// years closed. Years before 2024 are set up first, as a tenant that
// loaded its history would have them.
func budgetCompany(t *testing.T, from, to int) cfEnv {
	t.Helper()
	s := setupCarryForward(t)
	if from < 2024 {
		doc := carryForwardDoc(t)
		for y := from; y < 2024; y++ {
			doc.NumberRanges = append(doc.NumberRanges, config.NumberRange{Company: "2000", DocumentType: "journal_sa", FiscalYear: y, First: 100000, Last: 999999})
		}
		s.apply(t, doc)
	}
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
		FROM ledger.gl_account a, generate_series($5::int, $6::int) y, generate_series(1, 16) m, unnest(ARRAY['EUR', 'USD', 'GBP']) cur
		WHERE a.chart_id = $4 AND a.number LIKE 'B%' ORDER BY y, m`, s.ten.ID, s.de, s.leading, chart, from, to)
	pgtest.Exec(t, `ANALYZE ledger.period_balance, ledger.gl_account, ledger.period_status, ledger.carry_forward_balance`)
	s.setPeriods(t, s.de, from, to, 16, "closed")
	return s
}

// carryForwardBody is the request carrying 2000's year forward in its
// leading ledger.
func (s cfEnv) carryForwardBody(year int) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "fiscal_year": %d, "ledger_ids": [%q]}`, s.de, year, s.leading)
}

// wantRows fails unless a budget run has the basis and 7,506 rows: 2,500
// balance sheet accounts, retained earnings and a result, each in 3
// currencies.
func wantRows(t *testing.T, run map[string]any, basis string) {
	t.Helper()
	if r := run["items"].([]any)[0].(map[string]any); r["basis"] != basis || r["row_count"] != 7506.0 {
		t.Fatalf("the run: got %v with %v rows, want %s with 7,506", r["basis"], r["row_count"], basis)
	}
}

// p95 sorts the times and fails when their 95th percentile exceeds the
// budget, scaled by ERP_BUDGET_SCALE.
func p95(t *testing.T, what string, times []time.Duration, budget time.Duration) {
	t.Helper()
	slices.Sort(times)
	p := times[len(times)*95/100]
	t.Logf("%s: median %v, p95 %v", what, times[len(times)/2], p)
	if b := nfr.Budget(budget); p > b {
		t.Fatalf("p95 %v exceeds the %v budget (%v scaled by ERP_BUDGET_SCALE)", p, b, budget)
	}
}

// TestCarryForwardBudget holds a chained carry-forward of one company and
// ledger to its budget (ADR-0060 §13, 01 §1.3: p95 3 s) over 2025 and
// 2026 (480,000 balance rows). 2025 is carried forward once, from every
// year; each measured run carries 2026 forward chained from it,
// superseding the run before. A page of the last run's balances is then
// held to its budget (p95 150 ms): the first page and one deep in the
// run, 500 rows each, with the whole run's totals.
func TestCarryForwardBudget(t *testing.T) {
	nfr.Measure(t)
	s := budgetCompany(t, 2025, 2026)
	s.Must(201, "POST", "/v1/balance-carry-forwards", s.carryForwardBody(2025), "")
	pgtest.Exec(t, `ANALYZE ledger.carry_forward_balance`)
	var times []time.Duration
	var run map[string]any
	for range 10 {
		start := time.Now()
		run, _ = s.Must(201, "POST", "/v1/balance-carry-forwards", s.carryForwardBody(2026), "")
		times = append(times, time.Since(start))
		wantRows(t, run, "chained")
	}
	p95(t, "chained carry-forward of 5,000 accounts in 3 currencies", times, 3*time.Second)

	path := fmt.Sprintf("/v1/balance-carry-forwards/%v/balances?page_size=500", run["items"].([]any)[0].(map[string]any)["id"])
	deep := path
	for range 7 {
		deep = path + "&page_token=" + url.QueryEscape(s.token(t, deep))
	}
	pgtest.Exec(t, `ANALYZE ledger.carry_forward_balance`)
	s.Must(200, "GET", path, "", "") // warm, as a reader paging through the run is
	times = times[:0]
	for i := range 20 {
		at := map[bool]string{true: path, false: deep}[i%2 == 0]
		start := time.Now()
		page, _ := s.Must(200, "GET", at, "", "")
		times = append(times, time.Since(start))
		if n := len(page["items"].([]any)); n != 500 {
			t.Fatalf("a page: got %d rows, want 500", n)
		}
	}
	p95(t, "a page of 500 of a run's 7,506 balances", times, 150*time.Millisecond)
}

// TestCarryForwardAllYearsBudget holds a carry-forward from every year to
// its budget (ADR-0060 §13, 01 §1.3: p95 10 s): ten years of balances,
// 2017 to 2026 (2.4 million rows), with no run of 2025, so each measured
// run of 2026 sums every year, superseding the run before.
func TestCarryForwardAllYearsBudget(t *testing.T) {
	nfr.Measure(t)
	s := budgetCompany(t, 2017, 2026)
	var times []time.Duration
	for range 5 {
		start := time.Now()
		run, _ := s.Must(201, "POST", "/v1/balance-carry-forwards", s.carryForwardBody(2026), "")
		times = append(times, time.Since(start))
		wantRows(t, run, "all_years")
	}
	p95(t, "carry-forward of 5,000 accounts in 3 currencies from ten years", times, 10*time.Second)
}
