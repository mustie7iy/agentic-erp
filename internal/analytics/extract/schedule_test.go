package extract_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/analytics/extract"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// scheduled is a tenant whose changes the relay marks, and its scheduler.
type scheduled struct {
	env
	s extract.Scheduler
}

func newScheduled(t *testing.T) scheduled {
	t.Helper()
	e := setup(t)
	// The scheduler claims any due tenant of the database; only this
	// package's tests queue any, and each removes its own.
	t.Cleanup(func() { pgtest.Exec(t, `DELETE FROM analytics.extract_due WHERE tenant_id = $1`, e.Tenant.ID) })
	return scheduled{env: e, s: extract.Scheduler{Extractor: e.x, Begin: pgtest.Pool(t).Begin, WorkerRole: "erp_worker", SweepEvery: -1,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
}

// mark marks the tenant changed as the relay does, in its role.
func (c scheduled) mark(t *testing.T, schema string) {
	t.Helper()
	err := pgtest.Pool(t).InTx(context.Background(), database.Scope{Role: "erp_relay"}, func(ctx context.Context, tx pgx.Tx) error {
		return extract.MarkChanged(ctx, tx, schema, []uuid.UUID{c.Tenant.ID, c.Tenant.ID})
	})
	if err != nil {
		t.Fatal(err)
	}
}

// due is the tenant's queue row: when it was claimed and finished.
func (c scheduled) due(t *testing.T) (started, done *time.Time) {
	t.Helper()
	pgtest.Scan(t, `SELECT started_at, done_at FROM analytics.extract_due WHERE tenant_id = $1`, []any{c.Tenant.ID}, &started, &done)
	return started, done
}

func (c scheduled) runOnce(t *testing.T) {
	t.Helper()
	if _, err := c.s.RunOnce(context.Background()); err != nil {
		t.Fatalf("round: %v", err)
	}
}

// aged moves the tenant's queue row back by the scheduler's interval, as if
// five minutes had passed.
func (c scheduled) aged(t *testing.T) {
	t.Helper()
	pgtest.Exec(t, `UPDATE analytics.extract_due SET changed_at = changed_at - interval '5 minutes', started_at = started_at - interval '5 minutes',
		done_at = done_at - interval '5 minutes' WHERE tenant_id = $1`, c.Tenant.ID)
}

func TestSchedulerRunsChangedTenants(t *testing.T) {
	c := newScheduled(t)
	c.post(t, "2026-03-02", "1.00")
	c.mark(t, "identity") // no dataset reads it
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM analytics.extract_due WHERE tenant_id = $1`, []any{c.Tenant.ID}, &n)
	if n != 0 {
		t.Fatalf("marks of an unread context: got %d, want 0", n)
	}
	c.mark(t, "ledger")
	pool := pgtest.Pool(t)
	err := pool.InTx(context.Background(), database.Scope{Role: "erp_relay"}, func(ctx context.Context, tx pgx.Tx) error {
		return extract.MarkChanged(ctx, &pgtest.FailingTx{Tx: tx, FailAt: 1}, "ledger", []uuid.UUID{c.Tenant.ID})
	})
	pgtest.WantErr(t, err, "mark 1 tenants changed")
	c.runOnce(t)
	started, done := c.due(t)
	if started == nil || done == nil || !done.Equal(*started) {
		t.Fatalf("queue: got started %v done %v, want the run recorded", started, done)
	}
	if c.checkedAt(t).IsZero() {
		t.Fatal("pointer: got none, want the run's")
	}

	t.Run("run twice", func(t *testing.T) {
		c.runOnce(t)
		if again, _ := c.due(t); !again.Equal(*started) {
			t.Errorf("claim: got %v, want %v kept", again, started)
		}
	})
	t.Run("at most once per interval", func(t *testing.T) {
		c.mark(t, "payables")
		c.runOnce(t)
		if again, _ := c.due(t); !again.Equal(*started) {
			t.Errorf("claim within the interval: got %v, want %v kept", again, started)
		}
		c.aged(t)
		c.runOnce(t)
		if again, done := c.due(t); !again.After(*started) || !done.Equal(*again) {
			t.Errorf("claim after the interval: got %v done %v, want a new run after %v", again, done, started)
		}
	})
}

// TestSchedulerRetriesAFailedRun: a run that fails leaves the tenant due,
// and the next claim after the interval runs it again.
func TestSchedulerRetriesAFailedRun(t *testing.T) {
	c := newScheduled(t)
	c.post(t, "2026-03-02", "1.00")
	c.s.Extractor.Store = failingStore{Store: c.server.Store(), method: "put", part: "gl_lines"}
	c.mark(t, "ledger")
	if _, err := c.s.RunOnce(context.Background()); !errors.Is(err, blobstore.ErrLocked) {
		t.Fatalf("round: got %v, want the store's failure", err)
	}
	if _, done := c.due(t); done != nil {
		t.Fatalf("done: got %v, want none", done)
	}
	c.s.Extractor.Store = c.server.Store()
	c.aged(t)
	c.runOnce(t)
	if _, done := c.due(t); done == nil {
		t.Error("done after the retry: got none, want the run recorded")
	}
}

func TestSchedulerDatabaseFailures(t *testing.T) {
	c := newScheduled(t)
	c.post(t, "2026-03-02", "1.00")
	pool := pgtest.Pool(t)
	for n := 1; ; n++ {
		c.mark(t, "ledger")
		count := 0
		s := c.s
		s.Begin = func(ctx context.Context, sc database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, sc)
			return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, err
		}
		_, err := s.RunOnce(context.Background())
		if err == nil {
			if n < 4 {
				t.Fatalf("statement %d: round succeeded, want every statement covered", n)
			}
			return
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: got %v, want the injected failure", n, err)
		}
		c.aged(t)
	}
}

// TestSchedulerRun: the loop runs rounds until its context ends.
func TestSchedulerRun(t *testing.T) {
	c := newScheduled(t)
	c.post(t, "2026-03-02", "1.00")
	c.mark(t, "ledger")
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	c.s.Interval, c.s.Every = 10*time.Millisecond, time.Minute
	go func() { c.s.Run(ctx); close(stopped) }()
	pgtest.Eventually(t, time.Minute, func() bool { _, done := c.due(t); return done != nil }, "tenant %s not extracted", c.Tenant.ID)
	cancel()
	<-stopped
}

// TestSchedulerClaims: what a claim and its done cover.
func TestSchedulerClaims(t *testing.T) {
	c := newScheduled(t)
	c.post(t, "2026-03-02", "1.00")
	c.mark(t, "ledger")
	c.runOnce(t)

	t.Run("nothing marked since", func(t *testing.T) {
		c.aged(t)
		before, _ := c.due(t)
		c.runOnce(t)
		if again, _ := c.due(t); !again.Equal(*before) {
			t.Errorf("claim: got %v, want none after %v", again, before)
		}
	})
	t.Run("marked after the claim", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE analytics.extract_due SET changed_at = started_at + interval '1 second' WHERE tenant_id = $1`, c.Tenant.ID)
		c.aged(t)
		before, _ := c.due(t)
		c.runOnce(t)
		if again, _ := c.due(t); !again.After(*before) {
			t.Errorf("claim: got %v, want a new one after %v", again, before)
		}
	})
	t.Run("claim taken over during the run", func(t *testing.T) {
		c.post(t, "2026-03-03", "1.00")
		c.mark(t, "ledger")
		c.aged(t)
		_, doneBefore := c.due(t)
		s := c.s
		primary := s.Extractor.Primary
		s.Extractor.Primary = func(ctx context.Context, sc database.Scope) (pgx.Tx, error) {
			pgtest.Exec(t, `UPDATE analytics.extract_due SET started_at = started_at + interval '1 second' WHERE tenant_id = $1`, c.Tenant.ID)
			return primary(ctx, sc)
		}
		if _, err := s.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, done := c.due(t); !done.Equal(*doneBefore) {
			t.Errorf("done: got %v, want %v kept for the claim that took over", done, doneBefore)
		}
	})
}

