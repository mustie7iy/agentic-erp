package workflows_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	sdkworkflow "go.temporal.io/sdk/workflow"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/workflows"
)

// Consolidation runs (ADR-0059 §6, T68d): the workflow prepares the run,
// translates its companies, eliminates and completes it through the
// consolidation context's port.

type conScenario struct {
	e fixture.Env
	c *workflows.Consolidations
}

// conSetup is a tenant with fixture.Consolidation's group ACME (2000, in
// EUR, holding 1000, in USD, from 2026-11), both companies on one fiscal
// calendar, October's and November's closing rate USD/EUR 0.9, an entry of
// 100.00 in each company, 1000's share capital and 2000's investment. The test's own runs are carried out by the test, never by
// the dispatcher, which reads every tenant's queue: at the end their
// Operations still without a workflow are marked as taken, as deprSetup's.
func conSetup(t *testing.T) *conScenario {
	t.Helper()
	e := fixture.NewWith(t, nil, fixture.Consolidation, "consolidation.read", "consolidation.run.manage", "ledger.entry.read", "ledger.entry.post",
		"platform.operation.read", "platform.operation.cancel")
	t.Cleanup(func() {
		pgtest.Exec(t, `UPDATE platform.operation SET workflow_id = 'carried out by the test', version = version + 1
			WHERE tenant_id = $1 AND kind = $2 AND workflow_id IS NULL`, e.Tenant.ID, workflows.ConsolidateKind)
	})
	pgtest.Exec(t, `UPDATE tenancy.legal_entity u SET fiscal_calendar_id = d.fiscal_calendar_id, version = u.version + 1
		FROM tenancy.legal_entity d WHERE u.id = $1 AND d.id = $2`, e.US, e.DE)
	pgtest.Exec(t, `UPDATE ledger.period_status u SET calendar_id = d.fiscal_calendar_id, version = u.version + 1
		FROM tenancy.legal_entity d WHERE u.legal_entity_id = $1 AND d.id = $2`, e.US, e.DE)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-10-01', 0.9, 'ECB', $2),
		       ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-11-01', 0.9, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	for _, c := range []struct {
		le             uuid.UUID
		code, currency string
	}{{e.US, "1000", "USD"}, {e.DE, "2000", "EUR"}} {
		e.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-11-05", "posting_date": "2026-11-05",
			"currency": %q, "lines": [{"account_id": %q, "amount": {"amount": "100.00", "currency": %q}},
			{"account_id": %q, "amount": {"amount": "-100.00", "currency": %q}}]}`, c.le, c.currency, e.Account(t, c.code, "100000"), c.currency,
			e.Account(t, c.code, "400000"), c.currency), "")
	}
	// 1000's share capital before its first period and 2000's investment
	// equal to it (USD 100.00 at October's 0.9), which a run eliminates
	// (ADR-0059 §9).
	for _, x := range []struct {
		le                                         uuid.UUID
		code, day, currency, debit, credit, amount string
	}{{e.US, "1000", "2026-10-15", "USD", "100000", "300000", "100.00"}, {e.DE, "2000", "2026-11-05", "EUR", "150000", "100000", "90.00"}} {
		e.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": %q, "posting_date": %q, "currency": %q,
			"lines": [{"account_id": %q, "amount": {"amount": %q, "currency": %q}}, {"account_id": %q, "amount": {"amount": "-%s", "currency": %q}}]}`,
			x.le, x.day, x.day, x.currency, e.Account(t, x.code, x.debit), x.amount, x.currency, e.Account(t, x.code, x.credit), x.amount, x.currency), "")
	}
	return &conScenario{e: e, c: &workflows.Consolidations{Pool: pgtest.Pool(t), AppRole: "erp_app", WorkerRole: "erp_worker",
		Runs: app.Consolidation(app.Config{}), Actors: identity.Actors{}}}
}

