package controlling_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/nfr"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestMarginReportBudget holds a company's margin report of a year, every
// page, to the week 3 budget of a report read (p95 3 s) at the volume the
// trial balance and the center reports are measured at
// (TestTrialBalanceBudget, TestCenterBalancesBudget): 5,000 accounts, half
// of them revenue and half cost of sales, with three years of projected
// balances in two currencies, by account (360,000 rows) and by assignment
// (360,000 rows, each account on one of 100 profit centers: no
// requirement gives a number of profit centers), and 100 entries the
// projector has not reached yet, whose revenue carries no profit center.
// The report is 100 profit centers and the unassigned row, read in pages
// of 50.
func TestMarginReportBudget(t *testing.T) {
	nfr.Measure(t)
	p := setup(t)
	marginBudgetData(t, p)
	var times []time.Duration
	for range 10 {
		start := time.Now()
		rows, token, totals := 0, "", ""
		for {
			page := p.report(t, p.DE, "&fiscal_period_to=12&page_size=50"+token)
			next := ""
			if last := page[len(page)-1]; strings.HasPrefix(last, "next:") {
				page, next = page[:len(page)-1], last[len("next:"):]
			}
			rows, totals = rows+len(page)-2, page[len(page)-1]
			if named := len(strings.Fields(page[0][strings.Index(page[0], "[")+1 : strings.Index(page[0], "]")])); named != 2500 {
				t.Fatalf("cost of sales accounts: got %d, want 2,500", named)
			}
			if next == "" {
				break
			}
			token = "&page_token=" + next
		}
		times = append(times, time.Since(start))
		// Per profit center 25 revenue accounts and 25 cost of sales
		// accounts in 12 periods and two currencies: 600 balances of
		// 9.90 and of 5.90; the queued entries add 100.00 of revenue.
		if want := "totals: revenue 594100.00 EUR cost 354000.00 EUR margin 240100.00 EUR"; rows != 101 || totals != want {
			t.Fatalf("got %d rows and %s, want 101 rows and %s", rows, totals, want)
		}
	}
	slices.Sort(times)
	p95 := times[len(times)*95/100]
	t.Logf("margin report of 100 profit centers over 720,000 balances and 100 queued entries, in pages of 50: median %v, p95 %v", times[len(times)/2], p95)
	if budget := nfr.Budget(3 * time.Second); p95 > budget {
		t.Fatalf("p95 %v exceeds the %v budget (3 s scaled by ERP_BUDGET_SCALE)", p95, budget)
	}
}

