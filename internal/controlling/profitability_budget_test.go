package controlling_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/nfr"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestProfitabilityReportBudget holds net revenue by profitability
// segment to the week 3 budget of a report read (p95 3 s) at the volume
// the receivables reports are measured at (01-requirements: 100,000 open
// items of one company): a company's 100,000 posted billing documents of
// a year, each of two items, one with an item key and a discount, the
// other without a key, for 1,000 customers and 20 item keys (21,000
// segments), 5,000 posted credit memos, and 20,000 billing documents of
// the year before, which the report reads past. The first page of 100
// segments is read; every page computes every segment and its totals.
func TestProfitabilityReportBudget(t *testing.T) {
	nfr.Measure(t)
	p := setupProfitable(t)
	profitabilityBudgetData(t, p)
	path := report + "?legal_entity_id=" + p.DE.String() + "&fiscal_year=2026&page_size=100"
	var times []time.Duration
	for range 10 {
		start := time.Now()
		r, _ := p.Must(200, "GET", path, "", "")
		times = append(times, time.Since(start))
		// Each document bills 87.50 less 4.38 of a key and 237.53 of none;
		// each credit memo 1.00 of the key.
		if got, want := totalsOf(t, r), "32503325.03 - 438004.38 - 5001.00 = 32060319.65"; got != want {
			t.Fatalf("totals: got %s, want %s", got, want)
		}
		if got := len(rows(t, r)); got != 100 {
			t.Fatalf("rows: got %d, want a page of 100", got)
		}
	}
	slices.Sort(times)
	p95 := times[len(times)*95/100]
	t.Logf("profitability report over 100,000 billing documents (21,000 segments), a page of 100: median %v, p95 %v", times[len(times)/2], p95)
	if budget := nfr.Budget(3 * time.Second); p95 > budget {
		t.Fatalf("p95 %v exceeds the %v budget (3 s scaled by ERP_BUDGET_SCALE)", p95, budget)
	}
}

