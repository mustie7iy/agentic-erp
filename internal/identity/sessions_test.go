package identity_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// sessionClock is a settable clock.
type sessionClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *sessionClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *sessionClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// sessionFixture is a tenant's person, a clock and the sessions.
type sessionFixture struct {
	t      *testing.T
	ten    pgtest.Tenant
	clock  *sessionClock
	s      *identity.Sessions
	person reqctx.Principal
}

func newSessions(t *testing.T) *sessionFixture {
	t.Helper()
	ten := pgtest.NewTenant(t)
	c := &sessionClock{now: time.Now().Truncate(time.Second)}
	return &sessionFixture{t: t, ten: ten, clock: c, s: &identity.Sessions{Pool: pgtest.Pool(t), AppRole: "erp_app", Now: c.Now},
		person: reqctx.Principal{ID: ten.Human.ID, Kind: reqctx.KindHuman, TenantID: ten.ID}}
}

// claims is a person's token in a session with sid, under a policy of an
// hour's lifetime and ten minutes idle.
func (f *sessionFixture) claims(sid string) reqctx.Claims {
	now := f.clock.Now()
	return reqctx.Claims{Issuer: pgtest.Issuer(f.t), Subject: f.ten.Human.Subject, TenantID: f.ten.ID, SessionID: sid,
		AuthTime: now, IssuedAt: now, Expiry: now.Add(5 * time.Minute),
		Session: reqctx.SessionRule{MaxAge: time.Hour, Idle: 10 * time.Minute}}
}

func (f *sessionFixture) check(c reqctx.Claims) error {
	return f.s.Check(context.Background(), c, f.person)
}

func (f *sessionFixture) row(sid string) (lastSeen time.Time, ended *time.Time, reason *string) {
	f.t.Helper()
	pgtest.Scan(f.t, `SELECT s.last_seen_at, s.ended_at, s.end_reason FROM identity.session s
		WHERE s.tenant_id = $1 AND s.principal_id = $2 ORDER BY s.created_at DESC LIMIT 1`, []any{f.ten.ID, f.ten.Human.ID}, &lastSeen, &ended, &reason)
	_ = sid // one session per test case: the latest is the one
	return lastSeen, ended, reason
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	p := problem.From(err)
	if p.Status != 401 || p.Code != code {
		t.Errorf("got %d %s (%v), want 401 %s", p.Status, p.Code, err, code)
	}
}

func auditCount(t *testing.T, tenant uuid.UUID, action string) int {
	t.Helper()
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = $2`, []any{tenant, action}, &n)
	return n
}

func TestSessionIdleTimeout(t *testing.T) {
	f := newSessions(t)
	c := f.claims("sid-idle")
	if err := f.check(c); err != nil {
		t.Fatalf("first sighting: %v", err)
	}
	first, _, _ := f.row("sid-idle")
	t.Run("started and audited", func(t *testing.T) {
		if n := auditCount(t, f.ten.ID, "identity.session.start"); n != 1 {
			t.Errorf("session starts audited: got %d, want 1", n)
		}
	})
	t.Run("within a minute the database is not touched", func(t *testing.T) {
		f.clock.Advance(30 * time.Second)
		if err := f.check(c); err != nil {
			t.Fatal(err)
		}
		if seen, _, _ := f.row("sid-idle"); !seen.Equal(first) {
			t.Errorf("last seen: got %v, want %v kept", seen, first)
		}
	})
	t.Run("after it the sighting is recorded", func(t *testing.T) {
		f.clock.Advance(time.Minute)
		if err := f.check(c); err != nil {
			t.Fatal(err)
		}
		if seen, _, _ := f.row("sid-idle"); !seen.After(first) {
			t.Errorf("last seen: got %v, want after %v", seen, first)
		}
	})
	t.Run("idle past the timeout it ends", func(t *testing.T) {
		f.clock.Advance(11 * time.Minute)
		wantCode(t, f.check(c), "identity.session_idle")
		_, ended, reason := f.row("sid-idle")
		if ended == nil || reason == nil || *reason != "idle" {
			t.Errorf("session: got ended %v (%v), want ended idle", ended, reason)
		}
		if n := auditCount(t, f.ten.ID, "identity.session.end"); n != 1 {
			t.Errorf("session ends audited: got %d, want 1", n)
		}
	})
	t.Run("and never resumes, even with a refreshed token", func(t *testing.T) {
		refreshed := f.claims("sid-idle")
		wantCode(t, f.check(refreshed), "identity.session_idle")
	})
}

func TestSessionLifetime(t *testing.T) {
	f := newSessions(t)
	c := f.claims("sid-life")
	for range 11 { // 55 minutes, never idle
		if err := f.check(c); err != nil {
			t.Fatalf("within the lifetime: %v", err)
		}
		f.clock.Advance(5 * time.Minute)
	}
	if err := f.check(c); err != nil {
		t.Fatalf("at 55 minutes: %v", err)
	}
	f.clock.Advance(5*time.Minute + time.Second)
	wantCode(t, f.check(c), "identity.session_expired")
	if _, _, reason := f.row("sid-life"); reason == nil || *reason != "expired" {
		t.Errorf("end reason: got %v, want expired", reason)
	}
	f.s.TouchEvery = time.Second // the cache would answer within the minute otherwise
	wantCode(t, f.check(c), "identity.session_expired")
	t.Run("a cached session past its lifetime is checked again", func(t *testing.T) {
		g := newSessions(t)
		d := g.claims("sid-cached")
		if err := g.check(d); err != nil {
			t.Fatal(err)
		}
		g.clock.Advance(time.Hour + time.Second)
		wantCode(t, g.check(d), "identity.session_expired")
	})
}

func TestSessionsRefused(t *testing.T) {
	f := newSessions(t)
	t.Run("an authentication older than the lifetime", func(t *testing.T) {
		c := f.claims("sid-old")
		c.AuthTime = f.clock.Now().Add(-2 * time.Hour)
		wantCode(t, f.check(c), "identity.session_expired")
	})
	t.Run("a token outliving the session lifetime", func(t *testing.T) {
		c := f.claims("sid-long")
		c.Expiry = c.IssuedAt.Add(2 * time.Hour)
		wantCode(t, f.check(c), "identity.session_expired")
	})
	t.Run("neither sid nor auth_time", func(t *testing.T) {
		c := f.claims("")
		c.AuthTime = time.Time{}
		wantCode(t, f.check(c), "identity.session_unknown")
	})
	t.Run("no session rule", func(t *testing.T) {
		c := f.claims("")
		c.AuthTime, c.Session = time.Time{}, reqctx.SessionRule{}
		if err := f.check(c); err != nil {
			t.Errorf("a token of a deployment issuer: got %v, want no session check", err)
		}
	})
}

// TestSessionsByAuthTime: without a sid, a session is the subject's
// authentication at auth_time; another authentication is another session.
func TestSessionsByAuthTime(t *testing.T) {
	f := newSessions(t)
	c := f.claims("")
	if err := f.check(c); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(11 * time.Minute)
	wantCode(t, f.check(c), "identity.session_idle")
	again := f.claims("")
	if err := f.check(again); err != nil {
		t.Errorf("a new authentication: got %v, want a new session", err)
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM identity.session WHERE tenant_id = $1`, []any{f.ten.ID}, &n)
	if n != 2 {
		t.Errorf("sessions: got %d, want 2", n)
	}
}