// TestSchedulerPendingStaysDue: a run that leaves entries above its cutoff
// does not record the change done.
func TestSchedulerPendingStaysDue(t *testing.T) {
	c := newScheduled(t)
	c.post(t, "2026-03-02", "1.00")
	c.runOnce(t) // no mark: nothing due
	ctx := context.Background()
	hold, err := pgtest.Admin(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hold.Exec(ctx, `SELECT pg_current_xact_id()`); err != nil {
		t.Fatal(err)
	}
	c.postOnly(t, "2026-03-03", "2.00")
	c.mark(t, "ledger")
	c.runOnce(t)
	if started, done := c.due(t); started == nil || done != nil {
		t.Errorf("queue: got started %v done %v, want claimed and not done", started, done)
	}
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestSchedulersClaimOnce: two replicas' rounds run a due tenant once.
func TestSchedulersClaimOnce(t *testing.T) {
	c := newScheduled(t)
	c.post(t, "2026-03-02", "1.00")
	c.mark(t, "ledger")
	var runs atomic.Int32
	s := c.s
	replica := s.Extractor.Replica
	s.Extractor.Replica = func(ctx context.Context, sc database.Scope) (pgx.Tx, error) {
		if sc.TenantID == c.Tenant.ID {
			runs.Add(1)
		}
		return replica(ctx, sc)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() { defer wg.Done(); _, errs[i] = s.RunOnce(context.Background()) }()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("round %d: %v", i, err)
		}
	}
	if n := runs.Load(); n != 1 {
		t.Errorf("runs of the tenant: got %d, want 1", n)
	}
}

// TestMarksTheActualsChanged: a batch of the payments context's events (a
// statement imported, a line matched or posted) or of the configuration's
// (derivation rules, house banks) marks its tenant due, since either
// changes the cash actuals; the cash context's own events (memo records,
// snapshots, comments) change no dataset.
func TestMarksTheActualsChanged(t *testing.T) {
	c := newScheduled(t)
	for schema, want := range map[string]int{"cash": 0, "payments": 1, "config": 1} {
		t.Run(schema, func(t *testing.T) {
			pgtest.Exec(t, `DELETE FROM analytics.extract_due WHERE tenant_id = $1`, c.Tenant.ID)
			c.mark(t, schema)
			var n int
			pgtest.Scan(t, `SELECT count(*) FROM analytics.extract_due WHERE tenant_id = $1`, []any{c.Tenant.ID}, &n)
			if n != want {
				t.Errorf("marks: got %d, want %d", n, want)
			}
		})
	}
}