// marginBudgetData loads the budget's accounts (R1 to R2500 revenue, E1 to
// E2500 expense, which the configuration names as cost of sales), profit
// centers, balances and queued entries.
func marginBudgetData(t *testing.T, p plan) {
	t.Helper()
	fixture.Project(t)
	var chart uuid.UUID
	pgtest.Scan(t, `SELECT id FROM ledger.chart_of_accounts WHERE tenant_id = $1 AND code = '2000'`, []any{p.Tenant.ID}, &chart)
	pgtest.Exec(t, `INSERT INTO ledger.gl_account (tenant_id, id, chart_id, number, name, account_type, cost_element, status, created_by, updated_by)
		SELECT $1, gen_random_uuid(), $2, k.prefix || n, 'Account ' || n, k.type, k.element, 'active', $3, $3
		FROM generate_series(1, 2500) n, (VALUES ('R', 'revenue', 'revenue'), ('E', 'expense', 'primary_cost')) k(prefix, type, element)`,
		p.Tenant.ID, chart, p.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO ledger.company_account (tenant_id, legal_entity_id, account_id, created_by, updated_by)
		SELECT $1, $2, id, $3, $3 FROM ledger.gl_account WHERE chart_id = $4 AND number ~ '^[RE][0-9]+$'`, p.Tenant.ID, p.DE, p.Tenant.Human.ID, chart)
	// The configuration's chart has the expense accounts too, as an
	// activated document would, and names them all as cost of sales.
	var company int
	pgtest.Scan(t, `SELECT c.n - 1 FROM config.config_version v JOIN config.active_version a ON a.tenant_id = v.tenant_id AND a.number = v.number,
		jsonb_array_elements(v.document -> 'companies') WITH ORDINALITY c(co, n) WHERE v.tenant_id = $1 AND c.co ->> 'code' = '2000'`,
		[]any{p.Tenant.ID}, &company)
	chartPath := fmt.Sprintf("{companies,%d,chart_of_accounts}", company)
	p.reconfigure(t, `jsonb_set(v.document, $2::text[], v.document #> $2::text[]
		|| (SELECT jsonb_agg(jsonb_build_object('number', 'E' || n, 'name', 'Account ' || n, 'type', 'expense')) FROM generate_series(1, 2500) n))`, chartPath)
	p.nameCostOfSales(t, `[{"chart": "2000", "cost_of_sales_accounts": [{"from": "E0", "to": "EZ"}]}]`)
	// Active centers sit in the standard hierarchy at commit (T56b): each is
	// created with its place, beside PC1, in one statement.
	pgtest.Exec(t, `WITH pc AS (
			INSERT INTO ledger.profit_center (tenant_id, id, code, name, responsible, status, created_by, updated_by)
			SELECT $1, gen_random_uuid(), 'Q' || n, 'PC ' || n, 'J', 'active', $3, $3 FROM generate_series(1, 100) n RETURNING id),
		co AS (INSERT INTO ledger.profit_center_company (tenant_id, profit_center_id, legal_entity_id, created_by) SELECT $1, id, $2, $3 FROM pc)
		INSERT INTO ledger.hierarchy_member (tenant_id, id, hierarchy_id, kind, node_id, profit_center_id, created_by, updated_by)
		SELECT $1, gen_random_uuid(), m.hierarchy_id, 'profit_center', m.node_id, pc.id, $3, $3
		FROM pc, ledger.hierarchy_member m WHERE m.tenant_id = $1 AND m.profit_center_id = $4`, p.Tenant.ID, p.DE, p.Tenant.Human.ID, p.c.Profit)
	// A revenue account's balance is 10.00 credit and 0.10 debit, a cost
	// account's 6.00 debit and 0.10 credit, written period by period, as
	// the projector writes a period's rows when its first lines are posted.
	const balances = `SELECT $1, l.id, $2, a.id, %s y, m, cur, d.debit, d.credit, d.debit, d.credit, d.debit, d.credit
		FROM ledger.gl_account a
		JOIN ledger.profit_center pc ON pc.tenant_id = $1 AND pc.code = 'Q' || (substr(a.number, 2)::int % 100 + 1)
		CROSS JOIN LATERAL (SELECT CASE WHEN a.account_type = 'revenue' THEN 10 ELSE 600 END AS debit,
			CASE WHEN a.account_type = 'revenue' THEN 1000 ELSE 10 END AS credit) d,
			ledger.ledger l, generate_series(2024, 2026) y, generate_series(1, 12) m, unnest(ARRAY['EUR', 'USD']) cur
		WHERE a.chart_id = $3 AND a.number ~ '^[RE][0-9]+$' AND l.tenant_id = $1 AND l.is_leading ORDER BY y, m`
	pgtest.Exec(t, `INSERT INTO ledger.period_balance (tenant_id, ledger_id, legal_entity_id, account_id, fiscal_year, fiscal_period, currency_tc,
		debit_tc, credit_tc, debit_lc, credit_lc, debit_gc, credit_gc) `+strings.Replace(balances, "%s", "", 1), p.Tenant.ID, p.DE, chart)
	pgtest.Exec(t, `INSERT INTO ledger.assignment_balance (tenant_id, ledger_id, legal_entity_id, account_id, profit_center_id, fiscal_year, fiscal_period,
		currency_tc, debit_tc, credit_tc, debit_lc, credit_lc, debit_gc, credit_gc) `+strings.Replace(balances, "%s", "pc.id,", 1), p.Tenant.ID, p.DE, chart)
	for range 100 {
		p.post(t, "2026-06-15", on(p.revenue, -100, &p.c.Sales), on(p.bank, 100, nil))
	}
	pgtest.Exec(t, `ANALYZE ledger.period_balance, ledger.assignment_balance, ledger.journal_entry, ledger.journal_line, ledger.balance_projection,
		ledger.profit_center, ledger.gl_account, ledger.company_account`)
	t.Log("loaded 360,000 period balances, 360,000 assignment balances and 100 queued entries")
}
