package workflows

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
	"github.com/mustie7iy/agentic-erp/internal/platform/worker"
)

// Payment release (T27, ADR-0012): the release of a run is a workflow that
// posts one payment per activity (one ACID transaction, the posting's
// integrity rule), reports progress on the operation, and writes the bank
// file at the end. A business problem (the ledger refusing a posting, the
// bank file invalid) fails the operation through FailRelease; a failure of
// the machinery, the file store not answering (503) among them, is retried
// by the policy and, past it, leaves the run releasing for :release to
// resume. The bank file is kept under the run's key, so a FinishRelease
// retried after the store took the file finds it there and writes no
// second object.

// ReleaseInput names the run a release workflow completes.
type ReleaseInput struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	RunID       uuid.UUID `json:"run_id"`
	OperationID uuid.UUID `json:"operation_id"`
}

// ReleaseWorkflow is the registered name of ReleasePaymentRun.
const ReleaseWorkflow = "ReleasePaymentRun"

// ProblemError is the application error type an activity returns for a
// business problem: it is not retried, and the workflow records it on the
// operation.
const ProblemError = "problem"

// releaseSteps are the steps of a run's release. The workflow runs each as
// an activity and the Releaser calls them directly, so both share one
// control flow (run) and differ only in how a step is carried out.
type releaseSteps struct {
	toPost func() ([]uuid.UUID, error)
	post   func(pay uuid.UUID) error
	finish func() error
	fail   func(why problem.Problem) error
}

// run posts the run's payments in order and ends the release; a problem
// fails the release instead. In a workflow it is deterministic: it only
// sequences the steps, which issue the same activities in the same order
// on every replay.
func (s releaseSteps) run() error {
	todo, err := s.toPost()
	for _, pay := range todo {
		if err != nil {
			break
		}
		err = s.post(pay)
	}
	if err == nil {
		err = s.finish()
	}
	if why, ok := problemOf(err); ok {
		return s.fail(why)
	}
	return err
}

// problemOf is the business problem an activity failed with (asProblem),
// ok false for any other error.
func problemOf(err error) (problem.Problem, bool) {
	var app *temporal.ApplicationError
	var why problem.Problem
	if errors.As(err, &app) && app.Type() == ProblemError && app.Details(&why) == nil {
		return why, true
	}
	return problem.Problem{}, false
}

// recording is ctx for the step that records how a run ended, its Fail
// activity: ctx's retry policy with no limit on attempts. A failure the
// step does not record leaves the run queued, running or releasing for
// good, and for a consolidation or depreciation run blocks every later run
// of its group or company (one queued or running at a time), so a failure
// of the machinery is tried until it clears, with ctx's backoff up to its
// maximum interval (ADR-0012 As built). The step is idempotent, guarded by
// the run's expected status, so the retries record it once; a business
// problem the step returns as ProblemError (asProblem) is not retried.
// The retry policy is not part of the command a replay matches, so
// histories recorded before replay alike. ctx must carry activity options
// with a retry policy, as every workflow here sets.
func recording(ctx workflow.Context) workflow.Context {
	retry := *workflow.GetActivityOptions(ctx).RetryPolicy
	retry.MaximumAttempts = 0
	return workflow.WithRetryPolicy(ctx, retry)
}

