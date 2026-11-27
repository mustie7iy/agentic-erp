package controlling_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

// salesPerms are what the profitability tests' people hold: selling,
// billing and crediting, recording prices, taking returns, and the
// report's read.
var salesPerms = []string{"sales.order.read", "sales.order.write", "sales.order.confirm", "sales.delivery.confirm", "sales.billing.read",
	"sales.billing.write", "sales.billing.post", "sales.price.write", "credit.limit.write", "sales.credit_memo_request.read",
	"sales.credit_memo_request.write", "sales.credit_memo_request.approve", "controlling.profitability.read", "sales.return.write", "sales.return.confirm"}

// profitable is a tenant whose company 2000 (EUR) sells to Northwind
// (BP70000001) and Contoso (BP70000002) on the customer reconciliation
// account 120000, with the pricing procedure PR00, K007: BRACKET-M8's
// general price is 12.50 EUR, and Northwind has five percent off it.
type profitable struct {
	fixture.Env
	revenue            uuid.UUID // 400000
	northwind, contoso uuid.UUID
}

func setupProfitable(t *testing.T) profitable {
	t.Helper()
	f := fixture.NewWith(t, nil, func(doc *config.Config) {
		de := doc.Company("2000")
		de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "120000", Name: "Trade receivables", Type: config.Asset, CustomerReconciliation: true})
		doc.Pricing.ConditionTypes = []config.ConditionType{{Code: "PR00", Name: "Price", Kind: config.ConditionPrice},
			{Code: "K007", Name: "Customer discount", Kind: config.ConditionDiscountPercent}}
		doc.Pricing.Procedure = []config.ProcedureStep{{ConditionType: "PR00"}, {ConditionType: "K007"}}
		for _, y := range fixture.Years() {
			doc.NumberRanges = append(doc.NumberRanges, config.NumberRange{Company: "2000", DocumentType: "journal_dr", FiscalYear: y, First: 100000, Last: 999999},
				config.NumberRange{Company: "2000", DocumentType: "journal_dg", FiscalYear: y, First: 100000, Last: 999999})
		}
	}, salesPerms...)
	p := profitable{Env: f, revenue: f.Account(t, "2000", "400000")}
	p.northwind, p.contoso = p.customer(t, "BP70000001", "Northwind Traders"), p.customer(t, "BP70000002", "Contoso Ltd")
	p.Must(201, "POST", "/v1/sales-prices", fmt.Sprintf(`{"legal_entity_id": %q, "item_key": "BRACKET-M8", "unit": "EA",
		"unit_price": {"amount": "12.50", "currency": "EUR"}, "valid_from": "2020-01-01"}`, p.DE), "")
	p.Must(201, "POST", "/v1/sales-prices", fmt.Sprintf(`{"legal_entity_id": %q, "item_key": "BRACKET-M8", "customer_id": %q,
		"condition_type": "K007", "percent": "5", "valid_from": "2020-01-01"}`, p.DE, p.northwind), "")
	return p
}

// customer inserts an active customer set up in company 2000.
func (p profitable) customer(t *testing.T, number, name string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by)
		VALUES ($1, $2, $3, 'organization', $4, 'DE', 'active', $5, $5)`, p.Tenant.ID, id, number, name, p.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO partner.partner_role (tenant_id, partner_id, role, created_by) VALUES ($1, $2, 'customer', $3)`, p.Tenant.ID, id, p.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO partner.customer_company (tenant_id, partner_id, legal_entity_id, reconciliation_account_id, payment_term_code, created_by, updated_by)
		VALUES ($1, $2, $3, $4, 'NT30', $5, $5)`, p.Tenant.ID, id, p.DE, p.Account(t, "2000", "120000"), p.Tenant.Human.ID)
	return id
}

// bracket is an order item of BRACKET-M8 priced from its records;
// installed one of no item key at an entered price, in EUR.
func (p profitable) bracket(quantity string) string {
	return fmt.Sprintf(`{"item_key": "BRACKET-M8", "description": "Machined bracket M8", "quantity": {"value": %q, "unit": "EA"}, "account_id": %q}`,
		quantity, p.revenue)
}

func (p profitable) installed(price, currency string) string {
	return fmt.Sprintf(`{"description": "Installation", "quantity": {"value": "1", "unit": "EA"}, "unit_price": {"amount": %q, "currency": %q}, "account_id": %q}`,
		price, currency, p.revenue)
}

// order creates and confirms an order of the customer in the currency
// and returns its id.
func (p profitable) order(t *testing.T, customer uuid.UUID, currency string, items ...string) string {
	t.Helper()
	o, _ := p.Must(201, "POST", "/v1/sales-orders", fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "currency": %q, "items": [%s]}`,
		p.DE, customer, currency, strings.Join(items, ", ")), "")
	id := o["id"].(string)
	p.Must(200, "POST", "/v1/sales-orders/"+id+":confirm", "", `"1"`)
	return id
}

