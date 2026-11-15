package watchers_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"ledger.entry.post", "ledger.entry.read", "ledger.account.read", "payables.invoice.read", "procurement.order.read",
	"partner.supplier.read", "tenancy.org.read", "analytics.query.run", "analytics.watcher.read", "analytics.watcher.write"}

// today is the evaluations' date: the fixture dates its documents in
// 2026, and a watcher's query reads from two fiscal years before today's.
var today = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)

// env is a tenant with revenue in 2000 (EUR) in January, February and
// March 2026 and in 1000 (USD) in February, whose API dates evaluations
// today.
type env struct{ fixture.Env }

func setup(t *testing.T, permissions ...string) env {
	t.Helper()
	e := env{fixture.New(t, append(permissions, perms...)...)}
	e.API = apitest.NewWith(t, func(c *app.Config) { c.Now = func() time.Time { return today } })
	// A company's journal is kept in its currency; the group's rate is needed to post USD.
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2025-01-01', 0.9, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	for _, r := range []struct {
		le                 uuid.UUID
		company, cur, date string
		amount             string
	}{
		{e.DE, "2000", "EUR", "2026-01-15", "100.00"}, {e.DE, "2000", "EUR", "2026-02-15", "200.00"},
		{e.DE, "2000", "EUR", "2026-03-15", "500.00"}, {e.US, "1000", "USD", "2026-02-11", "800.00"},
	} {
		e.revenue(t, r.le, r.company, r.cur, r.date, r.amount)
	}
	return e
}

