package ledger_test

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/nfr"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestCenterBalancesBudget holds both center reports of one company and
// period, every page, to the trial balance's budget (01-requirements:
// p95 1 s) at the trial balance budget's volume (TestTrialBalanceBudget):
// 5,000 accounts with three years of projected balances in two currencies,
// here by assignment (360,000 rows, each account on one of 100 cost
// centers and 100 profit centers: no requirement gives a number of
// centers), and 2,000 lines on cost center 1000 the projector has not
// reached yet. The balances are written period by period, as the projector
// writes a period's rows when its first lines are posted, so a period's
// rows lie together as they would.
func TestCenterBalancesBudget(t *testing.T) {
	nfr.Measure(t)
	r := centerBudgetData(t)
	for _, path := range []string{"/v1/cost-center-balances", "/v1/profit-center-balances"} {
		var times []time.Duration
		for range 10 {
			start := time.Now()
			rows, token := 0, ""
			for {
				q := "legal_entity_id=" + r.de.String() + "&fiscal_year=2026&fiscal_period_from=6&fiscal_period_to=6&page_size=500"
				if token != "" {
					q += "&page_token=" + token
				}
				page, _ := r.Must(200, "GET", path+"?"+q, "", "")
				rows += len(page["items"].([]any))
				next, ok := page["next_page_token"].(string)
				if !ok {
					break
				}
				token = next
			}
			times = append(times, time.Since(start))
			if rows != 10020 {
				t.Fatalf("%s rows: got %d, want 10,020", path, rows)
			}
		}
		slices.Sort(times)
		p95 := times[len(times)*95/100]
		t.Logf("%s of 10,020 rows: median %v, p95 %v", path, times[len(times)/2], p95)
		if budget := nfr.Budget(time.Second); p95 > budget {
			t.Fatalf("%s p95 %v exceeds the %v budget (1 s scaled by ERP_BUDGET_SCALE)", path, p95, budget)
		}
	}
}

// centerBudgetData loads the budget's centers, balances and postings.
func centerBudgetData(t *testing.T) assigned {
	t.Helper()
	r := setupAssigned(t)
	fixture.Project(t)
	var chart uuid.UUID
	pgtest.Scan(t, `SELECT id FROM ledger.chart_of_accounts WHERE tenant_id = $1 AND code = '2000'`, []any{r.ten.ID}, &chart)
	pgtest.Exec(t, `INSERT INTO ledger.gl_account (tenant_id, id, chart_id, number, name, account_type, cost_element, status, created_by, updated_by)
		SELECT $1, gen_random_uuid(), $2, 'P' || n, 'Account ' || n, 'expense', 'primary_cost', 'active', $3, $3 FROM generate_series(1, 5000) n`,
		r.ten.ID, chart, r.ten.Human.ID)
	pgtest.Exec(t, `INSERT INTO ledger.company_account (tenant_id, legal_entity_id, account_id, created_by, updated_by)
		SELECT $1, $2, id, $3, $3 FROM ledger.gl_account WHERE chart_id = $4 AND number LIKE 'P%'`, r.ten.ID, r.de, r.ten.Human.ID, chart)
	// Active centers sit in the standard hierarchies at commit (T56b): each
	// is created with its place in one statement.
	pgtest.Exec(t, `WITH c AS (
			INSERT INTO ledger.cost_center (tenant_id, id, legal_entity_id, code, name, responsible, status, created_by, updated_by)
			SELECT $1, gen_random_uuid(), $2, 'B' || n, 'CC ' || n, 'J', 'active', $3, $3 FROM generate_series(1, 100) n RETURNING id)
		INSERT INTO ledger.hierarchy_member (tenant_id, id, hierarchy_id, kind, node_id, cost_center_id, created_by, updated_by)
		SELECT $1, gen_random_uuid(), $4, 'cost_center', $5, id, $3, $3 FROM c`, r.ten.ID, r.de, r.ten.Human.ID, r.ccTree, r.ccLeaf)
	pgtest.Exec(t, `WITH p AS (
			INSERT INTO ledger.profit_center (tenant_id, id, code, name, responsible, status, created_by, updated_by)
			SELECT $1, gen_random_uuid(), 'Q' || n, 'PC ' || n, 'J', 'active', $3, $3 FROM generate_series(1, 100) n RETURNING id),
		a AS (INSERT INTO ledger.profit_center_company (tenant_id, profit_center_id, legal_entity_id, created_by) SELECT $1, id, $2, $3 FROM p)
		INSERT INTO ledger.hierarchy_member (tenant_id, id, hierarchy_id, kind, node_id, profit_center_id, created_by, updated_by)
		SELECT $1, gen_random_uuid(), $4, 'profit_center', $5, id, $3, $3 FROM p`, r.ten.ID, r.de, r.ten.Human.ID, r.pcTree, r.pcLeaf)
	pgtest.Exec(t, `INSERT INTO ledger.assignment_balance (tenant_id, ledger_id, legal_entity_id, account_id, cost_center_id, profit_center_id, fiscal_year,
			fiscal_period, currency_tc, debit_tc, credit_tc, debit_lc, credit_lc, debit_gc, credit_gc)
		SELECT $1, l.id, $2, a.id, c.id, p.id, y, m, cur, 1000, 10, 1000, 10, 1000, 10
		FROM ledger.gl_account a
		JOIN ledger.cost_center c ON c.tenant_id = $1 AND c.code = 'B' || (substr(a.number, 2)::int % 100 + 1)
		JOIN ledger.profit_center p ON p.tenant_id = $1 AND p.code = 'Q' || (substr(a.number, 2)::int % 100 + 1),
			ledger.ledger l, generate_series(2024, 2026) y, generate_series(1, 12) m, unnest(ARRAY['EUR', 'USD']) cur
		WHERE a.chart_id = $3 AND a.number LIKE 'P%' AND l.tenant_id = $1 AND l.code = '0L'
		ORDER BY y, m`, r.ten.ID, r.de, chart)
	rows, _ := pgtest.Admin(t).Query(t.Context(), `SELECT id::text FROM ledger.gl_account WHERE chart_id = $1 AND number LIKE 'P%' ORDER BY number LIMIT 20`, chart)
	accounts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(accounts) != 20 {
		t.Fatalf("accounts: got %d (%v), want 20", len(accounts), err)
	}
	lines := make([]centeredLine, len(accounts))
	for i, a := range accounts {
		lines[i] = centeredLine{account: a, amount: []string{"1.00", "-1.00"}[i%2], cc: r.cc1000.String()}
	}
	for range 100 {
		r.Must(201, "POST", "/v1/journal-entries", centeredEntry(r.de, "2026-06-15", lines...), "")
	}
	pgtest.Exec(t, `ANALYZE ledger.assignment_balance, ledger.journal_entry, ledger.journal_line, ledger.balance_projection, ledger.cost_center,
		ledger.profit_center, ledger.gl_account`)
	t.Logf("loaded 360,000 assignment balances and %d pending lines", 100*len(lines))
	return r
}
