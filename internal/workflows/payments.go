// Package workflows holds the application's Temporal workflows and
// activities (ADR-0012). A workflow orchestrates; every activity is one unit
// of work on the application's services, in the tenant's scope as the
// application role, idempotent so a retry does nothing twice. The API sees
// each workflow as an Operation.
package workflows

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// ProposeInput names the payment run a proposal workflow builds.
type ProposeInput struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	RunID       uuid.UUID `json:"run_id"`
	OperationID uuid.UUID `json:"operation_id"`
}

// ProposeWorkflow is the registered name of ProposePaymentRun.
const ProposeWorkflow = "ProposePaymentRun"

// ProposePaymentRun builds a payment run: one activity that does what the
// proposal worker did, retried on failure. A cancellation (the operation's
// :cancel reaches the workflow) runs the activity once more outside the
// cancelled context: it finds the operation cancelled and fails the run,
// freeing its items, so nothing is left half proposed.
func ProposePaymentRun(ctx workflow.Context, in ProposeInput) error {
	ao := workflow.ActivityOptions{StartToCloseTimeout: 30 * time.Minute, HeartbeatTimeout: 0,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: time.Minute, MaximumAttempts: 5}}
	var a *Payments
	err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, ao), a.Propose, in).Get(ctx, nil)
	if temporal.IsCanceledError(err) || ctx.Err() != nil {
		detached, cancel := workflow.NewDisconnectedContext(ctx)
		defer cancel()
		return workflow.ExecuteActivity(workflow.WithActivityOptions(detached, ao), a.Propose, in).Get(detached, nil)
	}
	return err
}

// Payments holds the payment activities: the services the API uses, and the
// pool and role they run under.
type Payments struct {
	Pool      *database.Pool
	AppRole   string // erp_app: the run is built in the tenant's scope
	Endpoints *payments.Endpoints
	Timeout   time.Duration // statement timeout while building; default 5 minutes
	Now       func() time.Time
	// Begin starts transactions; nil means Pool.Begin. Tests inject failures.
	Begin func(context.Context, database.Scope) (pgx.Tx, error)
}

func (p *Payments) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	if p.Begin != nil {
		return p.Begin(ctx, s)
	}
	return p.Pool.Begin(ctx, s)
}

// Propose builds the run as its proposer, in one transaction with its
// audit entries and events; a run no longer proposing is left alone, so a
// retry after a commit does nothing. A problem while building fails the
// run and its operation (the proposer's rules); any other error is
// returned for the retry policy.
func (p *Payments) Propose(ctx context.Context, in ProposeInput) error {
	tx, err := p.begin(ctx, database.Scope{Role: p.AppRole, TenantID: in.TenantID})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('statement_timeout', $1, true)`, fmt.Sprintf("%dms", timeout.Milliseconds())); err != nil {
		return err
	}
	job, ok, err := payments.JobFor(ctx, tx, in.RunID)
	if err != nil || !ok {
		return err
	}
	if err := database.SetActor(ctx, tx, job.ProposedBy); err != nil {
		return err
	}
	actor := reqctx.Principal{ID: job.ProposedBy, Kind: job.ProposerKind, TenantID: in.TenantID}
	rctx, buf := audit.WithBuffer(reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), actor))
	if err := p.Endpoints.Process(rctx, job); err != nil {
		return err
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	if err := buf.Flush(rctx, tx, actor, "", now()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
