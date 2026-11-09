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

// yearCheckPermissions are Carla's, who also parks entries, creates and
// depreciates assets, runs revaluations and carries the year forward in
// these tests.
var yearCheckPermissions = append(append([]string{}, depreciationPermissions...), "ledger.entry.park", "ledger.carry_forward.run")

// withYearChecks is withDepreciation (company 2000 keeping 0L and 2L,
// machines depreciated in both) with company 2000's retained earnings
// account 330000 and 2027 set up, so 2026 can be carried forward, and a
// year task for each year check (ADR-0060 §8).
func withYearChecks(doc *config.Config) {
	withDepreciation(doc)
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "330000", Name: "Retained earnings", Type: config.Equity})
	de.YearEnd.RetainedEarningsAccount = "330000"
	doc.NumberRanges = append(doc.NumberRanges, config.NumberRange{Company: "2000", DocumentType: "journal_sa", FiscalYear: 2027, First: 100000, Last: 999999})
	doc.Close.YearTasks = []config.CloseTask{
		{Code: "ADJUSTED", Name: "Auditors' adjustments entered", Kind: config.CloseManual, DueDay: 10},
		{Code: "NO_PARKED", Name: "No entry of the year left parked", Kind: config.CloseCheck, Check: "no_parked_entries", DueDay: 10,
			DependsOn: []string{"ADJUSTED"}},
		{Code: "REVALUED", Name: "Foreign currency revalued at the year's end", Kind: config.CloseCheck, Check: "revaluation_posted", DueDay: 1},
		{Code: "DEPRECIATED", Name: "Depreciation of every period posted", Kind: config.CloseCheck, Check: "depreciation_year_posted", DueDay: 1},
		{Code: "CARRIED", Name: "Balances carried forward", Kind: config.CloseCheck, Check: "carry_forward_current", DueDay: 15},
	}
}

// checkLines are a year check's result: whether it passed and its
// summary, then each ledger it names with its period, run and staleness.
func checkLines(t *testing.T, task map[string]any) []string {
	t.Helper()
	r, ok := task["check_result"].(map[string]any)
	if !ok {
		t.Fatalf("task %v: got no check result", task["code"])
	}
	out := []string{fmt.Sprintf("%v %v: %v", r["check"], r["passed"], r["summary"])}
	if n, ok := r["ledger_periods_count"]; ok {
		out[0] += fmt.Sprintf(" | %v failing", n)
	}
	for _, x := range apitest.List(t, r, "ledgers") {
		l := x.(map[string]any)
		line := fmt.Sprintf("%v %v posted=%v", l["ledger_code"], l["key_date"], l["posted"])
		for _, key := range []string{"fiscal_period", "assets_to_depreciate", "stale"} {
			if v, ok := l[key]; ok {
				line += fmt.Sprintf(" %s=%v", key, v)
			}
		}
		if opened, ok := l["opened_since"]; ok {
			var periods []string
			for _, y := range opened.([]any) {
				p := y.(map[string]any)
				periods = append(periods, fmt.Sprintf("%v/%v in %v", p["fiscal_year"], p["period"], p["ledger_code"]))
			}
			line += " opened since [" + strings.Join(periods, ", ") + "]"
		}
		out = append(out, line)
	}
	return out
}

