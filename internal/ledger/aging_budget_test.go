package ledger_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestOpenItemsAgingBudget measures the open items' first page with and
// without their aging (ADR-0052 G4) at the volume 01-requirements names
// for a company's supplier open items, the payment proposal's 100,000:
// 2,000 suppliers with 50 items each on the payables account, half with
// payment terms. The aging reads every item the filters select, so it
// may cost at most what the page's own read costs; the page itself does
// not meet 01's list budget (p95 150 ms) at this volume, with or without
// the aging (0.85 s and 1.3 s on a laptop when measured, T73j): that is
// tracker item T73j3's. The pilot's own volumes are not known yet
// (ADR-0052, facts).
func TestOpenItemsAgingBudget(t *testing.T) {
	if testing.Short() || raceDetector {
		t.Skip("performance budget: runs without -short and without the race detector (make perf, CI)")
	}
	e := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), seedingDeadline)
	defer cancel()
	admin := pgtest.Admin(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	ap, expense := e.account(t, "2000", "211000"), e.account(t, "2000", "520000")
	// The items are written straight into the journal, balanced, without
	// the row checks a posting runs (as TestProposalBudget seeds them).
	exec(`SET session_replication_role = replica`)
	exec(`WITH s AS (
			SELECT n, gen_random_uuid() AS entry, md5('p' || (n % 2000))::uuid AS partner FROM generate_series(1, 100000) n
		), j AS (
			INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type, document_number,
				document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, reference, source_context, posted_by)
			SELECT $1, 2026, entry, $2, l.id, 3, 'KR', 'B' || n, '2026-03-01', '2026-03-01', 'EUR', 'EUR', 'EUR', 1, 1, 'INV-' || n, 'payables', $3
			FROM s, ledger.ledger l WHERE l.tenant_id = $1 AND l.is_leading RETURNING id, ledger_id
		) INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period, posting_date,
			account_id, amount_tc, amount_lc, amount_gc, partner_id)
		SELECT $1, 2026, s.entry, i, $2, j.ledger_id, 3, '2026-03-01', CASE WHEN i = 1 THEN $4::uuid ELSE $5::uuid END,
			CASE WHEN i = 1 THEN 100 ELSE -100 END, CASE WHEN i = 1 THEN 100 ELSE -100 END, CASE WHEN i = 1 THEN 100 ELSE -100 END,
			CASE WHEN i = 1 THEN NULL ELSE s.partner END
		FROM s JOIN j ON j.id = s.entry, generate_series(1, 2) i`, e.ten.ID, e.de, e.ten.Human.ID, expense, ap)
	exec(`INSERT INTO ledger.open_item_terms (tenant_id, fiscal_year, journal_entry_id, line_no, payment_term_code, baseline_date, net_days)
		SELECT l.tenant_id, l.fiscal_year, l.journal_entry_id, l.line_no, 'NT30', DATE '2026-01-01' + (abs(hashtext(l.journal_entry_id::text)) % 300), 30
		FROM ledger.journal_line l WHERE l.tenant_id = $1 AND l.account_id = $2 AND abs(hashtext(l.journal_entry_id::text)) % 2 = 0`, e.ten.ID, ap)
	exec(`RESET session_replication_role`)
	exec(`ANALYZE ledger.journal_entry, ledger.journal_line, ledger.open_item_terms`)
	path := "/v1/open-items?legal_entity_id=" + e.de.String() + "&account_id=" + ap + "&page_size=100"
	p95 := map[string]time.Duration{}
	for _, aged := range []string{"", "&aging_key_date=2026-06-30"} {
		var times []time.Duration
		for range 10 {
			start := time.Now()
			page, _ := e.Must(200, "GET", path+aged, "", "")
			times = append(times, time.Since(start))
			if aged == "" {
				continue
			}
			var n float64
			for _, b := range page["aging"].([]any) {
				n += b.(map[string]any)["item_count"].(float64)
			}
			if n != 100000 {
				t.Fatalf("aged items: got %v, want 100000", n)
			}
		}
		slices.Sort(times)
		p95[aged] = times[len(times)*95/100]
		t.Logf("open items %q over 100,000 items: median %v, p95 %v", aged, times[len(times)/2], p95[aged])
	}
	if aged, page := p95["&aging_key_date=2026-06-30"], p95[""]; aged > 2*page {
		t.Errorf("open items aged: p95 %v, more than twice the page's own %v", aged, page)
	}
}
