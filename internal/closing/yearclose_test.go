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
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// withYearTemplate is withTemplate with year tasks (ADR-0060 §8): two for
// every company, the second depending on the first, and one for company
// 1000 only; AP_CUTOFF repeats a period task's code, a list's codes being
// its own.
func withYearTemplate(doc *config.Config) {
	withTemplate(doc)
	doc.Close.YearTasks = []config.CloseTask{
		{Code: "AP_CUTOFF", Name: "Auditors' adjustments entered", Kind: config.CloseManual, DueDay: 2},
		{Code: "TAX", Name: "Tax provision booked", Kind: config.CloseManual, DueDay: 5, DependsOn: []string{"AP_CUTOFF"}},
		{Code: "INVENTORY", Name: "Inventory count reconciled", Kind: config.CloseManual, DueDay: -1, Companies: []string{"1000"}},
	}
}

// openYear opens the year close of company le's 2026 and returns it with
// its ETag.
func openYear(t *testing.T, c apitest.Client, le uuid.UUID, ledgers string) (map[string]any, string) {
	t.Helper()
	return c.Must(201, "POST", "/v1/year-closes", fmt.Sprintf(`{"legal_entity_id": %q, "fiscal_year": 2026%s}`, le, ledgers), "")
}

// yearLedgers reads a year close's ledgers as one line each: code, whether
// leading, and its periods as number and status, special ones starred.
func yearLedgers(t *testing.T, close map[string]any) []string {
	t.Helper()
	var out []string
	for _, x := range apitest.List(t, close, "ledgers") {
		l := x.(map[string]any)
		var periods []string
		for _, y := range apitest.List(t, l, "periods") {
			p := y.(map[string]any)
			star := ""
			if p["is_special"] == true {
				star = "*"
			}
			periods = append(periods, fmt.Sprintf("%v%s=%v", p["period"], star, p["status"]))
		}
		out = append(out, fmt.Sprintf("%v leading=%v %s", l["ledger_code"], l["is_leading"], strings.Join(periods, " ")))
	}
	return out
}

