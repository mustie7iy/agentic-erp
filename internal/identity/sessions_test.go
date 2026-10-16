package identity_test

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
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
	return f.s.Check(context.Background(), c)
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
	go func() { errs <- first.Check(context.Background(), c) }()
	pid := <-reached
	go func() { errs <- f.s.Check(context.Background(), c) }()
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
			if err := (&identity.Sessions{Pool: pool, AppRole: "erp_app", Now: f.clock.Now}).Check(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			f.clock.Advance(2 * time.Minute)
			return c
		},
		"end": func() reqctx.Claims {
			c := f.claims("sid-" + uuid.NewString())
			if err := (&identity.Sessions{Pool: pool, AppRole: "erp_app", Now: f.clock.Now}).Check(context.Background(), c); err != nil {
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
				err := s.Check(context.Background(), c)
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
		pgtest.WantErr(t, s.Check(context.Background(), f.claims("sid-down")), "database down")
	})
	t.Run("a failed commit", func(t *testing.T) {
		s := &identity.Sessions{Pool: pool, AppRole: "erp_app", Now: f.clock.Now, Begin: func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, scope)
			return &pgtest.FailingTx{Tx: tx, FailCommit: true}, err
		}}
		if err := s.Check(context.Background(), f.claims("sid-commit")); !errors.Is(err, pgtest.ErrInjected) {
			t.Errorf("got %v, want the injected failure", err)
		}
	})
}

// sweeperAt is the sweeper as it runs at the clock's time.
func (f *sessionFixture) sweeperAt() *identity.SessionSweeper {
	return &identity.SessionSweeper{Pool: f.s.Pool, AppRole: "erp_app", WorkerRole: "erp_worker", Now: f.clock.Now, Log: slog.New(slog.DiscardHandler)}
}

func (f *sessionFixture) sessions() int {
	f.t.Helper()
	var n int
	pgtest.Scan(f.t, `SELECT count(*) FROM identity.session WHERE tenant_id = $1`, []any{f.ten.ID}, &n)
	return n
}

