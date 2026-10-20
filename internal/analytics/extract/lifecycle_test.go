package extract_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/analytics/extract"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// files lists a manifest's file keys.
func files(m extract.Manifest) []string {
	var out []string
	for _, d := range m.Datasets {
		for _, f := range d.Files {
			out = append(out, f.Key)
		}
	}
	return out
}

// expire moves the tenant's superseded manifests back beyond Keep.
func (e env) expire(t *testing.T) {
	t.Helper()
	pgtest.Exec(t, `UPDATE analytics.superseded SET superseded_at = superseded_at - interval '8 days' WHERE tenant_id = $1`, e.Tenant.ID)
}

func (e env) collect(t *testing.T) int {
	t.Helper()
	n, err := e.x.Collect(context.Background(), e.Tenant.ID)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	return n
}

// TestCollect: a superseded manifest's files stay for Keep, then those no
// manifest in use lists go with it, once.
func TestCollect(t *testing.T) {
	e := setup(t)
	e.post(t, "2026-03-02", "1.00")
	first := e.run(t).ManifestKey
	e.post(t, "2026-03-03", "2.00")
	second := e.run(t).ManifestKey
	old, cur := e.manifest(t, first), e.manifest(t, second)
	if n := e.collect(t); n != 0 {
		t.Fatalf("collect within Keep: got %d deleted, want 0", n)
	}
	e.expire(t)
	var gone []string
	for _, k := range append(files(old), first) {
		if !slices.Contains(files(cur), k) {
			gone = append(gone, k)
		}
	}
	if n := e.collect(t); n != len(gone) || len(gone) < 2 {
		t.Fatalf("collect: got %d deleted, want %d (the manifest and its changed snapshot files)", n, len(gone))
	}
	for _, k := range gone {
		if _, ok := e.server.Object(k); ok {
			t.Errorf("%s: got kept, want deleted", k)
		}
	}
	for _, k := range append(files(cur), second) {
		if _, ok := e.server.Object(k); !ok {
			t.Errorf("%s of the current manifest: got deleted, want kept", k)
		}
	}
	var rows int
	pgtest.Scan(t, `SELECT count(*) FROM analytics.superseded WHERE tenant_id = $1`, []any{e.Tenant.ID}, &rows)
	if rows != 0 {
		t.Errorf("superseded rows: got %d, want 0", rows)
	}
	if n := e.collect(t); n != 0 {
		t.Errorf("collect again: got %d deleted, want 0", n)
	}
	// A dropped key already gone is nothing to delete; its row goes.
	goneKey := extract.ManifestKey(e.Tenant.ID, 3, "")
	pgtest.Exec(t, `INSERT INTO analytics.superseded (tenant_id, manifest_key, superseded_at, dropped) VALUES ($1, $2, now() - interval '8 days', ARRAY[$2])`,
		e.Tenant.ID, goneKey)
	e.collect(t)
	pgtest.Scan(t, `SELECT count(*) FROM analytics.superseded WHERE tenant_id = $1`, []any{e.Tenant.ID}, &rows)
	if rows != 0 {
		t.Errorf("rows after collecting a key already gone: got %d, want 0", rows)
	}
	// The pointer read back is still the second run's, which reads alike.
	if res := e.run(t); res.Published || res.ManifestKey != second {
		t.Errorf("run after collect: got %+v, want a check of %s", res, second)
	}
}

// listless is a store that cannot list or delete.
type listless struct{ blobstore.Store }

// deleteFails fails deleting keys holding part.
type deleteFails struct {
	blobstore.S3
	part string
}

func (d deleteFails) Delete(ctx context.Context, key string) error {
	if strings.Contains(key, d.part) {
		return errors.New("delete refused")
	}
	return d.S3.Delete(ctx, key)
}

// listFails fails every listing.
type listFails struct{ blobstore.S3 }

func (listFails) List(context.Context, string) ([]string, error) {
	return nil, errors.New("list refused")
}

