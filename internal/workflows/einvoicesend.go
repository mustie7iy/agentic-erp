package workflows

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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
// long as each step says, until it is failed, or completed and no longer
// followed (its delivery status and its tax data document's final, As
// built E4). A cancelled
// Operation is ended by the next step, which fails the submission
// (einvoicing.send_cancelled). The einvoicing context is a leaf, so the
// workflow reaches it through the EInvoiceSends port, which the
// composition root wires to it.

// EInvoiceSendKind is the kind of a send's Operation (einvoicing.KindSend,
// which the composition root's test holds equal), and EInvoiceTaxDataKind
// that of a person's request of a transmitted e-invoice's tax data
// document (einvoicing.KindTaxData, As built E4), which SendEInvoice
// carries out too: the request takes over the submission, whose step makes
// it and goes on reading the delivery status.
const (
	EInvoiceSendKind    = "einvoicing.send"
	EInvoiceTaxDataKind = "einvoicing.tax_data"
)

// EInvoiceSendStep is where a send stands after a step: finished, or to
// be advanced again after Wait.
type EInvoiceSendStep struct {
	Done bool          `json:"done"`
	Wait time.Duration `json:"wait"`
}

// EInvoiceSends is a send's work as the einvoicing context does it: the
// send's principal, its one step, idempotent, and the sends the in-process
// sender advances.
type EInvoiceSends interface {
	Sender(tenant uuid.UUID) reqctx.Principal
	AdvanceEInvoiceSubmission(ctx context.Context, outgoing, operation uuid.UUID, in Within) (EInvoiceSendStep, error)
	// UnfinishedEInvoiceSends lists, in the transaction's tenant, the sends
	// no workflow carries out whose step is due at now or whose Operation
	// was cancelled, by e-invoice with its Operation.
	UnfinishedEInvoiceSends(ctx context.Context, tx pgx.Tx, now time.Time) (map[uuid.UUID]uuid.UUID, error)
	// FollowedEInvoiceTenants lists, across tenants (the worker role's
	// transaction), the active tenants with a completed send whose
	// delivery read or tax data request is due at now (As built E4): its
	// send's Operation has finished, so the tenants of send Operations do
	// not name them.
	FollowedEInvoiceTenants(ctx context.Context, tx pgx.Tx, now time.Time) ([]uuid.UUID, error)
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
// The steps run on a context the engine's cancellation (which the
// Operation's :cancel sends) does not reach, so a step under way, a call
// to the provider among it, is never cut off; the cancellation cuts the
// wait short instead, and the steps go on, waits and all, until one finds
// the Operation cancelled and ends the submission.
func SendEInvoice(ctx workflow.Context, in EInvoiceSendInput) error {
	steps, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()
	steps = workflow.WithActivityOptions(steps, sendOptions)
	waits := ctx
	var a *EInvoiceSenders
	for i := 0; i < sendStepsPerRun; i++ {
		var step EInvoiceSendStep
		if err := perform(steps, in.TenantID, &step, a.AdvanceEInvoiceSubmission, in); err != nil {
			return err
		}
		if step.Done {
			return nil
		}
		if err := workflow.Sleep(waits, max(step.Wait, time.Second)); err != nil { // a timer fails only when cancelled
			waits = steps // cancelled: the next step ends the submission
		}
	}
	return workflow.NewContinueAsNewError(steps, EInvoiceSendWorkflow, in) //nolint:wrapcheck // the engine's continue-as-new, returned as it is
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
// send of every tenant with send Operations whose step is due or whose
// Operation was cancelled (so that its step ends it), as the einvoicing
// context lists them (unfinishedRuns, as the depreciation runner's), so a
// send waiting for its next status read costs nothing until then. A step
// that fails is tried again the next round.
type EInvoiceSender struct {
	Senders  *EInvoiceSenders
	Interval time.Duration // between rounds; default 1 second
	Log      *slog.Logger
}

// RunOnce advances every send due or cancelled and returns how many steps
// it took.
func (w *EInvoiceSender) RunOnce(ctx context.Context) (int, error) {
	s := w.Senders
	now := clock.Now(s.Now)
	tenants := func(ctx context.Context, tx pgx.Tx) ([]uuid.UUID, error) {
		sending, err := operations.TenantsOf(ctx, tx, EInvoiceSendKind)
		if err != nil {
			return nil, err
		}
		followed, err := s.Sends.FollowedEInvoiceTenants(ctx, tx, now)
		all := slices.Concat(sending, followed)
		slices.SortFunc(all, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
		return slices.Compact(all), err
	}
	sends, err := unfinishedRunsOf(ctx, s.begin, s.AppRole, s.WorkerRole, EInvoiceSendKind, tenants, func(ctx context.Context, tx pgx.Tx) (map[uuid.UUID]uuid.UUID, error) {
		return s.Sends.UnfinishedEInvoiceSends(ctx, tx, now)
	})
	failures := []error{err}
	n := 0
	for _, r := range sends {
		if _, err := s.AdvanceEInvoiceSubmission(ctx, EInvoiceSendInput{TenantID: r.tenant, OutgoingID: r.run, OperationID: r.operation}); err != nil {
			failures = append(failures, fmt.Errorf("e-invoice sends: send %s: %w", r.operation, err))
			continue
		}
		n++
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