// TestYearCloseNoParkedEntries: a year close's parked-entries check counts
// every entry of the company dated in the year waiting for a person, the
// one parked for special period 13 included (ADR-0060 §3, §8), and passes
// once they are cancelled; it waits for the task it depends on, and each
// run is published with whether it passed.
func TestYearCloseNoParkedEntries(t *testing.T) {
	e := fixture.NewWith(t, nil, withYearChecks, yearCheckPermissions...)
	got, tag := openYear(t, e.Client, e.DE, "")
	id := apitest.String(t, got, "id")
	base := "/v1/year-closes/" + id + "/tasks/"
	if task := taskOf(t, got, "CARRIED"); task["kind"] != "check" || task["check"] != "carry_forward_current" || task["check_result"] != nil {
		t.Errorf("a check task as opened: got %v %v %v, want a carry_forward_current check never run", task["kind"], task["check"], task["check_result"])
	}
	regular := e.Park(t, "2026-06-10", "June's rent", [2]string{"520000", "100.00"}, [2]string{"100000", "-100.00"})
	audit := e.Park(t, "2026-12-31", "Audit adjustment", [2]string{"520000", "25.00"}, [2]string{"100000", "-25.00"})
	e.Must(200, "PATCH", "/v1/parked-journal-entries/"+audit.String(), `{"fiscal_period": 13}`, `"1"`)

	e.Problem(409, "closing.task_blocked", "POST", base+"NO_PARKED:check", "", tag)
	e.Problem(409, "closing.task_not_check", "POST", base+"ADJUSTED:check", "", tag)
	_, tag = e.Must(200, "POST", base+"ADJUSTED:complete", "", tag)
	task, tag := e.Must(200, "POST", base+"NO_PARKED:check", "", tag)
	name := task["check_result"].(map[string]any)["checked_by_name"]
	want := fmt.Sprintf("no_parked_entries passed=false by %v: 2 parked entries dated 2026-01-01 to 2026-12-31 are parked or pending approval; "+
		"post or cancel them. | 2: June's rent parked 2026-06-10 %v/human Audit adjustment parked 2026-12-31 %v/human", name, name, name)
	if got := result(t, task); got != want {
		t.Errorf("failing:\ngot  %s\nwant %s", got, want)
	}
	wantFields(t, "failing", task, map[string]any{"status": "open", "close_version": 3.0})

	for _, p := range []uuid.UUID{regular, audit} {
		var version int
		pgtest.Scan(t, `SELECT version FROM ledger.parked_entry WHERE id = $1`, []any{p}, &version)
		e.Must(200, "POST", "/v1/parked-journal-entries/"+p.String()+":cancel", `{"reason": "Not needed"}`, fmt.Sprintf(`"%d"`, version))
	}
	task, _ = e.Must(200, "POST", base+"NO_PARKED:check", "", tag)
	if got, want := result(t, task), fmt.Sprintf("no_parked_entries passed=true by %v: No parked entry dated 2026-01-01 to 2026-12-31 is parked "+
		"or pending approval. | 0:", name); got != want {
		t.Errorf("passing:\ngot  %s\nwant %s", got, want)
	}
	wantFields(t, "passing", task, map[string]any{"status": "done", "completed_by_name": name})

	var changes []string
	for _, ev := range apitest.Published(t, e.Tenant, outbox.Event{Context: "closing", Name: "year_close_changed"}) {
		changes = append(changes, fmt.Sprintf("%v %v %v", ev["change"], ev["task_code"], ev["check_passed"]))
	}
	wantLines(t, "events", changes, []string{"created <nil> <nil>", "task_completed ADJUSTED <nil>", "task_checked NO_PARKED false",
		"task_checked NO_PARKED true"})
	var audited string
	pgtest.Scan(t, `SELECT string_agg(detail->>'passed', ' ' ORDER BY seq) FROM audit.audit_event WHERE tenant_id = $1
		AND action = 'closing.year_close.check_task'`, []any{e.Tenant.ID}, &audited)
	if audited != "false true" {
		t.Errorf("audited checks: got %q, want false true", audited)
	}
}

