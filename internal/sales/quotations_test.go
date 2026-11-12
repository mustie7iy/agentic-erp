package sales_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
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
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"sales.quotation.read", "sales.quotation.write", "sales.order.read", "sales.order.write", "sales.order.confirm",
	"sales.delivery.confirm", "sales.billing.read", "sales.billing.write"}

// far is a validity no run of the tests reaches.
const far = "2099-12-31"

// env is a configured tenant whose company 2000 (EUR, dated in Toronto's
// time zone) has the output tax codes A1 (19%) and AJ (levying its lines'
// subdivision) on account 230000 beside the fixture's input code V1, the
// customer reconciliation account 120000, and a customer set up in the
// company on it with the payment term NT30.
type env struct {
	fixture.Env
	revenue, other uuid.UUID // accounts 400000 and 530000 of company 2000
	customer       uuid.UUID
}

// outputCodes gives company 2000 its output tax account and codes and
// Toronto's time zone.
func outputCodes(doc *config.Config) {
	de := doc.Company("2000")
	de.TimeZone = "America/Toronto"
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "230000", Name: "Output tax", Type: config.Liability},
		config.Account{Number: "120000", Name: "Trade receivables", Type: config.Asset, CustomerReconciliation: true})
	output := func(code string, onLine bool) config.TaxCode {
		c := config.TaxComponent{Key: "vat", AccountKey: config.KeyOutput, Account: "230000",
			Rates: []config.TaxRate{{RatePercent: "19", ValidFrom: "2026-01-01"}}}
		if onLine {
			c.Jurisdiction, c.JurisdictionOnLine = "DE", true
		}
		return config.TaxCode{Code: code, Description: "Output tax " + code, Direction: config.TaxOutput, Components: []config.TaxComponent{c}}
	}
	de.TaxCodes = append(de.TaxCodes, output("A1", false), output("AJ", true))
}

func setup(t *testing.T) env {
	t.Helper()
	f := fixture.NewWith(t, nil, outputCodes, perms...)
	e := env{Env: f, revenue: f.Account(t, "2000", "400000"), other: f.Account(t, "2000", "530000")}
	e.customer = e.partner(t, "BP70000001", "Northwind Traders", "active", "customer")
	e.companyData(t, e.customer, e.DE, "NT30")
	return e
}

// companyData sets the customer up in the company on the receivables
// account 120000 of company 2000 with the payment term, as an approved
// partner change would (T74a).
func (e env) companyData(t *testing.T, customer, company uuid.UUID, term string) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO partner.customer_company (tenant_id, partner_id, legal_entity_id, reconciliation_account_id, payment_term_code, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $6)`, e.Tenant.ID, customer, company, e.Account(t, "2000", "120000"), term, e.Tenant.Human.ID)
}

// partner inserts a business partner in the status with the roles.
func (e env) partner(t *testing.T, number, name, status string, roles ...string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by)
		VALUES ($1, $2, $3, 'organization', $4, 'DE', $5, $6, $6)`, e.Tenant.ID, id, number, name, status, e.Tenant.Human.ID)
	for _, r := range roles {
		pgtest.Exec(t, `INSERT INTO partner.partner_role (tenant_id, partner_id, role, created_by) VALUES ($1, $2, $3, $4)`, e.Tenant.ID, id, r, e.Tenant.Human.ID)
	}
	return id
}

// line is an item as a test names it; the zero value's fields take the
// defaults of item.
type line struct {
	quantity, unit, price, currency, code, key string
	priceUnit                                  int
	account                                    uuid.UUID
}

// item is the JSON of an item: by default 1 EA at the price in EUR on the
// revenue account, without a tax code.
func (e env) item(l line) string {
	def := func(v *string, d string) {
		if *v == "" {
			*v = d
		}
	}
	def(&l.quantity, "1")
	def(&l.unit, "EA")
	def(&l.price, "10.00")
	def(&l.currency, "EUR")
	if l.account == uuid.Nil {
		l.account = e.revenue
	}
	extra := ""
	if l.code != "" {
		extra += fmt.Sprintf(`, "tax_code": %q`, l.code)
	}
	if l.key != "" {
		extra += fmt.Sprintf(`, "item_key": %q`, l.key)
	}
	if l.priceUnit != 0 {
		extra += fmt.Sprintf(`, "price_unit": %d`, l.priceUnit)
	}
	return fmt.Sprintf(`{"description": "Machined bracket", "quantity": {"value": %q, "unit": %q}, "unit_price": {"amount": %q, "currency": %q}, "account_id": %q%s}`,
		l.quantity, l.unit, l.price, l.currency, l.account, extra)
}

