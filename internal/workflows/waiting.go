package workflows

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
)

// A running workflow waits while its tenant is not active (po:2026-11-09,
// ADR-0015 As built). The activities open their tenant's transactions with
// database.BeginActive, which refuses one while the tenant is suspended
// (or provisioning, or offboarded) with a *database.TenantInactiveError:
// an ordinary error, not a non-retryable application error, so the step
// commits nothing and is tried again. A policy with a limit on attempts
// would spend them during a suspension longer than its backoff and fail
// the run, so every step runs through perform: its policy hands that
// failure to the workflow at once (TenantInactive among its non-retryable
// types), which waits in AwaitActive, retried with no limit until the
// tenant is active, and then schedules the step again with its attempts
// whole, so the run carries on where it stopped and never fails because
// of the suspension. Other failures keep their policy's limit.
//
// Replay: the new commands (AwaitActive, the step scheduled again) follow
// only an activity failure of type TenantInactive, which no activity
// returned before this change, so histories recorded before replay alike.

// TenantInactive is the type the engine gives the failure of an activity
// whose tenant was not active: the SDK names the failure of a Go error by
// its type, each error of a wrapped chain in turn
// (go.temporal.io/sdk@v1.49.0 internal/failure_converter.go ErrorToFailure
// and getErrType), so database.TenantInactiveError's name.
const TenantInactive = "TenantInactiveError"

// perform runs the activity with args and decodes its result into out
// (nil: none); when it failed on its tenant being inactive (at once, or
// after its attempts when the activity wrapped the error), it waits in
// AwaitActive until the tenant is active and runs it again. The
// error is Future.Get's, unwrapped: it is the workflow result, and the
// engine records Get's ActivityError, which carries the activity type and
// id, as the failure; a wrap would put a generic wrapError failure on top
// and add no context.
func perform(ctx workflow.Context, tenant uuid.UUID, out any, activity any, args ...any) error {
	// The step's own attempts are not spent on the suspension: its failure
	// goes straight to the wait, so the policy's limit is left whole for
	// other failures once the tenant is active again. The retry policy is
	// not part of the command a replay matches.
	if ao := workflow.GetActivityOptions(ctx); ao.RetryPolicy != nil {
		retry := *ao.RetryPolicy
		retry.NonRetryableErrorTypes = append(slices.Clone(retry.NonRetryableErrorTypes), TenantInactive)
		ctx = workflow.WithRetryPolicy(ctx, retry)
	}
	for {
		err := workflow.ExecuteActivity(ctx, activity, args...).Get(ctx, out)
		if !inactive(err) {
			return err //nolint:wrapcheck // the workflow result, see perform
		}
		// No limit on attempts, at most a minute apart: the run goes on
		// within a minute of its tenant's reactivation. The engine retries
		// an activity without adding to the workflow's history, so a long
		// suspension costs the run a few events, not one per attempt.
		var t *Tenants
		awaiting := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute,
			RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: time.Minute}})
		if err := workflow.ExecuteActivity(awaiting, t.AwaitActive, tenant).Get(ctx, nil); err != nil {
			return err //nolint:wrapcheck // the workflow result, see perform
		}
	}
}

// inactive reports whether err, an activity's failure, is its tenant
// being inactive: an application failure of type TenantInactive anywhere
// in its chain, since the activity may have wrapped the error.
func inactive(err error) bool {
	var app *temporal.ApplicationError
	for errors.As(err, &app) {
		if app.Type() == TenantInactive {
			return true
		}
		err = app.Unwrap()
	}
	return false
}

// Tenants holds the activity a workflow waits in while its tenant is not
// active.
type Tenants struct {
	Pool    *database.Pool
	AppRole string // erp_app: reads the tenant's status in its scope
	// Begin starts transactions; nil means Pool.Begin. Tests inject failures.
	Begin database.BeginFunc
}

// AwaitActive succeeds once the tenant is active and fails, to be retried,
// while it is not. It changes nothing, so it may run any number of times.
func (t *Tenants) AwaitActive(ctx context.Context, tenant uuid.UUID) error {
	tx, err := database.BeginActive(ctx, t.Pool, t.Begin, database.Scope{Role: t.AppRole, TenantID: tenant, ReadOnly: true})
	if err != nil {
		return err
	}
	// Read only and nothing read: the rollback ends a transaction that did nothing.
	_ = tx.Rollback(context.WithoutCancel(ctx))
	return nil
}
