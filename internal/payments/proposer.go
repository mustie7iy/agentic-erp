package payments

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
	"github.com/mustie7iy/agentic-erp/internal/platform/worker"
)

// Proposer builds the proposals that payment-runs:propose queues in a cell
// without a workflow engine (ADR-0012; with Temporal, the proposal
// workflow builds them): a round finds the tenants with jobs, then does
// each job in one transaction of its tenant, as the principal that proposed
// it. A job that fails for a reason other than a problem stays queued and
// is retried; claimed jobs are skipped by other replicas.
type Proposer struct {
	Endpoints  *Endpoints
	Pool       *database.Pool
	AppRole    string        // erp_app: builds the run in the tenant's scope
	WorkerRole string        // erp_worker: finds jobs across tenants
	Timeout    time.Duration // statement timeout while building; default 5 minutes
	Interval   time.Duration // idle wait between rounds; default 1s
	Log        *slog.Logger
	Now        func() time.Time
	// Begin starts transactions; nil means Pool.Begin. Tests replace it to
	// inject database failures.
	Begin database.BeginFunc
}

func (w *Proposer) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	return database.BeginOr(ctx, w.Pool, w.Begin, s)
}

// tenants returns the tenants with jobs.
func (w *Proposer) tenants(ctx context.Context) ([]uuid.UUID, error) {
	tenants, err := database.Tenants(ctx, w.Pool, w.Begin, w.WorkerRole, `SELECT DISTINCT o.tenant_id FROM platform.operation o
		JOIN payments.payment_run r ON r.tenant_id = o.tenant_id AND r.operation_id = o.id
		WHERE o.kind = $1 AND o.status IN ('queued', 'cancelled') AND r.status = 'proposing' AND o.tenant_id IN `+database.ActiveTenants, ProposeKind)
	if err != nil {
		return nil, fmt.Errorf("payments: list the tenants with proposal jobs: %w", err)
	}
	return tenants, nil
}

// one does the tenant's next job and reports whether there was one.
func (w *Proposer) one(ctx context.Context, tenant uuid.UUID) (bool, error) {
	tx, err := w.begin(ctx, database.Scope{Role: w.AppRole, TenantID: tenant})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	// A proposal over 100,000 items outlasts a request's statement timeout.
	if _, err := tx.Exec(ctx, `SELECT set_config('statement_timeout', $1, true)`, fmt.Sprintf("%dms", timeout.Milliseconds())); err != nil {
		return false, fmt.Errorf("payments: set the proposal statement timeout: %w", err)
	}
	job, ok, err := w.Endpoints.NextJob(ctx, tx)
	if err != nil || !ok {
		return false, err
	}
	p := reqctx.Principal{ID: job.ProposedBy, Kind: job.ProposerKind, TenantID: tenant}
	if err := uow.Act(ctx, tx, p, w.Now, func(ctx context.Context) error {
		return w.Endpoints.Process(ctx, job)
	}); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return true, fmt.Errorf("payments: commit the proposal of run %s: %w", job.RunID, err)
	}
	return true, nil
}

// RunOnce does every job there is and returns how many it did. A tenant
// whose job fails is left for the next round; the others go on.
func (w *Proposer) RunOnce(ctx context.Context) (int, error) { return w.queue().RunOnce(ctx) }

func (w *Proposer) queue() worker.Queue {
	return worker.Queue{Name: "payment proposer", Tenants: w.tenants, One: w.one}
}

// Run does jobs until ctx ends; errors are logged and retried after the
// interval.
func (w *Proposer) Run(ctx context.Context) {
	worker.Loop(ctx, w.Interval, time.Second, w.queue().Round, w.Log, "payment proposal failed")
}
