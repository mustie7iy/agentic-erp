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

	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
	"github.com/mustie7iy/agentic-erp/internal/platform/worker"
)

// Snapshots of the liquidity forecast (ADR-0051 §6, T53f): the
// dispatcher's snapshot step starts, for each company whose daily snapshot
// is due (its local time past its cash.snapshot_time, the date's snapshot
// not taken), a TakeCashSnapshot workflow whose id names the company and
// the date, so the engine starts one per date however many replicas and
// rounds dispatch it. Its activities take the snapshot, once (the database
// keeps one daily snapshot per company and date), then delete the
// company's snapshots past their retention. The cash context is a leaf
// (ADR-0051 §2): the activities reach it through CashSnapshots, which the
// composition root wires.

// CashSnapshots is what the snapshot workflow asks the cash context
// (cash.Endpoints implements it).
type CashSnapshots interface {
	// SnapshotTenants lists, as the worker role, the tenants that manage cash.
	SnapshotTenants(ctx context.Context, tx pgx.Tx) ([]uuid.UUID, error)
	// Snapshotter is the principal the snapshots are taken as in the tenant.
	Snapshotter(tenant uuid.UUID) reqctx.Principal
	// DueSnapshots are the tenant's companies whose snapshot is due at now,
	// by legal entity, with the company's date.
	DueSnapshots(ctx context.Context, now time.Time) (map[uuid.UUID]time.Time, error)
	// ReadDailySnapshot reads the legal entity's snapshot of the date in
	// the context's transaction and returns what writes it in another, or
	// none with the outcome (exists, skipped).
	ReadDailySnapshot(ctx context.Context, legalEntity uuid.UUID, date time.Time) (func(ctx context.Context) (string, error), string, error)
	// PruneSnapshots deletes the legal entity's snapshots past their
	// retention at the date and says how many.
	PruneSnapshots(ctx context.Context, legalEntity uuid.UUID, date time.Time) (int, error)
}

// SnapshotInput names a company's daily snapshot.
type SnapshotInput struct {
	TenantID      uuid.UUID `json:"tenant_id"`
	LegalEntityID uuid.UUID `json:"legal_entity_id"`
	Date          string    `json:"date"` // YYYY-MM-DD, the company's day
}

// SnapshotWorkflow is the registered name of TakeCashSnapshot.
const SnapshotWorkflow = "TakeCashSnapshot"

// SnapshotWorkflowID is the id of a company's snapshot of a date:
// {tenant}-legal_entity-{legal entity}-cash_snapshot-{date}.
func SnapshotWorkflowID(in SnapshotInput) string {
	return fmt.Sprintf("%s-legal_entity-%s-cash_snapshot-%s", in.TenantID, in.LegalEntityID, in.Date)
}

// TakeCashSnapshot takes the company's snapshot of the date, then deletes
// its snapshots past their retention. Each activity is retried until it
// answers; a date that does not parse is not retried.
func TakeCashSnapshot(ctx workflow.Context, in SnapshotInput) error {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: time.Minute}})
	var a *Snapshots
	if err := workflow.ExecuteActivity(ctx, a.TakeSnapshot, in).Get(ctx, nil); err != nil {
		return err //nolint:wrapcheck // workflow result: the engine records Future.Get's ActivityError, which carries the activity type and id, as the failure; a wrap would put a generic wrapError failure on top and add no context
	}
	return workflow.ExecuteActivity(ctx, a.PruneSnapshots, in).Get(ctx, nil) //nolint:wrapcheck // workflow result, as above
}

// Snapshots holds the snapshot activities and the dispatcher's question of
// which are due. Each runs in a transaction of the tenant as the cash
// context's snapshotter.
type Snapshots struct {
	Pool       *database.Pool
	AppRole    string // erp_app: in the tenant's scope
	WorkerRole string // erp_worker: finds the tenants that manage cash
	Cash       CashSnapshots
	// Now is the audit entries' clock; the companies' days are the cash
	// context's (cash.Endpoints.Now). nil: the wall clock.
	Now func() time.Time
	// Begin starts transactions; nil means Pool.Begin. Tests inject failures.
	Begin database.BeginFunc
}

func (s *Snapshots) begin(ctx context.Context, sc database.Scope) (pgx.Tx, error) {
	return database.BeginOr(ctx, s.Pool, s.Begin, sc)
}

// as runs fn in one transaction of the tenant as the snapshotter.
func (s *Snapshots) as(ctx context.Context, tenant uuid.UUID, name string, fn func(ctx context.Context) error) error {
	return s.job(database.Scope{Role: s.AppRole, TenantID: tenant}, name).As(ctx,
		func(context.Context, pgx.Tx) (reqctx.Principal, bool, error) {
			return s.Cash.Snapshotter(tenant), true, nil
		}, fn)
}

// reading runs fn as the snapshotter in a read-only transaction of the
// tenant at REPEATABLE READ: one database state for the forecast's reads.
func (s *Snapshots) reading(ctx context.Context, tenant uuid.UUID, name string, fn func(ctx context.Context) error) error {
	return s.job(database.Scope{Role: s.AppRole, TenantID: tenant, ReadOnly: true, Isolation: pgx.RepeatableRead}, name).As(ctx,
		func(context.Context, pgx.Tx) (reqctx.Principal, bool, error) {
			return s.Cash.Snapshotter(tenant), true, nil
		}, fn)
}

func (s *Snapshots) job(scope database.Scope, name string) uow.Job {
	return uow.Job{Begin: s.begin, Scope: scope, Now: s.Now, Name: name}
}

