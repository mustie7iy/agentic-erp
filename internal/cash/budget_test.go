package cash_test

import (
	"slices"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/nfr"
)

// TestCashPositionBudget holds the cash position to ADR-0051's read budget
// (p95 2 s, §6) at a large company's scale: 100,000 payments released from
// the USD bank since January, each posted to the leading and the parallel
// ledger, of which the last 2,000 are still in transit on the clearing
// account; 1,000 payments of proposed runs from the CAD bank; a statement
// a day with 20 posted lines; the 31 days of the longest position.
func TestCashPositionBudget(t *testing.T) {
	if testing.Short() || raceDetector {
		t.Skip("performance budget: runs without -short and without the race detector (make perf, CI)")
	}
	e := setupPosition(t)
	e.budgetData(t)
	var times []time.Duration
	for range 20 {
		start := time.Now()
		p := e.positionOf(t, "&days=31")
		times = append(times, time.Since(start))
		usd := bankOf(t, p, "USD")
		if got := amount(usd["book_balance"].(map[string]any)["payments_in_transit"]); got != "-20000.00 USD" {
			t.Fatalf("payments in transit: got %s, want -20000.00 USD", got)
		}
	}
	slices.Sort(times)
	p95 := times[len(times)*95/100]
	t.Logf("cash position over 100,000 payments (2,000 in transit), 1,000 proposed and 106 statements, 31 days: median %v, p95 %v", times[len(times)/2], p95)
	if budget := nfr.Budget(2 * time.Second); p95 > budget {
		t.Fatalf("cash position p95 %v exceeds the %v budget (2 s scaled by ERP_BUDGET_SCALE)", p95, budget)
	}
}

