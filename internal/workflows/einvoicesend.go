package workflows

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
	"github.com/mustie7iy/agentic-erp/internal/platform/worker"
)

// E-invoice sends (ADR-0061 §9, As built E3): the request queues the
// e-invoice's submission with its Operation (kind einvoicing.send, the
// e-invoice its resource); the dispatcher starts SendEInvoice for it,
// which advances the submission one idempotent step at a time (send, read
// the provider's status, send again after SENDING_FAILED) and sleeps as
// long as each step says, until it is completed or failed. The einvoicing
// context is a leaf, so the workflow reaches it through the EInvoiceSends
// port, which the composition root wires to it.

// EInvoiceSendKind is the kind of a send's Operation (einvoicing.KindSend,
// which the composition root's test holds equal).
const EInvoiceSendKind = "einvoicing.send"

// EInvoiceSendStep is where a send stands after a step: finished, or to
// be advanced again after Wait.
type EInvoiceSendStep struct {
	Done bool          `json:"done"`
	Wait time.Duration `json:"wait"`
}

// EInvoiceSends is a send's work as the einvoicing context does it: the
// send's principal and its one step, idempotent.
type EInvoiceSends interface {
	Sender(tenant uuid.UUID) reqctx.Principal
	AdvanceEInvoiceSubmission(ctx context.Context, outgoing, operation uuid.UUID, in Within) (EInvoiceSendStep, error)
}

// EInvoiceSendInput names the send a SendEInvoice workflow carries out.
type EInvoiceSendInput struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	OutgoingID  uuid.UUID `json:"outgoing_einvoice_id"`
	OperationID uuid.UUID `json:"operation_id"`
}

// EInvoiceSendWorkflow is the registered name of SendEInvoice.
const EInvoiceSendWorkflow = "SendEInvoice"

// EInvoiceSendWorkflowID is a send's workflow id:
// {tenant}-einvoice_send-{operation} (ADR-0012, the Operation as business
// key: a person's send again after a failure is a new Operation).
func EInvoiceSendWorkflowID(tenant, operation uuid.UUID) string {
	return fmt.Sprintf("%s-einvoice_send-%s", tenant, operation)
}

// sendStepsPerRun bounds one run's history: a step is an activity and a
// timer, about nine events, so a run continues as new well within the
// engine's 51,200; a submission is read at most 500 times, so a run or two
// carries a send to its end.
const sendStepsPerRun = 1000

// sendOptions are the step's options: tried with backoff, without a limit,
// on an error of the machinery (the database failing), since the
// submission is recorded and only the step's own transactions failed; the
// provider's failures the step records itself.
var sendOptions = workflow.ActivityOptions{StartToCloseTimeout: 5 * time.Minute,
	RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: 10 * time.Minute,
		NonRetryableErrorTypes: []string{ProblemError}}}

// SendEInvoice carries out a send: it advances the submission and sleeps
// as long as the step says, until the step is done; after sendStepsPerRun
// steps it continues as new. Each step runs through perform, so a send
// whose tenant is suspended waits until it is active again (po:2026-11-09).
func SendEInvoice(ctx workflow.Context, in EInvoiceSendInput) error {
	ctx = workflow.WithActivityOptions(ctx, sendOptions)
	var a *EInvoiceSenders
	for i := 0; i < sendStepsPerRun; i++ {
		var step EInvoiceSendStep
		if err := perform(ctx, in.TenantID, &step, a.AdvanceEInvoiceSubmission, in); err != nil {
			return err
		}
		if step.Done {
			return nil
		}
		if err := workflow.Sleep(ctx, max(step.Wait, time.Second)); err != nil {
			return err //nolint:wrapcheck // the engine's cancellation, the workflow's result as it is
		}
	}
	return workflow.NewContinueAsNewError(ctx, EInvoiceSendWorkflow, in) //nolint:wrapcheck // the engine's continue-as-new, returned as it is
}

