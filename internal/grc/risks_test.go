package grc_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/grc"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// riskOf is the JSON of a risk of the category, owned by owner, rated
// likelihood by impact and mitigated by the controls.
func riskOf(title, category string, owner uuid.UUID, likelihood, impact int, controls ...string) string {
	ids := make([]string, len(controls))
	for i, c := range controls {
		ids[i] = fmt.Sprintf("%q", c)
	}
	return fmt.Sprintf(`{"title": %q, "category": %q, "owner_id": %q, "likelihood": %d, "impact": %d, "control_ids": [%s]}`,
		title, category, owner, likelihood, impact, strings.Join(ids, ", "))
}

// risk records a risk owned by carla and returns its id and ETag.
func (e env) risk(t *testing.T, title, category string, likelihood, impact int, controls ...string) (string, string) {
	t.Helper()
	r, tag := e.Must(201, "POST", "/v1/risks", riskOf(title, category, e.carla.ID, likelihood, impact, controls...), "")
	return apitest.String(t, r, "id"), tag
}

// me is the name of the tenant's person, the caller of the tests.
func (e env) me(t *testing.T) string {
	t.Helper()
	var name string
	pgtest.Scan(t, `SELECT display_name FROM identity.principal WHERE id = $1`, []any{e.Tenant.Human.ID}, &name)
	return name
}

// riskAudited are the audit actions of a risk, in the order they were
// written.
func (e env) riskAudited(t *testing.T, id string) []string {
	t.Helper()
	var out []string
	pgtest.Scan(t, `SELECT coalesce(array_agg(action ORDER BY seq), '{}') FROM audit.audit_event
		WHERE tenant_id = $1 AND object_type = 'grc.risk' AND object_id = $2`, []any{e.Tenant.ID, id}, &out)
	return out
}

// riskEvents are the tenant's RiskChanged events in the order they were
// written.
func (e env) riskEvents(t *testing.T) []map[string]any {
	t.Helper()
	return apitest.Published(t, e.Tenant, outbox.Event{Context: "grc", Name: "risk_changed"})
}

// controlsOf are a risk's controls as "code status", by code.
func controlsOf(t *testing.T, r map[string]any) string {
	t.Helper()
	cs := objects(t, r, "controls")
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = fmt.Sprint(c["code"], " ", c["status"])
	}
	return strings.Join(out, ", ")
}

// TestRecordRisk: a risk is recorded open with its first rating by the
// caller and the controls that mitigate it, and reads back with its
// owner's and rater's names and its score; the audit record and
// RiskChanged name it (T98c acceptance).
func TestRecordRisk(t *testing.T) {
	e := setup(t)
	pay, _ := e.record(t, "PAY-02", "Payments")
	dup, _ := e.record(t, "PAY-01", "Payments")
	req := apitest.Req{Method: "POST", Path: "/v1/risks", Body: riskOf("Duplicate payments", "Payments", e.carla.ID, 2, 4, pay, dup),
		Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()}
	rec := e.API.Do(req)
	if rec.Code != 201 {
		t.Fatalf("status: got %d, want 201: %s", rec.Code, rec.Body)
	}
	got := apitest.JSON(t, rec)
	id := apitest.String(t, got, "id")
	if tag := rec.Header().Get("ETag"); tag != `"1"` {
		t.Errorf("ETag: got %s, want \"1\"", tag)
	}
	if loc := rec.Header().Get("Location"); loc != "/v1/risks/"+id {
		t.Errorf("Location: got %s, want /v1/risks/%s", loc, id)
	}
	want := map[string]any{"title": "Duplicate payments", "category": "Payments", "owner_id": e.carla.ID.String(), "owner_name": "Carla Controller",
		"likelihood": float64(2), "impact": float64(4), "score": float64(8), "rated_by": e.Tenant.Human.ID.String(),
		"rated_by_name": e.me(t), "status": "open", "version": float64(1)}
	wantFields(t, got, want)
	if c := controlsOf(t, got); c != "PAY-01 active, PAY-02 active" {
		t.Errorf("controls: got %q, want PAY-01 active, PAY-02 active (by code)", c)
	}
	for _, f := range []string{"close_reason", "closed_at", "closed_by", "closed_by_name"} {
		if _, has := got[f]; has {
			t.Errorf("%s: got %v, want it absent on an open risk", f, got[f])
		}
	}
	read, tag := e.Must(200, "GET", "/v1/risks/"+id, "", "")
	wantFields(t, read, want)
	if tag != `"1"` {
		t.Errorf("ETag of the read: got %s, want \"1\"", tag)
	}
	if replay := e.API.Do(req); replay.Code != 201 || apitest.String(t, apitest.JSON(t, replay), "id") != id {
		t.Errorf("a retry with the same key: got %d %s, want the risk recorded", replay.Code, replay.Body)
	}
	if a := e.riskAudited(t, id); strings.Join(a, " ") != "grc.risk.create" {
		t.Errorf("audit: got %v, want grc.risk.create", a)
	}
	events := e.riskEvents(t)
	if len(events) != 1 {
		t.Fatalf("events: got %d, want 1", len(events))
	}
	wantFields(t, events[0], map[string]any{"risk_id": id, "title": "Duplicate payments", "change": "created", "score": float64(8), "status": "open",
		"version": float64(1)})
	if r, ok := events[0]["rating"].(map[string]any); !ok || r["score"] != float64(8) || r["rated_by"] != e.Tenant.Human.ID.String() {
		t.Errorf("the event's rating: got %v, want the first rating, 8, by the caller", events[0]["rating"])
	}
	if ids := fmt.Sprint(events[0]["control_ids"]); ids != fmt.Sprint([]any{dup, pay}) {
		t.Errorf("the event's controls: got %s, want %v by code", ids, []string{dup, pay})
	}
	ratings, _ := e.Must(200, "GET", "/v1/risks/"+id+"/ratings", "", "")
	if r := objects(t, ratings, "items"); len(r) != 1 || r[0]["score"] != float64(8) || r[0]["rated_by_name"] != e.me(t) {
		t.Errorf("ratings: got %v, want the first rating", r)
	}
}

