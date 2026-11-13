package grc_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/grc"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// perms are what the tests' person holds: reading the library, recording
// controls and recording tests, and what the matrix and the links also
// need: reading access data and the configuration.
var perms = []string{"grc.control.read", "grc.control.write", "grc.control.test", "identity.access.read", "config.version.read"}

// env is a configured tenant whose person (named "human") is the
// compliance officer; carla is another active person.
type env struct {
	fixture.Env
	carla pgtest.Actor
}

func setup(t *testing.T) env {
	t.Helper()
	f := fixture.New(t, perms...)
	return env{Env: f, carla: f.Person(t, "Carla Controller")}
}

// in is the tenant reporting to a subtest.
func (e env) in(t *testing.T) env {
	t.Helper()
	e.Client = e.In(t)
	return e
}

// controlOf is the JSON of a control with the code, owned by owner.
func controlOf(code, process string, owner uuid.UUID) string {
	return fmt.Sprintf(`{"code": %q, "objective": "A payment run is released by another person than the one who proposed it.",
		"process": %q, "owner_id": %q, "frequency": "continuous", "nature": "automated"}`, code, process, owner)
}

// record records a control and returns its id and ETag.
func (e env) record(t *testing.T, code, process string) (string, string) {
	t.Helper()
	c, tag := e.Must(201, "POST", "/v1/controls", controlOf(code, process, e.Tenant.Human.ID), "")
	return apitest.String(t, c, "id"), tag
}

// testOf is the JSON of a test.
func testOf(on, result, note string) string {
	return fmt.Sprintf(`{"tested_on": %q, "result": %q, "note": %q}`, on, result, note)
}

// day is the database's date today plus days, as the API gives a date.
func day(t *testing.T, days int) string {
	t.Helper()
	var d string
	pgtest.Scan(t, `SELECT (current_date + $1::int)::text`, []any{days}, &d)
	return d
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

// column is one field of each object, joined by spaces.
func column(items []map[string]any, field string) string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = fmt.Sprint(it[field])
	}
	return strings.Join(out, " ")
}

// audited are the audit records of a control as "action detail", in the
// order they were written.
func (e env) audited(t *testing.T, id string) []string {
	t.Helper()
	var out []string
	pgtest.Scan(t, `SELECT coalesce(array_agg(action || ' ' || detail::text ORDER BY seq), '{}') FROM audit.audit_event
		WHERE tenant_id = $1 AND object_type = 'grc.control' AND object_id = $2`, []any{e.Tenant.ID, id}, &out)
	return out
}

// changed are the tenant's ControlChanged events in the order they were
// written.
func (e env) changed(t *testing.T) []map[string]any {
	t.Helper()
	return apitest.Published(t, e.Tenant, outbox.Event{Context: "grc", Name: "control_changed"})
}

// wantFields checks the fields of an answer one by one.
func wantFields(t *testing.T, got map[string]any, want map[string]any) {
	t.Helper()
	for f, w := range want {
		if got[f] != w {
			t.Errorf("%s: got %v, want %v", f, got[f], w)
		}
	}
}