// getFails fails reading keys holding part.
type getFails struct {
	blobstore.S3
	part string
}

func (g getFails) Get(ctx context.Context, key string) ([]byte, string, error) {
	if strings.Contains(key, g.part) {
		return nil, "", errors.New("get refused")
	}
	return g.S3.Get(ctx, key)
}

func TestCollectFailures(t *testing.T) {
	e := setup(t)
	e.post(t, "2026-03-02", "1.00")
	e.run(t)
	e.post(t, "2026-03-03", "2.00")
	second := e.run(t).ManifestKey
	e.expire(t)
	e.x.Keep = 24 * time.Hour
	for name, c := range map[string]struct {
		store blobstore.Store
		want  string
	}{
		"no eraser":       {listless{e.server.Store()}, "cannot delete"},
		"manifest in use": {getFails{S3: e.server.Store(), part: second}, "get refused"},
		"delete":          {deleteFails{S3: e.server.Store(), part: "manifest"}, "delete refused"},
	} {
		t.Run(name, func(t *testing.T) {
			x := e.x
			x.Store = c.store
			_, err := x.Collect(context.Background(), e.Tenant.ID)
			pgtest.WantErr(t, err, c.want)
		})
	}
	t.Run("database", func(t *testing.T) {
		pool := pgtest.Pool(t)
		for n := 1; ; n++ {
			count := 0
			x := e.x
			x.Primary = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
				tx, err := pool.Begin(ctx, s)
				return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, err
			}
			_, err := x.Collect(context.Background(), e.Tenant.ID)
			if err == nil {
				if n < 3 {
					t.Fatalf("statement %d: collect succeeded, want every statement covered", n)
				}
				return
			}
			pgtest.WantErr(t, err, pgtest.ErrInjected.Error())
		}
	})
}

// TestCompaction: a month's journal parts are merged into one when they
// would reach CompactAt, named by the interval they cover together.
func TestCompaction(t *testing.T) {
	e := setup(t)
	e.x.CompactAt = 3
	var lines int
	for i, amount := range []string{"1.00", "2.00", "3.00"} {
		e.post(t, fmt.Sprintf("2026-03-%02d", 3-i), amount) // posted out of date order: the merge sorts
		e.run(t)
		lines += 2
	}
	var key string
	pgtest.Scan(t, `SELECT manifest_key FROM analytics.snapshot WHERE tenant_id = $1`, []any{e.Tenant.ID}, &key)
	gl := e.manifest(t, key).Datasets["gl_lines"]
	if len(gl.Files) != 1 || gl.Rows != int64(lines) {
		t.Fatalf("gl_lines: got %d files and %d rows, want 1 file of %d", len(gl.Files), gl.Rows, lines)
	}
	if !strings.Contains(gl.Files[0].Key, "/part-00000000000000000000-") {
		t.Errorf("merged part: got %s, want it named from the first part's start", gl.Files[0].Key)
	}
	rows := e.rows(t, gl.Files[0].Key)
	if len(rows) != lines {
		t.Fatalf("merged rows: got %d, want %d", len(rows), lines)
	}
	if first := rows[0].AmountMinor; first != 300 && first != -300 {
		t.Errorf("first merged row: got %d, want a line of the 1 March entry (3.00)", first)
	}
	// The replaced parts leave with the manifest that listed them.
	e.expire(t)
	if n := e.collect(t); n < 3 {
		t.Errorf("collect after compaction: got %d deleted, want the two replaced parts and manifests", n)
	}
}