// heldCommit is a transaction whose commit waits for release, after
// telling its backend's pid on reached.
type heldCommit struct {
	pgx.Tx
	reached chan<- int32
	release <-chan struct{}
	pid     int32
}

func (h *heldCommit) Commit(ctx context.Context) error {
	h.reached <- h.pid
	<-h.release
	return h.Tx.Commit(ctx)
}

// TestSessionFirstSightingsAtOnce: two first sightings of a session at
// once record one session; the second waits for the first's and reads it.
func TestSessionFirstSightingsAtOnce(t *testing.T) {
	f := newSessions(t)
	c := f.claims("sid-race")
	reached, release := make(chan int32, 1), make(chan struct{})
	first := &identity.Sessions{Pool: f.s.Pool, AppRole: "erp_app", Now: f.clock.Now, Begin: func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
		tx, err := f.s.Pool.Begin(ctx, scope)
		if err != nil {
			return nil, err
		}
		var pid int32
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			return nil, err
		}
		return &heldCommit{Tx: tx, reached: reached, release: release, pid: pid}, nil
	}}
	errs := make(chan error, 2)
	go func() { errs <- first.Check(context.Background(), c, f.person) }()
	pid := <-reached
	go func() { errs <- f.s.Check(context.Background(), c, f.person) }()
	pgtest.Eventually(t, 10*time.Second, func() bool {
		var n int
		pgtest.Scan(t, `SELECT count(*) FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))`, []any{pid}, &n)
		return n > 0
	}, "the second sighting does not wait for the first")
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Errorf("first sighting: %v", err)
		}
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM identity.session WHERE tenant_id = $1`, []any{f.ten.ID}, &n)
	if n != 1 {
		t.Errorf("sessions: got %d, want 1", n)
	}
}

func TestSessionsCacheBound(t *testing.T) {
	f := newSessions(t)
	f.s.MaxCached = 2
	for _, sid := range []string{"a", "b", "c"} {
		if err := f.check(f.claims(sid)); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.s.Cached(); n > 2 {
		t.Errorf("cached sessions: got %d, want at most 2", n)
	}
}

// TestSessionDatabaseFailures: every statement of a sighting failing in
// turn is reported; a failed start leaves no session.
func TestSessionDatabaseFailures(t *testing.T) {
	f := newSessions(t)
	pool := f.s.Pool
	started := f.claims("sid-failing-ended")
	if err := f.check(started); err != nil {
		t.Fatal(err)
	}
	for name, setup := range map[string]func() reqctx.Claims{
		"start": func() reqctx.Claims { return f.claims("sid-" + uuid.NewString()) },
		"touch": func() reqctx.Claims {
			c := f.claims("sid-" + uuid.NewString())
			if err := (&identity.Sessions{Pool: pool, AppRole: "erp_app", Now: f.clock.Now}).Check(context.Background(), c, f.person); err != nil {
				t.Fatal(err)
			}
			f.clock.Advance(2 * time.Minute)
			return c
		},
		"end": func() reqctx.Claims {
			c := f.claims("sid-" + uuid.NewString())
			if err := (&identity.Sessions{Pool: pool, AppRole: "erp_app", Now: f.clock.Now}).Check(context.Background(), c, f.person); err != nil {
				t.Fatal(err)
			}
			f.clock.Advance(11 * time.Minute)
			return c
		},
	} {
		t.Run(name, func(t *testing.T) {
			for n := 1; n < 20; n++ {
				c := setup()
				s := &identity.Sessions{Pool: pool, AppRole: "erp_app", Now: f.clock.Now, Begin: func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
					tx, err := pool.Begin(ctx, scope)
					return &pgtest.FailingTx{Tx: tx, FailAt: n}, err
				}}
				err := s.Check(context.Background(), c, f.person)
				if !errors.Is(err, pgtest.ErrInjected) {
					if p := problem.From(err); err != nil && p.Status == 401 {
						return // every statement before the answer has failed once
					}
					if err == nil {
						return
					}
					t.Fatalf("statement %d: got %v, want the injected failure", n, err)
				}
			}
		})
	}
	t.Run("no transaction", func(t *testing.T) {
		s := &identity.Sessions{Pool: pool, AppRole: "erp_app", Now: f.clock.Now, Begin: func(context.Context, database.Scope) (pgx.Tx, error) {
			return nil, errors.New("database down")
		}}
		pgtest.WantErr(t, s.Check(context.Background(), f.claims("sid-down"), f.person), "database down")
	})
	t.Run("a failed commit", func(t *testing.T) {
		s := &identity.Sessions{Pool: pool, AppRole: "erp_app", Now: f.clock.Now, Begin: func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, scope)
			return &pgtest.FailingTx{Tx: tx, FailCommit: true}, err
		}}
		if err := s.Check(context.Background(), f.claims("sid-commit"), f.person); !errors.Is(err, pgtest.ErrInjected) {
			t.Errorf("got %v, want the injected failure", err)
		}
	})
}

func TestSessionSweeper(t *testing.T) {
	f := newSessions(t)
	fresh, stale := f.claims("sid-fresh"), f.claims("sid-stale")
	for _, c := range []reqctx.Claims{fresh, stale} {
		if err := f.check(c); err != nil {
			t.Fatal(err)
		}
	}
	pgtest.Exec(t, `UPDATE identity.session SET last_seen_at = now() - interval '8 days', version = version + 1
		WHERE tenant_id = $1 AND session_key = (SELECT session_key FROM identity.session WHERE tenant_id = $1 ORDER BY created_at DESC LIMIT 1)`, f.ten.ID)
	sw := &identity.SessionSweeper{Pool: f.s.Pool, WorkerRole: "erp_worker", Log: slog.New(slog.DiscardHandler)}
	for round := range 2 {
		if _, err := sw.RunOnce(context.Background()); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		var n int
		pgtest.Scan(t, `SELECT count(*) FROM identity.session WHERE tenant_id = $1`, []any{f.ten.ID}, &n)
		if n != 1 {
			t.Errorf("round %d: sessions left: got %d, want the fresh one", round, n)
		}
	}
	failing := &identity.SessionSweeper{Pool: f.s.Pool, WorkerRole: "erp_worker", Begin: func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
		tx, err := f.s.Pool.Begin(ctx, scope)
		return &pgtest.FailingTx{Tx: tx, FailAt: 1}, err
	}}
	if _, err := failing.RunOnce(context.Background()); !errors.Is(err, pgtest.ErrInjected) {
		t.Errorf("failing round: got %v, want the injected failure", err)
	}
	lost := &identity.SessionSweeper{Pool: f.s.Pool, WorkerRole: "erp_worker", Begin: func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
		tx, err := f.s.Pool.Begin(ctx, scope)
		return &pgtest.FailingTx{Tx: tx, FailCommit: true}, err
	}}
	if _, err := lost.RunOnce(context.Background()); !errors.Is(err, pgtest.ErrInjected) {
		t.Errorf("a failed commit: got %v, want the injected failure", err)
	}
	quiet := &identity.SessionSweeper{Pool: f.s.Pool, WorkerRole: "erp_worker"}
	stopped, stop := context.WithCancel(context.Background())
	stop()
	quiet.Run(stopped) // without a logger
	down := &identity.SessionSweeper{Pool: f.s.Pool, WorkerRole: "erp_worker", Begin: func(context.Context, database.Scope) (pgx.Tx, error) {
		return nil, errors.New("database down")
	}}
	if _, err := down.RunOnce(context.Background()); err == nil {
		t.Error("no transaction: got success, want the failure")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sw.Run(ctx) // one round, then the stop
}
