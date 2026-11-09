package consolidation_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/consolidation"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

const runsPath = "/v1/consolidation-runs"

// TestConsolidationRunRefusals (ADR-0059 §6): a run of a group the caller
// may not see whole, of a special period, of a period the parent does not
// have, with a member included on another calendar, with a stale closing
// rate, while another run of the group is queued, of a group whose parent
// is not in the group currency and with a missing closing rate is
// refused, each with its problem on the field.
func TestConsolidationRunRefusals(t *testing.T) {
	e := runEnv(t)
	start := func(group string, year, period int) string {
		return fmt.Sprintf(`{"group_code": %q, "fiscal_year": %d, "period": %d}`, group, year, period)
	}
	reader := e.As(e.Reader(t, "Reader of 2000", e.DE))
	reader.Problem(404, "consolidation.group_unknown", "POST", runsPath, start("ACME", 2026, 11), "")
	e.Problem(404, "consolidation.group_unknown", "POST", runsPath, start("NONE", 2026, 11), "")
	e.Problem(422, "consolidation.special_period", "POST", runsPath, start("ACME", 2026, 13), "")
	e.Problem(422, "consolidation.period_unknown", "POST", runsPath, start("ACME", 2027, 1), "")
	e.Problem(422, "consolidation.calendar_mismatch", "POST", runsPath, start("ACME", 2026, 11), "")
	e.Must(202, "POST", runsPath, start("ACME", 2026, 10), "") // 1000 is not included before 2026-11
	sameCalendar(t, e)
	p := e.Problem(409, "consolidation.run_in_progress", "POST", runsPath, start("ACME", 2026, 11), "")
	if !strings.Contains(p["detail"].(string), runsPath+"/") {
		t.Errorf("the run in progress: got %v, want its path named", p["detail"])
	}
	pgtest.Exec(t, `UPDATE consolidation.run SET status = 'failed', problem = '{"code": "x", "detail": "x"}', version = version + 1 WHERE tenant_id = $1`,
		e.Tenant.ID)
	p = e.Problem(422, "consolidation.rate_stale", "POST", runsPath, start("ACME", 2026, 12), "")
	if !strings.Contains(p["detail"].(string), "USD/EUR on 2026-12-31") {
		t.Errorf("the stale rate: got %v, want the pair and the date named", p["detail"])
	}
	t.Run("the parent in another currency", func(t *testing.T) {
		e := fixture.NewWith(t, nil, func(doc *config.Config) {
			withRuns(doc)
			g := &doc.Consolidation.Groups[0]
			g.Parent, g.Members[0].Company, g.Members[0].InvestmentAccount = "1000", "2000", "100000"
		}, runPermissions...)
		e.Problem(422, "consolidation.parent_currency", "POST", runsPath, start("ACME", 2026, 11), "")
	})
	t.Run("no closing rate", func(t *testing.T) {
		e := fixture.NewWith(t, nil, func(doc *config.Config) {
			withRuns(doc)
			doc.Consolidation.RateType = "planning"
		}, runPermissions...)
		sameCalendar(t, e)
		p := e.Problem(422, "consolidation.rate_missing", "POST", runsPath, start("ACME", 2026, 11), "")
		if !strings.Contains(p["detail"].(string), "planning rate USD/EUR is valid on 2026-11-30") {
			t.Errorf("the missing rate: got %v, want the type, pair and date named", p["detail"])
		}
	})
}