// bill confirms "item quantity" of the order as delivered on the day,
// bills what is delivered and, with post, posts it on the day; it returns
// the billing document's id.
func (p profitable) bill(t *testing.T, order, day string, post bool, delivered ...string) string {
	t.Helper()
	items := make([]string, len(delivered))
	for i, d := range delivered {
		f := strings.Fields(d)
		items[i] = fmt.Sprintf(`{"item_no": %s, "quantity": {"value": %q, "unit": "EA"}}`, f[0], f[1])
	}
	p.Must(201, "POST", "/v1/sales-orders/"+order+"/deliveries", fmt.Sprintf(`{"delivery_date": %q, "items": [%s]}`, day, strings.Join(items, ", ")), "")
	doc, tag := p.Must(201, "POST", "/v1/billing-documents", fmt.Sprintf(`{"sales_order_id": %q, "billing_date": %q}`, order, day), "")
	if post {
		p.Must(200, "POST", "/v1/billing-documents/"+doc["id"].(string)+":post", fmt.Sprintf(`{"posting_date": %q}`, day), tag)
	}
	return doc["id"].(string)
}

// credit posts a credit memo of the amount in EUR on the billing
// document's first item on the day.
func (p profitable) credit(t *testing.T, doc, amount, day string) {
	t.Helper()
	r, _ := p.Must(201, "POST", "/v1/credit-memo-requests", fmt.Sprintf(`{"billing_document_id": %q, "reason": "Goodwill",
		"items": [{"billing_item_no": 1, "net_amount": {"amount": %q, "currency": "EUR"}}]}`, doc, amount), "")
	id := r["id"].(string)
	p.Must(200, "POST", "/v1/credit-memo-requests/"+id+":release", "", `"1"`)
	p.Must(200, "POST", "/v1/credit-memo-requests/"+id+":post", fmt.Sprintf(`{"posting_date": %q}`, day), `"2"`)
}

// setupSold sells in 2026: Northwind 7 brackets, 12.50 each less five
// percent (87.50 less 4.38: 83.12), billed 3 in March and 4 with an
// installation of 237.53 in May, and credited 10.00 of the second billing
// in June; Contoso 4 brackets at 12.50 (50.00) in May, and 2 more billed
// in May but not posted; and Northwind an installation in US dollars in
// May, which the report leaves out.
func setupSold(t *testing.T) profitable {
	t.Helper()
	p := setupProfitable(t)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-01-01', 0.8, 'ECB', $2)`, p.Tenant.ID, p.Tenant.Human.ID)
	northwind := p.order(t, p.northwind, "EUR", p.bracket("7"), p.installed("237.53", "EUR"))
	p.bill(t, northwind, "2026-03-10", true, "1 3")
	second := p.bill(t, northwind, "2026-05-20", true, "1 4", "2 1")
	p.credit(t, second, "10.00", "2026-06-15")
	p.bill(t, p.order(t, p.contoso, "EUR", p.bracket("4")), "2026-05-21", true, "1 4")
	p.bill(t, p.order(t, p.contoso, "EUR", p.bracket("2")), "2026-05-21", false, "1 2")
	p.bill(t, p.order(t, p.northwind, "USD", p.installed("100.00", "USD")), "2026-05-22", true, "1 1")
	return p
}

const report = "/v1/profitability-report"

// rows are the report's rows: "customer key: gross discounts credits =
// net", the key "-" for none.
func rows(t *testing.T, r map[string]any) []string {
	t.Helper()
	items, _ := r["items"].([]any)
	out := make([]string, len(items))
	for i, it := range items {
		m, _ := it.(map[string]any)
		key, ok := m["item_key"].(string)
		if !ok {
			key = "-"
		}
		out[i] = fmt.Sprintf("%v %s: %s", m["customer_number"], key, figures(t, m))
	}
	return out
}

