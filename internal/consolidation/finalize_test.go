package consolidation_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/consolidation"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// finalEnv is icEnv's tenant and figures, its person and agent also
// holding the permissions to accept differences and finalize runs.
func finalEnv(t *testing.T) fixture.Env {
	t.Helper()
	e, _ := icEnv(t)
	for _, p := range []string{"consolidation.difference.accept", "consolidation.run.finalize"} {
		e.Tenant.Grant(t, p, e.Tenant.Human, e.Tenant.Agent)
	}
	return e
}

// setPeriod moves November 2026's leading-ledger period of the companies
// to each status in turn, its version one up at each, as the ledger's
// period operations do.
func setPeriod(t *testing.T, e fixture.Env, companies []uuid.UUID, statuses ...string) {
	t.Helper()
	for _, c := range companies {
		for _, s := range statuses {
			pgtest.Exec(t, `UPDATE ledger.period_status SET status = $3, version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
				AND fiscal_year = 2026 AND period = 11 AND ledger_id = (SELECT id FROM ledger.ledger WHERE tenant_id = $1 AND is_leading)`, e.Tenant.ID, c, s)
		}
	}
}

// computed starts November's run of ACME and carries it out.
func computed(t *testing.T, e fixture.Env) uuid.UUID {
	t.Helper()
	run, _ := start(t, e)
	carryOut(t, e, app.Consolidation(app.Config{}), run)
	return run
}

// openDifference is a run's open difference (icFigures' IC_RESULTS pair)
// and its ETag.
func openDifference(t *testing.T, e fixture.Env, run uuid.UUID) (string, string) {
	t.Helper()
	page, _ := e.Must(200, "GET", runsPath+"/"+run.String()+"/differences?status=open", "", "")
	items := page["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("open differences of run %s: got %d, want 1", run, len(items))
	}
	d := items[0].(map[string]any)
	return runsPath + "/" + run.String() + "/differences/" + d["id"].(string), fmt.Sprintf(`"%v"`, d["version"])
}

// accept accepts a run's open difference.
func accept(t *testing.T, e fixture.Env, run uuid.UUID) {
	t.Helper()
	path, tag := openDifference(t, e, run)
	e.Must(200, "POST", path+":accept", `{"reason": "Rates"}`, tag)
}

// finalize finalizes a run at the ETag it has now.
func finalize(t *testing.T, e fixture.Env, run uuid.UUID) map[string]any {
	t.Helper()
	_, tag := e.Must(200, "GET", runsPath+"/"+run.String(), "", "")
	got, _ := e.Must(200, "POST", runsPath+"/"+run.String()+":finalize", "", tag)
	return got
}

