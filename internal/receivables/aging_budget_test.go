package receivables_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/nfr"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestReceivablesAgingBudget measures GET /v1/receivables-aging (T75e,
// ADR-0063 §10) against 01-requirements §1.3: the aging of one company
// over 100,000 open customer items, p95 3 s, the budget of the other
// report computed on read over a company's open items (the liquidity
// forecast). The volume is the one 01 names for a company's supplier open
// items, taken for its customers' as well: 2,000 customers with 50 items
// each on the receivables account, half with payment terms, their due
// dates spread over 300 days. Every call reads every item, whatever the
// page, since the totals are those of all customers: the first page and
// the one after it are measured. The pilot's own
// volumes are not known yet (ADR-0052, facts).
func TestReceivablesAgingBudget(t *testing.T) {
	nfr.Measure(t)
	e := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute) // seeding 100,000 entries, on a loaded machine too
	defer cancel()
	admin := pgtest.Admin(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	// The items are written straight into the journal, balanced, without
	// the row checks a posting runs (as TestOpenItemsAgingBudget seeds
	// them): the customers are ids alone, so the page's rows name none.
	exec(`SET session_replication_role = replica`)
	exec(`WITH s AS (
			SELECT n, gen_random_uuid() AS entry, md5('c' || (n % 2000))::uuid AS partner FROM generate_series(1, 100000) n
		), j AS (
			INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type, document_number,
				document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, reference, source_context, posted_by)
			SELECT $1, 2026, entry, $2, l.id, 3, 'DR', 'B' || n, '2026-03-01', '2026-03-01', 'EUR', 'EUR', 'EUR', 1, 1, 'CI-' || n, 'receivables', $3
			FROM s, ledger.ledger l WHERE l.tenant_id = $1 AND l.is_leading RETURNING id, ledger_id
		) INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period, posting_date,
			account_id, amount_tc, amount_lc, amount_gc, partner_id)
		SELECT $1, 2026, s.entry, i, $2, j.ledger_id, 3, '2026-03-01', CASE WHEN i = 1 THEN $4::uuid ELSE $5::uuid END,
			CASE WHEN i = 1 THEN 100 ELSE -100 END, CASE WHEN i = 1 THEN 100 ELSE -100 END, CASE WHEN i = 1 THEN 100 ELSE -100 END,
			CASE WHEN i = 1 THEN s.partner ELSE NULL END
		FROM s JOIN j ON j.id = s.entry, generate_series(1, 2) i`, e.Tenant.ID, e.DE, e.Tenant.Human.ID, e.receivable, e.revenue)
	exec(`INSERT INTO ledger.open_item_terms (tenant_id, fiscal_year, journal_entry_id, line_no, payment_term_code, baseline_date, net_days)
		SELECT l.tenant_id, l.fiscal_year, l.journal_entry_id, l.line_no, 'NT30', DATE '2026-01-01' + (abs(hashtext(l.journal_entry_id::text)) % 300), 30
		FROM ledger.journal_line l WHERE l.tenant_id = $1 AND l.account_id = $2 AND abs(hashtext(l.journal_entry_id::text)) % 2 = 0`,
		e.Tenant.ID, e.receivable)
	exec(`RESET session_replication_role`)
	exec(`ANALYZE ledger.journal_entry, ledger.journal_line, ledger.open_item_terms`)
	path := "/v1/receivables-aging?legal_entity_id=" + e.DE.String() + "&key_date=2026-06-30&page_size=100"
	var times []time.Duration
	next := ""
	for i := range 20 {
		start := time.Now()
		got, _ := e.Must(200, "GET", path+next, "", "")
		times = append(times, time.Since(start))
		totals, _ := got["totals"].(map[string]any)
		if totals["item_count"] != float64(100000) || totals["customer_count"] != float64(2000) || len(items(t, got)) != 100 {
			t.Fatalf("aged: got %v items of %v customers, %d on the page; want 100000 of 2000, 100 on the page", totals["item_count"],
				totals["customer_count"], len(items(t, got)))
		}
		next = "" // the first page and the second in turn
		if i%2 == 0 {
			next = "&page_token=" + got["next_page_token"].(string)
		}
	}
	slices.Sort(times)
	p95 := times[len(times)*95/100]
	t.Logf("AR aging over 100,000 open items of 2,000 customers, first and second page: median %v, p95 %v", times[len(times)/2], p95)
	if budget := nfr.Budget(3 * time.Second); p95 > budget {
		t.Fatalf("AR aging p95 %v exceeds the %v budget (3 s scaled by ERP_BUDGET_SCALE)", p95, budget)
	}
}
