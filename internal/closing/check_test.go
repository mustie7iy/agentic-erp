package closing_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// checkPermissions are Carla's, who also parks, cancels and posts
// entries and runs revaluations in these tests.
var checkPermissions = append(append([]string{}, closePermissions...), "ledger.entry.park", "ledger.entry.post")

// withChecks is withTemplate with two check tasks: no parked entry left,
// waiting for the invoices to be entered, and the revaluation posted.
func withChecks(doc *config.Config) {
	withTemplate(doc)
	doc.Close.Tasks = append(doc.Close.Tasks,
		config.CloseTask{Code: "NO_PARKED", Name: "No entry of the period left parked", Kind: config.CloseCheck, Check: "no_parked_entries", DueDay: 3,
			DependsOn: []string{"AP_CUTOFF"}},
		config.CloseTask{Code: "REVALUED", Name: "Foreign currency revalued", Kind: config.CloseCheck, Check: "revaluation_posted", DueDay: 1})
}

// taskOf is the task of a close with the code.
func taskOf(t *testing.T, close map[string]any, code string) map[string]any {
	t.Helper()
	for _, x := range apitest.List(t, close, "tasks") {
		if k := x.(map[string]any); k["code"] == code {
			return k
		}
	}
	t.Fatalf("close has no task %s", code)
	return nil
}

// result is a task's check result: its check, whether it passed, who ran
// it, its summary and the parked entries it named, each by header text,
// status, posting date and who parked it.
func result(t *testing.T, task map[string]any) string {
	t.Helper()
	r, ok := task["check_result"].(map[string]any)
	if !ok {
		t.Fatalf("task %v: got no check result", task["code"])
	}
	out := fmt.Sprintf("%v passed=%v by %v: %v", r["check"], r["passed"], r["checked_by_name"], r["summary"])
	if n, ok := r["parked_entries_count"]; ok {
		out += fmt.Sprintf(" | %v:", n)
		for _, x := range apitest.List(t, r, "parked_entries") {
			p := x.(map[string]any)
			out += fmt.Sprintf(" %v %v %v %v/%v", p["header_text"], p["status"], p["posting_date"], p["created_by_name"], p["created_by_kind"])
		}
	}
	return out
}

