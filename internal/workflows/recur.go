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

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
	"github.com/mustie7iy/agentic-erp/internal/platform/worker"
)

// Recurring entries (ADR-0056): the dispatcher's recur step starts, for
// each entry template's next due date, a workflow whose one activity parks
// the template's entry for its owner. The workflow id names the template,
// the date and the template's version, so the engine starts one per date
// however many replicas and rounds dispatch it, and a template changed
// meanwhile gets a fresh start rather than an id used up; the template
// deals with its dates in order, and the database keeps one entry per
// template and date, however often the activity runs. Entries are parked,
// never posted (product owner 2026-11-01).

// RecurInput names a template's due date.
type RecurInput struct {
	TenantID   uuid.UUID `json:"tenant_id"`
	TemplateID uuid.UUID `json:"template_id"`
	Date       string    `json:"date"`    // YYYY-MM-DD, the company's day
	Version    int32     `json:"version"` // the template's when it was found due
}

// RecurWorkflow is the registered name of ParkRecurringEntry.
const RecurWorkflow = "ParkRecurringEntry"

// RecurWorkflowID is the id of the workflow parking a template's entry
// for a date: {tenant}-entry_template-{template}-{date}-v{version}.
func RecurWorkflowID(in RecurInput) string {
	return fmt.Sprintf("%s-entry_template-%s-%s-v%d", in.TenantID, in.TemplateID, in.Date, in.Version)
}

// ParkRecurringEntry parks the template's entry for the date. Its one
// activity is retried until it answers: a refusal is not a failure, the
// activity records it on the template, and a date not the template's to
// park yet (an earlier one pending, or not due on the company's day) is
// retried until it is.
func ParkRecurringEntry(ctx workflow.Context, in RecurInput) error {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: time.Minute}})
	var a *Recurrences
	return perform(ctx, in.TenantID, nil, a.ParkOccurrence, in)
}

// Recurrences holds the recurring entries' activity and the dispatcher's
// question of which dates are due. Each runs in a transaction of the
// tenant as the ERP's recurrer (ledger.Recurrer).
type Recurrences struct {
	Pool       *database.Pool
	AppRole    string // erp_app: in the tenant's scope
	WorkerRole string // erp_worker: finds the tenants with templates due
	Endpoints  *ledger.Endpoints
	// Now is the audit entries' clock; the companies' days are the
	// endpoints' (ledger.Endpoints.Now). nil: the wall clock.
	Now func() time.Time
	// Begin starts transactions; nil means Pool.Begin. Tests inject failures.
	Begin database.BeginFunc
}

func (r *Recurrences) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	return database.BeginActive(ctx, r.Pool, r.Begin, s)
}

// as runs fn in one transaction of the tenant as the recurrer.
func (r *Recurrences) as(ctx context.Context, tenant uuid.UUID, name string, fn func(ctx context.Context) error) error {
	return uow.Job{Begin: r.begin, Scope: database.Scope{Role: r.AppRole, TenantID: tenant}, Now: r.Now, Name: name}.As(ctx,
		func(context.Context, pgx.Tx) (reqctx.Principal, bool, error) {
			return ledger.Recurrer(tenant), true, nil
		}, fn)
}

// ParkOccurrence parks the template's entry for the date (ledger.Endpoints.ParkOccurrence)
// and says what it did: parked, refused or skipped. Run twice, the second
// run finds the date behind the template's next and skips. A date not the
// template's to park yet is an error the engine retries; one that does
// not parse is not retried.
func (r *Recurrences) ParkOccurrence(ctx context.Context, in RecurInput) (string, error) {
	date, err := time.Parse(time.DateOnly, in.Date)
	if err != nil {
		return "", temporal.NewNonRetryableApplicationError(err.Error(), "InvalidDate", err) //nolint:wrapcheck // constructs the engine's non-retryable ApplicationError with err as its cause; nothing to wrap
	}
	var outcome string
	err = r.as(ctx, in.TenantID, fmt.Sprintf("workflows: park entry template %s on %s", in.TemplateID, in.Date), func(ctx context.Context) error {
		var err error
		outcome, err = r.Endpoints.ParkOccurrence(ctx, in.TemplateID, date)
		return err
	})
	return outcome, asProblem(err)
}

// Due lists the next due date of every tenant's templates at now: the
// tenants as the worker role, the dates in each tenant on its companies'
// days.
func (r *Recurrences) Due(ctx context.Context, now time.Time) ([]RecurInput, error) {
	tenants, err := r.tenants(ctx, now)
	if err != nil {
		return nil, err
	}
	var due []RecurInput
	var failures []error
	for _, tenant := range tenants {
		err := r.as(ctx, tenant, fmt.Sprintf("workflows: due entry templates of tenant %s", tenant), func(ctx context.Context) error {
			dates, err := r.Endpoints.DueOccurrences(ctx)
			for _, o := range dates {
				due = append(due, RecurInput{TenantID: tenant, TemplateID: o.TemplateID, Date: o.Date.Format(time.DateOnly), Version: o.Version})
			}
			return err
		})
		if err != nil {
			failures = append(failures, err)
		}
	}
	return due, errors.Join(failures...)
}

func (r *Recurrences) tenants(ctx context.Context, now time.Time) ([]uuid.UUID, error) {
	tx, err := r.begin(ctx, database.Scope{Role: r.WorkerRole, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	tenants, err := ledger.RecurringTenants(ctx, tx, now)
	if err != nil {
		return nil, fmt.Errorf("workflows: %w", err)
	}
	return tenants, nil
}

// Recurrer is the in-process recurring entries worker of a cell without a
// workflow engine: each round it parks each template's next due date with
// the activity the workflow runs (a missed run catches up a date per
// round), until the engine takes over.
type Recurrer struct {
	Recurrences *Recurrences
	Interval    time.Duration // between rounds; default 1 second
	Now         func() time.Time
	Log         *slog.Logger
}

// RunOnce parks every due date and returns how many entries it parked.
func (w *Recurrer) RunOnce(ctx context.Context) (int, error) {
	due, err := w.Recurrences.Due(ctx, clock.Now(w.Now))
	n := 0
	failures := []error{err}
	for _, in := range due {
		outcome, err := w.Recurrences.ParkOccurrence(ctx, in)
		if err != nil {
			failures = append(failures, fmt.Errorf("recurring entries: template %s on %s: %w", in.TemplateID, in.Date, err))
			continue
		}
		if outcome == ledger.OccurrenceParked {
			n++
		}
	}
	return n, errors.Join(failures...)
}

// Run parks due dates until ctx ends; errors are logged and retried after
// the interval.
func (w *Recurrer) Run(ctx context.Context) {
	worker.Loop(ctx, w.Interval, time.Second, func(ctx context.Context) (bool, error) {
		n, err := w.RunOnce(ctx)
		if n > 0 && !worker.Failed(ctx, err) {
			w.Log.InfoContext(ctx, "recurring entries parked", "count", n)
		}
		return false, err
	}, w.Log, "recurring entries failed")
}