// TestAcceptIntercompanyDifference: a person accepts an open difference
// with a reason at its ETag: it is accepted with who, when and why, its
// version and the run's one up, audited and published; the run's counts
// and the list show it. A stale ETag, a blank reason, a difference not
// open, one of another run or none, a run not computed and an agent are
// refused.
func TestAcceptIntercompanyDifference(t *testing.T) {
	e := finalEnv(t)
	run := computed(t, e)
	path, tag := openDifference(t, e, run)
	before, _ := e.Must(200, "GET", runsPath+"/"+run.String(), "", "")
	e.Problem(412, "request.version_mismatch", "POST", path+":accept", `{"reason": "Rates"}`, `"2"`)
	e.Blank("reason", "POST", path+":accept", `{"reason": "  "}`, tag)
	agent := apitest.Client{T: t, API: e.API, Tenant: e.Tenant, As: e.Tenant.Agent}
	agent.Problem(403, "identity.agent_not_allowed", "POST", path+":accept", `{"reason": "Rates"}`, tag)
	got, etag := e.Must(200, "POST", path+":accept", `{"reason": "The revenue is booked at the invoice date's rate"}`, tag)
	var name string
	pgtest.Scan(t, `SELECT display_name FROM identity.principal WHERE id = $1`, []any{e.Tenant.Human.ID}, &name)
	if s := fmt.Sprint(got["status"], " ", got["accepted_by"], " ", got["accepted_by_name"], " ", got["reason"], " ", got["version"], " ", etag,
		" ", got["accepted_at"] != nil); s != fmt.Sprintf(`accepted %s %s The revenue is booked at the invoice date's rate 2 "2" true`, e.Tenant.Human.ID, name) {
		t.Errorf("the accepted difference: got %s, want it accepted by %s with the reason at version 2", s, name)
	}
	if got["difference"].(map[string]any)["amount"] != "-20.00" || got["elimination"].(map[string]any)["code"] != "IC_RESULTS" {
		t.Errorf("the accepted difference's figures: got %v %v, want IC_RESULTS -20.00", got["elimination"], got["difference"])
	}
	after, _ := e.Must(200, "GET", runsPath+"/"+run.String(), "", "")
	if a, b := after["version"].(float64), before["version"].(float64); a != b+1 {
		t.Errorf("the run's version: got %v, want %v", a, b+1)
	}
	if c := fmt.Sprint(after["difference_counts"]); !strings.Contains(c, "map[count:1 status:accepted]") || strings.Contains(c, "status:open") {
		t.Errorf("the run's difference counts: got %s, want one accepted and none open", c)
	}
	page, _ := e.Must(200, "GET", runsPath+"/"+run.String()+"/differences?status=accepted", "", "")
	if items := page["items"].([]any); len(items) != 1 || items[0].(map[string]any)["accepted_by_name"] != name {
		t.Errorf("the accepted differences listed: got %v, want the one, named", items)
	}
	e.Problem(409, "consolidation.difference_not_open", "POST", path+":accept", `{"reason": "Again"}`, `"2"`)
	matched, _ := e.Must(200, "GET", runsPath+"/"+run.String()+"/differences?status=matched", "", "")
	e.Problem(409, "consolidation.difference_not_open", "POST", runsPath+"/"+run.String()+"/differences/"+
		matched["items"].([]any)[0].(map[string]any)["id"].(string)+":accept", `{"reason": "r"}`, `"1"`)
	e.Problem(404, "request.not_found", "POST", runsPath+"/"+run.String()+"/differences/"+uuid.NewString()+":accept", `{"reason": "r"}`, `"1"`)
	var event, audited string
	pgtest.Scan(t, `SELECT payload->>'reason' || ' ' || (payload->'difference'->>'amount') || ' ' || (payload->>'run_version') FROM consolidation.outbox
		WHERE tenant_id = $1 AND event_type LIKE '%intercompany_difference_accepted%'`, []any{e.Tenant.ID}, &event)
	pgtest.Scan(t, `SELECT action FROM audit.audit_event WHERE tenant_id = $1 AND object_type = 'consolidation.run_difference'`, []any{e.Tenant.ID}, &audited)
	if w := fmt.Sprintf("The revenue is booked at the invoice date's rate -20.00 %v", after["version"]); event != w ||
		audited != "consolidation.difference.accepted" {
		t.Errorf("the event and audit record: got %q and %q, want %q and consolidation.difference.accepted", event, audited, w)
	}
	other := computed(t, e)
	otherPath, otherTag := openDifference(t, e, other)
	_, otherDifference, _ := strings.Cut(otherPath, "/differences/")
	e.Problem(404, "request.not_found", "POST", runsPath+"/"+run.String()+"/differences/"+otherDifference+":accept", `{"reason": "r"}`, otherTag)
	e.Must(200, "POST", runsPath+"/"+other.String()+":discard", "", `"3"`)
	e.Problem(409, "consolidation.run_not_computed", "POST", otherPath+":accept", `{"reason": "r"}`, otherTag)
}

