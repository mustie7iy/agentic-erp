// Package session is erp-web's store of signed-in people (ADR-0052 §6,
// T73b): server-side sessions in the web schema of the global directory's
// database, which erp-web reaches as a login of its own working as the
// erp_web role, seeing that schema alone (directory.WebSchema, T72h), and
// the web clients it signs people in with, found by their login code. A
// session holds who signed in and the tokens the provider issued, sealed
// (Sealer); the browser holds only its random id, of which the store
// keeps the SHA-256. Nothing here is business data, and nothing is
// exported or fingerprinted. The store is the one package of the
// workspace that holds a database pool (internal/layout).
package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
)

// Role is the database role erp-web's transactions run as.
const Role = "erp_web"

// MaxConns is the store's pool per replica: no transaction of the store
// waits on anything but the database (a renewal calls the provider holding
// none), so a few serve many requests.
const MaxConns = 10

// The store's own limits: the longest any tenant may set (ADR-0031), so
// the cell's own, stricter rule is what people meet (its 401 ends the
// session here too), and how often a request records that it was seen.
const (
	DefaultMaxAge     = 24 * time.Hour
	DefaultIdle       = 8 * time.Hour
	DefaultTouchEvery = time.Minute
)

// ErrNoSession means there is no session with the id: never made, signed
// out, past its lifetime or idle too long (then deleted).
var ErrNoSession = errors.New("session: no session")

// ErrChanged means the session changed since it was read.
var ErrChanged = errors.New("session: the session changed meanwhile")

// Session is a signed-in person.
type Session struct {
	// ID is the session's id, known from the cookie; the store keeps its
	// hash alone.
	ID                         string
	TenantID                   uuid.UUID
	Issuer, Subject, LoginCode string
	Created, LastSeen, Expires time.Time
	Version                    int32
	Secrets
}

