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

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
)

// Approval deadlines (T29, ADR-0012): every pending approval request is
// watched by a workflow that waits the escalation period and, while no
// decision has arrived, escalates it, up to a limit. Decisions stay
// synchronous human transitions on the API; the workflow only owns the
// clock, and ends at the first check that finds the request decided.

// DeadlineInput names the request a deadline workflow watches and how.
type DeadlineInput struct {
	TenantID  uuid.UUID     `json:"tenant_id"`
	RequestID uuid.UUID     `json:"request_id"`
	After     time.Duration `json:"after"` // the escalation period
	Max       int           `json:"max"`   // escalations before the workflow stops reminding
}

// DeadlineWorkflow is the registered name of ApprovalDeadline.
const DeadlineWorkflow = "ApprovalDeadline"

// Versions of the escalation loop (ADR-0012: workflow versioning is
// mandatory). A change to the loop adds a version, and histories recorded
// under an earlier one replay the earlier path.
const (
	// unguardedEscalation (T29) escalates without telling the activity how
	// many escalations came before; histories without the marker run it too.
	// It must stay unguarded: a workflow that ran it may already have been
	// escalated twice by a retried activity, so its count can exceed the loop
	// index, and the guard would fail it for good (ErrEscalationCount).
	unguardedEscalation workflow.Version = 1
	// guardedEscalation passes the escalations made so far, which guards the
	// activity's update so a retried activity escalates once (ADR-0008).
	guardedEscalation workflow.Version = 2
)

// ApprovalDeadline sleeps the period and escalates the request while it
// stays pending.
func ApprovalDeadline(ctx workflow.Context, in DeadlineInput) error {
	v := workflow.GetVersion(ctx, "escalation-loop", workflow.DefaultVersion, guardedEscalation)
	ao := workflow.ActivityOptions{StartToCloseTimeout: time.Minute,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: time.Minute}}
	ctx = workflow.WithActivityOptions(ctx, ao)
	var a *Approvals
	for i := 0; i < in.Max; i++ {
		if err := workflow.Sleep(ctx, in.After); err != nil {
			return fmt.Errorf("workflows: approval deadline of request %s: %w", in.RequestID, err) // cancellation is matched with errors.As
		}
		input := EscalateInput{DeadlineInput: in}
		if v > unguardedEscalation { // guardedEscalation or later
			prior := int16(i) // escalations so far: one per earlier pass, none reset
			input.Prior = &prior
		}
		var pending bool
		if err := workflow.ExecuteActivity(ctx, a.Escalate, input).Get(ctx, &pending); err != nil {
			return err //nolint:wrapcheck // workflow result: the engine records Future.Get's ActivityError, which carries the activity type and id, as the failure; a wrap would put a generic wrapError failure on top and add no context
		}
		if !pending {
			return nil
		}
	}
	return nil
}

// EscalateInput is the escalation activity's input: the watched request
// and the escalations the workflow made before this one, which guards the
// update so a retried activity escalates once. Prior is nil under the
// unguarded version of the loop and in activities scheduled before it
// existed (the fields of DeadlineInput are inlined, so their payloads still
// decode).
type EscalateInput struct {
	DeadlineInput
	Prior *int16 `json:"prior,omitempty"`
}

// Approvals holds the escalation activity.
type Approvals struct {
	Pool    *database.Pool
	AppRole string
	Now     func() time.Time
	// Begin starts transactions; nil means Pool.Begin. Tests inject failures.
	Begin database.BeginFunc
}

func (a *Approvals) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	return database.BeginOr(ctx, a.Pool, a.Begin, s)
}

// Escalate escalates the request when it is still pending, in one
// transaction in the tenant's scope, and reports whether it still is. A
// count no retry can repair fails the activity for good, so the workflow
// fails visibly instead of retrying every minute.
func (a *Approvals) Escalate(ctx context.Context, in EscalateInput) (bool, error) {
	tx, err := a.begin(ctx, database.Scope{Role: a.AppRole, TenantID: in.TenantID})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	pending, err := approvals.Escalate(ctx, tx, in.TenantID, in.RequestID, in.Prior, clock.Now(a.Now).UTC())
	if errors.Is(err, approvals.ErrEscalationCount) {
		return false, temporal.NewNonRetryableApplicationError(err.Error(), "EscalationCount", err) //nolint:wrapcheck // constructs the engine's error with err as its cause; nothing to wrap
	}
	if err != nil || !pending {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return true, fmt.Errorf("workflows: escalate approval request %s: commit: %w", in.RequestID, err)
	}
	return true, nil
}
