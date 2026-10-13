package payments_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

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
	if n, err := bad.RunOnce(context.Background()); err == nil || n != 0 || status() != "proposing" {
		t.Fatalf("built without the app role: %d %v", n, err)
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
			if n != 1 || status() != "proposed" {
				t.Fatalf("after %d failures: %d %s", failAt-1, n, status())
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) || status() != "proposing" || failAt > 100 {
			t.Fatalf("statement %d: %v (%s)", failAt, err, status())
		}
	}
	for name, begin := range map[string]func(context.Context, database.Scope) (pgx.Tx, error){
		"begin": func(context.Context, database.Scope) (pgx.Tx, error) { return nil, pgtest.ErrInjected },
		"find": func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			return &pgtest.FailingTx{Tx: tx, FailAt: 1}, err
		},
	} {
		w := proposer(t)
		w.Begin = begin
		if _, err := w.RunOnce(context.Background()); !errors.Is(err, pgtest.ErrInjected) {
			t.Errorf("%s: %v", name, err)
		}
	}
	e.propose(t, "2026-04-16", "2026-04-16")
	begun := 0
	w := proposer(t)
	w.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		if begun++; s.TenantID != uuid.Nil {
			return nil, pgtest.ErrInjected
		}
		return pool.Begin(ctx, s)
	}
	if _, err := w.RunOnce(context.Background()); !errors.Is(err, pgtest.ErrInjected) || begun != 2 {
		t.Fatalf("tenant transaction: %v after %d", err, begun)
	}

	// Run logs failed rounds and stops with its context.
	var logs syncBuffer
	failing := proposer(t)
	failing.WorkerRole, failing.Interval, failing.Log = "no_such_role", time.Millisecond, slog.New(slog.NewTextHandler(&logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { failing.Run(ctx); close(done) }()
	pgtest.Eventually(t, 10*time.Second, func() bool { return strings.Contains(logs.String(), "payment proposal failed") },
		"logs: %s", &logs)
	cancel()
	<-done
	stopped, stop := context.WithCancel(context.Background())
	stop()
	proposer(t).Run(stopped)
}
