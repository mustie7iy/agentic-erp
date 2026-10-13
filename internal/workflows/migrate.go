package workflows

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Migration runs (T28, ADR-0012): a run's start (extract, check, load) or
// reconciliation is one unit of work, the integration context's, run as the
// activity of a workflow; the request only queues it.

// MigrateInput names the run and the operation a workflow executes.
type MigrateInput struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	RunID       uuid.UUID `json:"run_id"`
	OperationID uuid.UUID `json:"operation_id"`
}

// MigrateWorkflow is the registered name of MigrateRun.
const MigrateWorkflow = "MigrateRun"

// MigrateRun executes the run's operation in one activity, retried on a
// failure of the machinery; problems of the data are the run's report.
func MigrateRun(ctx workflow.Context, in MigrateInput) error {
	ao := workflow.ActivityOptions{StartToCloseTimeout: 2 * time.Hour,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: time.Minute, MaximumAttempts: 5}}
	var a *Migrations
	return workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, ao), a.Execute, in).Get(ctx, nil)
}

// Migrations holds the migration activity: the integration endpoints the
// API uses, and the pool and role they run under.
type Migrations struct {
	Pool      *database.Pool
	AppRole   string
	Endpoints *integration.Endpoints
	Now       func() time.Time
	// Begin starts transactions; nil means Pool.Begin. Tests inject failures.
	Begin database.BeginFunc
}

func (m *Migrations) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	return database.BeginOr(ctx, m.Pool, m.Begin, s)
}

// Execute does the run's work as the operation's requester, in one
// transaction with its report, audit entries and events; an operation no
// longer running is left alone.
func (m *Migrations) Execute(ctx context.Context, in MigrateInput) error {
	tx, err := m.begin(ctx, database.Scope{Role: m.AppRole, TenantID: in.TenantID})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var by uuid.UUID
	var kind string
	if err := tx.QueryRow(ctx, `SELECT o.created_by, p.kind FROM platform.operation o JOIN identity.principal p ON p.tenant_id = o.tenant_id AND p.id = o.created_by
		WHERE o.id = $1`, in.OperationID).Scan(&by, &kind); err != nil {
		return err
	}
	if err := database.SetActor(ctx, tx, by); err != nil {
		return err
	}
	actor := reqctx.Principal{ID: by, Kind: kind, TenantID: in.TenantID}
	rctx, buf := audit.WithBuffer(reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), actor))
	if _, err := m.Endpoints.Execute(rctx, in.RunID); err != nil {
		return err
	}
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	if err := buf.Flush(rctx, tx, actor, "", now()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
