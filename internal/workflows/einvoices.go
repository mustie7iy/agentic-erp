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
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
	"github.com/mustie7iy/agentic-erp/internal/platform/worker"
)

// E-invoice fetches (ADR-0061 §4, T65a): the request queues a fetch from
// an einvoice_provider connector as its Operation; the dispatcher starts
// FetchEInvoices for it, whose activities list what the provider holds,
// record each message (and acknowledge it after its commit) and finish the
// Operation with the counts. The einvoicing context is a leaf that no
// context imports (ADR-0061 §2), so the workflow reaches it through the
// EInvoices port, which the composition root wires to it.

// EInvoiceFetchKind is the kind of a fetch's Operation (einvoicing.KindFetch,
// which the composition root's test holds equal).
const EInvoiceFetchKind = "einvoicing.fetch"

// EInvoiceMessage is a message the provider holds: its id and the
// participant it is addressed to.
type EInvoiceMessage struct {
	ID       string `json:"id"`
	Receiver string `json:"receiver,omitempty"`
}

// EInvoiceFailure is a message a fetch did not record: its id and the
// problem code that stopped it (platform.unavailable for a failure of the
// machinery that outlasted the step's tries).
type EInvoiceFailure struct {
	ID   string `json:"id"`
	Code string `json:"code"`
}

// EInvoiceCounts are what a fetch found: the messages it recorded and
// those recorded by an earlier fetch.
type EInvoiceCounts struct {
	Recorded        int `json:"recorded"`
	AlreadyRecorded int `json:"already_recorded"`
}

// Within runs fn in one transaction of the fetch's tenant as its
// principal; a step that talks to the provider opens one around each of
// its database parts, never around the provider's call.
type Within = func(ctx context.Context, fn func(ctx context.Context) error) error

// EInvoices is a fetch's work as the einvoicing context does it (ADR-0061
// §4), each step idempotent: the fetch's principal, the messages pending at
// the connector's provider, recording one (its outcome: recorded or
// already_recorded), and ending the Operation, with its counts and the
// messages not fetched, or with the problem that stopped it. Finish and
// Fail run in the transaction the caller set up.
type EInvoices interface {
	Fetcher(tenant uuid.UUID) reqctx.Principal
	PendingEInvoices(ctx context.Context, connector uuid.UUID, in Within) ([]EInvoiceMessage, error)
	RecordEInvoice(ctx context.Context, connector, operation uuid.UUID, m EInvoiceMessage, in Within) (string, error)
	FinishEInvoiceFetch(ctx context.Context, operation uuid.UUID, counts EInvoiceCounts, failed []EInvoiceFailure) error
	FailEInvoiceFetch(ctx context.Context, operation uuid.UUID, why *problem.Problem) error
}

// EInvoiceFetchInput names the fetch a FetchEInvoices workflow carries out.
type EInvoiceFetchInput struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	ConnectorID uuid.UUID `json:"connector_id"`
	OperationID uuid.UUID `json:"operation_id"`
}

// EInvoiceFetchWorkflow is the registered name of FetchEInvoices.
const EInvoiceFetchWorkflow = "FetchEInvoices"

// EInvoiceFetchWorkflowID is the id of an on-demand fetch's workflow:
// {tenant}-einvoice_fetch-{operation} (ADR-0012, the Operation as business
// key; the scheduled fetch of T65b names the connector and its period).
func EInvoiceFetchWorkflowID(tenant, operation uuid.UUID) string {
	return fmt.Sprintf("%s-einvoice_fetch-%s", tenant, operation)
}

// fetchSteps are a fetch's steps. The workflow runs each as an activity
// and the in-process fetcher calls them directly, so both share one
// control flow (run).
type fetchSteps struct {
	pending func() ([]EInvoiceMessage, error)
	record  func(m EInvoiceMessage) (string, error)
	finish  func(counts EInvoiceCounts, failed []EInvoiceFailure) error
	fail    func(why problem.Problem) error
}

// unlisted is the problem a fetch fails with when the provider's messages
// could not be listed: after the step's five tries in the workflow, after
// one in process (EInvoiceFetcher).
func unlisted() problem.Problem {
	return *problem.Unavailable("The provider's messages could not be listed; nothing was fetched. Fetch again.", 0)
}

// failure is how a message's step failed: its problem's code, or
// platform.unavailable for a failure of the machinery.
func failure(id string, err error) EInvoiceFailure {
	if why, ok := problemOf(err); ok {
		return EInvoiceFailure{ID: id, Code: why.Code}
	}
	return EInvoiceFailure{ID: id, Code: "platform.unavailable"}
}