// TestSweeperEndsSessionsNobodyReturnsTo: a session idle longer than any
// tenant allows is ended by the sweeper, audited, and deleted a week
// later; running a round twice changes nothing more.
func TestSweeperEndsSessionsNobodyReturnsTo(t *testing.T) {
	f := newSessions(t)
	if err := f.check(f.claims("sid-silent")); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(identity.MaxIdleTimeout + time.Minute)
	for range 2 {
		if _, err := f.sweeperAt().RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if _, ended, reason := f.row("sid-silent"); ended == nil || reason == nil || *reason != "idle" {
		t.Errorf("session: got ended %v (%v), want ended idle", ended, reason)
	}
	if n := auditCount(t, f.ten.ID, "identity.session.end"); n != 1 {
		t.Errorf("session ends audited: got %d, want 1", n)
	}
	f.clock.Advance(identity.SessionRetention + time.Minute)
	if _, err := f.sweeperAt().RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.sessions(); n != 0 {
		t.Errorf("sessions a week after their end: got %d, want 0", n)
	}
}

// TestSidOnlySessionsStayEnded: a session keyed by sid alone ended stays
// recorded for 90 days, so a token refreshed at the provider, carrying the
// same sid and no auth_time, is still refused after a week.
func TestSidOnlySessionsStayEnded(t *testing.T) {
	f := newSessions(t)
	c := f.claims("sid-only")
	c.AuthTime = time.Time{}
	if err := f.check(c); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(11 * time.Minute)
	wantCode(t, f.check(c), "identity.session_idle")
	f.clock.Advance(identity.SessionRetention + time.Hour)
	if _, err := f.sweeperAt().RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	refreshed := f.claims("sid-only")
	refreshed.AuthTime = time.Time{}
	wantCode(t, f.check(refreshed), "identity.session_idle")
	f.clock.Advance(identity.SidOnlySessionRetention)
	if _, err := f.sweeperAt().RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.sessions(); n != 0 {
		t.Errorf("sessions after 90 days: got %d, want 0", n)
	}
}

// TestSessionsArePerPrincipal: two tenants' people signed in through one
// identity provider's session (one sid) have a session each; one ending
// never answers for the other, on the same replica.
func TestSessionsArePerPrincipal(t *testing.T) {
	f := newSessions(t)
	other := pgtest.NewTenant(t)
	pgtest.Exec(t, `UPDATE identity.principal SET external_subject = $2, version = version + 1 WHERE tenant_id = $1 AND id = $3`,
		other.ID, f.ten.Human.Subject, other.Human.ID)
	inA, inB := f.claims("sid-shared"), f.claims("sid-shared")
	inB.TenantID, inB.Session.Idle = other.ID, 5*time.Minute
	for _, c := range []reqctx.Claims{inA, inB} {
		if err := f.check(c); err != nil {
			t.Fatal(err)
		}
	}
	for range 6 { // A stays busy, B goes idle
		f.clock.Advance(time.Minute)
		if err := f.check(inA); err != nil {
			t.Fatal(err)
		}
	}
	wantCode(t, f.check(inB), "identity.session_idle")
	if err := f.check(inA); err != nil {
		t.Errorf("the other tenant's session: got %v, want it kept", err)
	}
}

// TestSessionsArePeoples: services, agents, principals not provisioned and
// people whose token fails the multi-factor rule start no session; the
// unit of work answers them.
func TestSessionsArePeoples(t *testing.T) {
	f := newSessions(t)
	service := f.claims("")
	service.Subject, service.AuthTime = f.ten.Service.Subject, time.Time{}
	unknown := f.claims("sid-unknown")
	unknown.Subject = "nobody"
	mfa := f.claims("sid-mfa")
	mfa.MFA = reqctx.MFA{Required: true, AMRValues: []string{"mfa"}}
	for name, c := range map[string]reqctx.Claims{"service": service, "unknown": unknown, "without multi-factor": mfa} {
		t.Run(name, func(t *testing.T) {
			if err := f.check(c); err != nil {
				t.Errorf("got %v, want no session check", err)
			}
		})
	}
	if n := f.sessions(); n != 0 {
		t.Errorf("sessions: got %d, want none", n)
	}
	if err := f.check(service); err != nil { // remembered as no person
		t.Errorf("the service again: %v", err)
	}
}

// TestSessionStartsNoLaterThanNow: an auth_time ahead of the cell's clock
// (the provider's clock running fast) does not lengthen the session.
func TestSessionStartsNoLaterThanNow(t *testing.T) {
	f := newSessions(t)
	c := f.claims("sid-ahead")
	c.AuthTime = f.clock.Now().Add(10 * time.Minute)
	if err := f.check(c); err != nil {
		t.Fatal(err)
	}
	f.s.TouchEvery = time.Second
	for range 12 {
		f.clock.Advance(5 * time.Minute)
		_ = f.check(c) // the hour runs out on the last
	}
	wantCode(t, f.check(c), "identity.session_expired")
}

// TestSessionSweeperFailures fails each of a round's transactions, chosen
// by what it runs as (the package database holds other tests' sessions, so
// counting transactions would not say which is which): its first
// statement, its commit, or its start.
func TestSessionSweeperFailures(t *testing.T) {
	f := newSessions(t)
	pool := f.s.Pool
	kinds := map[string]func(database.Scope) bool{
		"finding":  func(s database.Scope) bool { return s.Role == "erp_worker" && s.ReadOnly },
		"ending":   func(s database.Scope) bool { return s.Role == "erp_app" },
		"deleting": func(s database.Scope) bool { return s.Role == "erp_worker" && !s.ReadOnly },
	}
	for kind, is := range kinds {
		for name, fail := range map[string]func(pgx.Tx) (pgx.Tx, error){
			"statement": func(tx pgx.Tx) (pgx.Tx, error) { return &pgtest.FailingTx{Tx: tx, FailAt: 1}, nil },
			"commit":    func(tx pgx.Tx) (pgx.Tx, error) { return &pgtest.FailingTx{Tx: tx, FailCommit: true}, nil },
			"no transaction": func(tx pgx.Tx) (pgx.Tx, error) {
				_ = tx.Rollback(context.Background())
				return nil, errors.New("database down")
			},
			"audit": func(tx pgx.Tx) (pgx.Tx, error) { return &pgtest.FailingTx{Tx: tx, FailAt: 2}, nil },
		} {
			if (kind == "finding" && name == "commit") || (kind != "ending" && name == "audit") {
				continue // a read-only round commits nothing; only the ending audits
			}
			t.Run(kind+" "+name, func(t *testing.T) {
				if err := f.check(f.claims("sid-" + uuid.NewString())); err != nil {
					t.Fatal(err)
				}
				f.clock.Advance(identity.MaxIdleTimeout + time.Minute)
				sw := f.sweeperAt()
				sw.Batch = 1000
				sw.Begin = func(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
					tx, err := pool.Begin(ctx, scope)
					if err != nil || !is(scope) {
						return tx, err
					}
					return fail(tx)
				}
				_, err := sw.RunOnce(context.Background())
				if !errors.Is(err, pgtest.ErrInjected) && (err == nil || !strings.Contains(err.Error(), "database down")) {
					t.Errorf("got %v, want the injected failure", err)
				}
			})
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.sweeperAt().Run(ctx)
	(&identity.SessionSweeper{Pool: pool, AppRole: "erp_app", WorkerRole: "erp_worker"}).Run(ctx) // without a logger
}

// TestSessionSightingsOutOfOrder: a sighting older than the last one
// recorded (a request that read the clock first but locked the row last,
// or a replica whose clock is behind) leaves the last sighting as it is.
func TestSessionSightingsOutOfOrder(t *testing.T) {
	f := newSessions(t)
	c := f.claims("sid-order")
	if err := f.check(c); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(2 * time.Minute)
	if err := f.check(c); err != nil {
		t.Fatal(err)
	}
	behind := &identity.Sessions{Pool: f.s.Pool, AppRole: "erp_app", Now: func() time.Time { return f.clock.Now().Add(-time.Minute) }}
	if err := behind.Check(context.Background(), c); err != nil {
		t.Errorf("an earlier sighting: got %v, want accepted", err)
	}
}

// TestStepUpDoesNotReviveASession: a token of an ended session that fails
// the multi-factor rule is left to the unit of work, and remembers
// nothing: the same session's step-up token is still refused.
func TestStepUpDoesNotReviveASession(t *testing.T) {
	f := newSessions(t)
	c := f.claims("sid-step-up")
	if err := f.check(c); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(11 * time.Minute)
	wantCode(t, f.check(c), "identity.session_idle")
	weak := f.claims("sid-step-up")
	weak.MFA = reqctx.MFA{Required: true, AMRValues: []string{"mfa"}}
	if err := f.check(weak); err != nil {
		t.Fatalf("a token failing the multi-factor rule: got %v, want it left to the unit of work", err)
	}
	strong := weak
	strong.AMR = []string{"mfa"}
	wantCode(t, f.check(strong), "identity.session_idle")
}

// TestSweeperRunsAgainOnAFullBatch: a round that found a full batch of
// idle sessions runs again at once, until the batch is not full.
func TestSweeperRunsAgainOnAFullBatch(t *testing.T) {
	f := newSessions(t)
	for i := range 3 {
		if err := f.check(f.claims("sid-batch-" + strconv.Itoa(i))); err != nil {
			t.Fatal(err)
		}
	}
	f.clock.Advance(identity.MaxIdleTimeout + time.Minute)
	sw := f.sweeperAt()
	sw.Batch, sw.Interval = 1, time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sw.Run(ctx); close(done) }()
	pgtest.Eventually(t, 10*time.Second, func() bool {
		var n int
		pgtest.Scan(t, `SELECT count(*) FROM identity.session WHERE tenant_id = $1 AND ended_at IS NULL`, []any{f.ten.ID}, &n)
		return n == 0
	}, "a full batch did not make the sweeper run again before its hour")
	cancel()
	<-done
}

// TestSweeperIsFairToTenants: a round takes every tenant's oldest idle
// session before any tenant's second, so one tenant with many (say, ones
// that keep failing to end) cannot keep the others' from being ended.
func TestSweeperIsFairToTenants(t *testing.T) {
	past := time.Now().Add(-30 * 24 * time.Hour).Truncate(time.Second) // older than every other test's sessions
	busy, quiet := newSessions(t), newSessions(t)
	busy.clock.now, quiet.clock.now = past, past
	// Earlier runs' sessions from their own past are ended first, so this
	// round sees only this test's.
	clear := busy.sweeperAt()
	clear.Batch = 100000
	if _, err := clear.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if err := busy.check(busy.claims("sid-busy-" + strconv.Itoa(i))); err != nil {
			t.Fatal(err)
		}
	}
	quiet.clock.Advance(time.Minute) // its session is the newest of the four
	if err := quiet.check(quiet.claims("sid-quiet")); err != nil {
		t.Fatal(err)
	}
	quiet.clock.Advance(identity.MaxIdleTimeout + time.Hour)
	sw := quiet.sweeperAt()
	sw.Batch = 2
	if _, err := sw.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ended, _ := quiet.row("sid-quiet"); ended == nil {
		t.Error("the other tenant's idle session: got not ended, want ended in the first round")
	}
}
