package payments_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestProposer(t *testing.T) {
	e := setup(t)
	op := e.propose(t, "2026-04-15", "2026-04-15")
	status := func() string {
		r, _ := e.Must(200, "GET", op["resource"].(string), "", "")
		return r["status"].(string)
	}

	bad := proposer(t)
	bad.WorkerRole = "no_such_role"
	if _, err := bad.RunOnce(context.Background()); err == nil {
		t.Fatal("found jobs without the worker role")
	}
	bad = proposer(t)
	bad.AppRole = "no_such_role"
	n, err := bad.RunOnce(context.Background())
	if err == nil {
		t.Error("without the app role: got no error, want one")
	}
	if n != 0 {
		t.Errorf("without the app role: got %d jobs done, want 0", n)
	}
	if got := status(); got != "proposing" {
		t.Fatalf("without the app role: got status %s, want proposing", got)
	}
	// Every statement of a round can fail, and so can its commit; the job
	// stays queued until one succeeds.
	pool := pgtest.Pool(t)
	lost := proposer(t)
	lost.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, s)
		if err != nil || s.TenantID == uuid.Nil {
			return tx, err
		}
		return &pgtest.FailingTx{Tx: tx, FailCommit: true}, nil
	}
	if _, err := lost.RunOnce(context.Background()); !errors.Is(err, pgtest.ErrInjected) {
		t.Fatalf("failed commit: got %v, want the injected failure", err)
	}
	if got := status(); got != "proposing" {
		t.Fatalf("after a failed commit: got status %s, want proposing", got)
	}
	for failAt := 1; ; failAt++ {
		w := proposer(t)
		w.Timeout, w.Now = time.Minute, time.Now
		w.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			if err != nil || s.TenantID == uuid.Nil {
				return tx, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: failAt}, nil
		}
		n, err := w.RunOnce(context.Background())
		if err == nil {
			if n != 1 {
				t.Errorf("after %d failures: got %d jobs done, want 1", failAt-1, n)
			}
			if got := status(); got != "proposed" {
				t.Errorf("after %d failures: got status %s, want proposed", failAt-1, got)
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: got %v, want the injected failure", failAt, err)
		}
		if got := status(); got != "proposing" {
			t.Fatalf("statement %d: got status %s, want proposing", failAt, got)
		}
		if failAt > 100 {
			t.Fatalf("statement %d: still failing after 100 statements", failAt)
		}
	}
	for name, begin := range map[string]func(context.Context, database.Scope) (pgx.Tx, error){
		"begin": func(context.Context, database.Scope) (pgx.Tx, error) { return nil, pgtest.ErrInjected },
		"find": func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			return &pgtest.FailingTx{Tx: tx, FailAt: 1}, err
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := proposer(t)
			w.Begin = begin
			if _, err := w.RunOnce(context.Background()); !errors.Is(err, pgtest.ErrInjected) {
				t.Errorf("got %v, want the injected failure", err)
			}
		})
	}
	second := e.propose(t, "2026-04-16", "2026-04-16")
	begun := 0
	w := proposer(t)
	w.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		if begun++; s.TenantID != uuid.Nil {
			return nil, pgtest.ErrInjected
		}
		return pool.Begin(ctx, s)
	}
	if _, err := w.RunOnce(context.Background()); !errors.Is(err, pgtest.ErrInjected) {
		t.Errorf("tenant transaction: got %v, want the injected failure", err)
	}
	if begun != 2 {
		t.Errorf("tenant transaction: got %d transactions begun, want 2", begun)
	}
	// The worker is shared by every test of the package and takes the jobs
	// of all tenants: finish this proposal here, or the next test's worker
	// would do it too and count it as its own.
	if n := work(t); n != 1 {
		t.Errorf("leftover proposal: got %d jobs done, want 1", n)
	}
	if r, _ := e.Must(200, "GET", second["resource"].(string), "", ""); r["status"] != "proposed" {
		t.Errorf("leftover proposal: got status %v, want proposed", r["status"])
	}

	// Run logs failed rounds and stops with its context.
	var logs pgtest.SyncBuffer
	failing := proposer(t)
	failing.WorkerRole, failing.Interval, failing.Log = "no_such_role", time.Millisecond, slog.New(slog.NewTextHandler(&logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { failing.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done }) // stops the worker should the wait fail
	pgtest.Eventually(t, 10*time.Second, func() bool { return strings.Contains(logs.String(), "payment proposal failed") },
		"logs: %s", &logs)
	cancel()
	<-done
	stopped, stop := context.WithCancel(context.Background())
	stop()
	proposer(t).Run(stopped)
}
