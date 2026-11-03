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

// signPermissions are Carla's when she signs closes off: she also closes,
// opens and locks periods, and parks and posts entries.
var signPermissions = append(append([]string{}, closePermissions...), "closing.close.sign_off", "ledger.period.read", "ledger.period.manage",
	"ledger.period.lock", "ledger.entry.park", "ledger.entry.post")

// withSignOff is withTemplate (company 2000 keeps 0L and 2L) with the
// parked-entries check, waiting for the invoices to be entered.
func withSignOff(doc *config.Config) {
	withTemplate(doc)
	doc.Close.Tasks = append(doc.Close.Tasks, config.CloseTask{Code: "NO_PARKED", Name: "No entry of the period left parked", Kind: config.CloseCheck,
		Check: "no_parked_entries", DueDay: 3, DependsOn: []string{"AP_CUTOFF"}})
}

// signing is a close of company 2000 under test: its path and ETag.
type signing struct {
	e    fixture.Env
	path string
	tag  string
}

// signable opens the close of company 2000's period of 2026 and completes
// its manual tasks.
func signable(t *testing.T, e fixture.Env, period int) *signing {
	t.Helper()
	got, tag := open(t, e.Client, e.DE, period, "")
	s := &signing{e: e, path: "/v1/period-closes/" + apitest.String(t, got, "id"), tag: tag}
	for _, code := range []string{"AP_CUTOFF", "REVIEW"} {
		s.task(t, 200, code+":complete", "")
	}
	return s
}

// task acts on a task of the close, keeping the close's new ETag.
func (s *signing) task(t *testing.T, status int, action, body string) map[string]any {
	t.Helper()
	got, tag := s.e.Must(status, "POST", s.path+"/tasks/"+action, body, s.tag)
	s.tag = tag
	return got
}

// read reads the close, keeping its ETag.
func (s *signing) read(t *testing.T) map[string]any {
	t.Helper()
	got, tag := s.e.Must(200, "GET", s.path, "", "")
	s.tag = tag
	return got
}

// signOff signs the close off with the note.
func (s *signing) signOff(t *testing.T, note string) map[string]any {
	t.Helper()
	got, tag := s.e.Must(200, "POST", s.path+":signOff", fmt.Sprintf(`{"note": %q}`, note), s.tag)
	s.tag = tag
	return got
}

// periods moves each covered ledger's period through the ledger's own
// operation (:close, :open or :lock), as a person does.
func (s *signing) periods(t *testing.T, action string) {
	t.Helper()
	for _, x := range apitest.List(t, s.read(t), "ledgers") {
		path := "/v1/fiscal-periods/" + x.(map[string]any)["fiscal_period_id"].(string)
		_, tag := s.e.Must(200, "GET", path, "", "")
		s.e.Must(200, "POST", path+":"+action, "", tag)
	}
}

// ledgerLines are a sign-off's covered ledgers, one line each.
func ledgerLines(t *testing.T, signOff map[string]any) []string {
	t.Helper()
	var out []string
	for _, x := range apitest.List(t, signOff, "ledgers") {
		l := x.(map[string]any)
		out = append(out, fmt.Sprintf("%v %v leading=%v locked=%v", l["ledger_code"], l["ledger_name"], l["is_leading"], l["locked"]))
	}
	return out
}

// entry is a manual entry of company 2000 dated day, in EUR, in every
// ledger it keeps or, with extra `, "ledger_id": ...`, in one.
func entry(t *testing.T, e fixture.Env, day, extra string) string {
	t.Helper()
	return fmt.Sprintf(`{"legal_entity_id": %q, "document_date": %q, "posting_date": %q, "currency": "EUR"%s, "lines": [
		{"account_id": %q, "amount": {"amount": "25.00", "currency": "EUR"}}, {"account_id": %q, "amount": {"amount": "-25.00", "currency": "EUR"}}]}`,
		e.DE, day, day, extra, e.Account(t, "2000", "520000"), e.Account(t, "2000", "100000"))
}