// budgetData writes the budget's payments, entries, clearings and
// statements straight into the tables, without the checks a posting runs;
// each payment pays a supplier of its own, as payment_items_once wants.
func (e positionEnv) budgetData(t *testing.T) {
	t.Helper()
	ten, le, by := e.Tenant.ID, e.le, e.Tenant.Human.ID
	e.exec(t, `SET session_replication_role = replica`)
	e.exec(t, `WITH r AS (
			INSERT INTO payments.payment_run (tenant_id, id, document_number, legal_entity_id, house_bank_id, run_date, due_by, currency, total_amount, status,
				proposed_by, decided_by, decided_at, created_by, updated_by)
			SELECT $1, gen_random_uuid(), 'B' || n, $2, $3, date '2026-01-01' + n, date '2026-01-01' + n, 'USD', 1000000, 'released', $4, $5, now(), $4, $4
			FROM generate_series(1, 100) n RETURNING id)
		INSERT INTO payments.payment (tenant_id, id, payment_run_id, supplier_id, bank_account_id, amount, currency, end_to_end_id, journal_entry_year,
			journal_entry_id, created_by)
		SELECT $1, gen_random_uuid(), r.id, gen_random_uuid(), gen_random_uuid(), 1000, 'USD', md5(random()::text), 2026, gen_random_uuid(), $4
		FROM r, generate_series(1, 1000)`, ten, le, e.usd, by, e.Approver.ID)
	e.exec(t, `WITH x AS (
			SELECT p.id, p.journal_entry_id, r.run_date, g.id AS ledger, g.is_leading FROM payments.payment p
			JOIN payments.payment_run r ON r.tenant_id = p.tenant_id AND r.id = p.payment_run_id AND r.document_number LIKE 'B%'
			JOIN ledger.ledger g ON g.tenant_id = p.tenant_id WHERE p.tenant_id = $1),
		en AS (
			INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type, document_number,
				document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, source_document_id, posted_by)
			SELECT $1, 2026, CASE WHEN is_leading THEN journal_entry_id ELSE gen_random_uuid() END, $2, ledger, extract(month FROM run_date), 'ZP',
				md5(random()::text), run_date, run_date, 'USD', 'CAD', 'EUR', 1.35, 0.68, 'payments', id, $3 FROM x
			RETURNING id, ledger_id, posting_date, fiscal_period)
		INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period, posting_date,
			account_id, amount_tc, amount_lc, amount_gc)
		SELECT $1, 2026, en.id, n, $2, en.ledger_id, en.fiscal_period, en.posting_date, CASE WHEN n = 1 THEN $4::uuid ELSE $5::uuid END,
			CASE WHEN n = 1 THEN -1000 ELSE 1000 END, CASE WHEN n = 1 THEN -1350 ELSE 1350 END, CASE WHEN n = 1 THEN -918 ELSE 918 END
		FROM en, generate_series(1, 2) n`, ten, le, by, e.clearing, e.expense)
	// Cleared but for the payments of the last two runs.
	e.exec(t, `INSERT INTO ledger.clearing_item (tenant_id, fiscal_year, journal_entry_id, line_no, clearing_id)
		SELECT $1, 2026, p.journal_entry_id, 1, gen_random_uuid() FROM payments.payment p
		JOIN payments.payment_run r ON r.tenant_id = p.tenant_id AND r.id = p.payment_run_id AND r.document_number LIKE 'B%'
		WHERE p.tenant_id = $1 AND r.document_number NOT IN ('B99', 'B100')`, ten)
	e.exec(t, `WITH r AS (
			INSERT INTO payments.payment_run (tenant_id, id, document_number, legal_entity_id, house_bank_id, run_date, due_by, currency, total_amount, status,
				proposed_by, created_by, updated_by)
			SELECT $1, gen_random_uuid(), 'P' || n, $2, $3, date '2026-04-16' + n, date '2026-04-16' + n, 'CAD', 50000, 'proposed', $4, $4, $4
			FROM generate_series(1, 10) n RETURNING id)
		INSERT INTO payments.payment (tenant_id, id, payment_run_id, supplier_id, bank_account_id, amount, currency, end_to_end_id, created_by)
		SELECT $1, gen_random_uuid(), r.id, gen_random_uuid(), gen_random_uuid(), 500, 'CAD', md5(random()::text), $4 FROM r, generate_series(1, 100)`,
		ten, le, e.main, by)
	e.exec(t, `WITH s AS (
			INSERT INTO payments.bank_statement (tenant_id, id, house_bank_id, legal_entity_id, format, file_sha256, file_position, file_ref, statement_key,
				statement_ref, sequence, account, currency, opening_balance, opening_date, closing_balance, closing_date, line_count, status, created_by, updated_by)
			SELECT $1, gen_random_uuid(), $2, $3, 'camt.053.001.02', sha256(n::text::bytea), 1, 'statements/' || n, 'S' || n, 'S' || n, n::text, 'ACCOUNT', 'USD',
				10000000, date '2025-12-31' + n, 10000000, date '2025-12-31' + n, 20, 'imported', $4, $4 FROM generate_series(1, 106) n RETURNING id, closing_date)
		INSERT INTO payments.bank_statement_line (tenant_id, statement_id, line_no, amount, booking_date, value_date, transaction_code, bank_reference,
			owner_reference, text, status, created_by, updated_by)
		SELECT $1, s.id, n, CASE WHEN n % 2 = 0 THEN 100 ELSE -100 END, s.closing_date, s.closing_date, 'PMNT-ICDT-ESCT', 'B', 'O', 'T', 'posted', $4, $4
		FROM s, generate_series(1, 20) n`, ten, e.usd, le, by)
	e.exec(t, `RESET session_replication_role`)
	e.exec(t, `ANALYZE ledger.journal_entry, ledger.journal_line, ledger.clearing_item, payments.payment, payments.payment_run, payments.bank_statement,
		payments.bank_statement_line`)
}