// TestConsolidationRunLife (ADR-0059 §5, §6): a computed run is discarded
// with its ETag (412 with another, 409 once discarded); a queued run is
// not discardable; runs are listed newest first by group, year, period and
// status, paged; a caller who may not act in every company of the group
// finds no run; each change of status is published and audited.
func TestConsolidationRunLife(t *testing.T) {
	e := runEnv(t)
	sameCalendar(t, e)
	h := app.Consolidation(app.Config{})
	first, _ := start(t, e)
	carryOut(t, e, h, first)
	second, _ := start(t, e)
	path := runsPath + "/" + second.String()
	e.Problem(409, "consolidation.run_not_discardable", "POST", path+":discard", "", `"1"`)
	carryOut(t, e, h, second)
	e.Problem(412, "request.version_mismatch", "POST", path+":discard", "", `"1"`)
	got, tag := e.Must(200, "POST", path+":discard", "", `"3"`)
	if s := fmt.Sprint(got["status"], " ", tag, " ", len(got["members"].([]any))); s != `discarded "4" 2` {
		t.Errorf("the discarded run: got %s, want discarded at \"4\" with its members", s)
	}
	e.Problem(409, "consolidation.run_not_discardable", "POST", path+":discard", "", `"4"`)
	list := func(query string) string {
		page, _ := e.Must(200, "GET", runsPath+"?"+query, "", "")
		var ids []string
		for _, it := range page["items"].([]any) {
			ids = append(ids, it.(map[string]any)["id"].(string))
		}
		if tok, ok := page["next_page_token"].(string); ok {
			ids = append(ids, "next:"+tok[:1])
		}
		return strings.Join(ids, " ")
	}
	for _, c := range []struct{ query, want string }{
		{"group_code=ACME&fiscal_year=2026&period=11", second.String() + " " + first.String()},
		{"status=computed", first.String()},
		{"status=discarded", second.String()},
		{"period=10", ""},
		{"group_code=NONE", ""},
		{"fiscal_year=2025", ""},
	} {
		if got := list(c.query); got != c.want {
			t.Errorf("runs of %s: got %s, want %s", c.query, got, c.want)
		}
	}
	page, _ := e.Must(200, "GET", runsPath+"?page_size=1", "", "")
	next, _ := e.Must(200, "GET", runsPath+"?page_size=1&page_token="+page["next_page_token"].(string), "", "")
	if got := fmt.Sprint(page["items"].([]any)[0].(map[string]any)["id"], " ", next["items"].([]any)[0].(map[string]any)["id"], " ", next["next_page_token"]); got !=
		second.String()+" "+first.String()+" <nil>" {
		t.Errorf("the pages: got %s, want the second run, then the first and no more", got)
	}
	reader := e.As(e.Reader(t, "Reader of 2000", e.DE))
	reader.Problem(404, "request.not_found", "GET", runsPath+"/"+first.String(), "", "")
	reader.Problem(404, "request.not_found", "GET", runsPath+"/"+first.String()+"/balances", "", "")
	reader.Problem(404, "request.not_found", "POST", runsPath+"/"+first.String()+":discard", "", `"3"`)
	if p, _ := reader.Must(200, "GET", runsPath, "", ""); len(p["items"].([]any)) != 0 {
		t.Errorf("the runs a reader of 2000 lists: got %v, want none", p["items"])
	}
	var events, audited string
	pgtest.Scan(t, `SELECT string_agg(payload->>'status_before' || '>' || (payload->>'status'), ' ' ORDER BY occurred_at, id) FROM consolidation.outbox
		WHERE tenant_id = $1 AND aggregate_id = $2`, []any{e.Tenant.ID, second}, &events)
	pgtest.Scan(t, `SELECT string_agg(action, ' ' ORDER BY occurred_at, seq) FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2`,
		[]any{e.Tenant.ID, second}, &audited)
	if events != "queued>running running>computed computed>discarded" {
		t.Errorf("the second run's events: got %s, want each change of status", events)
	}
	if audited != "consolidation.run.queued consolidation.run.running consolidation.run.computed consolidation.run.discarded" {
		t.Errorf("the second run's audit records: got %s, want each change of status", audited)
	}
}