func TestCompactionFailures(t *testing.T) {
	for name, c := range map[string]struct {
		store func(e env) blobstore.Store
		want  string
	}{
		"read": {func(e env) blobstore.Store {
			return failingStore{Store: e.server.Store(), method: "get", part: "gl_lines"}
		}, "read"},
		"changed": {func(e env) blobstore.Store { return changedParts{e.server.Store()} }, "not the file the manifest lists"},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			e.x.CompactAt = 2
			e.post(t, "2026-03-02", "1.00")
			e.run(t)
			e.post(t, "2026-03-03", "1.00")
			x := e.x
			x.Store = c.store(e)
			_, err := x.Run(context.Background(), e.Tenant.ID)
			pgtest.WantErr(t, err, c.want)
		})
	}
	t.Run("not parquet", func(t *testing.T) {
		e := setup(t)
		e.x.CompactAt = 2
		e.post(t, "2026-03-02", "1.00")
		res := e.run(t)
		// The pointer moved to a manifest listing, as its journal part, bytes
		// that are what it says but not Parquet.
		m := e.manifest(t, res.ManifestKey)
		gl := m.Datasets["gl_lines"]
		bad := strings.Replace(gl.Files[0].Key, "/part-", "/part-bad-", 1)
		sum := sha256.Sum256([]byte("garbage"))
		gl.Files = []extract.File{{Key: bad, Size: 7, SHA256: hex.EncodeToString(sum[:]), Rows: 1}}
		m.Datasets["gl_lines"] = gl
		b, _ := json.Marshal(m) // plain data
		key := extract.ManifestKey(e.Tenant.ID, res.Watermark, "0123456789abcdef")
		for k, v := range map[string][]byte{bad: []byte("garbage"), key: b} {
			if err := e.server.Store().Put(context.Background(), k, v, blobstore.PutOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		pgtest.Exec(t, `UPDATE analytics.snapshot SET manifest_key = $2 WHERE tenant_id = $1`, e.Tenant.ID, key)
		e.post(t, "2026-03-03", "1.00")
		_, err := e.x.Run(context.Background(), e.Tenant.ID)
		pgtest.WantErr(t, err, "read "+bad)
	})
}

// changedParts answers other bytes for journal parts.
type changedParts struct{ blobstore.S3 }

func (c changedParts) Get(ctx context.Context, key string) ([]byte, string, error) {
	b, ct, err := c.S3.Get(ctx, key)
	if strings.Contains(key, "/gl_lines/") {
		b = append(b, 0)
	}
	return b, ct, err
}

// TestReplicaWait: a run for a change the replica has not replayed reads on
// the primary; one it has, on the replica.
func TestReplicaWait(t *testing.T) {
	e := setup(t)
	e.post(t, "2026-03-02", "1.00")
	replica := 0
	x := e.x
	x.Replica = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		replica++
		return e.x.Replica(ctx, s)
	}
	for name, c := range map[string]struct {
		replayed    bool
		wantReplica int
	}{"lagging": {false, 0}, "replayed": {true, 1}} {
		t.Run(name, func(t *testing.T) {
			replica = 0
			x.Replayed = func(context.Context, string) (bool, error) { return c.replayed, nil }
			if _, err := x.RunAt(context.Background(), e.Tenant.ID, "0/1"); err != nil {
				t.Fatal(err)
			}
			if replica != c.wantReplica {
				t.Errorf("reads on the replica: got %d, want %d", replica, c.wantReplica)
			}
		})
	}
	t.Run("asked", func(t *testing.T) {
		x.Replayed = nil
		replica = 0
		if _, err := x.RunAt(context.Background(), e.Tenant.ID, "0/1"); err != nil {
			t.Fatal(err)
		}
		if replica != 2 { // the question and the read: a primary is current
			t.Errorf("transactions on the replica: got %d, want 2", replica)
		}
	})
	t.Run("failing", func(t *testing.T) {
		x.Replayed = func(context.Context, string) (bool, error) { return false, errors.New("replica gone") }
		_, err := x.RunAt(context.Background(), e.Tenant.ID, "0/1")
		pgtest.WantErr(t, err, "replica gone")
		x.Replayed = nil
		x.Replica = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := e.x.Replica(ctx, s)
			return &pgtest.FailingTx{Tx: tx, FailAt: 1}, err
		}
		_, err = x.RunAt(context.Background(), e.Tenant.ID, "0/1")
		pgtest.WantErr(t, err, "replica replay")
	})
}

