// Package database opens the PostgreSQL pool and starts transactions in a
// scope: the database role the transaction runs as, and the tenant and actor
// that row-level security and triggers see (ADR-0003). Nothing in the
// platform touches tenant data outside such a transaction. Production
// reaches the database over verified TLS only (RequireVerifiedTLS,
// ADR-0033).
package database

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	// Isolation is the transaction's isolation level; empty keeps the
	// server's default (READ COMMITTED). An extract that reads several
	// tables as of one database state asks for REPEATABLE READ (ADR-0044).
	Isolation pgx.TxIsoLevel
}

// ActiveTenants is the set of tenants a background worker works for, as a
// subquery: a worker's query across tenants adds
// `AND <tenant column> IN ` + ActiveTenants. A tenant that is not active
// (provisioning, suspended or offboarded) is refused at the door
// (ADR-0015), so its queued work stays where it is, untouched, and the
// worker picks it up once the tenant is active again; offboarding's erasure
// is not such work and does not use it. The worker role may read the
// tenant's id and status (000023).
const ActiveTenants = `(SELECT id FROM tenancy.tenant WHERE status = 'active')`

// Settings bound how long a transaction may wait or run, and how many
// connections the pool opens.
type Settings struct {
	LockTimeout      time.Duration // waiting for a row lock, e.g. on a concurrent idempotency key
	StatementTimeout time.Duration
	// MaxConns is the pool's size; 0 keeps pgx's default (the number of CPUs,
	// at least 4). Requests beyond it wait for a connection until their
	// deadline, which sheds load as 503 instead of overloading the database.
	MaxConns int32
	// Password, when set, is the login's password, asked for each new
	// connection in place of the URL's: a password that rotates (RDS's
	// managed master credentials, mounted as a file External Secrets
	// refreshes) reaches new connections without a restart.
	Password func(context.Context) (string, error)
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
	if settings.Password != nil {
		cfg.BeforeConnect = func(ctx context.Context, cc *pgx.ConnConfig) error {
			pw, err := settings.Password(ctx)
			if err != nil {
				return fmt.Errorf("database: password: %w", err)
			}
			cc.Password = pw
			return nil
		}
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
	opts := pgx.TxOptions{IsoLevel: s.Isolation}
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

// TenantInactiveError is why BeginActive refused a transaction: the
// scope's tenant is not active (ADR-0015). It carries its own context
// rather than being wrapped, so an activity that returns it as it is fails
// with it outermost: its type name is what the workflow engine names that
// failure and what a retry policy can match (workflows.TenantInactive), so
// it is not renamed lightly.
type TenantInactiveError struct {
	Tenant uuid.UUID
	Status string // suspended, provisioning or offboarded
}

func (e *TenantInactiveError) Error() string {
	return fmt.Sprintf("database: tenant %s is %s, not active", e.Tenant, e.Status)
}

// BeginActive starts a transaction as BeginOr does and, in a tenant's
// scope, only while the tenant is active: for one that is not, the
// transaction is rolled back and a *TenantInactiveError returned, which a
// workflow's step passes on as a failure to try again later
// (po:2026-11-09: a running workflow waits while its tenant is suspended,
// ADR-0015 As built). A scope without a tenant (a worker's read across
// tenants) is not checked. The status is read, not locked, as the unit of
// work reads it for a request: a step whose transaction began before a
// suspension committed may still commit, as a request in flight may.
func BeginActive(ctx context.Context, pool *Pool, begin BeginFunc, s Scope) (pgx.Tx, error) {
	tx, err := BeginOr(ctx, pool, begin, s)
	if err != nil || s.TenantID == uuid.Nil {
		return tx, err
	}
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM tenancy.tenant WHERE id = $1`, s.TenantID).Scan(&status); err != nil {
		// Best effort: the read's error is the one returned; a failed rollback
		// only means the connection is discarded, which ends the transaction too.
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("database: read the status of tenant %s: %w", s.TenantID, err)
	}
	if status != "active" {
		_ = tx.Rollback(context.WithoutCancel(ctx)) // best effort, as above
		return nil, &TenantInactiveError{Tenant: s.TenantID, Status: status}
	}
	return tx, nil
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
	return setRole(ctx, tx, s.Role)
}

func setRole(ctx context.Context, tx pgx.Tx, role string) error {
	if role == "" {
		return nil
	}
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
		return fmt.Errorf("database: role %s: %w", role, err)
	}
	return nil
}

// Rescope moves a transaction to another scope's tenant and role for the
// rest of it (its timeouts stay), so that one transaction does work that
// needs two roles and commits or fails as one: offboarding erases a
// tenant's rows as the worker and records the erasure, with the counts it
// returned, as the application in the tenant's scope (ADR-0021).
//
// It fails closed: a scope without a role is refused (Begin's "keep the
// login role" would here keep the previous role, which may bypass
// row-level security), and so is one asking for a read-only transaction
// or an isolation level, which a transaction under way cannot take.
func Rescope(ctx context.Context, tx pgx.Tx, s Scope) error {
	if s.Role == "" || s.ReadOnly || s.Isolation != "" {
		return errors.New("database: rescope: a role is required, and a transaction under way takes no read-only mode or isolation level")
	}
	tenant := ""
	if s.TenantID != uuid.Nil {
		tenant = s.TenantID.String()
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
		return fmt.Errorf("database: rescope: %w", err)
	}
	return setRole(ctx, tx, s.Role)
}

func ms(d time.Duration) string { return fmt.Sprintf("%dms", d.Milliseconds()) }

// SetActor records the acting principal on the transaction (app.actor_id).
func SetActor(ctx context.Context, tx pgx.Tx, actor uuid.UUID) error {
	if _, err := tx.Exec(ctx, "SELECT set_config('app.actor_id', $1, true)", actor.String()); err != nil {
		return fmt.Errorf("database: actor: %w", err)
	}
	return nil
}

// LockTenant takes the transaction's advisory lock named by name and
// parts within the transaction's tenant, and holds it to commit or
// rollback (ADR-0008): a check that no row lock can guard, such as what
// is left to promise of a material at a plant, serializes on it. The key
// is "<name>:<tenant>:<part>:<part>…", hashed by hashtextextended, as the
// call sites that build it in SQL write it, so a caller moved to this
// helper keeps waiting for the ones that have not moved. The tenant is
// the transaction's (platform.require_tenant(), which refuses a
// transaction without one), never the caller's input, so two tenants
// never wait for each other (ADR-0003). A caller that takes several
// takes them in one fixed order, so two transactions never wait on each
// other in a cycle.
func LockTenant(ctx context.Context, tx pgx.Tx, name string, parts ...string) error {
	var suffix strings.Builder
	for _, part := range parts {
		suffix.WriteString(":" + part)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text || ':' || platform.require_tenant() || $2::text, 0))`,
		name, suffix.String()); err != nil {
		return fmt.Errorf("database: advisory lock %s%s: %w", name, suffix.String(), err)
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

// Tenants runs a background worker's read across tenants, query, in a
// read-only transaction as role (the worker role, which reads every
// tenant's queue and nothing else) and returns the tenant ids it selects.
// query selects one uuid column and normally ends its condition with
// `AND tenant_id IN ` + ActiveTenants; the caller names what it listed
// when it wraps the error.
func Tenants(ctx context.Context, pool *Pool, begin BeginFunc, role, query string, args ...any) ([]uuid.UUID, error) {
	tx, err := BeginOr(ctx, pool, begin, Scope{Role: role, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	rows, _ := tx.Query(ctx, query, args...)
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("database: read the tenants: %w", err)
	}
	return tenants, nil
}
