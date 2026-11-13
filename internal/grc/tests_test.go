package grc_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// today is the database's date today.
func (e env) today(t *testing.T) time.Time {
	t.Helper()
	var d time.Time
	pgtest.Scan(t, `SELECT current_date`, nil, &d)
	return d
}

// problemCode is an error as "status code".
func problemCode(err error) string {
	p := problem.From(err)
	return fmt.Sprintf("%d %s", p.Status, p.Code)
}

// TestRecordATest: the result of testing a control is recorded with its
// date, its result, a note and who tested, and the control shows it as
// its last test without a new version; the audit record and
// ControlChanged carry the date and the result, never the note (T98a
// acceptance).
func TestRecordATest(t *testing.T) {
	e := setup(t)
	id, tag := e.record(t, "PAY-01", "Payments")
	path := "/v1/controls/" + id
	const note = "Run PR-12: release by the proposer refused."
	carla := e.As(e.carla)
	got, _ := carla.Must(201, "POST", path+"/tests", testOf(day(t, -1), "passed", note), "")
	test := apitest.String(t, got, "id")
	wantFields(t, got, map[string]any{"control_id": id, "control_code": "PAY-01", "tested_on": day(t, -1), "result": "passed", "note": note,
		"tested_by": e.carla.ID.String(), "tested_by_name": "Carla Controller"})
	if got["created_at"] == nil {
		t.Error("created_at: got none, want when the test was recorded")
	}
	read, after := e.Must(200, "GET", path, "", "")
	if after != tag {
		t.Errorf("the control's ETag after a test: got %s, want %s unchanged", after, tag)
	}
	wantFields(t, apitest.Object(t, read, "last_test"), map[string]any{"id": test, "control_id": id, "control_code": "PAY-01",
		"tested_on": day(t, -1), "result": "passed", "note": note, "tested_by": e.carla.ID.String(), "tested_by_name": "Carla Controller"})
	a := e.audited(t, id)
	wantAudit := fmt.Sprintf(`grc.control.test {"code": "PAY-01", "result": "passed", "test_id": %q, "tested_on": %q}`, test, day(t, -1))
	if len(a) != 2 || a[1] != wantAudit {
		t.Errorf("audit: got %v, want %s second", a, wantAudit)
	}
	events := e.changed(t)
	if len(events) != 2 {
		t.Fatalf("events: got %d, want 2", len(events))
	}
	wantFields(t, events[1], map[string]any{"control_id": id, "change": "tested", "version": float64(1), "status": "active"})
	wantFields(t, apitest.Object(t, events[1], "test"), map[string]any{"test_id": test, "tested_on": day(t, -1), "result": "passed",
		"tested_by": e.carla.ID.String()})
	apitest.NeverPublished(t, e.Tenant, "grc", "PR-12")
	for _, rec := range a {
		if strings.Contains(rec, "PR-12") {
			t.Errorf("audit record %s: got the note in it, want none", rec)
		}
	}
}

// TestLastTest: a control's last test is the one with the latest test
// date, and among the tests of one date the one recorded last; the list
// of its tests is in that order.
func TestLastTest(t *testing.T) {
	e := setup(t)
	id, _ := e.record(t, "PAY-01", "Payments")
	path := "/v1/controls/" + id
	last := func(t *testing.T) string {
		t.Helper()
		read, _ := e.Must(200, "GET", path, "", "")
		return fmt.Sprint(apitest.Object(t, read, "last_test")["note"])
	}
	e.Must(201, "POST", path+"/tests", testOf(day(t, -5), "failed", "first"), "")
	if got := last(t); got != "first" {
		t.Errorf("after one test: got %s, want first", got)
	}
	e.Must(201, "POST", path+"/tests", testOf(day(t, -9), "passed", "earlier, recorded later"), "")
	if got := last(t); got != "first" {
		t.Errorf("after a test of an earlier date: got %s, want first", got)
	}
	e.Must(201, "POST", path+"/tests", testOf(day(t, -5), "passed", "same date, recorded later"), "")
	if got := last(t); got != "same date, recorded later" {
		t.Errorf("after a second test of the date: got %s, want it", got)
	}
	e.Must(201, "POST", path+"/tests", testOf(day(t, 0), "failed", "today"), "")
	if got := last(t); got != "today" {
		t.Errorf("after a test dated today: got %s, want today", got)
	}
	page, _ := e.Must(200, "GET", path+"/tests", "", "")
	if got, want := column(objects(t, page, "items"), "note"), "today same date, recorded later first earlier, recorded later"; got != want {
		t.Errorf("the tests, the latest first: got %q, want %q", got, want)
	}
	page, _ = e.Must(200, "GET", path+"/tests?order_by=tested_on%20asc", "", "")
	if got, want := column(objects(t, page, "items"), "note"), "earlier, recorded later first same date, recorded later today"; got != want {
		t.Errorf("the tests, the earliest first: got %q, want %q", got, want)
	}
	page, _ = e.Must(200, "GET", path+"/tests?result=failed", "", "")
	if got, want := column(objects(t, page, "items"), "note"), "today first"; got != want {
		t.Errorf("the failed tests: got %q, want %q", got, want)
	}
	var notes []string
	token := ""
	for {
		page, _ = e.Must(200, "GET", path+"/tests?page_size=3"+token, "", "")
		notes = append(notes, column(objects(t, page, "items"), "note"))
		next, more := page["next_page_token"].(string)
		if !more {
			break
		}
		token = "&page_token=" + next
	}
	if got, want := strings.Join(notes, " | "), "today same date, recorded later first | earlier, recorded later"; got != want {
		t.Errorf("the tests in pages of three: got %q, want %q", got, want)
	}
	e.Problem(400, "request.invalid_order_by", "GET", path+"/tests?order_by=result", "", "")
	e.Problem(404, "request.not_found", "GET", "/v1/controls/"+uuid.Must(uuid.NewV7()).String()+"/tests", "", "")
}