// body is the JSON of a quotation of company 2000 in EUR to the customer,
// valid until the day.
func (e env) body(customer uuid.UUID, validUntil string, items ...string) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "valid_until": %q, "currency": "EUR", "items": [%s]}`,
		e.DE, customer, validUntil, strings.Join(items, ", "))
}

// quoted creates a quotation of one item of 10.00 EUR and returns its id.
func (e env) quoted(t *testing.T) string {
	t.Helper()
	got, _ := e.Must(201, "POST", "/v1/quotations", e.body(e.customer, far, e.item(line{})), "")
	return got["id"].(string)
}

func amount(t *testing.T, m map[string]any, field string) string {
	t.Helper()
	a, ok := m[field].(map[string]any)
	if !ok {
		t.Fatalf("%s: got %v, want an amount", field, m[field])
	}
	return fmt.Sprintf("%v %v", a["amount"], a["currency"])
}

func items(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	raw, _ := m["items"].([]any)
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

// refusal checks that err is the problem with the status and code.
func refusal(t *testing.T, err error, status int, code string) {
	t.Helper()
	p := problem.From(err)
	if p.Code != code {
		t.Errorf("problem code: got %s (%v), want %s", p.Code, err, code)
	}
	if p.Status != status {
		t.Errorf("status: got %d, want %d", p.Status, status)
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

func (e env) inRequest(t *testing.T, fn func(ctx context.Context) error) error {
	t.Helper()
	return apitest.InRequest(t, e.Tenant, e.Tenant.Human, fn)
}

// endpoints are the context's endpoints as the API wires them, with the
// clock at now.
func endpoints(t *testing.T, now time.Time) *sales.Endpoints {
	t.Helper()
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return app.Sales(app.Config{Pages: pages, Now: func() time.Time { return now }})
}

func instant(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// audits counts the audit records of an action on an object.
func (e env) audits(t *testing.T, action, id string) int {
	t.Helper()
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = $2 AND object_id = $3`, []any{e.Tenant.ID, action, id}, &n)
	return n
}

// events are the payloads of the document's events of the name, in the
// order they were written, each conforming to the event contract.
func (e env) events(t *testing.T, name, key, id string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, p := range apitest.Published(t, e.Tenant, outbox.Event{Context: "sales", Name: name}) {
		if p[key] == id {
			out = append(out, p)
		}
	}
	return out
}