// start starts November's run of ACME.
func (s *conScenario) start(t *testing.T) workflows.ConsolidationInput {
	t.Helper()
	op, _ := s.e.Must(202, "POST", "/v1/consolidation-runs", `{"group_code": "ACME", "fiscal_year": 2026, "period": 11}`, "")
	run := uuid.MustParse(strings.TrimPrefix(op["resource"].(string), "/v1/consolidation-runs/"))
	return workflows.ConsolidationInput{TenantID: s.e.Tenant.ID, RunID: run, OperationID: uuid.MustParse(op["id"].(string))}
}

// cancel cancels the run's Operation as a client does.
func (s *conScenario) cancel(t *testing.T, in workflows.ConsolidationInput) {
	t.Helper()
	path := "/v1/operations/" + in.OperationID.String()
	_, tag := s.e.Must(200, "GET", path, "", "")
	s.e.Must(200, "POST", path+":cancel", "", tag)
}

// conState is what a run's activities change: the run and its Operation
// (status and version), the run's problem, its members translated, its
// rows, and its tenant's consolidation events and audit records.
type conState struct {
	Run, Operation, Problem         string
	Translated, Rows, Events, Audit int
	Entries, Acquisitions           int
}

func conStateOf(t *testing.T, in workflows.ConsolidationInput) conState {
	t.Helper()
	var s conState
	pgtest.Scan(t, `SELECT r.status || '/' || r.version, o.status || '/' || o.version, coalesce(r.problem->>'code', ''),
			(SELECT count(*) FROM consolidation.run_member m WHERE m.run_id = r.id AND m.translated_at IS NOT NULL),
			(SELECT count(*) FROM consolidation.run_balance b WHERE b.run_id = r.id),
			(SELECT count(*) FROM consolidation.outbox x WHERE x.tenant_id = r.tenant_id),
			(SELECT count(*) FROM audit.audit_event a WHERE a.tenant_id = r.tenant_id),
			(SELECT count(*) FROM consolidation.run_entry n WHERE n.run_id = r.id),
			(SELECT count(*) FROM consolidation.run_acquisition q WHERE q.run_id = r.id)
		FROM consolidation.run r JOIN platform.operation o ON o.id = r.operation_id WHERE r.id = $1`, []any{in.RunID},
		&s.Run, &s.Operation, &s.Problem, &s.Translated, &s.Rows, &s.Events, &s.Audit, &s.Entries, &s.Acquisitions)
	return s
}

// runConsolidation runs the workflow in the SDK's test environment with
// the real activities against the package database.
func (s *conScenario) runConsolidation(t *testing.T, in workflows.ConsolidationInput, set ...func(env *testsuite.TestWorkflowEnvironment)) error {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := workflowEnv(&suite)
	env.RegisterWorkflowWithOptions(workflows.ConsolidationRun, sdkworkflow.RegisterOptions{Name: workflows.ConsolidationWorkflow})
	env.RegisterActivity(s.c)
	for _, f := range set {
		f(env)
	}
	env.ExecuteWorkflow(workflows.ConsolidationWorkflow, in)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	return env.GetWorkflowError()
}

// TestConsolidationRunWorkflow: the workflow carries a run out, computed
// with both companies translated and its Operation succeeded; a run whose
// Operation is cancelled after its first company is failed with
// consolidation.run_cancelled at the next step, its Operation staying
// cancelled.
func TestConsolidationRunWorkflow(t *testing.T) {
	s := conSetup(t)
	in := s.start(t)
	if err := s.runConsolidation(t, in); err != nil {
		t.Fatalf("the run: %v", err)
	}
	got := conStateOf(t, in)
	if run := fmt.Sprint(got.Run, " ", strings.Split(got.Operation, "/")[0], " ", got.Translated, " ", got.Rows, " ", got.Entries, " ", got.Acquisitions); run != "computed/3 succeeded 2 6 1 1" {
		t.Errorf("the run: got %s, want computed, its Operation succeeded, two companies translated in six rows, 1000's investment eliminated", run)
	}
	again := s.start(t)
	cancelAfterFirst := func(env *testsuite.TestWorkflowEnvironment) {
		env.SetOnActivityCompletedListener(func(info *activity.Info, _ converter.EncodedValue, err error) {
			if info.ActivityType.Name == "TranslateMember" && err == nil && conStateOf(t, again).Translated == 1 {
				s.cancel(t, again)
			}
		})
	}
	if err := s.runConsolidation(t, again, cancelAfterFirst); err != nil {
		t.Fatalf("the run cancelled after its first company: %v", err)
	}
	got = conStateOf(t, again)
	if run := fmt.Sprint(strings.Split(got.Run, "/")[0], " ", got.Problem, " ", strings.Split(got.Operation, "/")[0], " ", got.Translated); run !=
		"failed consolidation.run_cancelled cancelled 1" {
		t.Errorf("the cancelled run: got %s, want it failed with consolidation.run_cancelled after one company, its Operation cancelled", run)
	}
}

