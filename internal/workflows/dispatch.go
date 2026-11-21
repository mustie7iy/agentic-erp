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
	"go.temporal.io/sdk/activity"
	sdkworkflow "go.temporal.io/sdk/workflow"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
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
	// Statements is the period of each house bank's statement fetch
	// (ERP_STATEMENT_FETCH_INTERVAL, ADR-0047); 0 fetches none.
	Statements time.Duration
	// EInvoices is the period of each einvoice_provider connector's fetch
	// (ERP_EINVOICE_FETCH_INTERVAL, ADR-0061 §4); 0 fetches none.
	EInvoices time.Duration
	// Recurring finds the entry templates' due dates (ADR-0056); nil
	// parks none.
	Recurring *Recurrences
	// Snapshots finds the companies' due cash snapshots (ADR-0051 §6), at
	// most once a minute; nil takes none.
	Snapshots *Snapshots
	Now       func() time.Time // the fetch periods' and due dates' clock; nil: time.Now
	Log       *slog.Logger
	// Begin starts transactions; nil means Pool.Begin. Tests inject failures.
	Begin database.BeginFunc

	fetched          time.Time // the last statement fetch period started without a failure
	einvoicesFetched time.Time // the last e-invoice fetch period started without a failure
	snapshotted      time.Time // the last minute whose due snapshots were started without a failure
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
	case DepreciateKind:
		return workflow.Start{ID: DepreciationWorkflowID(w.TenantID, w.Resource), Queue: queue, Name: DepreciationWorkflow,
			Args: []any{DepreciationInput{TenantID: w.TenantID, RunID: w.Resource, OperationID: w.ID}}}
	case ConsolidateKind:
		return workflow.Start{ID: ConsolidationWorkflowID(w.TenantID, w.Resource), Queue: queue, Name: ConsolidationWorkflow,
			Args: []any{ConsolidationInput{TenantID: w.TenantID, RunID: w.Resource, OperationID: w.ID}}}
	case MrpKind:
		return workflow.Start{ID: MrpWorkflowID(w.TenantID, w.Resource), Queue: queue, Name: MrpWorkflow,
			Args: []any{MrpInput{TenantID: w.TenantID, RunID: w.Resource, OperationID: w.ID}}}
	case EInvoiceFetchKind:
		return workflow.Start{ID: EInvoiceFetchWorkflowID(w.TenantID, w.ID), Queue: queue, Name: EInvoiceFetchWorkflow,
			Args: []any{EInvoiceFetchInput{TenantID: w.TenantID, ConnectorID: w.Resource, OperationID: w.ID}}}
	case EInvoiceSendKind, EInvoiceTaxDataKind:
		return workflow.Start{ID: EInvoiceSendWorkflowID(w.TenantID, w.ID), Queue: queue, Name: EInvoiceSendWorkflow,
			Args: []any{EInvoiceSendInput{TenantID: w.TenantID, OutgoingID: w.Resource, OperationID: w.ID}}}
	case configuration.SimulateKind:
		return workflow.Start{ID: SimulateWorkflowID(w.TenantID, w.Number, w.Attempt), Queue: queue, Name: SimulateWorkflow,
			Args: []any{SimulateInput{TenantID: w.TenantID, OperationID: w.ID}}}
	}
	return workflow.Start{ID: WorkflowID(w.TenantID, w.LegalEntity, w.Number), Queue: queue, Name: ProposeWorkflow,
		Args: []any{ProposeInput{TenantID: w.TenantID, RunID: w.Resource, OperationID: w.ID}}}
}