// TestSignOff: a sign-off is refused while a manual task is open, while a
// check fails when run again (naming it, and keeping nothing) and while a
// covered period is open; once the periods are closed it locks each, the
// leading ledger first, records the sign-off and the checks' results, and
// the close is signed off: postings dated in the period are refused, and
// the close refuses every write and a second sign-off. The calendar and
// the list name the signer; the sign-off is audited and published, and
// each lock is the ledger's.
func TestSignOff(t *testing.T) {
	e := fixture.NewWith(t, nil, withSignOff, signPermissions...)
	got, tag := open(t, e.Client, e.DE, 1, "")
	s := &signing{e: e, path: "/v1/period-closes/" + apitest.String(t, got, "id"), tag: tag}
	p := e.Problem(409, "closing.tasks_open", "POST", s.path+":signOff", "", s.tag)
	if want := "Manual task AP_CUTOFF, REVIEW is open; complete every manual task before signing off."; p["detail"] != want {
		t.Errorf("tasks open: got %v, want %s", p["detail"], want)
	}
	s.task(t, 200, "AP_CUTOFF:complete", "")
	s.task(t, 200, "REVIEW:complete", "")
	parked := e.Park(t, "2026-01-20", "Office rent accrual", [2]string{"520000", "100.00"}, [2]string{"100000", "-100.00"})

	p = e.Problem(409, "closing.checks_failing", "POST", s.path+":signOff", "", s.tag)
	failing := apitest.List(t, p, "failing_checks")
	if len(failing) != 1 {
		t.Fatalf("failing checks: got %v, want one", failing)
	}
	wantFields(t, "the failing check", failing[0].(map[string]any), map[string]any{"task_code": "NO_PARKED", "task_name": "No entry of the period left parked",
		"check": "no_parked_entries", "summary": "1 parked entry dated 2026-01-01 to 2026-01-31 is parked or pending approval; post or cancel it."})
	refused := s.read(t)
	if task := taskOf(t, refused, "NO_PARKED"); task["check_result"] != nil {
		t.Errorf("a refused sign-off's check: got %v recorded, want nothing kept", task["check_result"])
	}
	wantFields(t, "after the refusals", refused, map[string]any{"status": "open", "version": 3.0})

	e.Must(200, "POST", "/v1/parked-journal-entries/"+parked.String()+":cancel", `{"reason": "Accrued twice"}`, `"1"`)
	p = e.Problem(409, "closing.period_not_closed", "POST", s.path+":signOff", "", s.tag)
	if !strings.Contains(p["detail"].(string), "Period 2026/1 is open in ledger 0L, 2L") {
		t.Errorf("periods open: got %v, want both ledgers named", p["detail"])
	}
	s.periods(t, "close")
	s.read(t)
	off := s.signOff(t, "January closed")
	wantLines(t, "the sign-off's ledgers", ledgerLines(t, off), []string{"0L Leading ledger leading=true locked=true", "2L HGB leading=false locked=true"})
	name := off["signed_by_name"]
	wantFields(t, "the sign-off", off, map[string]any{"close_id": strings.TrimPrefix(s.path, "/v1/period-closes/"), "company_code": "2000",
		"company_name": "Acme GmbH", "fiscal_year": 2026.0, "fiscal_period": 1.0, "signed_by": e.Tenant.Human.ID.String(), "note": "January closed"})
	if name == nil || name == "" {
		t.Errorf("signed_by_name: got %v, want the signer's name", name)
	}

	c := s.read(t)
	wantFields(t, "the close signed off", c, map[string]any{"status": "signed_off", "signed_off_by": e.Tenant.Human.ID.String(), "signed_off_by_name": name,
		"signed_off_at": off["signed_at"]})
	for _, x := range apitest.List(t, c, "ledgers") {
		if l := x.(map[string]any); l["period_status"] != "locked" {
			t.Errorf("ledger %v after the sign-off: got %v, want locked", l["ledger_code"], l["period_status"])
		}
	}
	if offs := apitest.List(t, c, "sign_offs"); len(offs) != 1 || offs[0].(map[string]any)["id"] != off["id"] {
		t.Errorf("the close's sign-offs: got %v, want the one", offs)
	}
	if r := taskOf(t, c, "NO_PARKED")["check_result"].(map[string]any); r["passed"] != true {
		t.Errorf("the check run by the sign-off: got %v, want its passing result recorded", r)
	}

	e.Problem(409, "ledger.period_not_open", "POST", "/v1/journal-entries", entry(t, e, "2026-01-15", ""), "")
	e.Problem(409, "closing.close_signed_off", "POST", s.path+":signOff", "", s.tag)
	e.Problem(409, "closing.close_signed_off", "POST", s.path+"/tasks/AP_CUTOFF:reopen", `{"reason": "Look again"}`, s.tag)
	e.Problem(409, "closing.close_signed_off", "POST", s.path+"/tasks/NO_PARKED:check", "", s.tag)

	row, _ := e.Must(200, "GET", fmt.Sprintf("/v1/close-calendar?legal_entity_id=%s&fiscal_year=2026", e.DE), "", "")
	cal := apitest.List(t, row, "periods")[0].(map[string]any)["close"].(map[string]any)
	wantFields(t, "the calendar's close", cal, map[string]any{"status": "signed_off", "signed_off_by_name": name, "signed_off_at": off["signed_at"]})
	list, _ := e.Must(200, "GET", "/v1/period-closes?status=signed_off", "", "")
	if items := apitest.List(t, list, "items"); len(items) != 1 || items[0].(map[string]any)["signed_off_by_name"] != name {
		t.Errorf("the list's signed-off closes: got %v, want the one named", items)
	}

	var events []string
	for _, ev := range apitest.Published(t, e.Tenant, outbox.Event{Context: "closing", Name: "period_close_signed_off"}) {
		events = append(events, fmt.Sprintf("%v %v %v %v", ev["sign_off_id"], ev["note"], ev["status"], ev["periods"]))
	}
	wantLines(t, "PeriodCloseSignedOff", events, []string{fmt.Sprintf("%v January closed signed_off [map[fiscal_period_id:%v ledger_code:0L ledger_id:%v locked:true] "+
		"map[fiscal_period_id:%v ledger_code:2L ledger_id:%v locked:true]]", off["id"], idOf(t, off, 0, "fiscal_period_id"), idOf(t, off, 0, "ledger_id"),
		idOf(t, off, 1, "fiscal_period_id"), idOf(t, off, 1, "ledger_id"))})
	if n := len(apitest.Published(t, e.Tenant, outbox.Event{Context: "ledger", Name: "period_locked"})); n != 2 {
		t.Errorf("the ledger's PeriodLocked: got %d, want 2", n)
	}
	var audited int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'closing.period_close.sign_off'`, []any{e.Tenant.ID}, &audited)
	if audited != 1 {
		t.Errorf("audited sign-offs: got %d, want 1", audited)
	}
}

// idOf is the field of a sign-off's i-th ledger.
func idOf(t *testing.T, signOff map[string]any, i int, field string) any {
	t.Helper()
	return apitest.List(t, signOff, "ledgers")[i].(map[string]any)[field]
}

// TestSignOffAfterUnlock: a signed-off close whose period an approved
// unlock returned to closed is signed off again as it is; unlocked again,
// its first write (a task reopened) reopens it, audited and published
// before the write; a correcting entry is posted in the reopened period,
// and a third sign-off locks it again. A period locked already is left as
// it is, and named as not locked by the sign-off.
func TestSignOffAfterUnlock(t *testing.T) {
	e := fixture.NewWith(t, nil, withSignOff, signPermissions...)
	s := signable(t, e, 1)
	s.periods(t, "close")
	s.read(t)
	s.signOff(t, "First")
	unlock := func(ledger string) {
		t.Helper()
		var path string
		for _, x := range apitest.List(t, s.read(t), "ledgers") {
			if l := x.(map[string]any); l["ledger_code"] == ledger {
				path = "/v1/fiscal-periods/" + l["fiscal_period_id"].(string)
			}
		}
		_, tag := e.Must(200, "GET", path, "", "")
		p, _ := e.Must(200, "POST", path+":unlock", `{"reason": "Audit adjustment"}`, tag)
		e.Approve(t, p["id"].(string))
	}

	unlock("2L")
	off := s.signOff(t, "Second")
	wantLines(t, "signed off again", ledgerLines(t, off), []string{"0L Leading ledger leading=true locked=false", "2L HGB leading=false locked=true"})
	if c := s.read(t); c["status"] != "signed_off" || len(apitest.List(t, c, "sign_offs")) != 2 {
		t.Errorf("after the second sign-off: got %v with %d sign-offs, want signed_off with 2", c["status"], len(apitest.List(t, c, "sign_offs")))
	}

	unlock("0L")
	task := s.task(t, 200, "AP_CUTOFF:reopen", `{"reason": "A correcting entry is due"}`)
	wantFields(t, "the task reopened", task, map[string]any{"status": "open"})
	wantFields(t, "the close reopened", s.read(t), map[string]any{"status": "open"})
	e.Problem(409, "closing.tasks_open", "POST", s.path+":signOff", "", s.tag)
	path := "/v1/fiscal-periods/" + idOf(t, off, 0, "fiscal_period_id").(string)
	_, tag := e.Must(200, "GET", path, "", "")
	_, tag = e.Must(200, "POST", path+":open", "", tag)
	// 2L stays locked: the correction is the leading ledger's alone.
	e.Must(201, "POST", "/v1/journal-entries", entry(t, e, "2026-01-31", fmt.Sprintf(`, "ledger_id": %q`, idOf(t, off, 0, "ledger_id"))), "")
	e.Must(200, "POST", path+":close", "", tag)
	s.task(t, 200, "AP_CUTOFF:complete", `{"note": "Correcting entry posted"}`)
	off = s.signOff(t, "Third")
	wantLines(t, "signed off a third time", ledgerLines(t, off), []string{"0L Leading ledger leading=true locked=true", "2L HGB leading=false locked=false"})

	var changes []string
	for _, ev := range apitest.Published(t, e.Tenant, outbox.Event{Context: "closing", Name: "period_close_changed"}) {
		changes = append(changes, fmt.Sprintf("%v %v %v", ev["change"], ev["task_code"], ev["status"]))
	}
	wantLines(t, "the changes after the unlock", changes[len(changes)-3:], []string{"reopened <nil> open", "task_reopened AP_CUTOFF open",
		"task_completed AP_CUTOFF open"})
	var signed []string
	for _, ev := range apitest.Published(t, e.Tenant, outbox.Event{Context: "closing", Name: "period_close_signed_off"}) {
		signed = append(signed, fmt.Sprint(ev["note"]))
	}
	wantLines(t, "the sign-offs published", signed, []string{"First", "Second", "Third"})
	var reopened string
	pgtest.Scan(t, `SELECT detail->>'unlocked_ledgers' FROM audit.audit_event WHERE tenant_id = $1 AND action = 'closing.period_close.reopen'`,
		[]any{e.Tenant.ID}, &reopened)
	if reopened != `["0L"]` {
		t.Errorf("the reopening audited: got %s, want the unlocked ledger 0L", reopened)
	}
}

// TestSignOffRefusals: a sign-off needs If-Match and a current version;
// agents do not sign off; a person who may not act in the close's company
// does not find it.
func TestSignOffRefusals(t *testing.T) {
	e := fixture.NewWith(t, nil, withSignOff, signPermissions...)
	s := signable(t, e, 1)
	e.Problem(428, "request.precondition_required", "POST", s.path+":signOff", "", "")
	e.Problem(412, "request.version_mismatch", "POST", s.path+":signOff", "", `"1"`)
	e.As(e.Tenant.Agent).Problem(403, "identity.agent_not_allowed", "POST", s.path+":signOff", "", s.tag)
	e.As(e.Reader(t, "reader", e.US)).Problem(404, "request.not_found", "POST", s.path+":signOff", "", s.tag)
	e.Problem(400, "request.invalid_body", "POST", s.path+":signOff", `{"note": ""}`, s.tag)
}

// TestSignOffDatabaseFailures signs a close off, and reopens a signed-off
// close whose period was unlocked, with each database statement failing in
// turn, and outside a unit of work.
func TestSignOffDatabaseFailures(t *testing.T) {
	e := fixture.NewWith(t, nil, withSignOff, signPermissions...)
	s := signable(t, e, 1)
	s.periods(t, "close")
	s.read(t)
	id := uuid.MustParse(strings.TrimPrefix(s.path, "/v1/period-closes/"))
	h := endpoints(t)
	signOff := func(ctx context.Context) error {
		_, err := h.SignOffPeriodClose(ctx, gen.NewOptPeriodCloseSignOffInput(gen.PeriodCloseSignOffInput{Note: gen.NewOptString("n")}),
			gen.SignOffPeriodCloseParams{ID: id, IfMatch: s.tag})
		return err
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "sign off", signOff)
	if err := signOff(context.Background()); err == nil {
		t.Error("sign off outside a unit of work: got no error, want one")
	}

	s.signOff(t, "Signed")
	pgtest.Unguarded(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
		AND fiscal_year = 2026 AND period = 1 AND ledger_id = $3`, e.Tenant.ID, e.DE, e.Ledger(t, "2L"))
	reopen := func(ctx context.Context) error {
		_, err := h.ReopenCloseTask(ctx, &gen.ReasonInput{Reason: "Again"}, gen.ReopenCloseTaskParams{ID: id, Code: "REVIEW", IfMatch: s.tag})
		return err
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "reopen", reopen)
	get := func(ctx context.Context) error {
		_, err := h.GetCloseCalendar(ctx, gen.GetCloseCalendarParams{LegalEntityID: e.DE, FiscalYear: 2026})
		return err
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "calendar", get)
}
