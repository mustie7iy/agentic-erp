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

	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/worker"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Statuses of a directory entry. The cell's statuses pass through; ReadOnly
// is set in the directory alone, while a tenant moves between cells (05
// §5.4): the router lets reads through and refuses writes. Erased is an
// offboarded tenant whose data the cell erased (ADR-0021). The directory
// is the record outside the cell (ADR-0040): once offboarded or erased, an
// entry stays so whatever a cell restored to an earlier point says.
const (
	Active     = "active"
	Suspended  = "suspended"
	Offboarded = "offboarded"
	ReadOnly   = "read_only"
	Erased     = "erased"
)

// StatusOf is the directory status of a cell's tenant: a tenant still
// provisioning is routable (its first requests are its administrator's),
// an erased one is Erased.
func StatusOf(t tenancy.Lifecycle) string {
	switch {
	case t.ErasedAt != nil:
		return Erased
	case t.Status == "provisioning":
		return Active
	}
	return t.Status
}

// Entry maps a tenant to its cell.
type Entry struct {
	TenantID  uuid.UUID `json:"tenant_id"`
	Region    string    `json:"region"`
	Cell      string    `json:"cell"`
	APIURL    string    `json:"api_url"` // the cell's public API base URL
	Status    string    `json:"status"`
	Version   int32     `json:"version"` // moves on with every change; the ETag of the global control plane
	UpdatedAt time.Time `json:"updated_at"`
}

