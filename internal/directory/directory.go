// Package directory is the global tenant directory of 05-scalability §5.4
// (ADR-0022): which region and cell a tenant lives in, and whether its
// requests may be routed there. It is small, read mostly, holds no business
// data, and lives outside the cells' databases: the router reads it to
// forward a request to the tenant's cell, and every cell keeps its own
// tenants' entries current.
package directory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
)

// Statuses of a directory entry. The cell's statuses pass through; ReadOnly
// is set in the directory alone, while a tenant moves between cells (05
// §5.4): the router lets reads through and refuses writes.
const (
	Active     = "active"
	Suspended  = "suspended"
	Offboarded = "offboarded"
	ReadOnly   = "read_only"
)

// Entry maps a tenant to its cell.
type Entry struct {
	TenantID  uuid.UUID `json:"tenant_id"`
	Region    string    `json:"region"`
	Cell      string    `json:"cell"`
	APIURL    string    `json:"api_url"` // the cell's public API base URL
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ErrUnknown means the directory has no entry for the tenant.
var ErrUnknown = errors.New("directory: unknown tenant")

// Store is the directory's port.
type Store interface {
	// Lookup finds a tenant's entry; ErrUnknown when there is none.
	Lookup(ctx context.Context, tenant uuid.UUID) (Entry, error)
	// Upsert records a tenant's entry. A read_only status already in the
	// directory is kept, since only an operator sets and clears it.
	Upsert(ctx context.Context, e Entry) error
}

// Schema is the directory's one table, created when the store opens.
const Schema = `
CREATE SCHEMA IF NOT EXISTS directory;
CREATE TABLE IF NOT EXISTS directory.tenant (
  tenant_id  uuid PRIMARY KEY,
  region     text NOT NULL,
  cell       text NOT NULL,
  api_url    text NOT NULL,
  status     text NOT NULL CHECK (status IN ('active', 'suspended', 'offboarded', 'read_only')),
  updated_at timestamptz NOT NULL DEFAULT now()
);`

// Postgres keeps the directory in a database of its own (ERP_DIRECTORY_URL).
type Postgres struct {
	Pool *database.Pool
}

// Open connects to the directory database and makes sure its table exists.
func Open(ctx context.Context, url string) (*Postgres, error) {
	pool, err := database.Open(ctx, url, database.DefaultSettings)
	if err != nil {
		return nil, fmt.Errorf("directory: %w", err)
	}
	p := &Postgres{Pool: pool}
	if err := pool.InTx(ctx, database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, Schema)
		return err
	}); err != nil {
		pool.Close()
		return nil, fmt.Errorf("directory: schema: %w", err)
	}
	return p, nil
}

// Close releases the connections.
func (p *Postgres) Close() { p.Pool.Close() }

// Lookup implements Store.
func (p *Postgres) Lookup(ctx context.Context, tenant uuid.UUID) (Entry, error) {
	var e Entry
	err := p.Pool.InTx(ctx, database.Scope{ReadOnly: true}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT tenant_id, region, cell, api_url, status, updated_at FROM directory.tenant WHERE tenant_id = $1`, tenant).
			Scan(&e.TenantID, &e.Region, &e.Cell, &e.APIURL, &e.Status, &e.UpdatedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Entry{}, ErrUnknown
	}
	if err != nil {
		return Entry{}, fmt.Errorf("directory: lookup: %w", err)
	}
	return e, nil
}

// Upsert implements Store.
func (p *Postgres) Upsert(ctx context.Context, e Entry) error {
	err := p.Pool.InTx(ctx, database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO directory.tenant (tenant_id, region, cell, api_url, status) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (tenant_id) DO UPDATE SET region = EXCLUDED.region, cell = EXCLUDED.cell, api_url = EXCLUDED.api_url,
			status = CASE WHEN directory.tenant.status = 'read_only' THEN 'read_only' ELSE EXCLUDED.status END, updated_at = now()`,
			e.TenantID, e.Region, e.Cell, e.APIURL, e.Status)
		return err
	})
	if err != nil {
		return fmt.Errorf("directory: upsert: %w", err)
	}
	return nil
}

