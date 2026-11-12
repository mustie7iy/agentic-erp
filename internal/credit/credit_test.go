package credit_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// perms are what the tests' person holds: reading and setting limits.
var perms = []string{"credit.limit.read", "credit.limit.write"}

// env is a configured tenant whose company 2000 (EUR) has the customer
// reconciliation account 120000. north is a customer with customer data
// in companies 2000 and 1000 (USD), east in company 2000; south is a
// customer without customer data anywhere; vendor is a business partner
// that is no customer. A US dollar is 0.80 EUR.
type env struct {
	fixture.Env
	receivable, revenue        uuid.UUID // accounts 120000 and 400000 of company 2000
	north, south, east, vendor uuid.UUID
}

func receivables(doc *config.Config) {
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "120000", Name: "Trade receivables", Type: config.Asset, CustomerReconciliation: true})
}

func setup(t *testing.T) env {
	t.Helper()
	f := fixture.NewWith(t, nil, receivables, perms...)
	e := env{Env: f, receivable: f.Account(t, "2000", "120000"), revenue: f.Account(t, "2000", "400000")}
	// Numbered against their names' order, so the limits' order by number is seen.
	e.north = e.partner(t, "BP70000002", "Northwind Traders", "customer")
	e.south = e.partner(t, "BP70000001", "Southwind Ltd", "customer")
	e.east = e.partner(t, "BP70000003", "Eastwind AG", "customer")
	e.vendor = e.partner(t, "BP70000004", "Westwind Supplies", "supplier")
	e.companyData(t, e.north, e.DE)
	e.companyData(t, e.north, e.US)
	e.companyData(t, e.east, e.DE)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-01-01', 0.8, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	return e
}

// partner inserts an active business partner with the role.
func (e env) partner(t *testing.T, number, name, role string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by)
		VALUES ($1, $2, $3, 'organization', $4, 'DE', 'active', $5, $5)`, e.Tenant.ID, id, number, name, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO partner.partner_role (tenant_id, partner_id, role, created_by) VALUES ($1, $2, $3, $4)`, e.Tenant.ID, id, role, e.Tenant.Human.ID)
	return id
}

// companyData sets the customer up in the company on the receivables
// account 120000 with NT30, as an approved partner change would (T74a).
func (e env) companyData(t *testing.T, customer, company uuid.UUID) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO partner.customer_company (tenant_id, partner_id, legal_entity_id, reconciliation_account_id, payment_term_code, created_by, updated_by)
		VALUES ($1, $2, $3, $4, 'NT30', $5, $5)`, e.Tenant.ID, customer, company, e.receivable, e.Tenant.Human.ID)
}

// item is a customer item to post in company 2000: the amount in minor
// units of the currency, debit positive.
type item struct {
	customer uuid.UUID
	amount   money.Minor
	currency string
	ref      string
}

// post posts the items as receivables' postings would, each on the
// receivables account 120000 with its customer against revenue.
func (e env) post(t *testing.T, items ...item) {
	t.Helper()
	post := ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}}
	day := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		for _, it := range items {
			in := ledger.Posting{LegalEntityID: e.DE, DocumentType: "SA", DocumentDate: day, PostingDate: day, Currency: it.currency,
				Reference: &it.ref, SourceContext: "receivables", SourceDocumentID: uuid.Must(uuid.NewV7()),
				Lines: []ledger.PostingLine{{AccountID: e.receivable, Amount: it.amount, PartnerID: &it.customer}, {AccountID: e.revenue, Amount: -it.amount}}}
			if _, err := post.Post(ctx, in); err != nil {
				return fmt.Errorf("post %s: %w", it.ref, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// limitOf is the JSON of a credit limit of the customer in the company.
func limitOf(company, customer uuid.UUID, amount, currency string) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "limit": {"amount": %q, "currency": %q}}`, company, customer, amount, currency)
}

// change is the JSON of a change of a limit's amount.
func change(amount, currency string) string {
	return fmt.Sprintf(`{"limit": {"amount": %q, "currency": %q}}`, amount, currency)
}

func amount(t *testing.T, m map[string]any, field string) string {
	t.Helper()
	a := apitest.Object(t, m, field)
	return fmt.Sprintf("%v %v", a["amount"], a["currency"])
}

