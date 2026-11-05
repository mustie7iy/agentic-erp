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

	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
	"github.com/mustie7iy/agentic-erp/internal/platform/worker"
)

// Depreciation runs (ADR-0057 §7, T57f): the request queues a run with its
// Operation; the dispatcher starts DepreciationRun for it, whose
// activities plan the run, post its batches one transaction each and
// finish it, or fail it on a business problem. The assets context is a
// leaf that no context imports (ADR-0057 §2), so the workflow reaches it
// through the Depreciation port, which the composition root wires to it.

// DepreciateKind is the kind of a depreciation run's Operation
// (assets.KindDepreciate, which the composition root's test holds equal).
const DepreciateKind = "assets.depreciate"

// Depreciation is a depreciation run's work as the assets context does it
// (ADR-0057 §7), each step in the transaction and as the principal the
// caller set up (uow): plan the run and list its batches, post a batch,
// finish the run, and fail it with a problem. Each is idempotent.
type Depreciation interface {
	PlanDepreciationRun(ctx context.Context, run uuid.UUID) ([]int32, error)
	PostDepreciationBatch(ctx context.Context, run uuid.UUID, batch int32) error
	FinishDepreciationRun(ctx context.Context, run uuid.UUID) error
	FailDepreciationRun(ctx context.Context, run uuid.UUID, why *problem.Problem) error
	// UnfinishedDepreciationRuns lists the runs queued or running in the
	// transaction's tenant, by run with its Operation: the in-process
	// runner's work.
	UnfinishedDepreciationRuns(ctx context.Context, tx pgx.Tx) (map[uuid.UUID]uuid.UUID, error)
}

// DepreciationInput names the run a DepreciationRun workflow carries out.
type DepreciationInput struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	RunID       uuid.UUID `json:"run_id"`
	OperationID uuid.UUID `json:"operation_id"`
}

// DepreciationWorkflow is the registered name of DepreciationRun.
const DepreciationWorkflow = "DepreciationRun"

// DepreciationWorkflowID is the id of a run's workflow:
// {tenant}-depreciation_run-{run} (ADR-0012, the run as business key).
func DepreciationWorkflowID(tenant, run uuid.UUID) string {
	return fmt.Sprintf("%s-depreciation_run-%s", tenant, run)
}

// depreciationSteps are a run's steps. The workflow runs each as an
// activity and the Depreciator calls them directly, so both share one
// control flow (run).
type depreciationSteps struct {
	plan   func() ([]int32, error)
	post   func(batch int32) error
	finish func() error
	fail   func(why problem.Problem) error
	// final is true where a step's error is final: in the workflow, where
	// it comes back only once the step's retries are spent.
	final bool
}

// interrupted is the problem a run fails with when a step's error outlasts
// its retries: the run then holds its company and ledger no longer.
func interrupted() problem.Problem {
	return *problem.New(http.StatusInternalServerError, "platform.internal", "Internal error", "The depreciation run stopped on an error that outlasted "+
		"its retries; the batches already posted stay posted, and a new run of the period posts what is left.")
}

// run plans the run, posts its batches in order and finishes it; a
// business problem (a cancelled Operation's assets.run_cancelled
// included) fails the run instead, and so does, where it is final, any
// other error, which the run still returns once the run is failed, so the
// workflow fails with it for the operators. In a workflow it only
// sequences the steps, so it is deterministic.
func (s depreciationSteps) run() error {
	batches, err := s.plan()
	for _, b := range batches {
		if err != nil {
			break
		}
		err = s.post(b)
	}
	if err == nil {
		err = s.finish()
	}
	if why, ok := problemOf(err); ok {
		return s.fail(why)
	}
	if err != nil && s.final {
		if failed := s.fail(interrupted()); failed != nil {
			return failed
		}
	}
	return err
}

