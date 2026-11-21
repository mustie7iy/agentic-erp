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

// Planning runs (ADR-0084 §3, S2k): the workflow carries a run out
// through the mrp context's port in one activity, or fails it.

type mrpScenario struct {
	e     fixture.Env
	m     *workflows.Mrps
	plant uuid.UUID
}

// mrpSetup is a tenant with an active plant P1 of company 2000 that plans
// nothing, so its runs complete with nothing planned. The test's own runs
// are carried out by the test, never by the dispatcher, which reads every
// tenant's queue: at the end their Operations still without a workflow
// are marked as taken, as conSetup's.
func mrpSetup(t *testing.T) *mrpScenario {
	t.Helper()
	e := fixture.NewWith(t, nil, nil, "tenancy.org.manage", "mrp.plan.read", "mrp.run.start", "platform.operation.read", "platform.operation.cancel")
	t.Cleanup(func() {
		pgtest.Exec(t, `UPDATE platform.operation SET workflow_id = 'carried out by the test', version = version + 1
			WHERE tenant_id = $1 AND kind = $2 AND workflow_id IS NULL`, e.Tenant.ID, workflows.MrpKind)
	})
	u, _ := e.Must(201, "POST", "/v1/org-units", fmt.Sprintf(`{"legal_entity_id": %q, "kind": "plant", "code": "P1", "name": "Plant P1"}`, e.DE), "")
	e.Must(200, "POST", "/v1/org-units/"+u["id"].(string)+":activate", "", `"1"`)
	return &mrpScenario{e: e, plant: uuid.MustParse(u["id"].(string)), m: &workflows.Mrps{Pool: pgtest.Pool(t), AppRole: "erp_app", WorkerRole: "erp_worker",
		Runs: app.MRP(app.Config{}), Actors: identity.Actors{}}}
}

// start starts a run of the plant.
func (s *mrpScenario) start(t *testing.T) workflows.MrpInput {
	t.Helper()
	op, _ := s.e.Must(202, "POST", "/v1/mrp-runs", fmt.Sprintf(`{"plant_id": %q}`, s.plant), "")
	run := uuid.MustParse(strings.TrimPrefix(op["resource"].(string), "/v1/mrp-runs/"))
	return workflows.MrpInput{TenantID: s.e.Tenant.ID, RunID: run, OperationID: uuid.MustParse(op["id"].(string))}
}

// mrpStateOf is the run's status and problem and its Operation's status.
func mrpStateOf(t *testing.T, in workflows.MrpInput) string {
	t.Helper()
	var run, code, op string
	pgtest.Scan(t, `SELECT r.status, coalesce(r.error->>'code', ''), o.status FROM mrp.run r JOIN platform.operation o ON o.id = r.operation_id
		WHERE r.id = $1`, []any{in.RunID}, &run, &code, &op)
	return run + " " + code + " " + op
}

// runMrp runs the workflow in the SDK's test environment with the real
// activities against the package database.
func (s *mrpScenario) runMrp(t *testing.T, in workflows.MrpInput) error {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := workflowEnv(&suite)
	env.RegisterWorkflowWithOptions(workflows.MrpRun, sdkworkflow.RegisterOptions{Name: workflows.MrpWorkflow})
	env.RegisterActivity(s.m)
	env.ExecuteWorkflow(workflows.MrpWorkflow, in)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	return env.GetWorkflowError()
}

// TestMrpRunWorkflow: the workflow carries a run out, completed and its
// Operation succeeded; one whose Operation was cancelled fails with
// mrp.run_cancelled, its Operation staying cancelled.
func TestMrpRunWorkflow(t *testing.T) {
	s := mrpSetup(t)
	in := s.start(t)
	if err := s.runMrp(t, in); err != nil {
		t.Fatalf("the run: %v", err)
	}
	if got := mrpStateOf(t, in); got != "completed  succeeded" {
		t.Errorf("the run: got %s, want completed, its Operation succeeded", got)
	}
	again := s.start(t)
	path := "/v1/operations/" + again.OperationID.String()
	_, tag := s.e.Must(200, "GET", path, "", "")
	s.e.Must(200, "POST", path+":cancel", "", tag)
	if err := s.runMrp(t, again); err != nil {
		t.Fatalf("the cancelled run: %v", err)
	}
	if got := mrpStateOf(t, again); got != "failed mrp.run_cancelled cancelled" {
		t.Errorf("the cancelled run: got %s, want it failed with mrp.run_cancelled, its Operation cancelled", got)
	}
}

// stuckPlans is the mrp context with every plan failing on the
// machinery.
type stuckPlans struct {
	workflows.Mrp
	tries *atomic.Int32
}

func (s stuckPlans) PlanRun(context.Context, uuid.UUID) error {
	s.tries.Add(1)
	return errors.New("procurement unreachable")
}

