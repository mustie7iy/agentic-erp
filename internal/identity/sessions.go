package identity

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/worker"
)

// People's sessions (ADR-0031). The API takes bearer tokens, so a session
// is one authentication at the identity provider: its sid, or the
// subject's auth_time. Its lifetime runs from auth_time (or its first
// sighting), its idle time from the last request, kept in identity.session.
// A sighting is recorded in a transaction of its own, since the request's
// may be read-only or rolled back, and at most once a minute per replica:
// the idle timeout is at least five minutes, so a skipped sighting hides at
// most a minute of idleness. An ended session never resumes, so a token
// refreshed at the provider cannot revive it; the person signs in again.

// The defaults of Sessions.
const (
	DefaultTouchEvery     = time.Minute
	DefaultCachedSessions = 100000
	SessionRetention      = 7 * 24 * time.Hour
)

// Sessions checks people's sessions in the unit of work.
type Sessions struct {
	Pool       *database.Pool
	AppRole    string
	Now        func() time.Time // nil: the wall clock
	TouchEvery time.Duration    // 0: DefaultTouchEvery
	MaxCached  int              // 0: DefaultCachedSessions
	// Begin starts the transaction; nil means Pool.Begin. Tests inject
	// failures.
	Begin database.BeginFunc

	mu   sync.Mutex
	seen map[[32]byte]sighting
}

// sighting is when this replica last recorded a session, and when the
// session started.
type sighting struct{ started, touched time.Time }

// session is a row of identity.session.
type session struct {
	id                uuid.UUID
	started, lastSeen time.Time
	ended             bool
	reason            string
}

func sessionProblem(code, detail string) *problem.Problem {
	p := problem.New(http.StatusUnauthorized, code, "Session ended", detail)
	p.Header = http.Header{"Www-Authenticate": {`Bearer error="invalid_token", error_description="` + detail + `"`}}
	return p
}

var (
	errExpired = func() error {
		return sessionProblem("identity.session_expired", "The session has outlived the tenant's session lifetime; sign in again.")
	}
	errIdle = func() error {
		return sessionProblem("identity.session_idle", "The session was idle longer than the tenant allows; sign in again.")
	}
)

// Check applies the tenant's session rule to a person's token. A token of
// a deployment's own issuer carries no rule and is not checked.
func (s *Sessions) Check(ctx context.Context, c reqctx.Claims, p reqctx.Principal) error {
	rule := c.Session
	if rule.MaxAge <= 0 {
		return nil
	}
	if !c.Expiry.IsZero() && c.Expiry.Sub(c.IssuedAt) > rule.MaxAge {
		return sessionProblem("identity.session_expired", "The token outlives the tenant's session lifetime.")
	}
	key, ok := sessionKey(c)
	if !ok {
		return sessionProblem("identity.session_unknown", "A person's token must carry auth_time or sid, which the session is tracked by.")
	}
	now := clock.Now(s.Now)
	start := now
	if !c.AuthTime.IsZero() {
		start = c.AuthTime
	}
	if s.recent(key, now, rule) {
		return nil
	}
	started, err := s.touch(ctx, p, key, start, now, rule)
	if err != nil {
		return err
	}
	s.remember(key, sighting{started: started, touched: now})
	return nil
}

// sessionKey identifies the authentication: the issuer's sid, or the
// issuer's subject at auth_time.
func sessionKey(c reqctx.Claims) ([32]byte, bool) {
	switch {
	case c.SessionID != "":
		return sha256.Sum256([]byte(c.Issuer + "\nsid\n" + c.SessionID)), true
	case !c.AuthTime.IsZero():
		return sha256.Sum256([]byte(c.Issuer + "\nsub\n" + c.Subject + "\n" + strconv.FormatInt(c.AuthTime.Unix(), 10))), true
	}
	return [32]byte{}, false
}

// recent reports a session this replica recorded less than TouchEvery ago
// that is still within its lifetime.
func (s *Sessions) recent(key [32]byte, now time.Time, rule reqctx.SessionRule) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen, ok := s.seen[key]
	every := DefaultTouchEvery
	if s.TouchEvery > 0 {
		every = s.TouchEvery
	}
	return ok && now.Sub(seen.touched) < every && now.Sub(seen.started) < rule.MaxAge
}

func (s *Sessions) remember(key [32]byte, seen sighting) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := DefaultCachedSessions
	if s.MaxCached > 0 {
		limit = s.MaxCached
	}
	if s.seen == nil || len(s.seen) >= limit {
		s.seen = map[[32]byte]sighting{}
	}
	s.seen[key] = seen
}

// Cached is how many sessions this replica remembers.
func (s *Sessions) Cached() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

