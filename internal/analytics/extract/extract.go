// Package extract feeds the analytical store (ADR-0044): per tenant, it
// reads the tenant's datasets in one REPEATABLE READ transaction under the
// tenant's row-level security (ADR-0003), on the read replica in
// production, writes them as Parquet files through blobstore, writes a
// manifest naming every file, and publishes the manifest by moving the
// tenant's pointer (analytics.snapshot) in one transaction guarded by the
// manifest it replaced (ADR-0008).
//
// Documents and master data (purchase order items, supplier invoices, open
// items, accounts, suppliers) are snapshots: each run reads them whole, in
// the same transaction as the journal, and names each file by its content,
// so an unchanged month or dimension keeps its file and a run that finds
// nothing new writes nothing. Fields the masking policy hides (bank
// accounts, tax identifiers) are never read.
//
// The journal is appended by a transaction-id watermark: a run reads the
// entries whose posted_xid lies at or above the previous run's cutoff and
// below its own, the xmin of its snapshot. Every transaction below xmin has
// finished, so no entry is skipped however long its posting transaction
// ran. A run that finds nothing new writes nothing but its check time on
// the pointer, so running it twice leaves the store as one run did.
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
	"github.com/jackc/pgx/v5/pgconn"
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
	// replica, read only once it has replayed past the WAL position of the
	// run's claim (RunAt), else the primary.
	Replica database.BeginFunc
	// Primary begins the publication, which writes the pointer, and the
	// reading transaction while the replica lags.
	Primary database.BeginFunc
	Role    string // the application role (erp_app); never one that bypasses row-level security
	// WorkerRole (erp_worker) forgets the superseded manifests Collect has
	// deleted: the application deletes no row.
	WorkerRole string
	Store      blobstore.Store
	// Keep is how long a superseded manifest's files stay, so a running
	// query never loses one (Collect); default Keep.
	Keep time.Duration
	// CompactAt is how many journal parts a month gathers before a run
	// merges them into one; default CompactAt.
	CompactAt int
	// Replayed reports whether the replica has replayed past lsn; nil asks
	// the replica (pg_last_wal_replay_lsn). Tests replace it to stand for a
	// lagging replica.
	Replayed func(ctx context.Context, lsn string) (bool, error)
}

// Result is what a run did: the tenant's current manifest and watermark,
// the journal lines it appended (none when nothing was new), whether it
// published a manifest, and whether journal entries above its cutoff wait
// for a later run (an open transaction holds the snapshot horizon back).
type Result struct {
	ManifestKey string
	Watermark   uint64
	Lines       int
	Published   bool
	Pending     bool
}

// pointer is a tenant's row of analytics.snapshot; set is false before the
// tenant's first publication.
type pointer struct {
	key       string
	watermark uint64
	set       bool
}

// run is one extract of a tenant: what its reading transaction saw and the
// manifest it builds from the previous one.
type run struct {
	x       Extractor
	tenant  uuid.UUID
	prev    pointer
	prevM   Manifest
	cutoff  uint64
	asOf    time.Time
	m       Manifest
	lines   int  // journal lines appended
	pending bool // journal entries at or above the cutoff wait for a later run
}

// Run extracts the tenant's datasets on the replica and publishes them.
// A run that finds nothing new writes no file and no manifest; it records
// its check time on the pointer unless journal entries wait above its
// cutoff.
func (x Extractor) Run(ctx context.Context, tenant uuid.UUID) (Result, error) {
	return x.RunAt(ctx, tenant, "")
}

// RunAt is Run for a change the primary had reached at WAL position lsn
// (the scheduler's claim): it reads on the replica only once the replica
// has replayed past lsn, and on the primary until then, so a lagging
// replica never finds nothing new and marks the change done (ADR-0044).
// An empty lsn reads on the replica.
func (x Extractor) RunAt(ctx context.Context, tenant uuid.UUID, lsn string) (Result, error) {
	begin, err := x.reader(ctx, lsn)
	if err != nil {
		return Result{}, wrapRun(tenant, err)
	}
	r, err := x.read(ctx, tenant, begin)
	if err == nil && r.prev.set && r.lines == 0 && sameSnapshots(r.prevM, r.m) {
		err = x.touch(ctx, tenant, r)
		res := Result{ManifestKey: r.prev.key, Watermark: r.prev.watermark, Pending: r.pending}
		return res, wrapRun(tenant, err)
	}
	var key string
	if err == nil {
		key, err = x.putManifest(ctx, r)
	}
	if err == nil {
		err = x.publish(ctx, tenant, r, key)
	}
	if errors.Is(err, ErrMoved) {
		// The lost run's manifest and files go with the winner's
		// superseded ones (Collect).
		err = errors.Join(err, x.abandon(ctx, tenant, key, r.m))
	}
	if err != nil {
		return Result{}, wrapRun(tenant, err)
	}
	return Result{ManifestKey: key, Watermark: r.cutoff, Lines: r.lines, Published: true, Pending: r.pending}, nil
}