// stuckTranslations is the consolidation context with every translation
// failing on the machinery, and its failure of a run failing so too, on
// its first stuck tries (failTries counts them all).
type stuckTranslations struct {
	workflows.Consolidation
	tries, failTries *atomic.Int32
	stuck            int32
	refused          bool // the failure refused with a business problem on every try
}

func (s stuckTranslations) TranslateMember(context.Context, uuid.UUID, uuid.UUID) error {
	s.tries.Add(1)
	return errors.New("ledger unreachable")
}

func (s stuckTranslations) FailConsolidationRun(ctx context.Context, run uuid.UUID, why *problem.Problem) error {
	if s.failTries.Add(1) <= s.stuck {
		return errors.New("audit unreachable")
	}
	if s.refused {
		return problem.Conflict("test.fail_refused", "Refused.")
	}
	return s.Consolidation.FailConsolidationRun(ctx, run, why)
}

// TestConsolidationRunInterrupted: a translation failing on the machinery
// is tried five times; then the run fails as an internal error, freeing
// its group, its Operation fails with it, and so does the workflow, with
// the translation's error. A failure of the run failing on the machinery
// is tried past the five, until it is recorded (review:T68d): the run
// still ends failed and its group is free. A failure refused with a
// business problem is not retried: the workflow fails with the problem
// and the run stays running (ADR-0012 As built).
func TestConsolidationRunInterrupted(t *testing.T) {
	cases := []struct {
		name         string
		stuck, tries int32
		refused      bool
		err, run     string
	}{
		{"the failure recorded at once", 0, 1, false, "ledger unreachable", "failed platform.internal failed"},
		{"the failure failing seven times first", 7, 8, false, "ledger unreachable", "failed platform.internal failed"},
		{"the failure refused", 0, 1, true, "Refused.", "running  running"}, // last: the run keeps its group
	}
	s := conSetup(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tries, failTries := &atomic.Int32{}, &atomic.Int32{}
			s.c.Runs = stuckTranslations{Consolidation: app.Consolidation(app.Config{}), tries: tries, failTries: failTries, stuck: c.stuck, refused: c.refused}
			in := s.start(t)
			if err := s.runConsolidation(t, in); err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("the workflow: got %v, want an error with %q", err, c.err)
			}
			if n := tries.Load(); n != 5 {
				t.Errorf("the translation's tries: got %d, want 5", n)
			}
			if n := failTries.Load(); n != c.tries {
				t.Errorf("the failure's tries: got %d, want %d", n, c.tries)
			}
			got := conStateOf(t, in)
			if run := fmt.Sprint(strings.Split(got.Run, "/")[0], " ", got.Problem, " ", strings.Split(got.Operation, "/")[0]); run != c.run {
				t.Errorf("the run, its problem and its Operation: got %s, want %s", run, c.run)
			}
		})
	}
}