// run lists the provider's messages and records each; one that fails
// after its tries does not stop the others, and the fetch then fails
// naming it with the code that stopped it. A business problem listing them (the
// connector refused since the fetch was queued) fails the fetch with it;
// any other error fails it as unlisted and is returned, so the workflow
// fails with it for the operators. In a workflow it only sequences the
// steps, so it is deterministic.
func (s fetchSteps) run() error {
	messages, err := s.pending()
	if why, ok := problemOf(err); ok {
		return s.fail(why)
	}
	if err != nil {
		return errors.Join(err, s.fail(unlisted()))
	}
	var counts EInvoiceCounts
	var failed []EInvoiceFailure
	for _, m := range messages {
		outcome, err := s.record(m)
		switch {
		case err != nil:
			failed = append(failed, failure(m.ID, err))
		case outcome == "recorded":
			counts.Recorded++
		default:
			counts.AlreadyRecorded++
		}
	}
	if err := s.finish(counts, failed); err != nil {
		return err
	}
	if len(failed) > 0 {
		ids := make([]string, len(failed))
		for i, f := range failed {
			ids[i] = f.ID + " (" + f.Code + ")"
		}
		return temporal.NewNonRetryableApplicationError("messages not fetched: "+strings.Join(ids, ", "), "EInvoicesNotFetched", nil) //nolint:wrapcheck // constructs the engine's non-retryable ApplicationError; nothing to wrap
	}
	return nil
}

// FetchEInvoices carries out a fetch. Each step is tried five times with
// backoff on an error of the machinery (the provider unreachable, the store
// or the database failing), as the other workflows' (ADR-0012); a business
// problem is not retried. Ending the Operation is tried until it is
// recorded (recording), so a fetch always ends.
//
// The steps return Future.Get's error unwrapped: it is the workflow result,
// and the engine records Get's ActivityError, which carries the activity
// type and id, as the failure; a wrap would put a generic wrapError failure
// on top and add no context.
func FetchEInvoices(ctx workflow.Context, in EInvoiceFetchInput) error {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: time.Minute, MaximumAttempts: 5,
			NonRetryableErrorTypes: []string{ProblemError}}})
	var a *EInvoiceFetches
	return fetchSteps{
		pending: func() ([]EInvoiceMessage, error) {
			var messages []EInvoiceMessage
			err := workflow.ExecuteActivity(ctx, a.PendingEInvoices, in).Get(ctx, &messages)
			return messages, err //nolint:wrapcheck // the workflow result, see FetchEInvoices
		},
		record: func(m EInvoiceMessage) (string, error) {
			var outcome string
			err := workflow.ExecuteActivity(ctx, a.RecordEInvoice, in, m).Get(ctx, &outcome)
			return outcome, err //nolint:wrapcheck // the workflow result, see FetchEInvoices
		},
		finish: func(counts EInvoiceCounts, failed []EInvoiceFailure) error {
			return workflow.ExecuteActivity(recording(ctx), a.FinishEInvoiceFetch, in, counts, failed).Get(ctx, nil)
		},
		fail: func(why problem.Problem) error {
			return workflow.ExecuteActivity(recording(ctx), a.FailEInvoiceFetch, in, why).Get(ctx, nil)
		},
	}.run()
}

// EInvoiceFetches holds the fetch's activities: each runs its step of the
// einvoicing context in the fetch's tenant, as the context's system
// principal acting for the connector's companies (ADR-0061 §4).
type EInvoiceFetches struct {
	Pool       *database.Pool
	AppRole    string // erp_app: in the tenant's scope
	WorkerRole string // erp_worker: finds the fetches still queued (EInvoiceFetcher)
	Fetches    EInvoices
	Now        func() time.Time
	// Begin starts transactions; nil means Pool.Begin. Tests inject failures.
	Begin database.BeginFunc
}

func (f *EInvoiceFetches) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	return database.BeginOr(ctx, f.Pool, f.Begin, s)
}

// within is the fetch's Within: a transaction of its tenant as the
// fetcher.
func (f *EInvoiceFetches) within(in EInvoiceFetchInput) Within {
	return func(ctx context.Context, fn func(ctx context.Context) error) error {
		return uow.Job{Begin: f.begin, Scope: database.Scope{Role: f.AppRole, TenantID: in.TenantID}, Now: f.Now,
			Name: "workflows: e-invoice fetch " + in.OperationID.String()}.As(ctx, func(context.Context, pgx.Tx) (reqctx.Principal, bool, error) {
			return f.Fetches.Fetcher(in.TenantID), true, nil
		}, fn)
	}
}