// held holds a transaction with an id open until the test ends, so the
// snapshot horizon stays at its id, and returns the id.
func held(t *testing.T) uint64 {
	t.Helper()
	ctx := context.Background()
	tx, err := pgtest.Pool(t).Begin(ctx, database.Scope{Role: "erp_worker"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) }) // nothing was written
	var id uint64
	if err := tx.QueryRow(ctx, `SELECT pg_current_xact_id()::text::numeric`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestSchedulerRebuilds: a run behind its watermark rebuilds the tenant's
// store when the database is behind it (a restore) and runs from the
// start; behind the replica alone, it is retried.
func TestSchedulerRebuilds(t *testing.T) {
	c := newScheduled(t)
	c.post(t, "2026-03-02", "1.00")
	ahead := uint64(1) << 62
	stray := "analytics/v1/tenant=" + c.Tenant.ID.String() + "/gl_lines/month=2026-03/part-stray.parquet"
	if err := c.server.Store().Put(context.Background(), stray, []byte("x"), blobstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	pgtest.Exec(t, `INSERT INTO analytics.snapshot (tenant_id, manifest_key, watermark, as_of) VALUES ($1, $2, $3::text::xid8, now())`,
		c.Tenant.ID, extract.ManifestKey(c.Tenant.ID, ahead, ""), fmt.Sprint(ahead))
	c.mark(t, "ledger")
	c.runOnce(t)
	if _, ok := c.server.Object(stray); ok {
		t.Error("stray file: got kept, want deleted by the rebuild")
	}
	var wm uint64
	pgtest.Scan(t, `SELECT watermark::text::numeric FROM analytics.snapshot WHERE tenant_id = $1`, []any{c.Tenant.ID}, &wm)
	if wm >= ahead {
		t.Errorf("watermark: got %d, want a rebuilt one below %d", wm, ahead)
	}

	t.Run("replica behind", func(t *testing.T) {
		// A watermark just above the open transaction: the replica's
		// horizon (here the primary's) is below it, the database's next
		// transaction id, once a later one commits, above it.
		next := fmt.Sprint(held(t) + 1)
		pgtest.Exec(t, `UPDATE analytics.snapshot SET watermark = $2::text::xid8, manifest_key = 'analytics/v1/tenant=' || tenant_id || '/manifest-' || lpad($2::text, 20, '0') || '.json'
			WHERE tenant_id = $1`, c.Tenant.ID, next)
		c.aged(t)
		c.mark(t, "ledger")
		_, err := c.s.RunOnce(context.Background())
		if !errors.Is(err, extract.ErrBehind) {
			t.Fatalf("round: got %v, want ErrBehind", err)
		}
		var still string
		pgtest.Scan(t, `SELECT watermark::text FROM analytics.snapshot WHERE tenant_id = $1`, []any{c.Tenant.ID}, &still)
		if still != next {
			t.Errorf("watermark: got %s, want %s kept for the retry", still, next)
		}
	})
}

func TestRebuildAhead(t *testing.T) {
	e := setup(t)
	e.post(t, "2026-03-02", "1.00")
	e.run(t)
	pool := pgtest.Pool(t)
	ahead := uint64(1) << 62
	pgtest.Exec(t, `UPDATE analytics.snapshot SET watermark = $2::text::xid8, manifest_key = $3 WHERE tenant_id = $1`,
		e.Tenant.ID, fmt.Sprint(ahead), extract.ManifestKey(e.Tenant.ID, ahead, ""))
	t.Cleanup(func() { pgtest.Exec(t, `DELETE FROM analytics.extract_due WHERE tenant_id = $1`, e.Tenant.ID) })
	rebuilt, err := extract.RebuildAhead(context.Background(), pool.Begin, "erp_worker", e.server.Store())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(rebuilt, e.Tenant.ID) {
		t.Errorf("rebuilt: got %v, want %s among them", rebuilt, e.Tenant.ID)
	}
	if left := e.server.Keys(); slices.ContainsFunc(left, func(k string) bool { return strings.Contains(k, e.Tenant.ID.String()) }) {
		t.Errorf("files: got %v left, want none of the tenant", left)
	}
	var pointers, due int
	pgtest.Scan(t, `SELECT (SELECT count(*) FROM analytics.snapshot WHERE tenant_id = $1), (SELECT count(*) FROM analytics.extract_due WHERE tenant_id = $1)`,
		[]any{e.Tenant.ID}, &pointers, &due)
	if pointers != 0 || due != 1 {
		t.Errorf("after the rebuild: got %d pointers and %d due rows, want 0 and 1", pointers, due)
	}
	// Run again, a rebuild finds nothing more to do.
	if _, err := extract.RebuildAhead(context.Background(), pool.Begin, "erp_worker", e.server.Store()); err != nil {
		t.Fatal(err)
	}
	if res := e.run(t); !res.Published || res.Lines != 2 {
		t.Errorf("first run after the rebuild: got %+v, want every line published", res)
	}
}

func TestRebuildFailures(t *testing.T) {
	e := setup(t)
	pool := pgtest.Pool(t)
	ctx := context.Background()
	pgtest.WantErr(t, extract.Rebuild(ctx, pool.Begin, "erp_worker", listless{e.server.Store()}, e.Tenant.ID), "cannot delete")
	if err := e.server.Store().Put(ctx, "analytics/v1/tenant="+e.Tenant.ID.String()+"/x.json", []byte("{}"), blobstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	pgtest.WantErr(t, extract.Rebuild(ctx, pool.Begin, "erp_worker", deleteFails{S3: e.server.Store(), part: "x.json"}, e.Tenant.ID), "delete refused")
	pgtest.WantErr(t, extract.Rebuild(ctx, pool.Begin, "erp_worker", listFails{e.server.Store()}, e.Tenant.ID), "list refused")
	failing := func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, s)
		return &pgtest.FailingTx{Tx: tx, FailAt: 1}, err
	}
	pgtest.WantErr(t, extract.Rebuild(ctx, failing, "erp_worker", e.server.Store(), e.Tenant.ID), pgtest.ErrInjected.Error())
	_, err := extract.RebuildAhead(ctx, failing, "erp_worker", e.server.Store())
	pgtest.WantErr(t, err, pgtest.ErrInjected.Error())
	ahead := uint64(1) << 62
	pgtest.Exec(t, `INSERT INTO analytics.snapshot (tenant_id, manifest_key, watermark, as_of) VALUES ($1, $2, $3::text::xid8, now())`,
		e.Tenant.ID, extract.ManifestKey(e.Tenant.ID, ahead, ""), fmt.Sprint(ahead))
	t.Cleanup(func() {
		pgtest.Exec(t, `DELETE FROM analytics.snapshot WHERE tenant_id = $1`, e.Tenant.ID)
		pgtest.Exec(t, `DELETE FROM analytics.extract_due WHERE tenant_id = $1`, e.Tenant.ID)
	})
	_, err = extract.RebuildAhead(ctx, pool.Begin, "erp_worker", listless{e.server.Store()})
	pgtest.WantErr(t, err, "cannot delete")
}

// sweepTx answers the sweep's statement in a transaction that is never
// committed, after reporting whether the tenant was marked.
type sweepTx struct {
	pgx.Tx
	check func(pgx.Tx)
}

func (s sweepTx) Commit(context.Context) error {
	s.check(s.Tx)
	return nil // the deferred rollback discards the marks of other tests' tenants
}

// TestSweep: the sweep marks due an active tenant whose store was not found
// current within the target, and not one found current.
func TestSweep(t *testing.T) {
	e := setup(t)
	pool := pgtest.Pool(t)
	marked := func(t *testing.T) bool {
		t.Helper()
		var got bool
		begin := func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			return sweepTx{Tx: tx, check: func(tx pgx.Tx) {
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM analytics.extract_due WHERE tenant_id = $1)`, e.Tenant.ID).Scan(&got); err != nil {
					t.Fatal(err)
				}
			}}, err
		}
		if _, err := extract.Sweep(context.Background(), begin, "erp_worker", 0); err != nil {
			t.Fatalf("sweep: %v", err)
		}
		return got
	}
	if !marked(t) {
		t.Error("tenant without a store: got not marked, want marked")
	}
	e.post(t, "2026-03-02", "1.00")
	e.run(t)
	if marked(t) {
		t.Error("tenant found current: got marked, want left alone")
	}
	pgtest.Exec(t, `UPDATE analytics.snapshot SET checked_at = checked_at - interval '16 minutes' WHERE tenant_id = $1`, e.Tenant.ID)
	if !marked(t) {
		t.Error("tenant not found current for 16 minutes: got not marked, want marked")
	}
	failing := func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, s)
		return &pgtest.FailingTx{Tx: tx, FailAt: 1}, err
	}
	_, err := extract.Sweep(context.Background(), failing, "erp_worker", time.Minute)
	pgtest.WantErr(t, err, "sweep")
}

// noSweep stands in for the sweep's statement, which would mark every
// tenant of the shared database, and counts it.
type noSweep struct {
	pgx.Tx
	sweeps *int
}

func (n noSweep) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "FROM tenancy.tenant t") {
		*n.sweeps++
		return pgconn.NewCommandTag("INSERT 0 0"), nil
	}
	return n.Tx.Exec(ctx, sql, args...)
}

// TestSchedulerSweeps: a scheduler sweeps on its first round and then once
// per SweepEvery; a failed sweep does not stop the round.
func TestSchedulerSweeps(t *testing.T) {
	c := newScheduled(t)
	pool := pgtest.Pool(t)
	sweeps := 0
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	c.s.SweepEvery, c.s.Now = 0, func() time.Time { return now }
	c.s.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, s)
		return noSweep{Tx: tx, sweeps: &sweeps}, err
	}
	for _, step := range []struct {
		after time.Duration
		want  int
	}{{0, 1}, {30 * time.Minute, 1}, {31 * time.Minute, 2}} {
		now = now.Add(step.after)
		if _, err := c.s.RunOnce(context.Background()); err != nil { // c.s itself: it remembers its last sweep
			t.Fatal(err)
		}
		if sweeps != step.want {
			t.Errorf("after %v: got %d sweeps, want %d", step.after, sweeps, step.want)
		}
	}
	c.s.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, s)
		return &pgtest.FailingTx{Tx: tx, FailAt: 1}, err
	}
	now = now.Add(2 * time.Hour)
	_, err := c.s.RunOnce(context.Background())
	pgtest.WantErr(t, err, "sweep")
}

// TestSchedulerCollects: after a tenant's run the scheduler collects what
// its superseded manifests left; a failed collection is reported, the run
// recorded.
func TestSchedulerCollects(t *testing.T) {
	c := newScheduled(t)
	c.post(t, "2026-03-02", "1.00")
	first := c.run(t).ManifestKey
	c.post(t, "2026-03-03", "1.00")
	c.mark(t, "ledger")
	c.runOnce(t)
	c.expire(t)
	c.aged(t)
	c.mark(t, "ledger")
	c.s.Extractor.Store = deleteFails{S3: c.server.Store(), part: first}
	_, err := c.s.RunOnce(context.Background())
	pgtest.WantErr(t, err, "delete refused")
	if _, done := c.due(t); done == nil {
		t.Error("run with a failed collection: got not done, want done")
	}
	c.s.Extractor.Store = c.server.Store()
	c.aged(t)
	c.mark(t, "ledger")
	c.runOnce(t)
	if _, ok := c.server.Object(first); ok {
		t.Errorf("superseded manifest %s: got kept, want collected", first)
	}
}

// TestSchedulerRebuildFailures: the check of the primary failing, or the
// rebuild, leaves the tenant for its next claim.
func TestSchedulerRebuildFailures(t *testing.T) {
	c := newScheduled(t)
	ahead := uint64(1) << 62
	pgtest.Exec(t, `INSERT INTO analytics.snapshot (tenant_id, manifest_key, watermark, as_of) VALUES ($1, $2, $3::text::xid8, now())`,
		c.Tenant.ID, extract.ManifestKey(c.Tenant.ID, ahead, ""), fmt.Sprint(ahead))
	t.Cleanup(func() { pgtest.Exec(t, `DELETE FROM analytics.snapshot WHERE tenant_id = $1`, c.Tenant.ID) })
	pool := pgtest.Pool(t)
	c.mark(t, "ledger")
	claims := 0
	c.s.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, s)
		if claims++; claims == 1 {
			return tx, err // the claim
		}
		return &pgtest.FailingTx{Tx: tx, FailAt: 1}, err
	}
	_, err := c.s.RunOnce(context.Background())
	pgtest.WantErr(t, err, pgtest.ErrInjected.Error())
	c.aged(t)
	c.s.Begin = pool.Begin
	c.s.Extractor.Store = listless{c.server.Store()}
	_, err = c.s.RunOnce(context.Background())
	pgtest.WantErr(t, err, "cannot delete")
}

// gets counts the reads of a store.
type gets struct {
	blobstore.S3
	n *int
}

func (g gets) Get(ctx context.Context, key string) ([]byte, string, error) {
	*g.n++
	return g.S3.Get(ctx, key)
}

// TestCollectReadsOneManifest: however many supersessions a tenant has,
// Collect reads the current manifest alone; a file dropped and taken up
// again stays.
func TestCollectReadsOneManifest(t *testing.T) {
	e := setup(t)
	for i := range 4 {
		e.post(t, fmt.Sprintf("2026-03-%02d", i+1), "1.00")
		e.run(t)
	}
	pgtest.Exec(t, `UPDATE analytics.superseded SET superseded_at = superseded_at - interval '8 days'
		WHERE tenant_id = $1 AND manifest_key = (SELECT min(manifest_key) FROM analytics.superseded WHERE tenant_id = $1)`, e.Tenant.ID)
	n := 0
	x := e.x
	x.Store = gets{S3: e.server.Store(), n: &n}
	if _, err := x.Collect(context.Background(), e.Tenant.ID); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("manifests read: got %d, want 1", n)
	}
}

// TestCollectWithoutCurrent: a pointer naming a manifest already gone, or
// none (a rebuild under way), keeps nothing beyond the recent drops.
func TestCollectWithoutCurrent(t *testing.T) {
	e := setup(t)
	e.post(t, "2026-03-02", "1.00")
	e.run(t)
	e.post(t, "2026-03-03", "1.00")
	res := e.run(t)
	e.expire(t)
	pgtest.Exec(t, `UPDATE analytics.snapshot SET manifest_key = $2 WHERE tenant_id = $1`, e.Tenant.ID, extract.ManifestKey(e.Tenant.ID, res.Watermark, "0123456789abcdef"))
	if n := e.collect(t); n == 0 {
		t.Error("collect under a pointer to a manifest gone: got nothing deleted, want the expired drops")
	}
	e.post(t, "2026-03-04", "1.00")
	// A key dropped again since stays for that supersession's Keep.
	pgtest.Exec(t, `INSERT INTO analytics.superseded (tenant_id, manifest_key, superseded_at, dropped) VALUES
		($1, $2, now() - interval '8 days', ARRAY[$2, 'taken-up-again']), ($1, $3, now(), ARRAY['taken-up-again'])`,
		e.Tenant.ID, res.ManifestKey, extract.ManifestKey(e.Tenant.ID, res.Watermark, "fedcba9876543210"))
	pgtest.Exec(t, `DELETE FROM analytics.snapshot WHERE tenant_id = $1`, e.Tenant.ID)
	if n := e.collect(t); n != 1 {
		t.Errorf("collect without a pointer: got %d deleted, want 1", n)
	}
}