// Secrets are a session's sealed part.
type Secrets struct {
	AccessToken  string    `json:"access_token"`
	AccessExpiry time.Time `json:"access_expiry"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	IDToken      string    `json:"id_token"`
	// CSRF is the synchronizer token every state-changing form carries.
	CSRF string `json:"csrf"`
	// SteppedUp is when the person last signed in again for a step-up
	// challenge (RFC 9470); a second challenge right after it is refused
	// rather than looped.
	SteppedUp time.Time `json:"stepped_up,omitzero"`
}

// Store keeps sessions.
type Store struct {
	Pool       *database.Pool
	Sealer     *Sealer
	Now        func() time.Time // nil: the wall clock
	MaxAge     time.Duration    // 0: DefaultMaxAge
	Idle       time.Duration    // 0: DefaultIdle
	TouchEvery time.Duration    // 0: DefaultTouchEvery
	RenewFor   time.Duration    // a renewal's claim; 0: RenewClaim
	RenewEvery time.Duration    // how often a waiting request looks again; 0: RenewPoll
	// InTx runs a transaction; nil: Pool.InTx. Tests inject failures.
	InTx func(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error
}

func (st *Store) now() time.Time { return clock.Now(st.Now) }

func or(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// querier is what the store's statements use of their transaction.
// Declared here, its errors leave the package through inTx alone, which
// wraps each once with what the store was doing.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// inTx runs fn in a transaction as erp_web and wraps its error, or the
// transaction's, as "session: <what>: ..."; the store's sentinel errors
// (ErrNoSession, ErrChanged) stay matchable with errors.Is.
func (st *Store) inTx(ctx context.Context, readOnly bool, what string, fn func(context.Context, querier) error) error {
	scope := database.Scope{Role: Role, ReadOnly: readOnly}
	run := func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, tx) }
	var err error
	if st.InTx != nil {
		err = st.InTx(ctx, scope, run)
	} else {
		err = st.Pool.InTx(ctx, scope, run)
	}
	if err != nil {
		return fmt.Errorf("session: %s: %w", what, err)
	}
	return nil
}

// Hash is what the store keeps of a session id.
func Hash(id string) []byte {
	sum := sha256.Sum256([]byte(id))
	return sum[:]
}

// NewSecret is a fresh 256-bit random value, URL-safe: a session id or a
// CSRF token.
func NewSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error: it crashes the program instead (Go 1.24+)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (st *Store) seal(hash []byte, sec Secrets) []byte {
	plain, _ := json.Marshal(sec) // #nosec G117 -- marshalled to be sealed at once, never kept or sent in the clear; strings and times always encode
	return st.Sealer.Seal(hash, plain)
}

func (st *Store) open(hash, sealed []byte) (Secrets, error) {
	plain, err := st.Sealer.Open(hash, sealed)
	if err != nil {
		return Secrets{}, err
	}
	var sec Secrets
	if err := json.Unmarshal(plain, &sec); err != nil {
		return Secrets{}, fmt.Errorf("%w: %w", ErrUnsealed, err)
	}
	return sec, nil
}

// Create starts a session with a fresh random id and CSRF token, never
// one the browser sent (no fixation), lasting MaxAge at most.
func (st *Store) Create(ctx context.Context, s Session) (Session, error) {
	now := st.now()
	s.ID, s.CSRF = NewSecret(), NewSecret()
	s.Created, s.LastSeen, s.Expires, s.Version = now, now, now.Add(or(st.MaxAge, DefaultMaxAge)), 1
	hash := Hash(s.ID)
	err := st.inTx(ctx, false, "create", func(ctx context.Context, tx querier) error {
		_, err := tx.Exec(ctx, `INSERT INTO web.session (id_hash, tenant_id, issuer, subject, login_code, created_at, last_seen_at, expires_at, sealed)
			VALUES ($1, $2, $3, $4, $5, $6, $6, $7, $8)`, hash, s.TenantID, s.Issuer, s.Subject, s.LoginCode, now, s.Expires, st.seal(hash, s.Secrets))
		return err
	})
	if err != nil {
		return Session{}, err
	}
	return s, nil
}

const columns = `tenant_id, issuer, subject, login_code, created_at, last_seen_at, expires_at, version, sealed`

func (st *Store) scan(row pgx.Row, id string, more ...any) (Session, error) {
	s := Session{ID: id}
	var sealed []byte
	if err := row.Scan(append([]any{&s.TenantID, &s.Issuer, &s.Subject, &s.LoginCode, &s.Created, &s.LastSeen, &s.Expires, &s.Version, &sealed}, more...)...); err != nil {
		return s, fmt.Errorf("read the session: %w", err)
	}
	var err error
	s.Secrets, err = st.open(Hash(id), sealed)
	return s, err
}

// Load reads the session with the id. One past its lifetime or idle
// longer than Idle is deleted and is ErrNoSession, as is one whose secrets
// no longer open (a retired sealing key). A session not seen for
// TouchEvery is recorded as seen now.
func (st *Store) Load(ctx context.Context, id string) (Session, error) {
	if id == "" {
		return Session{}, ErrNoSession
	}
	now, hash := st.now(), Hash(id)
	var s Session
	ended := false
	err := st.inTx(ctx, false, "load", func(ctx context.Context, tx querier) error {
		var err error
		s, err = st.scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM web.session WHERE id_hash = $1`, hash), id)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrNoSession
		case errors.Is(err, ErrUnsealed), err == nil && (!now.Before(s.Expires) || now.Sub(s.LastSeen) >= or(st.Idle, DefaultIdle)):
			// Deleted, and the deletion committed, before the answer.
			ended = true
			_, err = tx.Exec(ctx, `DELETE FROM web.session WHERE id_hash = $1`, hash)
		case err == nil && now.Sub(s.LastSeen) >= or(st.TouchEvery, DefaultTouchEvery):
			s.LastSeen = now
			_, err = tx.Exec(ctx, `UPDATE web.session SET last_seen_at = $2 WHERE id_hash = $1 AND last_seen_at < $2`, hash, now)
		}
		return err
	})
	switch {
	case errors.Is(err, ErrNoSession):
		return Session{}, ErrNoSession
	case err != nil:
		return Session{}, err
	case ended:
		return Session{}, ErrNoSession
	}
	return s, nil
}

