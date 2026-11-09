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
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
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
	err := perform(workflow.WithActivityOptions(ctx, ao), in.TenantID, nil, a.Propose, in)
	if temporal.IsCanceledError(err) || ctx.Err() != nil {
		detached, cancel := workflow.NewDisconnectedContext(ctx)
		defer cancel()
		return perform(workflow.WithActivityOptions(detached, ao), in.TenantID, nil, a.Propose, in)
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
	Begin database.BeginFunc
}

func (p *Payments) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	return database.BeginActive(ctx, p.Pool, p.Begin, s)
}

// Propose builds the run as its proposer, in one transaction with its
// audit entries and events; a run no longer proposing is left alone, so a
// retry after a commit does nothing. A problem while building fails the
// run and its operation (the proposer's rules); any other error is
// returned for the retry policy.
func (p *Payments) Propose(ctx context.Context, in ProposeInput) error {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	var job payments.Job
	proposer := func(ctx context.Context, tx pgx.Tx) (reqctx.Principal, bool, error) {
		if _, err := tx.Exec(ctx, `SELECT set_config('statement_timeout', $1, true)`, fmt.Sprintf("%dms", timeout.Milliseconds())); err != nil {
			return reqctx.Principal{}, false, fmt.Errorf("workflows: propose payment run %s: set the statement timeout: %w", in.RunID, err)
		}
		var ok bool
		var err error
		if job, ok, err = p.Endpoints.JobFor(ctx, tx, in.RunID); err != nil || !ok {
			return reqctx.Principal{}, false, err
		}
		return reqctx.Principal{ID: job.ProposedBy, Kind: job.ProposerKind, TenantID: in.TenantID}, true, nil
	}
	return uow.Job{Begin: p.begin, Scope: database.Scope{Role: p.AppRole, TenantID: in.TenantID}, Now: p.Now,
		Name: fmt.Sprintf("workflows: propose payment run %s", in.RunID)}.As(ctx, proposer, func(ctx context.Context) error {
		return p.Endpoints.Process(ctx, job)
	})
}
