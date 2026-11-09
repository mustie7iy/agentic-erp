package closing_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// withYearSignOff is withYearTemplate (company 2000 keeps 0L and 2L, its
// year tasks AP_CUTOFF and TAX) with the parked-entries check of the year,
// waiting for AP_CUTOFF.
func withYearSignOff(doc *config.Config) {
	withYearTemplate(doc)
	doc.Close.YearTasks = append(doc.Close.YearTasks, config.CloseTask{Code: "NO_PARKED", Name: "No entry of the year left parked",
		Kind: config.CloseCheck, Check: "no_parked_entries", DueDay: 10, DependsOn: []string{"AP_CUTOFF"}})
}

// yearSigning is a year close of company 2000's 2026 under test: its path
// and ETag.
type yearSigning struct {
	signing
}

// yearSignable opens the year close of company 2000's 2026 and completes
// its manual tasks.
func yearSignable(t *testing.T, e fixture.Env) *yearSigning {
	t.Helper()
	got, tag := openYear(t, e.Client, e.DE, "")
	s := &yearSigning{signing{e: e, path: "/v1/year-closes/" + apitest.String(t, got, "id"), tag: tag}}
	for _, code := range []string{"AP_CUTOFF", "TAX"} {
		s.task(t, 200, code+":complete", "")
	}
	return s
}

// id is the year close's id.
func (s *yearSigning) id(t *testing.T) uuid.UUID {
	t.Helper()
	return uuid.MustParse(strings.TrimPrefix(s.path, "/v1/year-closes/"))
}

// period is the id of the covered ledger's period of 2026, read from the
// year close.
func (s *yearSigning) period(t *testing.T, ledger string, number int) string {
	t.Helper()
	for _, x := range apitest.List(t, s.read(t), "ledgers") {
		l := x.(map[string]any)
		for _, y := range apitest.List(t, l, "periods") {
			if p := y.(map[string]any); l["ledger_code"] == ledger && p["period"] == float64(number) {
				return p["fiscal_period_id"].(string)
			}
		}
	}
	t.Fatalf("year close %s: no period %d in ledger %s", s.path, number, ledger)
	return ""
}

// act moves a period through the ledger's own operation (:open, :close
// or :lock), as a person does.
func (s *yearSigning) act(t *testing.T, ledger string, number int, action string) {
	t.Helper()
	path := "/v1/fiscal-periods/" + s.period(t, ledger, number)
	_, tag := s.e.Must(200, "GET", path, "", "")
	s.e.Must(200, "POST", path+":"+action, "", tag)
	s.read(t)
}

// unlock has a locked period of the year unlocked through an approved
// request (T32), which returns it to closed.
func (s *yearSigning) unlock(t *testing.T, ledger string, number int) {
	t.Helper()
	path := "/v1/fiscal-periods/" + s.period(t, ledger, number)
	_, tag := s.e.Must(200, "GET", path, "", "")
	p, _ := s.e.Must(200, "POST", path+":unlock", `{"reason": "Audit adjustment"}`, tag)
	s.e.Approve(t, p["id"].(string))
	s.read(t)
}

// lockRegular closes and locks the regular periods 1 to last of 2026 of
// company 2000's ledger, as the period closes' sign-offs would.
func lockRegular(t *testing.T, e fixture.Env, ledger string, last int) {
	t.Helper()
	for _, move := range [][2]string{{"open", "closed"}, {"closed", "locked"}} {
		pgtest.Exec(t, `UPDATE ledger.period_status SET status = $5, version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
			AND ledger_id = $3 AND fiscal_year = 2026 AND period <= $4 AND status = $6`, e.Tenant.ID, e.DE, e.Ledger(t, ledger), last, move[1], move[0])
	}
}

// specialLines are a year sign-off's covered ledgers, one line each, with
// each special period and whether the sign-off locked it.
func specialLines(t *testing.T, signOff map[string]any) []string {
	t.Helper()
	var out []string
	for _, x := range apitest.List(t, signOff, "ledgers") {
		l := x.(map[string]any)
		line := fmt.Sprintf("%v %v leading=%v", l["ledger_code"], l["ledger_name"], l["is_leading"])
		for _, y := range apitest.List(t, l, "periods") {
			p := y.(map[string]any)
			line += fmt.Sprintf(" %v=%v", p["period"], p["locked"])
		}
		out = append(out, line)
	}
	return out
}

