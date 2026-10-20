package extract

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

// Keep is how long a superseded manifest's files stay after its
// replacement was published: a query that read the old pointer finishes
// long before (ADR-0044).
const Keep = 7 * 24 * time.Hour

func (x Extractor) keep() time.Duration {
	if x.Keep <= 0 {
		return Keep
	}
	return x.Keep
}

// ErrNoEraser is returned when the store cannot list and delete, which the
// lifecycle and a rebuild need.
var ErrNoEraser = errors.New("analytics: the object store cannot delete files")

// Collect deletes what the tenant's manifests superseded more than Keep ago
// listed and no manifest still in use lists (the current one, and those
// superseded since), then those manifests and their rows, and returns how
// many objects it deleted. It is idempotent: an object already gone is
// nothing to delete, and a row stays until its objects are gone.
func (x Extractor) Collect(ctx context.Context, tenant uuid.UUID) (int, error) {
	eraser, ok := x.Store.(blobstore.Eraser)
	if !ok {
		return 0, ErrNoEraser
	}
	expired, inUse, err := x.superseded(ctx, tenant)
	if err != nil || len(expired) == 0 {
		return 0, wrapCollect(tenant, err)
	}
	keep := map[string]bool{}
	for _, key := range inUse {
		keep[key] = true
		m, err := x.manifestAt(ctx, tenant, key)
		if err != nil {
			return 0, wrapCollect(tenant, err)
		}
		for _, f := range files(m) {
			keep[f] = true
		}
	}
	n := 0
	for _, key := range expired {
		m, err := x.manifestAt(ctx, tenant, key)
		if err != nil {
			return n, wrapCollect(tenant, err)
		}
		for _, k := range append(files(m), key) {
			if keep[k] {
				continue
			}
			if err := eraser.Delete(ctx, k); err != nil {
				return n, wrapCollect(tenant, err)
			}
			keep[k] = true // listed by two expired manifests, deleted once
			n++
		}
	}
	return n, wrapCollect(tenant, x.forget(ctx, tenant, expired))
}

func wrapCollect(tenant uuid.UUID, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("analytics: collect tenant %s: %w", tenant, err)
}

// superseded reads the tenant's manifests superseded more than Keep ago and
// those in use: the current one and the ones superseded since.
func (x Extractor) superseded(ctx context.Context, tenant uuid.UUID) (expired, inUse []string, err error) {
	job := uow.Job{Begin: x.Primary, Name: "analytics: superseded", Scope: database.Scope{Role: x.Role, TenantID: tenant, ReadOnly: true}}
	err = job.Tx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		rows, _ := tx.Query(ctx, `SELECT manifest_key, superseded_at < now() - $1 * interval '1 millisecond' FROM analytics.superseded
			UNION ALL SELECT manifest_key, false FROM analytics.snapshot ORDER BY 1`, x.keep().Milliseconds())
		var key string
		var old bool
		_, err := pgx.ForEachRow(rows, []any{&key, &old}, func() error {
			if old {
				expired = append(expired, key)
			} else {
				inUse = append(inUse, key)
			}
			return nil
		})
		if err != nil {
			return false, fmt.Errorf("superseded manifests: %w", err)
		}
		return false, nil
	})
	return expired, inUse, err
}

// manifestAt reads a manifest; one already deleted lists nothing.
func (x Extractor) manifestAt(ctx context.Context, tenant uuid.UUID, key string) (Manifest, error) {
	m, err := x.manifest(ctx, tenant, pointer{key: key, set: true})
	if errors.Is(err, blobstore.ErrNotFound) {
		return Manifest{}, nil
	}
	return m, err
}

// files are the keys of a manifest's files.
func files(m Manifest) []string {
	var out []string
	for _, d := range m.Datasets {
		for _, f := range d.Files {
			out = append(out, f.Key)
		}
	}
	return out
}