// figures are a row's or the totals' amounts in EUR.
func figures(t *testing.T, m map[string]any) string {
	t.Helper()
	var f []string
	for _, k := range []string{"gross_value", "discounts", "credit_memos", "net_revenue"} {
		a, _ := m[k].(map[string]any)
		if a["currency"] != "EUR" {
			t.Errorf("%s's currency: got %v, want EUR", k, a["currency"])
		}
		f = append(f, fmt.Sprint(a["amount"]))
	}
	return fmt.Sprintf("%s - %s - %s = %s", f[0], f[1], f[2], f[3])
}

func totalsOf(t *testing.T, r map[string]any) string {
	t.Helper()
	m, _ := r["totals"].(map[string]any)
	return figures(t, m)
}

// TestProfitabilityReport: per customer and item key the gross value
// billed, the discounts, the credit memos and the net revenue, exact in
// minor units; an item billed in parts carries its discount in shares
// adding up to it; the totals are the net of the posted billing documents
// less the posted credit memos; a document not posted, or in another
// currency, is not counted (T92a; ADR-0075 §6).
func TestProfitabilityReport(t *testing.T) {
	p := setupSold(t)
	r, _ := p.Must(200, "GET", report+"?legal_entity_id="+p.DE.String()+"&fiscal_year=2026", "", "")
	want := []string{"BP70000001 -: 237.53 - 0.00 - 0.00 = 237.53", "BP70000001 BRACKET-M8: 87.50 - 4.38 - 10.00 = 73.12",
		"BP70000002 BRACKET-M8: 50.00 - 0.00 - 0.00 = 50.00"}
	if got := rows(t, r); strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Errorf("rows: got %v, want %v", got, want)
	}
	if got, want := totalsOf(t, r), "375.03 - 4.38 - 10.00 = 360.65"; got != want {
		t.Errorf("totals: got %s, want %s", got, want)
	}
	var billed, credited int64
	pgtest.Scan(t, `SELECT (SELECT sum(net_amount) FROM sales.billing_document WHERE tenant_id = $1 AND status = 'posted' AND currency = 'EUR'),
		(SELECT sum(i.net_amount) FROM sales.credit_memo_request_item i JOIN sales.credit_memo_request r ON r.tenant_id = i.tenant_id AND r.id = i.request_id
			WHERE r.tenant_id = $1 AND r.status = 'posted')`, []any{p.Tenant.ID}, &billed, &credited)
	if got, want := billed-credited, int64(36065); got != want {
		t.Errorf("the posted documents' net: got %d, want %d, the totals' net revenue", got, want)
	}
	selection := map[string]any{"legal_entity_id": p.DE.String(), "fiscal_year": float64(2026), "fiscal_period_from": float64(1), "fiscal_period_to": float64(12),
		"date_from": "2026-01-01", "date_to": "2026-12-31", "currency": "EUR", "includes_cost": true, "foreign_currency_documents": float64(1)}
	for k, v := range selection {
		if r[k] != v {
			t.Errorf("%s: got %v, want %v", k, r[k], v)
		}
	}
	if _, ok := r["next_page_token"]; ok {
		t.Errorf("next_page_token: got %v, want none on the only page", r["next_page_token"])
	}
	if _, ok := r["customer_id"]; ok {
		t.Errorf("customer_id: got %v, want none for every customer", r["customer_id"])
	}
	first, _ := r["items"].([]any)[0].(map[string]any)
	if first["customer_name"] != "Northwind Traders" || first["customer_id"] != p.northwind.String() {
		t.Errorf("the first row's customer: got %v %v, want Northwind Traders %s", first["customer_name"], first["customer_id"], p.northwind)
	}
}