// revenue posts revenue of the amount in the company on the date.
func (e env) revenue(t *testing.T, le uuid.UUID, company, cur, date, amount string) {
	t.Helper()
	e.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": %q, "posting_date": %q, "currency": %q,
		"lines": [{"account_id": %q, "amount": {"amount": %q, "currency": %q}}, {"account_id": %q, "amount": {"amount": "-%s", "currency": %q}}]}`,
		le, date, date, cur, e.Account(t, company, "100000"), amount, cur, e.Account(t, company, "400000"), amount, cur), "")
}

// watch sets a watcher as the client and returns it.
func watch(t *testing.T, c apitest.Client, body string) map[string]any {
	t.Helper()
	w, _ := c.Must(201, "POST", "/v1/metric-watchers", body, "")
	return w
}

// evaluate evaluates the watcher at its version and returns the
// evaluation.
func evaluate(t *testing.T, c apitest.Client, w map[string]any) map[string]any {
	t.Helper()
	ev, _ := c.Must(200, "POST", "/v1/metric-watchers/"+apitest.String(t, w, "id")+":evaluate", "", etag(w))
	return ev
}

func etag(w map[string]any) string { return fmt.Sprintf(`"%v"`, w["version"]) }

// series is an evaluation's series as "company currency figure
// compared percent holds", "-" for what is absent.
func series(ev map[string]any) []string {
	var out []string
	for _, s := range ev["series"].([]any) {
		m := s.(map[string]any)
		out = append(out, fmt.Sprintf("%s %s %s %s %s %v", m["company"], or(m["currency"]), m["figure"], or(m["compared_with"]), or(m["percent"]), m["holds"]))
	}
	return out
}

// findings is an alert's findings as series writes them, without holds.
func findings(alert map[string]any) []string {
	var out []string
	for _, f := range alert["findings"].([]any) {
		m := f.(map[string]any)
		out = append(out, fmt.Sprintf("%s %s %s %s %s", m["company"], or(m["currency"]), m["figure"], m["compared_with"], or(m["percent"])))
	}
	return out
}

func or(v any) any {
	if v == nil {
		return "-"
	}
	return v
}

// alerts counts the watcher's alerts and their MetricAlertRaised events.
func (e env) alerts(t *testing.T, w map[string]any) (alerts, events int) {
	t.Helper()
	pgtest.Scan(t, `SELECT (SELECT count(*) FROM analytics.metric_alert WHERE tenant_id = $1 AND watcher_id = $2),
		(SELECT count(*) FROM analytics.outbox WHERE tenant_id = $1 AND event_type = 'com.example.erp.analytics.metric_alert_raised.v1' AND payload->>'watcher_id' = $2::text)`,
		[]any{e.Tenant.ID, apitest.String(t, w, "id")}, &alerts, &events)
	return alerts, events
}

// TestAbove: a watcher on revenue above a value judges the latest period
// with data, March, in each series with a figure there, and raises one
// alert naming the figure and the value; evaluated again, it raises none
// and answers the same alert. A value in another currency judges nothing.
func TestAbove(t *testing.T) {
	e := setup(t)
	w := watch(t, e.Client, `{"name": "Revenue above 400", "metric": "revenue", "grain": "month", "condition": {"kind": "above", "value": "400", "currency": "EUR"}}`)
	if w["status"] != "active" || w["company"] != nil || len(w["filters"].([]any)) != 0 {
		t.Errorf("watcher: got %v, want active over every company without filters", w)
	}
	ev := evaluate(t, e.Client, w)
	if got := fmt.Sprint(ev["fiscal_year"], "/", ev["fiscal_period"]); got != "2026/3" {
		t.Errorf("period: got %s, want 2026/3", got)
	}
	if got := fmt.Sprint(series(ev)); got != "[2000 EUR 500.00 400 - true]" {
		t.Errorf("series: got %s, want 2000's March alone", got)
	}
	alert := ev["alert"].(map[string]any)
	if ev["raised"] != true || fmt.Sprint(findings(alert)) != "[2000 EUR 500.00 400 -]" || fmt.Sprint(alert["companies"]) != "[2000]" {
		t.Errorf("alert: got raised %v, %v; want 2000's figure and the value", ev["raised"], alert)
	}
	if alert["watcher_name"] != "Revenue above 400" || alert["metric"] != "revenue" || alert["status"] != "open" || alert["raised_as"] != e.Tenant.Human.ID.String() {
		t.Errorf("alert: got %v, want the watcher's name and metric, open, raised as its creator", alert)
	}
	again := evaluate(t, e.Client, w)
	if again["raised"] != false || again["alert"].(map[string]any)["id"] != alert["id"] {
		t.Errorf("again: got raised %v, alert %v; want the same alert, not raised", again["raised"], again["alert"])
	}
	if a, n := e.alerts(t, w); a != 1 || n != 1 {
		t.Errorf("alerts and events: got %d and %d, want one of each", a, n)
	}
	usd := watch(t, e.Client, `{"name": "USD", "metric": "revenue", "grain": "month", "condition": {"kind": "above", "value": "1", "currency": "USD"}}`)
	ev = evaluate(t, e.Client, usd)
	if got := fmt.Sprint(series(ev)); got != "[2000 EUR 500.00 - - false]" || ev["alert"] != nil {
		t.Errorf("a value in USD: got %s, alert %v; want EUR's figure not compared, no alert", got, ev["alert"])
	}
}

// TestConditions: below and change judge one company's figures; a change
// compares with the previous period's figure in percent of it; a year
// without a previous one judges nothing.
func TestConditions(t *testing.T) {
	e := setup(t)
	for name, c := range map[string]struct {
		body, series string
		raised       bool
	}{
		"below":           {`"company": "2000", "grain": "month", "condition": {"kind": "below", "value": "600.5", "currency": "EUR"}`, "[2000 EUR 500.00 600.5 - true]", true},
		"not below":       {`"company": "2000", "grain": "month", "condition": {"kind": "below", "value": "500", "currency": "EUR"}`, "[2000 EUR 500.00 500 - false]", false},
		"change":          {`"company": "2000", "grain": "month", "condition": {"kind": "change", "percent": "149.99"}`, "[2000 EUR 500.00 200.00 150.00 true]", true},
		"small change":    {`"company": "2000", "grain": "month", "condition": {"kind": "change", "percent": "150"}`, "[2000 EUR 500.00 200.00 150.00 false]", false},
		"no earlier year": {`"grain": "year", "condition": {"kind": "change", "percent": "1"}`, "[1000 USD 800.00 - - false 2000 EUR 800.00 - - false]", false},
		"other company":   {`"company": "1000", "grain": "month", "condition": {"kind": "above", "value": "1", "currency": "USD"}`, "[1000 USD 800.00 1 - true]", true},
	} {
		t.Run(name, func(t *testing.T) {
			w := watch(t, e.Client, `{"name": "`+name+`", "metric": "revenue", `+c.body+`}`)
			ev := evaluate(t, e.Client, w)
			if got := fmt.Sprint(series(ev)); got != c.series {
				t.Errorf("series: got %s, want %s", got, c.series)
			}
			if got := ev["raised"] == true && ev["alert"] != nil; got != c.raised {
				t.Errorf("raised: got %v, want %v", got, c.raised)
			}
		})
	}
}

// TestFilters: a watcher's filters narrow its query; a count is judged
// without a currency.
func TestFilters(t *testing.T) {
	e := setup(t)
	w := watch(t, e.Client, `{"name": "January", "metric": "revenue", "grain": "month", "company": "2000",
		"filters": [{"dim": "fiscal_month", "op": "in", "values": ["1"]}], "condition": {"kind": "above", "value": "99.99", "currency": "EUR"}}`)
	if got := fmt.Sprint(series(evaluate(t, e.Client, w))); got != "[2000 EUR 100.00 99.99 - true]" {
		t.Errorf("January alone: got %s, want its figure", got)
	}
	none := watch(t, e.Client, `{"name": "Invoices", "metric": "invoice_count", "grain": "month", "condition": {"kind": "above", "value": "0"}}`)
	ev := evaluate(t, e.Client, none)
	if ev["fiscal_year"] != nil || len(ev["series"].([]any)) != 0 || ev["alert"] != nil {
		t.Errorf("no invoices: got %v, want nothing judged", ev)
	}
}

// TestCompanyAccess: a watcher over every company judges the companies
// its creator may read when it is evaluated, and one of another company
// judges nothing.
func TestCompanyAccess(t *testing.T) {
	e := setup(t)
	reader := e.As(e.Reader(t, "Rhea Reader", e.US))
	w := watch(t, reader, `{"name": "All", "metric": "revenue", "grain": "month", "condition": {"kind": "above", "value": "0", "currency": "USD"}}`)
	if got := fmt.Sprint(series(evaluate(t, reader, w))); got != "[1000 USD 800.00 0 - true]" {
		t.Errorf("the reader's companies: got %s, want 1000's alone", got)
	}
	other := watch(t, reader, `{"name": "2000", "metric": "revenue", "company": "2000", "grain": "month", "condition": {"kind": "above", "value": "0", "currency": "EUR"}}`)
	if ev := evaluate(t, reader, other); len(ev["series"].([]any)) != 0 {
		t.Errorf("another company: got %v, want nothing judged", ev["series"])
	}
}

// TestOwnWatchers: a watcher is its creator's: another person neither
// reads, lists, evaluates nor removes it; removed, it is kept, listed with
// its status and not evaluated.
func TestOwnWatchers(t *testing.T) {
	e := setup(t)
	w := watch(t, e.Client, `{"name": "Mine", "metric": "revenue", "grain": "year", "condition": {"kind": "change", "percent": "10"}}`)
	path := "/v1/metric-watchers/" + apitest.String(t, w, "id")
	other := e.As(e.Reader(t, "Otto Other", e.US, e.DE))
	other.Problem(404, "request.not_found", "GET", path, "", "")
	other.Problem(404, "request.not_found", "POST", path+":evaluate", "", etag(w))
	other.Problem(404, "request.not_found", "POST", path+":remove", "", etag(w))
	if page, _ := other.Must(200, "GET", "/v1/metric-watchers", "", ""); len(page["items"].([]any)) != 0 {
		t.Errorf("another's list: got %v, want none", page["items"])
	}
	got, tag := e.Must(200, "GET", path, "", "")
	if got["name"] != "Mine" || tag != `"1"` {
		t.Errorf("get: got %v %s, want the watcher at version 1", got, tag)
	}
	e.Problem(412, "request.version_mismatch", "POST", path+":remove", "", `"9"`)
	e.Problem(412, "request.version_mismatch", "POST", path+":evaluate", "", `"9"`)
	removed, _ := e.Must(200, "POST", path+":remove", "", etag(w))
	if removed["status"] != "removed" || removed["version"] != float64(2) {
		t.Errorf("removed: got %v, want removed at version 2", removed)
	}
	e.Problem(409, "analytics.watcher_inactive", "POST", path+":remove", "", etag(removed))
	e.Problem(409, "analytics.watcher_inactive", "POST", path+":evaluate", "", etag(removed))
	for query, want := range map[string]int{"": 1, "?status=removed": 1, "?status=active": 0, "?metric=revenue": 1, "?metric=spend": 0} {
		if page, _ := e.Must(200, "GET", "/v1/metric-watchers"+query, "", ""); len(page["items"].([]any)) != want {
			t.Errorf("list%s: got %d, want %d", query, len(page["items"].([]any)), want)
		}
	}
	var audits int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2 AND action LIKE 'analytics.metric_watcher.%'`,
		[]any{e.Tenant.ID, w["id"]}, &audits)
	if audits != 2 {
		t.Errorf("audit records: got %d, want the creation's and the removal's", audits)
	}
}

