package cash_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/nfr"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestCashPositionBudget holds the cash position to ADR-0051's read budget
// (p95 2 s, §6) at a large company's scale: 100,000 payments released from
// the USD bank since January, each posted to the leading and the parallel
// ledger, of which the last 2,000 are still in transit on the clearing
// account; 1,000 payments of proposed runs from the CAD bank; a statement
// a day with 20 posted lines; the 31 days of the longest position.
func TestCashPositionBudget(t *testing.T) {
	nfr.Measure(t)
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
// p99 6 s, ADR-0051 §6 as built T53e4, 01 §Performance): Maple Inc owes two
// suppliers set up in the company and its blocked vendor, each item with
// its terms (net 15-119 days from its document date, every third with a 2%
// tier) and its expense line, read in the company's buckets and converted
// into CAD, and one month's flows drilled down by liquidity item. Since
// T53g it also has 10,000 purchase orders of five items, 2,000 of them
// released and open (forecastOrderData). No index is added for the due
// date, which is computed: the read takes every open item, so an index on
// it would not narrow it (§6).
func TestLiquidityForecastBudget(t *testing.T) {
	nfr.Measure(t)
	e := setupForecast(t, withItems)
	e.forecastBudgetData(t)
	e.forecastOrderData(t)
	reportBudget(t, e.Client, "liquidity forecast over 100,000 open items", "/v1/liquidity-forecast?legal_entity_id="+e.le.String()+"&display_currency=CAD", func(f map[string]any) {
		cad := f["currencies"].([]any)[0].(map[string]any)
		if got := amount(cad["blocked"].(map[string]any)["amount"]); got == "0.00 CAD" {
			t.Fatalf("blocked: got %s, want the vendor's items", got)
		}
		if !strings.Contains(fmt.Sprint(cad["buckets"]), "purchase_order") {
			t.Fatal("orders: got no purchase_order flow, want the open orders'")
		}
	})
	reportBudget(t, e.Client, "drill-down of a month over 100,000 open items", "/v1/liquidity-forecast-flows?legal_entity_id="+e.le.String()+"&currency=CAD&from=2026-07-17&to=2026-08-16"+
		"&liquidity_item=OP_SUPPLIERS&page_size=500", func(p map[string]any) {
		if p["count"].(float64) == 0 {
			t.Fatal("drill-down: got no flows, want the month's")
		}
	})
}

// groupBudgetCompanies are the further companies of the group forecast's
// budget, each configured as Maple Inc is (withGroupBudget).
var groupBudgetCompanies = []fixture.Company{{Code: "3100", Name: "Maple West", Country: "CA", Currency: "CAD"},
	{Code: "3200", Name: "Maple East", Country: "CA", Currency: "CAD"}, {Code: "3300", Name: "Maple North", Country: "CA", Currency: "CAD"},
	{Code: "3400", Name: "Maple South", Country: "CA", Currency: "CAD"}}

// withGroupBudget is withItems with groupBudgetCompanies configured as
// Maple Inc is: its chart, house banks MAIN in CAD and USD in USD, payment
// method T and cash settings.
func withGroupBudget(doc *config.Config) {
	withItems(doc)
	for _, c := range groupBudgetCompanies {
		co := *doc.Company("3000")
		co.Code, co.Name = c.Code, c.Name
		doc.Companies = append(doc.Companies, co)
	}
}

// TestGroupLiquidityForecastBudget holds the group liquidity forecast to
// the budget of the company's (p95 3 s, p99 6 s; 01 §1.3, PO decision of
// 2026-11-04, ADR-0051 §9 as built T53e5) at 100,000 open items in total:
// Maple Inc and four companies configured as it is, 20,000 items each as
// forecastBudgetData writes them, every third in USD, so that the group's
// read pays its per-company cost five times and converts two currencies
// into EUR, the group currency, at planning rates; Acme Corp US and Acme
// GmbH, with no items, are read too, as every company the caller may act
// in is. Read in the default buckets.
func TestGroupLiquidityForecastBudget(t *testing.T) {
	nfr.Measure(t)
	e := setupForecastOf(t, groupBudgetCompanies, withGroupBudget)
	companies := []budgetCompany{{le: e.le, payables: e.payables, expense: e.expense}}
	for _, c := range groupBudgetCompanies {
		var le uuid.UUID
		pgtest.Scan(t, `SELECT id FROM tenancy.legal_entity WHERE tenant_id = $1 AND code = $2`, []any{e.Tenant.ID, c.Code}, &le)
		companies = append(companies, budgetCompany{le: le, payables: e.Account(t, c.Code, "211000"), expense: e.Account(t, c.Code, "520000")})
	}
	e.budgetItems(t, companies, 20000, true)
	e.rate(t, "planning", "CAD", "EUR", "2026-04-01", "0.660000000000")
	e.rate(t, "planning", "USD", "EUR", "2026-04-01", "0.920000000000")
	reportBudget(t, e.Client, "group liquidity forecast over 100,000 open items of five companies", "/v1/group-liquidity-forecast", func(g map[string]any) {
		if got := len(g["companies"].([]any)); got != 7 {
			t.Fatalf("companies: got %d, want 7", got)
		}
		// Each of the five companies converted both currencies: the read
		// took its items.
		for _, c := range g["companies"].([]any) {
			cm := c.(map[string]any)
			if strings.HasPrefix(cm["company_code"].(string), "3") {
				if got := len(cm["rates"].([]any)); got != 2 {
					t.Fatalf("rates of %s: got %d, want CAD's and USD's", cm["company_code"], got)
				}
			}
		}
		if got := g["converted"].(map[string]any)["incomplete_currencies"].([]any); len(got) != 0 {
			t.Fatalf("converted incomplete: got %v, want none", got)
		}
		blocked := map[string]string{}
		for _, c := range g["currencies"].([]any) {
			cm := c.(map[string]any)
			blocked[cm["currency"].(string)] = amount(cm["blocked"].(map[string]any)["amount"])
		}
		for _, code := range []string{"CAD", "USD"} {
			if got, ok := blocked[code]; !ok || got == "0.00 "+code {
				t.Fatalf("blocked in %s: got %q, want the vendor's items", code, got)
			}
		}
	})
}

// reportBudget holds a report's read to the report budget of 01 §1.3
// (p95 3 s, scaled by ERP_BUDGET_SCALE): the first read warms the caches
// and is not counted, then 20 reads are timed and each checked. The p95
// of 20 is the slowest read, so it holds the p99 of 6 s too.
func reportBudget(t *testing.T, c apitest.Client, what, query string, check func(map[string]any)) {
	t.Helper()
	c.Must(200, "GET", query, "", "")
	var times []time.Duration
	for range 20 {
		start := time.Now()
		f, _ := c.Must(200, "GET", query, "", "")
		times = append(times, time.Since(start))
		check(f)
	}
	slices.Sort(times)
	p95 := times[len(times)*95/100]
	t.Logf("%s: median %v, p95 (the slowest of 20) %v", what, times[len(times)/2], p95)
	if budget := nfr.Budget(3 * time.Second); p95 > budget {
		t.Fatalf("%s: p95 %v exceeds the %v budget (3 s scaled by ERP_BUDGET_SCALE)", what, p95, budget)
	}
}

// forecastBudgetData writes the budget's 100,000 open items straight into
// the leading ledger, without the checks a posting runs: 40,000 for
// Maple Inc's supplier, 50,000 for a further supplier and 10,000 for its
// blocked vendor.
func (e forecastEnv) forecastBudgetData(t *testing.T) {
	t.Helper()
	e.budgetItems(t, []budgetCompany{{le: e.le, payables: e.payables, expense: e.expense}}, 100000, false)
}

// budgetCompany is a company budgetItems writes open items for: its legal
// entity and its payables and expense accounts.
type budgetCompany struct{ le, payables, expense uuid.UUID }

// budgetItems writes perCompany open items of each company straight into
// the leading ledger, without the checks a posting runs: in each company,
// every tenth item the vendor's (blocked for payment there), the other
// even ones the supplier's and the odd ones a further supplier's, all paid
// by method T, each with its terms (net 15-119 days from its document
// date, every third with a 2% tier) and its expense line; in CAD, or with
// usd every third (the first, fourth and so on) in USD. Each line's local
// and group amounts repeat the transaction amount: the forecast reads the
// transaction currency's.
func (e forecastEnv) budgetItems(t *testing.T, companies []budgetCompany, perCompany int, usd bool) {
	t.Helper()
	other := e.NewSupplier(t, "BP90000041")
	les, payables, expenses := make([]uuid.UUID, len(companies)), make([]uuid.UUID, len(companies)), make([]uuid.UUID, len(companies))
	for i, c := range companies {
		les[i], payables[i], expenses[i] = c.le, c.payables, c.expense
	}
	e.exec(t, `UPDATE partner.supplier_company SET payment_methods = '{T}', is_payment_blocked = partner_id = $2, version = version + 1
		WHERE tenant_id = $1 AND legal_entity_id = ANY($3) AND partner_id IN ($2, $4, $5)`, e.Tenant.ID, e.Vendor, les, e.Supplier, other)
	e.exec(t, `SET session_replication_role = replica`)
	e.exec(t, `CREATE TEMP TABLE budget_item AS
		SELECT n, gen_random_uuid() AS id, date '2026-01-01' + (n % 107) AS day, c.k, c.le, c.payables, c.expense,
			CASE WHEN n % 10 = 0 THEN $1::uuid WHEN n % 2 = 0 THEN $2::uuid ELSE $3::uuid END AS partner,
			CASE WHEN $8 AND n % 3 = 1 THEN 'USD' ELSE 'CAD' END AS currency
		FROM unnest($4::uuid[], $5::uuid[], $6::uuid[]) WITH ORDINALITY AS c(le, payables, expense, k), generate_series(1, $7) n`,
		e.Vendor, e.Supplier, other, les, payables, expenses, perCompany, usd)
	e.exec(t, `INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type, document_number,
			document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, source_document_id, posted_by)
		SELECT $1, 2026, id, le, $2, extract(month FROM day), 'KR', 'B' || k || '-' || n, day, day, currency, 'CAD', 'EUR', 1, 1, 'payables', gen_random_uuid(), $3
		FROM budget_item`, e.Tenant.ID, e.leading, e.Tenant.Human.ID)
	e.exec(t, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period, posting_date,
			account_id, partner_id, amount_tc, amount_lc, amount_gc)
		SELECT $1, 2026, id, j, le, $2, extract(month FROM day), day, CASE WHEN j = 1 THEN payables ELSE expense END, CASE WHEN j = 1 THEN partner END,
			CASE WHEN j = 1 THEN -(1000 + n % 997) ELSE 1000 + n % 997 END, CASE WHEN j = 1 THEN -(1000 + n % 997) ELSE 1000 + n % 997 END,
			CASE WHEN j = 1 THEN -(1000 + n % 997) ELSE 1000 + n % 997 END
		FROM budget_item, generate_series(1, 2) j`, e.Tenant.ID, e.leading)
	e.exec(t, `INSERT INTO ledger.open_item_terms (tenant_id, fiscal_year, journal_entry_id, line_no, payment_term_code, baseline_date, net_days,
			discount_days, discount_pct)
		SELECT $1, 2026, id, 1, 'T', day, 15 + n % 105, CASE WHEN n % 3 = 0 THEN 10 END, CASE WHEN n % 3 = 0 THEN 2 END FROM budget_item`, e.Tenant.ID)
	e.exec(t, `DROP TABLE budget_item`)
	e.exec(t, `RESET session_replication_role`)
	e.exec(t, `ANALYZE ledger.journal_entry, ledger.journal_line, ledger.open_item_terms, partner.supplier_company`)
}

// forecastOrderData writes the budget's purchase orders straight into
// procurement and payables, without the checks an order's release and an
// invoice's posting run: 10,000 orders of five items of Maple's two
// suppliers, 8,000 closed and 2,000 released, each item 10 EA at 1.00 to
// 99.00, the first two with V1, the first three delivered on a day of the
// next 300 (the others by the lead days), and a posted invoice of 2 EA on
// each released order's first item.
func (e forecastEnv) forecastOrderData(t *testing.T) {
	t.Helper()
	e.exec(t, `SET session_replication_role = replica`)
	e.exec(t, `CREATE TEMP TABLE budget_order AS
		SELECT n, gen_random_uuid() AS id, CASE WHEN n % 2 = 0 THEN $1::uuid ELSE (SELECT partner_id FROM partner.supplier_company
			WHERE legal_entity_id = $2 AND partner_id NOT IN ($1, $3) ORDER BY partner_id LIMIT 1) END AS supplier,
			CASE WHEN n % 5 = 0 THEN 'released' ELSE 'closed' END AS status
		FROM generate_series(1, 10000) n`, e.Supplier, e.le, e.Vendor)
	e.exec(t, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
			payment_term_code, status, created_by, updated_by)
		SELECT $1, id, 'PB' || n, $2, supplier, 'CAD', date '2026-01-01' + (n % 100), 'NT30', status, $3, $3 FROM budget_order`,
		e.Tenant.ID, e.le, e.Tenant.Human.ID)
	e.exec(t, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code, unit_price,
			account_id, tax_code, valuation_price, valuation_price_unit, delivery_date)
		SELECT $1, id, k, 'Goods', 10, 'EA', 100 * (1 + (n + k) % 99), $2, CASE WHEN k <= 2 THEN 'V1' END, 100 * (1 + (n + k) % 99), 1,
			CASE WHEN k <= 3 THEN date '2026-04-17' + ((n * k) % 300) END
		FROM budget_order, generate_series(1, 5) k`, e.Tenant.ID, e.expense)
	e.exec(t, `INSERT INTO payables.supplier_invoice (tenant_id, id, document_number, legal_entity_id, supplier_id, supplier_reference, invoice_date,
			currency, gross_amount, tax_amount, purchase_order_id, status, source, kind, journal_entry_year, journal_entry_id, created_by, updated_by)
		SELECT $1, gen_random_uuid(), 'IB' || n, $2, supplier, 'IB' || n, date '2026-04-01', 'CAD', 200, 0, id, 'posted', 'manual', 'invoice', 2026,
			gen_random_uuid(), $3, $3 FROM budget_order WHERE status = 'released'`, e.Tenant.ID, e.le, e.Tenant.Human.ID)
	e.exec(t, `INSERT INTO payables.supplier_invoice_item (tenant_id, supplier_invoice_id, item_no, purchase_order_id, po_item_no, quantity, uom_code, amount)
		SELECT $1, i.id, 1, i.purchase_order_id, 1, 2, 'EA', 200 FROM payables.supplier_invoice i WHERE i.document_number LIKE 'IB%'`, e.Tenant.ID)
	e.exec(t, `DROP TABLE budget_order`)
	e.exec(t, `RESET session_replication_role`)
	e.exec(t, `ANALYZE procurement.purchase_order, procurement.purchase_order_item, payables.supplier_invoice, payables.supplier_invoice_item`)
}