// Cell is a cell known to the directory: registered by the cell itself,
// with how many tenants the directory places there.
type Cell struct {
	Name       string    `json:"name"`
	Region     string    `json:"region"`
	APIURL     string    `json:"api_url"`
	ControlURL string    `json:"control_url"` // the admin listener the global control plane provisions through
	Accepting  bool      `json:"accepting"`   // takes new tenants
	Tenants    int       `json:"tenants"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ErrUnknownCell means the directory knows no such cell.
var ErrUnknownCell = errors.New("directory: unknown cell")

// ErrUnknown means the directory has no entry for the tenant.
var ErrUnknown = errors.New("directory: unknown tenant")

// Store is the directory's port.
type Store interface {
	// Lookup finds a tenant's entry; ErrUnknown when there is none.
	Lookup(ctx context.Context, tenant uuid.UUID) (Entry, error)
	// Upsert records a tenant's entry. A read_only status already in the
	// directory is kept, since only an operator sets and clears it; so are
	// offboarded (but for erased) and erased, which no restore of a cell
	// takes back (ADR-0040).
	Upsert(ctx context.Context, e Entry) error
}

// Schema is the directory's tables, created when the store opens: the
// tenants, the cells, the issuers (which tenant an active identity
// provider's issuer and audience belong to, ADR-0031), and the placements
// (which cell an Idempotency-Key of a global provisioning went to, so a
// retry goes to the same cell).
const Schema = `
CREATE SCHEMA IF NOT EXISTS directory;
CREATE TABLE IF NOT EXISTS directory.tenant (
  tenant_id  uuid PRIMARY KEY,
  region     text NOT NULL,
  cell       text NOT NULL,
  api_url    text NOT NULL,
  status     text NOT NULL CONSTRAINT tenant_status_check CHECK (status IN ('active', 'suspended', 'offboarded', 'read_only', 'erased')),
  version    integer NOT NULL DEFAULT 1,
  updated_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE directory.tenant ADD COLUMN IF NOT EXISTS version integer NOT NULL DEFAULT 1;
DO $$ BEGIN
  -- A directory made before erased (ADR-0040) gains it, once.
  IF NOT EXISTS (SELECT FROM pg_constraint WHERE conrelid = 'directory.tenant'::regclass AND conname = 'tenant_status_check'
                 AND pg_get_constraintdef(oid) LIKE '%erased%') THEN
    ALTER TABLE directory.tenant DROP CONSTRAINT IF EXISTS tenant_status_check;
    ALTER TABLE directory.tenant ADD CONSTRAINT tenant_status_check CHECK (status IN ('active', 'suspended', 'offboarded', 'read_only', 'erased'));
  END IF;
END $$;
CREATE TABLE IF NOT EXISTS directory.cell (
  name        text PRIMARY KEY,
  region      text NOT NULL,
  api_url     text NOT NULL,
  control_url text NOT NULL,
  accepting   boolean NOT NULL DEFAULT true,
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS directory.issuer (
  issuer     text NOT NULL,
  audience   text NOT NULL,
  tenant_id  uuid NOT NULL,
  cell       text NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (issuer, audience)
);
ALTER TABLE directory.issuer ADD COLUMN IF NOT EXISTS provider_id uuid;
CREATE TABLE IF NOT EXISTS directory.placement (
  idempotency_key text PRIMARY KEY,
  cell            text NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now()
);`

// Postgres keeps the directory in a database of its own (ERP_DIRECTORY_URL).
type Postgres struct {
	Pool *database.Pool
	// InTx runs a transaction; nil: Pool.InTx. Tests inject failures.
	InTx func(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error
}

func (p *Postgres) inTx(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error {
	if p.InTx != nil {
		return p.InTx(ctx, scope, fn)
	}
	return p.Pool.InTx(ctx, scope, fn)
}

// Open connects to the directory database and makes sure its table exists.
func Open(ctx context.Context, url string) (*Postgres, error) {
	return OpenWith(ctx, url, nil)
}

// OpenWith is Open with a password asked for each new connection in
// place of the URL's, when password is not nil: the directory's RDS-managed
// master password rotates (database.Settings.Password).
func OpenWith(ctx context.Context, url string, password func(context.Context) (string, error)) (*Postgres, error) {
	settings := database.DefaultSettings
	settings.Password = password
	pool, err := database.Open(ctx, url, settings)
	if err != nil {
		return nil, fmt.Errorf("directory: %w", err)
	}
	p := &Postgres{Pool: pool}
	if err := pool.InTx(ctx, database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
		// Openers starting together (replicas of a cell, a router and a
		// control plane) serialize on an advisory lock: two concurrent
		// CREATE SCHEMA IF NOT EXISTS otherwise race into a unique violation
		// on pg_namespace.
		_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('directory.schema'));`+Schema)
		if err != nil {
			return fmt.Errorf("lock and create: %w", err)
		}
		return nil
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
	err := p.inTx(ctx, database.Scope{ReadOnly: true}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT `+entryColumns+` FROM directory.tenant WHERE tenant_id = $1`, tenant).Scan(e.fields()...)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Entry{}, ErrUnknown
	}
	if err != nil {
		return Entry{}, fmt.Errorf("directory: lookup: %w", err)
	}
	return e, nil
}

const entryColumns = `tenant_id, region, cell, api_url, status, version, updated_at`

func (e *Entry) fields() []any {
	return []any{&e.TenantID, &e.Region, &e.Cell, &e.APIURL, &e.Status, &e.Version, &e.UpdatedAt}
}

// keptStatus is the status an upsert leaves: read_only and erased stay,
// offboarded stays unless the tenant is now erased, any other takes the
// cell's.
const keptStatus = `CASE WHEN directory.tenant.status IN ('read_only', 'erased') THEN directory.tenant.status
	WHEN directory.tenant.status = 'offboarded' AND EXCLUDED.status <> 'erased' THEN 'offboarded' ELSE EXCLUDED.status END`

// Upsert implements Store.
func (p *Postgres) Upsert(ctx context.Context, e Entry) error {
	err := p.inTx(ctx, database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO directory.tenant (tenant_id, region, cell, api_url, status) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (tenant_id) DO UPDATE SET region = EXCLUDED.region, cell = EXCLUDED.cell, api_url = EXCLUDED.api_url,
			status = `+keptStatus+`, version = directory.tenant.version + 1, updated_at = now()
			WHERE (directory.tenant.region, directory.tenant.cell, directory.tenant.api_url) IS DISTINCT FROM (EXCLUDED.region, EXCLUDED.cell, EXCLUDED.api_url)
			   OR directory.tenant.status <> `+keptStatus,
			e.TenantID, e.Region, e.Cell, e.APIURL, e.Status)
		if err != nil {
			return fmt.Errorf("write tenant %s: %w", e.TenantID, err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("directory: upsert: %w", err)
	}
	return nil
}

// update changes an entry if its version matches (0: any), returning the
// new entry; ErrUnknown when there is no entry, ErrVersion on a mismatch.
func (p *Postgres) update(ctx context.Context, tenant uuid.UUID, version int32, set string, args ...any) (Entry, error) {
	var e Entry
	err := p.inTx(ctx, database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
		var current int32
		if err := tx.QueryRow(ctx, `SELECT version FROM directory.tenant WHERE tenant_id = $1 FOR UPDATE`, tenant).Scan(&current); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrUnknown
			}
			return fmt.Errorf("read version of tenant %s: %w", tenant, err)
		}
		if version != 0 && version != current {
			return fmt.Errorf("%w: current version %d", ErrVersion, current)
		}
		args = append([]any{tenant}, args...)
		return tx.QueryRow(ctx, `UPDATE directory.tenant SET `+set+`, version = version + 1, updated_at = now() WHERE tenant_id = $1 RETURNING `+entryColumns, args...).Scan(e.fields()...)
	})
	if errors.Is(err, ErrUnknown) || errors.Is(err, ErrVersion) {
		return Entry{}, err
	}
	if err != nil {
		return Entry{}, fmt.Errorf("directory: update: %w", err)
	}
	return e, nil
}