// objects are the objects of a list field.
func objects(t *testing.T, m map[string]any, field string) []map[string]any {
	t.Helper()
	raw := apitest.List(t, m, field)
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		o, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("%s[%d]: got %v, want an object", field, i, r)
		}
		out[i] = o
	}
	return out
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
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

// audited are the audit records of a limit as "action detail", in the
// order they were written.
func (e env) audited(t *testing.T, id string) []string {
	t.Helper()
	var out []string
	pgtest.Scan(t, `SELECT coalesce(array_agg(action || ' ' || detail::text ORDER BY seq), '{}') FROM audit.audit_event
		WHERE tenant_id = $1 AND object_type = 'credit.credit_limit' AND object_id = $2`, []any{e.Tenant.ID, id}, &out)
	return out
}

// changed are the tenant's CreditLimitChanged events in the order they
// were written, each naming its company by code.
func (e env) changed(t *testing.T) []map[string]any {
	t.Helper()
	return apitest.CompanyNamed(t, apitest.Published(t, e.Tenant, outbox.Event{Context: "credit", Name: "credit_limit_changed"}))
}

// TestSetAndRead: a limit is set for a customer in a company, in the
// company's currency, and reads back with the names of what it
// references and its amount with its currency; the audit record and
// CreditLimitChanged carry the limit and no limit before, the event the
// customer by id and number, never by name (T77a acceptance).
func TestSetAndRead(t *testing.T) {
	e := setup(t)
	req := apitest.Req{Method: "POST", Path: "/v1/credit-limits", Body: limitOf(e.DE, e.north, "500.00", "EUR"), Tenant: e.Tenant.ID, As: e.Tenant.Human,
		Key: apitest.NewKey()}
	rec := e.API.Do(req)
	if rec.Code != 201 {
		t.Fatalf("status: got %d, want 201: %s", rec.Code, rec.Body)
	}
	got := apitest.JSON(t, rec)
	id := apitest.String(t, got, "id")
	if tag := rec.Header().Get("ETag"); tag != `"1"` {
		t.Errorf("ETag: got %s, want \"1\"", tag)
	}
	if loc := rec.Header().Get("Location"); loc != "/v1/credit-limits/"+id {
		t.Errorf("Location: got %s, want /v1/credit-limits/%s", loc, id)
	}
	for f, want := range map[string]any{"legal_entity_id": e.DE.String(), "company_code": "2000", "company_name": "Acme GmbH",
		"customer_id": e.north.String(), "customer_number": "BP70000002", "customer_name": "Northwind Traders", "version": float64(1)} {
		if got[f] != want {
			t.Errorf("%s: got %v, want %v", f, got[f], want)
		}
	}
	if a := amount(t, got, "limit"); a != "500.00 EUR" {
		t.Errorf("limit: got %s, want 500.00 EUR", a)
	}
	read, tag := e.Must(200, "GET", "/v1/credit-limits/"+id, "", "")
	if a, b := mustJSON(t, read)+" "+tag, mustJSON(t, got)+` "1"`; a != b {
		t.Errorf("read back: got %s, want %s", a, b)
	}
	t.Run("the same request again", func(t *testing.T) {
		again := e.API.Do(req)
		if got := fmt.Sprintf("%d %s", again.Code, again.Header().Get("Idempotent-Replayed")); got != "201 true" {
			t.Fatalf("replay: got %s, want 201 true: %s", got, again.Body)
		}
		if got := apitest.JSON(t, again)["id"]; got != id {
			t.Errorf("replay: got limit %v, want the first answer's %s", got, id)
		}
	})
	wantAudit := fmt.Sprintf(`credit.credit_limit.create {"limit": {"amount": "500.00", "currency": "EUR"}, "customer_id": %q, "limit_before": null, `+
		`"customer_number": "BP70000002", "legal_entity_id": %q}`, e.north, e.DE)
	if a := e.audited(t, id); len(a) != 1 || a[0] != wantAudit {
		t.Errorf("audit records: got %q, want one: %s", a, wantAudit)
	}
	events := e.changed(t)
	if len(events) != 1 {
		t.Fatalf("CreditLimitChanged events: got %d, want 1", len(events))
	}
	want := map[string]any{"credit_limit_id": id, "legal_entity_id": e.DE.String(), "legal_entity_code": "2000", "customer_id": e.north.String(),
		"customer_number": "BP70000002", "limit": map[string]any{"amount": "500.00", "currency": "EUR"}, "limit_before": nil, "version": float64(1)}
	if a, b := mustJSON(t, events[0]), mustJSON(t, want); a != b {
		t.Errorf("event payload: got %s, want %s", a, b)
	}
	apitest.NeverPublished(t, e.Tenant, "credit", "Northwind")
}

