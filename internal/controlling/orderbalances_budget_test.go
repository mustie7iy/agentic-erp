package controlling_test

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/nfr"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestInternalOrderBalancesBudget holds an order's actual costs of a
// year, every page, to the report budget of the center reports
// (01-requirements: p95 1 s; TestCenterBalancesBudget): an order with
// 20,001 lines on 51 accounts in twelve periods (301 rows, read in pages
// of 100) among the 200,001 entries (400,002 lines) of its company's year,
// the volume of TestJournalEntriesByAccountBudget. No requirement names an
// order's share of a company's lines; a tenth of a year's is far above an
// order of the first version, and the read is by the order's own lines
// (journal_line_by_internal_order, 000270), so it does not grow with the
// year's other lines.
func TestInternalOrderBalancesBudget(t *testing.T) {
	nfr.Measure(t)
	p := setup(t)
	order := p.releasedOrder(t, "FAIR-26")
	orderBudgetData(t, p, order)
	var times []time.Duration
	for range 10 {
		start := time.Now()
		rows, token, totals := 0, "", ""
		for {
			page, next := balances(t, p.Client, order, "&page_size=100"+token)
			more := page[len(page)-1] == "next"
			if more {
				page = page[:len(page)-1]
			}
			rows, totals = rows+len(page)-1, page[len(page)-1]
			if !more {
				break
			}
			token = "&page_token=" + next
		}
		times = append(times, time.Since(start))
		if rows != 301 || totals != "totals 200010.00 0.00 200010.00 EUR" {
			t.Fatalf("got %d rows and %s, want 301 rows and totals 200010.00 0.00 200010.00 EUR", rows, totals)
		}
	}
	slices.Sort(times)
	p95 := times[len(times)*95/100]
	t.Logf("an order's 301 rows of 20,001 lines among 400,002, in pages of 100: median %v, p95 %v", times[len(times)/2], p95)
	if budget := nfr.Budget(time.Second); p95 > budget {
		t.Fatalf("p95 %v exceeds the %v budget (1 s scaled by ERP_BUDGET_SCALE)", p95, budget)
	}
}

// orderBudgetData loads the budget's year: an entry posted to the order,
// copied 20,000 times on 50 further cost accounts over the twelve periods,
// and 180,000 copies of it that name no order.
func orderBudgetData(t *testing.T, p plan, order string) {
	t.Helper()
	entry, _ := p.charge(t, order, "2026-06-15", p.office, "10.00")
	var chart uuid.UUID
	pgtest.Scan(t, `SELECT id FROM ledger.chart_of_accounts WHERE tenant_id = $1 AND code = '2000'`, []any{p.Tenant.ID}, &chart)
	pgtest.Exec(t, `INSERT INTO ledger.gl_account (tenant_id, id, chart_id, number, name, account_type, cost_element, status, created_by, updated_by)
		SELECT $1, gen_random_uuid(), $2, 'O' || lpad(n::text, 2, '0'), 'Order cost ' || n, 'expense', 'primary_cost', 'active', $3, $3 FROM generate_series(1, 50) n`,
		p.Tenant.ID, chart, p.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO ledger.company_account (tenant_id, legal_entity_id, account_id, created_by, updated_by)
		SELECT $1, $2, id, $3, $3 FROM ledger.gl_account WHERE chart_id = $4 AND number LIKE 'O%'`, p.Tenant.ID, p.DE, p.Tenant.Human.ID, chart)
	// Each copy balances at commit: one statement per seed, its entries
	// and their lines together. A copy on the order moves its order line
	// to one of the 50 accounts; a copy off it keeps the account and
	// names no order.
	for _, s := range []struct {
		prefix  string
		n       int
		onOrder bool
	}{{"ORD", 20_000, true}, {"OFF", 180_000, false}} {
		pgtest.Exec(t, `WITH seed AS MATERIALIZED (SELECT g, gen_random_uuid() AS id, (g % 12 + 1)::smallint AS m FROM generate_series(1, $2::int) g),
			accounts AS MATERIALIZED (SELECT id, row_number() OVER (ORDER BY number) - 1 AS n FROM ledger.gl_account WHERE chart_id = $3 AND number LIKE 'O%'),
			entries AS (INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type, document_number,
				document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, reference, source_context, posted_by)
			 SELECT e.tenant_id, e.fiscal_year, s.id, e.legal_entity_id, e.ledger_id, s.m, e.document_type, $4 || lpad(s.g::text, 7, '0'),
				make_date(2026, s.m, 15), make_date(2026, s.m, 15), e.currency_tc, e.currency_lc, e.currency_gc, e.rate_tc_lc, e.rate_lc_gc, $4 || '-' || s.g,
				e.source_context, e.posted_by
			 FROM ledger.journal_entry e, seed s WHERE e.id = $1)
			INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period, posting_date,
				account_id, amount_tc, amount_lc, amount_gc, profit_center_id, segment_id, internal_order_id)
			SELECT l.tenant_id, l.fiscal_year, s.id, l.line_no, l.legal_entity_id, l.ledger_id, s.m, make_date(2026, s.m, 15),
				CASE WHEN $5 AND l.internal_order_id IS NOT NULL THEN (SELECT a.id FROM accounts a WHERE a.n = s.g % 50) ELSE l.account_id END,
				l.amount_tc, l.amount_lc, l.amount_gc, l.profit_center_id, l.segment_id, CASE WHEN $5 THEN l.internal_order_id END
			FROM ledger.journal_line l, seed s WHERE l.journal_entry_id = $1`, entry["id"], s.n, chart, s.prefix, s.onOrder)
	}
	pgtest.Exec(t, `ANALYZE ledger.journal_entry, ledger.journal_line, ledger.gl_account`)
	t.Log("loaded 200,001 entries, 20,001 of them on the order")
}