// PendingEInvoices lists the messages the connector's provider holds.
func (f *EInvoiceFetches) PendingEInvoices(ctx context.Context, in EInvoiceFetchInput) ([]EInvoiceMessage, error) {
	messages, err := f.Fetches.PendingEInvoices(ctx, in.ConnectorID, f.within(in))
	return messages, asProblem(err)
}

// RecordEInvoice records one message and acknowledges it.
func (f *EInvoiceFetches) RecordEInvoice(ctx context.Context, in EInvoiceFetchInput, m EInvoiceMessage) (string, error) {
	outcome, err := f.Fetches.RecordEInvoice(ctx, in.ConnectorID, in.OperationID, m, f.within(in))
	return outcome, asProblem(err)
}

// FinishEInvoiceFetch ends the fetch's Operation with its counts.
func (f *EInvoiceFetches) FinishEInvoiceFetch(ctx context.Context, in EInvoiceFetchInput, counts EInvoiceCounts, failed []EInvoiceFailure) error {
	return asProblem(f.within(in)(ctx, func(ctx context.Context) error {
		return f.Fetches.FinishEInvoiceFetch(ctx, in.OperationID, counts, failed)
	}))
}

// FailEInvoiceFetch fails the fetch's Operation with the problem that
// stopped it.
func (f *EInvoiceFetches) FailEInvoiceFetch(ctx context.Context, in EInvoiceFetchInput, why problem.Problem) error {
	return asProblem(f.within(in)(ctx, func(ctx context.Context) error { return f.Fetches.FailEInvoiceFetch(ctx, in.OperationID, &why) }))
}

// EInvoiceFetcher is the in-process fetch worker of a cell without a
// workflow engine: each round it carries out, with the steps the workflow
// runs, every fetch of every tenant still queued. It tries each step once,
// without the workflow's retries: a provider that fails a step fails the
// fetch as the workflow's fifth failed try would (unlisted, or the message
// named with platform.unavailable), and the person fetches again. Only a
// fetch whose Operation cannot be ended (the database failing) stays
// queued, and is tried again the next round.
type EInvoiceFetcher struct {
	Fetches  *EInvoiceFetches
	Interval time.Duration // between rounds; default 1 second
	Log      *slog.Logger
}

// Fetch carries out one fetch: the steps of the workflow, in order, each
// called directly.
func (w *EInvoiceFetcher) Fetch(ctx context.Context, in EInvoiceFetchInput) error {
	f := w.Fetches
	return fetchSteps{
		pending: func() ([]EInvoiceMessage, error) { return f.PendingEInvoices(ctx, in) },
		record:  func(m EInvoiceMessage) (string, error) { return f.RecordEInvoice(ctx, in, m) },
		finish: func(counts EInvoiceCounts, failed []EInvoiceFailure) error {
			return f.FinishEInvoiceFetch(ctx, in, counts, failed)
		},
		fail: func(why problem.Problem) error { return f.FailEInvoiceFetch(ctx, in, why) },
	}.run()
}

// queued lists the fetches still queued, across tenants as the worker
// role.
func (w *EInvoiceFetcher) queued(ctx context.Context) ([]operations.Waiting, error) {
	tx, err := w.Fetches.begin(ctx, database.Scope{Role: w.Fetches.WorkerRole, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	waiting, err := operations.StillQueuedOf(ctx, tx, EInvoiceFetchKind)
	if err != nil {
		return nil, fmt.Errorf("workflows: %w", err)
	}
	return waiting, nil
}

// RunOnce carries out every fetch still queued and returns how many it
// ended.
func (w *EInvoiceFetcher) RunOnce(ctx context.Context) (int, error) {
	waiting, err := w.queued(ctx)
	failures := []error{err}
	n := 0
	for _, q := range waiting {
		if err := w.Fetch(ctx, EInvoiceFetchInput{TenantID: q.TenantID, ConnectorID: q.Resource, OperationID: q.ID}); err != nil {
			failures = append(failures, fmt.Errorf("e-invoice fetches: fetch %s: %w", q.ID, err))
			continue
		}
		n++
	}
	return n, errors.Join(failures...)
}

// Run carries out fetches until ctx ends; errors are logged and retried
// after the interval.
func (w *EInvoiceFetcher) Run(ctx context.Context) {
	worker.Loop(ctx, w.Interval, time.Second, func(ctx context.Context) (bool, error) {
		n, err := w.RunOnce(ctx)
		if n > 0 && !worker.Failed(ctx, err) {
			w.Log.InfoContext(ctx, "e-invoice fetches ended", "count", n)
		}
		return false, err
	}, w.Log, "e-invoice fetches failed")
}