// TestRiskRefusals: what a risk may not say is refused naming the
// field, and nothing is recorded.
func TestRiskRefusals(t *testing.T) {
	e := setup(t)
	active, _ := e.record(t, "PAY-01", "Payments")
	retired, tag := e.record(t, "PAY-02", "Payments")
	e.Must(200, "POST", "/v1/controls/"+retired+":retire", "", tag)
	me := e.Tenant.Human.ID
	unknown := uuid.Must(uuid.NewV7()).String()
	for _, c := range []struct {
		name, body string
		status     int
		code       string
		field      string // of the first field error, empty: none
	}{
		{"a title of white space", riskOf(" ", "Payments", me, 1, 1), 422, "request.validation_failed", "title"},
		{"a title with white space around it", riskOf("Fraud ", "Payments", me, 1, 1), 422, "grc.invalid_text", "title"},
		{"a category with white space around it", riskOf("Fraud", " Payments", me, 1, 1), 422, "grc.invalid_text", "category"},
		{"an owner the tenant does not have", riskOf("Fraud", "Payments", uuid.Must(uuid.NewV7()), 1, 1), 422, "request.reference_not_found", "owner_id"},
		{"an agent as the owner", riskOf("Fraud", "Payments", e.Tenant.Agent.ID, 1, 1), 422, "grc.invalid_owner", "owner_id"},
		{"a likelihood of 6", riskOf("Fraud", "Payments", me, 6, 1), 400, "request.invalid_body", ""},
		{"an impact of 0", riskOf("Fraud", "Payments", me, 1, 0), 400, "request.invalid_body", ""},
		{"a control the library does not have", riskOf("Fraud", "Payments", me, 1, 1, active, unknown), 422, "request.reference_not_found", "control_ids[1]"},
		{"a retired control", riskOf("Fraud", "Payments", me, 1, 1, retired), 422, "grc.invalid_control", "control_ids[0]"},
		{"a control named twice", riskOf("Fraud", "Payments", me, 1, 1, active, active), 422, "grc.invalid_control", "control_ids[1]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := e.in(t).Problem(c.status, c.code, "POST", "/v1/risks", c.body, "")
			if c.field == "" {
				return
			}
			if errs := objects(t, m, "errors"); len(errs) == 0 || errs[0]["field"] != c.field {
				t.Errorf("field: got %v, want %s", errs, c.field)
			}
		})
	}
	page, _ := e.Must(200, "GET", "/v1/risks", "", "")
	if n := len(objects(t, page, "items")); n != 0 {
		t.Errorf("the register after the refusals: got %d risks, want none", n)
	}
	e.Problem(404, "request.not_found", "GET", "/v1/risks/"+unknown, "", "")
	e.Problem(404, "request.not_found", "GET", "/v1/risks/"+unknown+"/ratings", "", "")
	e.Problem(404, "request.not_found", "POST", "/v1/risks/"+unknown+"/ratings", `{"likelihood": 1, "impact": 1}`, "")
}

