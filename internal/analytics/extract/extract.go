// Package extract feeds the analytical store (ADR-0044): per tenant, it
// reads the tenant's datasets in one REPEATABLE READ transaction under the
// tenant's row-level security (ADR-0003), on the read replica in
// production, writes them as Parquet files through blobstore, writes a
// manifest naming every file, and publishes the manifest by moving the
// tenant's pointer (analytics.snapshot) in one transaction guarded by the
// manifest it replaced (ADR-0008).
//
// The journal is appended by a transaction-id watermark: a run reads the
// entries whose posted_xid lies at or above the previous run's cutoff and
// below its own, the xmin of its snapshot. Every transaction below xmin has
// finished, so no entry is skipped however long its posting transaction
// ran. A run that finds nothing new writes nothing, so running it twice
// leaves the store as one run did.
//
// The extractor reads the ledger's, the directory's and the partners'
// tables directly, read-only: it is the store's copy of what the API
// returns (the semantic layer's BuildFacts), held to it by the parity test
// (T55e), and a dataset's columns are the contract in analytics/datasets.
// Money is integer minor units with its currency and exponent (ADR-0005).
package extract

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

// ErrMoved is returned when another run published between this run's read
// and its publication; the files it wrote stay unreferenced and the sweep's
// next run starts from the newer pointer.
var ErrMoved = errors.New("analytics: snapshot moved by another run")

// ErrBehind is returned when the database's transaction ids are below the
// tenant's watermark: the database was restored or moved and the tenant's
// store must be rebuilt (ADR-0044), which the sweep alerts on.
var ErrBehind = errors.New("analytics: watermark ahead of the database; rebuild the store")

// Extractor extracts one tenant at a time.
type Extractor struct {
	// Replica begins the extract's reading transaction: the cell's read
	// replica in production, the primary in tests.
	Replica database.BeginFunc
	// Primary begins the publication, which writes the pointer.
	Primary database.BeginFunc
	Role    string // the application role (erp_app); never one that bypasses row-level security
	Store   blobstore.Store
}

// Result is what a run did: the tenant's current manifest and watermark,
// and the journal lines it appended (none when nothing was new).
type Result struct {
	ManifestKey string
	Watermark   uint64
	Lines       int
}

// pointer is a tenant's row of analytics.snapshot; set is false before the
// tenant's first publication.
type pointer struct {
	key       string
	watermark uint64
	set       bool
}

// snapshot is what one reading transaction saw.
type snapshot struct {
	prev   pointer
	cutoff uint64
	asOf   time.Time
	lines  []glLine
}

// Run extracts the tenant's journal lines above its watermark and publishes
// them. Nothing new leaves the files and the pointer as they are.
func (x Extractor) Run(ctx context.Context, tenant uuid.UUID) (Result, error) {
	s, err := x.read(ctx, tenant)
	if err != nil {
		return Result{}, fmt.Errorf("analytics: extract tenant %s: %w", tenant, err)
	}
	if len(s.lines) == 0 {
		return Result{ManifestKey: s.prev.key, Watermark: s.prev.watermark}, nil
	}
	key, asOf, err := x.write(ctx, tenant, s)
	if err != nil {
		return Result{}, fmt.Errorf("analytics: extract tenant %s: %w", tenant, err)
	}
	s.asOf = asOf
	if err := x.publish(ctx, tenant, s, key); err != nil {
		return Result{}, fmt.Errorf("analytics: extract tenant %s: %w", tenant, err)
	}
	return Result{ManifestKey: key, Watermark: s.cutoff, Lines: len(s.lines)}, nil
}

// read takes the pointer, the cutoff and the new lines from one snapshot of
// the tenant's rows.
func (x Extractor) read(ctx context.Context, tenant uuid.UUID) (snapshot, error) {
	var s snapshot
	job := uow.Job{Begin: x.Replica, Name: "analytics: read",
		Scope: database.Scope{Role: x.Role, TenantID: tenant, ReadOnly: true, Isolation: pgx.RepeatableRead}}
	err := job.Tx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		var err error
		if s.prev, err = current(ctx, tx); err != nil {
			return false, err
		}
		if err := tx.QueryRow(ctx, `SELECT pg_snapshot_xmin(pg_current_snapshot())::text, now()`).Scan((*xid)(&s.cutoff), &s.asOf); err != nil {
			return false, fmt.Errorf("cutoff: %w", err)
		}
		// A cutoff at the watermark reads the empty interval. Below it, the
		// database was restored or moved under the store: appending from the
		// stored watermark would skip the entries posted with the transaction
		// ids in between, so the run stops until the store is rebuilt
		// (ADR-0044).
		if s.prev.set && s.cutoff < s.prev.watermark {
			return false, fmt.Errorf("%w: watermark %d, cutoff %d", ErrBehind, s.prev.watermark, s.cutoff)
		}
		s.lines, err = readLines(ctx, tx, s.prev, s.cutoff)
		return false, err
	})
	return s, err
}

// current reads the tenant's pointer.
func current(ctx context.Context, tx pgx.Tx) (pointer, error) {
	var p pointer
	err := tx.QueryRow(ctx, `SELECT manifest_key, watermark::text FROM analytics.snapshot`).Scan(&p.key, (*xid)(&p.watermark))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, fmt.Errorf("pointer: %w", err)
	}
	p.set = true
	return p, nil
}

// publish moves the tenant's pointer to the manifest, guarded by the one the
// run read: a run that lost the race reports ErrMoved and changes nothing.
func (x Extractor) publish(ctx context.Context, tenant uuid.UUID, s snapshot, key string) error {
	job := uow.Job{Begin: x.Primary, Name: "analytics: publish", Scope: database.Scope{Role: x.Role, TenantID: tenant}}
	return job.Tx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		wm := strconv.FormatUint(s.cutoff, 10)
		var sql string
		args := []any{key, wm, s.asOf}
		if s.prev.set {
			sql = `UPDATE analytics.snapshot SET manifest_key = $1, watermark = $2::text::xid8, as_of = $3, published_at = now()
				WHERE manifest_key = $4`
			args = append(args, s.prev.key)
		} else {
			sql = `INSERT INTO analytics.snapshot (tenant_id, manifest_key, watermark, as_of)
				VALUES (platform.current_tenant(), $1, $2::text::xid8, $3) ON CONFLICT (tenant_id) DO NOTHING`
		}
		tag, err := tx.Exec(ctx, sql, args...)
		if err != nil {
			return false, fmt.Errorf("publish: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return false, ErrMoved
		}
		return true, nil
	})
}

// xid scans an xid8 the query cast to text: pgx has no codec for xid8, and
// its 64 bits do not fit a bigint.
type xid uint64

// ScanText implements pgtype.TextScanner.
func (x *xid) ScanText(v pgtype.Text) error {
	n, err := strconv.ParseUint(v.String, 10, 64)
	if err != nil {
		return fmt.Errorf("transaction id %q: %w", v.String, err)
	}
	*x = xid(n)
	return nil
}