// TestFinalizeConsolidationRun: a computed run is finalized only without
// open differences, from periods locked when it read its figures and now,
// at the versions it recorded; final, it names its finalizer and its
// statements say so. An unlock, a posting and a relock make it stale, and
// refuse a run translated before them; a new run finalized supersedes it.
func TestFinalizeConsolidationRun(t *testing.T) {
	e := finalEnv(t)
	both := []uuid.UUID{e.US, e.DE}
	draft := computed(t, e)
	path := runsPath + "/" + draft.String()
	_, tag := e.Must(200, "GET", path, "", "")
	e.Problem(409, "consolidation.differences_open", "POST", path+":finalize", "", tag)
	agent := apitest.Client{T: t, API: e.API, Tenant: e.Tenant, As: e.Tenant.Agent}
	agent.Problem(403, "identity.agent_not_allowed", "POST", path+":finalize", "", tag)
	accept(t, e, draft)
	e.Problem(412, "request.version_mismatch", "POST", path+":finalize", "", tag)
	_, tag = e.Must(200, "GET", path, "", "")
	p := e.Problem(409, "consolidation.period_not_locked", "POST", path+":finalize", "", tag)
	if d := p["detail"].(string); !strings.Contains(d, "company 2000 (open when the run read its figures, open now), company 1000 (open") {
		t.Errorf("the periods not locked: got %s, want both companies named", d)
	}
	setPeriod(t, e, both, "closed", "locked")
	p = e.Problem(409, "consolidation.period_not_locked", "POST", path+":finalize", "", tag)
	if d := p["detail"].(string); !strings.Contains(d, "company 1000 (open when the run read its figures, locked now)") {
		t.Errorf("the periods locked after the run: got %s, want them named as read", d)
	}
	first, before := computed(t, e), computed(t, e)
	accept(t, e, first)
	accept(t, e, before)
	got := finalize(t, e, first)
	var name string
	pgtest.Scan(t, `SELECT display_name FROM identity.principal WHERE id = $1`, []any{e.Tenant.Human.ID}, &name)
	if s := fmt.Sprint(got["status"], " ", got["stale"], " ", got["finalized_by"], " ", got["finalized_by_name"], " ", got["supersedes"], " ",
		got["finalized_at"] != nil); s != fmt.Sprintf("final false %s %s <nil> true", e.Tenant.Human.ID, name) {
		t.Errorf("the final run: got %s, want final, fresh, finalized by %s", s, name)
	}
	firstPath := runsPath + "/" + first.String()
	statement, _ := e.Must(200, "GET", firstPath+"/statements/balance_sheet", "", "")
	if s := fmt.Sprint(statement["run_status"], " ", statement["stale"]); s != "final false" {
		t.Errorf("the final run's statement: got %s, want final false", s)
	}
	e.Problem(409, "consolidation.run_not_computed", "POST", firstPath+":finalize", "", fmt.Sprintf(`"%v"`, got["version"]))
	e.Problem(409, "consolidation.run_not_discardable", "POST", firstPath+":discard", "", fmt.Sprintf(`"%v"`, got["version"]))
	// An unlock requested (the period's version moves, ledger.unlock.go),
	// approved, a posting and a relock.
	pgtest.Exec(t, `UPDATE ledger.period_status SET unlock_request_id = gen_random_uuid(), version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
		AND fiscal_year = 2026 AND period = 11`, e.Tenant.ID, e.US)
	_, tag = e.Must(200, "GET", runsPath+"/"+before.String(), "", "")
	if p = e.Problem(409, "consolidation.run_stale", "POST", runsPath+"/"+before.String()+":finalize", "", tag); !strings.Contains(p["detail"].(string),
		"company 1000 (version") || !strings.Contains(p["detail"].(string), "pending") || strings.Contains(p["detail"].(string), "2000") {
		t.Errorf("the run with a pending unlock request: got %s, want company 1000 alone named with the request", p["detail"])
	}
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', unlock_request_id = NULL, version = version + 1 WHERE tenant_id = $1
		AND legal_entity_id = $2 AND fiscal_year = 2026 AND period = 11`, e.Tenant.ID, e.US)
	setPeriod(t, e, []uuid.UUID{e.US}, "open")
	postIC(t, e, e.US, "1000", "USD", "2026-11-20", icLine{"100000", "10.00", nil}, icLine{"400000", "-10.00", nil})
	setPeriod(t, e, []uuid.UUID{e.US}, "closed", "locked")
	stale, _ := e.Must(200, "GET", firstPath, "", "")
	statement, _ = e.Must(200, "GET", firstPath+"/statements/profit_and_loss", "", "")
	if s := fmt.Sprint(stale["status"], " ", stale["stale"], " ", statement["stale"]); s != "final true true" {
		t.Errorf("the final run after the relock: got %s, want final and stale", s)
	}
	if p = e.Problem(409, "consolidation.run_stale", "POST", runsPath+"/"+before.String()+":finalize", "", tag); strings.Contains(p["detail"].(string),
		"pending") {
		t.Errorf("the run translated before the unlock: got %s, want it stale without a pending request", p["detail"])
	}
	again := computed(t, e)
	accept(t, e, again)
	got = finalize(t, e, again)
	if s := fmt.Sprint(got["status"], " ", got["stale"], " ", got["supersedes"]); s != "final false "+first.String() {
		t.Errorf("the new final run: got %s, want final, fresh, superseding %s", s, first)
	}
	old, _ := e.Must(200, "GET", firstPath, "", "")
	if s := fmt.Sprint(old["status"], " ", old["superseded_by"], " ", old["finalized_by_name"]); s != "superseded "+again.String()+" "+name {
		t.Errorf("the superseded run: got %s, want superseded by %s, its finalizer kept", s, again)
	}
	var events string
	pgtest.Scan(t, `SELECT string_agg(payload->>'status_before' || '>' || (payload->>'status'), ' ' ORDER BY occurred_at, id) FROM consolidation.outbox
		WHERE tenant_id = $1 AND aggregate_id = $2`, []any{e.Tenant.ID, first}, &events)
	if !strings.HasSuffix(events, "computed>final final>superseded") {
		t.Errorf("the first final run's events: got %s, want it finalized and superseded", events)
	}
	page, _ := e.Must(200, "GET", runsPath+"?status=superseded", "", "")
	if items := page["items"].([]any); len(items) != 1 || items[0].(map[string]any)["id"] != first.String() {
		t.Errorf("the superseded runs: got %v, want %s", items, first)
	}
}

// TestFinalizationsSerialize: two finalizations of one period wait for
// each other on the period's runs; the second sees the first's final run
// and supersedes it, so one run of the period is final.
func TestFinalizationsSerialize(t *testing.T) {
	e := finalEnv(t)
	setPeriod(t, e, []uuid.UUID{e.US, e.DE}, "closed", "locked")
	a, b := computed(t, e), computed(t, e)
	accept(t, e, a)
	accept(t, e, b)
	h := app.Consolidation(app.Config{})
	fin := func(run uuid.UUID) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := h.FinalizeConsolidationRun(ctx, gen.FinalizeConsolidationRunParams{ID: run, IfMatch: "*"})
			return err
		}
	}
	first, second := apitest.Serialized(t, e.Tenant, e.Tenant.Human, fin(a), fin(b))
	if first != nil || second != nil {
		t.Fatalf("the finalizations: got %v and %v, want both", first, second)
	}
	var statuses string
	pgtest.Scan(t, `SELECT string_agg(status || coalesce(':' || (superseded_by = $3)::text, ''), ' ' ORDER BY id = $2 DESC) FROM consolidation.run
		WHERE tenant_id = $1 AND id IN ($2, $3)`, []any{e.Tenant.ID, a, b}, &statuses)
	if statuses != "superseded:true final" {
		t.Errorf("the runs: got %s, want the first superseded by the second, final", statuses)
	}
	if p := problem.From(&pgconn.PgError{Code: "23505", ConstraintName: "consolidation_run_one_final"}); p.Code != "consolidation.final_exists" {
		t.Errorf("the one-final index: got %s, want consolidation.final_exists", p.Code)
	}
}

// TestFinalizationDatabaseFailures: every database statement of an
// acceptance, a finalization and a list of accepted differences failing
// fails it, never hidden, and a period the ledger no longer has is named
// as none; outside a unit of work each refuses.
func TestFinalizationDatabaseFailures(t *testing.T) {
	e := finalEnv(t)
	setPeriod(t, e, []uuid.UUID{e.US, e.DE}, "closed", "locked")
	prior, run := computed(t, e), computed(t, e)
	accept(t, e, prior)
	finalize(t, e, prior)
	path, _ := openDifference(t, e, run)
	difference := uuid.MustParse(path[strings.LastIndex(path, "/")+1:])
	fresh := func() *consolidation.Endpoints { return app.Consolidation(app.Config{}) }
	acceptIt := func(ctx context.Context) error {
		_, err := fresh().AcceptIntercompanyDifference(ctx, &gen.ReasonInput{Reason: "r"},
			gen.AcceptIntercompanyDifferenceParams{ID: run, DifferenceID: difference, IfMatch: `"1"`})
		return err
	}
	finalizeIt := func(ctx context.Context) error {
		_, err := fresh().FinalizeConsolidationRun(ctx, gen.FinalizeConsolidationRunParams{ID: run, IfMatch: "*"})
		return err
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "accept", acceptIt)
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, acceptIt); err != nil {
		t.Fatal(err)
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "finalize", finalizeIt)
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "accepted differences", func(ctx context.Context) error {
		_, err := fresh().ListIntercompanyDifferences(ctx, gen.ListIntercompanyDifferencesParams{ID: run, Status: gen.NewOptString("accepted")})
		return err
	})
	gone := fresh()
	gone.Ledger = noPeriod{gone.Ledger}
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := gone.FinalizeConsolidationRun(ctx, gen.FinalizeConsolidationRunParams{ID: run, IfMatch: "*"})
		return err
	}); !strings.Contains(fmt.Sprint(err), "locked when the run read its figures, none now") {
		t.Errorf("a period the ledger no longer has: got %v, want it named as none", err)
	}
	for name, fn := range map[string]func(ctx context.Context) error{"accept": acceptIt, "finalize": finalizeIt} {
		if err := fn(context.Background()); err == nil {
			t.Errorf("%s outside a unit of work: got no error, want one", name)
		}
	}
}
