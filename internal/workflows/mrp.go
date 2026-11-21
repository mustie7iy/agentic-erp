package workflows

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
	"github.com/mustie7iy/agentic-erp/internal/platform/worker"
)

// Planning runs (ADR-0084 §3, S2k): the request queues a run of a plant
// with its Operation; the dispatcher starts MrpRun for it, whose activity
// plans the plant in one transaction, or fails the run on a business
// problem. The mrp context is a leaf that no context imports (ADR-0084
// §1), so the workflow reaches it through the Mrp port, which the
// composition root wires to it.

// MrpKind is the kind of a planning run's Operation (mrp.KindRun, which
// the composition root's test holds equal).
const MrpKind = "mrp.run"

// Mrp is a planning run's work as the mrp context does it, each step in
// the transaction and as the principal the caller set up (uow): carry the
// run out, and fail it with a problem. Both are idempotent.
type Mrp interface {
	PlanRun(ctx context.Context, run uuid.UUID) error
	FailRun(ctx context.Context, run uuid.UUID, why *problem.Problem) error
	// UnfinishedRuns lists the runs queued in the transaction's tenant, by
	// run with its Operation: the in-process runner's work.
	UnfinishedRuns(ctx context.Context, tx pgx.Tx) (map[uuid.UUID]uuid.UUID, error)
}

// MrpInput names the run an MrpRun workflow carries out.
type MrpInput struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	RunID       uuid.UUID `json:"run_id"`
	OperationID uuid.UUID `json:"operation_id"`
}

// MrpWorkflow is the registered name of MrpRun.
const MrpWorkflow = "MrpRun"

// MrpWorkflowID is the id of a run's workflow: {tenant}-mrp_run-{run}
// (ADR-0012, the run as business key).
func MrpWorkflowID(tenant, run uuid.UUID) string {
	return fmt.Sprintf("%s-mrp_run-%s", tenant, run)
}

// mrpSteps are a run's steps. The workflow runs each as an activity and
// the Planner calls them directly, so both share one control flow (run).
type mrpSteps struct {
	plan func() error
	fail func(why problem.Problem) error
	// final is true where a step's error is final: in the workflow, where
	// it comes back only once the step's retries are spent.
	final bool
}

// mrpInterrupted is the problem a run fails with when its step's error
// outlasts its retries, which frees its plant for a new run.
func mrpInterrupted() problem.Problem {
	return *problem.New(http.StatusInternalServerError, "platform.internal", "Internal error", "The planning run stopped on an error that outlasted "+
		"its retries; it proposed nothing. Start a new run of the plant.")
}

// run plans the run; a business problem (a cancelled Operation's
// mrp.run_cancelled included) fails the run instead, and so does, where
// it is final, any other error, which the run still returns once the run
// is failed. In a workflow it only sequences the steps, so it is
// deterministic.
func (s mrpSteps) run() error {
	err := s.plan()
	if why, ok := problemOf(err); ok {
		return s.fail(why)
	}
	if err != nil && s.final {
		if failed := s.fail(mrpInterrupted()); failed != nil {
			return failed
		}
	}
	return err
}

// MrpRun carries out a planning run: one activity plans the plant in one
// transaction, so a run proposes everything or nothing. A failure of the
// machinery is tried five times with backoff (ADR-0012); one that
// outlasts them fails the run as an internal error and fails the
// workflow with it. The run's failure is tried until it is recorded
// (recording), so the run always ends and frees its plant. A business
// problem is not retried. The activities run on a disconnected context,
// so the step under way commits or not as a whole. The steps return
// Future.Get's error unwrapped, as DepreciationRun's do.
func MrpRun(ctx workflow.Context, in MrpInput) error {
	ctx, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: time.Minute, MaximumAttempts: 5,
			NonRetryableErrorTypes: []string{ProblemError}}})
	var a *Mrps
	return mrpSteps{
		plan: func() error { return perform(ctx, in.TenantID, nil, a.PlanMrpRun, in) },
		fail: func(why problem.Problem) error {
			return perform(recording(ctx), in.TenantID, nil, a.FailMrpRun, in, why)
		},
		final: true,
	}.run()
}