// TestYearCloseRevaluationPosted: a year close's revaluation check fails
// naming each covered ledger without a posted run keyed on the year's last
// day, a run of another day not counting, and passes once each has one.
func TestYearCloseRevaluationPosted(t *testing.T) {
	e := fixture.NewWith(t, nil, withYearChecks, yearCheckPermissions...)
	e.Revalues(t, withYearChecks)
	got, tag := openYear(t, e.Client, e.DE, "")
	base := "/v1/year-closes/" + apitest.String(t, got, "id") + "/tasks/REVALUED:check"
	revalue := func(ledger, day string) {
		e.Must(201, "POST", "/v1/revaluation-runs", fmt.Sprintf(`{"legal_entity_id": %q, "ledger_id": %q, "key_date": %q}`, e.DE, e.Ledger(t, ledger), day), "")
	}
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'bank_selling', 'USD', 'EUR', '2026-12-15', 0.92, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	revalue(config.LeadingLedger, "2026-03-31")
	task, tag := e.Must(200, "POST", base, "", tag)
	wantLines(t, "a run of March only", checkLines(t, task), []string{
		"revaluation_posted false: No posted revaluation run at the year's last day in ledger 0L, 2L; run the revaluation first.",
		"0L 2026-12-31 posted=false", "2L 2026-12-31 posted=false"})
	revalue(config.LeadingLedger, "2026-12-31")
	revalue("2L", "2026-12-31")
	task, _ = e.Must(200, "POST", base, "", tag)
	wantLines(t, "a run of the year's last day in each ledger", checkLines(t, task), []string{
		"revaluation_posted true: Every covered ledger has a posted revaluation run at its year's last day.",
		"0L 2026-12-31 posted=true", "2L 2026-12-31 posted=true"})
	wantFields(t, "passing", task, map[string]any{"status": "done"})
}

// TestYearCloseDepreciationPosted: a year close's depreciation check
// passes with nothing to depreciate; with a machine capitalised in January
// it fails in every regular period of each covered ledger, counting them
// and naming the first 20; once December's run has posted in a ledger,
// which catches up the year, that ledger passes; once in each, the check
// passes.
func TestYearCloseDepreciationPosted(t *testing.T) {
	e := fixture.NewWith(t, nil, withYearChecks, yearCheckPermissions...)
	got, tag := openYear(t, e.Client, e.DE, "")
	base := "/v1/year-closes/" + apitest.String(t, got, "id") + "/tasks/DEPRECIATED:check"
	task, tag := e.Must(200, "POST", base, "", tag)
	wantLines(t, "nothing to depreciate", checkLines(t, task), []string{"depreciation_year_posted true: Every covered ledger has a posted depreciation " +
		"run, nothing to depreciate or a posted run of a later period in each regular period of 2026. | 0 failing"})

	machine(t, e)
	task, tag = e.Must(200, "POST", base, "", tag)
	lines := checkLines(t, task)
	wantLines(t, "a machine to depreciate", []string{lines[0], lines[1], lines[12], lines[13], lines[20]}, []string{
		"depreciation_year_posted false: No posted depreciation run of 2026 in ledger 0L (period 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12), " +
			"2L (period 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12); run the depreciation of those periods first. | 24 failing",
		"0L 2026-01-31 posted=false fiscal_period=1 assets_to_depreciate=1", "0L 2026-12-31 posted=false fiscal_period=12 assets_to_depreciate=1",
		"2L 2026-01-31 posted=false fiscal_period=1 assets_to_depreciate=1", "2L 2026-08-31 posted=false fiscal_period=8 assets_to_depreciate=1"})
	if len(lines) != 21 {
		t.Errorf("the periods named: got %d, want the first 20", len(lines)-1)
	}

	if _, status := depreciateIn(t, e, e.Ledger(t, config.LeadingLedger), 12, false); status != "posted" {
		t.Fatalf("December's run in 0L: got %s, want posted", status)
	}
	task, tag = e.Must(200, "POST", base, "", tag)
	wantLines(t, "December's run in 0L", checkLines(t, task)[:2], []string{
		"depreciation_year_posted false: No posted depreciation run of 2026 in ledger 2L (period 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12); " +
			"run the depreciation of those periods first. | 12 failing",
		"2L 2026-01-31 posted=false fiscal_period=1 assets_to_depreciate=1"})

	if _, status := depreciateIn(t, e, e.Ledger(t, "2L"), 12, false); status != "posted" {
		t.Fatalf("December's run in 2L: got %s, want posted", status)
	}
	task, _ = e.Must(200, "POST", base, "", tag)
	wantLines(t, "December's run in each ledger", checkLines(t, task), []string{"depreciation_year_posted true: Every covered ledger has a posted " +
		"depreciation run, nothing to depreciate or a posted run of a later period in each regular period of 2026. | 0 failing"})
	wantFields(t, "passing", task, map[string]any{"status": "done"})
}

