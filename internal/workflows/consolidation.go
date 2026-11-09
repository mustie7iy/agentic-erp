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

// Consolidation runs (ADR-0059 §6, T68d): the request queues a run with
// its Operation; the dispatcher starts ConsolidationRun for it, whose
// activities prepare the run, translate its companies one transaction
// each, eliminate and complete it, or fail it on a business problem. The
// consolidation context is a leaf that no context imports (ADR-0059 §2),
// so the workflow reaches it through the Consolidation port, which the
// composition root wires to it.

// ConsolidateKind is the kind of a consolidation run's Operation
// (consolidation.KindRun, which the composition root's test holds equal).
const ConsolidateKind = "consolidation.run"

// Consolidation is a consolidation run's work as the consolidation context
// does it (ADR-0059 §6), each step in the transaction and as the principal
// the caller set up (uow): prepare the run and list its companies,
// translate one, eliminate, complete the run, and fail it with a problem.
// Each is idempotent.
type Consolidation interface {
	PrepareConsolidationRun(ctx context.Context, run uuid.UUID) ([]uuid.UUID, error)
	TranslateMember(ctx context.Context, run, company uuid.UUID) error
	EliminateConsolidationRun(ctx context.Context, run uuid.UUID) error
	CompleteConsolidationRun(ctx context.Context, run uuid.UUID) error
	FailConsolidationRun(ctx context.Context, run uuid.UUID, why *problem.Problem) error
	// UnfinishedConsolidationRuns lists the runs queued or running in the
	// transaction's tenant, by run with its Operation: the in-process
	// runner's work.
	UnfinishedConsolidationRuns(ctx context.Context, tx pgx.Tx) (map[uuid.UUID]uuid.UUID, error)
}

// ConsolidationInput names the run a ConsolidationRun workflow carries out.
type ConsolidationInput struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	RunID       uuid.UUID `json:"run_id"`
	OperationID uuid.UUID `json:"operation_id"`
}

// ConsolidationWorkflow is the registered name of ConsolidationRun.
const ConsolidationWorkflow = "ConsolidationRun"

// ConsolidationWorkflowID is the id of a run's workflow:
// {tenant}-consolidation_run-{run} (ADR-0012, the run as business key).
func ConsolidationWorkflowID(tenant, run uuid.UUID) string {
	return fmt.Sprintf("%s-consolidation_run-%s", tenant, run)
}

// consolidationSteps are a run's steps. The workflow runs each as an
// activity and the Consolidator calls them directly, so both share one
// control flow (run).
type consolidationSteps struct {
	prepare   func() ([]uuid.UUID, error)
	translate func(company uuid.UUID) error
	eliminate func() error
	complete  func() error
	fail      func(why problem.Problem) error
	// final is true where a step's error is final: in the workflow, where
	// it comes back only once the step's retries are spent.
	final bool
}

// consolidationInterrupted is the problem a run fails with when a step's
// error outlasts its retries, which frees its group for a new run.
func consolidationInterrupted() problem.Problem {
	return *problem.New(http.StatusInternalServerError, "platform.internal", "Internal error", "The consolidation run stopped on an error that "+
		"outlasted its retries; start a new run of the period.")
}

// run prepares the run, translates its companies in order, eliminates and
// completes it; a business problem (a cancelled Operation's
// consolidation.run_cancelled included) fails the run instead, and so
// does, where it is final, any other error, which the run still returns
// once the run is failed, so the workflow fails with it for the
// operators. In a workflow it only sequences the steps, so it is
// deterministic.
func (s consolidationSteps) run() error {
	companies, err := s.prepare()
	for _, c := range companies {
		if err != nil {
			break
		}
		err = s.translate(c)
	}
	if err == nil {
		err = s.eliminate()
	}
	if err == nil {
		err = s.complete()
	}
	if why, ok := problemOf(err); ok {
		return s.fail(why)
	}
	if err != nil && s.final {
		if failed := s.fail(consolidationInterrupted()); failed != nil {
			return failed
		}
	}
	return err
}

// ConsolidationRun carries out a consolidation run. A cancellation of its
// Operation stops it at the next step: each step reads the Operation and
// refuses once it is cancelled (consolidation.run_cancelled), which fails
// the run and frees its group. The engine's cancellation does not cut a
// step off: the activities run on a disconnected context, so the step
// under way commits or not as a whole and the failing step still runs. A
// failure of the machinery is tried five times with backoff, as the
// other workflows' (ADR-0012); one that outlasts them fails the run as an
// internal error and fails the workflow with it. The run's failure is
// tried until it is recorded (recording), so the run always ends and
// frees its group. A business problem is not retried. The steps return
// Future.Get's error unwrapped, as DepreciationRun's do.
func ConsolidationRun(ctx workflow.Context, in ConsolidationInput) error {
	ctx, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: time.Minute, MaximumAttempts: 5,
			NonRetryableErrorTypes: []string{ProblemError}}})
	var a *Consolidations
	return consolidationSteps{
		prepare: func() ([]uuid.UUID, error) {
			var companies []uuid.UUID
			err := perform(ctx, in.TenantID, &companies, a.PrepareConsolidationRun, in)
			return companies, err
		},
		translate: func(company uuid.UUID) error {
			return perform(ctx, in.TenantID, nil, a.TranslateMember, in, company)
		},
		eliminate: func() error {
			return perform(ctx, in.TenantID, nil, a.EliminateConsolidationRun, in)
		},
		complete: func() error {
			return perform(ctx, in.TenantID, nil, a.CompleteConsolidationRun, in)
		},
		fail: func(why problem.Problem) error {
			return perform(recording(ctx), in.TenantID, nil, a.FailConsolidationRun, in, why)
		},
		final: true,
	}.run()
}