// TestNoParkedEntries: the parked-entries check, waiting for the task it
// depends on, fails naming the entry parked in the period (not the one
// parked after it) and leaves the task open; once the entry is cancelled
// it passes and the task is done, by whoever ran it. Run again it keeps
// the task's completion; reopened, the task keeps its result; an entry
// parked since fails it and opens the task again. Each run moves the
// close's version and is published with whether it passed; a manual task
// is not checked and a check task not completed by hand.
func TestNoParkedEntries(t *testing.T) {
	e := fixture.NewWith(t, nil, withChecks, checkPermissions...)
	got, tag := open(t, e.Client, e.DE, 1, "")
	base := "/v1/period-closes/" + apitest.String(t, got, "id") + "/tasks/"
	if task := taskOf(t, got, "NO_PARKED"); task["kind"] != "check" || task["check"] != "no_parked_entries" || task["check_result"] != nil {
		t.Errorf("the check task as opened: got %v %v %v, want a no_parked_entries check never run", task["kind"], task["check"], task["check_result"])
	}
	parked := e.Park(t, "2026-01-20", "Office rent accrual", [2]string{"520000", "100.00"}, [2]string{"100000", "-100.00"})
	e.Park(t, "2026-02-02", "February's rent", [2]string{"520000", "100.00"}, [2]string{"100000", "-100.00"})

	e.Problem(409, "closing.task_blocked", "POST", base+"NO_PARKED:check", "", tag)
	e.Problem(409, "closing.task_not_check", "POST", base+"AP_CUTOFF:check", "", tag)
	e.Problem(409, "closing.task_is_check", "POST", base+"REVALUED:complete", "", tag)
	_, tag = e.Must(200, "POST", base+"AP_CUTOFF:complete", "", tag)

	task, tag := e.Must(200, "POST", base+"NO_PARKED:check", "", tag)
	name := task["check_result"].(map[string]any)["checked_by_name"]
	want := fmt.Sprintf("no_parked_entries passed=false by %v: 1 parked entry dated 2026-01-01 to 2026-01-31 is parked or pending approval; post or cancel it. "+
		"| 1: Office rent accrual parked 2026-01-20 %v/human", name, name)
	if got := result(t, task); got != want {
		t.Errorf("failing:\ngot  %s\nwant %s", got, want)
	}
	wantFields(t, "failing", task, map[string]any{"status": "open", "completed_by": nil, "close_version": 3.0})
	if id := apitest.List(t, task["check_result"].(map[string]any), "parked_entries")[0].(map[string]any)["id"]; id != parked.String() {
		t.Errorf("the entry named: got %v, want %s", id, parked)
	}

	e.Must(200, "POST", "/v1/parked-journal-entries/"+parked.String()+":cancel", `{"reason": "Accrued twice"}`, `"1"`)
	task, tag = e.Must(200, "POST", base+"NO_PARKED:check", "", tag)
	if got, want := result(t, task), fmt.Sprintf("no_parked_entries passed=true by %v: No parked entry dated 2026-01-01 to 2026-01-31 is parked or pending approval. | 0:", name); got != want {
		t.Errorf("passing:\ngot  %s\nwant %s", got, want)
	}
	wantFields(t, "passing", task, map[string]any{"status": "done", "completed_by": e.Tenant.Human.ID.String(), "completed_by_name": name})
	done := task["completed_at"]

	task, tag = e.Must(200, "POST", base+"NO_PARKED:check", "", tag)
	wantFields(t, "passing again", task, map[string]any{"status": "done", "completed_at": done})
	task, tag = e.Must(200, "POST", base+"NO_PARKED:reopen", `{"reason": "Look again"}`, tag)
	wantFields(t, "reopened", task, map[string]any{"status": "open"})
	if task["check_result"] == nil {
		t.Error("reopened: got no check result, want the latest kept")
	}
	_, tag = e.Must(200, "POST", base+"NO_PARKED:check", "", tag)
	e.Park(t, "2026-01-31", "A late correction", [2]string{"520000", "5.00"}, [2]string{"100000", "-5.00"})
	task, _ = e.Must(200, "POST", base+"NO_PARKED:check", "", tag)
	wantFields(t, "failing after a pass", task, map[string]any{"status": "open", "completed_by": nil, "completed_at": nil})

	var changes []string
	for _, ev := range apitest.Published(t, e.Tenant, outbox.Event{Context: "closing", Name: "period_close_changed"}) {
		changes = append(changes, fmt.Sprintf("%v %v %v", ev["change"], ev["task_code"], ev["check_passed"]))
	}
	wantLines(t, "events", changes, []string{"created <nil> <nil>", "task_completed AP_CUTOFF <nil>", "task_checked NO_PARKED false",
		"task_checked NO_PARKED true", "task_checked NO_PARKED true", "task_reopened NO_PARKED <nil>", "task_checked NO_PARKED true",
		"task_checked NO_PARKED false"})
	var audited string
	pgtest.Scan(t, `SELECT string_agg(detail->>'passed', ' ' ORDER BY seq) FROM audit.audit_event WHERE tenant_id = $1
		AND action = 'closing.period_close.check_task'`, []any{e.Tenant.ID}, &audited)
	if audited != "false true true true false" {
		t.Errorf("audited checks: got %q, want false true true true false", audited)
	}
}

