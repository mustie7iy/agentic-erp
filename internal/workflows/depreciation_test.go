package workflows_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

// Depreciation runs (ADR-0057 §7, T57f): the workflow plans the run,
// posts its batches and finishes it through the assets context's port.

type deprScenario struct {
	e fixture.Env
	d *workflows.Depreciations
}

// deprSetup is a tenant with the prototype's asset classes and a machine
// of company 2000 capitalised at 1,200.00 EUR valued 10 June 2026, so its
// depreciation is 10.00 a month from June.
func deprSetup(t *testing.T) *deprScenario {
	t.Helper()
	e := fixture.NewWith(t, nil, fixture.AssetClasses, "assets.asset.read", "assets.asset.change", "assets.asset.post", "assets.depreciation.run",
		"ledger.entry.post")
	s := &deprScenario{e: e, d: &workflows.Depreciations{Pool: pgtest.Pool(t), AppRole: "erp_app", WorkerRole: "erp_worker", Runs: app.Assets(app.Config{}),
		Actors: identity.Actors{}}}
	s.machine(t, "Mill")
	return s
}

// machine is a machine of company 2000 capitalised at 1,200.00 EUR
// valued 10 June 2026.
func (s *deprScenario) machine(t *testing.T, description string) {
	t.Helper()
	e := s.e
	a, _ := e.Must(201, "POST", "/v1/assets", fmt.Sprintf(`{"legal_entity_id": %q, "class_code": "MACHINES", "description": %q}`, e.DE, description), "")
	je, _ := e.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-06-10", "posting_date": "2026-06-10",
		"currency": "EUR", "lines": [{"account_id": %q, "amount": {"amount": "1200.00", "currency": "EUR"}},
		{"account_id": %q, "amount": {"amount": "-1200.00", "currency": "EUR"}}]}`, e.DE, e.Account(t, "2000", "169000"), e.Account(t, "2000", "100000")), "")
	e.Must(201, "POST", "/v1/asset-acquisitions", fmt.Sprintf(`{"asset_id": %q, "source": {"entry_id": %q, "line_no": 1}, "posting_date": "2026-06-15"}`,
		a["id"], je["id"]), "")
}

// start starts the run of the period in the leading ledger.
func (s *deprScenario) start(t *testing.T, period int) workflows.DepreciationInput {
	t.Helper()
	op, _ := s.e.Must(202, "POST", "/v1/depreciation-runs", fmt.Sprintf(`{"legal_entity_id": %q, "fiscal_year": 2026, "period": %d}`, s.e.DE, period), "")
	run := uuid.MustParse(strings.TrimPrefix(op["resource"].(string), "/v1/depreciation-runs/"))
	return workflows.DepreciationInput{TenantID: s.e.Tenant.ID, RunID: run, OperationID: uuid.MustParse(op["id"].(string))}
}

// close closes a period of 2026 in the leading ledger.
func (s *deprScenario) close(t *testing.T, period int) {
	t.Helper()
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE legal_entity_id = $1 AND fiscal_year = 2026 AND period = $2`,
		s.e.DE, period)
}

// deprState is what a run's activities change: the run and its
// Operation (status and version), its batches' statuses, and the
// tenant's journal entries, assets events and audit records.
type deprState struct {
	Run, Operation, Batches  string
	Entries, Events, Audited int
}

func deprStateOf(t *testing.T, in workflows.DepreciationInput) deprState {
	t.Helper()
	var s deprState
	pgtest.Scan(t, `SELECT r.status || '/' || r.version, o.status || '/' || o.version,
			coalesce((SELECT string_agg(b.status, ' ' ORDER BY b.batch_no) FROM assets.depreciation_batch b WHERE b.run_id = r.id), ''),
			(SELECT count(*) FROM ledger.journal_entry j WHERE j.tenant_id = r.tenant_id),
			(SELECT count(*) FROM assets.outbox x WHERE x.tenant_id = r.tenant_id),
			(SELECT count(*) FROM audit.audit_event a WHERE a.tenant_id = r.tenant_id)
		FROM assets.depreciation_run r JOIN platform.operation o ON o.id = r.operation_id WHERE r.id = $1`, []any{in.RunID},
		&s.Run, &s.Operation, &s.Batches, &s.Entries, &s.Events, &s.Audited)
	return s
}

// runDepreciation runs the workflow in the SDK's test environment with
// the real activities against the package database.
func (s *deprScenario) runDepreciation(t *testing.T, in workflows.DepreciationInput) error {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := workflowEnv(&suite)
	env.RegisterWorkflowWithOptions(workflows.DepreciationRun, sdkworkflow.RegisterOptions{Name: workflows.DepreciationWorkflow})
	env.RegisterActivity(s.d)
	env.RegisterDelayedCallback(env.CancelWorkflow, 0) // a cancelled Operation does not stop the run
	env.ExecuteWorkflow(workflows.DepreciationWorkflow, in)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	return env.GetWorkflowError()
}