// TestConsolidationActivitiesRunTwice: each activity whose transaction
// committed and whose completion was lost runs again as a no-op success
// (ADR-0012): the same run, Operation, members, rows, events and audit
// records.
func TestConsolidationActivitiesRunTwice(t *testing.T) {
	s := conSetup(t)
	ctx := context.Background()
	in := s.start(t)
	twiceCon(t, "PrepareConsolidationRun", in, func() error { _, err := s.c.PrepareConsolidationRun(ctx, in); return err })
	companies, err := s.c.PrepareConsolidationRun(ctx, in)
	if err != nil || len(companies) != 2 {
		t.Fatalf("the run's companies: got %v %v, want two", companies, err)
	}
	for _, c := range companies {
		twiceCon(t, "TranslateMember", in, func() error { return s.c.TranslateMember(ctx, in, c) })
	}
	twiceCon(t, "EliminateConsolidationRun", in, func() error { return s.c.EliminateConsolidationRun(ctx, in) })
	twiceCon(t, "CompleteConsolidationRun", in, func() error { return s.c.CompleteConsolidationRun(ctx, in) })
	if got := conStateOf(t, in); got.Run != "computed/3" || got.Entries != 1 || got.Acquisitions != 1 {
		t.Errorf("the run: got %s with %d entries and %d acquisitions, want computed with 1000's investment entry and acquisition", got.Run,
			got.Entries, got.Acquisitions)
	}
	if companies, err := s.c.PrepareConsolidationRun(ctx, in); err != nil || len(companies) != 0 {
		t.Errorf("a computed run's preparation: got %v %v, want no company", companies, err)
	}
	failed := s.start(t)
	why := problem.Conflict("consolidation.rate_stale", "Stale.")
	twiceCon(t, "FailConsolidationRun", failed, func() error { return s.c.FailConsolidationRun(ctx, failed, *why) })
	if got := conStateOf(t, failed); got.Run != "failed/2" {
		t.Errorf("the failed run: got %s, want failed", got.Run)
	}
}

// twiceCon runs an activity whose first run committed and whose
// completion was lost, so it runs again: both runs succeed and the second
// changes nothing.
func twiceCon(t *testing.T, name string, in workflows.ConsolidationInput, activity func() error) {
	t.Helper()
	if err := activity(); err != nil {
		t.Fatalf("%s, first run: got %v, want nil", name, err)
	}
	first := conStateOf(t, in)
	if err := activity(); err != nil {
		t.Fatalf("%s, run again: got %v, want nil", name, err)
	}
	if again := conStateOf(t, in); again != first {
		t.Fatalf("%s, run again: got %+v, want %+v as after the first run", name, again, first)
	}
}

// TestDispatchConsolidation: a queued run's Operation becomes a
// ConsolidationRun named by the run, once; the id is recorded on the
// Operation.
func TestDispatchConsolidation(t *testing.T) {
	s := conSetup(t)
	in := s.start(t)
	engine := &fakeEngine{}
	d := &workflows.Dispatcher{Pool: pgtest.Pool(t), WorkerRole: "erp_worker", Client: engine, Queue: "erp-test"}
	ctx := context.Background()
	id := workflows.ConsolidationWorkflowID(in.TenantID, in.RunID)
	if want := fmt.Sprintf("%s-consolidation_run-%s", in.TenantID, in.RunID); id != want {
		t.Errorf("workflow id: got %s, want %s", id, want)
	}
	for range 2 {
		if _, err := d.RunOnce(ctx); err != nil {
			t.Fatalf("round: %v", err)
		}
	}
	var ours []string
	for _, st := range engine.started {
		if st.ID == id {
			ours = append(ours, fmt.Sprint(st.Name, " ", st.Queue, " ", st.Args[0] == in))
		}
	}
	if len(ours) != 1 || ours[0] != "ConsolidationRun erp-test true" {
		t.Errorf("started: got %v, want the run's workflow once with its input", ours)
	}
	var recorded string
	pgtest.Scan(t, `SELECT workflow_id FROM platform.operation WHERE id = $1`, []any{in.OperationID}, &recorded)
	if recorded != id {
		t.Errorf("recorded workflow: got %s, want %s", recorded, id)
	}
}