// RunOnce dispatches every pending proposal and returns how many it started.
//
// A round reads what to start in one read-only transaction and closes it
// before calling the engine, and records each started workflow in a
// transaction of its own: no row lock (an operation's, an approval
// request's) is held across a call to the engine, so a slow engine never
// keeps a person deciding a request waiting on the dispatcher. Recording
// apart from the start is safe because the workflow id is derived from
// the business key: a round that starts and fails to record is followed by
// one that starts the same id, which the engine answers as known.
func (d *Dispatcher) RunOnce(ctx context.Context) (int, error) {
	r, err := d.read(ctx)
	if err != nil {
		return 0, err
	}
	n, failures, err := d.startRecorded(ctx, r.recorded)
	if err != nil {
		return n, err
	}
	for _, p := range []struct {
		due  periodic
		done *time.Time
	}{{r.statements, &d.fetched}, {r.einvoices, &d.einvoicesFetched}} {
		k, err := d.startPeriodic(ctx, p.due, p.done)
		n += k
		if err != nil {
			failures = append(failures, err)
		}
	}
	for _, step := range []func(context.Context) (int, error){d.recur, d.snapshot} {
		k, err := step(ctx)
		n += k
		if err != nil {
			failures = append(failures, err)
		}
	}
	return n, errors.Join(failures...)
}

// recorded is a workflow to start and how to record it once started.
type recorded struct {
	start  workflow.Start
	record func(context.Context, pgx.Tx) error
}

// periodic are a period's scheduled starts; due is false when the period
// was started already by this dispatcher, or none is configured.
type periodic struct {
	at     time.Time
	due    bool
	starts []workflow.Start
}

// roundReads is what a round starts, as read before the engine is called.
type roundReads struct {
	recorded   []recorded // queued operations, then unwatched approval requests
	statements periodic
	einvoices  periodic
}