// Mrps holds the planning run's activities: each runs one step of the
// mrp context in a transaction of the run's tenant, as the person who
// started the run (the Operation's requester).
type Mrps struct {
	Pool       *database.Pool
	AppRole    string // erp_app: in the tenant's scope
	WorkerRole string // erp_worker: finds the tenants with runs to carry out (Planner)
	Runs       Mrp
	Actors     Actors
	Now        func() time.Time
	// Begin starts transactions; nil means Pool.Begin. Tests inject failures.
	Begin database.BeginFunc
}

func (m *Mrps) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	return database.BeginActive(ctx, m.Pool, m.Begin, s)
}

// step runs fn in one transaction of the run's tenant as its requester; a
// business problem comes back as the non-retryable error the workflow
// records. The plan needs its requester still active (working): one
// blocked or deprovisioned since the start is refused with
// identity.principal_blocked (PO 2026-11-03), which fails the run; the
// failure's step records how the run ended whatever their status.
func (m *Mrps) step(ctx context.Context, in MrpInput, acting bool, fn func(ctx context.Context) error) error {
	what := "planning run " + in.RunID.String()
	actor := requester
	if acting {
		actor = working
	}
	return asProblem(uow.Job{Begin: m.begin, Scope: database.Scope{Role: m.AppRole, TenantID: in.TenantID}, Now: m.Now,
		Name: "workflows: " + what}.As(ctx, actor(m.Actors, in.OperationID, what), fn))
}

// PlanMrpRun carries the run out.
func (m *Mrps) PlanMrpRun(ctx context.Context, in MrpInput) error {
	return m.step(ctx, in, true, func(ctx context.Context) error { return m.Runs.PlanRun(ctx, in.RunID) })
}

// FailMrpRun sets the run failed with the problem that stopped it.
func (m *Mrps) FailMrpRun(ctx context.Context, in MrpInput, why problem.Problem) error {
	return m.step(ctx, in, false, func(ctx context.Context) error { return m.Runs.FailRun(ctx, in.RunID, &why) })
}

// Planner is the in-process planning run worker of a cell without a
// workflow engine: each round it carries out, with the steps the workflow
// runs, every run of every tenant still queued. A run whose Operation was
// cancelled is taken up too, so that its step fails it; a step failing on
// the machinery is tried again the next round, and cancelling the
// Operation ends a run that keeps failing so.
type Planner struct {
	Mrps     *Mrps
	Interval time.Duration // between rounds; default 1 second
	Log      *slog.Logger
}

// Plan carries out one run: the steps of the workflow, each called
// directly in its own transaction.
func (w *Planner) Plan(ctx context.Context, in MrpInput) error {
	m := w.Mrps
	return mrpSteps{
		plan: func() error { return m.PlanMrpRun(ctx, in) },
		fail: func(why problem.Problem) error { return m.FailMrpRun(ctx, in, why) },
	}.run()
}

// RunOnce carries out every unfinished run and returns how many it ended.
func (w *Planner) RunOnce(ctx context.Context) (int, error) {
	m := w.Mrps
	runs, err := unfinishedRuns(ctx, m.begin, m.AppRole, m.WorkerRole, MrpKind, m.Runs.UnfinishedRuns)
	failures := []error{err}
	n := 0
	for _, r := range runs {
		if err := w.Plan(ctx, MrpInput{TenantID: r.tenant, RunID: r.run, OperationID: r.operation}); err != nil {
			failures = append(failures, fmt.Errorf("planning runs: run %s: %w", r.run, err))
			continue
		}
		n++
	}
	return n, errors.Join(failures...)
}

// Run carries out runs until ctx ends; errors are logged and retried after
// the interval.
func (w *Planner) Run(ctx context.Context) {
	worker.Loop(ctx, w.Interval, time.Second, func(ctx context.Context) (bool, error) {
		n, err := w.RunOnce(ctx)
		if n > 0 && !worker.Failed(ctx, err) {
			w.Log.InfoContext(ctx, "planning runs ended", "count", n)
		}
		return false, err
	}, w.Log, "planning runs failed")
}
