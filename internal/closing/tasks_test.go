package closing_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestCloseTasks: a person assigns a task, completes it with a note once
// the tasks it depends on are done, and reopens it with a reason, which
// reopens nothing that depends on it; each change moves the close's
// version, refuses a stale one, and is published.
func TestCloseTasks(t *testing.T) {
	e := fixture.NewWith(t, nil, withTemplate, closePermissions...)
	c := at(t, e, time.Date(2026, 2, 5, 12, 0, 0, 0, time.UTC))
	got, tag := open(t, c, e.US, 1, "")
	base := "/v1/period-closes/" + apitest.String(t, got, "id") + "/tasks/"
	me := fmt.Sprintf(`{"assignee_id": %q}`, e.Tenant.Human.ID)

	task, next := c.Must(200, "PATCH", base+"AP_CUTOFF", me, tag)
	if next != `"2"` {
		t.Errorf("assigned, ETag: got %s, want \"2\"", next)
	}
	wantFields(t, "assigned", task, map[string]any{"assignee_id": e.Tenant.Human.ID.String(), "close_version": 2.0})
	c.Problem(412, "request.version_mismatch", "PATCH", base+"AP_CUTOFF", me, tag)
	c.Problem(409, "closing.task_blocked", "POST", base+"REPORT:complete", "", next)
	task, next = c.Must(200, "POST", base+"AP_CUTOFF:complete", `{"note": "Entered"}`, next)
	wantFields(t, "completed", task, map[string]any{"status": "done", "overdue": false, "note": "Entered", "completed_by": e.Tenant.Human.ID.String(),
		"completed_by_name": task["assignee_name"]})
	if task["completed_at"] == nil {
		t.Errorf("completed: got no completed_at, want when")
	}
	c.Problem(409, "closing.task_already_done", "POST", base+"AP_CUTOFF:complete", "", next)
	_, next = c.Must(200, "POST", base+"REPORT:complete", "", next)
	task, next = c.Must(200, "POST", base+"AP_CUTOFF:reopen", `{"reason": "A late invoice arrived"}`, next)
	wantFields(t, "reopened", task, map[string]any{"status": "open", "overdue": true, "note": nil, "completed_by": nil, "completed_by_name": nil})
	c.Problem(409, "closing.task_not_done", "POST", base+"AP_CUTOFF:reopen", `{"reason": "Again"}`, next)
	task, next = c.Must(200, "PATCH", base+"AP_CUTOFF", `{"assignee_id": null}`, next)
	if _, ok := task["assignee_id"]; ok {
		t.Errorf("unassigned: got assignee %v, want none", task["assignee_id"])
	}
	if _, same := c.Must(200, "PATCH", base+"AP_CUTOFF", `{}`, next); same != next {
		t.Errorf("an empty patch: got ETag %s, want %s unchanged", same, next)
	}

	read, _ := c.Must(200, "GET", "/v1/period-closes/"+apitest.String(t, got, "id"), "", "")
	wantLines(t, "the checklist", tasks(t, read), []string{
		"AP_CUTOFF 2026-02-04 open overdue=true blocked=[] assignee=<nil> done_by=<nil> note=<nil>",
		"REVIEW 2026-01-30 open overdue=true blocked=[] assignee=<nil> done_by=<nil> note=<nil>",
		fmt.Sprintf("REPORT 2026-02-09 done overdue=false blocked=[AP_CUTOFF] assignee=<nil> done_by=%s note=<nil>", read["tasks"].([]any)[2].(map[string]any)["completed_by_name"]),
	})
	wantFields(t, "the close", read, map[string]any{"tasks_done": 1.0, "version": 6.0})
	var changes []string
	for _, ev := range apitest.Published(t, e.Tenant, outbox.Event{Context: "closing", Name: "period_close_changed"}) {
		changes = append(changes, fmt.Sprintf("%v %v %v %v", ev["change"], ev["task_code"], ev["reason"], ev["version"]))
	}
	wantLines(t, "events", changes, []string{"created <nil> <nil> 1", "task_assigned AP_CUTOFF <nil> 2", "task_completed AP_CUTOFF <nil> 3",
		"task_completed REPORT <nil> 4", "task_reopened AP_CUTOFF A late invoice arrived 5", "task_assigned AP_CUTOFF <nil> 6"})
}

// TestCloseTaskRefusals: a task is assigned to an active person of the
// tenant only; an unknown task or close is not found; a change needs
// If-Match; agents do not work the checklist.
func TestCloseTaskRefusals(t *testing.T) {
	e := fixture.NewWith(t, nil, withTemplate, closePermissions...)
	got, tag := open(t, e.Client, e.US, 1, "")
	base := "/v1/period-closes/" + apitest.String(t, got, "id") + "/tasks/"
	blocked := e.Person(t, "blocked")
	pgtest.Exec(t, `UPDATE identity.principal SET status = 'blocked', version = version + 1 WHERE id = $1`, blocked.ID)
	for name, id := range map[string]uuid.UUID{"an agent": e.Tenant.Agent.ID, "a service": e.Tenant.Service.ID, "a blocked person": blocked.ID,
		"an unknown principal": uuid.New()} {
		t.Run(name, func(t *testing.T) {
			p := e.Client.In(t).Problem(422, "closing.assignee_invalid", "PATCH", base+"REVIEW", fmt.Sprintf(`{"assignee_id": %q}`, id), tag)
			if errs, _ := p["errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["field"] != "assignee_id" {
				t.Errorf("field: got %v, want assignee_id", p["errors"])
			}
		})
	}
	e.Problem(404, "request.not_found", "POST", base+"NO_SUCH:complete", "", tag)
	e.Problem(404, "request.not_found", "POST", "/v1/period-closes/"+uuid.NewString()+"/tasks/REVIEW:complete", "", tag)
	e.Problem(428, "request.precondition_required", "POST", base+"REVIEW:complete", "", "")
	e.As(e.Tenant.Agent).Problem(403, "identity.agent_not_allowed", "POST", base+"REVIEW:complete", "", tag)
	reader := e.As(e.Reader(t, "reader", e.DE))
	reader.Problem(404, "request.not_found", "POST", base+"REVIEW:complete", "", tag)
}