// SetStatus sets an entry's status in the directory alone (read_only for a
// move, and back); ErrUnknown when there is no entry.
func (p *Postgres) SetStatus(ctx context.Context, tenant uuid.UUID, status string) error {
	err := p.Pool.InTx(ctx, database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE directory.tenant SET status = $2, updated_at = now() WHERE tenant_id = $1`, tenant, status)
		if err == nil && tag.RowsAffected() == 0 {
			return ErrUnknown
		}
		return err
	})
	if errors.Is(err, ErrUnknown) {
		return err
	}
	if err != nil {
		return fmt.Errorf("directory: set status: %w", err)
	}
	return nil
}

// Cache answers lookups from memory for TTL, hits and misses alike, so the
// router asks the directory once per tenant per TTL rather than per request.
type Cache struct {
	Store Store
	TTL   time.Duration    // default 30 seconds
	Now   func() time.Time // nil: time.Now

	mu      sync.Mutex
	entries map[uuid.UUID]cached
}

type cached struct {
	entry Entry
	err   error
	until time.Time
}

func (c *Cache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Lookup implements Store from the cache, else from the store.
func (c *Cache) Lookup(ctx context.Context, tenant uuid.UUID) (Entry, error) {
	now := c.now()
	c.mu.Lock()
	if hit, ok := c.entries[tenant]; ok && now.Before(hit.until) {
		c.mu.Unlock()
		return hit.entry, hit.err
	}
	c.mu.Unlock()
	e, err := c.Store.Lookup(ctx, tenant)
	if err != nil && !errors.Is(err, ErrUnknown) {
		return Entry{}, err // a directory failure is not remembered
	}
	ttl := c.TTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[uuid.UUID]cached{}
	}
	c.entries[tenant] = cached{entry: e, err: err, until: now.Add(ttl)}
	c.mu.Unlock()
	return e, err
}

// Upsert implements Store: through to the store, and the cache forgets the tenant.
func (c *Cache) Upsert(ctx context.Context, e Entry) error {
	if err := c.Store.Upsert(ctx, e); err != nil {
		return err
	}
	c.mu.Lock()
	delete(c.entries, e.TenantID)
	c.mu.Unlock()
	return nil
}

// Sync keeps the directory's entries for one cell's tenants current: every
// tenant of the cell, with its status, region and this cell's API URL, in
// rounds and on request after a change.
type Sync struct {
	Pool        *database.Pool // the cell's database
	ControlRole string         // erp_control: reads tenants across the cell
	Directory   Store
	Cell        string
	APIURL      string
	Interval    time.Duration // between rounds; default 1 minute
	Log         *slog.Logger
	// Begin starts transactions; nil means Pool.Begin. Tests inject failures.
	Begin func(context.Context, database.Scope) (pgx.Tx, error)
}

func (s *Sync) begin(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
	if s.Begin != nil {
		return s.Begin(ctx, scope)
	}
	return s.Pool.Begin(ctx, scope)
}

// Register records one tenant of the cell right after a change (a
// provisioning, a status change). Failures are logged: the next round
// catches up.
func (s *Sync) Register(ctx context.Context, tenant uuid.UUID, region, status string) {
	if err := s.Directory.Upsert(ctx, Entry{TenantID: tenant, Region: region, Cell: s.Cell, APIURL: s.APIURL, Status: status}); err != nil {
		s.Log.ErrorContext(ctx, "directory registration failed; the next round retries", "tenant_id", tenant, "error", err)
	}
}

// RunOnce records every tenant of the cell and returns how many.
func (s *Sync) RunOnce(ctx context.Context) (int, error) {
	tx, err := s.begin(ctx, database.Scope{Role: s.ControlRole, ReadOnly: true})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	rows, _ := tx.Query(ctx, `SELECT id, region, status FROM tenancy.tenant ORDER BY id`)
	type tenant struct {
		ID             uuid.UUID
		Region, Status string
	}
	tenants, err := pgx.CollectRows(rows, pgx.RowToStructByPos[tenant])
	if err != nil {
		return 0, err
	}
	for _, t := range tenants {
		status := t.Status
		if status == "provisioning" {
			status = Active // routable: the tenant's first requests are its administrator's
		}
		if err := s.Directory.Upsert(ctx, Entry{TenantID: t.ID, Region: t.Region, Cell: s.Cell, APIURL: s.APIURL, Status: status}); err != nil {
			return 0, err
		}
	}
	return len(tenants), nil
}

// Run rounds until ctx ends, logging failures.
func (s *Sync) Run(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	for {
		if _, err := s.RunOnce(ctx); err != nil && ctx.Err() == nil {
			s.Log.ErrorContext(ctx, "directory sync failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