// goodsIssue writes a goods issue's delivery of the order's first item,
// as the shipping context's goods issue leaves it (ADR-0083 §5): quantity
// EA issued on the day at a cost of minor units of the currency.
func (p profitable) goodsIssue(t *testing.T, order, day, quantity string, cost int64, currency string) {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO sales.delivery (tenant_id, id, sales_order_id, delivery_no, delivery_date, outbound_delivery_id, outbound_delivery_number,
			material_document_id, material_document_number, created_by)
		SELECT $1, $2, $3, 1, $4, gen_random_uuid(), 'OD1', gen_random_uuid(), 'MD1', $5 FROM (SELECT set_config('app.tenant_id', $1::uuid::text, true)) scoped`,
		p.Tenant.ID, id, order, day, p.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO sales.delivery_item (tenant_id, delivery_id, sales_order_id, order_item_no, quantity, cost_amount, cost_currency)
		SELECT $1, $2, $3, 1, $4::numeric, $5, $6 FROM (SELECT set_config('app.tenant_id', $1::uuid::text, true)) scoped`,
		p.Tenant.ID, id, order, quantity, cost, currency)
}

// TestProfitabilityCostOfSales: a segment's cost of sales is the cost of
// its order items' goods issues posted in the selection, its margin net
// revenue less it, and the totals add both up; a goods issue counts in
// the period of its own date, even before its billing, and one of an
// order in another currency is left out and counted (ADR-0083 §6, S2b2).
func TestProfitabilityCostOfSales(t *testing.T) {
	p := setupProfitable(t)
	shipped := p.order(t, p.northwind, "EUR", p.bracket("4"))
	p.goodsIssue(t, shipped, "2026-03-09", "4", 3000, "EUR")
	doc, tag := p.Must(201, "POST", "/v1/billing-documents", fmt.Sprintf(`{"sales_order_id": %q, "billing_date": "2026-03-10"}`, shipped), "")
	p.Must(200, "POST", "/v1/billing-documents/"+doc["id"].(string)+":post", `{"posting_date": "2026-03-10"}`, tag)
	other := p.order(t, p.contoso, "EUR", p.bracket("2"))
	p.goodsIssue(t, other, "2026-04-01", "2", 1500, "EUR")
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-01-01', 0.8, 'ECB', $2)`, p.Tenant.ID, p.Tenant.Human.ID)
	usd := p.order(t, p.northwind, "USD", p.installed("100.00", "USD"))
	p.goodsIssue(t, usd, "2026-03-09", "1", 900, "EUR")
	r, _ := p.Must(200, "GET", report+"?legal_entity_id="+p.DE.String()+"&fiscal_year=2026", "", "")
	costs := func(r map[string]any) string {
		var out []string
		for _, it := range r["items"].([]any) {
			m := it.(map[string]any)
			out = append(out, fmt.Sprintf("%v: net %v, cost %v, margin %v", m["customer_number"], m["net_revenue"].(map[string]any)["amount"],
				m["cost_of_sales"].(map[string]any)["amount"], m["margin"].(map[string]any)["amount"]))
		}
		tot := r["totals"].(map[string]any)
		return strings.Join(out, "; ") + fmt.Sprintf(" | totals: cost %v, margin %v", tot["cost_of_sales"].(map[string]any)["amount"],
			tot["margin"].(map[string]any)["amount"])
	}
	if got, want := costs(r), "BP70000001: net 47.50, cost 30.00, margin 17.50; BP70000002: net 0.00, cost 15.00, margin -15.00 | totals: cost 45.00, margin 2.50"; got != want {
		t.Errorf("the report: got %s, want %s", got, want)
	}
	if r["foreign_currency_documents"] != float64(1) {
		t.Errorf("foreign currency documents: got %v, want 1, the USD order's goods issue", r["foreign_currency_documents"])
	}
	march, _ := p.Must(200, "GET", report+"?legal_entity_id="+p.DE.String()+"&fiscal_year=2026&fiscal_period_from=3&fiscal_period_to=3", "", "")
	if got, want := costs(march), "BP70000001: net 47.50, cost 30.00, margin 17.50 | totals: cost 30.00, margin 17.50"; got != want {
		t.Errorf("March: got %s, want %s", got, want)
	}
}