// DepreciationRun carries out a depreciation run. A cancellation of its
// Operation stops it at the next step: each step reads the Operation and
// refuses once it is cancelled (assets.run_cancelled), which fails the
// run, so the run frees its company and ledger (one run at a time) and the
// batches posted before stay posted. The engine's cancellation, which
// :cancel also sends, does not cut a step off: the activities run on a
// context it does not reach, so the step under way commits or not as a
// whole and the failing step still runs. A failure of the machinery is
// tried five times with backoff, as the other workflows' (ADR-0012);
// one that outlasts them fails the run as an internal error and fails the
// workflow with it. A business problem is not retried.
//
// The steps return Future.Get's error unwrapped: it is the workflow result,
// and the engine records Get's ActivityError, which carries the activity
// type and id, as the failure; a wrap would put a generic wrapError failure
// on top and add no context.
func DepreciationRun(ctx workflow.Context, in DepreciationInput) error {
	ctx, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: time.Minute, MaximumAttempts: 5,
			NonRetryableErrorTypes: []string{ProblemError}}})
	var a *Depreciations
	return depreciationSteps{
		plan: func() ([]int32, error) {
			var batches []int32
			err := workflow.ExecuteActivity(ctx, a.PlanDepreciationRun, in).Get(ctx, &batches)
			return batches, err //nolint:wrapcheck // the workflow result, see DepreciationRun
		},
		post: func(batch int32) error {
			return workflow.ExecuteActivity(ctx, a.PostDepreciationBatch, in, batch).Get(ctx, nil)
		},
		finish: func() error {
			return workflow.ExecuteActivity(ctx, a.FinishDepreciationRun, in).Get(ctx, nil)
		},
		fail: func(why problem.Problem) error {
			return workflow.ExecuteActivity(ctx, a.FailDepreciationRun, in, why).Get(ctx, nil)
		},
		final: true,
	}.run()
}

// Depreciations holds the depreciation run's activities: each runs one
// step of the assets context in a transaction of the run's tenant, as the
// person who started the run (the Operation's requester).
type Depreciations struct {
	Pool       *database.Pool
	AppRole    string // erp_app: in the tenant's scope
	WorkerRole string // erp_worker: finds the tenants with runs to carry out (Depreciator)
	Runs       Depreciation
	Actors     Actors
	Now        func() time.Time
	// Begin starts transactions; nil means Pool.Begin. Tests inject failures.
	Begin database.BeginFunc
}

func (d *Depreciations) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	return database.BeginOr(ctx, d.Pool, d.Begin, s)
}

// as runs fn in one transaction of the run's tenant as its requester; a
// business problem comes back as the non-retryable error the workflow
// records. It is the failure's step: it records how the run ended, as the
// requester whatever their status.
func (d *Depreciations) as(ctx context.Context, in DepreciationInput, fn func(ctx context.Context) error) error {
	return d.step(ctx, in, requester, fn)
}

// acting is as for a step that does the run's work (the plan, a batch, the
// finish): its requester must still be active (working). One blocked or
// deprovisioned since the start is refused with identity.principal_blocked
// (PO 2026-11-03), which fails the run through FailDepreciationRun; the
// batches posted before stay posted.
func (d *Depreciations) acting(ctx context.Context, in DepreciationInput, fn func(ctx context.Context) error) error {
	return d.step(ctx, in, working, fn)
}

// step runs fn in one transaction of the run's tenant as who the actor
// reads.
func (d *Depreciations) step(ctx context.Context, in DepreciationInput, actor func(Actors, uuid.UUID, string) func(context.Context, pgx.Tx) (reqctx.Principal, bool, error),
	fn func(ctx context.Context) error) error {
	what := "depreciation run " + in.RunID.String()
	return asProblem(uow.Job{Begin: d.begin, Scope: database.Scope{Role: d.AppRole, TenantID: in.TenantID}, Now: d.Now,
		Name: "workflows: " + what}.As(ctx, actor(d.Actors, in.OperationID, what), fn))
}

// PlanDepreciationRun plans the run and returns its batches.
func (d *Depreciations) PlanDepreciationRun(ctx context.Context, in DepreciationInput) ([]int32, error) {
	var batches []int32
	err := d.acting(ctx, in, func(ctx context.Context) error {
		var err error
		batches, err = d.Runs.PlanDepreciationRun(ctx, in.RunID)
		return err
	})
	return batches, err
}

// PostDepreciationBatch posts one batch of the run.
func (d *Depreciations) PostDepreciationBatch(ctx context.Context, in DepreciationInput, batch int32) error {
	return d.acting(ctx, in, func(ctx context.Context) error { return d.Runs.PostDepreciationBatch(ctx, in.RunID, batch) })
}

// FinishDepreciationRun sets the run posted.
func (d *Depreciations) FinishDepreciationRun(ctx context.Context, in DepreciationInput) error {
	return d.acting(ctx, in, func(ctx context.Context) error { return d.Runs.FinishDepreciationRun(ctx, in.RunID) })
}

// FailDepreciationRun sets the run failed with the problem that stopped it.
func (d *Depreciations) FailDepreciationRun(ctx context.Context, in DepreciationInput, why problem.Problem) error {
	return d.as(ctx, in, func(ctx context.Context) error { return d.Runs.FailDepreciationRun(ctx, in.RunID, &why) })
}