// TestYearCloseDepreciationLate: a machine capitalised after December's
// run posted is late for each of the 11 periods before December in the
// ledger: the year check passes with one warning of it, its depreciation
// missing from January, not one per period.
func TestYearCloseDepreciationLate(t *testing.T) {
	e := fixture.NewWith(t, nil, withYearChecks, yearCheckPermissions...)
	got, tag := openYear(t, e.Client, e.DE, ",\"ledger_ids\": [\""+e.Ledger(t, config.LeadingLedger).String()+"\"]")
	if _, status := depreciateIn(t, e, e.Ledger(t, config.LeadingLedger), 12, false); status != "posted" {
		t.Fatalf("December's run: got %s, want posted", status)
	}
	machine(t, e)
	task, _ := e.Must(200, "POST", "/v1/year-closes/"+apitest.String(t, got, "id")+"/tasks/DEPRECIATED:check", "", tag)
	r := task["check_result"].(map[string]any)
	wantFields(t, "a late machine", r, map[string]any{"passed": true, "warnings_count": 1.0, "ledger_periods_count": 0.0})
	if w := apitest.List(t, r, "warnings"); len(w) != 1 || w[0].(map[string]any)["missing_from_period"] != 1.0 {
		t.Errorf("warnings: got %v, want one, the machine's from period 1", w)
	}
	if s := r["summary"].(string); !strings.HasSuffix(s, " See the warnings.") {
		t.Errorf("summary: got %q, want it to point at the warnings", s)
	}
}

