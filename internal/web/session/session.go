// Package session is erp-web's store of signed-in people (ADR-0052 §6,
// T73b): server-side sessions in the web schema of the global directory's
// database, which erp-web reaches as the erp_web role, seeing that schema
// alone (directory.WebSchema), and the web clients it signs people in
// with, found by their login code. A session holds who signed in and the
// tokens the provider issued, sealed (Sealer); the browser holds only its
// random id, of which the store keeps the SHA-256. Nothing here is
// business data, and nothing is exported or fingerprinted. The store is
// the one package of the workspace that holds a database pool
// (internal/layout).
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

	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
)

// Role is the database role erp-web's transactions run as.
const Role = "erp_web"

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

func (st *Store) inTx(ctx context.Context, readOnly bool, fn func(context.Context, pgx.Tx) error) error {
	scope := database.Scope{Role: Role, ReadOnly: readOnly}
	if st.InTx != nil {
		return st.InTx(ctx, scope, fn)
	}
	return st.Pool.InTx(ctx, scope, fn)
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
	plain, _ := json.Marshal(sec) // strings and times always encode
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
	err := st.inTx(ctx, false, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO web.session (id_hash, tenant_id, issuer, subject, login_code, created_at, last_seen_at, expires_at, sealed)
			VALUES ($1, $2, $3, $4, $5, $6, $6, $7, $8)`, hash, s.TenantID, s.Issuer, s.Subject, s.LoginCode, now, s.Expires, st.seal(hash, s.Secrets))
		return err //nolint:wrapcheck // wrapped below
	})
	if err != nil {
		return Session{}, fmt.Errorf("session: create: %w", err)
	}
	return s, nil
}

const columns = `tenant_id, issuer, subject, login_code, created_at, last_seen_at, expires_at, version, sealed`

func (st *Store) scan(row pgx.Row, id string) (Session, error) {
	s := Session{ID: id}
	var sealed []byte
	if err := row.Scan(&s.TenantID, &s.Issuer, &s.Subject, &s.LoginCode, &s.Created, &s.LastSeen, &s.Expires, &s.Version, &sealed); err != nil {
		return s, err //nolint:wrapcheck // callers wrap
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
	err := st.inTx(ctx, false, func(ctx context.Context, tx pgx.Tx) error {
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
		return err //nolint:wrapcheck // wrapped below
	})
	switch {
	case errors.Is(err, ErrNoSession):
		return Session{}, err
	case err != nil:
		return Session{}, fmt.Errorf("session: load: %w", err)
	case ended:
		return Session{}, ErrNoSession
	}
	return s, nil
}

// Renew replaces the session's secrets with those renew makes from its
// current ones, holding the session's row FOR UPDATE meanwhile: concurrent
// requests of one session, on any replica, renew it once, the others
// waiting and then taking the renewed secrets, for which fresh reports
// true. A refresh token is used once (rotation), so it must never be
// spent twice (ADR-0052 §5). renew runs inside the transaction, holding
// the lock, and should be bounded by ctx.
func (st *Store) Renew(ctx context.Context, s Session, fresh func(Secrets) bool, renew func(context.Context, Secrets) (Secrets, error)) (Session, error) {
	hash := Hash(s.ID)
	err := st.inTx(ctx, false, func(ctx context.Context, tx pgx.Tx) error {
		cur, err := st.scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM web.session WHERE id_hash = $1 FOR UPDATE`, hash), s.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoSession
		}
		if err != nil {
			return err
		}
		if s = cur; fresh(cur.Secrets) {
			return nil
		}
		if s.Secrets, err = renew(ctx, cur.Secrets); err != nil {
			return err
		}
		s.Version++
		_, err = tx.Exec(ctx, `UPDATE web.session SET sealed = $2, version = $3 WHERE id_hash = $1`, hash, st.seal(hash, s.Secrets), s.Version)
		return err //nolint:wrapcheck // wrapped below
	})
	if err != nil {
		return Session{}, fmt.Errorf("session: renew: %w", err)
	}
	return s, nil
}

// Delete ends the session with the id; one already gone is no error.
func (st *Store) Delete(ctx context.Context, id string) error {
	err := st.inTx(ctx, false, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM web.session WHERE id_hash = $1`, Hash(id))
		return err //nolint:wrapcheck // wrapped below
	})
	if err != nil {
		return fmt.Errorf("session: delete: %w", err)
	}
	return nil
}

// Sweep deletes the sessions past their lifetime or idle longer than Idle,
// which nobody came back to end; it returns how many. Running it twice
// deletes nothing more.
func (st *Store) Sweep(ctx context.Context) (int64, error) {
	now := st.now()
	var n int64
	err := st.inTx(ctx, false, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM web.session WHERE expires_at <= $1 OR last_seen_at <= $2`, now, now.Add(-or(st.Idle, DefaultIdle)))
		n = tag.RowsAffected()
		return err //nolint:wrapcheck // wrapped below
	})
	if err != nil {
		return 0, fmt.Errorf("session: sweep: %w", err)
	}
	return n, nil
}

// Check reads nothing a person owns: it tells whether the store's schema
// is there and erp_web may use it, so a missing schema (the directory not
// yet opened by its owners) fails startup rather than the first sign-in.
func (st *Store) Check(ctx context.Context) error {
	err := st.inTx(ctx, true, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT FROM web.session LIMIT 0; SELECT FROM web.login_client LIMIT 0`)
		return err //nolint:wrapcheck // wrapped below
	})
	if err != nil {
		return fmt.Errorf("session: the web schema of the directory database: %w", err)
	}
	return nil
}

// Open connects to the directory database with url, asking password for
// each new connection when it is not nil (the directory's rotating
// password, database.Settings), and checks the web schema is there.
func Open(ctx context.Context, url string, password func(context.Context) (string, error), sealer *Sealer) (*Store, error) {
	settings := database.DefaultSettings
	settings.Password = password
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