// TestRecordAndRead: a control is recorded with its code, objective,
// process, owner, frequency and nature, active, and reads back with its
// owner's name and no last test; the audit record and ControlChanged
// name it (T98a acceptance).
func TestRecordAndRead(t *testing.T) {
	e := setup(t)
	req := apitest.Req{Method: "POST", Path: "/v1/controls", Body: controlOf("PAY-01", "Payments", e.carla.ID), Tenant: e.Tenant.ID, As: e.Tenant.Human,
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
	if loc := rec.Header().Get("Location"); loc != "/v1/controls/"+id {
		t.Errorf("Location: got %s, want /v1/controls/%s", loc, id)
	}
	want := map[string]any{"code": "PAY-01", "process": "Payments", "owner_id": e.carla.ID.String(), "owner_name": "Carla Controller",
		"frequency": "continuous", "nature": "automated", "status": "active", "version": float64(1),
		"objective": "A payment run is released by another person than the one who proposed it."}
	wantFields(t, got, want)
	if _, has := got["last_test"]; has {
		t.Errorf("last_test: got %v, want it absent for a control never tested", got["last_test"])
	}
	read, tag := e.Must(200, "GET", "/v1/controls/"+id, "", "")
	wantFields(t, read, want)
	if tag != `"1"` {
		t.Errorf("ETag of the read: got %s, want \"1\"", tag)
	}
	if replay := e.API.Do(req); replay.Code != 201 || apitest.String(t, apitest.JSON(t, replay), "id") != id {
		t.Errorf("a retry with the same key: got %d %s, want the control recorded", replay.Code, replay.Body)
	}
	wantAudit := fmt.Sprintf(`grc.control.create {"code": "PAY-01", "nature": "automated", "process": "Payments", "owner_id": %q, "frequency": "continuous"}`, e.carla.ID)
	if a := e.audited(t, id); len(a) != 1 || a[0] != wantAudit {
		t.Errorf("audit: got %v, want %s", a, wantAudit)
	}
	events := e.changed(t)
	if len(events) != 1 {
		t.Fatalf("events: got %d, want 1", len(events))
	}
	wantFields(t, events[0], map[string]any{"control_id": id, "code": "PAY-01", "process": "Payments", "owner_id": e.carla.ID.String(),
		"frequency": "continuous", "nature": "automated", "status": "active", "change": "created", "test": nil, "link": nil, "version": float64(1)})
}

// TestControlRefusals: what a control may not say is refused naming the
// field, and a code is one control's whatever its case.
func TestControlRefusals(t *testing.T) {
	e := setup(t)
	e.record(t, "PAY-01", "Payments")
	blocked := e.Person(t, "Bo Blocked")
	pgtest.Exec(t, `UPDATE identity.principal SET status = 'blocked', version = version + 1 WHERE id = $1`, blocked.ID)
	body := func(code, objective, process string, owner uuid.UUID, frequency, nature string) string {
		return fmt.Sprintf(`{"code": %q, "objective": %q, "process": %q, "owner_id": %q, "frequency": %q, "nature": %q}`,
			code, objective, process, owner, frequency, nature)
	}
	me := e.Tenant.Human.ID
	for _, c := range []struct {
		name, body string
		status     int
		code       string
		field      string // of the first field error, empty: none
	}{
		{"a code taken", body("PAY-01", "x", "Payments", me, "daily", "manual"), 409, "grc.control_code_taken", ""},
		{"a code taken in another case", body("pay-01", "x", "Payments", me, "daily", "manual"), 409, "grc.control_code_taken", ""},
		{"a code with a space", body("PAY 02", "x", "Payments", me, "daily", "manual"), 400, "request.invalid_body", ""},
		{"an owner the tenant does not have", body("PAY-02", "x", "Payments", uuid.Must(uuid.NewV7()), "daily", "manual"), 422, "request.reference_not_found", "owner_id"},
		{"an agent as the owner", body("PAY-02", "x", "Payments", e.Tenant.Agent.ID, "daily", "manual"), 422, "grc.invalid_owner", "owner_id"},
		{"a blocked person as the owner", body("PAY-02", "x", "Payments", blocked.ID, "daily", "manual"), 422, "grc.invalid_owner", "owner_id"},
		{"an objective of white space", body("PAY-02", "  ", "Payments", me, "daily", "manual"), 422, "request.validation_failed", "objective"},
		{"a process of white space", body("PAY-02", "x", "  ", me, "daily", "manual"), 422, "request.validation_failed", "process"},
		{"a process with white space around it", body("PAY-02", "x", " Payments", me, "daily", "manual"), 422, "grc.invalid_process", "process"},
		{"an unknown frequency", body("PAY-02", "x", "Payments", me, "hourly", "manual"), 422, "request.validation_failed", "frequency"},
		{"an unknown nature", body("PAY-02", "x", "Payments", me, "daily", "hybrid"), 422, "request.validation_failed", "nature"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := e.in(t).Problem(c.status, c.code, "POST", "/v1/controls", c.body, "")
			if c.field == "" {
				return
			}
			errs := objects(t, m, "errors")
			if len(errs) == 0 || errs[0]["field"] != c.field {
				t.Errorf("field: got %v, want %s", errs, c.field)
			}
		})
	}
	page, _ := e.Must(200, "GET", "/v1/controls", "", "")
	if got := column(objects(t, page, "items"), "code"); got != "PAY-01" {
		t.Errorf("the library after the refusals: got %q, want PAY-01", got)
	}
}