// TestNoParkedEntriesNamesTwenty: the check counts every entry left and
// names the first twenty by posting date.
func TestNoParkedEntriesNamesTwenty(t *testing.T) {
	e := fixture.NewWith(t, nil, withChecks, checkPermissions...)
	got, tag := open(t, e.Client, e.DE, 1, "")
	base := "/v1/period-closes/" + apitest.String(t, got, "id") + "/tasks/"
	_, tag = e.Must(200, "POST", base+"AP_CUTOFF:complete", "", tag)
	for i := 1; i <= 21; i++ {
		e.Park(t, fmt.Sprintf("2026-01-%02d", i), fmt.Sprintf("Entry %02d", i), [2]string{"520000", "1.00"}, [2]string{"100000", "-1.00"})
	}
	task, _ := e.Must(200, "POST", base+"NO_PARKED:check", "", tag)
	r := task["check_result"].(map[string]any)
	named := apitest.List(t, r, "parked_entries")
	if r["parked_entries_count"] != 21.0 || len(named) != 20 {
		t.Fatalf("got %v counted, %d named, want 21 and 20", r["parked_entries_count"], len(named))
	}
	if first, last := named[0].(map[string]any)["header_text"], named[19].(map[string]any)["header_text"]; first != "Entry 01" || last != "Entry 20" {
		t.Errorf("named: got %v to %v, want Entry 01 to Entry 20", first, last)
	}
	if !strings.HasPrefix(r["summary"].(string), "21 parked entries dated 2026-01-01 to 2026-01-31") {
		t.Errorf("summary: got %v, want 21 counted", r["summary"])
	}
}

// TestRevaluationPosted: the revaluation check fails naming each covered
// ledger without a posted run keyed on its period's last day, and passes
// once each has one, naming the runs; a cancelled run does not count.
func TestRevaluationPosted(t *testing.T) {
	e := fixture.NewWith(t, nil, withChecks, append(checkPermissions, "ledger.period.read")...)
	e.Revalues(t, withChecks)
	got, tag := open(t, e.Client, e.DE, 3, "")
	base := "/v1/period-closes/" + apitest.String(t, got, "id") + "/tasks/"
	leading, local := e.Ledger(t, config.LeadingLedger), e.Ledger(t, "2L")
	lines := func(task map[string]any) []string {
		r := task["check_result"].(map[string]any)
		out := []string{fmt.Sprintf("%v %v", r["passed"], r["summary"])}
		for _, x := range apitest.List(t, r, "ledgers") {
			l := x.(map[string]any)
			out = append(out, fmt.Sprintf("%v %v %v posted=%v run=%v", l["ledger_code"], l["ledger_name"], l["key_date"], l["posted"], l["revaluation_run_id"]))
		}
		return out
	}

	task, tag := e.Must(200, "POST", base+"REVALUED:check", "", tag)
	wantLines(t, "before any run", lines(task), []string{
		"false No posted revaluation run at the period's last day in ledger 0L, 2L; run the revaluation first.",
		"0L Leading ledger 2026-03-31 posted=false run=<nil>", "2L HGB 2026-03-31 posted=false run=<nil>"})
	wantFields(t, "before any run", task, map[string]any{"status": "open"})

	run0, _ := e.Must(201, "POST", "/v1/revaluation-runs", fmt.Sprintf(`{"legal_entity_id": %q, "ledger_id": %q, "key_date": "2026-03-31"}`, e.DE, leading), "")
	pgtest.Unguarded(t, `INSERT INTO ledger.revaluation_run (tenant_id, id, legal_entity_id, ledger_id, key_date, rate_type, status, created_by, updated_by)
		VALUES ($1, gen_random_uuid(), $2, $3, '2026-03-31', 'bank_selling', 'cancelled', $4, $4)`, e.Tenant.ID, e.DE, local, e.Tenant.Human.ID)
	task, tag = e.Must(200, "POST", base+"REVALUED:check", "", tag)
	wantLines(t, "the leading ledger's run", lines(task), []string{
		"false No posted revaluation run at the period's last day in ledger 2L; run the revaluation first.",
		fmt.Sprintf("0L Leading ledger 2026-03-31 posted=true run=%v", run0["id"]), "2L HGB 2026-03-31 posted=false run=<nil>"})

	run2, _ := e.Must(201, "POST", "/v1/revaluation-runs", fmt.Sprintf(`{"legal_entity_id": %q, "ledger_id": %q, "key_date": "2026-03-31"}`, e.DE, local), "")
	task, _ = e.Must(200, "POST", base+"REVALUED:check", "", tag)
	wantLines(t, "every ledger's run", lines(task), []string{
		"true Every covered ledger has a posted revaluation run at its period's last day.",
		fmt.Sprintf("0L Leading ledger 2026-03-31 posted=true run=%v", run0["id"]), fmt.Sprintf("2L HGB 2026-03-31 posted=true run=%v", run2["id"])})
	wantFields(t, "every ledger's run", task, map[string]any{"status": "done"})
}

