package workflows

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
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

// ApprovalDeadline sleeps the period and escalates the request while it
// stays pending. Its logic is versioned (ADR-0012: workflow versioning is
// mandatory): a change to the loop adds a version, and histories recorded
// under an earlier one replay the earlier path.
func ApprovalDeadline(ctx workflow.Context, in DeadlineInput) error {
	_ = workflow.GetVersion(ctx, "escalation-loop", workflow.DefaultVersion, 1)
	ao := workflow.ActivityOptions{StartToCloseTimeout: time.Minute,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: time.Minute}}
	ctx = workflow.WithActivityOptions(ctx, ao)
	var a *Approvals
	for i := 0; i < in.Max; i++ {
		if err := workflow.Sleep(ctx, in.After); err != nil {
			return err
		}
		var pending bool
		if err := workflow.ExecuteActivity(ctx, a.Escalate, in).Get(ctx, &pending); err != nil {
			return err
		}
		if !pending {
			return nil
		}
	}
	return nil
}

// Approvals holds the escalation activity.
type Approvals struct {
	Pool    *database.Pool
	AppRole string
	Now     func() time.Time
	// Begin starts transactions; nil means Pool.Begin. Tests inject failures.
	Begin func(context.Context, database.Scope) (pgx.Tx, error)
}

func (a *Approvals) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	if a.Begin != nil {
		return a.Begin(ctx, s)
	}
	return a.Pool.Begin(ctx, s)
}

// Escalate escalates the request when it is still pending, in one
// transaction in the tenant's scope, and reports whether it was.
func (a *Approvals) Escalate(ctx context.Context, in DeadlineInput) (bool, error) {
	tx, err := a.begin(ctx, database.Scope{Role: a.AppRole, TenantID: in.TenantID})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	pending, err := approvals.Escalate(ctx, tx, in.TenantID, in.RequestID, now().UTC())
	if err != nil || !pending {
		return false, err
	}
	return true, tx.Commit(ctx)
}