// TestChange: an active control's texts, owner, frequency and nature
// change with the caller's version; the audit record names the fields
// changed and the code stays.
func TestChange(t *testing.T) {
	e := setup(t)
	id, tag := e.record(t, "PAY-01", "Payments")
	path := "/v1/controls/" + id
	got, next := e.Must(200, "PATCH", path, fmt.Sprintf(`{"frequency": "monthly", "nature": "manual", "owner_id": %q, "process": "Treasury",
		"objective": "Reviewed monthly."}`, e.carla.ID), tag)
	wantFields(t, got, map[string]any{"code": "PAY-01", "frequency": "monthly", "nature": "manual", "owner_id": e.carla.ID.String(),
		"owner_name": "Carla Controller", "process": "Treasury", "objective": "Reviewed monthly.", "version": float64(2), "status": "active"})
	if next != `"2"` {
		t.Errorf("ETag after the change: got %s, want \"2\"", next)
	}
	a := e.audited(t, id)
	if want := `grc.control.change {"code": "PAY-01", "fields": ["objective", "process", "owner_id", "frequency", "nature"]}`; len(a) != 2 || a[1] != want {
		t.Errorf("audit: got %v, want %s second", a, want)
	}
	events := e.changed(t)
	if len(events) != 2 {
		t.Fatalf("events: got %d, want 2", len(events))
	}
	wantFields(t, events[1], map[string]any{"change": "changed", "version": float64(2), "frequency": "monthly", "owner_id": e.carla.ID.String(), "test": nil})

	for _, c := range []struct {
		name, body, tag string
		status          int
		code            string
	}{
		{"with a stale version", `{"frequency": "daily"}`, tag, 412, "request.version_mismatch"},
		{"to what it already says", `{"frequency": "monthly", "process": "Treasury"}`, next, 422, "grc.nothing_to_change"},
		{"of nothing", `{}`, next, 422, "grc.nothing_to_change"},
		{"of the code", `{"code": "PAY-02"}`, next, 400, "request.invalid_body"},
		{"to an agent as the owner", fmt.Sprintf(`{"owner_id": %q}`, e.Tenant.Agent.ID), next, 422, "grc.invalid_owner"},
		{"to an unknown owner", fmt.Sprintf(`{"owner_id": %q}`, uuid.Must(uuid.NewV7())), next, 422, "request.reference_not_found"},
		{"to an objective of white space", `{"objective": " "}`, next, 422, "request.validation_failed"},
		{"to a process with white space around it", `{"process": "Treasury "}`, next, 422, "grc.invalid_process"},
		{"to an unknown frequency", `{"frequency": "hourly"}`, next, 422, "request.validation_failed"},
		{"to an unknown nature", `{"nature": "hybrid"}`, next, 422, "request.validation_failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e.in(t).Problem(c.status, c.code, "PATCH", path, c.body, c.tag)
		})
	}
	if read, _ := e.Must(200, "GET", path, "", ""); read["version"] != float64(2) {
		t.Errorf("version after the refused changes: got %v, want 2", read["version"])
	}
	e.Problem(404, "request.not_found", "PATCH", "/v1/controls/"+uuid.Must(uuid.NewV7()).String(), `{"frequency": "daily"}`, `"1"`)
}

// TestChangeKeepsAnOwnerNoLongerActive: a change that leaves the owner
// as it is does not check the owner again, so a control whose owner was
// blocked since can still be changed, to another owner too.
func TestChangeKeepsAnOwnerNoLongerActive(t *testing.T) {
	e := setup(t)
	bo := e.Person(t, "Bo Blocked")
	c, tag := e.Must(201, "POST", "/v1/controls", controlOf("PAY-01", "Payments", bo.ID), "")
	path := "/v1/controls/" + apitest.String(t, c, "id")
	pgtest.Exec(t, `UPDATE identity.principal SET status = 'blocked', version = version + 1 WHERE id = $1`, bo.ID)
	got, tag := e.Must(200, "PATCH", path, `{"frequency": "weekly"}`, tag)
	wantFields(t, got, map[string]any{"frequency": "weekly", "owner_id": bo.ID.String(), "owner_name": "Bo Blocked"})
	e.Problem(422, "grc.nothing_to_change", "PATCH", path, fmt.Sprintf(`{"owner_id": %q}`, bo.ID), tag)
	got, _ = e.Must(200, "PATCH", path, fmt.Sprintf(`{"owner_id": %q}`, e.carla.ID), tag)
	wantFields(t, got, map[string]any{"owner_name": "Carla Controller", "version": float64(3)})
}

