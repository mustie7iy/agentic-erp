package collections_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/nfr"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestCollectionWorklistBudget holds the collection worklist to the
// budget of a report computed on read over 100,000 open items (01 §1.3:
// p95 3 s, as the liquidity forecast's; ADR-0065 §9) at that volume:
// 2,000 customers with 50 open items each on the company's receivables
// account, all posted with payment terms, about three in four overdue on
// the key date, every item dunned once. A page of 100 customers reads
// every open item of the company, then the names and the recorded dunning
// of the page's customers' 5,000 items.
func TestCollectionWorklistBudget(t *testing.T) {
	nfr.Measure(t)
	e := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	admin := pgtest.Admin(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	exec(`INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by)
		SELECT $1, md5('c' || n)::uuid, 'BP8' || lpad(n::text, 7, '0'), 'organization', 'Customer ' || n, 'DE', 'active', $2, $2
		FROM generate_series(0, 1999) n`, e.Tenant.ID, e.Tenant.Human.ID)
	// The items are written straight into the journal, balanced, without
	// the row checks a posting runs (as TestOpenItemsAgingBudget seeds
	// them); the recorded levels without their runs' proposals.
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
			CASE WHEN i = 1 THEN 100 + s.n % 900 ELSE -100 - s.n % 900 END, CASE WHEN i = 1 THEN 100 + s.n % 900 ELSE -100 - s.n % 900 END,
			CASE WHEN i = 1 THEN 100 + s.n % 900 ELSE -100 - s.n % 900 END, CASE WHEN i = 1 THEN s.partner END
		FROM s JOIN j ON j.id = s.entry, generate_series(1, 2) i`, e.Tenant.ID, e.DE, e.Tenant.Human.ID, e.receivable, e.revenue)
	// Due between 31 January and 26 November 2026: about three in four before the key date, 15 September.
	exec(`INSERT INTO ledger.open_item_terms (tenant_id, fiscal_year, journal_entry_id, line_no, payment_term_code, baseline_date, net_days)
		SELECT l.tenant_id, l.fiscal_year, l.journal_entry_id, l.line_no, 'NT30', DATE '2026-01-01' + (abs(hashtext(l.journal_entry_id::text)) % 300), 30
		FROM ledger.journal_line l WHERE l.tenant_id = $1 AND l.account_id = $2`, e.Tenant.ID, e.receivable)
	exec(`INSERT INTO collections.dunned_item (tenant_id, fiscal_year, journal_entry_id, line_no, dunning_run_id, level, dunning_date)
		SELECT l.tenant_id, l.fiscal_year, l.journal_entry_id, l.line_no, md5('run')::uuid, 1, '2026-08-31'
		FROM ledger.journal_line l WHERE l.tenant_id = $1 AND l.account_id = $2`, e.Tenant.ID, e.receivable)
	exec(`RESET session_replication_role`)
	exec(`ANALYZE ledger.journal_entry, ledger.journal_line, ledger.open_item_terms, collections.dunned_item, partner.business_partner`)
	var times []time.Duration
	for range 10 {
		start := time.Now()
		page := e.worklist(t, "2026-09-15", "&page_size=100")
		times = append(times, time.Since(start))
		first := objects(t, page, "items")[0]
		if n := len(objects(t, page, "items")); n != 100 || page["customer_count"] != float64(2000) || first["dunning_level"] != float64(1) ||
			first["last_dunning_date"] != "2026-08-31" {
			t.Fatalf("the page: got %d of %v customers, the first at level %v dunned %v; want 100 of 2000, level 1 on 2026-08-31", n,
				page["customer_count"], first["dunning_level"], first["last_dunning_date"])
		}
	}
	slices.Sort(times)
	p95 := times[len(times)*95/100]
	t.Logf("collection worklist over 100,000 open items of 2,000 customers, a page of 100: median %v, p95 %v", times[len(times)/2], p95)
	if budget := nfr.Budget(3 * time.Second); p95 > budget {
		t.Fatalf("collection worklist p95 %v exceeds the %v budget (3 s scaled by ERP_BUDGET_SCALE)", p95, budget)
	}
}