// ErrVersion means the entry changed since it was read (If-Match).
var ErrVersion = errors.New("directory: version mismatch")

// SetStatus sets an entry's status in the directory alone (read_only for a
// move, and back), at the version given (0: any).
func (p *Postgres) SetStatus(ctx context.Context, tenant uuid.UUID, version int32, status string) (Entry, error) {
	return p.update(ctx, tenant, version, `status = $2`, status)
}

// ErrRegion means the cell named is in another region than the tenant.
var ErrRegion = errors.New("directory: the cell is in another region")

// Move points an entry at another registered cell of the tenant's region,
// at the version given (0: any): ErrUnknownCell, ErrRegion otherwise.
func (p *Postgres) Move(ctx context.Context, tenant uuid.UUID, version int32, cell string) (Entry, error) {
	var e Entry
	err := p.inTx(ctx, database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
		var current int32
		var region string
		if err := tx.QueryRow(ctx, `SELECT version, region FROM directory.tenant WHERE tenant_id = $1 FOR UPDATE`, tenant).Scan(&current, &region); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrUnknown
			}
			return fmt.Errorf("read version of tenant %s: %w", tenant, err)
		}
		if version != 0 && version != current {
			return fmt.Errorf("%w: current version %d", ErrVersion, current)
		}
		var cellRegion, apiURL string
		switch err := tx.QueryRow(ctx, `SELECT region, api_url FROM directory.cell WHERE name = $1`, cell).Scan(&cellRegion, &apiURL); {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrUnknownCell
		case err != nil:
			return fmt.Errorf("read cell %s: %w", cell, err)
		case cellRegion != region:
			return fmt.Errorf("%w: %s is in %s, the tenant in %s", ErrRegion, cell, cellRegion, region)
		}
		return tx.QueryRow(ctx, `UPDATE directory.tenant SET cell = $2, api_url = $3, version = version + 1, updated_at = now() WHERE tenant_id = $1 RETURNING `+entryColumns, tenant, cell, apiURL).Scan(e.fields()...)
	})
	for _, known := range []error{ErrUnknown, ErrVersion, ErrUnknownCell, ErrRegion} {
		if errors.Is(err, known) {
			return Entry{}, err
		}
	}
	if err != nil {
		return Entry{}, fmt.Errorf("directory: move: %w", err)
	}
	return e, nil
}

// Filter narrows a tenant listing; empty fields match everything.
type Filter struct {
	Region, Cell, Status string
}

// Sort is the order_by a directory listing accepts.
var Sort = pagination.Sort{Default: "updated_at", DefaultDesc: true, Fields: []pagination.Field{
	{Name: "updated_at", Column: "updated_at", Type: "timestamptz"},
	{Name: "cell", Column: "cell", Type: "text"},
}}

