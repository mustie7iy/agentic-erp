package reports_test

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// perms are what the tests' people hold: posting and reading the revenue
// a report shows, running queries and keeping reports.
var perms = []string{"ledger.entry.post", "ledger.entry.read", "ledger.account.read", "partner.supplier.read", "tenancy.org.read",
	"analytics.query.run", "analytics.report.read", "analytics.report.write"}

// env is a tenant with revenue in company 2000.
type env struct {
	fixture.Env
}

func setup(t *testing.T) env {
	t.Helper()
	e := env{Env: fixture.New(t, perms...)}
	e.revenue(t, "500.00")
	return e
}

// revenue posts revenue of the amount in EUR in company 2000.
func (e env) revenue(t *testing.T, amount string) {
	t.Helper()
	e.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-20", "posting_date": "2026-03-20",
		"currency": "EUR", "lines": [{"account_id": %q, "amount": {"amount": %q, "currency": "EUR"}},
		{"account_id": %q, "amount": {"amount": "-%s", "currency": "EUR"}}]}`,
		e.DE, e.Account(t, "2000", "100000"), amount, e.Account(t, "2000", "400000"), amount), "")
}

// reportOf is a report's JSON: a tile of company 2000's revenue and a
// table of it by company, with the commentary given.
func reportOf(commentary string) string {
	return fmt.Sprintf(`{"spec": {"title": "Revenue by company", "question": "What is our revenue by company?",
		"tiles": [{"label": "Revenue 2000", "query_id": "rev", "measure": "revenue", "where": {"company": "2000"}}],
		"tables": [{"title": "Revenue by company", "query_id": "rev"}], "commentary": %q},
		"queries": {"rev": {"measures": ["revenue"], "by": ["company"], "order_by": "company"}}}`, commentary)
}

// save saves a report and returns it and its ETag.
func (e env) save(t *testing.T, body string) (map[string]any, string) {
	t.Helper()
	return e.Must(201, "POST", "/v1/reports", body, "")
}

// cells are a run's results' rows as "value currency" cells.
func cells(t *testing.T, run map[string]any) string {
	t.Helper()
	var out []string
	for _, r := range apitest.List(t, run, "results") {
		for _, row := range r.(map[string]any)["rows"].([]any) {
			var cs []string
			for _, c := range row.([]any) {
				cell := c.(map[string]any)
				v := fmt.Sprint(cell["value"])
				if cur, ok := cell["currency"]; ok {
					v += " " + fmt.Sprint(cur)
				}
				cs = append(cs, v)
			}
			out = append(out, strings.Join(cs, " "))
		}
	}
	return strings.Join(out, ", ")
}

// audited are the audit records of a report as "action detail", in the
// order they were written.
func (e env) audited(t *testing.T, id string) []string {
	t.Helper()
	var out []string
	pgtest.Scan(t, `SELECT coalesce(array_agg(action || ' ' || detail::text ORDER BY seq), '{}') FROM audit.audit_event
		WHERE tenant_id = $1 AND object_type = 'analytics.report' AND object_id = $2`, []any{e.Tenant.ID, id}, &out)
	return out
}

// TestSaveAndRun: a report is saved as the caller's and run again on
// current data, which shows a figure posted since (T95a).
func TestSaveAndRun(t *testing.T) {
	e := setup(t)
	rep, tag := e.save(t, reportOf("Revenue in company 2000 is 500.00 EUR."))
	id := apitest.String(t, rep, "id")
	for f, want := range map[string]any{"title": "Revenue by company", "question": "What is our revenue by company?", "status": "active",
		"owner_id": e.Tenant.Human.ID.String(), "created_by": e.Tenant.Human.ID.String(), "version": float64(1)} {
		if rep[f] != want {
			t.Errorf("%s: got %v, want %v", f, rep[f], want)
		}
	}
	if rep["owner_name"] == "" || rep["owner_name"] == nil {
		t.Errorf("owner_name: got %v, want the owner's name", rep["owner_name"])
	}
	if tag != `"1"` {
		t.Errorf("ETag: got %s, want \"1\"", tag)
	}
	if got := fmt.Sprint(rep["queries"]); got != "map[rev:map[by:[company] measures:[revenue] order_by:company]]" {
		t.Errorf("queries: got %s, want the query as saved", got)
	}
	if got, _ := e.Must(200, "GET", "/v1/reports/"+id, "", ""); fmt.Sprint(got) != fmt.Sprint(rep) {
		t.Errorf("get: got %v, want %v", got, rep)
	}
	if got := e.audited(t, id); len(got) != 1 || !strings.HasPrefix(got[0], `analytics.report.create {"title": "Revenue by company", "queries": ["rev"]}`) {
		t.Errorf("audit: got %v, want the create", got)
	}
	run, runTag := e.Must(200, "POST", "/v1/reports/"+id+":run", "", tag)
	if got := cells(t, run); got != "2000 500.00 EUR" {
		t.Errorf("first run: got %s, want 2000's 500.00 EUR", got)
	}
	if runTag != tag {
		t.Errorf("run's ETag: got %s, want the report's %s", runTag, tag)
	}
	spec := run["spec"].(map[string]any)
	resultID := apitest.List(t, run, "results")[0].(map[string]any)["query_id"]
	if got := spec["tables"].([]any)[0].(map[string]any)["query_id"]; got != resultID {
		t.Errorf("the table's query: got %v, want the result's %v", got, resultID)
	}
	if spec["commentary"] != "Revenue in company 2000 is 500.00 EUR." || run["data_through"] != "2026-03-20" || run["report_id"] != id {
		t.Errorf("run: got %v, want the commentary, the data's date and the report", run)
	}
	e.revenue(t, "250.00")
	run, _ = e.Must(200, "POST", "/v1/reports/"+id+":run", "", tag)
	if got := cells(t, run); got != "2000 750.00 EUR" {
		t.Errorf("second run: got %s, want 2000's 750.00 EUR", got)
	}
	spec = run["spec"].(map[string]any)
	if spec["commentary"] != nil || !strings.Contains(fmt.Sprint(spec["assumptions"]), "commentary was removed") {
		t.Errorf("stale commentary: got %v and %v, want it left out, saying so", spec["commentary"], spec["assumptions"])
	}
	page, _ := e.Must(200, "GET", "/v1/reports?status=active", "", "")
	if items := apitest.List(t, page, "items"); len(items) != 1 || items[0].(map[string]any)["id"] != id {
		t.Errorf("list: got %v, want the report", items)
	}
}

// TestOwnerAlone: a report is read and run by its owner alone (ADR-0076
// §2); another person neither lists nor finds it.
func TestOwnerAlone(t *testing.T) {
	e := setup(t)
	rep, tag := e.save(t, reportOf(""))
	id := apitest.String(t, rep, "id")
	other := e.As(e.Person(t, "Olga Other"))
	page, _ := other.Must(200, "GET", "/v1/reports", "", "")
	if items := apitest.List(t, page, "items"); len(items) != 0 {
		t.Errorf("another's list: got %v, want none", items)
	}
	for name, c := range map[string]struct{ method, path string }{
		"get": {"GET", "/v1/reports/" + id}, "run": {"POST", "/v1/reports/" + id + ":run"}, "remove": {"POST", "/v1/reports/" + id + ":remove"},
	} {
		t.Run(name, func(t *testing.T) {
			other.In(t).Problem(404, "request.not_found", c.method, c.path, "", tag)
		})
	}
}

// TestSaveRefusals: a report is checked as a dashboard is built.
func TestSaveRefusals(t *testing.T) {
	e := setup(t)
	for name, c := range map[string]struct{ body, code string }{
		"a query the report lacks": {`{"spec": {"title": "T", "tables": [{"title": "x", "query_id": "nope"}]},
			"queries": {"rev": {"measures": ["revenue"], "by": ["company"]}}}`, "analytics.invalid_report"},
		"a measure the query lacks": {`{"spec": {"title": "T", "charts": [{"title": "x", "type": "bar", "query_id": "rev", "x": "company", "measure": "expenses"}]},
			"queries": {"rev": {"measures": ["revenue"], "by": ["company"]}}}`, "analytics.invalid_report"},
		"no tile, chart or table": {`{"spec": {"title": "T"}, "queries": {"rev": {"measures": ["revenue"]}}}`, "analytics.invalid_report"},
		"unsupported commentary":  {reportOf("Revenue in company 2000 is 999.00 EUR."), "analytics.invalid_report"},
		"a metric the model lacks": {`{"spec": {"title": "T", "tables": [{"title": "x", "query_id": "q"}]},
			"queries": {"q": {"measures": ["nope"]}}}`, "analytics.report_metric_removed"},
		"a query the semantic layer refuses": {`{"spec": {"title": "T", "tables": [{"title": "x", "query_id": "q"}]},
			"queries": {"q": {"measures": ["revenue"], "by": ["vendor"]}}}`, "analytics.invalid_query"},
		"a query id the contract lacks": {`{"spec": {"title": "T", "tables": [{"title": "x", "query_id": "a b"}]},
			"queries": {"a b": {"measures": ["revenue"], "by": ["company"]}}}`, "request.validation_failed"},
	} {
		t.Run(name, func(t *testing.T) {
			e.In(t).Problem(422, c.code, "POST", "/v1/reports", c.body, "")
		})
	}
	page, _ := e.Must(200, "GET", "/v1/reports", "", "")
	if items := apitest.List(t, page, "items"); len(items) != 0 {
		t.Errorf("reports after refusals: got %v, want none", items)
	}
}

// TestRunRefusals: a report whose query names a metric removed since,
// or whose specification its results no longer support, is refused.
func TestRunRefusals(t *testing.T) {
	e := setup(t)
	for name, c := range map[string]struct{ set, code, detail string }{
		"a metric removed since": {`queries = '{"rev": {"measures": ["gone"], "by": ["company"]}}'`, "analytics.report_metric_removed",
			"Query rev of the report names metric gone"},
		"a query the specification no longer finds": {`spec = '{"title": "T", "tables": [{"title": "x", "query_id": "other"}]}'`,
			"analytics.invalid_report", "The report's specification"},
	} {
		t.Run(name, func(t *testing.T) {
			rep, _ := e.save(t, reportOf(""))
			pgtest.Exec(t, `UPDATE analytics.report SET `+c.set+`, version = 2 WHERE id = $1`, rep["id"])
			p := e.In(t).Problem(422, c.code, "POST", "/v1/reports/"+apitest.String(t, rep, "id")+":run", "", `"2"`)
			if !strings.HasPrefix(fmt.Sprint(p["detail"]), c.detail) {
				t.Errorf("detail: got %v, want it to start %q", p["detail"], c.detail)
			}
		})
	}
}

// TestRemove: a report is removed once, at its version; it is kept,
// listed by status and not run.
func TestRemove(t *testing.T) {
	e := setup(t)
	rep, tag := e.save(t, reportOf(""))
	id := apitest.String(t, rep, "id")
	e.Problem(412, "request.version_mismatch", "POST", "/v1/reports/"+id+":remove", "", `"9"`)
	removed, tag2 := e.Must(200, "POST", "/v1/reports/"+id+":remove", "", tag)
	if removed["status"] != "removed" || tag2 != `"2"` {
		t.Errorf("removed: got %v with %s, want status removed at \"2\"", removed["status"], tag2)
	}
	e.Problem(409, "analytics.report_removed", "POST", "/v1/reports/"+id+":remove", "", tag2)
	e.Problem(409, "analytics.report_removed", "POST", "/v1/reports/"+id+":run", "", tag2)
	for status, want := range map[string]int{"removed": 1, "active": 0} {
		page, _ := e.Must(200, "GET", "/v1/reports?status="+status, "", "")
		if got := len(apitest.List(t, page, "items")); got != want {
			t.Errorf("reports %s: got %d, want %d", status, got, want)
		}
	}
	if got := e.audited(t, id); len(got) != 2 || got[1] != `analytics.report.remove {"title": "Revenue by company"}` {
		t.Errorf("audit: got %v, want the create and the remove", got)
	}
}

// TestPages: the caller's reports are listed by title, a page at a time.
func TestPages(t *testing.T) {
	e := setup(t)
	for _, title := range []string{"Beta", "Alpha"} {
		e.save(t, strings.Replace(reportOf(""), `"title": "Revenue by company", "question"`, `"title": "`+title+`", "question"`, 1))
	}
	var titles []string
	path := "/v1/reports?page_size=1"
	for range 3 {
		page, _ := e.Must(200, "GET", path, "", "")
		for _, it := range apitest.List(t, page, "items") {
			titles = append(titles, fmt.Sprint(it.(map[string]any)["title"]))
		}
		next, ok := page["next_page_token"].(string)
		if !ok {
			break
		}
		path = "/v1/reports?page_size=1&page_token=" + url.QueryEscape(next)
	}
	if got := strings.Join(titles, ", "); got != "Alpha, Beta" {
		t.Errorf("titles: got %s, want Alpha, Beta", got)
	}
}