// date parses the input's date; one that does not parse is not retried.
func (in SnapshotInput) date() (time.Time, error) {
	d, err := time.Parse(time.DateOnly, in.Date)
	if err != nil {
		return time.Time{}, temporal.NewNonRetryableApplicationError(err.Error(), "InvalidDate", err) //nolint:wrapcheck // constructs the engine's non-retryable ApplicationError with err as its cause; nothing to wrap
	}
	return d, nil
}

// TakeSnapshot takes the company's snapshot of the date and says what it
// did: it reads the forecast in a read-only transaction of one database
// state, ends it, then writes in a transaction of its own, so it never
// holds a connection while it waits for another. Run twice, the second run
// finds the first snapshot and writes and publishes nothing.
func (s *Snapshots) TakeSnapshot(ctx context.Context, in SnapshotInput) (string, error) {
	date, err := in.date()
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("workflows: snapshot of legal entity %s on %s", in.LegalEntityID, in.Date)
	var write func(ctx context.Context) (string, error)
	var outcome string
	err = s.reading(ctx, in.TenantID, name, func(ctx context.Context) error {
		var err error
		write, outcome, err = s.Cash.ReadDailySnapshot(ctx, in.LegalEntityID, date)
		return err
	})
	if err != nil || write == nil {
		return outcome, asProblem(err)
	}
	err = s.as(ctx, in.TenantID, name, func(ctx context.Context) error {
		var err error
		outcome, err = write(ctx)
		return err
	})
	return outcome, asProblem(err)
}

// PruneSnapshots deletes the company's snapshots past their retention at
// the date. Run twice, the second run finds none.
func (s *Snapshots) PruneSnapshots(ctx context.Context, in SnapshotInput) (int, error) {
	date, err := in.date()
	if err != nil {
		return 0, err
	}
	var n int
	err = s.as(ctx, in.TenantID, fmt.Sprintf("workflows: snapshots of legal entity %s before %s", in.LegalEntityID, in.Date), func(ctx context.Context) error {
		var err error
		n, err = s.Cash.PruneSnapshots(ctx, in.LegalEntityID, date)
		return err
	})
	return n, asProblem(err)
}

// Due lists every tenant's due snapshots at now, by tenant and legal
// entity: the tenants as the worker role, the companies in each tenant on
// their own days. One tenant's failure does not hide the others'.
func (s *Snapshots) Due(ctx context.Context, now time.Time) ([]SnapshotInput, error) {
	tenants, err := s.tenants(ctx)
	if err != nil {
		return nil, err
	}
	var due []SnapshotInput
	var failures []error
	for _, tenant := range tenants {
		err := s.as(ctx, tenant, fmt.Sprintf("workflows: due snapshots of tenant %s", tenant), func(ctx context.Context) error {
			dates, err := s.Cash.DueSnapshots(ctx, now)
			les := make([]uuid.UUID, 0, len(dates))
			for le := range dates {
				les = append(les, le)
			}
			slices.SortFunc(les, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
			for _, le := range les {
				due = append(due, SnapshotInput{TenantID: tenant, LegalEntityID: le, Date: dates[le].Format(time.DateOnly)})
			}
			return err
		})
		if err != nil {
			failures = append(failures, err)
		}
	}
	return due, errors.Join(failures...)
}

func (s *Snapshots) tenants(ctx context.Context) ([]uuid.UUID, error) {
	tx, err := s.begin(ctx, database.Scope{Role: s.WorkerRole, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	tenants, err := s.Cash.SnapshotTenants(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("workflows: %w", err)
	}
	return tenants, nil
}

// snapshotTaken is the outcome of a snapshot written (cash.SnapshotTaken).
const snapshotTaken = "taken"

// snapshotEvery is how often the dispatcher, and the in-process
// snapshotter, ask which snapshots are due: each asks every tenant that
// manages cash, so not every round.
const snapshotEvery = time.Minute

// Snapshotter is the in-process snapshot worker of a cell without a
// workflow engine: each round it takes every due snapshot, then prunes,
// with the activities the workflow runs.
type Snapshotter struct {
	Snapshots *Snapshots
	Interval  time.Duration // between rounds; default a minute
	Now       func() time.Time
	Log       *slog.Logger
}

// RunOnce takes every due snapshot and returns how many it took.
func (w *Snapshotter) RunOnce(ctx context.Context) (int, error) {
	due, err := w.Snapshots.Due(ctx, clock.Now(w.Now))
	n := 0
	failures := []error{err}
	for _, in := range due {
		outcome, err := w.Snapshots.TakeSnapshot(ctx, in)
		if err == nil {
			_, err = w.Snapshots.PruneSnapshots(ctx, in)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("cash snapshots: legal entity %s on %s: %w", in.LegalEntityID, in.Date, err))
			continue
		}
		if outcome == snapshotTaken {
			n++
		}
	}
	return n, errors.Join(failures...)
}

// Run takes due snapshots until ctx ends; errors are logged and retried
// after the interval.
func (w *Snapshotter) Run(ctx context.Context) {
	worker.Loop(ctx, w.Interval, snapshotEvery, func(ctx context.Context) (bool, error) {
		n, err := w.RunOnce(ctx)
		if n > 0 && !worker.Failed(ctx, err) {
			w.Log.InfoContext(ctx, "cash snapshots taken", "count", n)
		}
		return false, err
	}, w.Log, "cash snapshots failed")
}