// TestDepreciationRunWorkflow: the workflow carries a run out, its
// cancellation notwithstanding: June's run posts and succeeds its
// Operation; July's, its period closed after the start, fails with the
// ledger's problem on the run and the Operation.
func TestDepreciationRunWorkflow(t *testing.T) {
	s := deprSetup(t)
	june := s.start(t, 6)
	if err := s.runDepreciation(t, june); err != nil {
		t.Fatalf("June's run: %v", err)
	}
	if got := deprStateOf(t, june); got.Run != "posted/3" || !strings.HasPrefix(got.Operation, "succeeded/") || got.Batches != "posted" {
		t.Errorf("June's run: got %+v, want posted, its Operation succeeded", got)
	}
	july := s.start(t, 7)
	s.close(t, 7)
	if err := s.runDepreciation(t, july); err != nil {
		t.Fatalf("July's run: %v", err)
	}
	var got string
	pgtest.Scan(t, `SELECT r.status || ' ' || (r.error->>'code') || ' ' || o.status FROM assets.depreciation_run r JOIN platform.operation o ON o.id = r.operation_id
		WHERE r.id = $1`, []any{july.RunID}, &got)
	if got != "failed ledger.period_not_open failed" {
		t.Errorf("July's run: got %s, want it and its Operation failed with ledger.period_not_open", got)
	}
}

// TestDepreciationActivitiesRunTwice: each activity whose transaction
// committed and whose completion was lost runs again as a no-op success
// (ADR-0012): the same run, Operation, batches, entries, events and audit
// records.
func TestDepreciationActivitiesRunTwice(t *testing.T) {
	s := deprSetup(t)
	ctx := context.Background()
	in := s.start(t, 6)
	twiceDepr(t, "PlanDepreciationRun", in, func() error { _, err := s.d.PlanDepreciationRun(ctx, in); return err })
	batches, err := s.d.PlanDepreciationRun(ctx, in)
	if err != nil || len(batches) != 1 {
		t.Fatalf("the run's batches: got %v %v, want one", batches, err)
	}
	twiceDepr(t, "PostDepreciationBatch", in, func() error { return s.d.PostDepreciationBatch(ctx, in, batches[0]) })
	twiceDepr(t, "FinishDepreciationRun", in, func() error { return s.d.FinishDepreciationRun(ctx, in) })
	if got := deprStateOf(t, in); got.Run != "posted/3" {
		t.Errorf("the run: got %s, want posted", got.Run)
	}
	july := s.start(t, 7)
	why := problem.Conflict("ledger.period_not_open", "Period 2026/7 is closed.")
	twiceDepr(t, "FailDepreciationRun", july, func() error { return s.d.FailDepreciationRun(ctx, july, *why) })
	if got := deprStateOf(t, july); got.Run != "failed/2" {
		t.Errorf("the failed run: got %s, want failed", got.Run)
	}
}

// twiceDepr runs an activity whose first run committed and whose
// completion was lost, so it runs again: both runs succeed and the
// second changes nothing.
func twiceDepr(t *testing.T, name string, in workflows.DepreciationInput, activity func() error) {
	t.Helper()
	if err := activity(); err != nil {
		t.Fatalf("%s, first run: got %v, want nil", name, err)
	}
	first := deprStateOf(t, in)
	if err := activity(); err != nil {
		t.Fatalf("%s, run again: got %v, want nil", name, err)
	}
	if again := deprStateOf(t, in); again != first {
		t.Fatalf("%s, run again: got %+v, want %+v as after the first run", name, again, first)
	}
}

// TestDispatchDepreciation: a queued run's Operation, or one cancelled
// before any workflow took it, becomes a DepreciationRun named by the
// run, once; the id is recorded on the Operation.
func TestDispatchDepreciation(t *testing.T) {
	s := deprSetup(t)
	june := s.start(t, 6)
	engine := &fakeEngine{}
	d := &workflows.Dispatcher{Pool: pgtest.Pool(t), WorkerRole: "erp_worker", Client: engine, Queue: "erp-test"}
	ctx := context.Background()
	id := workflows.DepreciationWorkflowID(june.TenantID, june.RunID)
	if want := fmt.Sprintf("%s-depreciation_run-%s", june.TenantID, june.RunID); id != want {
		t.Errorf("workflow id: got %s, want %s", id, want)
	}
	pgtest.Exec(t, `UPDATE platform.operation SET status = 'cancelled', version = version + 1 WHERE id = $1`, june.OperationID)
	for range 2 {
		if _, err := d.RunOnce(ctx); err != nil {
			t.Fatalf("round: %v", err)
		}
	}
	var ours []string
	for _, st := range engine.started {
		if st.ID == id {
			ours = append(ours, fmt.Sprint(st.Name, " ", st.Queue, " ", st.Args[0] == june))
		}
	}
	if len(ours) != 1 || ours[0] != "DepreciationRun erp-test true" {
		t.Errorf("started: got %v, want the run's workflow once with its input", ours)
	}
	var recorded string
	pgtest.Scan(t, `SELECT workflow_id FROM platform.operation WHERE id = $1`, []any{june.OperationID}, &recorded)
	if recorded != id {
		t.Errorf("recorded workflow: got %s, want %s", recorded, id)
	}
}