// TestPaging: the watchers are listed by name, a page at a time.
func TestPaging(t *testing.T) {
	e := setup(t)
	for _, n := range []string{"c", "a", "b"} {
		watch(t, e.Client, `{"name": "`+n+`", "metric": "revenue", "grain": "year", "condition": {"kind": "change", "percent": "10"}}`)
	}
	var names []any
	token := ""
	for {
		page, _ := e.Must(200, "GET", "/v1/metric-watchers?page_size=2"+token, "", "")
		for _, w := range page["items"].([]any) {
			names = append(names, w.(map[string]any)["name"])
		}
		next, ok := page["next_page_token"].(string)
		if !ok {
			break
		}
		token = "&page_token=" + next
	}
	if fmt.Sprint(names) != "[a b c]" {
		t.Errorf("names: got %v, want a b c", names)
	}
	e.Problem(400, "request.invalid_order_by", "GET", "/v1/metric-watchers?order_by=metric", "", "")
}

// TestRefusals: a watcher without exactly its condition's settings, on a
// metric or dimension the model lacks, or of an unknown grain, is
// refused naming the field; an agent sets none.
func TestRefusals(t *testing.T) {
	e := setup(t)
	for name, c := range map[string]struct{ body, code, field string }{
		"blank name":          {`"name": " ", "metric": "revenue", "grain": "year", "condition": {"kind": "change", "percent": "1"}`, "request.validation_failed", "name"},
		"grain":               {`"name": "x", "metric": "revenue", "grain": "week", "condition": {"kind": "change", "percent": "1"}`, "analytics.invalid_watcher", "grain"},
		"kind":                {`"name": "x", "metric": "revenue", "grain": "year", "condition": {"kind": "equal", "value": "1"}`, "analytics.invalid_watcher", "condition.kind"},
		"no value":            {`"name": "x", "metric": "revenue", "grain": "year", "condition": {"kind": "above", "currency": "EUR"}`, "analytics.invalid_watcher", "condition.value"},
		"value of a change":   {`"name": "x", "metric": "revenue", "grain": "year", "condition": {"kind": "change", "value": "1", "percent": "1"}`, "analytics.invalid_watcher", "condition.value"},
		"no currency":         {`"name": "x", "metric": "revenue", "grain": "year", "condition": {"kind": "above", "value": "1"}`, "analytics.invalid_watcher", "condition.currency"},
		"currency of a count": {`"name": "x", "metric": "invoice_count", "grain": "year", "condition": {"kind": "above", "value": "1", "currency": "EUR"}`, "analytics.invalid_watcher", "condition.currency"},
		"currency of a change": {`"name": "x", "metric": "revenue", "grain": "year", "condition": {"kind": "change", "currency": "EUR", "percent": "1"}`,
			"analytics.invalid_watcher", "condition.currency"},
		"no percent":         {`"name": "x", "metric": "revenue", "grain": "year", "condition": {"kind": "change"}`, "analytics.invalid_watcher", "condition.percent"},
		"percent of a value": {`"name": "x", "metric": "revenue", "grain": "year", "condition": {"kind": "above", "value": "1", "currency": "EUR", "percent": "1"}`, "analytics.invalid_watcher", "condition.percent"},
		"metric":             {`"name": "x", "metric": "nope", "grain": "year", "condition": {"kind": "change", "percent": "1"}`, "analytics.invalid_query", ""},
		"filter":             {`"name": "x", "metric": "revenue", "grain": "year", "filters": [{"dim": "vendor", "op": "in", "values": ["1"]}], "condition": {"kind": "change", "percent": "1"}`, "analytics.invalid_query", ""},
	} {
		t.Run(name, func(t *testing.T) {
			p := e.Problem(422, c.code, "POST", "/v1/metric-watchers", "{"+c.body+"}", "")
			if c.field != "" && fmt.Sprint(p["errors"]) == "" {
				t.Errorf("got %v, want %s named", p, c.field)
			}
			if c.field != "" {
				if errs, _ := p["errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["field"] != c.field {
					t.Errorf("errors: got %v, want %s", p["errors"], c.field)
				}
			}
		})
	}
	e.As(e.Tenant.Agent).Problem(403, "identity.agent_not_allowed", "POST", "/v1/metric-watchers",
		`{"name": "x", "metric": "revenue", "grain": "year", "condition": {"kind": "change", "percent": "1"}}`, "")
}

// TestPermissions: evaluating needs the permissions the query's data is
// read with, as the query does.
func TestPermissions(t *testing.T) {
	e := setup(t)
	w := watch(t, e.Client, `{"name": "Orders", "metric": "purchase_order_value", "grain": "year", "condition": {"kind": "change", "percent": "1"}}`)
	e.Tenant.Revoke(t, "procurement.order.read", e.Tenant.Human)
	e.Problem(403, "identity.permission_missing", "POST", "/v1/metric-watchers/"+apitest.String(t, w, "id")+":evaluate", "", etag(w))
}