// TestChange: a limit's amount is changed with the caller's version of
// it; the audit record and the event carry the amounts before and after;
// a stale version changes nothing; the limit's currency stays, and a
// change to the amount it has is refused (T77a acceptance).
func TestChange(t *testing.T) {
	e := setup(t)
	set, tag := e.Must(201, "POST", "/v1/credit-limits", limitOf(e.DE, e.north, "500.00", "EUR"), "")
	id := apitest.String(t, set, "id")
	path := "/v1/credit-limits/" + id
	got, next := e.Must(200, "PATCH", path, change("750.00", "EUR"), tag)
	if a := fmt.Sprintf("%s, ETag %s, version %v of %v", amount(t, got, "limit"), next, got["version"], got["customer_name"]); a !=
		`750.00 EUR, ETag "2", version 2 of Northwind Traders` {
		t.Errorf("changed: got %s; want 750.00 EUR, ETag \"2\", version 2 of Northwind Traders", a)
	}
	for _, c := range []struct {
		name      string
		status    int
		code      string
		body, tag string
		field     any
	}{
		{"with the version before the change", 412, "request.version_mismatch", change("900.00", "EUR"), tag, nil},
		{"without a version", 428, "request.precondition_required", change("900.00", "EUR"), "", nil},
		{"to the amount it has", 422, "credit.nothing_to_change", change("750.00", "EUR"), next, nil},
		{"in another currency", 422, "credit.currency_mismatch", change("900.00", "USD"), next, "limit.currency"},
		{"to less than nothing", 422, "credit.invalid_amount", change("-0.01", "EUR"), next, "limit.amount"},
		{"with the wrong decimals", 400, "request.invalid_amount", change("900", "EUR"), next, "limit.amount"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := e.In(t).Problem(c.status, c.code, "PATCH", path, c.body, c.tag)
			if f := field(p); f != c.field {
				t.Errorf("field: got %v, want %v", f, c.field)
			}
		})
	}
	t.Run("a limit that does not exist", func(t *testing.T) {
		e.In(t).Problem(404, "request.not_found", "PATCH", "/v1/credit-limits/"+uuid.NewString(), change("900.00", "EUR"), next)
	})
	t.Run("by a person of another company", func(t *testing.T) {
		other := e.As(e.Reader(t, "A person of company 1000", e.US))
		other.In(t).Problem(404, "request.not_found", "PATCH", path, change("900.00", "EUR"), next)
	})
	t.Run("to nothing on credit", func(t *testing.T) {
		zero, _ := e.In(t).Must(200, "PATCH", path, change("0.00", "EUR"), next)
		if a := fmt.Sprintf("%s at version %v", amount(t, zero, "limit"), zero["version"]); a != "0.00 EUR at version 3" {
			t.Errorf("got %s, want 0.00 EUR at version 3", a)
		}
	})
	audits := e.audited(t, id)
	if len(audits) != 3 {
		t.Fatalf("audit records: got %q, want 3: the limit set and changed twice; the refused changes record nothing", audits)
	}
	wantAudit := fmt.Sprintf(`credit.credit_limit.change {"limit": {"amount": "750.00", "currency": "EUR"}, "customer_id": %q, `+
		`"limit_before": {"amount": "500.00", "currency": "EUR"}, "customer_number": "BP70000002", "legal_entity_id": %q}`, e.north, e.DE)
	if audits[1] != wantAudit {
		t.Errorf("the first change's audit record: got %s, want %s", audits[1], wantAudit)
	}
	events := e.changed(t)
	if len(events) != 3 {
		t.Fatalf("CreditLimitChanged events: got %d, want 3: the refused changes publish nothing", len(events))
	}
	for i, want := range []string{"750.00 500.00 2", "0.00 750.00 3"} {
		ev := events[i+1]
		before, _ := ev["limit_before"].(map[string]any)
		if got := fmt.Sprintf("%v %v %v", apitest.Object(t, ev, "limit")["amount"], before["amount"], ev["version"]); got != want {
			t.Errorf("event %d: got %s, want %s (limit, limit before, version)", i+2, got, want)
		}
	}
}