// TestYearCloseCarryForwardCurrent: a year close's carry-forward check
// fails naming each covered ledger without a current carry-forward from
// the year; once the year is carried forward it passes naming the runs;
// a period of the year opened since makes that ledger's run stale, which
// fails it naming the period, closed again or not; a re-run passes.
func TestYearCloseCarryForwardCurrent(t *testing.T) {
	e := fixture.NewWith(t, nil, withYearChecks, yearCheckPermissions...)
	got, tag := openYear(t, e.Client, e.DE, "")
	base := "/v1/year-closes/" + apitest.String(t, got, "id") + "/tasks/CARRIED:check"
	task, tag := e.Must(200, "POST", base, "", tag)
	wantLines(t, "nothing carried", checkLines(t, task), []string{
		"carry_forward_current false: No carry-forward from 2026 in ledger 0L, 2L; carry 2026 forward.",
		"0L 2026-12-31 posted=false", "2L 2026-12-31 posted=false"})

	set := func(status string, periods string) {
		pgtest.Exec(t, `UPDATE ledger.period_status SET status = $3, version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
			AND fiscal_year = 2026 AND status <> $3 AND ledger_id = ANY($4) AND period <= 16`, e.Tenant.ID, e.DE, status,
			map[string][]uuid.UUID{"all": {e.Ledger(t, config.LeadingLedger), e.Ledger(t, "2L")}, "2L": {e.Ledger(t, "2L")}}[periods])
	}
	set("closed", "all")
	e.Must(201, "POST", "/v1/balance-carry-forwards", fmt.Sprintf(`{"legal_entity_id": %q, "fiscal_year": 2026}`, e.DE), "")
	task, tag = e.Must(200, "POST", base, "", tag)
	wantLines(t, "carried forward", checkLines(t, task), []string{
		"carry_forward_current true: Every covered ledger has a current carry-forward from 2026 that is not stale.",
		"0L 2026-12-31 posted=true stale=false opened since []", "2L 2026-12-31 posted=true stale=false opened since []"})
	wantFields(t, "carried forward", task, map[string]any{"status": "done"})

	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'open', version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
		AND fiscal_year = 2026 AND period = 13 AND ledger_id = $3`, e.Tenant.ID, e.DE, e.Ledger(t, "2L"))
	task, tag = e.Must(200, "POST", base, "", tag)
	stale := []string{
		"carry_forward_current false: The carry-forward from 2026 is stale in ledger 2L (2026/13 opened since); close those periods and carry 2026 " +
			"forward again.",
		"0L 2026-12-31 posted=true stale=false opened since []", "2L 2026-12-31 posted=true stale=true opened since [2026/13 in 2L]"}
	wantLines(t, "period 13 of 2L opened", checkLines(t, task), stale)
	wantFields(t, "period 13 of 2L opened", task, map[string]any{"status": "open"})
	set("closed", "2L")
	task, tag = e.Must(200, "POST", base, "", tag)
	wantLines(t, "period 13 of 2L closed again", checkLines(t, task), stale)

	e.Must(201, "POST", "/v1/balance-carry-forwards", fmt.Sprintf(`{"legal_entity_id": %q, "fiscal_year": 2026}`, e.DE), "")
	task, _ = e.Must(200, "POST", base, "", tag)
	if got := checkLines(t, task)[0]; got != "carry_forward_current true: Every covered ledger has a current carry-forward from 2026 that is not stale." {
		t.Errorf("carried forward again: got %q, want passing", got)
	}
	if id := apitest.List(t, task["check_result"].(map[string]any), "ledgers")[0].(map[string]any)["carry_forward_run_id"]; id == nil {
		t.Error("carried forward again: got no run named, want 0L's")
	}
}

// TestYearCheckDatabaseFailures runs each year check with each of its
// database statements failing in turn, with something to find, and
// outside a unit of work; and fails a check this build does not run on a
// year close, or whose assets context does not answer, rather than
// recording a result.
func TestYearCheckDatabaseFailures(t *testing.T) {
	e := fixture.NewWith(t, nil, withYearChecks, yearCheckPermissions...)
	got, tag := openYear(t, e.Client, e.DE, "")
	id := uuid.MustParse(apitest.String(t, got, "id"))
	_, tag = e.Must(200, "POST", "/v1/year-closes/"+id.String()+"/tasks/ADJUSTED:complete", "", tag)
	e.Park(t, "2026-06-10", "June's rent", [2]string{"520000", "100.00"}, [2]string{"100000", "-100.00"})
	depreciateIn(t, e, e.Ledger(t, config.LeadingLedger), 12, false)
	machine(t, e) // late in 0L, whose December run posted; to depreciate in 2L
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
		AND fiscal_year = 2026 AND status = 'open'`, e.Tenant.ID, e.DE)
	e.Must(201, "POST", "/v1/balance-carry-forwards", fmt.Sprintf(`{"legal_entity_id": %q, "fiscal_year": 2026}`, e.DE), "")
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'open', version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
		AND fiscal_year = 2026 AND period = 13`, e.Tenant.ID, e.DE)
	h := endpoints(t)
	check := func(code string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := h.CheckYearCloseTask(ctx, gen.CheckYearCloseTaskParams{ID: id, Code: code, IfMatch: tag})
			return err
		}
	}
	for _, code := range []string{"NO_PARKED", "REVALUED", "DEPRECIATED", "CARRIED"} {
		t.Run(code, func(t *testing.T) {
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, code, check(code))
			if err := check(code)(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
		})
	}
	h.Depreciation = brokenDepreciation{}
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, check("DEPRECIATED"))
	if err == nil || !strings.Contains(err.Error(), "closing: depreciation of ledger 0L in 2026/1: the assets context is down") {
		t.Errorf("the assets context failing: got %v, want its error", err)
	}
	pgtest.Unguarded(t, `UPDATE closing.year_close_task SET check_kind = 'variances_explained' WHERE close_id = $1 AND code = 'REVALUED'`, id)
	err = apitest.InRequest(t, e.Tenant, e.Tenant.Human, check("REVALUED"))
	if err == nil || !strings.Contains(err.Error(), `"variances_explained", which this build does not run on a year close`) {
		t.Errorf("a period check on a year close: got %v, want the unknown check's failure", err)
	}
}
