package workflows

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	sdkworkflow "go.temporal.io/sdk/workflow"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/worker"
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
	// Deadline is the escalation policy of approval requests: the period
	// without a decision, and how often to escalate (ERP_APPROVAL_ESCALATE_AFTER,
	// ERP_APPROVAL_ESCALATIONS); a zero period watches no requests.
	Deadline    time.Duration
	Escalations int
	Pool        *database.Pool
	WorkerRole  string // erp_worker: reads queued operations across tenants
	Client      workflow.Client
	Queue       string        // the task queue the cell's workers listen on
	Interval    time.Duration // between rounds; default 1 second
	Log         *slog.Logger
	// Begin starts transactions; nil means Pool.Begin. Tests inject failures.
	Begin database.BeginFunc
}

func (d *Dispatcher) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	return database.BeginOr(ctx, d.Pool, d.Begin, s)
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

// start is what the dispatcher starts for a waiting operation.
func start(w operations.Waiting, queue string) workflow.Start {
	switch w.Kind {
	case "integration.migrate", "integration.reconcile":
		return workflow.Start{ID: MigrateWorkflowID(w.TenantID, w.Resource, w.Kind, w.Attempt), Queue: queue, Name: MigrateWorkflow,
			Args: []any{MigrateInput{TenantID: w.TenantID, RunID: w.Resource, OperationID: w.ID}}}
	case payments.ReleaseKind:
		return workflow.Start{ID: ReleaseWorkflowID(w.TenantID, w.LegalEntity, w.Number, w.Attempt), Queue: queue, Name: ReleaseWorkflow,
			Args: []any{ReleaseInput{TenantID: w.TenantID, RunID: w.Resource, OperationID: w.ID}}}
	case configuration.SimulateKind:
		return workflow.Start{ID: SimulateWorkflowID(w.TenantID, w.Number, w.Attempt), Queue: queue, Name: SimulateWorkflow,
			Args: []any{SimulateInput{TenantID: w.TenantID, OperationID: w.ID}}}
	}
	return workflow.Start{ID: WorkflowID(w.TenantID, w.LegalEntity, w.Number), Queue: queue, Name: ProposeWorkflow,
		Args: []any{ProposeInput{TenantID: w.TenantID, RunID: w.Resource, OperationID: w.ID}}}
}

// RunOnce dispatches every pending proposal and returns how many it started.
func (d *Dispatcher) RunOnce(ctx context.Context) (int, error) {
	tx, err := d.begin(ctx, database.Scope{Role: d.WorkerRole})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	jobs, err := queued(ctx, tx)
	if err != nil {
		return 0, err
	}
	n := 0
	var failures []error
	for _, j := range jobs {
		s := start(j, d.Queue)
		id := s.ID
		started, err := d.Client.Start(ctx, s)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE platform.operation SET workflow_id = $3, version = version + 1 WHERE tenant_id = $1 AND id = $2`, j.TenantID, j.ID, id); err != nil {
			return n, fmt.Errorf("dispatcher: recording the workflow of operation %s: %w", j.ID, err)
		}
		if started {
			n++
		}
	}
	if d.Deadline > 0 {
		k, err := d.watch(ctx, tx)
		n += k
		if err != nil {
			failures = append(failures, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return n, fmt.Errorf("dispatcher: committing the started workflows: %w", err)
	}
	return n, errors.Join(failures...)
}

// queued reads the operations waiting for a workflow: payment proposals
// and releases, configuration simulations, migrations and reconciliations,
// each listed by the context that owns its resource, in the order they
// were queued (operation ids are UUIDv7, and PostgreSQL orders uuids by
// their bytes, as bytes.Compare does).
func queued(ctx context.Context, tx pgx.Tx) ([]operations.Waiting, error) {
	var jobs []operations.Waiting
	for _, list := range []func(context.Context, pgx.Tx) ([]operations.Waiting, error){payments.Queued, configuration.QueuedSimulations, integration.QueuedRuns} {
		more, err := list(ctx, tx)
		if err != nil {
			return nil, fmt.Errorf("dispatcher: reading queued operations: %w", err)
		}
		jobs = append(jobs, more...)
	}
	slices.SortFunc(jobs, func(a, b operations.Waiting) int { return bytes.Compare(a.ID[:], b.ID[:]) })
	return jobs, nil
}

// DeadlineWorkflowID is the id of an approval request's deadline workflow.
func DeadlineWorkflowID(tenant, request uuid.UUID) string {
	return fmt.Sprintf("%s-approval_request-%s-deadline", tenant, request)
}

// watch starts a deadline workflow for every pending approval request that
// has none and records it on the request.
func (d *Dispatcher) watch(ctx context.Context, tx pgx.Tx) (int, error) {
	pending, err := approvals.Unwatched(ctx, tx)
	if err != nil {
		return 0, fmt.Errorf("dispatcher: %w", err)
	}
	n := 0
	var failures []error
	for _, q := range pending {
		id := DeadlineWorkflowID(q.TenantID, q.ID)
		started, err := d.Client.Start(ctx, workflow.Start{ID: id, Queue: d.Queue, Name: DeadlineWorkflow,
			Args: []any{DeadlineInput{TenantID: q.TenantID, RequestID: q.ID, After: d.Deadline, Max: d.Escalations}}})
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if err := approvals.Watched(ctx, tx, q.TenantID, q.ID, id); err != nil {
			return n, fmt.Errorf("dispatcher: %w", err)
		}
		if started {
			n++
		}
	}
	return n, errors.Join(failures...)
}

// Run dispatches until ctx ends, logging failures and work done.
func (d *Dispatcher) Run(ctx context.Context) {
	worker.Loop(ctx, d.Interval, time.Second, func(ctx context.Context) (bool, error) {
		n, err := d.RunOnce(ctx)
		if n > 0 && !worker.Failed(ctx, err) {
			d.Log.InfoContext(ctx, "workflows started", "count", n)
		}
		return false, err
	}, d.Log, "workflow dispatch failed")
}

// Register registers the workflows and activities a cell's worker hosts.
func Register(r workflow.Registry, p *Payments, s *Simulations, m *Migrations, a *Approvals) {
	r.RegisterWorkflowWithOptions(ProposePaymentRun, sdkworkflow.RegisterOptions{Name: ProposeWorkflow})
	r.RegisterWorkflowWithOptions(ReleasePaymentRun, sdkworkflow.RegisterOptions{Name: ReleaseWorkflow})
	r.RegisterWorkflowWithOptions(SimulateChange, sdkworkflow.RegisterOptions{Name: SimulateWorkflow})
	r.RegisterWorkflowWithOptions(MigrateRun, sdkworkflow.RegisterOptions{Name: MigrateWorkflow})
	r.RegisterActivity(p)
	r.RegisterActivity(s)
	r.RegisterWorkflowWithOptions(ApprovalDeadline, sdkworkflow.RegisterOptions{Name: DeadlineWorkflow})
	r.RegisterActivity(m)
	r.RegisterActivity(a)
}
