package workflows

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Payment release (T27, ADR-0012): the release of a run is a workflow that
// posts one payment per activity (one ACID transaction, the posting's
// integrity rule), reports progress on the operation, and writes the bank
// file at the end. A business problem (the ledger refusing a posting, the
// bank file invalid, the store unavailable) fails the operation through
// FailRelease; a failure of the machinery is retried by the policy and,
// past it, leaves the run releasing for :release to resume.

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

// ReleasePaymentRun posts the run's payments one activity each and ends
// the release; a problem fails the release instead.
func ReleasePaymentRun(ctx workflow.Context, in ReleaseInput) error {
	ao := workflow.ActivityOptions{StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: time.Minute, MaximumAttempts: 5, NonRetryableErrorTypes: []string{ProblemError}}}
	ctx = workflow.WithActivityOptions(ctx, ao)
	var a *Payments
	var todo []uuid.UUID
	err := workflow.ExecuteActivity(ctx, a.PaymentsToPost, in).Get(ctx, &todo)
	for _, pay := range todo {
		if err != nil {
			break
		}
		err = workflow.ExecuteActivity(ctx, a.PostPayment, in, pay).Get(ctx, nil)
	}
	if err == nil {
		err = workflow.ExecuteActivity(ctx, a.FinishRelease, in).Get(ctx, nil)
	}
	var app *temporal.ApplicationError
	if errors.As(err, &app) && app.Type() == ProblemError {
		var why problem.Problem
		if app.Details(&why) == nil {
			return workflow.ExecuteActivity(ctx, a.FailRelease, in, why).Get(ctx, nil) //nolint:wrapcheck // workflow result: the engine records Future.Get's ActivityError, which carries the activity type and id, as the failure; a wrap would put a generic wrapError failure on top and add no context
		}
	}
	return err //nolint:wrapcheck // workflow result: the engine records Future.Get's ActivityError, which carries the activity type and id, as the failure; a wrap would put a generic wrapError failure on top and add no context
}

// inRelease runs fn in one transaction as the run's releaser, with the
// audit buffer flushed and the transaction committed when fn succeeds.
//
// The endpoints act on the run's current release operation, so an activity
// of a workflow whose operation is no longer the run's (cancelled or failed,
// then taken over by a later :release) does nothing and succeeds: its
// operation is over, the newer workflow does the work, and failing instead
// would only send the stale workflow to FailRelease, which has nothing to
// record either (ADR-0012: an activity finding its work not its own is a
// no-op, not a problem). The run is read FOR UPDATE, so the check holds
// until the transaction ends.
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
	if err := database.SetActor(ctx, tx, actor.ID); err != nil {
		return err
	}
	rctx, buf := audit.WithBuffer(reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), actor))
	if err := fn(rctx, tx, r); err != nil {
		return err
	}
	if err := buf.Flush(rctx, tx, actor, "", clock.Now(p.Now)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("workflows: release payment run %s: commit: %w", in.RunID, err)
	}
	return nil
}

// asProblem turns a business problem into the non-retryable error the
// workflow records; other errors are returned for the retry policy.
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
	var live bool
	if err := tx.QueryRow(ctx, `SELECT status = 'releasing' AND release_operation_id IS NOT DISTINCT FROM $2 FROM payments.payment_run WHERE id = $1`,
		in.RunID, in.OperationID).Scan(&live); err != nil {
		return false, fmt.Errorf("workflows: read the release of run %s: %w", in.RunID, err)
	}
	return live, nil
}

// Releaser is the in-process release worker of a cell without a workflow
// engine: it runs the release steps the workflow would, one transaction
// each, for every queued release operation, until the engine takes over.
type Releaser struct {
	Payments   *Payments
	Pool       *database.Pool
	WorkerRole string // erp_worker: finds release operations across tenants
	Interval   time.Duration
	Log        interface {
		ErrorContext(context.Context, string, ...any)
	}
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
	rows, _ := tx.Query(ctx, `SELECT o.tenant_id, r.id, o.id FROM platform.operation o
		JOIN payments.payment_run r ON r.tenant_id = o.tenant_id AND r.release_operation_id = o.id
		WHERE o.kind = $1 AND o.status IN ('queued', 'running') AND r.status = 'releasing' ORDER BY o.id`, payments.ReleaseKind)
	jobs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ReleaseInput, error) {
		var in ReleaseInput
		err := row.Scan(&in.TenantID, &in.RunID, &in.OperationID)
		return in, errs.Wrapf(err, "scan release operation")
	})
	if err != nil {
		return jobs, fmt.Errorf("read queued release operations: %w", err)
	}
	return jobs, nil
}

// Release does one run's release: the steps of the workflow, in order.
func (w *Releaser) Release(ctx context.Context, in ReleaseInput) error {
	todo, err := w.Payments.PaymentsToPost(ctx, in)
	for _, pay := range todo {
		if err != nil {
			break
		}
		err = w.Payments.PostPayment(ctx, in, pay)
	}
	if err == nil {
		err = w.Payments.FinishRelease(ctx, in)
	}
	var app *temporal.ApplicationError
	if errors.As(err, &app) && app.Type() == ProblemError {
		var why problem.Problem
		if app.Details(&why) == nil {
			return w.Payments.FailRelease(ctx, in, why)
		}
	}
	return err
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
	interval := w.Interval
	if interval <= 0 {
		interval = time.Second
	}
	for {
		if _, err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
			w.Log.ErrorContext(ctx, "payment release failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