// TestCreateYearClose: a year close of a company's fiscal year gets the
// year tasks for the company, due on its business days counted from the
// year's last day, the configuration's version, and covers every ledger the
// company keeps with its periods of the year; it is audited and published,
// and the period closes' worklist does not list its tasks.
func TestCreateYearClose(t *testing.T) {
	e := fixture.NewWith(t, nil, withYearTemplate, closePermissions...)
	c := at(t, e, time.Date(2027, 1, 5, 12, 0, 0, 0, time.UTC))
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
		AND fiscal_year = 2026 AND period = 12`, e.Tenant.ID, e.US)
	got, tag := openYear(t, c, e.US, "")
	if tag != `"1"` {
		t.Errorf("ETag: got %s, want \"1\"", tag)
	}
	head := fmt.Sprintf("%v %v %v ends %v %v read %v %v: %v total, %v done, %v open, %v overdue", got["company_code"], got["company_name"], got["fiscal_year"],
		got["year_end"], got["status"], got["read_date"], got["time_zone"], got["tasks_total"], got["tasks_done"], got["tasks_open"], got["tasks_overdue"])
	if want := "1000 Acme Corp US 2026 ends 2026-12-31 open read 2027-01-05 UTC: 3 total, 0 done, 3 open, 2 overdue"; head != want {
		t.Errorf("year close: got %q, want %q", head, want)
	}
	if _, ok := got["fiscal_period"]; ok {
		t.Errorf("fiscal_period: got %v, want none on a year close", got["fiscal_period"])
	}
	// 31 December 2026 is a Thursday: two business days after it is Monday
	// 4 January, five Thursday 7 January, one before it Wednesday 30.
	wantLines(t, "tasks", tasks(t, got), []string{
		"AP_CUTOFF 2027-01-04 open overdue=true blocked=[] assignee=<nil> done_by=<nil> note=<nil>",
		"TAX 2027-01-07 open overdue=false blocked=[AP_CUTOFF] assignee=<nil> done_by=<nil> note=<nil>",
		"INVENTORY 2026-12-30 open overdue=true blocked=[] assignee=<nil> done_by=<nil> note=<nil>",
	})
	wantLines(t, "ledgers", yearLedgers(t, got), []string{"0L leading=true 1=open 2=open 3=open 4=open 5=open 6=open 7=open 8=open 9=open 10=open 11=open " +
		"12=closed 13*=closed 14*=closed 15*=closed 16*=closed"})
	id := apitest.String(t, got, "id")
	read, readTag := c.Must(200, "GET", "/v1/year-closes/"+id, "", "")
	if readTag != tag {
		t.Errorf("read back's ETag: got %s, want %s", readTag, tag)
	}
	wantFields(t, "read back", read, map[string]any{"tasks_overdue": got["tasks_overdue"], "config_version": got["config_version"]})

	events := apitest.Published(t, e.Tenant, outbox.Event{Context: "closing", Name: "year_close_changed"})
	if len(events) != 1 {
		t.Fatalf("events: got %v, want one", events)
	}
	wantFields(t, "the event", events[0], map[string]any{"change": "created", "year_close_id": id, "fiscal_year": 2026.0, "task_code": nil})
	if _, ok := events[0]["fiscal_period"]; ok {
		t.Errorf("the event: got fiscal_period %v, want none", events[0]["fiscal_period"])
	}
	var audited string
	pgtest.Scan(t, `SELECT string_agg(action || ' ' || object_type, ',') FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2`,
		[]any{e.Tenant.ID, uuid.MustParse(id)}, &audited)
	if audited != "closing.year_close.create closing.year_close" {
		t.Errorf("audited: got %q, want the creation", audited)
	}
	work, _ := c.Must(200, "GET", "/v1/close-tasks?legal_entity_id="+e.US.String(), "", "")
	if items := apitest.List(t, work, "items"); len(items) != 0 {
		t.Errorf("the period closes' worklist: got %v, want no year task", items)
	}
	// The period close of December is another resource.
	e.Problem(404, "request.not_found", "GET", "/v1/period-closes/"+id, "", "")
}

// TestCreateYearCloseLedgers: a year close covers the ledgers it names,
// the leading one always and first, then by code; a company's year close
// has the year tasks for every company, and no period task.
func TestCreateYearCloseLedgers(t *testing.T) {
	e := fixture.NewWith(t, nil, withYearTemplate, closePermissions...)
	both, _ := openYear(t, e.Client, e.DE, "")
	codes := func(close map[string]any) string {
		var out []string
		for _, l := range apitest.List(t, close, "ledgers") {
			out = append(out, l.(map[string]any)["ledger_code"].(string))
		}
		return strings.Join(out, " ")
	}
	if got := codes(both); got != "0L 2L" {
		t.Errorf("by default: got %s, want 0L 2L", got)
	}
	if got := len(tasks(t, both)); got != 2 {
		t.Errorf("tasks of company 2000: got %d, want 2", got)
	}
	other := fixture.NewWith(t, nil, withYearTemplate, closePermissions...)
	only, _ := openYear(t, other.Client, other.DE, fmt.Sprintf(`, "ledger_ids": [%q]`, other.Ledger(t, config.LeadingLedger)))
	if got := codes(only); got != "0L" {
		t.Errorf("the leading alone: got %s, want 0L", got)
	}
	third := fixture.NewWith(t, nil, withYearTemplate, closePermissions...)
	named, _ := openYear(t, third.Client, third.DE, fmt.Sprintf(`, "ledger_ids": [%q, %q]`, third.Ledger(t, "2L"), third.Ledger(t, config.LeadingLedger)))
	if got := codes(named); got != "0L 2L" {
		t.Errorf("named in another order: got %s, want 0L 2L", got)
	}
}

// TestCreateYearCloseRefusals: a company the caller cannot act in, a
// ledger it does not keep, a close without the leading ledger, a year the
// ledgers lack, a ledger named twice, a company the year tasks give no
// task, and a second close of a year are refused.
func TestCreateYearCloseRefusals(t *testing.T) {
	e := fixture.NewWith(t, nil, withYearTemplate, closePermissions...)
	leading, parallel := e.Ledger(t, config.LeadingLedger), e.Ledger(t, "2L")
	reader := e.As(e.Reader(t, "reader", e.US))
	body := func(le uuid.UUID, year int, ledgers string) string {
		return fmt.Sprintf(`{"legal_entity_id": %q, "fiscal_year": %d%s}`, le, year, ledgers)
	}
	cases := []struct {
		name, body, code, field string
		client                  apitest.Client
		status                  int
	}{
		{"an unknown legal entity", body(uuid.New(), 2026, ""), "request.reference_not_found", "legal_entity_id", e.Client, 422},
		{"a company the caller cannot act in", body(e.DE, 2026, ""), "request.reference_not_found", "legal_entity_id", reader, 422},
		{"a ledger the company does not keep", body(e.US, 2026, fmt.Sprintf(`, "ledger_ids": [%q, %q]`, leading, parallel)), "ledger.ledger_not_kept",
			"ledger_ids[1]", e.Client, 422},
		{"without the leading ledger", body(e.DE, 2026, fmt.Sprintf(`, "ledger_ids": [%q]`, parallel)), "request.validation_failed", "ledger_ids", e.Client, 422},
		{"a year without periods", body(e.US, 2999, ""), "closing.period_unknown", "fiscal_year", e.Client, 422},
		{"a named ledger without the year", body(e.US, 2999, fmt.Sprintf(`, "ledger_ids": [%q]`, leading)), "closing.period_unknown", "ledger_ids[0]",
			e.Client, 422},
		{"a ledger named twice", body(e.DE, 2026, fmt.Sprintf(`, "ledger_ids": [%q, %q]`, leading, leading)), "request.invalid_body", "", e.Client, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.client.In(t).Problem(tc.status, tc.code, "POST", "/v1/year-closes", tc.body, "")
			if errs, _ := p["errors"].([]any); tc.field != "" && (len(errs) != 1 || errs[0].(map[string]any)["field"] != tc.field) {
				t.Errorf("field: got %v, want %s", p["errors"], tc.field)
			}
		})
	}

	first, _ := openYear(t, e.Client, e.US, "")
	r := e.API.Do(apitest.Req{Method: "POST", Path: "/v1/year-closes", Body: body(e.US, 2026, ""), Tenant: e.Tenant.ID, As: e.Tenant.Human,
		Key: apitest.NewKey()})
	if got, want := fmt.Sprintf("%d %v", r.Code, apitest.JSON(t, r)["code"]), fmt.Sprintf("%d closing.year_close_exists", http.StatusConflict); got != want {
		t.Errorf("a second close: got %s, want %s", got, want)
	}
	if got, want := r.Header().Get("Location"), "/v1/year-closes/"+apitest.String(t, first, "id"); got != want {
		t.Errorf("a second close's Location: got %q, want %q", got, want)
	}
	// A period close of December is another close: the year's is no
	// period close's.
	open(t, e.Client, e.US, 12, "")

	periodsOnly := fixture.NewWith(t, nil, withTemplate, closePermissions...)
	p := periodsOnly.Problem(409, "closing.no_template", "POST", "/v1/year-closes", body(periodsOnly.US, 2026, ""), "")
	if !strings.Contains(fmt.Sprint(p["detail"]), "close.year_tasks") {
		t.Errorf("no year tasks: got %v, want the year tasks named", p["detail"])
	}
	onlyUS := fixture.NewWith(t, nil, func(doc *config.Config) {
		withYearTemplate(doc)
		doc.Close.YearTasks = doc.Close.YearTasks[2:] // INVENTORY, company 1000's alone
	}, closePermissions...)
	onlyUS.Problem(409, "closing.no_template", "POST", "/v1/year-closes", body(onlyUS.DE, 2026, ""), "")
	ten := pgtest.NewTenant(t, closePermissions...)
	none := apitest.Client{T: t, API: e.API, Tenant: ten, As: ten.Human}
	none.Problem(409, "config.not_bootstrapped", "POST", "/v1/year-closes", body(uuid.New(), 2026, ""), "")
}

// TestCreateYearCloseConcurrently: two closes of one year opened at once:
// the database's uniqueness lets one in and refuses the other with 409
// closing.year_close_exists.
func TestCreateYearCloseConcurrently(t *testing.T) {
	e := fixture.NewWith(t, nil, withYearTemplate, closePermissions...)
	h := endpoints(t)
	create := func(ctx context.Context) error {
		_, err := h.CreateYearClose(ctx, &gen.YearCloseCreate{LegalEntityID: e.US, FiscalYear: 2026}, gen.CreateYearCloseParams{})
		return err
	}
	first, second := apitest.Serialized(t, e.Tenant, e.Tenant.Human, create, create)
	if first != nil {
		t.Fatalf("first: got %v, want the close", first)
	}
	if got := problem.From(second).Code; got != "closing.year_close_exists" {
		t.Errorf("second: got %s, want closing.year_close_exists", got)
	}
}

// TestYearCloseTasks: a person assigns a year close's task, completes it
// with a note once the tasks it depends on are done, and reopens it with a
// reason; each change moves the close's version, refuses a stale one, and
// is audited and published as YearCloseChanged.
func TestYearCloseTasks(t *testing.T) {
	e := fixture.NewWith(t, nil, withYearTemplate, closePermissions...)
	c := at(t, e, time.Date(2027, 1, 5, 12, 0, 0, 0, time.UTC))
	got, tag := openYear(t, c, e.US, "")
	id := apitest.String(t, got, "id")
	base := "/v1/year-closes/" + id + "/tasks/"
	me := fmt.Sprintf(`{"assignee_id": %q}`, e.Tenant.Human.ID)

	task, next := c.Must(200, "PATCH", base+"AP_CUTOFF", me, tag)
	if next != `"2"` {
		t.Errorf("assigned, ETag: got %s, want \"2\"", next)
	}
	wantFields(t, "assigned", task, map[string]any{"assignee_id": e.Tenant.Human.ID.String(), "close_id": id, "close_version": 2.0, "fiscal_year": 2026.0})
	if _, ok := task["fiscal_period"]; ok {
		t.Errorf("assigned: got fiscal_period %v, want none", task["fiscal_period"])
	}
	c.Problem(412, "request.version_mismatch", "PATCH", base+"AP_CUTOFF", me, tag)
	c.Problem(409, "closing.task_blocked", "POST", base+"TAX:complete", "", next)
	task, next = c.Must(200, "POST", base+"AP_CUTOFF:complete", `{"note": "Three adjustments posted"}`, next)
	wantFields(t, "completed", task, map[string]any{"status": "done", "overdue": false, "note": "Three adjustments posted",
		"completed_by": e.Tenant.Human.ID.String(), "completed_by_name": task["assignee_name"]})
	c.Problem(409, "closing.task_already_done", "POST", base+"AP_CUTOFF:complete", "", next)
	_, next = c.Must(200, "POST", base+"TAX:complete", "", next)
	task, next = c.Must(200, "POST", base+"AP_CUTOFF:reopen", `{"reason": "A late adjustment arrived"}`, next)
	wantFields(t, "reopened", task, map[string]any{"status": "open", "overdue": true, "note": nil, "completed_by": nil})
	c.Problem(409, "closing.task_not_done", "POST", base+"AP_CUTOFF:reopen", `{"reason": "Again"}`, next)
	if _, same := c.Must(200, "PATCH", base+"AP_CUTOFF", `{}`, next); same != next {
		t.Errorf("an empty patch: got ETag %s, want %s unchanged", same, next)
	}

	read, _ := c.Must(200, "GET", "/v1/year-closes/"+id, "", "")
	wantFields(t, "the close", read, map[string]any{"tasks_done": 1.0, "tasks_open": 2.0, "version": 5.0})
	var changes []string
	for _, ev := range apitest.Published(t, e.Tenant, outbox.Event{Context: "closing", Name: "year_close_changed"}) {
		changes = append(changes, fmt.Sprintf("%v %v %v %v", ev["change"], ev["task_code"], ev["reason"], ev["version"]))
	}
	wantLines(t, "events", changes, []string{"created <nil> <nil> 1", "task_assigned AP_CUTOFF <nil> 2", "task_completed AP_CUTOFF <nil> 3",
		"task_completed TAX <nil> 4", "task_reopened AP_CUTOFF A late adjustment arrived 5"})
	var audited string
	pgtest.Scan(t, `SELECT string_agg(action, ' ' ORDER BY seq) FROM audit.audit_event WHERE tenant_id = $1 AND object_type = 'closing.year_close'`,
		[]any{e.Tenant.ID}, &audited)
	if want := "closing.year_close.create closing.year_close.assign_task closing.year_close.complete_task closing.year_close.complete_task " +
		"closing.year_close.reopen_task"; audited != want {
		t.Errorf("audited: got %q, want %q", audited, want)
	}
}

// TestYearCloseTaskRefusals: a year close's task is assigned only to an
// active person who may act in its company; an unknown task or close, or a
// period close's id, is not found; a change needs If-Match; agents do not
// work the checklist.
func TestYearCloseTaskRefusals(t *testing.T) {
	e := fixture.NewWith(t, nil, withYearTemplate, closePermissions...)
	got, tag := openYear(t, e.Client, e.US, "")
	base := "/v1/year-closes/" + apitest.String(t, got, "id") + "/tasks/"
	e.Problem(422, "closing.assignee_invalid", "PATCH", base+"TAX", fmt.Sprintf(`{"assignee_id": %q}`, e.Tenant.Agent.ID), tag)
	e.Problem(422, "closing.assignee_invalid", "PATCH", base+"TAX", fmt.Sprintf(`{"assignee_id": %q}`, e.Reader(t, "reader of 2000", e.DE).ID), tag)
	p := e.Problem(404, "request.not_found", "POST", base+"NO_SUCH:complete", "", tag)
	if !strings.Contains(fmt.Sprint(p["detail"]), "Year close") {
		t.Errorf("an unknown task: got %v, want the year close named", p["detail"])
	}
	e.Problem(404, "request.not_found", "POST", "/v1/year-closes/"+uuid.NewString()+"/tasks/TAX:complete", "", tag)
	period, periodTag := open(t, e.Client, e.US, 1, "")
	e.Problem(404, "request.not_found", "POST", "/v1/year-closes/"+apitest.String(t, period, "id")+"/tasks/AP_CUTOFF:complete", "", periodTag)
	e.Problem(428, "request.precondition_required", "POST", base+"TAX:complete", "", "")
	e.As(e.Tenant.Agent).Problem(403, "identity.agent_not_allowed", "POST", base+"AP_CUTOFF:complete", "", tag)
	reader := e.As(e.Reader(t, "reader", e.DE))
	reader.Problem(404, "request.not_found", "POST", base+"AP_CUTOFF:complete", "", tag)
	reader.Problem(404, "request.not_found", "GET", "/v1/year-closes/"+apitest.String(t, got, "id"), "", "")
	e.Tenant.Revoke(t, "ledger.entry.read", e.Tenant.Human)
	e.Problem(403, "identity.permission_missing", "GET", "/v1/year-closes/"+apitest.String(t, got, "id"), "", "")
}

// TestListYearCloses: the list shows the year closes of the companies the
// caller may act in, filtered by company, fiscal year and status, a page at
// a time, by year end or creation.
func TestListYearCloses(t *testing.T) {
	e := fixture.NewWith(t, nil, withYearTemplate, closePermissions...)
	us, _ := openYear(t, e.Client, e.US, "")
	de, _ := openYear(t, e.Client, e.DE, "")
	line := func(page map[string]any) string {
		var out []string
		for _, x := range apitest.List(t, page, "items") {
			i := x.(map[string]any)
			out = append(out, fmt.Sprintf("%v %v %v %v/%v", i["company_code"], i["fiscal_year"], i["year_end"], i["tasks_done"], i["tasks_total"]))
		}
		return strings.Join(out, ", ")
	}
	for name, tc := range map[string]struct{ query, want string }{
		"every close":       {"", "1000 2026 2026-12-31 0/3, 2000 2026 2026-12-31 0/2"},
		"by company":        {"?legal_entity_id=" + e.DE.String(), "2000 2026 2026-12-31 0/2"},
		"another year":      {"?fiscal_year=2025", ""},
		"signed off":        {"?status=signed_off", ""},
		"by creation, open": {"?order_by=created_at%20desc&status=open", "2000 2026 2026-12-31 0/2, 1000 2026 2026-12-31 0/3"},
	} {
		t.Run(name, func(t *testing.T) {
			page, _ := e.Must(200, "GET", "/v1/year-closes"+tc.query, "", "")
			if got := line(page); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	first, _ := e.Must(200, "GET", "/v1/year-closes?page_size=1", "", "")
	next, _ := e.Must(200, "GET", "/v1/year-closes?page_size=1&page_token="+apitest.String(t, first, "next_page_token"), "", "")
	ids := fmt.Sprintf("%v %v", apitest.List(t, first, "items")[0].(map[string]any)["id"], apitest.List(t, next, "items")[0].(map[string]any)["id"])
	if want := fmt.Sprintf("%v %v", us["id"], de["id"]); ids != want && ids != fmt.Sprintf("%v %v", de["id"], us["id"]) {
		t.Errorf("two pages: got %s, want both closes", ids)
	}
	reader, _ := e.As(e.Reader(t, "reader of 1000", e.US)).Must(200, "GET", "/v1/year-closes", "", "")
	if got := line(reader); got != "1000 2026 2026-12-31 0/3" {
		t.Errorf("a reader of company 1000: got %q, want its close alone", got)
	}
	e.Problem(400, "request.invalid_order_by", "GET", "/v1/year-closes?order_by=period_end", "", "")
}

// TestYearCloseAfterUnlock: a signed-off year close (as T64m6 will sign it
// off) whose covered ledgers' periods of the year are all locked refuses a
// task action; once one of them is no longer locked, the action sets it
// open again first, audited and published as reopened.
func TestYearCloseAfterUnlock(t *testing.T) {
	e := fixture.NewWith(t, nil, withYearTemplate, closePermissions...)
	got, _ := openYear(t, e.Client, e.US, "")
	id := uuid.MustParse(apitest.String(t, got, "id"))
	base := "/v1/year-closes/" + id.String() + "/tasks/"
	for _, status := range []string{"closed", "locked"} {
		pgtest.Exec(t, `UPDATE ledger.period_status SET status = $3, version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
			AND fiscal_year = 2026 AND status <> $3`, e.Tenant.ID, e.US, status)
	}
	pgtest.Exec(t, `UPDATE closing.year_close SET status = 'signed_off', version = version + 1 WHERE id = $1`, id)
	tag := `"2"`
	p := e.Problem(409, "closing.close_signed_off", "POST", base+"AP_CUTOFF:complete", "", tag)
	if !strings.Contains(fmt.Sprint(p["detail"]), "Year close "+id.String()+" of 2026 is signed off") {
		t.Errorf("signed off: got %v, want the year close named", p["detail"])
	}
	// An approved unlock returns a period to closed (T32); here period 13.
	pgtest.Exec(t, `UPDATE ledger.period_status SET unlock_request_id = gen_random_uuid(), version = version + 1 WHERE tenant_id = $1
		AND legal_entity_id = $2 AND fiscal_year = 2026 AND period = 13`, e.Tenant.ID, e.US)
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', unlock_request_id = NULL, version = version + 1 WHERE tenant_id = $1
		AND legal_entity_id = $2 AND fiscal_year = 2026 AND period = 13`, e.Tenant.ID, e.US)
	task, _ := e.Must(200, "POST", base+"AP_CUTOFF:complete", "", tag)
	wantFields(t, "completed after the unlock", task, map[string]any{"status": "done", "close_version": 4.0})
	read, _ := e.Must(200, "GET", "/v1/year-closes/"+id.String(), "", "")
	wantFields(t, "the close", read, map[string]any{"status": "open"})
	events := apitest.Published(t, e.Tenant, outbox.Event{Context: "closing", Name: "year_close_changed"})
	if got := fmt.Sprintf("%v %v", events[len(events)-2]["change"], events[len(events)-1]["change"]); got != "reopened task_completed" {
		t.Errorf("events: got %s, want reopened, then task_completed", got)
	}
}

