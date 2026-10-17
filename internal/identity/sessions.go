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
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
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

// sighting is when this replica last recorded a token's principal: a
// person's session and when it started, or a principal no session is kept
// for (a service, an agent).
type sighting struct {
	person           bool
	started, touched time.Time
}

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

// Check applies the tenant's session rule to a person's token, before the
// request's own transaction begins: it holds no connection while it waits
// for one (uow). It reads the token's principal itself; a principal no
// session is kept for, one not provisioned, and a person whose token fails
// the multi-factor rule are left to the unit of work, which answers them.
// A token of a deployment's own issuer carries no rule and is not checked.
func (s *Sessions) Check(ctx context.Context, c reqctx.Claims) error {
	rule := c.Session
	if rule.MaxAge <= 0 {
		return nil
	}
	key, keyed := sessionKey(c)
	now := clock.Now(s.Now)
	cacheKey := sha256.Sum256(append([]byte(c.TenantID.String()+"\n"+c.Issuer+"\n"+c.Subject+"\n"), key[:]...))
	if s.recent(cacheKey, now, rule) {
		return nil
	}
	seen, err := s.touch(ctx, c, key, keyed, now)
	if err != nil {
		return err
	}
	if seen != nil {
		s.remember(cacheKey, *seen)
	}
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

// recent reports a principal this replica recorded less than TouchEvery
// ago (and, for a person, less than the idle timeout ago) whose session is
// still within its lifetime.
func (s *Sessions) recent(key [32]byte, now time.Time, rule reqctx.SessionRule) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen, ok := s.seen[key]
	every := DefaultTouchEvery
	if s.TouchEvery > 0 {
		every = s.TouchEvery
	}
	since := now.Sub(seen.touched)
	return ok && since < every && (!seen.person || (since < rule.Idle && now.Sub(seen.started) < rule.MaxAge))
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

// Cached is how many principals this replica remembers.
func (s *Sessions) Cached() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

// touch records the sighting in a transaction of its own: it finds the
// token's principal, and for a person starts the session or ends it past
// its lifetime or idle timeout; an ending is committed before the request
// is refused. It returns what to remember, nil for nothing.
func (s *Sessions) touch(ctx context.Context, c reqctx.Claims, key [32]byte, keyed bool, now time.Time) (*sighting, error) {
	tx, err := database.BeginOr(ctx, s.Pool, s.Begin, database.Scope{Role: s.AppRole, TenantID: c.TenantID})
	if err != nil {
		return nil, fmt.Errorf("identity: session of %s: %w", c.Subject, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	p, err := sessionPrincipal(ctx, tx, c)
	switch {
	case err != nil, p.ID == uuid.Nil:
		return nil, err // a token of no principal is not remembered: the unit of work refuses it
	case p.Kind != Human:
		return &sighting{touched: now}, nil
	case authn.CheckMFA(c) != nil:
		// Refused by the unit of work's same check (uow.serve); remembering
		// it would let the same session's next token skip this one.
		return nil, nil
	}
	if err := s.personsToken(c, keyed); err != nil {
		return nil, err
	}
	row, found, err := s.lockOrStart(ctx, tx, p, key, c, now)
	if err != nil {
		return nil, err
	}
	answer := s.judge(row, found, now, c.Session)
	switch {
	case row.ended:
		return nil, answer
	case answer != nil:
		if err := s.end(ctx, tx, p, row, now, answer); err != nil {
			return nil, err
		}
	case found:
		if _, err := tx.Exec(ctx, `UPDATE identity.session SET last_seen_at = greatest(last_seen_at, $2), version = version + 1 WHERE id = $1`, row.id, now); err != nil {
			return nil, fmt.Errorf("identity: touch session %s: %w", row.id, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("identity: commit session %s: %w", row.id, err)
	}
	if answer != nil {
		return nil, answer
	}
	return &sighting{person: true, started: row.started, touched: now}, nil
}

// sessionPrincipal finds the token's active principal, as the unit of work
// resolves it (uow.resolve); a zero id when there is none. The tenant's
// status is the unit of work's to check: a suspended tenant's person may
// touch a session before being refused, which ends nothing early.
func sessionPrincipal(ctx context.Context, tx pgx.Tx, c reqctx.Claims) (reqctx.Principal, error) {
	p := reqctx.Principal{TenantID: c.TenantID}
	err := tx.QueryRow(ctx, `SELECT id, kind FROM identity.principal WHERE external_issuer = $1 AND external_subject = $2 AND status = 'active'`,
		c.Issuer, c.Subject).Scan(&p.ID, &p.Kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return reqctx.Principal{}, nil
	}
	if err != nil {
		return p, fmt.Errorf("identity: principal of session of %s: %w", c.Subject, err)
	}
	return p, nil
}

// personsToken refuses a person's token the session cannot be told by, or
// that outlives the session lifetime.
func (s *Sessions) personsToken(c reqctx.Claims, keyed bool) error {
	if !keyed {
		return sessionProblem("identity.session_unknown", "A person's token must carry auth_time or sid, which the session is tracked by.")
	}
	if !c.Expiry.IsZero() && c.Expiry.Sub(c.IssuedAt) > c.Session.MaxAge {
		return sessionProblem("identity.session_expired", "The token outlives the tenant's session lifetime.")
	}
	return nil
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
func (s *Sessions) lockOrStart(ctx context.Context, tx pgx.Tx, p reqctx.Principal, key [32]byte, c reqctx.Claims, now time.Time) (session, bool, error) {
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
	// The session starts at the authentication, never later than now (a
	// provider's clock running ahead must not lengthen it), or at its first
	// sighting when the token has no auth_time.
	start, authenticated := now, (*time.Time)(nil)
	if !c.AuthTime.IsZero() {
		start, authenticated = c.AuthTime, &c.AuthTime
		if start.After(now) {
			start = now
		}
	}
	if now.Sub(start) >= c.Session.MaxAge {
		return row, false, errExpired()
	}
	row, err = scanSession(tx.QueryRow(ctx, `INSERT INTO identity.session (tenant_id, id, principal_id, session_key, started_at, authenticated_at, last_seen_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (tenant_id, principal_id, session_key) DO NOTHING RETURNING `+sessionColumns,
		p.TenantID, uuid.Must(uuid.NewV7()), p.ID, key[:], start, authenticated, now))
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

// SessionSweeper ends, in rounds, the sessions idle longer than any tenant
// allows (MaxIdleTimeout), each audited in its tenant's trail, so a session
// nobody comes back to is recorded as ended too; and it deletes sessions
// that ended a week ago. A session keyed by sid alone is kept 90 days: a
// token refreshed at the provider carries the same sid and no auth_time to
// tell it is old, so the ended row is what refuses it.
type SessionSweeper struct {
	Pool       *database.Pool
	AppRole    string        // erp_app: sessions are ended in their tenant's scope
	WorkerRole string        // erp_worker: finds and deletes them across tenants
	Interval   time.Duration // between rounds; default one hour
	Batch      int           // sessions ended per round; 0: 500
	Now        func() time.Time
	Log        *slog.Logger
	Begin      database.BeginFunc
}

// Retention of ended sessions.
const (
	SessionRetention        = 7 * 24 * time.Hour
	SidOnlySessionRetention = 90 * 24 * time.Hour
)

// SweepActor is the actor the sweeper ends sessions as.
var SweepActor = uuid.MustParse("6f1d0a5e-3c2b-4a89-9e7d-5e55105e0e01")

// RunOnce ends idle sessions and deletes old ended ones; it returns how
// many it deleted. Running it twice changes nothing more.
func (s *SessionSweeper) RunOnce(ctx context.Context) (int64, error) {
	deleted, _, err := s.sweep(ctx)
	return deleted, err
}

// sweep is one round; more reports a full batch of idle sessions, so the
// next round runs at once. A tenant whose sessions cannot be ended is
// reported and the others' are ended still.
func (s *SessionSweeper) sweep(ctx context.Context) (int64, bool, error) {
	now := clock.Now(s.Now)
	idle, more, err := s.idle(ctx, now)
	if err != nil {
		return 0, false, err
	}
	var failed []error
	for tenant, ids := range idle {
		if err := s.endIdle(ctx, tenant, ids, now); err != nil {
			failed = append(failed, err)
		}
	}
	deleted, err := s.deleteEnded(ctx, now)
	return deleted, more, errors.Join(append(failed, err)...)
}

// deleteEnded deletes the sessions ended a week ago, or 90 days ago when
// keyed by sid alone.
func (s *SessionSweeper) deleteEnded(ctx context.Context, now time.Time) (int64, error) {
	tx, err := database.BeginOr(ctx, s.Pool, s.Begin, database.Scope{Role: s.WorkerRole})
	if err != nil {
		return 0, fmt.Errorf("identity: sweep sessions: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	tag, err := tx.Exec(ctx, `DELETE FROM identity.session WHERE ended_at < $1 AND (authenticated_at IS NOT NULL OR ended_at < $2)`,
		now.Add(-SessionRetention), now.Add(-SidOnlySessionRetention))
	if err != nil {
		return 0, fmt.Errorf("identity: sweep sessions: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("identity: commit the session sweep: %w", err)
	}
	return tag.RowsAffected(), nil
}

// idle finds sessions not ended and idle longer than any tenant allows, by
// tenant.
func (s *SessionSweeper) idle(ctx context.Context, now time.Time) (map[uuid.UUID][]uuid.UUID, bool, error) {
	tx, err := database.BeginOr(ctx, s.Pool, s.Begin, database.Scope{Role: s.WorkerRole, ReadOnly: true})
	if err != nil {
		return nil, false, fmt.Errorf("identity: find idle sessions: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	batch := 500
	if s.Batch > 0 {
		batch = s.Batch
	}
	// Every tenant's oldest idle session before any tenant's second: one
	// tenant with many, say ones that keep failing to end, cannot keep the
	// others' from being ended.
	rows, _ := tx.Query(ctx, `SELECT tenant_id, id FROM (
			SELECT tenant_id, id, last_seen_at, row_number() OVER (PARTITION BY tenant_id ORDER BY last_seen_at) AS turn
			FROM identity.session WHERE ended_at IS NULL AND last_seen_at < $1) s
		ORDER BY turn, last_seen_at LIMIT $2`, now.Add(-MaxIdleTimeout), batch)
	out := map[uuid.UUID][]uuid.UUID{}
	var tenant, id uuid.UUID
	found := 0
	if _, err := pgx.ForEachRow(rows, []any{&tenant, &id}, func() error {
		out[tenant] = append(out[tenant], id)
		found++
		return nil
	}); err != nil {
		return nil, false, fmt.Errorf("identity: find idle sessions: %w", err)
	}
	return out, found == batch, nil
}

// endIdle ends a tenant's idle sessions as idle, each audited.
func (s *SessionSweeper) endIdle(ctx context.Context, tenant uuid.UUID, ids []uuid.UUID, now time.Time) error {
	tx, err := database.BeginOr(ctx, s.Pool, s.Begin, database.Scope{Role: s.AppRole, TenantID: tenant})
	if err != nil {
		return fmt.Errorf("identity: end idle sessions of tenant %s: %w", tenant, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	rows, _ := tx.Query(ctx, `UPDATE identity.session SET ended_at = $2, end_reason = 'idle', version = version + 1
		WHERE id = ANY($1) AND ended_at IS NULL AND last_seen_at < $3 RETURNING id, started_at, last_seen_at`, ids, now, now.Add(-MaxIdleTimeout))
	var entries []audit.Entry
	var id uuid.UUID
	var started, lastSeen time.Time
	if _, err := pgx.ForEachRow(rows, []any{&id, &started, &lastSeen}, func() error {
		entries = append(entries, audit.Entry{Action: "identity.session.end", ObjectType: "identity.session", ObjectID: id, Outcome: "succeeded",
			Detail: map[string]any{"reason": "idle", "started_at": started, "last_seen_at": lastSeen, "swept": true}})
		return nil
	}); err != nil {
		return fmt.Errorf("identity: end idle sessions of tenant %s: %w", tenant, err)
	}
	if len(entries) > 0 { // another round ended them meanwhile otherwise
		if err := audit.Write(ctx, tx, reqctx.Principal{ID: SweepActor, Kind: reqctx.KindSystem, TenantID: tenant}, "", now, entries...); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("identity: commit the idle sessions of tenant %s: %w", tenant, err)
	}
	return nil
}

// Run sweeps until ctx ends, logging failures.
func (s *SessionSweeper) Run(ctx context.Context) {
	worker.Loop(ctx, s.Interval, time.Hour, func(ctx context.Context) (bool, error) {
		_, more, err := s.sweep(ctx)
		return more, err
	}, s.logger(), "session sweep failed")
}

func (s *SessionSweeper) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}