// TestRetire: a control is retired once, with the caller's version; a
// retired control changes no more and takes no test, and is still read.
func TestRetire(t *testing.T) {
	e := setup(t)
	id, tag := e.record(t, "PAY-01", "Payments")
	path := "/v1/controls/" + id
	e.Must(201, "POST", path+"/tests", testOf(day(t, 0), "passed", "Seen in run 12."), "")
	e.Problem(412, "request.version_mismatch", "POST", path+":retire", "", `"7"`)
	got, next := e.Must(200, "POST", path+":retire", "", tag)
	wantFields(t, got, map[string]any{"status": "retired", "version": float64(2), "code": "PAY-01"})
	if next != `"2"` {
		t.Errorf("ETag after retiring: got %s, want \"2\"", next)
	}
	for _, c := range []struct{ name, method, path, body string }{
		{"retired again", "POST", path + ":retire", ""},
		{"changed", "PATCH", path, `{"frequency": "daily"}`},
		{"tested", "POST", path + "/tests", testOf(day(t, 0), "failed", "Seen in run 13.")},
	} {
		t.Run(c.name, func(t *testing.T) {
			e.in(t).Problem(409, "grc.control_retired", c.method, c.path, c.body, next)
		})
	}
	read, _ := e.Must(200, "GET", path, "", "")
	wantFields(t, read, map[string]any{"status": "retired", "version": float64(2)})
	if last := apitest.Object(t, read, "last_test"); last["result"] != "passed" {
		t.Errorf("the retired control's last test: got %v, want the one recorded before", last)
	}
	tests, _ := e.Must(200, "GET", path+"/tests", "", "")
	if n := len(apitest.List(t, tests, "items")); n != 1 {
		t.Errorf("the retired control's tests: got %d, want 1", n)
	}
	e.Problem(409, "grc.control_code_taken", "POST", "/v1/controls", controlOf("PAY-01", "Payments", e.Tenant.Human.ID), "")
	a := e.audited(t, id)
	if want := `grc.control.retire {"code": "PAY-01"}`; len(a) != 3 || a[2] != want {
		t.Errorf("audit: got %v, want %s third", a, want)
	}
	events := e.changed(t)
	if len(events) != 3 {
		t.Fatalf("events: got %d, want 3", len(events))
	}
	wantFields(t, events[2], map[string]any{"change": "retired", "status": "retired", "version": float64(2), "test": nil})
	e.Problem(404, "request.not_found", "POST", "/v1/controls/"+uuid.Must(uuid.NewV7()).String()+":retire", "", `"1"`)
}

// TestTestWaitsForARetirement: recording a test locks the control, so a
// test started while the control is being retired waits and is then
// refused, never recorded on the retired control.
func TestTestWaitsForARetirement(t *testing.T) {
	e := setup(t)
	id, _ := e.record(t, "PAY-01", "Payments")
	control := uuid.MustParse(id)
	retired, tested := apitest.Serialized(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := grc.Retire(ctx, control, func(int32) error { return nil })
		return err
	}, func(ctx context.Context) error {
		_, err := grc.RecordTest(ctx, control, grc.NewTest{TestedOn: e.today(t), Result: "passed", Note: "Seen."})
		return err
	})
	if retired != nil {
		t.Fatalf("the retirement: got %v, want it done", retired)
	}
	if got := problemCode(tested); got != "409 grc.control_retired" {
		t.Errorf("the test that waited: got %s, want 409 grc.control_retired", got)
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM grc.control_test WHERE control_id = $1`, []any{control}, &n)
	if n != 0 {
		t.Errorf("tests of the retired control: got %d, want none", n)
	}
}

// TestOtherTenant: a tenant reads and changes its own library alone.
func TestOtherTenant(t *testing.T) {
	e := setup(t)
	id, tag := e.record(t, "PAY-01", "Payments")
	other := setup(t)
	other.Problem(404, "request.not_found", "GET", "/v1/controls/"+id, "", "")
	other.Problem(404, "request.not_found", "PATCH", "/v1/controls/"+id, `{"frequency": "daily"}`, tag)
	other.Problem(404, "request.not_found", "POST", "/v1/controls/"+id+"/tests", testOf(day(t, 0), "passed", "Seen."), "")
	other.Problem(404, "request.not_found", "GET", "/v1/controls/"+id+"/tests", "", "")
	page, _ := other.Must(200, "GET", "/v1/controls", "", "")
	if n := len(apitest.List(t, page, "items")); n != 0 {
		t.Errorf("the other tenant's library: got %d controls, want none", n)
	}
	// The same code is free in the other tenant.
	other.record(t, "PAY-01", "Payments")
}