// ReleasePaymentRun posts the run's payments one activity each and ends
// the release; a problem fails the release instead, its failure tried
// until it is recorded (recording).
//
// The steps return Future.Get's error unwrapped: it is the workflow result,
// and the engine records Get's ActivityError, which carries the activity
// type and id, as the failure; a wrap would put a generic wrapError failure
// on top and add no context.
func ReleasePaymentRun(ctx workflow.Context, in ReleaseInput) error {
	ao := workflow.ActivityOptions{StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: time.Minute, MaximumAttempts: 5, NonRetryableErrorTypes: []string{ProblemError}}}
	ctx = workflow.WithActivityOptions(ctx, ao)
	var a *Payments
	return releaseSteps{
		toPost: func() ([]uuid.UUID, error) {
			var todo []uuid.UUID
			err := workflow.ExecuteActivity(ctx, a.PaymentsToPost, in).Get(ctx, &todo)
			return todo, err //nolint:wrapcheck // the workflow result, see ReleasePaymentRun
		},
		post: func(pay uuid.UUID) error {
			return workflow.ExecuteActivity(ctx, a.PostPayment, in, pay).Get(ctx, nil)
		},
		finish: func() error {
			return workflow.ExecuteActivity(ctx, a.FinishRelease, in).Get(ctx, nil)
		},
		fail: func(why problem.Problem) error {
			return workflow.ExecuteActivity(recording(ctx), a.FailRelease, in, why).Get(ctx, nil)
		},
	}.run()
}