// TestLiquidityForecastBudget holds the liquidity forecast and its
// drill-down to their read budget at 100,000 open supplier items (p95 3 s,
// ADR-0051 §6 as built T53e4, 01 §Performance): Maple Inc owes two
// suppliers set up in the company and its blocked vendor, each item with
// its terms (net 15-119 days from its document date, every third with a 2%
// tier) and its expense line, read in the company's buckets and converted
// into CAD, and one month's flows drilled down by liquidity item. No index
// is added for the due date, which is computed: the read takes every open
// item, so an index on it would not narrow it (§6). The first read of each
// warms the caches and is not counted.
func TestLiquidityForecastBudget(t *testing.T) {
	if testing.Short() || raceDetector {
		t.Skip("performance budget: runs without -short and without the race detector (make perf, CI)")
	}
	e := setupForecast(t, withItems)
	e.forecastBudgetData(t)
	budget := nfr.Budget(3 * time.Second)
	measure := func(what, query string, check func(map[string]any)) {
		t.Helper()
		e.Must(200, "GET", query, "", "")
		var times []time.Duration
		for range 20 {
			start := time.Now()
			f, _ := e.Must(200, "GET", query, "", "")
			times = append(times, time.Since(start))
			check(f)
		}
		slices.Sort(times)
		p95 := times[len(times)*95/100]
		t.Logf("%s over 100,000 open items: median %v, p95 %v", what, times[len(times)/2], p95)
		if p95 > budget {
			t.Fatalf("%s p95 %v exceeds the %v budget (3 s scaled by ERP_BUDGET_SCALE)", what, p95, budget)
		}
	}
	measure("liquidity forecast", "/v1/liquidity-forecast?legal_entity_id="+e.le.String()+"&display_currency=CAD", func(f map[string]any) {
		if got := amount(f["currencies"].([]any)[0].(map[string]any)["blocked"].(map[string]any)["amount"]); got == "0.00 CAD" {
			t.Fatalf("blocked: got %s, want the vendor's items", got)
		}
	})
	measure("drill-down of a month", "/v1/liquidity-forecast-flows?legal_entity_id="+e.le.String()+"&currency=CAD&from=2026-07-17&to=2026-08-16"+
		"&liquidity_item=OP_SUPPLIERS&page_size=500", func(p map[string]any) {
		if p["count"].(float64) == 0 {
			t.Fatal("drill-down: got no flows, want the month's")
		}
	})
}

// forecastBudgetData writes the budget's 100,000 open items straight into
// the leading ledger, without the checks a posting runs: 45,000 for each
// supplier set up in Maple Inc and 10,000 for its blocked vendor.
func (e forecastEnv) forecastBudgetData(t *testing.T) {
	t.Helper()
	other := e.NewSupplier(t, "BP90000041")
	e.exec(t, `UPDATE partner.supplier_company SET payment_methods = '{T}', version = version + 1 WHERE legal_entity_id = $1 AND partner_id = $2`, e.le, other)
	e.exec(t, `SET session_replication_role = replica`)
	e.exec(t, `CREATE TEMP TABLE budget_item AS
		SELECT n, gen_random_uuid() AS id, date '2026-01-01' + (n % 107) AS day,
			CASE WHEN n % 10 = 0 THEN $1::uuid WHEN n % 2 = 0 THEN $2::uuid ELSE $3::uuid END AS partner
		FROM generate_series(1, 100000) n`, e.Vendor, e.Supplier, other)
	e.exec(t, `INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type, document_number,
			document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, source_document_id, posted_by)
		SELECT $1, 2026, id, $2, $3, extract(month FROM day), 'KR', 'B' || n, day, day, 'CAD', 'CAD', 'EUR', 1, 1, 'payables', gen_random_uuid(), $4
		FROM budget_item`, e.Tenant.ID, e.le, e.leading, e.Tenant.Human.ID)
	e.exec(t, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period, posting_date,
			account_id, partner_id, amount_tc, amount_lc, amount_gc)
		SELECT $1, 2026, id, k, $2, $3, extract(month FROM day), day, CASE WHEN k = 1 THEN $4::uuid ELSE $5::uuid END, CASE WHEN k = 1 THEN partner END,
			CASE WHEN k = 1 THEN -(1000 + n % 997) ELSE 1000 + n % 997 END, CASE WHEN k = 1 THEN -(1000 + n % 997) ELSE 1000 + n % 997 END,
			CASE WHEN k = 1 THEN -(1000 + n % 997) ELSE 1000 + n % 997 END
		FROM budget_item, generate_series(1, 2) k`, e.Tenant.ID, e.le, e.leading, e.payables, e.expense)
	e.exec(t, `INSERT INTO ledger.open_item_terms (tenant_id, fiscal_year, journal_entry_id, line_no, payment_term_code, baseline_date, net_days,
			discount_days, discount_pct)
		SELECT $1, 2026, id, 1, 'T', day, 15 + n % 105, CASE WHEN n % 3 = 0 THEN 10 END, CASE WHEN n % 3 = 0 THEN 2 END FROM budget_item`, e.Tenant.ID)
	e.exec(t, `DROP TABLE budget_item`)
	e.exec(t, `RESET session_replication_role`)
	e.exec(t, `ANALYZE ledger.journal_entry, ledger.journal_line, ledger.open_item_terms`)
}