// profitabilityBudgetData copies a posted billing document and a posted
// credit memo of Northwind, made through the API, as the API would have
// made them: orders, items, billing documents and their items for 1,000
// customers, and credit memos. The rows are written in bulk without the
// tables' triggers (fixture.SalesVolume's way), as the documents' own
// operations would leave them. Each copy calls jsonb_populate_record once
// per row in a LATERAL: written as (jsonb_populate_record(…)).* in the
// select list, PostgreSQL calls it once per column, which took the
// seeding to four minutes, past ci-local's 240 s limit on a budget test.
func profitabilityBudgetData(t *testing.T, p profitable) {
	t.Helper()
	order := p.order(t, p.northwind, "EUR", p.bracket("7"), p.installed("237.53", "EUR"))
	doc := p.bill(t, order, "2026-03-10", true, "1 7", "2 1")
	p.credit(t, doc, "1.00", "2026-03-11")
	ctx := context.Background()
	admin := pgtest.Admin(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	ten, who := p.Tenant.ID, p.Tenant.Human.ID
	var request uuid.UUID
	pgtest.Scan(t, `SELECT id FROM sales.credit_memo_request WHERE tenant_id = $1`, []any{ten}, &request)
	exec(`SET session_replication_role = replica`)
	defer exec(`RESET session_replication_role`)
	exec(`INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by)
		SELECT $1, md5($1::uuid::text || 'c' || k)::uuid, 'BC' || lpad(k::text, 8, '0'), 'organization', 'Customer ' || k, 'DE', 'active', $2, $2
		FROM generate_series(0, 999) k`, ten, who)
	copyDocuments(exec, ten, order, doc)
	copyCredits(exec, ten, request)
	exec(`ANALYZE sales.sales_order, sales.sales_order_item, sales.billing_document, sales.billing_document_item, sales.credit_memo_request,
		sales.credit_memo_request_item, partner.business_partner`)
	t.Log("loaded 120,000 posted billing documents of 240,000 items and 5,000 posted credit memos")
}

// copyDocuments copies the order, its items, the billing document and its
// items 120,000 times: documents 1 to 100,000 post in 2026, 100,001 to
// 120,000 in 2025.
func copyDocuments(exec func(string, ...any), ten uuid.UUID, order, doc string) {
	const docs = `generate_series(1, 120000) n CROSS JOIN LATERAL (SELECT md5($1::uuid::text || 'o' || n)::uuid AS xo, md5($1::uuid::text || 'b' || n)::uuid AS xb,
		md5($1::uuid::text || 'c' || n % 1000)::uuid AS xc, CASE WHEN n <= 100000 THEN date '2026-01-01' ELSE date '2025-01-01' END + n % 360 AS xday) x`
	exec(`INSERT INTO sales.sales_order SELECT rec.*
		FROM sales.sales_order s, `+docs+`,
			LATERAL jsonb_populate_record(NULL::sales.sales_order, to_jsonb(s)
			|| jsonb_build_object('id', x.xo, 'document_number', 'B' || n, 'customer_id', x.xc)) rec
		WHERE s.tenant_id = $1 AND s.id = $2`, ten, order)
	exec(`INSERT INTO sales.sales_order_item SELECT rec.*
		FROM sales.sales_order_item i, `+docs+`,
			LATERAL jsonb_populate_record(NULL::sales.sales_order_item, to_jsonb(i)
			|| jsonb_build_object('sales_order_id', x.xo, 'item_key', CASE WHEN i.item_key IS NULL THEN NULL ELSE 'K' || n % 20 END)) rec
		WHERE i.tenant_id = $1 AND i.sales_order_id = $2`, ten, order)
	exec(`INSERT INTO sales.billing_document SELECT rec.*
		FROM sales.billing_document b, `+docs+`,
			LATERAL jsonb_populate_record(NULL::sales.billing_document, to_jsonb(b)
			|| jsonb_build_object('id', x.xb, 'document_number', 'B' || n, 'customer_id', x.xc, 'sales_order_id', x.xo, 'billing_date', x.xday,
				'posting_date', x.xday, 'customer_invoice_id', md5($1::uuid::text || 'i' || n)::uuid)) rec
		WHERE b.tenant_id = $1 AND b.id = $2`, ten, doc)
	exec(`INSERT INTO sales.billing_document_item SELECT rec.*
		FROM sales.billing_document_item i, `+docs+`,
			LATERAL jsonb_populate_record(NULL::sales.billing_document_item, to_jsonb(i)
			|| jsonb_build_object('billing_document_id', x.xb, 'sales_order_id', x.xo)) rec
		WHERE i.tenant_id = $1 AND i.billing_document_id = $2`, ten, doc)
}

// copyCredits copies the credit memo request and its item on the first
// 5,000 documents, posted, of 1.00 each.
func copyCredits(exec func(string, ...any), ten, request uuid.UUID) {
	const credits = `generate_series(1, 5000) n CROSS JOIN LATERAL (SELECT md5($1::uuid::text || 'r' || n)::uuid AS xr, md5($1::uuid::text || 'b' || n)::uuid AS xb,
		md5($1::uuid::text || 'o' || n)::uuid AS xo, md5($1::uuid::text || 'c' || n % 1000)::uuid AS xc, date '2026-01-01' + n % 360 AS xday) x`
	exec(`INSERT INTO sales.credit_memo_request SELECT rec.*
		FROM sales.credit_memo_request r, `+credits+`,
			LATERAL jsonb_populate_record(NULL::sales.credit_memo_request, to_jsonb(r)
			|| jsonb_build_object('id', x.xr, 'document_number', 'R' || n, 'customer_id', x.xc, 'billing_document_id', x.xb, 'sales_order_id', x.xo,
				'posting_date', x.xday, 'customer_invoice_id', md5($1::uuid::text || 'm' || n)::uuid, 'customer_invoice_number', 'R' || n,
				'journal_entry_id', md5($1::uuid::text || 'j' || n)::uuid)) rec
		WHERE r.tenant_id = $1 AND r.id = $2`, ten, request)
	exec(`INSERT INTO sales.credit_memo_request_item SELECT rec.*
		FROM sales.credit_memo_request_item i, `+credits+`,
			LATERAL jsonb_populate_record(NULL::sales.credit_memo_request_item, to_jsonb(i)
			|| jsonb_build_object('request_id', x.xr, 'billing_document_id', x.xb)) rec
		WHERE i.tenant_id = $1 AND i.request_id = $2`, ten, request)
}