// touch records the sighting in a transaction of its own, starting the
// session or ending it past its lifetime or idle timeout; an ending is
// committed before the request is refused.
func (s *Sessions) touch(ctx context.Context, p reqctx.Principal, key [32]byte, start, now time.Time, rule reqctx.SessionRule) (time.Time, error) {
	tx, err := database.BeginOr(ctx, s.Pool, s.Begin, database.Scope{Role: s.AppRole, TenantID: p.TenantID})
	if err != nil {
		return time.Time{}, fmt.Errorf("identity: session of principal %s: %w", p.ID, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	row, found, err := s.lockOrStart(ctx, tx, p, key, start, now, rule)
	if err != nil {
		return time.Time{}, err
	}
	answer := s.judge(row, found, now, rule)
	switch {
	case row.ended:
		return time.Time{}, answer
	case answer != nil:
		if err := s.end(ctx, tx, p, row, now, answer); err != nil {
			return time.Time{}, err
		}
	case found:
		if _, err := tx.Exec(ctx, `UPDATE identity.session SET last_seen_at = $2, version = version + 1 WHERE id = $1`, row.id, now); err != nil {
			return time.Time{}, fmt.Errorf("identity: touch session %s: %w", row.id, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return time.Time{}, fmt.Errorf("identity: commit session %s: %w", row.id, err)
	}
	return row.started, answer
}

// judge is the answer for a session: its end's, nil while it lasts.
func (s *Sessions) judge(row session, found bool, now time.Time, rule reqctx.SessionRule) error {
	switch {
	case row.ended && row.reason == "idle":
		return errIdle()
	case row.ended:
		return errExpired()
	case !found:
		return nil
	case now.Sub(row.started) >= rule.MaxAge:
		return errExpired()
	case now.Sub(row.lastSeen) >= rule.Idle:
		return errIdle()
	}
	return nil
}

const sessionColumns = `id, started_at, last_seen_at, ended_at IS NOT NULL, coalesce(end_reason, '')`

func scanSession(row pgx.Row) (session, error) {
	var r session
	err := row.Scan(&r.id, &r.started, &r.lastSeen, &r.ended, &r.reason)
	return r, err //nolint:wrapcheck // callers wrap with the principal
}

// lockOrStart reads the session FOR UPDATE, or starts it (found false),
// recording the start in the audit trail; an authentication already older
// than the lifetime starts nothing and is refused. Two first sightings at
// once start it once: the second reads the first's row.
func (s *Sessions) lockOrStart(ctx context.Context, tx pgx.Tx, p reqctx.Principal, key [32]byte, start, now time.Time, rule reqctx.SessionRule) (session, bool, error) {
	read := func() (session, error) {
		return scanSession(tx.QueryRow(ctx, `SELECT `+sessionColumns+` FROM identity.session WHERE principal_id = $1 AND session_key = $2 FOR UPDATE`, p.ID, key[:]))
	}
	row, err := read()
	if err == nil {
		return row, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return row, false, readFailed(p, err)
	}
	if now.Sub(start) >= rule.MaxAge {
		return row, false, errExpired()
	}
	row, err = scanSession(tx.QueryRow(ctx, `INSERT INTO identity.session (tenant_id, id, principal_id, session_key, started_at, last_seen_at)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (tenant_id, principal_id, session_key) DO NOTHING RETURNING `+sessionColumns,
		p.TenantID, uuid.Must(uuid.NewV7()), p.ID, key[:], start, now))
	if errors.Is(err, pgx.ErrNoRows) { // started by a concurrent first sighting
		row, err = read()
		return row, err == nil, readFailed(p, err)
	}
	if err != nil {
		return row, false, fmt.Errorf("identity: start session of principal %s: %w", p.ID, err)
	}
	return row, false, s.audit(ctx, tx, p, now, "start", row.id, nil)
}

// readFailed wraps a failed read of a principal's session; nil for nil.
func readFailed(p reqctx.Principal, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("identity: read session of principal %s: %w", p.ID, err)
}

// end ends the session for the answer's reason and audits it.
func (s *Sessions) end(ctx context.Context, tx pgx.Tx, p reqctx.Principal, row session, now time.Time, answer error) error {
	reason := "expired"
	if problem.From(answer).Code == "identity.session_idle" {
		reason = "idle"
	}
	if _, err := tx.Exec(ctx, `UPDATE identity.session SET ended_at = $2, end_reason = $3, version = version + 1 WHERE id = $1`, row.id, now, reason); err != nil {
		return fmt.Errorf("identity: end session %s: %w", row.id, err)
	}
	return s.audit(ctx, tx, p, now, "end", row.id, map[string]any{"reason": reason, "started_at": row.started, "last_seen_at": row.lastSeen})
}

func (s *Sessions) audit(ctx context.Context, tx pgx.Tx, p reqctx.Principal, now time.Time, verb string, id uuid.UUID, detail map[string]any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	return audit.Write(ctx, tx, p, reqctx.TraceID(ctx), now, audit.Entry{Action: "identity.session." + verb, ObjectType: "identity.session", ObjectID: id, Outcome: "succeeded", Detail: detail})
}

// SessionSweeper deletes the sessions that ended, or went idle, more than
// SessionRetention ago, across tenants, in rounds.
type SessionSweeper struct {
	Pool       *database.Pool
	WorkerRole string        // erp_worker
	Interval   time.Duration // between rounds; default one hour
	Log        *slog.Logger
	Begin      database.BeginFunc
}

// RunOnce deletes the old sessions and returns how many. Running it twice
// deletes nothing more.
func (s *SessionSweeper) RunOnce(ctx context.Context) (int64, error) {
	tx, err := database.BeginOr(ctx, s.Pool, s.Begin, database.Scope{Role: s.WorkerRole})
	if err != nil {
		return 0, fmt.Errorf("identity: sweep sessions: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	tag, err := tx.Exec(ctx, `DELETE FROM identity.session WHERE coalesce(ended_at, last_seen_at) < now() - make_interval(secs => $1)`, SessionRetention.Seconds())
	if err != nil {
		return 0, fmt.Errorf("identity: sweep sessions: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("identity: commit the session sweep: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Run sweeps until ctx ends, logging failures.
func (s *SessionSweeper) Run(ctx context.Context) {
	worker.Loop(ctx, s.Interval, time.Hour, func(ctx context.Context) (bool, error) {
		_, err := s.RunOnce(ctx)
		return false, err
	}, s.logger(), "session sweep failed")
}

func (s *SessionSweeper) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}