// Depreciator is the in-process depreciation run worker of a cell without
// a workflow engine: each round it carries out, with the steps the
// workflow runs, every run of every tenant still queued or running, until
// the engine takes over. A run whose Operation was cancelled is taken up
// too, so that its next step fails it; a step failing on the machinery is
// tried again the next round, and cancelling the Operation ends a run
// that keeps failing so.
type Depreciator struct {
	Depreciations *Depreciations
	Interval      time.Duration // between rounds; default 1 second
	Log           *slog.Logger
}

// unfinishedRun is a run of a tenant still queued or running, with its
// Operation: the in-process runners' work.
type unfinishedRun struct{ tenant, run, operation uuid.UUID }

// unfinishedRuns lists the runs to carry out of every tenant with
// Operations of the kind: the tenants as the worker role, each tenant's
// unfinished runs (list) in its scope, in no particular order across
// tenants. A tenant whose runs cannot be read is reported and the others
// listed.
func unfinishedRuns(ctx context.Context, begin func(context.Context, database.Scope) (pgx.Tx, error), appRole, workerRole, kind string,
	list func(context.Context, pgx.Tx) (map[uuid.UUID]uuid.UUID, error)) ([]unfinishedRun, error) {
	tx, err := begin(ctx, database.Scope{Role: workerRole, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	tenants, err := operations.TenantsOf(ctx, tx, kind)
	if err != nil {
		return nil, fmt.Errorf("workflows: %w", err)
	}
	var runs []unfinishedRun
	var failures []error
	for _, tenant := range tenants {
		unfinished, err := tenantRuns(ctx, begin, appRole, tenant, list)
		if err != nil {
			failures = append(failures, fmt.Errorf("workflows: %s runs of tenant %s: %w", kind, tenant, err))
		}
		for run, op := range unfinished {
			runs = append(runs, unfinishedRun{tenant: tenant, run: run, operation: op})
		}
	}
	return runs, errors.Join(failures...)
}

// tenantRuns reads a tenant's unfinished runs in its scope.
func tenantRuns(ctx context.Context, begin func(context.Context, database.Scope) (pgx.Tx, error), appRole string, tenant uuid.UUID,
	list func(context.Context, pgx.Tx) (map[uuid.UUID]uuid.UUID, error)) (map[uuid.UUID]uuid.UUID, error) {
	tx, err := begin(ctx, database.Scope{Role: appRole, TenantID: tenant, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	return list(ctx, tx)
}

// Depreciate carries out one run: the steps of the workflow, in order,
// each called directly in its own transaction.
func (w *Depreciator) Depreciate(ctx context.Context, in DepreciationInput) error {
	d := w.Depreciations
	return depreciationSteps{
		plan:   func() ([]int32, error) { return d.PlanDepreciationRun(ctx, in) },
		post:   func(batch int32) error { return d.PostDepreciationBatch(ctx, in, batch) },
		finish: func() error { return d.FinishDepreciationRun(ctx, in) },
		fail:   func(why problem.Problem) error { return d.FailDepreciationRun(ctx, in, why) },
	}.run()
}

// RunOnce carries out every unfinished run and returns how many it ended.
func (w *Depreciator) RunOnce(ctx context.Context) (int, error) {
	d := w.Depreciations
	runs, err := unfinishedRuns(ctx, d.begin, d.AppRole, d.WorkerRole, DepreciateKind, d.Runs.UnfinishedDepreciationRuns)
	failures := []error{err}
	n := 0
	for _, r := range runs {
		if err := w.Depreciate(ctx, DepreciationInput{TenantID: r.tenant, RunID: r.run, OperationID: r.operation}); err != nil {
			failures = append(failures, fmt.Errorf("depreciation runs: run %s: %w", r.run, err))
			continue
		}
		n++
	}
	return n, errors.Join(failures...)
}

// Run carries out runs until ctx ends; errors are logged and retried after
// the interval.
func (w *Depreciator) Run(ctx context.Context) {
	worker.Loop(ctx, w.Interval, time.Second, func(ctx context.Context) (bool, error) {
		n, err := w.RunOnce(ctx)
		if n > 0 && !worker.Failed(ctx, err) {
			w.Log.InfoContext(ctx, "depreciation runs ended", "count", n)
		}
		return false, err
	}, w.Log, "depreciation runs failed")
}