func wrapRun(tenant uuid.UUID, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("analytics: extract tenant %s: %w", tenant, err)
}

// read takes the pointer, the cutoff and every dataset from one snapshot of
// the tenant's rows, writing the files as it goes, one posting month at a
// time, so a backlog of any length needs one month in memory.
func (x Extractor) read(ctx context.Context, tenant uuid.UUID, begin database.BeginFunc) (*run, error) {
	r := &run{x: x, tenant: tenant}
	job := uow.Job{Begin: begin, Name: "analytics: read",
		Scope: database.Scope{Role: x.Role, TenantID: tenant, ReadOnly: true, Isolation: pgx.RepeatableRead}}
	err := job.Tx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		if err := r.start(ctx, tx); err != nil {
			return false, err
		}
		if err := r.glLines(ctx, tx); err != nil {
			return false, err
		}
		return false, r.snapshots(ctx, tx)
	})
	return r, err
}

// start reads the pointer, the previous manifest and the cutoff.
func (r *run) start(ctx context.Context, tx pgx.Tx) error {
	var err error
	if r.prev, err = current(ctx, tx); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT pg_snapshot_xmin(pg_current_snapshot())::text, now()`).Scan((*xid)(&r.cutoff), &r.asOf); err != nil {
		return fmt.Errorf("cutoff: %w", err)
	}
	// A cutoff at the watermark reads the empty interval. Below it, the
	// database was restored or moved under the store: appending from the
	// stored watermark would skip the entries posted with the transaction
	// ids in between, so the run stops until the store is rebuilt
	// (ADR-0044).
	if r.prev.set && r.cutoff < r.prev.watermark {
		return fmt.Errorf("%w: watermark %d, cutoff %d", ErrBehind, r.prev.watermark, r.cutoff)
	}
	if r.prevM, err = r.x.manifest(ctx, r.tenant, r.prev); err != nil {
		return err
	}
	r.m = Manifest{TenantID: r.tenant, Datasets: map[string]Dataset{}}
	return nil
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
// run read, and lists the manifest it replaced as superseded: a run that
// lost the race reports ErrMoved and changes nothing.
func (x Extractor) publish(ctx context.Context, tenant uuid.UUID, r *run, key string) error {
	return x.onPointer(ctx, tenant, func(ctx context.Context, tx pgx.Tx) (pgconn.CommandTag, error) {
		tag, err := movePointer(ctx, tx, r, key)
		if err != nil || tag.RowsAffected() == 0 || !r.prev.set {
			return tag, err
		}
		// A manifest published again (the same files after a change and
		// its reversal) may still be listed: Collect keeps what the
		// pointer names.
		if _, err := tx.Exec(ctx, supersedeSQL, r.prev.key, dropped(r.prevM, r.prev.key, r.m)); err != nil {
			return tag, fmt.Errorf("list %s superseded: %w", r.prev.key, err)
		}
		return tag, nil
	})
}

// supersedeSQL lists a manifest as superseded now with what it dropped;
// listed again, it is kept from now on.
const supersedeSQL = `INSERT INTO analytics.superseded (tenant_id, manifest_key, dropped) VALUES (platform.current_tenant(), $1, $2)
	ON CONFLICT (tenant_id, manifest_key) DO UPDATE SET superseded_at = excluded.superseded_at, dropped = excluded.dropped`

// dropped are the keys old (named key) listed, itself included, that its
// successor next does not.
func dropped(old Manifest, key string, next Manifest) []string {
	kept := map[string]bool{}
	for _, k := range files(next) {
		kept[k] = true
	}
	out := []string{key}
	for _, k := range files(old) {
		if !kept[k] {
			out = append(out, k)
		}
	}
	return out
}

func movePointer(ctx context.Context, tx pgx.Tx, r *run, key string) (pgconn.CommandTag, error) {
	wm := strconv.FormatUint(r.cutoff, 10)
	sql, args := `INSERT INTO analytics.snapshot (tenant_id, manifest_key, watermark, as_of, checked_at)
		VALUES (platform.current_tenant(), $1, $2::text::xid8, $3, $4) ON CONFLICT (tenant_id) DO NOTHING`, []any{key, wm, r.m.asOf(), r.checked()}
	if r.prev.set {
		sql, args = `UPDATE analytics.snapshot SET manifest_key = $1, watermark = $2::text::xid8, as_of = $3, published_at = now(),
			checked_at = greatest(checked_at, $4) WHERE manifest_key = $5`, append(args, r.prev.key)
	}
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return tag, fmt.Errorf("move the pointer to %s: %w", key, err)
	}
	return tag, nil
}

// abandon lists the manifest of a run that lost the race as superseded
// with every file it lists, so the files it wrote are collected with it;
// those the winner shares are kept, since Collect keeps what the pointer
// names.
func (x Extractor) abandon(ctx context.Context, tenant uuid.UUID, key string, lost Manifest) error {
	job := uow.Job{Begin: x.Primary, Name: "analytics: abandon", Scope: database.Scope{Role: x.Role, TenantID: tenant}}
	return job.Tx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		if _, err := tx.Exec(ctx, supersedeSQL, key, dropped(lost, key, Manifest{})); err != nil {
			return false, fmt.Errorf("abandon %s: %w", key, err)
		}
		return true, nil
	})
}

// reader is where a run for a change at lsn reads: the replica once it has
// replayed past lsn, the primary until then.
func (x Extractor) reader(ctx context.Context, lsn string) (database.BeginFunc, error) {
	if lsn == "" {
		return x.Replica, nil
	}
	replayed := x.Replayed
	if replayed == nil {
		replayed = x.replayed
	}
	ok, err := replayed(ctx, lsn)
	if err != nil {
		return nil, err
	}
	if !ok {
		return x.Primary, nil
	}
	return x.Replica, nil
}

// replayed asks the replica whether it has replayed past lsn, in a
// transaction of its own: replay only advances, so the reading transaction
// begun after it sees at least as much. A server not in recovery is the
// primary, current by definition.
func (x Extractor) replayed(ctx context.Context, lsn string) (bool, error) {
	var ok bool
	job := uow.Job{Begin: x.Replica, Name: "analytics: replay", Scope: database.Scope{Role: x.Role, ReadOnly: true}}
	err := job.Tx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		if err := tx.QueryRow(ctx, `SELECT NOT pg_is_in_recovery() OR coalesce(pg_last_wal_replay_lsn() >= $1::pg_lsn, false)`, lsn).Scan(&ok); err != nil {
			return false, fmt.Errorf("replica replay: %w", err)
		}
		return false, nil
	})
	return ok, err
}

// touch records a run that found nothing new as the pointer's check time,
// guarded like a publication; with entries pending it changes nothing.
func (x Extractor) touch(ctx context.Context, tenant uuid.UUID, r *run) error {
	if r.pending {
		return nil
	}
	return x.onPointer(ctx, tenant, func(ctx context.Context, tx pgx.Tx) (pgconn.CommandTag, error) {
		return tx.Exec(ctx, `UPDATE analytics.snapshot SET checked_at = greatest(checked_at, $1) WHERE manifest_key = $2`, r.asOf, r.prev.key)
	})
}

// checked is the check time a publication records: none while journal
// entries wait above the cutoff, so the tenant is not taken for current.
func (r *run) checked() *time.Time {
	if r.pending {
		return nil
	}
	return &r.asOf
}

// onPointer runs a guarded write of the tenant's pointer on the primary;
// one that changed no row lost the race to another run.
func (x Extractor) onPointer(ctx context.Context, tenant uuid.UUID, write func(context.Context, pgx.Tx) (pgconn.CommandTag, error)) error {
	job := uow.Job{Begin: x.Primary, Name: "analytics: publish", Scope: database.Scope{Role: x.Role, TenantID: tenant}}
	return job.Tx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		tag, err := write(ctx, tx)
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
