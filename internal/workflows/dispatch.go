package workflows

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	sdkworkflow "go.temporal.io/sdk/workflow"

	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/workflow"
)

// Dispatcher starts a workflow for every queued operation of a kind that
// runs as one (ADR-0012). The operation row, committed by the request that
// created it, is the durable hand-off: the dispatcher reads it as the
// worker role across tenants, starts the workflow under an id derived from
// the business key, and records the id on the operation. A restart
// re-dispatches nothing twice (the engine refuses a known id), and a
// cancellation reaches the workflow by that id.
type Dispatcher struct {
	Pool       *database.Pool
	WorkerRole string // erp_worker: reads queued operations across tenants
	Client     workflow.Client
	Queue      string        // the task queue the cell's workers listen on
	Interval   time.Duration // between rounds; default 1 second
	Log        *slog.Logger
	// Begin starts transactions; nil means Pool.Begin. Tests inject failures.
	Begin func(context.Context, database.Scope) (pgx.Tx, error)
}

func (d *Dispatcher) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	if d.Begin != nil {
		return d.Begin(ctx, s)
	}
	return d.Pool.Begin(ctx, s)
}

// pending is a queued (or cancelled before it started) proposal with the
// business key its workflow id derives from.
type pending struct {
	tenant, operation, run, legalEntity uuid.UUID
	number                              string
}

// WorkflowID is the id of a payment run's proposal workflow:
// {tenant}-{legal entity}-payment_run-{document number} (ADR-0012).
func WorkflowID(tenant, legalEntity uuid.UUID, number string) string {
	return fmt.Sprintf("%s-%s-payment_run-%s", tenant, legalEntity, number)
}

// RunOnce dispatches every pending proposal and returns how many it started.
func (d *Dispatcher) RunOnce(ctx context.Context) (int, error) {
	tx, err := d.begin(ctx, database.Scope{Role: d.WorkerRole})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	rows, _ := tx.Query(ctx, `SELECT o.tenant_id, o.id, r.id, r.legal_entity_id, r.document_number FROM platform.operation o
		JOIN payments.payment_run r ON r.tenant_id = o.tenant_id AND r.operation_id = o.id
		WHERE o.kind = $1 AND o.status IN ('queued', 'cancelled') AND r.status = 'proposing' AND o.workflow_id IS NULL ORDER BY o.id`, payments.ProposeKind)
	jobs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (pending, error) {
		var p pending
		err := r.Scan(&p.tenant, &p.operation, &p.run, &p.legalEntity, &p.number)
		return p, err
	})
	if err != nil {
		return 0, fmt.Errorf("dispatcher: reading queued operations: %w", err)
	}
	n := 0
	var errs []error
	for _, j := range jobs {
		id := WorkflowID(j.tenant, j.legalEntity, j.number)
		started, err := d.Client.Start(ctx, workflow.Start{ID: id, Queue: d.Queue, Name: ProposeWorkflow,
			Args: []any{ProposeInput{TenantID: j.tenant, RunID: j.run, OperationID: j.operation}}})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE platform.operation SET workflow_id = $3, version = version + 1 WHERE tenant_id = $1 AND id = $2`, j.tenant, j.operation, id); err != nil {
			return n, fmt.Errorf("dispatcher: recording the workflow of operation %s: %w", j.operation, err)
		}
		if started {
			n++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return n, err
	}
	return n, errors.Join(errs...)
}

// Run dispatches until ctx ends, logging failures and work done.
func (d *Dispatcher) Run(ctx context.Context) {
	interval := d.Interval
	if interval <= 0 {
		interval = time.Second
	}
	for {
		if n, err := d.RunOnce(ctx); err != nil && ctx.Err() == nil {
			d.Log.ErrorContext(ctx, "workflow dispatch failed", "error", err)
		} else if n > 0 {
			d.Log.InfoContext(ctx, "workflows started", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// Register registers the workflows and activities a cell's worker hosts.
func Register(r workflow.Registry, p *Payments) {
	r.RegisterWorkflowWithOptions(ProposePaymentRun, sdkworkflow.RegisterOptions{Name: ProposeWorkflow})
	r.RegisterActivity(p)
}
