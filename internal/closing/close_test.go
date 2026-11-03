package closing_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// closePermissions are what Carla, the controller, holds to work a close.
var closePermissions = []string{"closing.close.read", "closing.close.manage", "ledger.entry.read"}

// at is a client of e whose clock reads the instant: overdue tasks are
// judged on the company's date then.
func at(t *testing.T, e fixture.Env, instant time.Time) apitest.Client {
	t.Helper()
	c := e.In(t)
	c.API = apitest.NewWith(t, func(cfg *app.Config) { cfg.Now = func() time.Time { return instant } })
	return c
}

// open opens the close of company le's period of 2026 and returns it
// with its ETag.
func open(t *testing.T, c apitest.Client, le uuid.UUID, period int, ledgers string) (map[string]any, string) {
	t.Helper()
	body := fmt.Sprintf(`{"legal_entity_id": %q, "fiscal_year": 2026, "fiscal_period": %d%s}`, le, period, ledgers)
	return c.Must(201, "POST", "/v1/period-closes", body, "")
}

// tasks reads a close's tasks as one line each: code, due date, status,
// overdue, what blocks it, its assignee and who completed it.
func tasks(t *testing.T, close map[string]any) []string {
	t.Helper()
	var out []string
	for _, x := range apitest.List(t, close, "tasks") {
		out = append(out, taskLine(x.(map[string]any)))
	}
	return out
}

func taskLine(k map[string]any) string {
	return fmt.Sprintf("%v %v %v overdue=%v blocked=%v assignee=%v done_by=%v note=%v", k["code"], k["due_on"], k["status"], k["overdue"], k["blocked_by"],
		k["assignee_name"], k["completed_by_name"], k["note"])
}

func wantLines(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\ngot  %q\nwant %q", what, got, want)
	}
}

// TestCreatePeriodClose: a close of a company's regular period gets the
// template's tasks for the company, due on its business days as the
// calendar plans them, the configuration's version, and covers every
// ledger the company keeps, the leading one first; it is published, and
// the calendar's row shows it.
func TestCreatePeriodClose(t *testing.T) {
	e := fixture.NewWith(t, nil, withTemplate, closePermissions...)
	c := at(t, e, time.Date(2026, 2, 5, 12, 0, 0, 0, time.UTC))
	got, tag := open(t, c, e.US, 1, "")
	if tag != `"1"` {
		t.Errorf("ETag: got %s, want \"1\"", tag)
	}
	head := fmt.Sprintf("%v %v %v/%v ends %v %v read %v %v: %v total, %v done, %v open, %v overdue", got["company_code"], got["company_name"], got["fiscal_year"],
		got["fiscal_period"], got["period_end"], got["status"], got["read_date"], got["time_zone"], got["tasks_total"], got["tasks_done"], got["tasks_open"],
		got["tasks_overdue"])
	if want := "1000 Acme Corp US 2026/1 ends 2026-01-31 open read 2026-02-05 UTC: 3 total, 0 done, 3 open, 2 overdue"; head != want {
		t.Errorf("close: got %q, want %q", head, want)
	}
	if v, ok := got["config_version"].(float64); !ok || v < 1 {
		t.Errorf("config_version: got %v, want the active version", got["config_version"])
	}
	// The template's order; January ends on a Saturday and Monday 2
	// February is company 1000's holiday.
	wantLines(t, "tasks", tasks(t, got), []string{
		"AP_CUTOFF 2026-02-04 open overdue=true blocked=[] assignee=<nil> done_by=<nil> note=<nil>",
		"REVIEW 2026-01-30 open overdue=true blocked=[] assignee=<nil> done_by=<nil> note=<nil>",
		"REPORT 2026-02-09 open overdue=false blocked=[AP_CUTOFF] assignee=<nil> done_by=<nil> note=<nil>",
	})
	ledgers := apitest.List(t, got, "ledgers")
	if len(ledgers) != 1 || ledgers[0].(map[string]any)["ledger_code"] != config.LeadingLedger || ledgers[0].(map[string]any)["period_status"] != "open" {
		t.Errorf("ledgers of company 1000: got %v, want the leading ledger's open period", ledgers)
	}
	id := apitest.String(t, got, "id")
	read, readTag := c.Must(200, "GET", "/v1/period-closes/"+id, "", "")
	if readTag != tag || read["tasks_overdue"] != got["tasks_overdue"] {
		t.Errorf("read back: got %s %v, want %s %v", readTag, read["tasks_overdue"], tag, got["tasks_overdue"])
	}

	events := apitest.Published(t, e.Tenant, outbox.Event{Context: "closing", Name: "period_close_changed"})
	if len(events) != 1 || events[0]["change"] != "created" || events[0]["period_close_id"] != id || events[0]["task_code"] != nil {
		t.Errorf("events: got %v, want one created event of the close", events)
	}

	cal, _ := c.Must(200, "GET", "/v1/close-calendar?legal_entity_id="+e.US.String()+"&fiscal_year=2026", "", "")
	row := apitest.List(t, cal, "periods")[0].(map[string]any)
	if got := fmt.Sprintf("%v %v", row["close"], row["first_due_on"]); got != fmt.Sprintf("map[id:%s status:open tasks_done:0 tasks_overdue:2 tasks_total:3] 2026-01-30", id) {
		t.Errorf("calendar row: got %s", got)
	}
}