// forget deletes, as the worker role, the tenant's rows of collected
// manifests that have not been listed again since.
func (x Extractor) forget(ctx context.Context, tenant uuid.UUID, keys []string) error {
	job := uow.Job{Begin: x.Primary, Name: "analytics: forget", Scope: database.Scope{Role: x.WorkerRole}}
	return job.Tx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		if _, err := tx.Exec(ctx, `DELETE FROM analytics.superseded WHERE tenant_id = $1 AND manifest_key = ANY($2)
			AND superseded_at < now() - $3 * interval '1 millisecond'`, tenant, keys, x.keep().Milliseconds()); err != nil {
			return false, fmt.Errorf("forget collected manifests: %w", err)
		}
		return true, nil
	})
}

// Ahead lists the tenants whose watermark is ahead of the database: the
// database was restored (or moved) under their stores, whose transaction
// ids it never reached, so their stores must be rebuilt (ADR-0040). It
// runs on the primary as the worker role, across tenants.
func Ahead(ctx context.Context, tx pgx.Tx) ([]uuid.UUID, error) {
	rows, _ := tx.Query(ctx, `SELECT tenant_id FROM analytics.snapshot WHERE watermark > pg_snapshot_xmax(pg_current_snapshot()) ORDER BY tenant_id`)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("analytics: stores ahead of the database: %w", err)
	}
	return ids, nil
}

// Rebuild deletes a tenant's store and marks the tenant due, so its next
// run extracts it from the start: the files first, so a reader holding the
// old pointer finds them gone and answers from the database, then, as the
// worker role, the pointer and its superseded list. Run again after a
// failure, it finishes what the last attempt left.
func Rebuild(ctx context.Context, begin database.BeginFunc, workerRole string, store blobstore.Store, tenant uuid.UUID) error {
	eraser, ok := store.(blobstore.Eraser)
	if !ok {
		return ErrNoEraser
	}
	keys, err := eraser.List(ctx, tenantPrefix(tenant)+"/")
	if err != nil {
		return fmt.Errorf("analytics: rebuild tenant %s: %w", tenant, err)
	}
	for _, k := range keys {
		if err := eraser.Delete(ctx, k); err != nil {
			return fmt.Errorf("analytics: rebuild tenant %s: %w", tenant, err)
		}
	}
	job := uow.Job{Begin: begin, Name: "analytics: rebuild", Scope: database.Scope{Role: workerRole}}
	return job.Tx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		if _, err := tx.Exec(ctx, `WITH p AS (DELETE FROM analytics.snapshot WHERE tenant_id = $1), s AS (DELETE FROM analytics.superseded WHERE tenant_id = $1)
			INSERT INTO analytics.extract_due AS d (tenant_id, changed_at) VALUES ($1, clock_timestamp())
			ON CONFLICT (tenant_id) DO UPDATE SET changed_at = greatest(d.changed_at, excluded.changed_at)`, tenant); err != nil {
			return false, fmt.Errorf("analytics: rebuild tenant %s: %w", tenant, err)
		}
		return true, nil
	})
}

// RebuildAhead rebuilds the store of every tenant whose watermark is ahead
// of the database and returns them (erp-server dr reconcile, ADR-0040).
func RebuildAhead(ctx context.Context, begin database.BeginFunc, workerRole string, store blobstore.Store) ([]uuid.UUID, error) {
	ahead, err := readAhead(ctx, begin, workerRole)
	if err != nil {
		return nil, err
	}
	for _, tenant := range ahead {
		if err := Rebuild(ctx, begin, workerRole, store, tenant); err != nil {
			return nil, err
		}
	}
	return ahead, nil
}

// readAhead runs Ahead on the primary as the worker role.
func readAhead(ctx context.Context, begin database.BeginFunc, workerRole string) ([]uuid.UUID, error) {
	var ahead []uuid.UUID
	job := uow.Job{Begin: begin, Name: "analytics: ahead", Scope: database.Scope{Role: workerRole, ReadOnly: true}}
	err := job.Tx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		var err error
		ahead, err = Ahead(ctx, tx)
		return false, err
	})
	return ahead, err
}