// TestMrpRunInterrupted: a plan failing on the machinery is tried five
// times; then the run fails as an internal error, freeing its plant, its
// Operation fails with it, and so does the workflow, with the plan's
// error.
func TestMrpRunInterrupted(t *testing.T) {
	s := mrpSetup(t)
	tries := &atomic.Int32{}
	s.m.Runs = stuckPlans{Mrp: app.MRP(app.Config{}), tries: tries}
	in := s.start(t)
	if err := s.runMrp(t, in); err == nil || !strings.Contains(err.Error(), "procurement unreachable") {
		t.Errorf("the workflow: got %v, want the plan's error", err)
	}
	if n := tries.Load(); n != 5 {
		t.Errorf("the plan's tries: got %d, want 5", n)
	}
	if got := mrpStateOf(t, in); got != "failed platform.internal failed" {
		t.Errorf("the run: got %s, want it and its Operation failed with platform.internal", got)
	}
	s.start(t) // the plant is free again
}

// refusedFailure is the mrp context with every plan failing on the
// machinery and the run's failure refused with a business problem.
type refusedFailure struct{ stuckPlans }

func (refusedFailure) FailRun(context.Context, uuid.UUID, *problem.Problem) error {
	return problem.Conflict("test.fail_refused", "Refused.")
}

// TestMrpRunFailureRefused: when the failure of an interrupted run is
// refused with a business problem, which is not retried, the workflow
// fails with that refusal rather than the plan's error, and the run is
// not failed (as DepreciationRun's refused failure, the workflow ends).
func TestMrpRunFailureRefused(t *testing.T) {
	s := mrpSetup(t)
	s.m.Runs = refusedFailure{stuckPlans{Mrp: app.MRP(app.Config{}), tries: &atomic.Int32{}}}
	in := s.start(t)
	err := s.runMrp(t, in)
	if err == nil || !strings.Contains(err.Error(), "Refused.") {
		t.Errorf("the workflow: got %v, want the failure's refusal", err)
	}
	if got := mrpStateOf(t, in); strings.HasPrefix(got, "failed") {
		t.Errorf("the run: got %s, want it not failed, its failure refused", got)
	}
}

// TestDispatchMrp: a queued run's Operation becomes an MrpRun named by
// the run, once; the id is recorded on the Operation.
func TestDispatchMrp(t *testing.T) {
	s := mrpSetup(t)
	in := s.start(t)
	engine := &fakeEngine{}
	d := &workflows.Dispatcher{Pool: pgtest.Pool(t), WorkerRole: "erp_worker", Client: engine, Queue: "erp-test"}
	ctx := context.Background()
	id := workflows.MrpWorkflowID(in.TenantID, in.RunID)
	if want := fmt.Sprintf("%s-mrp_run-%s", in.TenantID, in.RunID); id != want {
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
	if len(ours) != 1 || ours[0] != "MrpRun erp-test true" {
		t.Errorf("started: got %v, want the run's workflow once with its input", ours)
	}
}

// TestPlanner: without a workflow engine the in-process runner carries
// out every queued run; a step failing on the machinery is reported with
// the run; its loop logs the runs it ended.
func TestPlanner(t *testing.T) {
	s := mrpSetup(t)
	in := s.start(t)
	logs := &pgtest.SyncBuffer{}
	w := &workflows.Planner{Mrps: s.m, Log: slog.New(slog.NewTextHandler(logs, nil))}
	ctx := context.Background()
	if n, err := w.RunOnce(ctx); err != nil || n < 1 {
		t.Fatalf("round: got %d %v, want the run ended", n, err)
	}
	if got := mrpStateOf(t, in); got != "completed  succeeded" {
		t.Errorf("the run: got %s, want completed", got)
	}
	again := s.start(t)
	s.m.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
		if scope.TenantID == s.e.Tenant.ID && !scope.ReadOnly {
			return nil, errors.New("database down")
		}
		return pgtest.Pool(t).Begin(ctx, scope)
	}
	if _, err := w.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), again.RunID.String()) {
		t.Errorf("a step failing: got %v, want the run named", err)
	}
	s.m.Begin = nil
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { w.Run(runCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	pgtest.Eventually(t, 10*time.Second, func() bool { return strings.Contains(logs.String(), "planning runs ended") },
		"the runner did not log the runs it ended: %s", logs)
	if got := mrpStateOf(t, again); got != "completed  succeeded" {
		t.Errorf("the run left by the failure: got %s, want it completed by the loop", got)
	}
}

// TestMrpRunReplay: histories of the first version of MrpRun (testdata,
// recorded on a development server with stand-in activities at batch 87's
// integration: a run planned, and one whose plan fails with a business
// problem and goes to FailMrpRun) replay on the current code without a
// nondeterminism error. A change to the workflow that issues other
// commands adds a version (workflow.GetVersion, ADR-0012) and keeps these
// histories replaying.
func TestMrpRunReplay(t *testing.T) {
	for _, file := range []string{"mrp_run_v1.json", "mrp_run_problem_v1.json"} {
		t.Run(file, func(t *testing.T) {
			r := worker.NewWorkflowReplayer()
			r.RegisterWorkflowWithOptions(workflows.MrpRun, sdkworkflow.RegisterOptions{Name: workflows.MrpWorkflow})
			if err := r.ReplayWorkflowHistoryFromJSONFile(nil, filepath.Join("testdata", file)); err != nil {
				t.Fatalf("replay of %s: %v", file, err)
			}
		})
	}
}
