package assets_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/nfr"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestClearingItemsBudget checks 01-requirements' list budget (p95 150 ms)
// on the worklist of acquisitions (ADR-0057 §6): a company's year with
// 20,000 lines on its clearing account (a clearing account takes asset
// purchases only, far fewer than the journal's lines), each against a
// bank line, half of them capitalised in full by an acquisition, listed
// with and without remaining=true. The lines are written straight into
// the journal, without the row checks a posting runs (as
// TestOpenItemsAgingBudget seeds them), within a deadline of their own.
func TestClearingItemsBudget(t *testing.T) {
	nfr.Measure(t)
	e := newAcquisitions(t)
	a, _ := e.create(t, e.body(e.DE, "MACHINES", fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	admin := pgtest.Admin(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	clearing, bank := e.Account(t, "2000", "169000"), e.Account(t, "2000", "100000")
	start := time.Now()
	exec(`SET session_replication_role = replica`)
	exec(`CREATE TEMP TABLE seed AS SELECT n, gen_random_uuid() AS entry FROM generate_series(1, 20000) n`)
	exec(`INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type, document_number,
			document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, posted_by, posting_group_id)
		SELECT $1, 2026, s.entry, $2, $3, 1 + s.n % 12, 'SA', 'B' || s.n, DATE '2026-01-01' + s.n % 360, DATE '2026-01-01' + s.n % 360, 'EUR', 'EUR', 'EUR',
			1, 1, 'ledger', $4, s.entry FROM seed s`, e.Tenant.ID, e.DE, e.l0, e.Tenant.Human.ID)
	exec(`INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period, posting_date,
			account_id, amount_tc, amount_lc, amount_gc)
		SELECT $1, 2026, s.entry, i, $2, $3, 1 + s.n % 12, DATE '2026-01-01' + s.n % 360, CASE WHEN i = 1 THEN $4::uuid ELSE $5::uuid END,
			CASE WHEN i = 1 THEN 10000 ELSE -10000 END, CASE WHEN i = 1 THEN 10000 ELSE -10000 END, CASE WHEN i = 1 THEN 10000 ELSE -10000 END
		FROM seed s, generate_series(1, 2) i`, e.Tenant.ID, e.DE, e.l0, clearing, bank)
	exec(`INSERT INTO assets.clearing_use (tenant_id, entry_year, entry_id, line_no, legal_entity_id, currency, line_amount, line_lc, line_gc, used, used_lc, used_gc)
		SELECT $1, 2026, s.entry, 1, $2, 'EUR', 10000, 10000, 10000, 10000, 10000, 10000 FROM seed s WHERE s.n % 2 = 0`, e.Tenant.ID, e.DE)
	exec(`INSERT INTO assets.acquisition (tenant_id, id, legal_entity_id, asset_id, seq, value_date, posting_date, fiscal_year, currency, amount, amount_lc,
			amount_gc, currency_lc, currency_gc, source_entry_year, source_entry_id, source_line_no, source_document_number, source_context, status, created_by,
			updated_by)
		SELECT $1, gen_random_uuid(), $2, $3, 1 + s.n, '2026-12-31', '2026-12-31', 2026, 'EUR', 10000, 10000, 10000, 'EUR', 'EUR', 2026, s.entry, 1, 'B' || s.n,
			'ledger', 'posted', $4, $4 FROM seed s WHERE s.n % 2 = 0`, e.Tenant.ID, e.DE, a["id"], e.Tenant.Human.ID)
	exec(`RESET session_replication_role`)
	exec(`ANALYZE ledger.journal_entry, ledger.journal_line, assets.clearing_use, assets.acquisition`)
	t.Logf("seeded 20,000 clearing lines in %v", time.Since(start))
	for _, query := range []string{"", "&remaining=true"} {
		var times []time.Duration
		for range 10 {
			start := time.Now()
			page, _ := e.Must(200, "GET", "/v1/asset-clearing-items?legal_entity_id="+e.DE.String()+"&fiscal_year=2026"+query, "", "")
			times = append(times, time.Since(start))
			if n := len(page["items"].([]any)); n != 50 {
				t.Fatalf("lines on the page: got %d, want 50", n)
			}
		}
		slices.Sort(times)
		p95 := times[len(times)*95/100]
		t.Logf("clearing items %q over 20,000 lines: median %v, p95 %v", query, times[len(times)/2], p95)
		if budget := nfr.Budget(150 * time.Millisecond); p95 > budget {
			t.Errorf("clearing items %q: p95 %v exceeds the %v budget (150 ms scaled by ERP_BUDGET_SCALE)", query, p95, budget)
		}
	}
}