// TestDepreciator: without a workflow engine the in-process runner
// carries out every unfinished run, whatever its Operation's status, and
// a round with none does nothing; failures are reported.
func TestDepreciator(t *testing.T) {
	s := deprSetup(t)
	june := s.start(t, 6)
	pgtest.Exec(t, `UPDATE platform.operation SET status = 'cancelled', version = version + 1 WHERE id = $1`, june.OperationID)
	logs := &pgtest.SyncBuffer{}
	w := &workflows.Depreciator{Depreciations: s.d, Log: slog.New(slog.NewTextHandler(logs, nil))}
	ctx := context.Background()
	if n, err := w.RunOnce(ctx); err != nil || n < 1 {
		t.Fatalf("round: got %d %v, want the run carried out", n, err)
	}
	if got := deprStateOf(t, june); got.Run != "posted/3" || !strings.HasPrefix(got.Operation, "cancelled/") {
		t.Errorf("the run: got %+v, want it posted, its Operation left cancelled", got)
	}
	t.Run("failures", func(t *testing.T) {
		july := s.start(t, 7)
		defer func() { s.d.Begin = nil }()
		s.d.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
			if scope.TenantID == s.e.Tenant.ID && !scope.ReadOnly {
				return nil, errors.New("database down")
			}
			return pgtest.Pool(t).Begin(ctx, scope)
		}
		if _, err := w.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), july.RunID.String()) {
			t.Errorf("a step failing: got %v, want the run named", err)
		}
		s.d.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
			if scope.TenantID == s.e.Tenant.ID {
				return nil, errors.New("database down")
			}
			return pgtest.Pool(t).Begin(ctx, scope)
		}
		if _, err := w.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), "database down") {
			t.Errorf("the tenant's runs unread: got %v, want the failure reported", err)
		}
		s.d.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }
		if _, err := w.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), "database down") {
			t.Errorf("the tenants unread: got %v, want the failure reported", err)
		}
		pool := pgtest.Pool(t)
		count := 0
		s.d.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, scope)
			if err != nil || scope.TenantID != s.e.Tenant.ID {
				return tx, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: 1, Count: &count}, nil
		}
		if _, err := w.RunOnce(ctx); !errors.Is(err, pgtest.ErrInjected) {
			t.Errorf("the tenant's runs failing to read: got %v, want the injected failure", err)
		}
		s.d.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, scope)
			if err != nil || scope.TenantID != uuid.Nil {
				return tx, err
			}
			c := 0
			return &pgtest.FailingTx{Tx: tx, FailAt: 1, Count: &c}, nil
		}
		if _, err := w.RunOnce(ctx); !errors.Is(err, pgtest.ErrInjected) {
			t.Errorf("the tenants failing to read: got %v, want the injected failure", err)
		}
	})
	t.Run("run loop", func(t *testing.T) { // July's run, left by the failures, is carried out
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { w.Run(runCtx); close(done) }()
		t.Cleanup(func() { cancel(); <-done })
		pgtest.Eventually(t, 10*time.Second, func() bool { return strings.Contains(logs.String(), "depreciation runs ended") },
			"the runner did not log the runs it ended: %s", logs)
	})
}

// TestDepreciationRunReplay: a history of the first version of
// DepreciationRun (testdata, recorded on a development server with
// stand-in activities: a plan of two batches, both posted, the finish)
// replays on the current code without a nondeterminism error. A change to
// the workflow that issues other commands adds a version
// (workflow.GetVersion, ADR-0012) and keeps this history replaying.
func TestDepreciationRunReplay(t *testing.T) {
	r := worker.NewWorkflowReplayer()
	r.RegisterWorkflowWithOptions(workflows.DepreciationRun, sdkworkflow.RegisterOptions{Name: workflows.DepreciationWorkflow})
	if err := r.ReplayWorkflowHistoryFromJSONFile(nil, filepath.Join("testdata", "depreciation_run_v1.json")); err != nil {
		t.Fatalf("replay of the version 1 history: %v", err)
	}
}

// TestDepreciatorFailsARun: without a workflow engine a run whose period
// was closed after it started fails at its first batch with the ledger's
// problem, its later batches left pending.
func TestDepreciatorFailsARun(t *testing.T) {
	s := deprSetup(t)
	s.machine(t, "Lathe")
	h := app.Assets(app.Config{})
	h.BatchSize = 1
	s.d.Runs = h
	in := s.start(t, 6)
	s.close(t, 6)
	w := &workflows.Depreciator{Depreciations: s.d, Log: slog.New(slog.DiscardHandler)}
	if err := w.Depreciate(context.Background(), in); err != nil {
		t.Fatalf("the run: got %v, want it failed without an error", err)
	}
	if got := deprStateOf(t, in); !strings.HasPrefix(got.Run, "failed/") || got.Batches != "pending pending" {
		t.Errorf("the run: got %+v, want it failed, both batches pending", got)
	}
}