// EInvoiceSenders holds the send's activity: its step in the send's
// tenant, as the einvoicing context's system principal.
type EInvoiceSenders struct {
	Pool       *database.Pool
	AppRole    string // erp_app: in the tenant's scope
	WorkerRole string // erp_worker: finds the sends still queued (EInvoiceSender)
	Sends      EInvoiceSends
	Now        func() time.Time
	// Begin starts transactions; nil means Pool.Begin. Tests inject failures.
	Begin database.BeginFunc
}

func (s *EInvoiceSenders) begin(ctx context.Context, sc database.Scope) (pgx.Tx, error) {
	return database.BeginActive(ctx, s.Pool, s.Begin, sc)
}

// within is the send's Within: a transaction of its tenant as the sender.
func (s *EInvoiceSenders) within(in EInvoiceSendInput) Within {
	return tenantWithin(uow.Job{Begin: s.begin, Scope: database.Scope{Role: s.AppRole, TenantID: in.TenantID}, Now: s.Now,
		Name: "workflows: e-invoice send " + in.OperationID.String()}, s.Sends.Sender(in.TenantID))
}

// AdvanceEInvoiceSubmission advances the send's submission by one step.
func (s *EInvoiceSenders) AdvanceEInvoiceSubmission(ctx context.Context, in EInvoiceSendInput) (EInvoiceSendStep, error) {
	step, err := s.Sends.AdvanceEInvoiceSubmission(ctx, in.OutgoingID, in.OperationID, s.within(in))
	return step, asProblem(err)
}

// EInvoiceSender is the in-process send worker of a cell without a
// workflow engine: each round it advances, by the workflow's step, every
// send of every tenant still queued whose step is due, and keeps when each
// is next due, so a send waiting for its next status read costs nothing
// until then. A step that fails is tried again the next round.
type EInvoiceSender struct {
	Senders  *EInvoiceSenders
	Interval time.Duration // between rounds; default 1 second
	Log      *slog.Logger

	due map[uuid.UUID]time.Time // operation -> when its next step is due
}

// queued lists the sends still queued, across tenants as the worker role.
func (w *EInvoiceSender) queued(ctx context.Context) ([]operations.Waiting, error) {
	tx, err := w.Senders.begin(ctx, database.Scope{Role: w.Senders.WorkerRole, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	waiting, err := operations.StillQueuedOf(ctx, tx, EInvoiceSendKind)
	if err != nil {
		return nil, fmt.Errorf("workflows: %w", err)
	}
	return waiting, nil
}

// RunOnce advances every due send still queued and returns how many
// steps it took.
func (w *EInvoiceSender) RunOnce(ctx context.Context) (int, error) {
	waiting, err := w.queued(ctx)
	failures := []error{err}
	due := make(map[uuid.UUID]time.Time, len(waiting)) // only the sends still queued: one finished elsewhere is forgotten
	for _, q := range waiting {
		if at, ok := w.due[q.ID]; ok {
			due[q.ID] = at
		}
	}
	w.due = due
	now := clock.Now(w.Senders.Now)
	n := 0
	for _, q := range waiting {
		if at, ok := w.due[q.ID]; ok && now.Before(at) {
			continue
		}
		step, err := w.Senders.AdvanceEInvoiceSubmission(ctx, EInvoiceSendInput{TenantID: q.TenantID, OutgoingID: q.Resource, OperationID: q.ID})
		if err != nil {
			failures = append(failures, fmt.Errorf("e-invoice sends: send %s: %w", q.ID, err))
			continue
		}
		n++
		if step.Done {
			delete(w.due, q.ID)
		} else {
			w.due[q.ID] = now.Add(step.Wait)
		}
	}
	return n, errors.Join(failures...)
}

// Run advances sends until ctx ends; errors are logged and retried after
// the interval.
func (w *EInvoiceSender) Run(ctx context.Context) {
	worker.Loop(ctx, w.Interval, time.Second, func(ctx context.Context) (bool, error) {
		n, err := w.RunOnce(ctx)
		if n > 0 && !worker.Failed(ctx, err) {
			w.Log.InfoContext(ctx, "e-invoice send steps taken", "count", n)
		}
		return false, err
	}, w.Log, "e-invoice sends failed")
}