// TestConsolidationRunStops: a run whose Operation is cancelled before
// its preparation is refused at it with consolidation.run_cancelled and
// failed, its Operation left cancelled; a run whose closing rate is
// stale when it is prepared, or whose member has no period when it is
// translated, fails with that problem, its Operation failed with it.
func TestConsolidationRunStops(t *testing.T) {
	e := runEnv(t)
	sameCalendar(t, e)
	h := app.Consolidation(app.Config{})
	committed := func(fn func(ctx context.Context) error) error {
		return apitest.Committed(t, e.Tenant, e.Tenant.Human, fn)
	}
	state := func(run uuid.UUID) string {
		var s string
		pgtest.Scan(t, `SELECT r.status || ' ' || coalesce(r.problem->>'code', '') || ' ' || o.status FROM consolidation.run r
			JOIN platform.operation o ON o.id = r.operation_id WHERE r.id = $1`, []any{run}, &s)
		return s
	}
	fail := func(run uuid.UUID, err error) {
		t.Helper()
		var why *problem.Problem
		if !errors.As(err, &why) {
			t.Fatalf("got %v, want a problem", err)
		}
		if err := committed(func(ctx context.Context) error { return h.FailConsolidationRun(ctx, run, why) }); err != nil {
			t.Fatal(err)
		}
	}
	run, op := start(t, e)
	_, tag := e.Must(200, "GET", "/v1/operations/"+op.String(), "", "")
	e.Must(200, "POST", "/v1/operations/"+op.String()+":cancel", "", tag)
	fail(run, committed(func(ctx context.Context) error { _, err := h.PrepareConsolidationRun(ctx, run); return err }))
	if got := state(run); got != "failed consolidation.run_cancelled cancelled" {
		t.Errorf("the cancelled run: got %s, want failed with consolidation.run_cancelled, its Operation cancelled", got)
	}
	stale := app.Consolidation(app.Config{})
	stale.Ledger = staleRates{stale.Ledger}
	run, _ = start(t, e)
	fail(run, committed(func(ctx context.Context) error { _, err := stale.PrepareConsolidationRun(ctx, run); return err }))
	if got := state(run); got != "failed consolidation.rate_stale failed" {
		t.Errorf("the run whose rate went stale: got %s, want it and its Operation failed with consolidation.rate_stale", got)
	}
	run, _ = start(t, e)
	var companies []uuid.UUID
	if err := committed(func(ctx context.Context) error {
		var err error
		companies, err = h.PrepareConsolidationRun(ctx, run)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	post(t, e, e.DE, "2000", "EUR", "2026-11-05", "100000", "400000", "100.00", nil)
	unmapped := app.Consolidation(app.Config{})
	unmapped.Ledger = renamed{unmapped.Ledger}
	var why *problem.Problem
	if err := committed(func(ctx context.Context) error { return unmapped.TranslateMember(ctx, run, companies[1]) }); !errors.As(err, &why) ||
		why.Code != "consolidation.account_unmapped" {
		t.Errorf("a company account without a group account: got %v, want consolidation.account_unmapped", err)
	}
	gone := app.Consolidation(app.Config{})
	gone.Ledger = noPeriod{gone.Ledger}
	fail(run, committed(func(ctx context.Context) error { return gone.TranslateMember(ctx, run, companies[0]) }))
	if got := state(run); got != "failed consolidation.period_unknown failed" {
		t.Errorf("the run whose member has no period: got %s, want it failed with consolidation.period_unknown", got)
	}
	for name, step := range map[string]func(ctx context.Context) error{
		"prepare":   func(ctx context.Context) error { _, err := h.PrepareConsolidationRun(ctx, run); return err },
		"translate": func(ctx context.Context) error { return h.TranslateMember(ctx, run, companies[0]) },
		"eliminate": func(ctx context.Context) error { return h.EliminateConsolidationRun(ctx, run) },
		"complete":  func(ctx context.Context) error { return h.CompleteConsolidationRun(ctx, run) },
		"fail":      func(ctx context.Context) error { return h.FailConsolidationRun(ctx, run, problem.Conflict("x", "x")) },
	} {
		if err := committed(step); err != nil {
			t.Errorf("%s on a failed run: got %v, want it left alone", name, err)
		}
	}
	if got := state(run); got != "failed consolidation.period_unknown failed" {
		t.Errorf("the failed run after the steps: got %s, want it unchanged", got)
	}
	if err := committed(func(ctx context.Context) error { _, err := h.PrepareConsolidationRun(ctx, uuid.New()); return err }); err == nil {
		t.Error("an unknown run's preparation: got no error, want not found")
	}
}

// TestConsolidationRunIncomplete: a run is not completed while a company
// is not translated, and a company translated twice is written once.
func TestConsolidationRunIncomplete(t *testing.T) {
	e := runEnv(t)
	sameCalendar(t, e)
	h := app.Consolidation(app.Config{})
	committed := func(fn func(ctx context.Context) error) error {
		return apitest.Committed(t, e.Tenant, e.Tenant.Human, fn)
	}
	run, _ := start(t, e)
	var companies []uuid.UUID
	if err := committed(func(ctx context.Context) error {
		var err error
		companies, err = h.PrepareConsolidationRun(ctx, run)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := committed(func(ctx context.Context) error { return h.CompleteConsolidationRun(ctx, run) }); err == nil ||
		!strings.Contains(err.Error(), "2 companies not translated") {
		t.Errorf("completing before the translation: got %v, want it refused", err)
	}
	for range 2 {
		if err := committed(func(ctx context.Context) error { return h.TranslateMember(ctx, run, companies[0]) }); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM consolidation.run_member WHERE run_id = $1 AND translated_at IS NOT NULL`, []any{run}, &n)
	if n != 1 {
		t.Errorf("translated companies: got %d, want 1", n)
	}
}

// staleRates is the ledger with every rate valid from long ago.
type staleRates struct{ consolidation.Ledger }

func (s staleRates) RateOn(ctx context.Context, rateType, from, to string, date time.Time) (consolidation.Rate, bool, error) {
	r, ok, err := s.Ledger.RateOn(ctx, rateType, from, to, date)
	r.ValidFrom = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	return r, ok, err
}

// renamed is the ledger with every account renumbered, so none has a
// group account in the configuration.
type renamed struct{ consolidation.Ledger }

func (r renamed) Accounts(ctx context.Context, company uuid.UUID) ([]consolidation.Account, error) {
	as, err := r.Ledger.Accounts(ctx, company)
	for i := range as {
		as[i].Number = "X" + as[i].Number
	}
	return as, err
}

// noPeriod is the ledger with no company's period.
type noPeriod struct{ consolidation.Ledger }

func (noPeriod) PeriodOf(context.Context, uuid.UUID, int16, int16) (consolidation.Period, bool, error) {
	return consolidation.Period{}, false, nil
}

// TestConsolidationDatabaseFailures: every database statement of a run's
// requests and steps failing fails it, never hidden; outside a unit of
// work each refuses.
func TestConsolidationDatabaseFailures(t *testing.T) {
	e := runEnv(t)
	sameCalendar(t, e)
	figures(t, e)
	h := app.Consolidation(app.Config{})
	// fresh is the context with no configuration version kept, so each
	// failing call reads the run's version too (versionCache).
	fresh := func() *consolidation.Endpoints { return app.Consolidation(app.Config{}) }
	ctx := context.Background()
	start := &gen.ConsolidationRunStart{GroupCode: "ACME", FiscalYear: 2026, Period: 11}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "create", func(ctx context.Context) error {
		_, err := h.CreateConsolidationRun(ctx, start, gen.CreateConsolidationRunParams{})
		return err
	})
	run, _ := startRun(t, e)
	steps := []struct {
		name string
		fn   func(ctx context.Context) error
	}{
		{"prepare", func(ctx context.Context) error { _, err := h.PrepareConsolidationRun(ctx, run); return err }},
	}
	every := func(name string, fn func(ctx context.Context) error) {
		t.Helper()
		apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, fn)
		if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, fn); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	every(steps[0].name, steps[0].fn)
	var companies []uuid.UUID
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		var err error
		companies, err = h.PrepareConsolidationRun(ctx, run)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range companies {
		every("translate", func(ctx context.Context) error { return fresh().TranslateMember(ctx, run, c) })
	}
	every("eliminate", func(ctx context.Context) error { return h.EliminateConsolidationRun(ctx, run) })
	every("complete", func(ctx context.Context) error { return h.CompleteConsolidationRun(ctx, run) })
	reads := map[string]func(ctx context.Context) error{
		"get": func(ctx context.Context) error {
			_, err := fresh().GetConsolidationRun(ctx, gen.GetConsolidationRunParams{ID: run})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := fresh().ListConsolidationRuns(ctx, gen.ListConsolidationRunsParams{})
			return err
		},
		"balances": func(ctx context.Context) error {
			_, err := fresh().ListConsolidationBalances(ctx, gen.ListConsolidationBalancesParams{ID: run})
			return err
		},
		"statement": func(ctx context.Context) error {
			_, err := fresh().GetConsolidatedStatement(ctx, gen.GetConsolidatedStatementParams{ID: run, Statement: "balance_sheet"})
			return err
		},
		"differences": func(ctx context.Context) error {
			_, err := fresh().ListIntercompanyDifferences(ctx, gen.ListIntercompanyDifferencesParams{ID: run})
			return err
		},
		"entries": func(ctx context.Context) error {
			_, err := fresh().ListConsolidationEntries(ctx, gen.ListConsolidationEntriesParams{ID: run})
			return err
		},
		"discard": func(ctx context.Context) error {
			_, err := fresh().DiscardConsolidationRun(ctx, gen.DiscardConsolidationRunParams{ID: run, IfMatch: `"3"`})
			return err
		},
	}
	for name, fn := range reads {
		apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, fn)
		if err := fn(ctx); err == nil {
			t.Errorf("%s outside a unit of work: got no error, want one", name)
		}
	}
	failed, _ := startRun(t, e)
	every("fail", func(ctx context.Context) error {
		return h.FailConsolidationRun(ctx, failed, problem.Conflict("x", "x"))
	})
	for name, fn := range map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreateConsolidationRun(ctx, start, gen.CreateConsolidationRunParams{})
			return err
		},
		"prepare":   func(ctx context.Context) error { _, err := h.PrepareConsolidationRun(ctx, run); return err },
		"translate": func(ctx context.Context) error { return h.TranslateMember(ctx, run, companies[0]) },
		"eliminate": func(ctx context.Context) error { return h.EliminateConsolidationRun(ctx, run) },
		"complete":  func(ctx context.Context) error { return h.CompleteConsolidationRun(ctx, run) },
		"fail":      func(ctx context.Context) error { return h.FailConsolidationRun(ctx, run, problem.Conflict("x", "x")) },
	} {
		if err := fn(ctx); err == nil {
			t.Errorf("%s outside a unit of work: got no error, want one", name)
		}
	}
}

// startRun starts November's run of ACME through the API.
func startRun(t *testing.T, e fixture.Env) (uuid.UUID, uuid.UUID) {
	t.Helper()
	return start(t, e)
}

// TestConsolidationRunReads: a failed run shows its problem; the lists
// refuse a page size or token they did not give; two runs of a group
// started at once meet the database's one-at-a-time rule as
// consolidation.run_in_progress; a group of two members is checked in
// company code order (Maple, 3000, on its own calendar, is refused before
// 1000).
func TestConsolidationRunReads(t *testing.T) {
	e := runEnv(t)
	sameCalendar(t, e)
	run, _ := start(t, e)
	pgtest.Exec(t, `UPDATE consolidation.run SET status = 'failed', problem = '{"code": "consolidation.rate_stale", "detail": "Stale."}',
		version = version + 1 WHERE id = $1`, run)
	got, _ := e.Must(200, "GET", runsPath+"/"+run.String(), "", "")
	if p := fmt.Sprint(got["status"], " ", got["problem"]); p != "failed map[code:consolidation.rate_stale detail:Stale.]" {
		t.Errorf("the failed run: got %s, want its problem", p)
	}
	for _, path := range []string{runsPath, runsPath + "/" + run.String() + "/balances"} {
		e.Problem(400, "request.invalid_page_token", "GET", path+"?page_token=x", "", "")
	}
	h := app.Consolidation(app.Config{})
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.ListConsolidationRuns(ctx, gen.ListConsolidationRunsParams{PageSize: gen.NewOptInt32(0)})
		return err
	}); problem.From(err).Code != "request.invalid_page_size" {
		t.Errorf("the runs' page size 0: got %v, want request.invalid_page_size", err)
	}
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.ListConsolidationBalances(ctx, gen.ListConsolidationBalancesParams{ID: run, PageSize: gen.NewOptInt32(0)})
		return err
	}); problem.From(err).Code != "request.invalid_page_size" {
		t.Errorf("the balances' page size 0: got %v, want request.invalid_page_size", err)
	}
	if p := problem.From(&pgconn.PgError{Code: "23505", ConstraintName: "consolidation_run_one_at_a_time"}); p.Code != "consolidation.run_in_progress" {
		t.Errorf("the one-at-a-time index: got %s, want consolidation.run_in_progress", p.Code)
	}
	two := fixture.NewWith(t, []fixture.Company{fixture.MapleCompany, france, italia}, withGroups, runPermissions...)
	p := two.Problem(422, "consolidation.calendar_mismatch", "POST", runsPath, `{"group_code": "ACME", "fiscal_year": 2026, "period": 11}`, "")
	if !strings.Contains(p["detail"].(string), "Company 1000") {
		t.Errorf("the group of two members: got %v, want 1000 refused first", p["detail"])
	}
}