// TestRateRisk: a rating is kept with who rated and when, becomes the
// risk's at the next version, and the ratings read the latest first; the
// score is the latest rating's likelihood times its impact.
func TestRateRisk(t *testing.T) {
	e := setup(t)
	id, _ := e.risk(t, "Duplicate payments", "Payments", 2, 4)
	path := "/v1/risks/" + id
	rating, _ := e.Must(201, "POST", path+"/ratings", `{"likelihood": 1, "impact": 4}`, "")
	wantFields(t, rating, map[string]any{"risk_id": id, "likelihood": float64(1), "impact": float64(4), "score": float64(4),
		"rated_by": e.Tenant.Human.ID.String(), "rated_by_name": e.me(t)})
	carla := e.As(e.carla)
	carla.Must(201, "POST", path+"/ratings", `{"likelihood": 3, "impact": 5}`, "")
	read, tag := e.Must(200, "GET", path, "", "")
	wantFields(t, read, map[string]any{"likelihood": float64(3), "impact": float64(5), "score": float64(15), "version": float64(3),
		"rated_by": e.carla.ID.String(), "rated_by_name": "Carla Controller"})
	if tag != `"3"` {
		t.Errorf("ETag after two ratings: got %s, want \"3\"", tag)
	}
	first, _ := e.Must(200, "GET", path+"/ratings?page_size=2", "", "")
	items := objects(t, first, "items")
	if got := column(items, "score"); got != "15 4" {
		t.Errorf("ratings, the latest first: got %s, want 15 4", got)
	}
	if got := column(items, "rated_by_name"); got != "Carla Controller "+e.me(t) {
		t.Errorf("raters: got %s, want Carla Controller then %s", got, e.me(t))
	}
	rest, _ := e.Must(200, "GET", path+"/ratings?page_size=2&page_token="+apitest.String(t, first, "next_page_token"), "", "")
	if got := column(objects(t, rest, "items"), "score"); got != "8" {
		t.Errorf("the second page: got %s, want the first rating, 8", got)
	}
	if a := e.riskAudited(t, id); strings.Join(a, " ") != "grc.risk.create grc.risk.rate grc.risk.rate" {
		t.Errorf("audit: got %v, want a creation and two ratings", a)
	}
	events := e.riskEvents(t)
	if len(events) != 3 {
		t.Fatalf("events: got %d, want 3", len(events))
	}
	wantFields(t, events[1], map[string]any{"change": "rated", "score": float64(4), "version": float64(2)})
}