// TestConsolidator: without a workflow engine the in-process runner
// carries out every unfinished run, one whose Operation was cancelled
// included, which it fails with consolidation.run_cancelled, nothing
// translated and the Operation left cancelled; a step failing on the
// machinery is reported with the run; its loop logs the runs it ended.
func TestConsolidator(t *testing.T) {
	s := conSetup(t)
	in := s.start(t)
	s.cancel(t, in)
	logs := &pgtest.SyncBuffer{}
	w := &workflows.Consolidator{Consolidations: s.c, Log: slog.New(slog.NewTextHandler(logs, nil))}
	ctx := context.Background()
	if n, err := w.RunOnce(ctx); err != nil || n < 1 {
		t.Fatalf("round: got %d %v, want the run ended", n, err)
	}
	got := conStateOf(t, in)
	if run := fmt.Sprint(got.Run, " ", got.Problem, " ", got.Operation, " ", got.Translated); run != "failed/2 consolidation.run_cancelled cancelled/2 0" {
		t.Errorf("the run: got %s, want it failed with consolidation.run_cancelled, nothing translated, its Operation left cancelled", run)
	}
	again := s.start(t)
	s.c.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
		if scope.TenantID == s.e.Tenant.ID && !scope.ReadOnly {
			return nil, errors.New("database down")
		}
		return pgtest.Pool(t).Begin(ctx, scope)
	}
	if _, err := w.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), again.RunID.String()) {
		t.Errorf("a step failing: got %v, want the run named", err)
	}
	s.c.Begin = nil
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { w.Run(runCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	pgtest.Eventually(t, 10*time.Second, func() bool { return strings.Contains(logs.String(), "consolidation runs ended") },
		"the runner did not log the runs it ended: %s", logs)
	if got := conStateOf(t, again); !strings.HasPrefix(got.Run, "computed/") {
		t.Errorf("the run left by the failure: got %s, want it computed by the loop", got.Run)
	}
}

// TestConsolidationRunRequesterBlocked: a requester blocked after the
// run's preparation fails the run at its next step with
// identity.principal_blocked, on the run and its Operation, without a
// retry; the group is free for a new run.
func TestConsolidationRunRequesterBlocked(t *testing.T) {
	s := conSetup(t)
	in := s.start(t)
	var refused atomic.Int32
	blockAfterPrepare := func(env *testsuite.TestWorkflowEnvironment) {
		env.SetOnActivityCompletedListener(func(info *activity.Info, _ converter.EncodedValue, err error) {
			switch {
			case info.ActivityType.Name == "PrepareConsolidationRun" && err == nil:
				pgtest.Exec(t, `UPDATE identity.principal SET status = 'blocked', version = version + 1 WHERE id = $1`, s.e.Tenant.Human.ID)
			case err != nil:
				refused.Add(1)
			}
		})
	}
	if err := s.runConsolidation(t, in, blockAfterPrepare); err != nil {
		t.Fatalf("the workflow: got %v, want it ended without an error", err)
	}
	if n := refused.Load(); n != 1 {
		t.Errorf("the refused step's tries: got %d, want 1 (not retried)", n)
	}
	got := conStateOf(t, in)
	if run := fmt.Sprint(strings.Split(got.Run, "/")[0], " ", got.Problem, " ", strings.Split(got.Operation, "/")[0], " ", got.Translated); run !=
		"failed identity.principal_blocked failed 0" {
		t.Errorf("the run: got %s, want it and its Operation failed with identity.principal_blocked, nothing translated", run)
	}
	pgtest.Exec(t, `UPDATE identity.principal SET status = 'active', version = version + 1 WHERE id = $1`, s.e.Tenant.Human.ID)
	s.start(t) // the group is free again
}

// TestConsolidationRunReplay: a history of the first version of
// ConsolidationRun (testdata, recorded on a development server with
// stand-in activities: a preparation of two companies, both translated,
// the elimination and the completion) replays on the current code without
// a nondeterminism error. A change to the workflow that issues other
// commands adds a version (workflow.GetVersion, ADR-0012) and keeps this
// history replaying.
func TestConsolidationRunReplay(t *testing.T) {
	r := worker.NewWorkflowReplayer()
	r.RegisterWorkflowWithOptions(workflows.ConsolidationRun, sdkworkflow.RegisterOptions{Name: workflows.ConsolidationWorkflow})
	if err := r.ReplayWorkflowHistoryFromJSONFile(nil, filepath.Join("testdata", "consolidation_run_v1.json")); err != nil {
		t.Fatalf("replay of the version 1 history: %v", err)
	}
}
