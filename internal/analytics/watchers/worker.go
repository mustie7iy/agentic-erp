package watchers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

// Actors answers who the worker acts as: a watcher's creator, read in the
// evaluation's own transaction, and refused once no longer active, as a
// request of theirs is (identity.Actors; PO 2026-11-03).
type Actors interface {
	Actor(ctx context.Context, tx pgx.Tx, id uuid.UUID) (reqctx.Principal, error)
	MayAct(ctx context.Context, tx pgx.Tx, id uuid.UUID) error
}

// Worker evaluates a tenant's active watchers after a refresh of its
// analytical store (ADR-0077 §4): each in a transaction of its own as its
// creator, with their company access and permissions, as evaluateMetricWatcher
// evaluates it. Watchers reads the figures through a Queries that answers
// from the store and never reads the API as the creator; a watcher the
// store cannot answer waits for the next refresh. Evaluating twice raises
// no second alert (metric_alert_once).
type Worker struct {
	Watchers *Endpoints
	Begin    database.BeginFunc
	AppRole  string // erp_app: in the tenant's scope
	Actors   Actors
	Now      func() time.Time // stamps the audit entries; nil: the wall clock
	Log      *slog.Logger
}

// due is an active watcher and its creator.
type due struct{ id, creator uuid.UUID }

// Refreshed evaluates the tenant's active watchers, in the order of their
// ids. A refusal of one (its creator no longer active or lacking a
// permission, its metric's data refused) is logged and the others go on;
// a failure of the machinery is returned with the others', for the next
// refresh.
func (w *Worker) Refreshed(ctx context.Context, tenant uuid.UUID) error {
	list, err := w.active(ctx, tenant)
	if err != nil {
		return err
	}
	var errs []error
	for _, d := range list {
		err := uow.Job{Begin: w.Begin, Scope: database.Scope{Role: w.AppRole, TenantID: tenant}, Name: "analytics: evaluate watcher " + d.id.String(),
			Now: w.Now}.As(ctx, w.creator(d), func(ctx context.Context) error {
			_, err := w.Watchers.Evaluate(ctx, d.id, func(int32) error { return nil })
			return err
		})
		var p *problem.Problem
		switch {
		case errors.As(err, &p) && p.Status < http.StatusInternalServerError:
			w.log().InfoContext(ctx, "metric watcher not evaluated", "tenant", tenant, "watcher", d.id, "code", p.Code, "detail", p.Detail)
		case err != nil:
			errs = append(errs, fmt.Errorf("analytics: evaluate metric watcher %s of tenant %s: %w", d.id, tenant, err))
		}
	}
	return errors.Join(errs...)
}

// active lists the tenant's active watchers with their creators.
func (w *Worker) active(ctx context.Context, tenant uuid.UUID) ([]due, error) {
	var list []due
	err := uow.Job{Begin: w.Begin, Scope: database.Scope{Role: w.AppRole, TenantID: tenant, ReadOnly: true}, Name: "analytics: watchers"}.Tx(ctx,
		func(ctx context.Context, tx pgx.Tx) (bool, error) {
			rows, _ := tx.Query(ctx, `SELECT id, created_by FROM analytics.metric_watcher WHERE status = 'active' ORDER BY id`)
			var err error
			list, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (due, error) {
				var d due
				return d, r.Scan(&d.id, &d.creator)
			})
			if err != nil {
				return false, fmt.Errorf("analytics: active metric watchers of tenant %s: %w", tenant, err)
			}
			return false, nil
		})
	return list, err
}

// creator is the watcher's creator as the evaluation acts as them,
// refused once no longer active.
func (w *Worker) creator(d due) func(ctx context.Context, tx pgx.Tx) (reqctx.Principal, bool, error) {
	return func(ctx context.Context, tx pgx.Tx) (reqctx.Principal, bool, error) {
		p, err := w.Actors.Actor(ctx, tx, d.creator)
		if err != nil {
			return reqctx.Principal{}, false, fmt.Errorf("analytics: the creator of metric watcher %s: %w", d.id, err)
		}
		if err := w.Actors.MayAct(ctx, tx, d.creator); err != nil {
			return reqctx.Principal{}, false, err
		}
		return p, true, nil
	}
}

func (w *Worker) log() *slog.Logger {
	if w.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return w.Log
}