// TestProfitabilityReportSelections: a range of periods counts each
// document in the period of its own posting date, and the billed share
// of an item's discount is the part's; one customer's segments alone.
func TestProfitabilityReportSelections(t *testing.T) {
	p := setupSold(t)
	base := report + "?legal_entity_id=" + p.DE.String() + "&fiscal_year=2026"
	for _, c := range []struct {
		name, query string
		rows        []string
		totals      string
	}{
		{"March: the first part of the brackets", "&fiscal_period_from=3&fiscal_period_to=3",
			[]string{"BP70000001 BRACKET-M8: 37.50 - 1.88 - 0.00 = 35.62"}, "37.50 - 1.88 - 0.00 = 35.62"},
		{"May: the second part, its discount the rest", "&fiscal_period_from=5&fiscal_period_to=5",
			[]string{"BP70000001 -: 237.53 - 0.00 - 0.00 = 237.53", "BP70000001 BRACKET-M8: 50.00 - 2.50 - 0.00 = 47.50", "BP70000002 BRACKET-M8: 50.00 - 0.00 - 0.00 = 50.00"},
			"337.53 - 2.50 - 0.00 = 335.03"},
		{"from June: the credit memo alone", "&fiscal_period_from=6", []string{"BP70000001 BRACKET-M8: 0.00 - 0.00 - 10.00 = -10.00"}, "0.00 - 0.00 - 10.00 = -10.00"},
		{"up to February: nothing", "&fiscal_period_to=2", []string{}, "0.00 - 0.00 - 0.00 = 0.00"},
		{"Contoso alone", "&customer_id=" + p.contoso.String(), []string{"BP70000002 BRACKET-M8: 50.00 - 0.00 - 0.00 = 50.00"}, "50.00 - 0.00 - 0.00 = 50.00"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, _ := p.Must(200, "GET", base+c.query, "", "")
			if got := rows(t, r); strings.Join(got, "; ") != strings.Join(c.rows, "; ") {
				t.Errorf("rows: got %v, want %v", got, c.rows)
			}
			if got := totalsOf(t, r); got != c.totals {
				t.Errorf("totals: got %s, want %s", got, c.totals)
			}
		})
	}
	r, _ := p.Must(200, "GET", base+"&customer_id="+p.contoso.String()+"&fiscal_period_from=5&fiscal_period_to=6", "", "")
	if r["customer_id"] != p.contoso.String() || r["date_from"] != "2026-05-01" || r["date_to"] != "2026-06-30" || r["foreign_currency_documents"] != float64(0) {
		t.Errorf("the selection: got %v %v %v %v, want Contoso, 2026-05-01 to 2026-06-30 and no foreign document",
			r["customer_id"], r["date_from"], r["date_to"], r["foreign_currency_documents"])
	}
}

// TestProfitabilityReportPages: a page at a time, by the customer's
// number and then the item key, the totals on every page.
func TestProfitabilityReportPages(t *testing.T) {
	p := setupSold(t)
	path := report + "?legal_entity_id=" + p.DE.String() + "&fiscal_year=2026&page_size=1"
	var got []string
	for page := path; ; {
		r, _ := p.Must(200, "GET", page, "", "")
		if rs := rows(t, r); len(rs) != 1 {
			t.Fatalf("a page's rows: got %v, want one", rs)
		}
		got = append(got, rows(t, r)...)
		if tot := totalsOf(t, r); tot != "375.03 - 4.38 - 10.00 = 360.65" {
			t.Errorf("totals on page %d: got %s, want the selection's", len(got), tot)
		}
		next, ok := r["next_page_token"].(string)
		if !ok {
			break
		}
		page = path + "&page_token=" + next
	}
	want := "BP70000001 -: 237.53 - 0.00 - 0.00 = 237.53; BP70000001 BRACKET-M8: 87.50 - 4.38 - 10.00 = 73.12; BP70000002 BRACKET-M8: 50.00 - 0.00 - 0.00 = 50.00"
	if strings.Join(got, "; ") != want {
		t.Errorf("rows over the pages: got %v, want %s", got, want)
	}
	p.Problem(400, "request.invalid_page_token", "GET", report+"?legal_entity_id="+p.DE.String()+"&fiscal_year=2025&page_size=1&page_token=x", "", "")
}

