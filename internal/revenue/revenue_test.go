package revenue_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/revenue"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"sales.order.read", "sales.order.write", "sales.order.confirm", "sales.delivery.confirm", "revenue.contract.read", "revenue.contract.write",
	"revenue.contract.activate"}

const contracts = "/v1/revenue-contracts"

// env is a configured tenant whose company 2000 (EUR) has the customer
// reconciliation account 120000 and a customer set up on it with the
// payment term NT30, so sales orders can be created and confirmed there.
type env struct {
	fixture.Env
	account  uuid.UUID // revenue account 400000 of company 2000
	customer uuid.UUID
}

func setup(t *testing.T) env {
	t.Helper()
	f := fixture.NewWith(t, nil, func(doc *config.Config) {
		de := doc.Company("2000")
		de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "120000", Name: "Trade receivables", Type: config.Asset, CustomerReconciliation: true})
	}, perms...)
	e := env{Env: f, account: f.Account(t, "2000", "400000"), customer: uuid.Must(uuid.NewV7())}
	pgtest.Exec(t, `INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by)
		VALUES ($1, $2, 'BP70000001', 'organization', 'Northwind Traders', 'DE', 'active', $3, $3)`, e.Tenant.ID, e.customer, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO partner.partner_role (tenant_id, partner_id, role, created_by) VALUES ($1, $2, 'customer', $3)`,
		e.Tenant.ID, e.customer, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO partner.customer_company (tenant_id, partner_id, legal_entity_id, reconciliation_account_id, payment_term_code, created_by, updated_by)
		VALUES ($1, $2, $3, $4, 'NT30', $5, $5)`, e.Tenant.ID, e.customer, e.DE, e.Account(t, "2000", "120000"), e.Tenant.Human.ID)
	return e
}

// item is the JSON of an order item: the quantity in EA at the price in
// the currency on the revenue account; key is its item key, empty for
// none.
func (e env) item(description, key, quantity, price, currency string) string {
	extra := ""
	if key != "" {
		extra = fmt.Sprintf(`, "item_key": %q`, key)
	}
	return fmt.Sprintf(`{"description": %q, "quantity": {"value": %q, "unit": "EA"}, "unit_price": {"amount": %q, "currency": %q}, "account_id": %q%s}`,
		description, quantity, price, currency, e.account, extra)
}

// goods and support are the two items most tests order: 120 brackets at
// 12.50 (1500.00) and a year's support at 1200.00, in EUR.
func (e env) goods() string {
	return e.item("Machined bracket M8", "BRACKET-M8", "120", "12.50", "EUR")
}
func (e env) support() string {
	return e.item("Support for the fiscal year", "", "1", "1200.00", "EUR")
}

// draft creates a sales order of company 2000 in the currency with the
// items and returns it.
func (e env) draft(t *testing.T, currency string, items ...string) map[string]any {
	t.Helper()
	got, _ := e.Must(201, "POST", "/v1/sales-orders", fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "currency": %q, "items": [%s]}`,
		e.DE, e.customer, currency, strings.Join(items, ", ")), "")
	return got
}

// confirmed creates and confirms a sales order in EUR with the items and
// returns its id and document number.
func (e env) confirmed(t *testing.T, items ...string) (id, number string) {
	t.Helper()
	o := e.draft(t, "EUR", items...)
	id = o["id"].(string)
	e.Must(200, "POST", "/v1/sales-orders/"+id+":confirm", "", `"1"`)
	return id, o["document_number"].(string)
}

// foreign creates and confirms a sales order in US dollars and returns
// its id. The confirmation values the order in the company's currency
// for the customer's credit (T77b), so it needs the day's average rate:
// 0.80 EUR for a US dollar from 2026, recorded here.
func (e env) foreign(t *testing.T) string {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-01-01', 0.8, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	id := e.draft(t, "USD", e.item("Machined bracket M8", "", "1", "10.00", "USD"))["id"].(string)
	e.Must(200, "POST", "/v1/sales-orders/"+id+":confirm", "", `"1"`)
	return id
}

func forOrder(id string) string { return fmt.Sprintf(`{"sales_order_id": %q}`, id) }

func amount(t *testing.T, m map[string]any, field string) string {
	t.Helper()
	a, ok := m[field].(map[string]any)
	if !ok {
		t.Fatalf("%s: got %v, want an amount", field, m[field])
	}
	return fmt.Sprintf("%v %v", a["amount"], a["currency"])
}

func obligations(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	raw, _ := m["obligations"].([]any)
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		out[i], _ = r.(map[string]any)
	}
	return out
}

// field is the field the problem's first field error names.
func field(m map[string]any) any {
	errs, _ := m["errors"].([]any)
	if len(errs) == 0 {
		return nil
	}
	e, _ := errs[0].(map[string]any)
	return e["field"]
}

// fields checks each named field of m, one message per field.
func fields(t *testing.T, m, want map[string]any) {
	t.Helper()
	for f, w := range want {
		if m[f] != w {
			t.Errorf("%s: got %v, want %v", f, m[f], w)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// endpoints are the context's endpoints as the API wires them.
func endpoints(t *testing.T) *revenue.Endpoints {
	t.Helper()
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return app.Revenue(app.Config{Pages: pages, Now: time.Now})
}

// count is the number the query gives for the tenant, whose id is $1.
func (e env) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	pgtest.Scan(t, query, append([]any{e.Tenant.ID}, args...), &n)
	return n
}

// contractsOf counts the contracts of the sales order.
func (e env) contractsOf(t *testing.T, order string) int {
	t.Helper()
	return e.count(t, `SELECT count(*) FROM revenue.contract WHERE tenant_id = $1 AND sales_order_id = $2`, order)
}

// audits counts the creations of the contract in the audit trail made by
// the source: a person's request or the order's confirmation.
func (e env) audits(t *testing.T, id, source string) int {
	t.Helper()
	return e.count(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'revenue.contract.create' AND object_id = $2
		AND detail->>'source' = $3`, id, source)
}

// events are the payloads of RevenueContractChanged for the sales order,
// in the order they were written, each conforming to the event contract.
func (e env) events(t *testing.T, order string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, p := range apitest.Published(t, e.Tenant, outbox.Event{Context: "revenue", Name: "revenue_contract_changed"}) {
		if p["sales_order_id"] == order {
			out = append(out, p)
		}
	}
	return out
}

// contractOf is the one contract of the sales order, as the list finds
// it.
func (e env) contractOf(t *testing.T, order string) map[string]any {
	t.Helper()
	got, _ := e.Must(200, "GET", contracts+"?sales_order_id="+order, "", "")
	items := apitest.List(t, got, "items")
	if len(items) != 1 {
		t.Fatalf("contracts of sales order %s: got %d, want 1", order, len(items))
	}
	c, _ := items[0].(map[string]any)
	return c
}

// inRequest runs fn in a unit of work as the tenant's person.
func (e env) inRequest(t *testing.T, fn func(ctx context.Context) error) error {
	t.Helper()
	return apitest.InRequest(t, e.Tenant, e.Tenant.Human, fn)
}
