package watchers_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/analytics/extract"
	"github.com/mustie7iy/agentic-erp/internal/analytics/watchers"
	"github.com/mustie7iy/agentic-erp/internal/analytics/wire"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// worker evaluates watchers over q, as erp-server's worker does.
func worker(t *testing.T, q watchers.Queries) *watchers.Worker {
	t.Helper()
	return &watchers.Worker{Watchers: endpoints(t, q), Begin: pgtest.Pool(t).Begin, AppRole: "erp_app", Actors: identity.Actors{}}
}

// TestWorker: after a refresh, every active watcher is evaluated as its
// creator, once per period however often the worker runs; a removed one
// is not; a creator no longer active, or refused the data, is passed
// over; a failing answer fails the round.
func TestWorker(t *testing.T) {
	e := setup(t, alertPerms...)
	ctx := context.Background()
	active := watch(t, e.Client, `{"name": "Active", "metric": "revenue", "grain": "month", "condition": {"kind": "above", "value": "1", "currency": "EUR"}}`)
	removed := watch(t, e.Client, `{"name": "Removed", "metric": "revenue", "grain": "month", "condition": {"kind": "above", "value": "1", "currency": "EUR"}}`)
	e.Must(200, "POST", "/v1/metric-watchers/"+apitest.String(t, removed, "id")+":remove", "", etag(removed))
	w := worker(t, queries{m: model(t), res: march()})
	for range 2 {
		if err := w.Refreshed(ctx, e.Tenant.ID); err != nil {
			t.Fatalf("refreshed: %v", err)
		}
	}
	if a, n := e.alerts(t, active); a != 1 || n != 1 {
		t.Errorf("the active watcher's alerts and events after two rounds: got %d and %d, want one of each", a, n)
	}
	if a, _ := e.alerts(t, removed); a != 0 {
		t.Errorf("the removed watcher's alerts: got %d, want none", a)
	}
	var raisedAs uuid.UUID
	pgtest.Scan(t, `SELECT created_by FROM analytics.metric_alert WHERE tenant_id = $1`, []any{e.Tenant.ID}, &raisedAs)
	if raisedAs != e.Tenant.Human.ID {
		t.Errorf("raised as: got %s, want the creator", raisedAs)
	}
	t.Run("refused", func(t *testing.T) {
		if err := worker(t, queries{m: model(t), err: errors.New("down")}).Refreshed(ctx, e.Tenant.ID); err == nil || !strings.Contains(err.Error(), "down") {
			t.Errorf("a failing answer: got %v, want it", err)
		}
		reader := e.Reader(t, "Bea Blocked", e.DE)
		watch(t, e.As(reader), `{"name": "Blocked", "metric": "revenue", "grain": "year", "condition": {"kind": "above", "value": "1", "currency": "EUR"}}`)
		pgtest.Exec(t, `UPDATE identity.principal SET status = 'blocked', version = version + 1 WHERE id = $1`, reader.ID)
		if err := w.Refreshed(ctx, e.Tenant.ID); err != nil {
			t.Errorf("a creator blocked, no log: got %v, want the watcher passed over", err)
		}
		var logged bytes.Buffer
		w := *w
		w.Log = slog.New(slog.NewTextHandler(&logged, nil))
		if err := w.Refreshed(ctx, e.Tenant.ID); err != nil {
			t.Errorf("a creator blocked: got %v, want the watcher passed over", err)
		}
		if !strings.Contains(logged.String(), "identity.principal_blocked") {
			t.Errorf("log: got %q, want the creator's refusal", logged.String())
		}
		w.Actors = noActors{}
		if err := w.Refreshed(ctx, e.Tenant.ID); err == nil || !strings.Contains(err.Error(), "the creator of metric watcher") {
			t.Errorf("no creator: got %v, want the failure", err)
		}
	})
	t.Run("without the operation's permissions", func(t *testing.T) {
		later := watch(t, e.Client, `{"name": "Later", "metric": "revenue", "grain": "year", "condition": {"kind": "above", "value": "1", "currency": "EUR"}}`)
		e.Tenant.Revoke(t, "analytics.query.run", e.Tenant.Human)
		if err := w.Refreshed(ctx, e.Tenant.ID); err != nil {
			t.Errorf("a creator without analytics.query.run: got %v, want the watcher passed over", err)
		}
		if a, _ := e.alerts(t, later); a != 0 {
			t.Errorf("alerts of a creator without analytics.query.run: got %d, want none", a)
		}
	})
	t.Run("no list", func(t *testing.T) {
		down := errors.New("down")
		w := *w
		w.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, down }
		if err := w.Refreshed(ctx, e.Tenant.ID); !errors.Is(err, down) {
			t.Errorf("no transaction: got %v, want %v", err, down)
		}
		w.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pgtest.Pool(t).Begin(ctx, s)
			return &pgtest.FailingTx{Tx: tx, FailAt: 1}, err
		}
		pgtest.WantErr(t, w.Refreshed(ctx, e.Tenant.ID), "active metric watchers")
	})
}