// TestQuotationCreatedAndReadBack: a quotation is created open with its
// number, each item's value quantity × unit price ÷ price unit rounded
// half away from zero and a net value that is their sum exactly; it reads
// back the same, with the names of what it references; nothing is posted;
// the audit record and QuotationChanged are written (T78a acceptance).
func TestQuotationCreatedAndReadBack(t *testing.T) {
	e := setup(t)
	// 2.5 × 10.01 is 25.025: 25.03; 3 × 99.99 per 100 is 2.9997: 3.00;
	// 0.001 × 4.99 is 0.00499: refused alone, so it is not here.
	body := e.body(e.customer, far, e.item(line{quantity: "2.5", unit: "KG", price: "10.01", code: "A1", key: "BRACKET-M8"}),
		e.item(line{quantity: "3", price: "99.99", priceUnit: 100}), e.item(line{quantity: "120.000000", price: "12.50", account: e.other}))
	got, tag := e.Must(201, "POST", "/v1/quotations", body, "")
	if tag != `"1"` {
		t.Errorf("ETag: got %s, want \"1\"", tag)
	}
	for f, want := range map[string]any{"status": "open", "company_code": "2000", "company_name": "Acme GmbH", "customer_number": "BP70000001",
		"customer_name": "Northwind Traders", "valid_until": far, "currency": "EUR", "customer_id": e.customer.String(),
		"legal_entity_id": e.DE.String()} {
		if got[f] != want {
			t.Errorf("%s: got %v, want %v", f, got[f], want)
		}
	}
	if n, _ := got["document_number"].(string); !strings.HasPrefix(n, "QT") {
		t.Errorf("document number: got %v, want one of the quotation range (QT…)", got["document_number"])
	}
	if a := amount(t, got, "net_amount"); a != "1528.03 EUR" {
		t.Errorf("net value: got %s, want 1528.03 EUR", a)
	}
	its := items(t, got)
	if len(its) != 3 {
		t.Fatalf("items: got %d, want 3", len(its))
	}
	for i, want := range []struct {
		quantity, price, net, account string
		priceUnit                     float64
		code, key                     any
	}{{"2.5 KG", "10.01 EUR", "25.03 EUR", "400000 Sales Revenue", 1, "A1", "BRACKET-M8"}, {"3 EA", "99.99 EUR", "3.00 EUR", "400000 Sales Revenue", 100, nil, nil},
		{"120 EA", "12.50 EUR", "1500.00 EUR", "530000", 1, nil, nil}} {
		t.Run(fmt.Sprintf("item %d", i+1), func(t *testing.T) {
			it := its[i]
			q, _ := it["quantity"].(map[string]any)
			if s := fmt.Sprintf("%v %v", q["value"], q["unit"]); s != want.quantity {
				t.Errorf("quantity: got %s, want %s", s, want.quantity)
			}
			if a := amount(t, it, "unit_price"); a != want.price {
				t.Errorf("unit price: got %s, want %s", a, want.price)
			}
			if a := amount(t, it, "net_amount"); a != want.net {
				t.Errorf("net value: got %s, want %s", a, want.net)
			}
			if s := fmt.Sprintf("%v %v", it["account_number"], it["account_name"]); !strings.HasPrefix(s, want.account) {
				t.Errorf("account: got %s, want %s", s, want.account)
			}
			fields(t, it, map[string]any{"item_no": float64(i + 1), "price_unit": want.priceUnit, "tax_code": want.code, "item_key": want.key})
		})
	}
	id, _ := got["id"].(string)
	read, readTag := e.Must(200, "GET", "/v1/quotations/"+id, "", "")
	if a, b := mustJSON(t, read), mustJSON(t, got); a != b {
		t.Errorf("read back: got %s, want %s", a, b)
	}
	if readTag != tag {
		t.Errorf("ETag read back: got %s, want %s", readTag, tag)
	}
	if n := e.audits(t, "sales.quotation.create", id); n != 1 {
		t.Errorf("audit records of the creation: got %d, want 1", n)
	}
	want := map[string]any{"quotation_id": id, "document_number": got["document_number"], "legal_entity_id": e.DE.String(),
		"legal_entity_code": "2000", "customer_id": e.customer.String(), "customer_number": "BP70000001", "quotation_date": got["quotation_date"],
		"valid_until": far, "net_amount": map[string]any{"amount": "1528.03", "currency": "EUR"}, "status": "open", "status_before": nil,
		"version": float64(1)}
	if a, b := mustJSON(t, e.events(t, "quotation_changed", "quotation_id", id)), mustJSON(t, []map[string]any{want}); a != b {
		t.Errorf("QuotationChanged: got %s, want %s", a, b)
	}
	apitest.NeverPublished(t, e.Tenant, "sales", "Northwind Traders")
	var entries int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1`, []any{e.Tenant.ID}, &entries)
	if entries != 0 {
		t.Errorf("journal entries: got %d, want none (nothing is posted)", entries)
	}
}

// TestQuotationIdempotent: the same Idempotency-Key creates one quotation
// and answers it again.
func TestQuotationIdempotent(t *testing.T) {
	e := setup(t)
	req := apitest.Req{Method: "POST", Path: "/v1/quotations", Body: e.body(e.customer, far, e.item(line{})),
		Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()}
	first, second := e.API.Do(req), e.API.Do(req)
	for i, rec := range []*httptest.ResponseRecorder{first, second} {
		if rec.Code != 201 {
			t.Fatalf("status of call %d: got %d, want 201: %s", i+1, rec.Code, rec.Body)
		}
	}
	a, b := apitest.JSON(t, first), apitest.JSON(t, second)
	fields(t, b, map[string]any{"id": a["id"], "document_number": a["document_number"]})
	if h := second.Header().Get("Idempotent-Replayed"); h != "true" {
		t.Errorf("Idempotent-Replayed: got %q, want true", h)
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM sales.quotation WHERE tenant_id = $1`, []any{e.Tenant.ID}, &n)
	if n != 1 {
		t.Errorf("quotations: got %d, want 1", n)
	}
}

