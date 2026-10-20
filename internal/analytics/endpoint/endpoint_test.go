package endpoint_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"ledger.entry.post", "ledger.entry.read", "ledger.account.read", "payables.invoice.read", "procurement.order.read",
	"partner.supplier.read", "tenancy.org.read", "analytics.query.run"}

// seeded is a tenant with revenue in both companies.
func seeded(t *testing.T, permissions ...string) fixture.Env {
	t.Helper()
	e := fixture.New(t, permissions...)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2025-01-01', 0.9, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	for _, c := range []struct {
		le           uuid.UUID
		company, cur string
		date, amount string
	}{{e.DE, "2000", "EUR", "2026-03-20", "500.00"}, {e.US, "1000", "USD", "2026-02-11", "800.00"}} {
		e.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": %q, "posting_date": %q, "currency": %q,
			"lines": [{"account_id": %q, "amount": {"amount": %q, "currency": %q}}, {"account_id": %q, "amount": {"amount": "-%s", "currency": %q}}]}`,
			c.le, c.date, c.date, c.cur, e.Account(t, c.company, "100000"), c.amount, c.cur, e.Account(t, c.company, "400000"), c.amount, c.cur), "")
	}
	return e
}

// rows are a result's rows as their cells' values.
func rows(res map[string]any) [][]string {
	var out [][]string
	for _, r := range res["rows"].([]any) {
		var row []string
		for _, c := range r.([]any) {
			cell := c.(map[string]any)
			row = append(row, fmt.Sprint(cell["value"], " ", cell["currency"]))
		}
		out = append(out, row)
	}
	return out
}

// TestQuery: a query is answered from the database without a store, over
// the companies the caller may read and asked for, with its lineage; one
// the semantic layer refuses is 422.
func TestQuery(t *testing.T) {
	e := seeded(t, perms...)
	res, _ := e.Must(200, "POST", "/v1/analytics-queries", `{"measures": ["revenue"], "by": ["company"], "order_by": "company"}`, "")
	if got := fmt.Sprint(rows(res)); got != "[[1000 <nil> 800.00 USD] [2000 <nil> 500.00 EUR]]" {
		t.Errorf("rows: got %s, want each company's revenue", got)
	}
	if res["source"] != "database" || res["as_of"] == "" || res["data_through"] != "2026-03-20" {
		t.Errorf("lineage: got %v as of %v through %v, want the database's", res["source"], res["as_of"], res["data_through"])
	}
	res, _ = e.Must(200, "POST", "/v1/analytics-queries", `{"measures": ["revenue"], "by": ["company"], "companies": ["2000", "9999"]}`, "")
	if got := fmt.Sprint(rows(res)); got != "[[2000 <nil> 500.00 EUR]]" {
		t.Errorf("rows of 2000: got %s, want its revenue alone", got)
	}
	// Filters, a ledger, derived metrics, counts and ratios.
	var leading string
	pgtest.Scan(t, `SELECT id::text FROM ledger.ledger WHERE tenant_id = $1 AND is_leading`, []any{e.Tenant.ID}, &leading)
	res, _ = e.Must(200, "POST", "/v1/analytics-queries", fmt.Sprintf(`{"measures": ["operating_profit", "operating_margin", "invoice_count"],
		"by": ["company"], "where": [{"dim": "fiscal_year", "op": "in", "values": ["2026"]}], "ledger": %q, "companies": ["2000"]}`, leading), "")
	if got := fmt.Sprint(rows(res)); got != "[[2000 <nil> 500.00 EUR 1.0000 <nil> <nil> <nil>]]" {
		t.Errorf("rows: got %s, want 2000's profit, margin and no invoices", got)
	}
	if defs := res["definitions"].([]any); len(defs) < 3 {
		t.Errorf("definitions: got %v, want the derived metrics' and their bases'", defs)
	}
	for name, c := range map[string]struct{ body, want string }{
		"unknown metric":            {`{"measures": ["nope"]}`, "analytics.invalid_query"},
		"amounts in two currencies": {`{"measures": ["revenue"]}`, "analytics.invalid_query"},
	} {
		t.Run(name, func(t *testing.T) {
			if code, body, _ := e.Do("POST", "/v1/analytics-queries", c.body, ""); code != 422 || body["code"] != c.want {
				t.Errorf("got %d %v, want 422 %s", code, body, c.want)
			}
		})
	}
}

// TestCompanyAccess: an agent sees the companies it was given access to,
// and a company access naming no legal entity adds none.
func TestCompanyAccess(t *testing.T) {
	e := seeded(t, perms...)
	agent := e.As(e.Tenant.Agent)
	res, _ := agent.Must(200, "POST", "/v1/analytics-queries", `{"measures": ["revenue"], "by": ["company"]}`, "")
	if got := rows(res); len(got) != 0 {
		t.Errorf("rows without access: got %v, want none", got)
	}
	for _, le := range []uuid.UUID{e.DE, uuid.New()} {
		pgtest.Exec(t, `INSERT INTO identity.company_access (tenant_id, id, principal_id, legal_entity_id, valid_from, created_by)
			VALUES ($1, gen_random_uuid(), $2, $3, current_date - 1, $4)`, e.Tenant.ID, e.Tenant.Agent.ID, le, e.Tenant.Human.ID)
	}
	res, _ = agent.Must(200, "POST", "/v1/analytics-queries", `{"measures": ["revenue"], "by": ["company"]}`, "")
	if got := fmt.Sprint(rows(res)); got != "[[2000 <nil> 500.00 EUR]]" {
		t.Errorf("rows of the agent: got %s, want 2000's alone", got)
	}
}

// TestPermissions: a query reads only the fact types its metrics need, so
// a permission another needs does not refuse it; one it needs and the
// caller lacks refuses it, as the API's read would.
func TestPermissions(t *testing.T) {
	e := seeded(t, "ledger.entry.post", "ledger.entry.read", "ledger.account.read", "partner.supplier.read", "tenancy.org.read", "analytics.query.run")
	res, _ := e.Must(200, "POST", "/v1/analytics-queries", `{"measures": ["revenue"], "by": ["company"]}`, "")
	if got := len(rows(res)); got != 2 {
		t.Errorf("revenue rows without order permissions: got %d, want 2", got)
	}
	code, body, _ := e.Do("POST", "/v1/analytics-queries", `{"measures": ["purchase_order_value"]}`, "")
	if code != 403 || body["code"] != "identity.permission_missing" {
		t.Errorf("order value without procurement.order.read: got %d %v, want 403 identity.permission_missing", code, body)
	}
	// Orders alone: the journal is not read.
	orders := seeded(t, "ledger.entry.post", "ledger.account.read", "partner.supplier.read", "tenancy.org.read", "procurement.order.read", "analytics.query.run")
	orders.Must(200, "POST", "/v1/analytics-queries", `{"measures": ["purchase_order_value"]}`, "")
	// A read every path needs, refused by the API, refuses the query.
	blind := seeded(t, "ledger.entry.post", "ledger.entry.read", "ledger.account.read", "partner.supplier.read", "analytics.query.run")
	if code, body, _ := blind.Do("POST", "/v1/analytics-queries", `{"measures": ["revenue"]}`, ""); code != 403 {
		t.Errorf("without tenancy.org.read: got %d %v, want the API's 403", code, body)
	}
}