// TestCheckRefusals: a check needs If-Match, a current version and a
// task of the close; agents do not run checks; a person who may not act
// in the close's company does not find it.
func TestCheckRefusals(t *testing.T) {
	e := fixture.NewWith(t, nil, withChecks, checkPermissions...)
	got, tag := open(t, e.Client, e.DE, 1, "")
	base := "/v1/period-closes/" + apitest.String(t, got, "id") + "/tasks/"
	e.Problem(428, "request.precondition_required", "POST", base+"REVALUED:check", "", "")
	e.Problem(412, "request.version_mismatch", "POST", base+"REVALUED:check", "", `"9"`)
	e.Problem(404, "request.not_found", "POST", base+"NO_SUCH:check", "", tag)
	e.As(e.Tenant.Agent).Problem(403, "identity.agent_not_allowed", "POST", base+"REVALUED:check", "", tag)
	e.As(e.Reader(t, "reader", e.US)).Problem(404, "request.not_found", "POST", base+"REVALUED:check", "", tag)
}

// TestCheckDatabaseFailures runs each check with each of its database
// statements failing in turn, and outside a unit of work.
func TestCheckDatabaseFailures(t *testing.T) {
	e := fixture.NewWith(t, nil, withChecks, checkPermissions...)
	got, tag := open(t, e.Client, e.DE, 1, "")
	id := uuid.MustParse(apitest.String(t, got, "id"))
	_, tag = e.Must(200, "POST", "/v1/period-closes/"+id.String()+"/tasks/AP_CUTOFF:complete", "", tag)
	e.Park(t, "2026-01-20", "Office rent accrual", [2]string{"520000", "100.00"}, [2]string{"100000", "-100.00"})
	h := endpoints(t)
	for _, code := range []string{"NO_PARKED", "REVALUED"} {
		call := func(ctx context.Context) error {
			_, err := h.CheckCloseTask(ctx, gen.CheckCloseTaskParams{ID: id, Code: code, IfMatch: tag})
			return err
		}
		t.Run(code, func(t *testing.T) {
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, code, call)
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
		})
	}
}

// TestCheckBrokenData: a task whose check this build does not run, or a
// parked entry whose creator the identity context does not hold, fails
// the check rather than recording a result.
func TestCheckBrokenData(t *testing.T) {
	e := fixture.NewWith(t, nil, withChecks, checkPermissions...)
	got, tag := open(t, e.Client, e.DE, 1, "")
	id := uuid.MustParse(apitest.String(t, got, "id"))
	_, tag = e.Must(200, "POST", "/v1/period-closes/"+id.String()+"/tasks/AP_CUTOFF:complete", "", tag)
	h := endpoints(t)
	check := func(code string) error {
		return apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.CheckCloseTask(ctx, gen.CheckCloseTaskParams{ID: id, Code: code, IfMatch: tag})
			return err
		})
	}
	parked := e.Park(t, "2026-01-20", "Office rent accrual", [2]string{"520000", "100.00"}, [2]string{"100000", "-100.00"})
	pgtest.Unguarded(t, `UPDATE ledger.parked_entry SET created_by = $2, owner_id = $2 WHERE id = $1`, parked, uuid.New())
	if err := check("NO_PARKED"); err == nil || !strings.Contains(err.Error(), "not held by the identity context") {
		t.Errorf("an unknown creator: got %v, want the identity context's failure", err)
	}

	pgtest.Unguarded(t, `UPDATE closing.close_task SET check_kind = 'bank_reconciled' WHERE close_id = $1 AND code = 'REVALUED'`, id)
	if err := check("REVALUED"); err == nil || !strings.Contains(err.Error(), `"bank_reconciled", which this build does not`) {
		t.Errorf("an unknown check: got %v, want the unknown check's failure", err)
	}
}