// TestTestRefusals: a test dated after today, with an unknown result or
// a note of white space is refused naming the field; recording a test is
// a person's with the permission, never an agent's (T98a acceptance).
func TestTestRefusals(t *testing.T) {
	e := setup(t)
	id, _ := e.record(t, "PAY-01", "Payments")
	path := "/v1/controls/" + id + "/tests"
	for _, c := range []struct {
		name, body  string
		status      int
		code, field string
	}{
		{"dated tomorrow", testOf(day(t, 1), "passed", "Seen."), 422, "grc.test_in_future", "tested_on"},
		{"with an unknown result", testOf(day(t, 0), "partly", "Seen."), 422, "request.validation_failed", "result"},
		{"with a note of white space", testOf(day(t, 0), "passed", "  "), 422, "request.validation_failed", "note"},
		{"without a date", `{"result": "passed", "note": "Seen."}`, 400, "request.invalid_body", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := e.in(t).Problem(c.status, c.code, "POST", path, c.body, "")
			if c.field == "" {
				return
			}
			errs := objects(t, m, "errors")
			if len(errs) == 0 || errs[0]["field"] != c.field {
				t.Errorf("field: got %v, want %s", errs, c.field)
			}
		})
	}
	e.Problem(404, "request.not_found", "POST", "/v1/controls/"+uuid.Must(uuid.NewV7()).String()+"/tests", testOf(day(t, 0), "passed", "Seen."), "")
	t.Run("by an agent", func(t *testing.T) {
		e.As(e.Tenant.Agent).In(t).Problem(403, "identity.agent_not_allowed", "POST", path, testOf(day(t, 0), "passed", "Seen."), "")
	})
	t.Run("by a person without the permission", func(t *testing.T) {
		e.Tenant.Revoke(t, "grc.control.test", e.Tenant.Human)
		e.in(t).Problem(403, "identity.permission_missing", "POST", path, testOf(day(t, 0), "passed", "Seen."), "")
	})
	page, _ := e.Must(200, "GET", path, "", "")
	if n := len(apitest.List(t, page, "items")); n != 0 {
		t.Errorf("tests after the refusals: got %d, want none", n)
	}
}

// TestAgentRecordsAControl: an agent with the permission records and
// changes controls and reads the library; only recording a test is
// closed to it.
func TestAgentRecordsAControl(t *testing.T) {
	e := setup(t)
	agent := e.As(e.Tenant.Agent)
	c, tag := agent.Must(201, "POST", "/v1/controls", controlOf("PAY-01", "Payments", e.Tenant.Human.ID), "")
	path := "/v1/controls/" + apitest.String(t, c, "id")
	agent.Must(200, "PATCH", path, `{"frequency": "daily"}`, tag)
	page, _ := agent.Must(200, "GET", "/v1/controls", "", "")
	if got := column(objects(t, page, "items"), "code"); got != "PAY-01" {
		t.Errorf("the library an agent reads: got %q, want PAY-01", got)
	}
}