// special is a manual entry of company 2000 into special period 13 of
// 2026, dated its last day, in every ledger it keeps or, with extra, in
// one.
func special(t *testing.T, e fixture.Env, extra string) string {
	t.Helper()
	return entry(t, e, "2026-12-31", `, "fiscal_period": 13`+extra)
}

// TestYearSignOff: a year's sign-off is refused while a manual task is
// open, while a check fails when run again (keeping nothing), while a
// regular period of a covered ledger is not locked (naming each ledger
// with its periods) and while a special period is open (naming it with
// its ledgers); then it locks each closed special period, leaves one
// locked already as it is, records the signer's name, the note and the
// checks' results, and the year close is signed off: every period of the
// year is locked, a posting into period 13 is refused, and the close
// refuses every write and a second sign-off. The list names the signer;
// the sign-off is audited and published, and each lock is the ledger's.
func TestYearSignOff(t *testing.T) {
	e := fixture.NewWith(t, nil, withYearSignOff, signPermissions...)
	got, tag := openYear(t, e.Client, e.DE, "")
	s := &yearSigning{signing{e: e, path: "/v1/year-closes/" + apitest.String(t, got, "id"), tag: tag}}
	p := e.Problem(409, "closing.tasks_open", "POST", s.path+":signOff", "", s.tag)
	if want := "Manual task AP_CUTOFF, TAX is open; complete every manual task before signing off."; p["detail"] != want {
		t.Errorf("tasks open: got %v, want %s", p["detail"], want)
	}
	s.task(t, 200, "AP_CUTOFF:complete", "")
	s.task(t, 200, "TAX:complete", "")
	parked := e.Park(t, "2026-06-10", "June's rent", [2]string{"520000", "100.00"}, [2]string{"100000", "-100.00"})
	p = e.Problem(409, "closing.checks_failing", "POST", s.path+":signOff", "", s.tag)
	failing := apitest.List(t, p, "failing_checks")
	if len(failing) != 1 {
		t.Fatalf("failing checks: got %v, want one", failing)
	}
	wantFields(t, "the failing check", failing[0].(map[string]any), map[string]any{"task_code": "NO_PARKED", "check": "no_parked_entries",
		"summary": "1 parked entry dated 2026-01-01 to 2026-12-31 is parked or pending approval; post or cancel it."})
	if task := taskOf(t, s.read(t), "NO_PARKED"); task["check_result"] != nil {
		t.Errorf("a refused sign-off's check: got %v recorded, want nothing kept", task["check_result"])
	}
	e.Must(200, "POST", "/v1/parked-journal-entries/"+parked.String()+":cancel", `{"reason": "Not needed"}`, `"1"`)

	p = e.Problem(409, "closing.year_periods_not_locked", "POST", s.path+":signOff", "", s.tag)
	if want := "not locked: periods 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12 in ledger 0L; periods 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12 in ledger 2L."; !strings.Contains(
		p["detail"].(string), want) {
		t.Errorf("regular periods not locked: got %v, want %q", p["detail"], want)
	}
	lockRegular(t, e, "0L", 12)
	lockRegular(t, e, "2L", 11)
	p = e.Problem(409, "closing.year_periods_not_locked", "POST", s.path+":signOff", "", s.tag)
	if want := "not locked: period 12 in ledger 2L."; !strings.Contains(p["detail"].(string), want) {
		t.Errorf("2L's period 12 not locked: got %v, want %q", p["detail"], want)
	}
	lockRegular(t, e, "2L", 12)
	s.act(t, "0L", 13, "open")
	s.act(t, "2L", 13, "open")
	s.act(t, "2L", 15, "open")
	p = e.Problem(409, "closing.period_not_closed", "POST", s.path+":signOff", "", s.tag)
	if want := "Special period 2026/13 in ledger 0L, 2L; 2026/15 in ledger 2L is open"; !strings.HasPrefix(p["detail"].(string), want) {
		t.Errorf("special periods open: got %v, want %q", p["detail"], want)
	}
	s.act(t, "0L", 13, "close")
	s.act(t, "2L", 13, "close")
	s.act(t, "2L", 15, "close")
	s.act(t, "0L", 14, "lock")

	off := s.signOff(t, "2026 closed")
	wantLines(t, "the sign-off's ledgers", specialLines(t, off), []string{"0L Leading ledger leading=true 13=true 14=false 15=true 16=true",
		"2L HGB leading=false 13=true 14=true 15=true 16=true"})
	name := off["signed_by_name"]
	var stored string
	pgtest.Scan(t, `SELECT display_name FROM identity.principal WHERE id = $1`, []any{e.Tenant.Human.ID}, &stored)
	if name != stored || stored == "" {
		t.Errorf("signed_by_name: got %v, want the signer's name %q", name, stored)
	}
	wantFields(t, "the sign-off", off, map[string]any{"close_id": s.id(t).String(), "company_code": "2000", "company_name": "Acme GmbH",
		"fiscal_year": 2026.0, "signed_by": e.Tenant.Human.ID.String(), "note": "2026 closed"})
	if _, ok := off["fiscal_period"]; ok {
		t.Errorf("the sign-off: got fiscal_period %v, want none", off["fiscal_period"])
	}

	c := s.read(t)
	wantFields(t, "the year close signed off", c, map[string]any{"status": "signed_off", "signed_off_by": e.Tenant.Human.ID.String(),
		"signed_off_by_name": name, "signed_off_at": off["signed_at"]})
	for _, line := range yearLedgers(t, c) {
		if strings.Contains(line, "=open") || strings.Contains(line, "=closed") {
			t.Errorf("after the sign-off: got %s, want every period locked", line)
		}
	}
	if offs := apitest.List(t, c, "sign_offs"); len(offs) != 1 || offs[0].(map[string]any)["id"] != off["id"] {
		t.Errorf("the year close's sign-offs: got %v, want the one", offs)
	}
	if r := taskOf(t, c, "NO_PARKED")["check_result"].(map[string]any); r["passed"] != true {
		t.Errorf("the check run by the sign-off: got %v, want its passing result recorded", r)
	}
	list, _ := e.Must(200, "GET", "/v1/year-closes?status=signed_off", "", "")
	if items := apitest.List(t, list, "items"); len(items) != 1 || items[0].(map[string]any)["signed_off_by_name"] != name {
		t.Errorf("the list's signed-off year closes: got %v, want the one named", items)
	}

	e.Problem(409, "ledger.period_not_open", "POST", "/v1/journal-entries", special(t, e, ""), "")
	e.Problem(409, "closing.close_signed_off", "POST", s.path+":signOff", "", s.tag)
	e.Problem(409, "closing.close_signed_off", "POST", s.path+"/tasks/AP_CUTOFF:reopen", `{"reason": "Look again"}`, s.tag)

	var events []string
	for _, ev := range apitest.Published(t, e.Tenant, outbox.Event{Context: "closing", Name: "year_close_signed_off"}) {
		var periods []string
		for _, x := range ev["periods"].([]any) {
			q := x.(map[string]any)
			periods = append(periods, fmt.Sprintf("%v/%v=%v", q["ledger_code"], q["period"], q["locked"]))
		}
		events = append(events, fmt.Sprintf("%v %v %v %v %v %v", ev["year_close_id"], ev["sign_off_id"], ev["fiscal_year"], ev["note"], ev["status"],
			strings.Join(periods, " ")))
	}
	wantLines(t, "YearCloseSignedOff", events, []string{fmt.Sprintf("%v %v 2026 2026 closed signed_off 0L/13=true 0L/14=false 0L/15=true 0L/16=true "+
		"2L/13=true 2L/14=true 2L/15=true 2L/16=true", s.id(t), off["id"])})
	if n := len(apitest.Published(t, e.Tenant, outbox.Event{Context: "ledger", Name: "period_locked"})); n != 8 {
		t.Errorf("the ledger's PeriodLocked: got %d, want 8 (0L's 14 by Carla, then 7 by the sign-off)", n)
	}
	var audited int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'closing.year_close.sign_off'`, []any{e.Tenant.ID}, &audited)
	if audited != 1 {
		t.Errorf("audited sign-offs: got %d, want 1", audited)
	}
}

// TestYearSignOffAfterUnlock: a signed-off year close whose special period
// an approved unlock returned to closed is signed off again as it is; its
// regular period unlocked, the sign-off is refused until it is locked
// again; unlocked again, its first write (a task reopened) reopens it,
// audited and published before the write; a correcting entry is posted
// into the reopened special period, and a third sign-off locks it again.
func TestYearSignOffAfterUnlock(t *testing.T) {
	e := fixture.NewWith(t, nil, withYearSignOff, signPermissions...)
	s := yearSignable(t, e)
	lockRegular(t, e, "0L", 12)
	lockRegular(t, e, "2L", 12)
	s.read(t)
	s.signOff(t, "First")

	s.unlock(t, "2L", 13)
	off := s.signOff(t, "Second")
	wantLines(t, "signed off again", specialLines(t, off), []string{"0L Leading ledger leading=true 13=false 14=false 15=false 16=false",
		"2L HGB leading=false 13=true 14=false 15=false 16=false"})
	if c := s.read(t); c["status"] != "signed_off" || len(apitest.List(t, c, "sign_offs")) != 2 {
		t.Errorf("after the second sign-off: got %v with %d sign-offs, want signed_off with 2", c["status"], len(apitest.List(t, c, "sign_offs")))
	}
	s.unlock(t, "2L", 12)
	e.Problem(409, "closing.year_periods_not_locked", "POST", s.path+":signOff", "", s.tag)
	s.act(t, "2L", 12, "lock")

	s.unlock(t, "0L", 13)
	task := s.task(t, 200, "TAX:reopen", `{"reason": "A correcting entry is due"}`)
	wantFields(t, "the task reopened", task, map[string]any{"status": "open"})
	wantFields(t, "the year close reopened", s.read(t), map[string]any{"status": "open"})
	e.Problem(409, "closing.tasks_open", "POST", s.path+":signOff", "", s.tag)
	s.act(t, "0L", 13, "open")
	// 2L stays locked: the correction is the leading ledger's alone.
	e.Must(201, "POST", "/v1/journal-entries", special(t, e, fmt.Sprintf(`, "ledger_id": %q`, e.Ledger(t, "0L"))), "")
	s.act(t, "0L", 13, "close")
	s.task(t, 200, "TAX:complete", `{"note": "Correcting entry posted"}`)
	off = s.signOff(t, "Third")
	wantLines(t, "signed off a third time", specialLines(t, off), []string{"0L Leading ledger leading=true 13=true 14=false 15=false 16=false",
		"2L HGB leading=false 13=false 14=false 15=false 16=false"})

	var changes []string
	for _, ev := range apitest.Published(t, e.Tenant, outbox.Event{Context: "closing", Name: "year_close_changed"}) {
		changes = append(changes, fmt.Sprintf("%v %v %v", ev["change"], ev["task_code"], ev["status"]))
	}
	wantLines(t, "the changes after the unlock", changes[len(changes)-3:], []string{"reopened <nil> open", "task_reopened TAX open",
		"task_completed TAX open"})
	var signed []string
	for _, ev := range apitest.Published(t, e.Tenant, outbox.Event{Context: "closing", Name: "year_close_signed_off"}) {
		signed = append(signed, fmt.Sprint(ev["note"]))
	}
	wantLines(t, "the sign-offs published", signed, []string{"First", "Second", "Third"})
	var reopened string
	pgtest.Scan(t, `SELECT detail->>'unlocked_ledgers' FROM audit.audit_event WHERE tenant_id = $1 AND action = 'closing.year_close.reopen'`,
		[]any{e.Tenant.ID}, &reopened)
	if reopened != `["0L"]` {
		t.Errorf("the reopening audited: got %s, want the unlocked ledger 0L", reopened)
	}
}

// TestYearSignOffRefusals: a year's sign-off needs If-Match and a current
// version; agents do not sign off; a person who may not act in the close's
// company does not find it; a note is not empty.
func TestYearSignOffRefusals(t *testing.T) {
	e := fixture.NewWith(t, nil, withYearSignOff, signPermissions...)
	s := yearSignable(t, e)
	e.Problem(428, "request.precondition_required", "POST", s.path+":signOff", "", "")
	e.Problem(412, "request.version_mismatch", "POST", s.path+":signOff", "", `"1"`)
	e.As(e.Tenant.Agent).Problem(403, "identity.agent_not_allowed", "POST", s.path+":signOff", "", s.tag)
	e.As(e.Reader(t, "reader", e.US)).Problem(404, "request.not_found", "POST", s.path+":signOff", "", s.tag)
	e.Problem(400, "request.invalid_body", "POST", s.path+":signOff", `{"note": ""}`, s.tag)
}

// withYearThreeLedgers is withYearSignOff with company 2000 keeping three
// ledgers whose ids and codes sort differently (fixture.ThreeLedgers).
func withYearThreeLedgers(doc *config.Config) {
	withYearSignOff(doc)
	fixture.ThreeLedgers(doc)
}

// TestYearSignOffAndPostingLockInOneOrder runs a year's sign-off and a
// posting into special period 13 at once, on a company with three ledgers
// whose ids and codes sort differently. A third transaction holds a share
// lock, as a posting to those ledgers alone would, on period 13 of every
// ledger after the first k in the lock order: the sign-off locks every
// regular period and the first k ledgers' period 13 and waits; the
// posting, to every ledger, then waits for the sign-off without holding
// any period lock the sign-off needs next. Released, the sign-off locks the
// rest and commits, and the posting reads the period locked and is
// refused: in any other order one of them would deadlock.
func TestYearSignOffAndPostingLockInOneOrder(t *testing.T) {
	h := endpoints(t)
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	post := app.Ledger(app.Config{Pages: pages})
	day := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	for k := 1; k < len(lockOrder); k++ {
		t.Run(fmt.Sprintf("the first %d locked", k), func(t *testing.T) {
			e := fixture.NewWith(t, nil, withYearThreeLedgers, signPermissions...)
			for _, l := range lockOrder {
				lockRegular(t, e, l, 12)
			}
			s := yearSignable(t, e)
			create := gen.JournalEntryCreate{LegalEntityID: e.DE, DocumentDate: day, PostingDate: day, Currency: "EUR", FiscalPeriod: gen.NewOptInt32(13),
				Lines: []gen.JournalLineInput{{AccountID: e.Account(t, "2000", "520000"), Amount: gen.Money{Amount: "5.00", Currency: "EUR"}},
					{AccountID: e.Account(t, "2000", "100000"), Amount: gen.Money{Amount: "-5.00", Currency: "EUR"}}}}
			if got := yearLedgers(t, s.read(t)); len(got) != 3 || !strings.HasPrefix(got[0], "0L ") || !strings.HasPrefix(got[1], "0A ") {
				t.Fatalf("covered ledgers: got %v, want 0L, 0A, 1L", got)
			}
			var held []uuid.UUID
			for _, code := range lockOrder[k:] {
				held = append(held, uuid.MustParse(s.period(t, code, 13)))
			}
			holder := pgtest.Admin(t)
			ctx := context.Background()
			tx, err := holder.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }() // released below; a failed test releases it here
			var holderPID int32
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
				t.Fatal(err)
			}
			rows, _ := tx.Query(ctx, `SELECT id FROM ledger.period_status WHERE id = ANY($1) FOR SHARE`, held)
			if n, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID]); err != nil || len(n) != len(held) {
				t.Fatalf("hold the periods: got %v, %v", n, err)
			}

			signPID, signed := apitest.Started(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				_, err := h.SignOffYearClose(ctx, gen.OptPeriodCloseSignOffInput{}, gen.SignOffYearCloseParams{ID: s.id(t), IfMatch: s.tag})
				return err
			})
			signer := <-signPID
			apitest.WaitBlocked(t, signer, holderPID, signed, "the sign-off")
			postPID, posted := apitest.Started(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				_, err := post.PostJournalEntry(ctx, &create, gen.PostJournalEntryParams{})
				return err
			})
			apitest.WaitBlocked(t, <-postPID, signer, posted, "the posting")
			if err := tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-signed; err != nil {
				t.Errorf("sign-off: got %v, want it committed", err)
			}
			if err := <-posted; problem.From(err).Code != "ledger.period_not_open" {
				t.Errorf("posting after the sign-off: got %v, want ledger.period_not_open", err)
			}
		})
	}
}

// TestYearSignOffDatabaseFailures signs a year close off with each
// database statement failing in turn, and outside a unit of work, and
// reads it signed off the same way.
func TestYearSignOffDatabaseFailures(t *testing.T) {
	e := fixture.NewWith(t, nil, withYearSignOff, signPermissions...)
	s := yearSignable(t, e)
	lockRegular(t, e, "0L", 12)
	lockRegular(t, e, "2L", 12)
	s.read(t)
	h := endpoints(t)
	signOff := func(ctx context.Context) error {
		_, err := h.SignOffYearClose(ctx, gen.NewOptPeriodCloseSignOffInput(gen.PeriodCloseSignOffInput{Note: gen.NewOptString("n")}),
			gen.SignOffYearCloseParams{ID: s.id(t), IfMatch: s.tag})
		return err
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "sign off", signOff)
	if err := signOff(context.Background()); err == nil {
		t.Error("sign off outside a unit of work: got no error, want one")
	}
	s.signOff(t, "Signed")
	get := func(ctx context.Context) error {
		_, err := h.GetYearClose(ctx, gen.GetYearCloseParams{ID: s.id(t)})
		return err
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "get", get)
}