// TestCreatePeriodCloseLedgers: a close covers the ledgers it names, the
// leading one always and first, then by code.
func TestCreatePeriodCloseLedgers(t *testing.T) {
	e := fixture.NewWith(t, nil, withTemplate, closePermissions...)
	leading, parallel := e.Ledger(t, config.LeadingLedger), e.Ledger(t, "2L")
	both, _ := open(t, e.Client, e.DE, 1, "")
	only, _ := open(t, e.Client, e.DE, 2, fmt.Sprintf(`, "ledger_ids": [%q]`, leading))
	named, _ := open(t, e.Client, e.DE, 3, fmt.Sprintf(`, "ledger_ids": [%q, %q]`, parallel, leading))
	for name, tc := range map[string]struct {
		close map[string]any
		want  string
	}{
		"by default": {both, "0L 2L"}, "the leading alone": {only, "0L"}, "named in another order": {named, "0L 2L"},
	} {
		t.Run(name, func(t *testing.T) {
			var codes []string
			for _, l := range apitest.List(t, tc.close, "ledgers") {
				codes = append(codes, l.(map[string]any)["ledger_code"].(string))
			}
			if got := strings.Join(codes, " "); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
	// Company 2000's close has the template's tasks for every company.
	if got := len(tasks(t, both)); got != 2 {
		t.Errorf("tasks of company 2000: got %d, want 2", got)
	}
}

// TestCreatePeriodCloseRefusals: a special period, a company the caller
// cannot act in, a ledger it does not keep, one named twice, a close
// without the leading ledger, a period the ledgers lack, a company the
// template gives no task, and a second close of a period are refused.
func TestCreatePeriodCloseRefusals(t *testing.T) {
	e := fixture.NewWith(t, nil, withTemplate, closePermissions...)
	leading, parallel := e.Ledger(t, config.LeadingLedger), e.Ledger(t, "2L")
	reader := e.As(e.Reader(t, "reader", e.US))
	body := func(le uuid.UUID, year, period int, ledgers string) string {
		return fmt.Sprintf(`{"legal_entity_id": %q, "fiscal_year": %d, "fiscal_period": %d%s}`, le, year, period, ledgers)
	}
	cases := []struct {
		name, body, code, field string
		client                  apitest.Client
		status                  int
	}{
		{"a special period", body(e.US, 2026, 13, ""), "closing.special_period", "fiscal_period", e.Client, 422},
		{"an unknown legal entity", body(uuid.New(), 2026, 1, ""), "request.reference_not_found", "legal_entity_id", e.Client, 422},
		{"a company the caller cannot act in", body(e.DE, 2026, 1, ""), "request.reference_not_found", "legal_entity_id", reader, 422},
		{"a ledger the company does not keep", body(e.US, 2026, 1, fmt.Sprintf(`, "ledger_ids": [%q, %q]`, leading, parallel)), "ledger.ledger_not_kept",
			"ledger_ids[1]", e.Client, 422},
		{"without the leading ledger", body(e.DE, 2026, 1, fmt.Sprintf(`, "ledger_ids": [%q]`, parallel)), "request.validation_failed", "ledger_ids", e.Client, 422},
		{"a year without periods", body(e.US, 2999, 1, ""), "closing.period_unknown", "fiscal_period", e.Client, 422},
		{"a named ledger without the period", body(e.US, 2999, 1, fmt.Sprintf(`, "ledger_ids": [%q]`, leading)), "closing.period_unknown", "ledger_ids[0]",
			e.Client, 422},
		{"a ledger named twice", body(e.DE, 2026, 1, fmt.Sprintf(`, "ledger_ids": [%q, %q]`, leading, leading)), "request.invalid_body", "",
			e.Client, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.client.In(t).Problem(tc.status, tc.code, "POST", "/v1/period-closes", tc.body, "")
			if errs, _ := p["errors"].([]any); tc.field != "" && (len(errs) != 1 || errs[0].(map[string]any)["field"] != tc.field) {
				t.Errorf("field: got %v, want %s", p["errors"], tc.field)
			}
		})
	}

	first, _ := open(t, e.Client, e.US, 4, "")
	r := e.API.Do(apitest.Req{Method: "POST", Path: "/v1/period-closes", Body: body(e.US, 2026, 4, ""), Tenant: e.Tenant.ID, As: e.Tenant.Human,
		Key: apitest.NewKey()})
	if got := apitest.JSON(t, r)["code"]; r.Code != http.StatusConflict || got != "closing.close_exists" {
		t.Errorf("a second close: got %d %v, want 409 closing.close_exists", r.Code, got)
	}
	if got, want := r.Header().Get("Location"), "/v1/period-closes/"+apitest.String(t, first, "id"); got != want {
		t.Errorf("a second close's Location: got %q, want %q", got, want)
	}

	bare := fixture.NewWith(t, nil, nil, closePermissions...)
	bare.Problem(409, "closing.no_template", "POST", "/v1/period-closes", body(bare.US, 2026, 1, ""), "")
	ten := pgtest.NewTenant(t, closePermissions...)
	none := apitest.Client{T: t, API: e.API, Tenant: ten, As: ten.Human}
	none.Problem(409, "config.not_bootstrapped", "POST", "/v1/period-closes", body(uuid.New(), 2026, 1, ""), "")
}

// TestCreatePeriodCloseOnlyTheTemplatesTasks: a close gets only the
// template's tasks for its company, and a template giving a company none
// refuses its close (409 closing.no_template).
func TestCreatePeriodCloseOnlyTheTemplatesTasks(t *testing.T) {
	e := fixture.NewWith(t, nil, func(doc *config.Config) {
		withTemplate(doc)
		doc.Close.Tasks = doc.Close.Tasks[2:] // REPORT, company 1000's alone
		doc.Close.Tasks[0].DependsOn = nil
	}, closePermissions...)
	e.Problem(409, "closing.no_template", "POST", "/v1/period-closes", fmt.Sprintf(`{"legal_entity_id": %q, "fiscal_year": 2026, "fiscal_period": 1}`, e.DE), "")
	got, _ := open(t, at(t, e, time.Date(2026, 2, 5, 12, 0, 0, 0, time.UTC)), e.US, 1, "")
	wantLines(t, "company 1000's tasks", tasks(t, got), []string{"REPORT 2026-02-09 open overdue=false blocked=[] assignee=<nil> done_by=<nil> note=<nil>"})
}

// TestReadPeriodCloseRefusals: a close of a company the caller cannot act
// in, or none, is not found; reads need ledger.entry.read beside
// closing.close.read.
func TestReadPeriodCloseRefusals(t *testing.T) {
	e := fixture.NewWith(t, nil, withTemplate, closePermissions...)
	got, _ := open(t, e.Client, e.DE, 1, "")
	id := apitest.String(t, got, "id")
	e.As(e.Reader(t, "reader", e.US)).Problem(404, "request.not_found", "GET", "/v1/period-closes/"+id, "", "")
	e.Problem(404, "request.not_found", "GET", "/v1/period-closes/"+uuid.NewString(), "", "")
	e.Tenant.Revoke(t, "ledger.entry.read", e.Tenant.Human)
	e.Problem(403, "identity.permission_missing", "GET", "/v1/period-closes/"+id, "", "")
}

// TestCreatePeriodCloseConcurrently: two closes of one period opened at
// once: the database's uniqueness lets one in and refuses the other with
// 409 closing.close_exists.
func TestCreatePeriodCloseConcurrently(t *testing.T) {
	e := fixture.NewWith(t, nil, withTemplate, closePermissions...)
	h := endpoints(t)
	create := func(ctx context.Context) error {
		_, err := h.CreatePeriodClose(ctx, &gen.PeriodCloseCreate{LegalEntityID: e.US, FiscalYear: 2026, FiscalPeriod: 5}, gen.CreatePeriodCloseParams{})
		return err
	}
	first, second := apitest.Serialized(t, e.Tenant, e.Tenant.Human, create, create)
	if first != nil {
		t.Fatalf("first: got %v, want the close", first)
	}
	if got := problem.From(second).Code; got != "closing.close_exists" {
		t.Errorf("second: got %s, want closing.close_exists", got)
	}
}