// inRelease runs fn in one transaction as the run's releaser, with the
// audit buffer flushed and the transaction committed when fn succeeds.
//
// The endpoints act on the run's current release operation, so an activity
// of a workflow whose operation is no longer the run's (cancelled or failed,
// then taken over by a later :release) does nothing and succeeds: its
// operation is over and the newer workflow does the work. This follows
// ADR-0012's rule that an activity checks whether its effect is already
// there before acting: here the effect belongs to another operation. A
// problem instead would only send the stale workflow to FailRelease, which
// has nothing to record either, and fail a workflow nobody has to resolve.
// The run is read FOR UPDATE, so the check holds until the transaction ends.
func (p *Payments) inRelease(ctx context.Context, in ReleaseInput, fn func(ctx context.Context, tx pgx.Tx, r payments.Run) error) error {
	tx, err := p.begin(ctx, database.Scope{Role: p.AppRole, TenantID: in.TenantID})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	actor, r, err := p.Endpoints.Releaser(ctx, tx, in.RunID)
	if err != nil {
		return err
	}
	if r.ReleaseOpID == nil || *r.ReleaseOpID != in.OperationID {
		return nil
	}
	if err := uow.Act(ctx, tx, actor, p.Now, func(ctx context.Context) error {
		return fn(ctx, tx, r)
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("workflows: release payment run %s: commit: %w", in.RunID, err)
	}
	return nil
}

// asProblem turns a business problem (below 500) into the non-retryable
// error the workflow records; other errors, unavailability (503) included,
// are returned for the retry policy.
func asProblem(err error) error {
	var why *problem.Problem
	if errors.As(err, &why) && why.Status < 500 {
		return temporal.NewNonRetryableApplicationError(why.Detail, ProblemError, err, *why) //nolint:wrapcheck // constructs the engine's non-retryable ApplicationError with err as its cause; nothing to wrap
	}
	return err
}

// PaymentsToPost lists the run's payments still to post.
func (p *Payments) PaymentsToPost(ctx context.Context, in ReleaseInput) ([]uuid.UUID, error) {
	var todo []uuid.UUID
	err := p.inRelease(ctx, in, func(ctx context.Context, tx pgx.Tx, r payments.Run) error {
		var err error
		todo, _, err = payments.PaymentsToPost(ctx, tx, r.ID)
		return err
	})
	return todo, err
}

// PostPayment posts one payment; a payment already posted is left alone.
func (p *Payments) PostPayment(ctx context.Context, in ReleaseInput, pay uuid.UUID) error {
	return asProblem(p.inRelease(ctx, in, func(ctx context.Context, _ pgx.Tx, r payments.Run) error {
		_, err := p.Endpoints.PostPayment(ctx, r, pay)
		return err
	}))
}

// FinishRelease writes the bank file and releases the run.
func (p *Payments) FinishRelease(ctx context.Context, in ReleaseInput) error {
	return asProblem(p.inRelease(ctx, in, func(ctx context.Context, _ pgx.Tx, r payments.Run) error {
		_, err := p.Endpoints.FinishRelease(ctx, r)
		return err
	}))
}

// FailRelease records the problem that stopped the release. A run no
// longer releasing under the workflow's operation has nothing to record:
// the failure was recorded before and its completion lost (a run with
// nothing posted went back to proposed, so it has no releaser to act as),
// or a later :release took the run over.
func (p *Payments) FailRelease(ctx context.Context, in ReleaseInput, why problem.Problem) error {
	if live, err := p.releasing(ctx, in); err != nil || !live {
		return err
	}
	return p.inRelease(ctx, in, func(ctx context.Context, _ pgx.Tx, r payments.Run) error {
		_, err := p.Endpoints.FailRelease(ctx, r, &why)
		return err
	})
}

// releasing reads whether the run is releasing under the workflow's
// operation.
func (p *Payments) releasing(ctx context.Context, in ReleaseInput) (bool, error) {
	tx, err := p.begin(ctx, database.Scope{Role: p.AppRole, TenantID: in.TenantID, ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	live, err := payments.ReleasingUnder(ctx, tx, in.RunID, in.OperationID)
	if err != nil {
		return false, fmt.Errorf("workflows: %w", err)
	}
	return live, nil
}

// Releaser is the in-process release worker of a cell without a workflow
// engine: it runs the release steps the workflow would, one transaction
// each, for every queued release operation, until the engine takes over.
//
// Cancelling the release operation does not stop paying. A release under
// way keeps posting its payments (each posting stands), the operation stays
// cancelled, the bank file is not written and the run stays releasing; the
// run is resumed with :release, which starts a new operation. The releaser
// picks up queued and running operations only, so a cancelled one is not
// taken up again, and any step it still runs after the resume does nothing.
type Releaser struct {
	Payments   *Payments
	Pool       *database.Pool
	WorkerRole string // erp_worker: finds release operations across tenants
	Interval   time.Duration
	Log        *slog.Logger
	// Begin starts transactions; nil means Pool.Begin. Tests inject failures.
	Begin database.BeginFunc
}

func (w *Releaser) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	return database.BeginOr(ctx, w.Pool, w.Begin, s)
}

// pending lists the queued release operations across tenants.
func (w *Releaser) pending(ctx context.Context) ([]ReleaseInput, error) {
	tx, err := w.begin(ctx, database.Scope{Role: w.WorkerRole, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	releases, err := payments.Releases(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("read queued release operations: %w", err)
	}
	jobs := make([]ReleaseInput, len(releases))
	for i, r := range releases {
		jobs[i] = ReleaseInput{TenantID: r.TenantID, RunID: r.Resource, OperationID: r.ID}
	}
	return jobs, nil
}

// Release does one run's release: the steps of the workflow, in order,
// each called directly in its own transaction.
func (w *Releaser) Release(ctx context.Context, in ReleaseInput) error {
	return releaseSteps{
		toPost: func() ([]uuid.UUID, error) { return w.Payments.PaymentsToPost(ctx, in) },
		post:   func(pay uuid.UUID) error { return w.Payments.PostPayment(ctx, in, pay) },
		finish: func() error { return w.Payments.FinishRelease(ctx, in) },
		fail:   func(why problem.Problem) error { return w.Payments.FailRelease(ctx, in, why) },
	}.run()
}

// RunOnce releases every queued run and returns how many it completed.
func (w *Releaser) RunOnce(ctx context.Context) (int, error) {
	jobs, err := w.pending(ctx)
	if err != nil {
		return 0, fmt.Errorf("payment releaser: %w", err)
	}
	n := 0
	var failures []error
	for _, in := range jobs {
		if err := w.Release(ctx, in); err != nil {
			failures = append(failures, fmt.Errorf("payment releaser: run %s: %w", in.RunID, err))
			continue
		}
		n++
	}
	return n, errors.Join(failures...)
}

// Run releases until ctx ends; errors are logged and retried after the interval.
func (w *Releaser) Run(ctx context.Context) {
	worker.Loop(ctx, w.Interval, time.Second, func(ctx context.Context) (bool, error) {
		_, err := w.RunOnce(ctx)
		return false, err
	}, w.Log, "payment release failed")
}