// noActors reads no principal.
type noActors struct{ identity.Actors }

func (noActors) Actor(context.Context, pgx.Tx, uuid.UUID) (reqctx.Principal, error) {
	return reqctx.Principal{}, errors.New("no principal")
}

// storeReader answers every plan with 2000's March revenue, credits as the
// store keeps them, negative.
type storeReader struct{}

func (storeReader) Query(_ context.Context, req wire.Request) (*wire.Result, error) {
	return &wire.Result{ManifestKey: req.ManifestKey, AsOf: time.Now(), DataThrough: "2026-03-15",
		Rows: []wire.Row{{By: []string{"2000", "2026", "3"}, Currency: "EUR", Exponent: 2, Amount: -50000}}}, nil
}

// TestWorkerFromTheStore: erp-server's worker answers from the analytical
// store as the creator; without a store that may answer, it reads no API
// and fails the round, leaving the watcher for the next.
func TestWorkerFromTheStore(t *testing.T) {
	e := setup(t, alertPerms...)
	ctx := context.Background()
	w := watch(t, e.Client, `{"name": "Store", "metric": "revenue", "company": "2000", "grain": "month", "condition": {"kind": "above", "value": "400", "currency": "EUR"}}`)
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	cfg := app.Config{Pool: pgtest.Pool(t), AppRole: "erp_app", Pages: pages, Now: func() time.Time { return today }}
	if err := app.WatchEvaluator(cfg).Refreshed(ctx, e.Tenant.ID); err == nil || !strings.Contains(err.Error(), "analytical store alone") {
		t.Fatalf("without a store: got %v, want the API refused", err)
	}
	if a, _ := e.alerts(t, w); a != 0 {
		t.Fatalf("without a store: got %d alerts, want none", a)
	}
	pgtest.Exec(t, `INSERT INTO analytics.snapshot (tenant_id, manifest_key, watermark, as_of, checked_at) VALUES ($1, $2, '1'::xid8, now(), now())`,
		e.Tenant.ID, extract.ManifestKey(e.Tenant.ID, 1, "0123456789abcdef"))
	cfg.Analytics = storeReader{}
	e.Tenant.SetStatus(t, "suspended")
	if err := app.WatchEvaluator(cfg).Refreshed(ctx, e.Tenant.ID); err != nil {
		t.Fatalf("a suspended tenant: got %v, want the round ended", err)
	}
	if a, _ := e.alerts(t, w); a != 0 {
		t.Fatalf("a suspended tenant: got %d alerts, want none", a)
	}
	e.Tenant.SetStatus(t, "active")
	if err := app.WatchEvaluator(cfg).Refreshed(ctx, e.Tenant.ID); err != nil {
		t.Fatalf("from the store: %v", err)
	}
	var findings string
	pgtest.Scan(t, `SELECT findings::text FROM analytics.metric_alert WHERE tenant_id = $1`, []any{e.Tenant.ID}, &findings)
	if want := `[{"figure": "500.00", "company": "2000", "currency": "EUR", "compared_with": "400"}]`; findings != want {
		t.Errorf("findings: got %s, want %s", findings, want)
	}
}