// The bounds of a renewal: a claim lasts RenewClaim (the provider's call,
// bounded by its caller, must end well within it), and a request waiting
// for another's renewal looks again every RenewPoll for at most twice the
// claim.
const (
	RenewClaim = 30 * time.Second
	RenewPoll  = 100 * time.Millisecond
)

// ErrRenewing means another request's renewal of the session did not end
// in time.
var ErrRenewing = errors.New("session: the session's renewal did not end in time")

// Renew replaces the session's secrets with those renew makes from its
// current ones, once for all concurrent requests of the session on any
// replica (ADR-0052 §5: a refresh token is used once, so it must never be
// spent twice). The first claims the renewal (renewing_until), renews
// holding no transaction or connection, so a slow provider stalls no one
// else's requests, and writes the result back if the session did not
// change meanwhile; the others wait and take the renewed secrets, for
// which fresh reports true. A claim whose holder died expires after
// RenewClaim. renew should be bounded by its context, within RenewClaim.
func (st *Store) Renew(ctx context.Context, s Session, fresh func(Secrets) bool, renew func(context.Context, Secrets) (Secrets, error)) (Session, error) {
	giveUp := time.NewTimer(2 * or(st.RenewFor, RenewClaim))
	defer giveUp.Stop()
	for {
		cur, claim, err := st.claimRenewal(ctx, s.ID)
		switch {
		case err != nil:
			return Session{}, err
		case fresh(cur.Secrets):
			if claim != nil { // given up even when the client has gone
				return cur, st.release(context.WithoutCancel(ctx), s.ID, *claim)
			}
			return cur, nil
		case claim != nil:
			renewed, err := st.renewClaimed(ctx, cur, *claim, renew)
			if !errors.Is(err, ErrChanged) {
				return renewed, err
			}
			// Another request renewed it after this one's claim lapsed:
			// its secrets are the session's now.
		}
		select {
		case <-ctx.Done():
			return Session{}, fmt.Errorf("session: wait for a renewal: %w", ctx.Err())
		case <-giveUp.C:
			return Session{}, ErrRenewing
		case <-time.After(or(st.RenewEvery, RenewPoll)):
		}
	}
}