// read reads a round's work in one read-only transaction, closed before
// any workflow starts.
func (d *Dispatcher) read(ctx context.Context) (roundReads, error) {
	tx, err := d.begin(ctx, database.Scope{Role: d.WorkerRole, ReadOnly: true})
	if err != nil {
		return roundReads{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var r roundReads
	jobs, err := queued(ctx, tx)
	if err != nil {
		return roundReads{}, err
	}
	for _, j := range jobs {
		s := start(j, d.Queue)
		r.recorded = append(r.recorded, recorded{start: s, record: recordOperation(j, s.ID)})
	}
	watches, err := d.watches(ctx, tx)
	if err != nil {
		return roundReads{}, err
	}
	r.recorded = append(r.recorded, watches...)
	if r.statements, err = d.fetches(ctx, tx); err != nil {
		return roundReads{}, err
	}
	if r.einvoices, err = d.einvoiceFetches(ctx, tx); err != nil {
		return roundReads{}, err
	}
	return r, nil
}

// recordOperation records the workflow started for a queued operation.
func recordOperation(j operations.Waiting, workflowID string) func(context.Context, pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE platform.operation SET workflow_id = $3, version = version + 1 WHERE tenant_id = $1 AND id = $2`,
			j.TenantID, j.ID, workflowID); err != nil {
			return fmt.Errorf("dispatcher: recording the workflow of operation %s: %w", j.ID, err)
		}
		return nil
	}
}

// startRecorded starts each workflow and records it in a transaction of
// its own. A refused start does not stop the others and is returned among
// failures; a failed record stops the round (err).
func (d *Dispatcher) startRecorded(ctx context.Context, work []recorded) (n int, failures []error, err error) {
	for _, w := range work {
		started, err := d.Client.Start(ctx, w.start)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if err := d.inTx(ctx, w.record); err != nil {
			return n, failures, err
		}
		if started {
			n++
		}
	}
	return n, failures, nil
}

// inTx runs fn in a transaction of the worker role and commits it.
func (d *Dispatcher) inTx(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	tx, err := d.begin(ctx, database.Scope{Role: d.WorkerRole})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("dispatcher: committing a started workflow: %w", err)
	}
	return nil
}

// startPeriodic starts a due period's workflows and, when none failed,
// marks the period done for this dispatcher.
func (d *Dispatcher) startPeriodic(ctx context.Context, p periodic, done *time.Time) (int, error) {
	if !p.due {
		return 0, nil
	}
	n, err := d.startAll(ctx, p.starts)
	if err == nil {
		*done = p.at
	}
	return n, err
}

// queued reads the operations waiting for a workflow: payment proposals
// and releases, configuration simulations, migrations and reconciliations,
// each listed by the context that owns its resource, and depreciation,
// consolidation and planning runs and e-invoice fetches, sends and tax
// data requests, whose leaf contexts' tables the dispatcher may not read,
// from their operations alone (ADR-0057 §7, ADR-0059 §6, ADR-0061 §4 and
// As built E3, E4, ADR-0084 §3), in the order they were queued (operation ids are UUIDv7, and PostgreSQL orders uuids by
// their bytes, as bytes.Compare does).
func queued(ctx context.Context, tx pgx.Tx) ([]operations.Waiting, error) {
	var jobs []operations.Waiting
	depreciations := func(ctx context.Context, tx pgx.Tx) ([]operations.Waiting, error) {
		return operations.QueuedOf(ctx, tx, DepreciateKind)
	}
	consolidations := func(ctx context.Context, tx pgx.Tx) ([]operations.Waiting, error) {
		return operations.QueuedOf(ctx, tx, ConsolidateKind)
	}
	fetches := func(ctx context.Context, tx pgx.Tx) ([]operations.Waiting, error) {
		return operations.QueuedOf(ctx, tx, EInvoiceFetchKind)
	}
	sends := func(ctx context.Context, tx pgx.Tx) ([]operations.Waiting, error) {
		return operations.QueuedOf(ctx, tx, EInvoiceSendKind)
	}
	requests := func(ctx context.Context, tx pgx.Tx) ([]operations.Waiting, error) {
		return operations.QueuedOf(ctx, tx, EInvoiceTaxDataKind)
	}
	plans := func(ctx context.Context, tx pgx.Tx) ([]operations.Waiting, error) {
		return operations.QueuedOf(ctx, tx, MrpKind)
	}
	for _, list := range []func(context.Context, pgx.Tx) ([]operations.Waiting, error){payments.Queued, configuration.QueuedSimulations, integration.QueuedRuns,
		depreciations, consolidations, fetches, sends, requests, plans} {
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

// watches lists a deadline workflow for every pending approval request
// that has none, recorded on the request once started; with no escalation
// period it watches none.
func (d *Dispatcher) watches(ctx context.Context, tx pgx.Tx) ([]recorded, error) {
	if d.Deadline <= 0 {
		return nil, nil
	}
	pending, err := approvals.Unwatched(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("dispatcher: %w", err)
	}
	watches := make([]recorded, len(pending))
	for i, q := range pending {
		id := DeadlineWorkflowID(q.TenantID, q.ID)
		watches[i] = recorded{
			start: workflow.Start{ID: id, Queue: d.Queue, Name: DeadlineWorkflow,
				Args: []any{DeadlineInput{TenantID: q.TenantID, RequestID: q.ID, After: d.Deadline, Max: d.Escalations}}},
			record: func(ctx context.Context, tx pgx.Tx) error {
				if err := approvals.Watched(ctx, tx, q.TenantID, q.ID, id); err != nil {
					return fmt.Errorf("dispatcher: %w", err)
				}
				return nil
			},
		}
	}
	return watches, nil
}

// fetches lists, once per period, a statement fetch for every active
// house bank whose bank channel names statement directories (ADR-0047,
// T49d). The workflow id names the period, so the engine starts each once
// however many replicas dispatch; a period started with no failure is not
// looked at again by this dispatcher. With no period it fetches none.
func (d *Dispatcher) fetches(ctx context.Context, tx pgx.Tx) (periodic, error) {
	if d.Statements <= 0 {
		return periodic{}, nil
	}
	slot := clock.Now(d.Now).UTC().Truncate(d.Statements)
	if slot.Equal(d.fetched) {
		return periodic{}, nil
	}
	channels, err := integration.StatementChannels(ctx, tx)
	if err != nil {
		return periodic{}, fmt.Errorf("dispatcher: %w", err)
	}
	banks, err := payments.ChannelBanks(ctx, tx)
	if err != nil {
		return periodic{}, fmt.Errorf("dispatcher: %w", err)
	}
	p := periodic{at: slot, due: true}
	for _, b := range banks {
		if channels[integration.TenantConnector{TenantID: b.TenantID, ID: b.ConnectorID}] {
			p.starts = append(p.starts, workflow.Start{ID: FetchWorkflowID(b.TenantID, b.HouseBankID, slot), Queue: d.Queue, Name: FetchWorkflow,
				Args: []any{FetchInput{TenantID: b.TenantID, HouseBankID: b.HouseBankID}}})
		}
	}
	return p, nil
}

// einvoiceFetches lists, once per period, an e-invoice fetch from every
// active einvoice_provider connector of an active tenant (ADR-0061 §4).
// As fetches', the workflow id names the connector and the period, so the
// engine starts each once however many replicas dispatch, and a period
// started with no failure is not looked at again. With no period it
// fetches none.
func (d *Dispatcher) einvoiceFetches(ctx context.Context, tx pgx.Tx) (periodic, error) {
	if d.EInvoices <= 0 {
		return periodic{}, nil
	}
	period := clock.Now(d.Now).UTC().Truncate(d.EInvoices)
	if period.Equal(d.einvoicesFetched) {
		return periodic{}, nil
	}
	connectors, err := integration.EInvoiceConnectors(ctx, tx)
	if err != nil {
		return periodic{}, fmt.Errorf("dispatcher: %w", err)
	}
	p := periodic{at: period, due: true, starts: make([]workflow.Start, len(connectors))}
	for i, c := range connectors {
		p.starts[i] = workflow.Start{ID: ScheduledEInvoiceFetchWorkflowID(c.TenantID, c.ID, period), Queue: d.Queue, Name: ScheduledEInvoiceFetchWorkflow,
			Args: []any{ScheduledEInvoiceFetchInput(c.TenantID, c.ID, period)}}
	}
	return p, nil
}

// startAll starts each workflow and says how many the engine started (one
// it knew already is not); a refused start does not stop the others.
func (d *Dispatcher) startAll(ctx context.Context, starts []workflow.Start) (int, error) {
	n := 0
	var failures []error
	for _, s := range starts {
		started, err := d.Client.Start(ctx, s)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if started {
			n++
		}
	}
	return n, errors.Join(failures...)
}

// recur starts a workflow for each entry template's next due date
// (ADR-0056). The workflow id names the template, the date and the
// template's version, so a date is started once however many replicas and
// rounds dispatch it; the template moves on only once the date's entry is
// parked or refused, so a round lists it until then.
func (d *Dispatcher) recur(ctx context.Context) (int, error) {
	if d.Recurring == nil {
		return 0, nil
	}
	due, err := d.Recurring.Due(ctx, clock.Now(d.Now))
	failures := []error{err}
	n := 0
	for _, in := range due {
		started, err := d.Client.Start(ctx, workflow.Start{ID: RecurWorkflowID(in), Queue: d.Queue, Name: RecurWorkflow, Args: []any{in}})
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if started {
			n++
		}
	}
	if err := errors.Join(failures...); err != nil {
		return n, fmt.Errorf("dispatcher: %w", err)
	}
	return n, nil
}

// snapshot starts, once a minute, a TakeCashSnapshot workflow for every
// company whose daily snapshot is due (ADR-0051 §6). The workflow id names
// the company and the date, so the engine starts each once however many
// replicas and rounds dispatch it; a minute started with no failure is not
// looked at again by this dispatcher.
func (d *Dispatcher) snapshot(ctx context.Context) (int, error) {
	now := clock.Now(d.Now)
	slot := now.UTC().Truncate(snapshotEvery)
	if d.Snapshots == nil || slot.Equal(d.snapshotted) {
		return 0, nil
	}
	due, err := d.Snapshots.Due(ctx, now)
	failures := []error{err}
	n := 0
	for _, in := range due {
		started, err := d.Client.Start(ctx, workflow.Start{ID: SnapshotWorkflowID(in), Queue: d.Queue, Name: SnapshotWorkflow, Args: []any{in}})
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if started {
			n++
		}
	}
	if err := errors.Join(failures...); err != nil {
		return n, fmt.Errorf("dispatcher: %w", err)
	}
	d.snapshotted = slot
	return n, nil
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

// Register registers the workflows and activities a cell's worker hosts:
// with them AwaitActive, which every workflow's steps wait in while their
// tenant is not active (perform), reading the status with the payment
// activities' pool, role and Begin (a test's failures injected there
// reach it too, as intended: it is the same database).
func Register(r workflow.Registry, p *Payments, s *Simulations, m *Migrations, a *Approvals, st *Statements, rc *Recurrences, sn *Snapshots, d *Depreciations,
	c *Consolidations, e *EInvoiceFetches, es *EInvoiceSenders, mr *Mrps) {
	r.RegisterWorkflowWithOptions(ProposePaymentRun, sdkworkflow.RegisterOptions{Name: ProposeWorkflow})
	r.RegisterWorkflowWithOptions(ReleasePaymentRun, sdkworkflow.RegisterOptions{Name: ReleaseWorkflow})
	r.RegisterWorkflowWithOptions(SimulateChange, sdkworkflow.RegisterOptions{Name: SimulateWorkflow})
	r.RegisterWorkflowWithOptions(MigrateRun, sdkworkflow.RegisterOptions{Name: MigrateWorkflow})
	r.RegisterActivity(p)
	r.RegisterActivity(&Tenants{Pool: p.Pool, AppRole: p.AppRole, Begin: p.Begin})
	r.RegisterActivity(s)
	r.RegisterWorkflowWithOptions(ApprovalDeadline, sdkworkflow.RegisterOptions{Name: DeadlineWorkflow})
	r.RegisterActivity(m)
	r.RegisterActivity(a)
	r.RegisterWorkflowWithOptions(FetchBankStatements, sdkworkflow.RegisterOptions{Name: FetchWorkflow})
	r.RegisterActivity(st)
	r.RegisterWorkflowWithOptions(ParkRecurringEntry, sdkworkflow.RegisterOptions{Name: RecurWorkflow})
	r.RegisterActivity(rc)
	r.RegisterWorkflowWithOptions(TakeCashSnapshot, sdkworkflow.RegisterOptions{Name: SnapshotWorkflow})
	// By name: Snapshots.Due is the dispatcher's question, not an activity,
	// and Recurrences registers a "Due" already.
	r.RegisterActivityWithOptions(sn.TakeSnapshot, activity.RegisterOptions{Name: "TakeSnapshot"})
	r.RegisterActivityWithOptions(sn.PruneSnapshots, activity.RegisterOptions{Name: "PruneSnapshots"})
	r.RegisterWorkflowWithOptions(DepreciationRun, sdkworkflow.RegisterOptions{Name: DepreciationWorkflow})
	r.RegisterActivity(d)
	r.RegisterWorkflowWithOptions(ConsolidationRun, sdkworkflow.RegisterOptions{Name: ConsolidationWorkflow})
	r.RegisterActivity(c)
	r.RegisterWorkflowWithOptions(FetchEInvoices, sdkworkflow.RegisterOptions{Name: EInvoiceFetchWorkflow})
	r.RegisterWorkflowWithOptions(ScheduledEInvoiceFetch, sdkworkflow.RegisterOptions{Name: ScheduledEInvoiceFetchWorkflow})
	r.RegisterActivity(e)
	r.RegisterWorkflowWithOptions(SendEInvoice, sdkworkflow.RegisterOptions{Name: EInvoiceSendWorkflow})
	r.RegisterActivity(es)
	r.RegisterWorkflowWithOptions(MrpRun, sdkworkflow.RegisterOptions{Name: MrpWorkflow})
	r.RegisterActivity(mr)
}