// CellTenants returns every entry the directory places in the cell, in
// tenant order: what a restored cell is checked against (ADR-0040).
func (p *Postgres) CellTenants(ctx context.Context, cell string) ([]Entry, error) {
	var entries []Entry
	err := p.inTx(ctx, database.Scope{ReadOnly: true}, func(ctx context.Context, tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT `+entryColumns+` FROM directory.tenant WHERE cell = $1 ORDER BY tenant_id`, cell)
		var err error
		entries, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Entry, error) {
			var e Entry
			return e, r.Scan(e.fields()...)
		})
		if err != nil {
			return fmt.Errorf("read tenants of cell %s: %w", cell, err)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("directory: %w", err)
	}
	return entries, nil
}

// ListTenants returns one page of entries.
func (p *Postgres) ListTenants(ctx context.Context, f Filter, ks *pagination.Keyset) ([]Entry, string, error) {
	var entries []Entry
	err := p.inTx(ctx, database.Scope{ReadOnly: true}, func(ctx context.Context, tx pgx.Tx) error {
		// The keyset breaks ties on a column named id.
		q, args := ks.SQL(`SELECT `+entryColumns+` FROM (SELECT *, tenant_id AS id FROM directory.tenant) t WHERE ($1 = '' OR region = $1) AND ($2 = '' OR cell = $2) AND ($3 = '' OR status = $3)`,
			[]any{f.Region, f.Cell, f.Status})
		rows, _ := tx.Query(ctx, q, args...)
		var err error
		entries, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Entry, error) {
			var e Entry
			return e, r.Scan(e.fields()...)
		})
		if err != nil {
			return fmt.Errorf("read tenants: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, "", fmt.Errorf("directory: list: %w", err)
	}
	entries, next := pagination.Page(ks, entries, func(e Entry) (string, uuid.UUID) {
		if ks.Field() == "cell" {
			return e.Cell, e.TenantID
		}
		return e.UpdatedAt.UTC().Format(time.RFC3339Nano), e.TenantID
	})
	return entries, next, nil
}

// UpsertCell records a cell, as the cell itself does each round.
func (p *Postgres) UpsertCell(ctx context.Context, c Cell) error {
	err := p.inTx(ctx, database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO directory.cell (name, region, api_url, control_url, accepting) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (name) DO UPDATE SET region = EXCLUDED.region, api_url = EXCLUDED.api_url, control_url = EXCLUDED.control_url, accepting = EXCLUDED.accepting, updated_at = now()`,
			c.Name, c.Region, c.APIURL, c.ControlURL, c.Accepting)
		if err != nil {
			return fmt.Errorf("write cell %s: %w", c.Name, err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("directory: upsert cell: %w", err)
	}
	return nil
}

// ListCells returns the cells (of a region, when given) with their tenant
// counts, fewest tenants first.
func (p *Postgres) ListCells(ctx context.Context, region string) ([]Cell, error) {
	var cells []Cell
	err := p.inTx(ctx, database.Scope{ReadOnly: true}, func(ctx context.Context, tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT c.name, c.region, c.api_url, c.control_url, c.accepting, (SELECT count(*) FROM directory.tenant t WHERE t.cell = c.name), c.updated_at
			FROM directory.cell c WHERE $1 = '' OR c.region = $1 ORDER BY 6, c.name`, region)
		var err error
		cells, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Cell])
		if err != nil {
			return fmt.Errorf("read cells: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("directory: list cells: %w", err)
	}
	return cells, nil
}

// Place decides which cell a provisioning with the key goes to: the cell
// chosen for that key before, else the accepting cell of the region with the
// fewest tenants (or the cell named, which must accept), recorded under the
// key so a retry goes there too. ErrUnknownCell when none fits.
func (p *Postgres) Place(ctx context.Context, key, region, cell string) (Cell, error) {
	var chosen Cell
	err := p.inTx(ctx, database.Scope{}, func(ctx context.Context, tx pgx.Tx) error {
		var name string
		switch err := tx.QueryRow(ctx, `SELECT cell FROM directory.placement WHERE idempotency_key = $1`, key).Scan(&name); {
		case err == nil:
			cell = name
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("read placement of key %s: %w", key, err)
		}
		err := tx.QueryRow(ctx, `SELECT c.name, c.region, c.api_url, c.control_url, c.accepting, (SELECT count(*) FROM directory.tenant t WHERE t.cell = c.name), c.updated_at
			FROM directory.cell c WHERE ($1 = '' OR c.region = $1) AND ($2 = '' OR c.name = $2) AND (c.accepting OR $2 <> '' AND $3)
			ORDER BY 6, c.name LIMIT 1`, region, cell, name != "").Scan(&chosen.Name, &chosen.Region, &chosen.APIURL, &chosen.ControlURL, &chosen.Accepting, &chosen.Tenants, &chosen.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnknownCell
		}
		if err != nil {
			return fmt.Errorf("choose cell: %w", err)
		}
		_, err = tx.Exec(ctx, `INSERT INTO directory.placement (idempotency_key, cell) VALUES ($1, $2) ON CONFLICT (idempotency_key) DO NOTHING`, key, chosen.Name)
		if err != nil {
			return fmt.Errorf("record placement of key %s in cell %s: %w", key, chosen.Name, err)
		}
		return nil
	})
	if errors.Is(err, ErrUnknownCell) {
		return Cell{}, err
	}
	if err != nil {
		return Cell{}, fmt.Errorf("directory: place: %w", err)
	}
	return chosen, nil
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
	return clock.Now(c.Now)
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
	// The cell's own entry, when the directory keeps cells (Cells): its
	// region, the admin listener the global control plane provisions
	// through, and whether it takes new tenants.
	Region     string
	ControlURL string
	Accepting  bool
	// Providers lists the cell's active identity providers' issuers and
	// tenants in the round's transaction (identity.ActiveIdentityProviders,
	// wired by the server); nil: the round claims no issuers.
	Providers func(ctx context.Context, tx pgx.Tx) ([]Issuer, error)
	Interval  time.Duration // between rounds; default 1 minute
	Log       *slog.Logger
	// Begin starts transactions; nil means Pool.Begin. Tests inject failures.
	Begin database.BeginFunc
}

func (s *Sync) begin(ctx context.Context, scope database.Scope) (pgx.Tx, error) {
	return database.BeginOr(ctx, s.Pool, s.Begin, scope)
}

// Register records one tenant of the cell right after a change (a
// provisioning, a status change). Failures are logged: the next round
// catches up.
func (s *Sync) Register(ctx context.Context, tenant uuid.UUID, region, status string) {
	if err := s.Directory.Upsert(ctx, Entry{TenantID: tenant, Region: region, Cell: s.Cell, APIURL: s.APIURL, Status: status}); err != nil {
		s.Log.ErrorContext(ctx, "directory registration failed; the next round retries", "tenant_id", tenant, "error", err)
	}
}

// Cells is a directory that keeps cells too (the Postgres one).
type Cells interface {
	UpsertCell(ctx context.Context, c Cell) error
}

// RunOnce records the cell itself, every tenant of it and, where the
// directory keeps issuers, their active identity providers' issuers; it
// returns how many tenants.
func (s *Sync) RunOnce(ctx context.Context) (int, error) {
	if cells, ok := s.Directory.(Cells); ok && s.ControlURL != "" {
		if err := cells.UpsertCell(ctx, Cell{Name: s.Cell, Region: s.Region, APIURL: s.APIURL, ControlURL: s.ControlURL, Accepting: s.Accepting}); err != nil {
			return 0, err
		}
	}
	tx, err := s.begin(ctx, database.Scope{Role: s.ControlRole, ReadOnly: true})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	tenants, err := tenancy.AllTenants(ctx, tx)
	if err != nil {
		return 0, fmt.Errorf("directory: read tenants of cell %s: %w", s.Cell, err)
	}
	for _, t := range tenants {
		if err := s.Directory.Upsert(ctx, Entry{TenantID: t.ID, Region: t.Region, Cell: s.Cell, APIURL: s.APIURL, Status: StatusOf(t.Lifecycle)}); err != nil {
			return 0, err
		}
	}
	if err := s.syncIssuers(ctx, tx); err != nil {
		return 0, err
	}
	return len(tenants), nil
}

// Run rounds until ctx ends, logging failures.
func (s *Sync) Run(ctx context.Context) {
	worker.Loop(ctx, s.Interval, time.Minute, func(ctx context.Context) (bool, error) {
		_, err := s.RunOnce(ctx)
		return false, err
	}, s.Log, "directory sync failed")
}