// claimRenewal reads the session and claims its renewal unless another
// request holds an unexpired claim. claim is this request's claim (its
// expiry, by the database's one clock, which owns it), nil when another
// holds one.
func (st *Store) claimRenewal(ctx context.Context, id string) (Session, *time.Time, error) {
	hash := Hash(id)
	var s Session
	var claim *time.Time
	err := st.inTx(ctx, false, "renew", func(ctx context.Context, tx querier) error {
		var until time.Time
		var err error
		s, err = st.scan(tx.QueryRow(ctx, `UPDATE web.session SET renewing_until = clock_timestamp() + make_interval(secs => $2)
			WHERE id_hash = $1 AND (renewing_until IS NULL OR renewing_until <= clock_timestamp()) RETURNING `+columns+`, renewing_until`,
			hash, or(st.RenewFor, RenewClaim).Seconds()), id, &until)
		if err == nil {
			claim = &until
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		s, err = st.scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM web.session WHERE id_hash = $1`, hash), id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoSession
		}
		return err
	})
	if err != nil {
		return Session{}, nil, err
	}
	return s, claim, nil
}

// renewClaimed renews a session whose renewal this request claimed, and
// writes the result back while the claim is still this request's and the
// session unchanged (ErrChanged otherwise). The provider's answer and its
// write outlive the request (a client gone does not lose a rotated
// refresh token); a renewal that failed gives the claim up.
func (st *Store) renewClaimed(ctx context.Context, s Session, claim time.Time, renew func(context.Context, Secrets) (Secrets, error)) (Session, error) {
	ctx = context.WithoutCancel(ctx)
	sec, err := renew(ctx, s.Secrets)
	if err != nil {
		err = fmt.Errorf("session: renew: %w", err) // the provider's: the store's own are wrapped by inTx
	} else {
		err = st.writeRenewed(ctx, s, claim, sec)
	}
	switch {
	case errors.Is(err, ErrChanged):
		return Session{}, err
	case err != nil:
		if rerr := st.release(ctx, s.ID, claim); rerr != nil {
			return Session{}, errors.Join(err, rerr)
		}
		return Session{}, err
	}
	s.Secrets, s.Version = sec, s.Version+1
	return s, nil
}

// writeRenewed stores renewed secrets under the request's claim. A holder
// whose claim lapsed would lose what it renewed; the provider's call
// (renewTimeout, 15 s) and this write (writeTimeout) end within the claim
// (RenewClaim, 30 s), so only a process stalled for longer can.
func (st *Store) writeRenewed(ctx context.Context, s Session, claim time.Time, sec Secrets) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	hash := Hash(s.ID)
	return st.inTx(ctx, false, "write a renewal", func(ctx context.Context, tx querier) error {
		tag, err := tx.Exec(ctx, `UPDATE web.session SET sealed = $3, version = version + 1, renewing_until = NULL
			WHERE id_hash = $1 AND version = $2 AND renewing_until = $4`, hash, s.Version, st.seal(hash, sec), claim)
		if err == nil && tag.RowsAffected() == 0 {
			return ErrChanged
		}
		return err
	})
}

// writeTimeout bounds writing a renewal back once the provider answered.
const writeTimeout = 5 * time.Second

// release gives this request's renewal claim up, and no newer one.
func (st *Store) release(ctx context.Context, id string, claim time.Time) error {
	return st.inTx(ctx, false, "release a renewal", func(ctx context.Context, tx querier) error {
		_, err := tx.Exec(ctx, `UPDATE web.session SET renewing_until = NULL WHERE id_hash = $1 AND renewing_until = $2`, Hash(id), claim)
		return err
	})
}

// Delete ends the session with the id; one already gone is no error.
func (st *Store) Delete(ctx context.Context, id string) error {
	return st.inTx(ctx, false, "delete", func(ctx context.Context, tx querier) error {
		_, err := tx.Exec(ctx, `DELETE FROM web.session WHERE id_hash = $1`, Hash(id))
		return err
	})
}

// Sweep deletes the sessions past their lifetime or idle longer than Idle,
// which nobody came back to end; it returns how many. Running it twice
// deletes nothing more.
func (st *Store) Sweep(ctx context.Context) (int64, error) {
	now := st.now()
	var n int64
	err := st.inTx(ctx, false, "sweep", func(ctx context.Context, tx querier) error {
		tag, err := tx.Exec(ctx, `DELETE FROM web.session WHERE expires_at <= $1 OR last_seen_at <= $2`, now, now.Add(-or(st.Idle, DefaultIdle)))
		n = tag.RowsAffected()
		return err
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// Check reads nothing a person owns: it tells whether the store's schema
// is there and erp_web may use it, so a missing schema (the directory not
// yet set up, erp-router setup-directory) fails startup rather than the
// first sign-in.
func (st *Store) Check(ctx context.Context) error {
	return st.inTx(ctx, true, "the web schema of the directory database", func(ctx context.Context, tx querier) error {
		_, err := tx.Exec(ctx, `SELECT FROM web.session LIMIT 0; SELECT FROM web.login_client LIMIT 0`)
		return err
	})
}

// Open connects to the directory database with url, asking password for
// each new connection when it is not nil (erp-web's login's password,
// which changes while it runs: database.Settings), and checks the web
// schema is there.
func Open(ctx context.Context, url string, password func(context.Context) (string, error), sealer *Sealer) (*Store, error) {
	settings := database.DefaultSettings
	settings.Password, settings.MaxConns = password, MaxConns
	pool, err := database.Open(ctx, url, settings)
	if err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	st := &Store{Pool: pool, Sealer: sealer}
	if err := st.Check(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return st, nil
}

// Close releases the store's connections.
func (st *Store) Close() { st.Pool.Close() }