// TestChangeRisk: an open risk's texts, owner and controls change with
// the caller's version; a control left out is kept as removed and can be
// named again; a control kept is not checked again.
func TestChangeRisk(t *testing.T) {
	e := setup(t)
	pay1, _ := e.record(t, "PAY-01", "Payments")
	pay2, tag2 := e.record(t, "PAY-02", "Payments")
	pay3, _ := e.record(t, "PAY-03", "Payments")
	id, tag := e.risk(t, "Duplicate payments", "Payments", 2, 4, pay1, pay2)
	path := "/v1/risks/" + id
	e.Must(200, "POST", "/v1/controls/"+pay2+":retire", "", tag2)
	read, _ := e.Must(200, "GET", path, "", "")
	if c := controlsOf(t, read); c != "PAY-01 active, PAY-02 retired" {
		t.Errorf("a control retired after it was named: got %q, want it shown as retired", c)
	}
	got, next := e.Must(200, "PATCH", path, fmt.Sprintf(`{"title": "Payments made twice", "category": "Treasury", "owner_id": %q,
		"control_ids": [%q, %q]}`, e.Tenant.Human.ID, pay2, pay3), tag)
	wantFields(t, got, map[string]any{"title": "Payments made twice", "category": "Treasury", "owner_id": e.Tenant.Human.ID.String(),
		"owner_name": e.me(t), "version": float64(2), "score": float64(8)})
	if c := controlsOf(t, got); c != "PAY-02 retired, PAY-03 active" {
		t.Errorf("controls after the change: got %q, want PAY-02 retired, PAY-03 active", c)
	}
	var removed int
	pgtest.Scan(t, `SELECT count(*) FROM grc.risk_control WHERE risk_id = $1 AND control_id = $2 AND status = 'removed'
		AND removed_by = $3 AND removed_at IS NOT NULL`, []any{id, pay1, e.Tenant.Human.ID}, &removed)
	if removed != 1 {
		t.Errorf("PAY-01 left out: got %d removed rows, want 1, kept", removed)
	}
	again, next := e.Must(200, "PATCH", path, fmt.Sprintf(`{"control_ids": [%q, %q, %q]}`, pay1, pay2, pay3), next)
	if c := controlsOf(t, again); c != "PAY-01 active, PAY-02 retired, PAY-03 active" {
		t.Errorf("PAY-01 named again: got %q", c)
	}
	events := e.riskEvents(t)
	wantFields(t, events[len(events)-1], map[string]any{"change": "changed", "version": float64(3), "rating": nil})
	for _, c := range []struct {
		name, body, tag string
		status          int
		code            string
	}{
		{"with a stale version", `{"category": "Fraud"}`, tag, 412, "request.version_mismatch"},
		{"to what it already says", `{"category": "Treasury"}`, next, 422, "grc.nothing_to_change"},
		{"of the same controls in another order", fmt.Sprintf(`{"control_ids": [%q, %q, %q]}`, pay3, pay2, pay1), next, 422, "grc.nothing_to_change"},
		{"of nothing", `{}`, next, 422, "grc.nothing_to_change"},
		{"of the rating", `{"likelihood": 5}`, next, 400, "request.invalid_body"},
		{"naming a kept control twice", fmt.Sprintf(`{"control_ids": [%q, %q]}`, pay1, pay1), next, 422, "grc.invalid_control"},
		{"to an agent as the owner", fmt.Sprintf(`{"owner_id": %q}`, e.Tenant.Agent.ID), next, 422, "grc.invalid_owner"},
		{"to a title with white space around it", `{"title": " Fraud"}`, next, 422, "grc.invalid_text"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e.in(t).Problem(c.status, c.code, "PATCH", path, c.body, c.tag)
		})
	}
	other, _ := e.risk(t, "Fraud", "Payments", 1, 1)
	e.Problem(422, "grc.invalid_control", "PATCH", "/v1/risks/"+other, fmt.Sprintf(`{"control_ids": [%q]}`, pay2), `"1"`)
	e.Problem(404, "request.not_found", "PATCH", "/v1/risks/"+uuid.Must(uuid.NewV7()).String(), `{"category": "x"}`, `"1"`)
	if a := e.riskAudited(t, id); strings.Join(a, " ") != "grc.risk.create grc.risk.change grc.risk.change" {
		t.Errorf("audit: got %v, want a creation and two changes", a)
	}
}

// TestCloseRisk: an open risk is closed once with a reason; it then
// changes no more and takes no rating, and is still read and listed.
func TestCloseRisk(t *testing.T) {
	e := setup(t)
	id, first := e.risk(t, "Duplicate payments", "Payments", 2, 4)
	path := "/v1/risks/" + id
	_, tag := e.Must(200, "PATCH", path, `{"title": "Payments made twice"}`, first)
	e.Problem(412, "request.version_mismatch", "POST", path+":close", `{"reason": "Mitigated."}`, `"7"`)
	e.Problem(422, "grc.invalid_text", "POST", path+":close", `{"reason": "Mitigated. "}`, tag)
	got, next := e.Must(200, "POST", path+":close", `{"reason": "Mitigated by the duplicate check."}`, tag)
	wantFields(t, got, map[string]any{"status": "closed", "close_reason": "Mitigated by the duplicate check.", "closed_by": e.Tenant.Human.ID.String(),
		"closed_by_name": e.me(t), "version": float64(3)})
	if _, has := got["closed_at"]; !has {
		t.Error("closed_at: got none, want when it was closed")
	}
	for _, c := range []struct{ name, method, path, body, tag string }{
		{"closed again", "POST", path + ":close", `{"reason": "Again."}`, next},
		{"changed", "PATCH", path, `{"category": "Treasury"}`, next},
		{"rated", "POST", path + "/ratings", `{"likelihood": 1, "impact": 1}`, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			e.in(t).Problem(409, "grc.risk_closed", c.method, c.path, c.body, c.tag)
		})
	}
	closed, _ := e.Must(200, "GET", "/v1/risks?status=closed", "", "")
	if got := column(objects(t, closed, "items"), "id"); got != id {
		t.Errorf("closed risks: got %s, want %s", got, id)
	}
	open, _ := e.Must(200, "GET", "/v1/risks?status=open", "", "")
	if n := len(objects(t, open, "items")); n != 0 {
		t.Errorf("open risks: got %d, want none", n)
	}
	events := e.riskEvents(t)
	wantFields(t, events[len(events)-1], map[string]any{"change": "closed", "status": "closed", "rating": nil})
	if _, has := events[len(events)-1]["close_reason"]; has {
		t.Error("the event carries the reason; want it left out")
	}
}