// TestQuotationRefusals: every refusal of a quotation names the request's
// own field, and nothing is written.
func TestQuotationRefusals(t *testing.T) {
	e := setup(t)
	supplier := e.partner(t, "BP70000002", "Only a supplier", "active", "supplier")
	ok := e.item(line{})
	// Near the range's end: 90,000,000,000,000,000.00 is in range, two of them are not.
	huge := line{quantity: "9", price: "10000000000000000.00"}
	cases := []struct {
		name, body string
		status     int
		code       string
		field      any
	}{
		{"an unknown customer", e.body(uuid.New(), far, ok), 422, "request.reference_not_found", "customer_id"},
		{"a partner that is no customer", e.body(supplier, far, ok), 422, "sales.customer_not_usable", "customer_id"},
		{"an unknown company", strings.Replace(e.body(e.customer, far, ok), e.DE.String(), uuid.NewString(), 1), 422, "request.reference_not_found", "legal_entity_id"},
		{"an unknown currency", strings.ReplaceAll(e.body(e.customer, far, ok), "EUR", "XXA"), 422, "sales.unknown_currency", "currency"},
		{"valid until a day gone by", e.body(e.customer, "2020-01-01", ok), 422, "sales.invalid_validity", "valid_until"},
		{"a description of white space", strings.Replace(e.body(e.customer, far, ok), "Machined bracket", "   ", 1), 422, "request.validation_failed", "items[0].description"},
		{"an item key of white space", e.body(e.customer, far, ok, e.item(line{key: "  "})), 422, "request.validation_failed", "items[1].item_key"},
		{"a quantity of zero", e.body(e.customer, far, e.item(line{quantity: "0"})), 422, "sales.invalid_quantity", "items[0].quantity.value"},
		{"an unknown unit", e.body(e.customer, far, ok, e.item(line{unit: "XX"})), 422, "sales.unknown_unit", "items[1].quantity.unit"},
		{"a price in another currency", e.body(e.customer, far, ok, e.item(line{currency: "USD"})), 422, "sales.currency_mismatch", "items[1].unit_price.currency"},
		{"a price with too many decimals", e.body(e.customer, far, e.item(line{price: "1.234"})), 400, "request.invalid_amount", "items[0].unit_price.amount"},
		{"a price of zero", e.body(e.customer, far, e.item(line{price: "0.00"})), 422, "sales.amount_not_positive", "items[0].unit_price.amount"},
		{"a negative price", e.body(e.customer, far, e.item(line{price: "-1.00"})), 422, "sales.amount_not_positive", "items[0].unit_price.amount"},
		{"a value that rounds to zero", e.body(e.customer, far, e.item(line{quantity: "0.001", price: "4.99"})), 422, "sales.amount_not_positive", "items[0].unit_price.amount"},
		{"an account of no company", e.body(e.customer, far, ok, e.item(line{account: uuid.New()})), 422, "sales.account_not_postable", "items[1].account_id"},
		{"an unknown tax code", e.body(e.customer, far, e.item(line{code: "ZZ"})), 422, "sales.unknown_tax_code", "items[0].tax_code"},
		{"an input tax code", e.body(e.customer, far, ok, e.item(line{code: "V1"})), 422, "sales.unknown_tax_code", "items[1].tax_code"},
		{"a code levying its lines' subdivision", e.body(e.customer, far, e.item(line{code: "AJ"})), 422, "sales.tax_jurisdiction_not_supported", "items[0].tax_code"},
		{"an item's value out of range", e.body(e.customer, far, e.item(line{quantity: "99999999999999", price: "10000000000000000.00"})), 422, "sales.amount_too_large", "items"},
		{"a net value out of range", e.body(e.customer, far, e.item(huge), e.item(huge)), 422, "sales.amount_too_large", "items"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e.In(t).Problem(c.status, c.code, "POST", "/v1/quotations", c.body, "")
			if f := field(got); f != c.field {
				t.Errorf("field: got %v, want %v", f, c.field)
			}
		})
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM sales.quotation WHERE tenant_id = $1`, []any{e.Tenant.ID}, &n)
	if n != 0 {
		t.Errorf("quotations after the refusals: got %d, want none", n)
	}
	t.Run("no items", func(t *testing.T) {
		// The contract refuses an empty list before the handler; the port refuses it too.
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := endpoints(t, time.Now()).MakeQuotation(ctx, sales.QuotationInput{LegalEntityID: e.DE, CustomerID: e.customer,
				ValidUntil: instant(t, far+"T00:00:00Z"), Currency: "EUR"})
			return err
		})
		refusal(t, err, 422, "sales.items_required")
	})
}

// TestQuotationAnswered: the customer's answer is recorded once, at the
// version the caller read; a replay answers the same; the audit record
// and QuotationChanged are written (T78a acceptance).
func TestQuotationAnswered(t *testing.T) {
	e := setup(t)
	id := e.quoted(t)
	path := "/v1/quotations/" + id
	e.Problem(428, "request.precondition_required", "POST", path+":accept", "", "")
	e.Problem(412, "request.version_mismatch", "POST", path+":accept", "", `"7"`)
	req := apitest.Req{Method: "POST", Path: path + ":accept", Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(), IfMatch: `"1"`}
	first, second := e.API.Do(req), e.API.Do(req)
	for i, rec := range []*httptest.ResponseRecorder{first, second} {
		if rec.Code != 200 {
			t.Fatalf("status of call %d: got %d, want 200: %s", i+1, rec.Code, rec.Body)
		}
	}
	a, b := apitest.JSON(t, first), apitest.JSON(t, second)
	fields(t, a, map[string]any{"status": "accepted", "version": float64(2)})
	if tag := first.Header().Get("ETag"); tag != `"2"` {
		t.Errorf("ETag: got %s, want \"2\"", tag)
	}
	if mustJSON(t, a) != mustJSON(t, b) {
		t.Errorf("replay: got %v, want the first answer %v", b, a)
	}
	if h := second.Header().Get("Idempotent-Replayed"); h != "true" {
		t.Errorf("Idempotent-Replayed: got %q, want true", h)
	}
	if n := e.audits(t, "sales.quotation.accept", id); n != 1 {
		t.Errorf("audit records of the acceptance: got %d, want 1", n)
	}
	events := e.events(t, "quotation_changed", "quotation_id", id)
	if len(events) != 2 {
		t.Fatalf("QuotationChanged events: got %d, want 2 (created, accepted)", len(events))
	}
	fields(t, events[1], map[string]any{"status": "accepted", "status_before": "open", "version": float64(2), "customer_number": "BP70000001"})
	t.Run("answered once", func(t *testing.T) {
		e.In(t).Problem(409, "sales.invalid_status", "POST", path+":accept", "", `"2"`)
		e.In(t).Problem(409, "sales.invalid_status", "POST", path+":decline", "", `"2"`)
	})
	t.Run("rejected", func(t *testing.T) {
		other := e.quoted(t)
		got, tag := e.In(t).Must(200, "POST", "/v1/quotations/"+other+":decline", "", `"1"`)
		if got["status"] != "rejected" {
			t.Errorf("status: got %v, want rejected", got["status"])
		}
		if tag != `"2"` {
			t.Errorf("ETag: got %s, want \"2\"", tag)
		}
		if n := e.audits(t, "sales.quotation.decline", other); n != 1 {
			t.Errorf("audit records of the rejection: got %d, want 1", n)
		}
		ev := e.events(t, "quotation_changed", "quotation_id", other)
		if len(ev) != 2 {
			t.Fatalf("the rejection's events: got %d, want 2 (created, rejected)", len(ev))
		}
		fields(t, ev[1], map[string]any{"status": "rejected", "status_before": "open"})
		e.In(t).Problem(409, "sales.invalid_status", "POST", "/v1/quotations/"+other+":accept", "", `"2"`)
	})
	t.Run("two answers at once", func(t *testing.T) {
		other := e.quoted(t)
		codes := make([]int, 2)
		var wg sync.WaitGroup
		for i, action := range []string{":accept", ":decline"} {
			wg.Go(func() {
				codes[i] = e.API.Do(apitest.Req{Method: "POST", Path: "/v1/quotations/" + other + action, Tenant: e.Tenant.ID, As: e.Tenant.Human,
					Key: apitest.NewKey(), IfMatch: `"1"`}).Code
			})
		}
		wg.Wait()
		// The second waits for the first's row lock and then finds the
		// quotation at another version.
		slices.Sort(codes)
		if !slices.Equal(codes, []int{200, 412}) {
			t.Errorf("statuses: got %v, want one 200 and one 412", codes)
		}
		if ev := e.events(t, "quotation_changed", "quotation_id", other); len(ev) != 2 {
			t.Errorf("events of two answers at once: got %d, want 2 (created, one answer)", len(ev))
		}
	})
	t.Run("an unknown quotation", func(t *testing.T) {
		e.In(t).Problem(404, "request.not_found", "POST", "/v1/quotations/"+uuid.NewString()+":accept", "", `"1"`)
	})
}

// TestQuotationDatesAreTheCompanys: a quotation is dated the company's
// date in its time zone, not UTC's, its validity starts that day, and it
// is not accepted once that date is past its validity; it may still be
// rejected (T78a acceptance).
func TestQuotationDatesAreTheCompanys(t *testing.T) {
	e := setup(t)
	// 03:00 UTC on 10 March is still 9 March in Toronto.
	h := endpoints(t, instant(t, "2026-03-10T03:00:00Z"))
	in := sales.QuotationInput{LegalEntityID: e.DE, CustomerID: e.customer, ValidUntil: instant(t, "2026-03-09T00:00:00Z"), Currency: "EUR",
		Items: []sales.ItemInput{{Description: "Bracket", Quantity: "1", Unit: "EA", UnitPrice: sales.Amount{Value: "10.00", Currency: "EUR"},
			PriceUnit: 1, AccountID: e.revenue}}}
	never := func(int32) error { return nil }
	err := e.inRequest(t, func(ctx context.Context) error {
		early := in
		early.ValidUntil = instant(t, "2026-03-08T00:00:00Z")
		if _, err := h.MakeQuotation(ctx, early); problem.From(err).Code != "sales.invalid_validity" {
			t.Errorf("valid until the day before the company's date: got %v, want sales.invalid_validity", err)
		}
		q, err := h.MakeQuotation(ctx, in)
		if err != nil {
			return err
		}
		if got := q.QuotationDate.Format(time.DateOnly); got != "2026-03-09" {
			t.Errorf("quotation date: got %s, want 2026-03-09, Toronto's date", got)
		}
		// 03:00 UTC on 11 March is 10 March in Toronto: a day past its validity.
		h.Now = func() time.Time { return instant(t, "2026-03-11T03:00:00Z") }
		_, err = h.MarkAccepted(ctx, q.ID, never)
		refusal(t, err, 409, "sales.quotation_expired")
		rejected, err := h.MarkRejected(ctx, q.ID, never)
		if err == nil && rejected.Status != sales.Rejected {
			t.Errorf("rejected past its validity: got status %s, want rejected", rejected.Status)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("accepted on its last day", func(t *testing.T) {
		// 03:00 UTC on 10 March is 9 March in Toronto, the last day; UTC's date is past it.
		err := e.inRequest(t, func(ctx context.Context) error {
			h := endpoints(t, instant(t, "2026-03-10T03:00:00Z"))
			q, err := h.MakeQuotation(ctx, in)
			if err != nil {
				return err
			}
			accepted, err := h.MarkAccepted(ctx, q.ID, never)
			if err == nil && accepted.Status != sales.Accepted {
				t.Errorf("got status %s, want accepted", accepted.Status)
			}
			return err
		})
		if err != nil {
			t.Errorf("got %v, want the quotation accepted", err)
		}
	})
}

// TestQuotationsListed: quotations are listed by customer, status,
// company and number, in pages, and only those of the companies the
// caller may act in (T78a acceptance).
func TestQuotationsListed(t *testing.T) {
	e := setup(t)
	other := e.partner(t, "BP70000003", "Contoso", "active", "customer")
	first, second := e.quoted(t), e.quoted(t)
	theirs, _ := e.Must(201, "POST", "/v1/quotations", e.body(other, far, e.item(line{})), "")
	us, _ := e.Must(201, "POST", "/v1/quotations", strings.Replace(strings.ReplaceAll(e.body(e.customer, far,
		e.item(line{account: e.Account(t, "1000", "400000")})), "EUR", "USD"), e.DE.String(), e.US.String(), 1), "")
	e.Must(200, "POST", "/v1/quotations/"+second+":accept", "", `"1"`)
	ids := func(t *testing.T, c apitest.Client, query string) string {
		t.Helper()
		got, _ := c.In(t).Must(200, "GET", "/v1/quotations?"+query, "", "")
		var out []string
		for _, it := range items(t, got) {
			out = append(out, it["id"].(string))
		}
		return strings.Join(out, " ")
	}
	number := func(id string) string {
		got, _ := e.Must(200, "GET", "/v1/quotations/"+id, "", "")
		return got["document_number"].(string)
	}
	for _, c := range []struct{ name, query, want string }{
		{"a customer's, by number", "customer_id=" + e.customer.String() + "&legal_entity_id=" + e.DE.String() + "&order_by=document_number", first + " " + second},
		{"a customer's open ones", "customer_id=" + e.customer.String() + "&status=open&legal_entity_id=" + e.DE.String(), first},
		{"the accepted", "status=accepted", second},
		{"another customer's", "customer_id=" + other.String(), theirs["id"].(string)},
		{"a company's", "legal_entity_id=" + e.US.String(), us["id"].(string)},
		{"by number", "document_number=" + number(second) + "&legal_entity_id=" + e.DE.String(), second},
		{"newest number first", "legal_entity_id=" + e.DE.String() + "&order_by=document_number%20desc&page_size=2", theirs["id"].(string) + " " + second},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := ids(t, e.Client, c.query); got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
	t.Run("in pages", func(t *testing.T) {
		query := "legal_entity_id=" + e.DE.String() + "&order_by=document_number&page_size=2"
		page, _ := e.In(t).Must(200, "GET", "/v1/quotations?"+query, "", "")
		token, _ := page["next_page_token"].(string)
		if token == "" {
			t.Fatal("first page: got no next_page_token, want one")
		}
		if got := ids(t, e.Client, query+"&page_token="+token); got != theirs["id"].(string) {
			t.Errorf("second page: got %s, want %s", got, theirs["id"])
		}
	})
	t.Run("in pages by date", func(t *testing.T) {
		// The three are dated the same day: the id breaks the tie, so the pages hold each once.
		query := "legal_entity_id=" + e.DE.String() + "&page_size=2"
		page, _ := e.In(t).Must(200, "GET", "/v1/quotations?"+query, "", "")
		token, _ := page["next_page_token"].(string)
		if token == "" {
			t.Fatal("first page: got no next_page_token, want one")
		}
		all := strings.Fields(ids(t, e.Client, query) + " " + ids(t, e.Client, query+"&page_token="+token))
		slices.Sort(all)
		want := []string{first, second, theirs["id"].(string)}
		slices.Sort(want)
		if !slices.Equal(all, want) {
			t.Errorf("the two pages: got %v, want %v", all, want)
		}
	})
	t.Run("only the caller's companies", func(t *testing.T) {
		reader := e.As(e.Reader(t, "US sales", e.US))
		if got := ids(t, reader, ""); got != us["id"].(string) {
			t.Errorf("listed: got %s, want %s alone", got, us["id"])
		}
		reader.In(t).Problem(404, "request.not_found", "GET", "/v1/quotations/"+first, "", "")
		reader.In(t).Problem(404, "request.not_found", "POST", "/v1/quotations/"+first+":decline", "", `"1"`)
		reader.In(t).Problem(422, "request.reference_not_found", "POST", "/v1/quotations", e.body(e.customer, far, e.item(line{})), "")
	})
	t.Run("without the permission", func(t *testing.T) {
		nobody := e.As(e.Reader(t, "No sales", e.DE))
		pgtest.Exec(t, `DELETE FROM identity.principal_role WHERE tenant_id = $1 AND principal_id = $2`, e.Tenant.ID, nobody.As.ID)
		nobody.In(t).Problem(403, "identity.permission_missing", "GET", "/v1/quotations", "", "")
		nobody.In(t).Problem(403, "identity.permission_missing", "POST", "/v1/quotations", e.body(e.customer, far, e.item(line{})), "")
	})
}
