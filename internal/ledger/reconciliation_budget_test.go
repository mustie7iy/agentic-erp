package ledger_test

import (
	"context"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/nfr"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestLedgerReconciliationBudget holds the ledger reconciliation to the
// trial balance's budget (01-requirements: p95 1 s) at the scale of the
// largest tenant's company keeping two ledgers: 5,000 accounts with three
// years of projected balances in two currencies in each ledger, every
// tenth account differing in 2026, and 100 entries of 20 lines posted to
// every ledger of the company and 100 to the parallel ledger alone, which
// the projector has not reached.
// It reads every page of the year's differences, with the trial balances,
// and the drill-down of one account (T52e).
func TestLedgerReconciliationBudget(t *testing.T) {
	if testing.Short() || raceDetector {
		t.Skip("performance budget: runs without -short and without the race detector (make perf, CI)")
	}
	r := reconBudgetData(t)
	var times []time.Duration
	for range 10 {
		start := time.Now()
		rows, token := 0, ""
		for {
			q := url.Values{"page_size": {"500"}}
			if token != "" {
				q.Set("page_token", token)
			}
			page, _ := r.Must(200, "GET", "/v1/ledger-reconciliation?"+r.query(q), "", "")
			rows += len(page["items"].([]any))
			next, ok := page["next_page_token"].(string)
			if !ok {
				break
			}
			token = next
		}
		lines, _ := r.Must(200, "GET", "/v1/ledger-reconciliation/entries?"+r.query(url.Values{"account_id": {r.bank}, "page_size": {"500"}}), "", "")
		times = append(times, time.Since(start))
		// 500 accounts in two currencies, and the 20 accounts posted to in
		// euro but two of them (R10, R20) among those.
		if rows != 1018 || len(lines["items"].([]any)) != 100 {
			t.Fatalf("differences: got %d, want 1018; drill-down lines: got %d, want 100", rows, len(lines["items"].([]any)))
		}
	}
	slices.Sort(times)
	p95 := times[len(times)*95/100]
	t.Logf("ledger reconciliation of 2 x 360,000 balances (1,018 differences) and a 100-line drill-down: median %v, p95 %v", times[len(times)/2], p95)
	if budget := nfr.Budget(time.Second); p95 > budget {
		t.Fatalf("ledger reconciliation p95 %v exceeds the %v budget (1 s scaled by ERP_BUDGET_SCALE)", p95, budget)
	}
}

// reconBudgetData loads the budget's balances and postings.
func reconBudgetData(t *testing.T) reconEnv {
	t.Helper()
	r := reconEnv{parallelEnv: setupParallel(t)}
	r.T = t
	fixture.Project(t)
	var chart uuid.UUID
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT id FROM ledger.chart_of_accounts WHERE tenant_id = $1 AND code = '2000'`, r.ten.ID).Scan(&chart); err != nil {
		t.Fatalf("read chart 2000: %v", err)
	}
	pgtest.Exec(t, `INSERT INTO ledger.gl_account (tenant_id, id, chart_id, number, name, account_type, status, created_by, updated_by)
		SELECT $1, gen_random_uuid(), $2, 'R' || n, 'Account ' || n, 'expense', 'active', $3, $3 FROM generate_series(1, 5000) n`, r.ten.ID, chart, r.ten.Human.ID)
	pgtest.Exec(t, `INSERT INTO ledger.company_account (tenant_id, legal_entity_id, account_id, created_by, updated_by)
		SELECT $1, $2, id, $3, $3 FROM ledger.gl_account WHERE chart_id = $4 AND number LIKE 'R%'`, r.ten.ID, r.de, r.ten.Human.ID, chart)
	pgtest.Exec(t, `INSERT INTO ledger.period_balance (tenant_id, ledger_id, legal_entity_id, account_id, fiscal_year, fiscal_period, currency_tc,
			debit_tc, credit_tc, debit_lc, credit_lc, debit_gc, credit_gc)
		SELECT $1, l.id, $2, a.id, y, p, c, 1000 + CASE WHEN l.code = '2L' AND y = 2026 AND substr(a.number, 2)::int % 10 = 0 THEN 1 ELSE 0 END, 10, 1000, 10, 1000, 10
		FROM ledger.gl_account a, ledger.ledger l, generate_series(2024, 2026) y, generate_series(1, 12) p, unnest(ARRAY['EUR', 'USD']) c
		WHERE a.chart_id = $3 AND a.number LIKE 'R%' AND l.tenant_id = $1`, r.ten.ID, r.de, chart)
	rows, _ := pgtest.Admin(t).Query(context.Background(), `SELECT id::text FROM ledger.gl_account WHERE chart_id = $1 AND number LIKE 'R%' ORDER BY substr(number, 2)::int LIMIT 20`, chart)
	accounts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(accounts) != 20 {
		t.Fatalf("accounts: got %d (%v), want 20", len(accounts), err)
	}
	r.bank = accounts[0]
	lines := make([]line, len(accounts))
	for i, a := range accounts {
		lines[i] = line{a, []string{"1.00", "-1.00"}[i%2], ""}
	}
	for range 100 {
		r.Must(201, "POST", "/v1/journal-entries", entry(r.de, "2026-06-15", "EUR", lines...), "")
		r.Must(201, "POST", "/v1/journal-entries", with(entry(r.de, "2026-06-16", "EUR", lines...), `"ledger_id": "`+r.local+`"`), "")
	}
	pgtest.Exec(t, `ANALYZE ledger.period_balance, ledger.journal_entry, ledger.journal_line, ledger.balance_projection`)
	return r
}
