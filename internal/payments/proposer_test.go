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

// TestProposer: the worker needs its roles; every statement of a round and
// its commit can fail, and the job stays queued until a round succeeds; a
// failing begin, listing or tenant transaction is reported; Run logs failed
// rounds and stops with its context.
func TestProposer(t *testing.T) {
	s := &proposerScenario{e: setup(t), pool: pgtest.Pool(t)}
	s.op = s.e.propose(t, "2026-04-15", "2026-04-15")
	// The first steps work the one proposal in turn, and the worker is
	// shared by every test of the package, so the first failure stops the
	// rest rather than leave work queued for the next step.
	if !t.Run("a missing role finds or does nothing", s.missingRoles) {
		t.FailNow()
	}
	if !t.Run("the job stays queued until a round succeeds", s.failingRounds) {
		t.FailNow()
	}
	if !t.Run("a failing begin or listing is reported", s.beginAndFind) {
		t.FailNow()
	}
	if !t.Run("a failing tenant transaction is reported", s.tenantTransaction) {
		t.FailNow()
	}
	t.Run("Run logs failed rounds and stops with its context", proposerRunLoop)
}

// proposerScenario is what the steps of TestProposer share: the tenant,
// the pool the failure steps wrap, and the proposal the first steps work.
type proposerScenario struct {
	e    env
	pool *database.Pool
	op   map[string]any
}

// status reads the run of the proposal op asked for. The ETag that Must
// also returns is what a following write would send, and none follows.
func (s *proposerScenario) status(t *testing.T, op map[string]any) string {
	t.Helper()
	path, ok := op["resource"].(string)
	if !ok {
		t.Fatalf("proposal resource: got %v, want the run's path", op["resource"])
	}
	r, _ := s.e.in(t).Must(200, "GET", path, "", "")
	got, ok := r["status"].(string)
	if !ok {
		t.Fatalf("run status: got %v, want a string", r["status"])
	}
	return got
}

// missingRoles: without its worker role the worker finds no jobs; without
// its app role it does none, and the run stays proposing.
func (s *proposerScenario) missingRoles(t *testing.T) {
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
	if got := s.status(t, s.op); got != "proposing" {
		t.Fatalf("without the app role: got status %s, want proposing", got)
	}
}

// failingRounds: a failing commit, then every statement of a round failing
// in turn, leaves the job queued and the run proposing, until the round
// that fails nothing proposes it.
func (s *proposerScenario) failingRounds(t *testing.T) {
	pool := s.pool
	lost := proposer(t)
	lost.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, scope)
		if err != nil || scope.TenantID == uuid.Nil {
			return tx, err
		}
		return &pgtest.FailingTx{Tx: tx, FailCommit: true}, nil
	}
	if _, err := lost.RunOnce(context.Background()); !errors.Is(err, pgtest.ErrInjected) {
		t.Fatalf("failed commit: got %v, want the injected failure", err)
	}
	if got := s.status(t, s.op); got != "proposing" {
		t.Fatalf("after a failed commit: got status %s, want proposing", got)
	}
	for failAt := 1; ; failAt++ {
		w := proposer(t)
		w.Timeout, w.Now = time.Minute, time.Now
		w.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, scope)
			if err != nil || scope.TenantID == uuid.Nil {
				return tx, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: failAt}, nil
		}
		n, err := w.RunOnce(context.Background())
		if err == nil {
			if n != 1 {
				t.Errorf("after %d failures: got %d jobs done, want 1", failAt-1, n)
			}
			if got := s.status(t, s.op); got != "proposed" {
				t.Errorf("after %d failures: got status %s, want proposed", failAt-1, got)
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: got %v, want the injected failure", failAt, err)
		}
		if got := s.status(t, s.op); got != "proposing" {
			t.Fatalf("statement %d: got status %s, want proposing", failAt, got)
		}
		if failAt > 100 {
			t.Fatalf("statement %d: still failing after 100 statements", failAt)
		}
	}
}

// beginAndFind: a round whose begin or whose listing of the jobs fails
// reports it.
func (s *proposerScenario) beginAndFind(t *testing.T) {
	pool := s.pool
	for name, begin := range map[string]func(context.Context, database.Scope) (pgx.Tx, error){
		"begin": func(context.Context, database.Scope) (pgx.Tx, error) { return nil, pgtest.ErrInjected },
		"find": func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, scope)
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
}

// tenantTransaction: a round whose tenant transaction cannot begin reports
// it after the listing's. The proposal it leaves is finished here: the
// worker is shared by every test of the package and takes the jobs of all
// tenants, so the next test's worker would do it too and count it as its
// own.
func (s *proposerScenario) tenantTransaction(t *testing.T) {
	e, pool := s.e.in(t), s.pool
	second := e.propose(t, "2026-04-16", "2026-04-16")
	begun := 0
	w := proposer(t)
	w.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
		if begun++; scope.TenantID != uuid.Nil {
			return nil, pgtest.ErrInjected
		}
		return pool.Begin(ctx, scope)
	}
	if _, err := w.RunOnce(context.Background()); !errors.Is(err, pgtest.ErrInjected) {
		t.Errorf("tenant transaction: got %v, want the injected failure", err)
	}
	if begun != 2 {
		t.Errorf("tenant transaction: got %d transactions begun, want 2", begun)
	}
	if n := work(t); n != 1 {
		t.Errorf("leftover proposal: got %d jobs done, want 1", n)
	}
	if got := s.status(t, second); got != "proposed" {
		t.Errorf("leftover proposal: got status %v, want proposed", got)
	}
}

// proposerRunLoop: Run logs failed rounds and stops with its context, whether it
// is cancelled while it runs or before it starts.
func proposerRunLoop(t *testing.T) {
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
