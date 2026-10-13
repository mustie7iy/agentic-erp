package workflows

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	sdkworkflow "go.temporal.io/sdk/workflow"

	"github.com/mustie7iy/agentic-erp/internal/configuration"
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

// pending is a queued operation (or a proposal cancelled before it started)
// with the business key its workflow id derives from.
type pending struct {
	kind                                string
	tenant, operation, run, legalEntity uuid.UUID
	number                              string
	attempt                             int // of a release: its number among the run's release operations
}

// WorkflowID is the id of a payment run's proposal workflow:
// {tenant}-{legal entity}-payment_run-{document number} (ADR-0012).
func WorkflowID(tenant, legalEntity uuid.UUID, number string) string {
	return fmt.Sprintf("%s-%s-payment_run-%s", tenant, legalEntity, number)
}

// ReleaseWorkflowID is the id of a run's release workflow: the run's key
// plus the attempt, since a failed release is resumed by a new operation.
func ReleaseWorkflowID(tenant, legalEntity uuid.UUID, number string, attempt int) string {
	return fmt.Sprintf("%s-release-%d", WorkflowID(tenant, legalEntity, number), attempt)
}

// SimulateWorkflowID is the id of a change request's simulation workflow:
// {tenant}-change_request-{number}-simulate-{attempt}, since a request may
// be simulated again after a failed or cancelled attempt.
func SimulateWorkflowID(tenant uuid.UUID, number string, attempt int) string {
	return fmt.Sprintf("%s-change_request-%s-simulate-%d", tenant, number, attempt)
}

// MigrateWorkflowID is the id of a migration run's workflow: the run (which
// has no document number) and the operation's kind and attempt.
func MigrateWorkflowID(tenant, run uuid.UUID, kind string, attempt int) string {
	return fmt.Sprintf("%s-migration_run-%s-%s-%d", tenant, run, strings.TrimPrefix(kind, "integration."), attempt)
}

// start is what the dispatcher starts for a pending operation.
func (p pending) start(queue string) workflow.Start {
	switch p.kind {
	case "integration.migrate", "integration.reconcile":
		return workflow.Start{ID: MigrateWorkflowID(p.tenant, p.run, p.kind, p.attempt), Queue: queue, Name: MigrateWorkflow,
			Args: []any{MigrateInput{TenantID: p.tenant, RunID: p.run, OperationID: p.operation}}}
	case payments.ReleaseKind:
		return workflow.Start{ID: ReleaseWorkflowID(p.tenant, p.legalEntity, p.number, p.attempt), Queue: queue, Name: ReleaseWorkflow,
			Args: []any{ReleaseInput{TenantID: p.tenant, RunID: p.run, OperationID: p.operation}}}
	case configuration.SimulateKind:
		return workflow.Start{ID: SimulateWorkflowID(p.tenant, p.number, p.attempt), Queue: queue, Name: SimulateWorkflow,
			Args: []any{SimulateInput{TenantID: p.tenant, OperationID: p.operation}}}
	}
	return workflow.Start{ID: WorkflowID(p.tenant, p.legalEntity, p.number), Queue: queue, Name: ProposeWorkflow,
		Args: []any{ProposeInput{TenantID: p.tenant, RunID: p.run, OperationID: p.operation}}}
}

// RunOnce dispatches every pending proposal and returns how many it started.
func (d *Dispatcher) RunOnce(ctx context.Context) (int, error) {
	tx, err := d.begin(ctx, database.Scope{Role: d.WorkerRole})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	rows, _ := tx.Query(ctx, `SELECT o.kind, o.tenant_id, o.id, r.id, r.legal_entity_id, r.document_number, 0 FROM platform.operation o
			JOIN payments.payment_run r ON r.tenant_id = o.tenant_id AND r.operation_id = o.id
			WHERE o.kind = $1 AND o.status IN ('queued', 'cancelled') AND r.status = 'proposing' AND o.workflow_id IS NULL
		UNION ALL
		SELECT o.kind, o.tenant_id, o.id, r.id, r.legal_entity_id, r.document_number,
			(SELECT count(*) FROM platform.operation x WHERE x.tenant_id = o.tenant_id AND x.kind = o.kind AND x.resource = o.resource AND x.id <= o.id)
			FROM platform.operation o
			JOIN payments.payment_run r ON r.tenant_id = o.tenant_id AND r.release_operation_id = o.id
			WHERE o.kind = $2 AND o.status = 'queued' AND r.status = 'releasing' AND o.workflow_id IS NULL
		UNION ALL
		SELECT o.kind, o.tenant_id, o.id, o.id, '00000000-0000-0000-0000-000000000000'::uuid, cr.number,
			(SELECT count(*) FROM platform.operation x WHERE x.tenant_id = o.tenant_id AND x.kind = o.kind AND x.request->>'change_request_id' = o.request->>'change_request_id' AND x.id <= o.id)
			FROM platform.operation o
			JOIN config.change_request cr ON cr.tenant_id = o.tenant_id AND cr.id = (o.request->>'change_request_id')::uuid
			WHERE o.kind = $3 AND o.status = 'queued' AND o.workflow_id IS NULL
		UNION ALL
		SELECT o.kind, o.tenant_id, o.id, r.id, '00000000-0000-0000-0000-000000000000'::uuid, '',
			(SELECT count(*) FROM platform.operation x WHERE x.tenant_id = o.tenant_id AND x.kind = o.kind AND x.resource = o.resource AND x.id <= o.id)
			FROM platform.operation o
			JOIN integration.migration_run r ON r.tenant_id = o.tenant_id AND r.operation_id = o.id
			WHERE o.kind IN ('integration.migrate', 'integration.reconcile') AND o.status = 'queued' AND r.status = 'running' AND o.workflow_id IS NULL
		ORDER BY 3`, payments.ProposeKind, payments.ReleaseKind, configuration.SimulateKind)
	jobs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (pending, error) {
		var p pending
		err := r.Scan(&p.kind, &p.tenant, &p.operation, &p.run, &p.legalEntity, &p.number, &p.attempt)
		return p, err
	})
	if err != nil {
		return 0, fmt.Errorf("dispatcher: reading queued operations: %w", err)
	}
	n := 0
	var errs []error
	for _, j := range jobs {
		s := j.start(d.Queue)
		id := s.ID
		started, err := d.Client.Start(ctx, s)
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
func Register(r workflow.Registry, p *Payments, s *Simulations, m *Migrations) {
	r.RegisterWorkflowWithOptions(ProposePaymentRun, sdkworkflow.RegisterOptions{Name: ProposeWorkflow})
	r.RegisterWorkflowWithOptions(ReleasePaymentRun, sdkworkflow.RegisterOptions{Name: ReleaseWorkflow})
	r.RegisterWorkflowWithOptions(SimulateChange, sdkworkflow.RegisterOptions{Name: SimulateWorkflow})
	r.RegisterWorkflowWithOptions(MigrateRun, sdkworkflow.RegisterOptions{Name: MigrateWorkflow})
	r.RegisterActivity(p)
	r.RegisterActivity(s)
	r.RegisterActivity(m)
}