// TestProfitabilityReportRefusals: periods in the wrong order or no
// regular period of the year, a legal entity the caller cannot act in, a
// caller without the permissions.
func TestProfitabilityReportRefusals(t *testing.T) {
	p := setupProfitable(t)
	base := report + "?legal_entity_id=" + p.DE.String() + "&fiscal_year=2026"
	for _, c := range []struct {
		name, query, code, field string
		status                   int
	}{
		{"to before from", "&fiscal_period_from=5&fiscal_period_to=4", "request.invalid_parameter", "fiscal_period_to", 400},
		{"from a special period", "&fiscal_period_from=13", "controlling.period_not_regular", "fiscal_period_from", 422},
		{"to a special period", "&fiscal_period_to=14", "controlling.period_not_regular", "fiscal_period_to", 422},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := p.Problem(c.status, c.code, "GET", base+c.query, "", "")
			if f := fieldErrors(got); len(f) != 1 || !strings.HasPrefix(f[0], c.field+" ") {
				t.Errorf("field errors: got %v, want one on %s", f, c.field)
			}
		})
	}
	t.Run("a year not set up", func(t *testing.T) {
		p.Problem(422, "ledger.no_fiscal_period", "GET", report+"?legal_entity_id="+p.DE.String()+"&fiscal_year=1999", "", "")
	})
	t.Run("a legal entity not there", func(t *testing.T) {
		got := p.Problem(422, "request.reference_not_found", "GET", report+"?legal_entity_id="+uuid.NewString()+"&fiscal_year=2026", "", "")
		if f := fieldErrors(got); len(f) != 1 || !strings.HasPrefix(f[0], "legal_entity_id ") {
			t.Errorf("field errors: got %v, want one on legal_entity_id", f)
		}
		p.As(p.Reader(t, "Reader of 1000", p.US)).Problem(422, "request.reference_not_found", "GET", base, "", "")
	})
	t.Run("without the billing read", func(t *testing.T) {
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'sales.billing.read'`, p.Tenant.ID)
		p.Problem(403, "identity.permission_missing", "GET", base, "", "")
	})
}

// TestProfitabilityReportFails: a sum beyond what an amount holds fails
// the read, never shown wrapped; a page size the contract refuses first,
// sent by a direct call, is refused.
func TestProfitabilityReportFails(t *testing.T) {
	p := setupProfitable(t)
	h := endpoints(t)
	h.Sales, h.Partners = big{}, partner.Payees{}
	read := func(params gen.GetProfitabilityReportParams) error {
		return apitest.InRequest(t, p.Tenant, p.Tenant.Human, func(ctx context.Context) error {
			_, err := h.GetProfitabilityReport(ctx, params)
			return err
		})
	}
	if err := read(gen.GetProfitabilityReportParams{LegalEntityID: p.DE, FiscalYear: 2026}); err == nil || problem.From(err).Status != 500 {
		t.Errorf("totals beyond an amount: got %v, want an internal error", err)
	}
	err := read(gen.GetProfitabilityReportParams{LegalEntityID: p.DE, FiscalYear: 2026, PageSize: gen.NewOptInt32(0)})
	want(t, "page size 0", problem.From(err).Code, "request.invalid_page_size")
}

// big answers two segments whose gross values add up beyond an amount.
type big struct{}

func (big) Segments(context.Context, sales.SegmentQuery) ([]sales.Segment, int, error) {
	return []sales.Segment{{Gross: 1 << 62, Net: 1}, {Gross: 1 << 62, Net: 1}}, 0, nil
}

// TestProfitabilityDatabaseFailures: every statement of the report and
// of the sales context's segments failing in turn fails the read.
func TestProfitabilityDatabaseFailures(t *testing.T) {
	p := setupSold(t)
	h := endpoints(t)
	h.Sales, h.Partners = sales.Orders{}, partner.Payees{}
	calls := map[string]func(ctx context.Context) error{
		"the report, page by page": func(ctx context.Context) error {
			page := gen.GetProfitabilityReportParams{LegalEntityID: p.DE, FiscalYear: 2026, PageSize: gen.NewOptInt32(1)}
			for {
				res, err := h.GetProfitabilityReport(ctx, page)
				if err != nil {
					return err
				}
				next, more := res.(*gen.ProfitabilityReport).NextPageToken.Get()
				if !more {
					return nil
				}
				page.PageToken = gen.NewOptString(next)
			}
		},
		"the sales context's segments": func(ctx context.Context) error {
			_, _, err := sales.Orders{}.Segments(ctx, sales.SegmentQuery{LegalEntityID: p.DE, Currency: "EUR"})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
			apitest.EveryFailure(t, p.Tenant, p.Tenant.Human, name, call)
		})
	}
}