// TestLibrary: the library lists the controls by code with each one's
// last test, filtered by process, status, owner and the last test's
// result, in pages (T98a acceptance).
func TestLibrary(t *testing.T) {
	e := setup(t)
	// Recorded against their codes' order, so the order by code is seen.
	ids := map[string]string{}
	for _, c := range []struct {
		code, process string
		owner         uuid.UUID
	}{
		{"REC-02", "Receivables", e.carla.ID}, {"PAY-01", "Payments", e.Tenant.Human.ID}, {"PAY-03", "Payments", e.carla.ID},
		{"GL-01", "General ledger", e.Tenant.Human.ID}, {"PAY-02", "Payments", e.Tenant.Human.ID},
	} {
		got, _ := e.Must(201, "POST", "/v1/controls", controlOf(c.code, c.process, c.owner), "")
		ids[c.code] = apitest.String(t, got, "id")
	}
	test := func(code, on, result string) {
		e.Must(201, "POST", "/v1/controls/"+ids[code]+"/tests", testOf(on, result, code+" "+result), "")
	}
	test("PAY-01", day(t, -3), "failed")
	test("PAY-01", day(t, -1), "passed") // the last: passed
	test("PAY-02", day(t, -1), "passed")
	test("PAY-02", day(t, 0), "failed") // the last: failed
	test("REC-02", day(t, -2), "failed")
	_, tag := e.Must(200, "GET", "/v1/controls/"+ids["PAY-03"], "", "")
	e.Must(200, "POST", "/v1/controls/"+ids["PAY-03"]+":retire", "", tag)

	for _, c := range []struct{ name, query, want string }{
		{"the whole library by code", "", "GL-01 PAY-01 PAY-02 PAY-03 REC-02"},
		{"by code, descending", "?order_by=code%20desc", "REC-02 PAY-03 PAY-02 PAY-01 GL-01"},
		{"one process", "?process=Payments", "PAY-01 PAY-02 PAY-03"},
		{"a process of two words", "?process=General%20ledger", "GL-01"},
		{"a process nothing has", "?process=payments", ""},
		{"last tested as passed", "?last_result=passed", "PAY-01"},
		{"last tested as failed", "?last_result=failed", "PAY-02 REC-02"},
		{"never tested", "?last_result=none", "GL-01 PAY-03"},
		{"a process and a last result", "?process=Payments&last_result=failed", "PAY-02"},
		{"the active controls", "?status=active", "GL-01 PAY-01 PAY-02 REC-02"},
		{"the retired controls", "?status=retired", "PAY-03"},
		{"one owner's", "?owner_id=" + e.carla.ID.String(), "PAY-03 REC-02"},
	} {
		t.Run(c.name, func(t *testing.T) {
			page, _ := e.in(t).Must(200, "GET", "/v1/controls"+c.query, "", "")
			if got := column(objects(t, page, "items"), "code"); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
	t.Run("each with its owner's name and its last test", func(t *testing.T) {
		page, _ := e.in(t).Must(200, "GET", "/v1/controls?process=Payments", "", "")
		var got []string
		for _, c := range objects(t, page, "items") {
			last := "never tested"
			if _, has := c["last_test"]; has {
				lt := apitest.Object(t, c, "last_test")
				last = fmt.Sprintf("%v by %v", lt["note"], lt["tested_by_name"])
			}
			got = append(got, fmt.Sprintf("%v of %v: %s", c["code"], c["owner_name"], last))
		}
		want := "PAY-01 of human: PAY-01 passed by human; PAY-02 of human: PAY-02 failed by human; PAY-03 of Carla Controller: never tested"
		if g := strings.Join(got, "; "); g != want {
			t.Errorf("got %q, want %q", g, want)
		}
	})
	t.Run("in pages of two", func(t *testing.T) {
		e := e.in(t)
		var pages []string
		token := ""
		for {
			page, _ := e.Must(200, "GET", "/v1/controls?page_size=2"+token, "", "")
			pages = append(pages, column(objects(t, page, "items"), "code"))
			next, more := page["next_page_token"].(string)
			if !more {
				break
			}
			token = "&page_token=" + next
		}
		if got, want := strings.Join(pages, " | "), "GL-01 PAY-01 | PAY-02 PAY-03 | REC-02"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	e.Problem(400, "request.invalid_order_by", "GET", "/v1/controls?order_by=process", "", "")
	t.Run("without the permission", func(t *testing.T) {
		e.Tenant.Revoke(t, "grc.control.read", e.Tenant.Human)
		e.in(t).Problem(403, "identity.permission_missing", "GET", "/v1/controls", "", "")
	})
}