// Consolidations holds the consolidation run's activities: each runs one
// step of the consolidation context in a transaction of the run's tenant,
// as the person who started the run (the Operation's requester).
type Consolidations struct {
	Pool       *database.Pool
	AppRole    string // erp_app: in the tenant's scope
	WorkerRole string // erp_worker: finds the tenants with runs to carry out (Consolidator)
	Runs       Consolidation
	Actors     Actors
	Now        func() time.Time
	// Begin starts transactions; nil means Pool.Begin. Tests inject failures.
	Begin database.BeginFunc
}

func (c *Consolidations) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	return database.BeginActive(ctx, c.Pool, c.Begin, s)
}

// step runs fn in one transaction of the run's tenant as its requester; a
// business problem comes back as the non-retryable error the workflow
// records. A step doing the run's work (acting) needs its requester still
// active: one blocked or deprovisioned since the start is refused with
// identity.principal_blocked (PO 2026-11-03), which fails the run; the
// failure's step records how the run ended whatever their status.
func (c *Consolidations) step(ctx context.Context, in ConsolidationInput, acting bool, fn func(ctx context.Context) error) error {
	what := "consolidation run " + in.RunID.String()
	actor := requester
	if acting {
		actor = working
	}
	return asProblem(uow.Job{Begin: c.begin, Scope: database.Scope{Role: c.AppRole, TenantID: in.TenantID}, Now: c.Now,
		Name: "workflows: " + what}.As(ctx, actor(c.Actors, in.OperationID, what), fn))
}

// PrepareConsolidationRun prepares the run and returns its companies.
func (c *Consolidations) PrepareConsolidationRun(ctx context.Context, in ConsolidationInput) ([]uuid.UUID, error) {
	var companies []uuid.UUID
	err := c.step(ctx, in, true, func(ctx context.Context) error {
		var err error
		companies, err = c.Runs.PrepareConsolidationRun(ctx, in.RunID)
		return err
	})
	return companies, err
}

// TranslateMember translates one company of the run.
func (c *Consolidations) TranslateMember(ctx context.Context, in ConsolidationInput, company uuid.UUID) error {
	return c.step(ctx, in, true, func(ctx context.Context) error { return c.Runs.TranslateMember(ctx, in.RunID, company) })
}

// EliminateConsolidationRun eliminates the run's intercompany balances
// and investments (T68f, T68h).
func (c *Consolidations) EliminateConsolidationRun(ctx context.Context, in ConsolidationInput) error {
	return c.step(ctx, in, true, func(ctx context.Context) error { return c.Runs.EliminateConsolidationRun(ctx, in.RunID) })
}

// CompleteConsolidationRun sets the run computed.
func (c *Consolidations) CompleteConsolidationRun(ctx context.Context, in ConsolidationInput) error {
	return c.step(ctx, in, true, func(ctx context.Context) error { return c.Runs.CompleteConsolidationRun(ctx, in.RunID) })
}

// FailConsolidationRun sets the run failed with the problem that stopped
// it.
func (c *Consolidations) FailConsolidationRun(ctx context.Context, in ConsolidationInput, why problem.Problem) error {
	return c.step(ctx, in, false, func(ctx context.Context) error { return c.Runs.FailConsolidationRun(ctx, in.RunID, &why) })
}

// Consolidator is the in-process consolidation run worker of a cell
// without a workflow engine: each round it carries out, with the steps the
// workflow runs, every run of every tenant still queued or running. A run
// whose Operation was cancelled is taken up too, so that its next step
// fails it; a step failing on the machinery is tried again the next round,
// and cancelling the Operation ends a run that keeps failing so.
type Consolidator struct {
	Consolidations *Consolidations
	Interval       time.Duration // between rounds; default 1 second
	Log            *slog.Logger
}

// Consolidate carries out one run: the steps of the workflow, in order,
// each called directly in its own transaction.
func (w *Consolidator) Consolidate(ctx context.Context, in ConsolidationInput) error {
	c := w.Consolidations
	return consolidationSteps{
		prepare:   func() ([]uuid.UUID, error) { return c.PrepareConsolidationRun(ctx, in) },
		translate: func(company uuid.UUID) error { return c.TranslateMember(ctx, in, company) },
		eliminate: func() error { return c.EliminateConsolidationRun(ctx, in) },
		complete:  func() error { return c.CompleteConsolidationRun(ctx, in) },
		fail:      func(why problem.Problem) error { return c.FailConsolidationRun(ctx, in, why) },
	}.run()
}

// RunOnce carries out every unfinished run and returns how many it ended.
func (w *Consolidator) RunOnce(ctx context.Context) (int, error) {
	c := w.Consolidations
	runs, err := unfinishedRuns(ctx, c.begin, c.AppRole, c.WorkerRole, ConsolidateKind, c.Runs.UnfinishedConsolidationRuns)
	failures := []error{err}
	n := 0
	for _, r := range runs {
		if err := w.Consolidate(ctx, ConsolidationInput{TenantID: r.tenant, RunID: r.run, OperationID: r.operation}); err != nil {
			failures = append(failures, fmt.Errorf("consolidation runs: run %s: %w", r.run, err))
			continue
		}
		n++
	}
	return n, errors.Join(failures...)
}

// Run carries out runs until ctx ends; errors are logged and retried after
// the interval.
func (w *Consolidator) Run(ctx context.Context) {
	worker.Loop(ctx, w.Interval, time.Second, func(ctx context.Context) (bool, error) {
		n, err := w.RunOnce(ctx)
		if n > 0 && !worker.Failed(ctx, err) {
			w.Log.InfoContext(ctx, "consolidation runs ended", "count", n)
		}
		return false, err
	}, w.Log, "consolidation runs failed")
}