// TestRegister: the register lists the risks by score, the highest
// first, then by title, in pages, and is filtered by status, category,
// owner and a control that mitigates the risk now.
func TestRegister(t *testing.T) {
	e := setup(t)
	pay, _ := e.record(t, "PAY-01", "Payments")
	e.risk(t, "Bribery", "Compliance", 2, 2)
	e.risk(t, "Duplicate payments", "Payments", 2, 4, pay)
	e.risk(t, "Cyber attack", "IT", 5, 5)
	e.risk(t, "Ash cloud", "Operations", 4, 2)
	mine, _ := e.Must(201, "POST", "/v1/risks", riskOf("Fraud", "Payments", e.Tenant.Human.ID, 1, 1), "")
	var titles []string
	token := ""
	for pages := 0; ; pages++ {
		q := "/v1/risks?page_size=2"
		if token != "" {
			q += "&page_token=" + token
		}
		page, _ := e.Must(200, "GET", q, "", "")
		titles = append(titles, column(objects(t, page, "items"), "title"))
		next, has := page["next_page_token"].(string)
		if !has || pages > 5 {
			break
		}
		token = next
	}
	if got, want := strings.Join(titles, " | "), "Cyber attack Ash cloud | Duplicate payments Bribery | Fraud"; got != want {
		t.Errorf("the register by score, then title: got %q, want %q", got, want)
	}
	for _, c := range []struct{ name, query, want string }{
		{"by category", "category=Payments", "Duplicate payments Fraud"},
		{"by owner", "owner_id=" + e.Tenant.Human.ID.String(), "Fraud"},
		{"by control", "control_id=" + pay, "Duplicate payments"},
		{"by status", "status=closed", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			page, _ := e.in(t).Must(200, "GET", "/v1/risks?"+c.query, "", "")
			if got := column(objects(t, page, "items"), "title"); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
	e.Problem(400, "request.invalid_page_token", "GET", "/v1/risks?category=IT&page_token="+token, "", "")
	if apitest.String(t, mine, "owner_name") != e.me(t) {
		t.Errorf("owner_name: got %v, want %s", mine["owner_name"], e.me(t))
	}
}

// TestRiskWaitsForARetirement: a risk that names a control being retired
// waits for the retirement and is then refused; the retirement locks
// the control alone, so the two never wait for each other in a cycle
// (ADR-0078 §12, ADR-0008).
func TestRiskWaitsForARetirement(t *testing.T) {
	e := setup(t)
	control, _ := e.record(t, "PAY-01", "Payments")
	ctx := context.Background()
	tx, err := pgtest.Admin(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) }) // committed by the test; a failed test releases it here
	var holder int32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holder); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE grc.control SET status = 'retired', version = version + 1 WHERE id = $1`, control); err != nil {
		t.Fatal(err)
	}
	h := endpoints(t)
	pid, done := apitest.Started(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.RecordRisk(ctx, grc.NewRisk{Title: "Fraud", Category: "Payments", OwnerID: e.carla.ID, Likelihood: 1, Impact: 1,
			ControlIDs: []uuid.UUID{uuid.MustParse(control)}})
		return err
	})
	recorder := <-pid
	apitest.WaitBlocked(t, recorder, holder, done, "the risk")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if p := problem.From(<-done); p.Code != "grc.invalid_control" {
		t.Errorf("the risk after the retirement: got %v, want grc.invalid_control", p)
	}
}
