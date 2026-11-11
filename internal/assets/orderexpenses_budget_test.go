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

// TestAssetOrderExpensesBudget checks the report on asset purchase orders
// (B10; ADR-0057 As built) against the report budget the asset history
// sheet is held to (p95 1 s; 01-requirements sizes neither): a company's
// year with 2,000 asset order items, far more than a pilot company buys,
// each with an invoice entry whose price difference line is recorded,
// half of them with an earlier invoice entry that is not, and half
// cleared against the price difference account, beside 20,000 recorded
// lines of order items that are no assets. The report aggregates its
// rows in memory and pages them there, so every page costs the whole
// year. The rows are written straight into the journal, without the row
// checks a posting runs (as TestClearingItemsBudget seeds them), within
// a deadline of their own.
func TestAssetOrderExpensesBudget(t *testing.T) {
	nfr.Measure(t)
	e := newOrderExpenses(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	admin := pgtest.Admin(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	clearing, expense := e.Account(t, "2000", "169000"), e.Account(t, "2000", "520000")
	grir, diff, ap := e.Account(t, "2000", "219000"), e.Account(t, "2000", "590000"), e.Account(t, "2000", "211000")
	start := time.Now()
	exec(`SET session_replication_role = replica`)
	// Orders 1..2,000 hold an asset's item, the rest an expense item. Each
	// has an invoice entry (kind 1); the assets' odd ones an earlier,
	// unrecorded one (kind 2), their even ones a clearing (kind 3).
	exec(`CREATE TEMP TABLE seed AS SELECT n, n <= 2000 AS asset, gen_random_uuid() AS po FROM generate_series(1, 22000) n`)
	exec(`CREATE TEMP TABLE doc AS SELECT s.n, s.po, k AS kind, gen_random_uuid() AS entry FROM seed s, generate_series(1, 3) k
		WHERE k = 1 OR (s.asset AND k = 2 + (s.n + 1) % 2)`)
	exec(`INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date, payment_term_code,
			status, valuation, created_by, updated_by)
		SELECT $1, s.po, 'B' || lpad(s.n::text, 6, '0'), $2, $3, 'EUR', '2026-01-01', 'NT30', 'released', 'gr_ir', $4, $4 FROM seed s`,
		e.Tenant.ID, e.DE, e.Supplier, e.Tenant.Human.ID)
	exec(`INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code, unit_price, price_unit,
			account_id, valuation_price, valuation_price_unit)
		SELECT $1, s.po, 1, 'Item ' || s.n, 1, 'EA', 10000, 1, CASE WHEN s.asset THEN $2::uuid ELSE $3::uuid END, 10000, 1 FROM seed s`,
		e.Tenant.ID, clearing, expense)
	exec(`INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type, document_number,
			document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, source_document_id, posted_by,
			posting_group_id)
		SELECT $1, 2026, d.entry, $2, $3, 1 + d.n % 12, CASE WHEN d.kind = 3 THEN 'WE' ELSE 'KR' END, 'B' || d.kind || d.n, DATE '2026-01-01' + d.n % 360,
			DATE '2026-01-01' + d.n % 360, 'EUR', 'EUR', 'EUR', 1, 1, CASE WHEN d.kind = 3 THEN 'procurement' ELSE 'payables' END, d.entry, $4, d.entry
		FROM doc d`, e.Tenant.ID, e.DE, e.l0, e.Tenant.Human.ID)
	// Line 1 on GR/IR names the order item, line 2 is its difference (an
	// invoice's 5.00 over, a clearing's the whole 100.00), line 3 the
	// supplier's on an invoice.
	exec(`INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period, posting_date,
			account_id, amount_tc, amount_lc, amount_gc, partner_id, purchase_order_id, po_item_no, quantity, uom_code)
		SELECT $1, 2026, d.entry, i, $2, $3, 1 + d.n % 12, DATE '2026-01-01' + d.n % 360,
			CASE i WHEN 1 THEN $4::uuid WHEN 2 THEN $5::uuid ELSE $6::uuid END, x.amount, x.amount, x.amount, CASE WHEN i = 3 THEN $7::uuid END,
			CASE WHEN i = 1 THEN d.po END, CASE WHEN i = 1 THEN 1 END, CASE WHEN i = 1 THEN 1::numeric END, CASE WHEN i = 1 THEN 'EA' END
		FROM doc d, generate_series(1, 3) i,
			LATERAL (SELECT CASE WHEN d.kind = 3 THEN (ARRAY[10000, -10000])[i] ELSE (ARRAY[10000, 500, -10500])[i] END::bigint AS amount) x
		WHERE i < 3 OR d.kind < 3`, e.Tenant.ID, e.DE, e.l0, grir, diff, ap, e.Supplier)
	exec(`INSERT INTO ledger.price_difference_item (tenant_id, fiscal_year, journal_entry_id, line_no, purchase_order_id, po_item_no, tax_borne_tc,
			tax_borne_lc) SELECT $1, 2026, d.entry, 2, d.po, 1, 200, 200 FROM doc d WHERE d.kind = 1`, e.Tenant.ID)
	exec(`INSERT INTO ledger.gr_ir_clearing_entry (tenant_id, fiscal_year, journal_entry_id) SELECT $1, 2026, d.entry FROM doc d WHERE d.kind = 3`, e.Tenant.ID)
	exec(`RESET session_replication_role`)
	exec(`ANALYZE procurement.purchase_order, procurement.purchase_order_item, ledger.journal_entry, ledger.journal_line, ledger.price_difference_item,
		ledger.gr_ir_clearing_entry`)
	t.Logf("seeded 22,000 order items with 24,000 entries in %v", time.Since(start))
	var times []time.Duration
	for range 10 {
		start := time.Now()
		r, _ := e.Must(200, "GET", "/v1/asset-order-expenses?legal_entity_id="+e.DE.String()+"&fiscal_year=2026", "", "")
		times = append(times, time.Since(start))
		if n := len(r["items"].([]any)); n != 50 {
			t.Fatalf("rows on the page: got %d, want 50", n)
		}
		totals, un := r["totals"].(map[string]any), r["unattributed"].(map[string]any)
		want(t, "the totals of 2,000 asset order items", moneyOf(totals["tax_not_recovered"])+", "+moneyOf(totals["price_difference"])+", "+
			moneyOf(totals["expensed"])+", "+moneyOf(totals["gr_ir_cleared"]), "4000.00 EUR, 6000.00 EUR, 10000.00 EUR, -100000.00 EUR")
		want(t, "the unattributed of 1,000 earlier entries", fmt.Sprint(moneyOf(un["expensed"]), " in ", un["entries"]), "5000.00 EUR in 1000")
	}
	slices.Sort(times)
	p95 := times[len(times)*95/100]
	t.Logf("asset order expenses over 2,000 asset order items among 22,000: median %v, p95 %v", times[len(times)/2], p95)
	if budget := nfr.Budget(time.Second); p95 > budget {
		t.Errorf("asset order expenses: p95 %v exceeds the %v budget (1 s scaled by ERP_BUDGET_SCALE)", p95, budget)
	}
}
