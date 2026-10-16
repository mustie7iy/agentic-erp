// Package database opens the PostgreSQL pool and starts transactions in a
// scope: the database role the transaction runs as, and the tenant and actor
// that row-level security and triggers see (ADR-0003). Nothing in the
// platform touches tenant data outside such a transaction. Production
// reaches the database over verified TLS only (RequireVerifiedTLS,
// ADR-0033).
package database

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Scope is what a transaction runs as.
type Scope struct {
	// Role is the database role for the transaction (SET LOCAL ROLE), for
	// example erp_app or erp_relay. Empty keeps the login role.
	Role string
	// TenantID sets app.tenant_id for row-level security; zero leaves it unset,
	// so tenant tables show no rows.
	TenantID uuid.UUID
	// ReadOnly starts a read-only transaction.
	ReadOnly bool
}

// Settings bound how long a transaction may wait or run, and how many
// connections the pool opens.
type Settings struct {
	LockTimeout      time.Duration // waiting for a row lock, e.g. on a concurrent idempotency key
	StatementTimeout time.Duration
	// MaxConns is the pool's size; 0 keeps pgx's default (the number of CPUs,
	// at least 4). Requests beyond it wait for a connection until their
	// deadline, which sheds load as 503 instead of overloading the database.
	MaxConns int32
}

// DefaultSettings suit request transactions (ADR-0014).
var DefaultSettings = Settings{LockTimeout: 5 * time.Second, StatementTimeout: 30 * time.Second}

// Pool is the connection pool with the platform's transaction setup.
type Pool struct {
	pool     *pgxpool.Pool
	settings Settings
}

// Open connects and verifies the database is reachable.
func Open(ctx context.Context, url string, settings Settings) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("database: open: %w", err)
	}
	if settings.MaxConns != 0 {
		cfg.MaxConns = settings.MaxConns // the driver refuses a size below 1
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("database: open: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: ping: %w", err)
	}
	return &Pool{pool: pool, settings: settings}, nil
}

// Close releases all connections.
func (p *Pool) Close() { p.pool.Close() }

// Ping checks connectivity (readiness).
func (p *Pool) Ping(ctx context.Context) error {
	if err := p.pool.Ping(ctx); err != nil {
		return fmt.Errorf("database: ping: %w", err)
	}
	return nil
}

// WithConn runs fn on a dedicated connection, outside any transaction (for
// migrations and schema checks).
func (p *Pool) WithConn(ctx context.Context, fn func(*pgx.Conn) error) error {
	c, err := p.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("database: acquire: %w", err)
	}
	defer c.Release()
	return fn(c.Conn())
}

// Begin starts a transaction in the given scope. The caller commits or rolls
// back.
func (p *Pool) Begin(ctx context.Context, s Scope) (pgx.Tx, error) {
	opts := pgx.TxOptions{}
	if s.ReadOnly {
		opts.AccessMode = pgx.ReadOnly
	}
	tx, err := p.pool.BeginTx(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("database: begin: %w", err)
	}
	if err := p.setup(ctx, tx, s); err != nil {
		// Best effort: the setup error is the one returned; a failed rollback
		// only means the connection is discarded, which ends the transaction too.
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

// BeginFunc starts a transaction in a scope. Workers and services hold one
// as their Begin field, nil in production, so tests can inject failures.
type BeginFunc func(context.Context, Scope) (pgx.Tx, error)

// BeginOr starts a transaction with begin when it is set and with the pool
// otherwise.
func BeginOr(ctx context.Context, pool *Pool, begin BeginFunc, s Scope) (pgx.Tx, error) {
	if begin != nil {
		return begin(ctx, s)
	}
	return pool.Begin(ctx, s)
}

func (p *Pool) setup(ctx context.Context, tx pgx.Tx, s Scope) error {
	tenant := ""
	if s.TenantID != uuid.Nil {
		tenant = s.TenantID.String()
	}
	// set_config(..., true) is transaction-local, so a pooled connection never
	// carries one request's tenant into the next.
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true),
		set_config('lock_timeout', $2, true), set_config('statement_timeout', $3, true)`,
		tenant, ms(p.settings.LockTimeout), ms(p.settings.StatementTimeout)); err != nil {
		return fmt.Errorf("database: scope: %w", err)
	}
	if s.Role != "" {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{s.Role}.Sanitize()); err != nil {
			return fmt.Errorf("database: role %s: %w", s.Role, err)
		}
	}
	return nil
}

func ms(d time.Duration) string { return fmt.Sprintf("%dms", d.Milliseconds()) }

// SetActor records the acting principal on the transaction (app.actor_id).
func SetActor(ctx context.Context, tx pgx.Tx, actor uuid.UUID) error {
	if _, err := tx.Exec(ctx, "SELECT set_config('app.actor_id', $1, true)", actor.String()); err != nil {
		return fmt.Errorf("database: actor: %w", err)
	}
	return nil
}

// Today is the database's current date in the transaction: the day the
// validity checks and triggers judge by, so code that compares with them
// uses it rather than the application clock.
func Today(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var d time.Time
	if err := tx.QueryRow(ctx, `SELECT current_date`).Scan(&d); err != nil {
		return time.Time{}, fmt.Errorf("database: read current date: %w", err)
	}
	return d, nil
}

// InTx runs fn in a transaction and commits if it returns nil.
func (p *Pool) InTx(ctx context.Context, s Scope, fn func(context.Context, pgx.Tx) error) error {
	tx, err := p.Begin(ctx, s)
	if err != nil {
		return err
	}
	if err := fn(ctx, tx); err != nil {
		// Best effort: fn's error is the one returned; a failed rollback only
		// means the connection is discarded, which ends the transaction too.
		_ = tx.Rollback(ctx)
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("database: commit: %w", err)
	}
	return nil
}