// TestYearCloseDatabaseFailures calls each handler of the year close and
// its tasks with each of its database statements failing in turn, and
// outside a unit of work.
func TestYearCloseDatabaseFailures(t *testing.T) {
	e := fixture.NewWith(t, nil, withYearTemplate, closePermissions...)
	got, tag := openYear(t, e.Client, e.DE, "")
	id := uuid.MustParse(apitest.String(t, got, "id"))
	_, tag = e.Must(200, "PATCH", "/v1/year-closes/"+id.String()+"/tasks/TAX", `{"assignee_id": "`+e.Tenant.Human.ID.String()+`"}`, tag)
	_, tag = e.Must(200, "POST", "/v1/year-closes/"+id.String()+"/tasks/AP_CUTOFF:complete", "", tag)
	h := endpoints(t)
	calls := map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreateYearClose(ctx, &gen.YearCloseCreate{LegalEntityID: e.US, FiscalYear: 2026}, gen.CreateYearCloseParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetYearClose(ctx, gen.GetYearCloseParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListYearCloses(ctx, gen.ListYearClosesParams{})
			return err
		},
		"assign": func(ctx context.Context) error {
			_, err := h.UpdateYearCloseTask(ctx, &gen.CloseTaskUpdate{AssigneeID: gen.NewOptNilUUID(e.Approver.ID)},
				gen.UpdateYearCloseTaskParams{ID: id, Code: "TAX", IfMatch: tag})
			return err
		},
		"complete": func(ctx context.Context) error {
			_, err := h.CompleteYearCloseTask(ctx, gen.OptCloseTaskComplete{}, gen.CompleteYearCloseTaskParams{ID: id, Code: "TAX", IfMatch: tag})
			return err
		},
		"reopen": func(ctx context.Context) error {
			_, err := h.ReopenYearCloseTask(ctx, &gen.ReasonInput{Reason: "again"}, gen.ReopenYearCloseTaskParams{ID: id, Code: "AP_CUTOFF", IfMatch: tag})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
		})
	}
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.ListYearCloses(ctx, gen.ListYearClosesParams{PageToken: gen.NewOptString("forged")})
		return err
	})
	if got := problem.From(err).Code; got != "request.invalid_page_token" {
		t.Errorf("a forged page token: got %s, want request.invalid_page_token", got)
	}
}

// TestYearCloseBrokenData: a year close's task whose assignee the identity
// context does not hold fails the response rather than showing it without
// a name.
func TestYearCloseBrokenData(t *testing.T) {
	e := fixture.NewWith(t, nil, withYearTemplate, closePermissions...)
	got, _ := openYear(t, e.Client, e.DE, "")
	id := uuid.MustParse(apitest.String(t, got, "id"))
	pgtest.Exec(t, `UPDATE closing.year_close_task SET assignee_id = $2, version = version + 1 WHERE close_id = $1 AND code = 'TAX'`, id, uuid.New())
	h := endpoints(t)
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.GetYearClose(ctx, gen.GetYearCloseParams{ID: id})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "not held by the identity context") {
		t.Errorf("an unknown assignee: got %v, want the identity context's failure", err)
	}
}