// TestSetRefusals: a limit needs a company the caller may act in, a
// customer with customer data there, and an amount of zero or more in
// the company's currency; a customer has one limit per company.
func TestSetRefusals(t *testing.T) {
	e := setup(t)
	other := e.As(e.Reader(t, "A person of company 1000", e.US))
	for _, c := range []struct {
		name   string
		as     apitest.Client
		status int
		code   string
		body   string
		field  any
	}{
		{"an unknown company", e.Client, 422, "request.reference_not_found", limitOf(uuid.New(), e.north, "500.00", "EUR"), "legal_entity_id"},
		{"a company the caller may not act in", other, 422, "request.reference_not_found", limitOf(e.DE, e.north, "500.00", "EUR"), "legal_entity_id"},
		{"an unknown business partner", e.Client, 422, "request.reference_not_found", limitOf(e.DE, uuid.New(), "500.00", "EUR"), "customer_id"},
		{"a business partner that is no customer", e.Client, 422, "credit.customer_not_set_up", limitOf(e.DE, e.vendor, "500.00", "EUR"), "customer_id"},
		{"a customer without customer data", e.Client, 422, "credit.customer_not_set_up", limitOf(e.DE, e.south, "500.00", "EUR"), "customer_id"},
		{"a customer with customer data in another company", e.Client, 422, "credit.customer_not_set_up", limitOf(e.US, e.east, "500.00", "USD"), "customer_id"},
		{"another currency than the company's", e.Client, 422, "credit.currency_mismatch", limitOf(e.DE, e.north, "500.00", "USD"), "limit.currency"},
		{"less than nothing", e.Client, 422, "credit.invalid_amount", limitOf(e.DE, e.north, "-1.00", "EUR"), "limit.amount"},
		{"the wrong decimals", e.Client, 400, "request.invalid_amount", limitOf(e.DE, e.north, "500.0", "EUR"), "limit.amount"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := c.as.In(t).Problem(c.status, c.code, "POST", "/v1/credit-limits", c.body, "")
			if f := field(p); f != c.field {
				t.Errorf("field: got %v, want %v", f, c.field)
			}
		})
	}
	t.Run("a customer's second limit in a company", func(t *testing.T) {
		e.In(t).Must(201, "POST", "/v1/credit-limits", limitOf(e.DE, e.north, "500.00", "EUR"), "")
		e.In(t).Problem(409, "credit.limit_exists", "POST", "/v1/credit-limits", limitOf(e.DE, e.north, "900.00", "EUR"), "")
		e.In(t).Must(201, "POST", "/v1/credit-limits", limitOf(e.US, e.north, "900.00", "USD"), "")
	})
	var limits, events int
	pgtest.Scan(t, `SELECT (SELECT count(*) FROM credit.credit_limit WHERE tenant_id = $1), (SELECT count(*) FROM credit.outbox WHERE tenant_id = $1)`,
		[]any{e.Tenant.ID}, &limits, &events)
	if got := fmt.Sprintf("%d limits, %d events", limits, events); got != "2 limits, 2 events" {
		t.Errorf("stored: got %s, want 2 limits, 2 events: a refusal stores and publishes nothing", got)
	}
	t.Run("without the permission", func(t *testing.T) {
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'credit.limit.write'`, e.Tenant.ID)
		e.In(t).Problem(403, "identity.permission_missing", "POST", "/v1/credit-limits", limitOf(e.DE, e.east, "500.00", "EUR"), "")
	})
	t.Run("as an agent", func(t *testing.T) {
		agent := e.As(e.Tenant.Agent)
		agent.In(t).Problem(403, "identity.agent_not_allowed", "POST", "/v1/credit-limits", limitOf(e.DE, e.east, "500.00", "EUR"), "")
		agent.In(t).Problem(403, "identity.agent_not_allowed", "PATCH", "/v1/credit-limits/"+uuid.NewString(), change("1.00", "EUR"), `"1"`)
	})
}

// TestList: limits are listed by customer number, filtered by company
// and customer, and paged; a person sees the limits of the companies
// they may act in, and reads no other's.
func TestList(t *testing.T) {
	e := setup(t)
	ids := map[string]string{}
	for name, body := range map[string]string{"north in 2000": limitOf(e.DE, e.north, "500.00", "EUR"), "east in 2000": limitOf(e.DE, e.east, "100.00", "EUR"),
		"north in 1000": limitOf(e.US, e.north, "900.00", "USD")} {
		got, _ := e.Must(201, "POST", "/v1/credit-limits", body, "")
		ids[name] = apitest.String(t, got, "id")
	}
	list := func(t *testing.T, as apitest.Client, query string) (string, map[string]any) {
		t.Helper()
		page, _ := as.In(t).Must(200, "GET", "/v1/credit-limits"+query, "", "")
		var out []string
		for _, l := range objects(t, page, "items") {
			out = append(out, fmt.Sprintf("%v/%v %v %s", l["customer_number"], l["company_code"], l["customer_name"], amount(t, l, "limit")))
		}
		return strings.Join(out, "; "), page
	}
	const north2000, north1000, east2000 = "BP70000002/2000 Northwind Traders 500.00 EUR", "BP70000002/1000 Northwind Traders 900.00 USD",
		"BP70000003/2000 Eastwind AG 100.00 EUR"
	// Northwind's two limits have one customer number: they are listed in the order of their ids.
	norths := []string{north2000, north1000}
	if ids["north in 1000"] < ids["north in 2000"] {
		norths = []string{north1000, north2000}
	}
	other := e.As(e.Reader(t, "A person of company 1000", e.US))
	for _, c := range []struct {
		name  string
		as    apitest.Client
		query string
		want  []string
	}{
		{"of company 2000, by customer number", e.Client, "?legal_entity_id=" + e.DE.String(), []string{north2000, east2000}},
		{"the highest number first", e.Client, "?order_by=customer_number%20desc&legal_entity_id=" + e.DE.String(), []string{east2000, north2000}},
		{"of company 1000", e.Client, "?legal_entity_id=" + e.US.String(), []string{north1000}},
		{"of a customer in a company", e.Client, "?customer_id=" + e.east.String() + "&legal_entity_id=" + e.DE.String(), []string{east2000}},
		{"of a customer without a limit", e.Client, "?customer_id=" + e.south.String(), nil},
		{"every limit", e.Client, "", append(slices.Clone(norths), east2000)},
		{"of a customer in every company", e.Client, "?customer_id=" + e.north.String(), norths},
		{"by a person of company 1000", other, "", []string{north1000}},
		{"by that person, of company 2000", other, "?legal_entity_id=" + e.DE.String(), nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, _ := list(t, c.as, c.query); got != strings.Join(c.want, "; ") {
				t.Errorf("limits: got %q, want %q", got, strings.Join(c.want, "; "))
			}
		})
	}
	t.Run("of a customer, paged", func(t *testing.T) {
		query := "?customer_id=" + e.north.String() + "&page_size=1"
		first, page := list(t, e.Client, query)
		second, last := list(t, e.Client, query+"&page_token="+apitest.String(t, page, "next_page_token"))
		if got := []string{first, second}; !slices.Equal(got, norths) {
			t.Errorf("pages: got %q, want %q, one a page", got, norths)
		}
		if last["next_page_token"] != nil {
			t.Errorf("next page after the second: got %v, want none", last["next_page_token"])
		}
	})
	t.Run("another company's limit read", func(t *testing.T) {
		other.In(t).Problem(404, "request.not_found", "GET", "/v1/credit-limits/"+ids["north in 2000"], "", "")
		other.In(t).Must(200, "GET", "/v1/credit-limits/"+ids["north in 1000"], "", "")
	})
	t.Run("a limit that does not exist", func(t *testing.T) {
		e.In(t).Problem(404, "request.not_found", "GET", "/v1/credit-limits/"+uuid.NewString(), "", "")
	})
}
