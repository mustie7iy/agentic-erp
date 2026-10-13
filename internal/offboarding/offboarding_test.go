package offboarding_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/offboarding"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// failingStore fails every call.
type failingStore struct{ err error }

func (f failingStore) Put(context.Context, string, []byte, blobstore.PutOptions) error { return f.err }
func (f failingStore) Get(context.Context, string) ([]byte, string, error)             { return nil, "", f.err }
func (f failingStore) Stat(context.Context, string) (int64, string, error)             { return 0, "", f.err }

// plainStore is a store that cannot list or delete (no Eraser).
type plainStore struct{ blobstore.Store }

// brokenEraser lists but cannot delete, or cannot list.
type brokenEraser struct {
	blobstore.Store
	list, del error
}

func (b brokenEraser) List(ctx context.Context, prefix string) ([]string, error) {
	if b.list != nil {
		return nil, b.list
	}
	return b.Store.(blobstore.Eraser).List(ctx, prefix)
}
func (b brokenEraser) Delete(context.Context, string) error { return b.del }

func count(t *testing.T, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pgtest.Admin(t).QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func worker(t *testing.T, pool *database.Pool, store blobstore.Store, clock *time.Time) *offboarding.Worker {
	t.Helper()
	return &offboarding.Worker{Pool: pool, Store: store, AppRole: "erp_app", WorkerRole: "erp_worker", Grace: time.Hour, Retention: 24 * time.Hour,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return *clock }}
}

// TestOffboardingLifecycle: an offboarded tenant is exported to locked
// objects, kept for the grace period, then erased; its tenant row and its
// audit trail remain, and other tenants are untouched.
func TestOffboardingLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t)
	e := fixture.New(t, "payables.invoice.read")
	other := fixture.New(t, "payables.invoice.read")
	clock := time.Now().UTC().Truncate(time.Second)
	store := blobstore.Dir{Root: t.TempDir(), Now: func() time.Time { return clock }}
	w := worker(t, pool, store, &clock)
	w.ChunkRows = 1 // one row per object: every table with rows exports in parts
	tenant := e.Tenant.ID
	prefix := "tenants/" + tenant.String() + "/"
	// A document of the tenant, and an anchor locked for years.
	if err := store.Put(ctx, prefix+"documents/abc", []byte("%PDF"), blobstore.PutOptions{ContentType: "application/pdf"}); err != nil {
		t.Fatalf("store a document: %v", err)
	}
	if err := store.Put(ctx, prefix+"audit/anchors/000000000001.json", []byte("{}"), blobstore.PutOptions{ContentType: "application/json", RetainUntil: clock.Add(24 * time.Hour)}); err != nil {
		t.Fatalf("store an anchor: %v", err)
	}
	// Nothing to do while the tenant is active; an active tenant is not exported or erased on request either.
	if ex, er, err := w.RunOnce(ctx); err != nil || ex != 0 || er != 0 {
		t.Fatalf("active: %d %d %v", ex, er, err)
	}
	if _, err := w.Export(ctx, tenant); problem.From(err).Code != "tenancy.invalid_transition" {
		t.Fatalf("export of an active tenant: %v", err)
	}
	if _, err := w.Erase(ctx, tenant, offboarding.Actor, nil); problem.From(err).Code != "tenancy.invalid_transition" {
		t.Fatalf("erasure of an active tenant: %v", err)
	}
	pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'offboarded', version = version + 1 WHERE id = $1`, tenant)
	principals := count(t, `SELECT count(*) FROM identity.principal WHERE tenant_id = $1`, tenant)
	events := count(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1`, tenant)
	if principals == 0 || events == 0 {
		t.Fatalf("fixture: %d principals, %d audit events", principals, events)
	}
	// The round exports: locked objects per table and a manifest describing them; the tenant records it.
	ex, er, err := w.RunOnce(ctx)
	if err != nil || ex != 1 || er != 0 {
		t.Fatalf("export round: %d %d %v", ex, er, err)
	}
	var exportedAt, eraseAfter time.Time
	var ref string
	var erasedAt *time.Time
	if err := pgtest.Admin(t).QueryRow(ctx, `SELECT exported_at, export_ref, erase_after, erased_at FROM tenancy.tenant WHERE id = $1`, tenant).Scan(&exportedAt, &ref, &eraseAfter, &erasedAt); err != nil {
		t.Fatalf("read the tenant after export: %v", err)
	}
	if !exportedAt.Equal(clock) || !eraseAfter.Equal(clock.Add(time.Hour)) || erasedAt != nil || !strings.HasPrefix(ref, prefix+"exports/") || !strings.HasSuffix(ref, "/manifest.json") {
		t.Fatalf("tenant after export: %v %s %v %v", exportedAt, ref, eraseAfter, erasedAt)
	}
	body, ct, err := store.Get(ctx, ref)
	if err != nil || ct != "application/json" {
		t.Fatalf("manifest: %q %v", ct, err)
	}
	var m struct {
		TenantID  uuid.UUID `json:"tenant_id"`
		ChainHead *struct {
			Seq int64 `json:"seq"`
		} `json:"audit_chain_head"`
		Tables []struct {
			Name, Key, SHA256 string
			Part              int
			Rows              int64
		}
	}
	if err := json.Unmarshal(body, &m); err != nil || m.TenantID != tenant || m.ChainHead == nil || m.ChainHead.Seq != events || len(m.Tables) < 5 {
		t.Fatalf("manifest: %s %v", body, err)
	}
	found := false
	var principalRows int64
	for _, tb := range m.Tables {
		if tb.Name == "identity.principal" {
			found = true
			lines, ct, err := store.Get(ctx, tb.Key)
			if err != nil || ct != "application/x-ndjson" || int64(strings.Count(string(lines), "\n")) != tb.Rows || tb.Rows != 1 || !strings.HasSuffix(tb.Key, fmt.Sprintf(".%05d.jsonl", tb.Part)) {
				t.Fatalf("principal export part %d: %q %d %v", tb.Part, ct, tb.Rows, err)
			}
			principalRows += tb.Rows
			if err := store.Delete(ctx, tb.Key); !errors.Is(err, blobstore.ErrLocked) {
				t.Fatalf("the export is not locked: %v", err)
			}
		}
	}
	if principalRows != principals { // one row per part, with ChunkRows 1
		t.Fatalf("%d principal rows exported, want %d", principalRows, principals)
	}
	if !found {
		t.Fatalf("manifest without principals: %v", m.Tables)
	}
	if n := count(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'tenancy.tenant.export' AND actor_id = $2`, tenant, offboarding.Actor); n != 1 {
		t.Fatalf("%d export audit entries", n)
	}
	if n := count(t, `SELECT count(*) FROM tenancy.outbox WHERE tenant_id = $1 AND event_type LIKE '%tenant_exported%'`, tenant); n != 1 {
		t.Fatalf("%d export events", n)
	}
	// Within the grace period nothing more happens; exporting again is refused.
	if ex, er, err := w.RunOnce(ctx); err != nil || ex != 0 || er != 0 {
		t.Fatalf("grace: %d %d %v", ex, er, err)
	}
	if _, err := w.Export(ctx, tenant); problem.From(err).Code != "tenancy.invalid_transition" {
		t.Fatalf("second export: %v", err)
	}
	// After it the tenant is erased: rows gone but the audit trail, objects gone but the locked ones.
	clock = clock.Add(2 * time.Hour)
	ex, er, err = w.RunOnce(ctx)
	if err != nil || ex != 0 || er != 1 {
		t.Fatalf("erase round: %d %d %v", ex, er, err)
	}
	if n := count(t, `SELECT count(*) FROM identity.principal WHERE tenant_id = $1`, tenant); n != 0 {
		t.Fatalf("%d principals left", n)
	}
	if n := count(t, `SELECT count(*) FROM config.config_version WHERE tenant_id = $1`, tenant); n != 0 {
		t.Fatalf("%d insert-only configuration versions left", n)
	}
	if n := count(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'tenancy.tenant.erase'`, tenant); n != 1 {
		t.Fatalf("%d erase audit entries", n)
	}
	if n := count(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1`, tenant); n != events+2 {
		t.Fatalf("audit trail: %d events, want %d", n, events+2)
	}
	if n := count(t, `SELECT count(*) FROM tenancy.outbox WHERE tenant_id = $1`, tenant); n != 1 { // the erasure's own event, after the rest went
		t.Fatalf("%d events left", n)
	}
	if err := pgtest.Admin(t).QueryRow(ctx, `SELECT erased_at FROM tenancy.tenant WHERE id = $1`, tenant).Scan(&erasedAt); err != nil {
		t.Fatalf("read erased_at: %v", err)
	}
	if erasedAt == nil || !erasedAt.Equal(clock) {
		t.Fatalf("erased_at %v", erasedAt)
	}
	keys, _ := store.List(ctx, prefix)
	for _, k := range keys {
		if !strings.Contains(k, "/exports/") && !strings.Contains(k, "/audit/anchors/") {
			t.Fatalf("object left: %s", k)
		}
	}
	if len(keys) < 3 {
		t.Fatalf("exports and anchor gone: %v", keys)
	}
	if n := count(t, `SELECT count(*) FROM identity.principal WHERE tenant_id = $1`, other.Tenant.ID); n == 0 {
		t.Fatal("the other tenant lost its principals")
	}
	// Nothing left to do; erasing again is refused.
	if ex, er, err := w.RunOnce(ctx); err != nil || ex != 0 || er != 0 {
		t.Fatalf("after: %d %d %v", ex, er, err)
	}
	if _, err := w.Erase(ctx, tenant, offboarding.Actor, nil); problem.From(err).Code != "tenancy.invalid_transition" {
		t.Fatalf("second erasure: %v", err)
	}
	for _, id := range []uuid.UUID{uuid.New()} {
		if _, err := w.Export(ctx, id); problem.From(err).Status != 404 {
			t.Fatalf("export of nobody: %v", err)
		}
		if _, err := w.Erase(ctx, id, offboarding.Actor, nil); problem.From(err).Status != 404 {
			t.Fatalf("erasure of nobody: %v", err)
		}
	}
	// Run rounds until stopped: a tenant offboarded while it runs is exported.
	third := fixture.New(t, "payables.invoice.read")
	pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'offboarded', version = version + 1 WHERE id = $1`, third.Tenant.ID)
	w.Interval = time.Millisecond
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { w.Run(runCtx); close(done) }()
	pgtest.Eventually(t, 10*time.Second, func() bool {
		return count(t, `SELECT count(*) FROM tenancy.tenant WHERE id = $1 AND exported_at IS NOT NULL`, third.Tenant.ID) > 0
	}, "the running worker did not export the tenant")
	cancel()
	<-done
}

// beginFailing fails the k-th transaction a worker starts.
func beginFailing(pool *database.Pool, k int) func(context.Context, database.Scope) (pgx.Tx, error) {
	calls := 0
	return func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		calls++
		if calls == k {
			return nil, errors.New("pool down")
		}
		return pool.Begin(ctx, s)
	}
}

// TestOffboardingFailures: the store, the database and the tenant's state
// refusing, each reported and nothing half done.
func TestOffboardingFailures(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t)
	clock := time.Now().UTC()
	e := fixture.New(t, "payables.invoice.read")
	tenant := e.Tenant.ID
	pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'offboarded', version = version + 1 WHERE id = $1`, tenant)
	// Without a store, or with one that fails, nothing is exported and the tenant stays unexported.
	bare := &offboarding.Worker{Pool: pool, AppRole: "erp_app", WorkerRole: "erp_worker", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if _, err := bare.Export(ctx, tenant); problem.From(err).Status != 503 {
		t.Fatalf("without a store: %v", err)
	}
	if _, _, err := bare.RunOnce(ctx); problem.From(err).Status != 503 {
		t.Fatalf("round without a store: %v", err)
	}
	stopped, cancelNow := context.WithCancel(ctx)
	cancelNow()
	bare.Run(stopped) // the default interval, one round, then the stop
	if _, _, err := worker(t, pool, failingStore{err: errors.New("store down")}, &clock).RunOnce(ctx); err == nil || !strings.Contains(err.Error(), "store down") {
		t.Fatalf("failing store: %v", err)
	}
	if n := count(t, `SELECT count(*) FROM tenancy.tenant WHERE id = $1 AND exported_at IS NULL`, tenant); n != 1 {
		t.Fatal("exported despite the failure")
	}
	// A tenant exported by another round between the two transactions; the reset moves its offboarding time.
	store := blobstore.Dir{Root: t.TempDir(), Now: func() time.Time { return clock }}
	w := worker(t, pool, blobstore.Dir{Root: t.TempDir()}, &clock) // its objects go to a store of their own
	w.Begin = interfering(pool, 2, func() {
		pgtest.Exec(t, `UPDATE tenancy.tenant SET exported_at = now(), export_ref = 'elsewhere', erase_after = now() + interval '1 hour', version = version + 1 WHERE id = $1`, tenant)
	})
	if _, err := w.Export(ctx, tenant); problem.From(err).Code != "tenancy.invalid_transition" {
		t.Fatalf("exported by another round: %v", err)
	}
	pgtest.Exec(t, `UPDATE tenancy.tenant SET exported_at = NULL, export_ref = NULL, erase_after = NULL, version = version + 1 WHERE id = $1`, tenant)
	// Objects of an earlier attempt that did not get to record itself are kept, and described as stored.
	var offboardedAt time.Time
	if err := pgtest.Admin(t).QueryRow(ctx, `SELECT updated_at FROM tenancy.tenant WHERE id = $1`, tenant).Scan(&offboardedAt); err != nil {
		t.Fatalf("read the offboarding time: %v", err)
	}
	stale := "tenants/" + tenant.String() + "/exports/" + strconv.FormatInt(offboardedAt.Unix(), 10) + "/identity.principal.00001.jsonl"
	if err := store.Put(ctx, stale, []byte("{\"stale\":true}\n"), blobstore.PutOptions{ContentType: "application/x-ndjson"}); err != nil {
		t.Fatal(err)
	}
	// A store whose objects exist but cannot be read back fails the export.
	if _, err := worker(t, pool, existsUnreadable{}, &clock).Export(ctx, tenant); err == nil || !strings.Contains(err.Error(), "reading the stored") {
		t.Fatalf("unreadable existing object: %v", err)
	}
	// A store that takes the first part of a table but not the second.
	partial := worker(t, pool, secondPartFails{store}, &clock)
	partial.ChunkRows = 1
	if _, err := partial.Export(ctx, tenant); err == nil || !strings.Contains(err.Error(), "storing") || !strings.Contains(err.Error(), ".00002.jsonl") {
		t.Fatalf("second part failing: %v", err)
	}
	// A store that takes the tables but not the manifest.
	if _, err := worker(t, pool, manifestless{store}, &clock).Export(ctx, tenant); err == nil || !strings.Contains(err.Error(), "storing") || !strings.Contains(err.Error(), "manifest.json") {
		t.Fatalf("manifest failure: %v", err)
	}
	// A commit that fails.
	w = worker(t, pool, store, &clock)
	w.Begin = brokenCommitAt(pool, 2)
	if _, err := w.Export(ctx, tenant); err == nil || !strings.Contains(err.Error(), "commit failed") {
		t.Fatalf("export commit: %v", err)
	}
	// Each database statement of an export failing in turn is reported.
	// The transactions themselves failing to start, the first and the second.
	for k := 1; k <= 2; k++ {
		w.Begin = beginFailing(pool, k)
		if _, err := w.Export(ctx, tenant); err == nil || !strings.Contains(err.Error(), "pool down") {
			t.Fatalf("begin %d: %v", k, err)
		}
	}
	for n := 1; n < 200; n++ {
		count := 0
		w.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			if err != nil {
				return nil, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, nil
		}
		_, err := w.Export(ctx, tenant)
		if err == nil {
			if n == 1 {
				t.Fatal("exported without a statement to fail")
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: %v", n, err)
		}
	}
	w.Begin = nil
	ex, err := w.Export(ctx, tenant)
	if problem.From(err).Code != "tenancy.invalid_transition" { // the loop's last iteration exported it
		t.Fatalf("after the loop: %+v %v", ex, err)
	}
	content, _, _ := store.Get(ctx, stale)
	if string(content) != "{\"stale\":true}\n" {
		t.Fatal("the earlier object was replaced")
	}
	var manifest struct {
		Tables []struct{ Name, SHA256 string }
	}
	var ref string
	if err := pgtest.Admin(t).QueryRow(ctx, `SELECT export_ref FROM tenancy.tenant WHERE id = $1`, tenant).Scan(&ref); err != nil {
		t.Fatalf("read the export reference: %v", err)
	}
	body, _, _ := store.Get(ctx, ref)
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("decode the manifest: %v", err)
	}
	sum := sha256.Sum256(content)
	described := false
	for _, tb := range manifest.Tables {
		if tb.Name == "identity.principal" {
			described = tb.SHA256 == hex.EncodeToString(sum[:])
		}
	}
	if !described {
		t.Fatalf("the manifest does not describe the stored object: %+v", manifest.Tables)
	}
	// Erasure: before the grace period on request works; the store's listing and deletion failing stop it before the row is marked.
	pgtest.Exec(t, `UPDATE tenancy.tenant SET erase_after = now() - interval '1 hour', version = version + 1 WHERE id = $1`, tenant)
	broken := worker(t, pool, brokenEraser{Store: store, list: errors.New("list down")}, &clock)
	if _, err := broken.Erase(ctx, tenant, offboarding.Actor, nil); err == nil || !strings.Contains(err.Error(), "list down") {
		t.Fatalf("listing failure: %v", err)
	}
	broken = worker(t, pool, brokenEraser{Store: store, del: errors.New("delete down")}, &clock)
	if _, err := broken.Erase(ctx, tenant, offboarding.Actor, nil); err == nil || !strings.Contains(err.Error(), "delete down") {
		t.Fatalf("deletion failure: %v", err)
	}
	if n := count(t, `SELECT count(*) FROM tenancy.tenant WHERE id = $1 AND erased_at IS NULL`, tenant); n != 1 {
		t.Fatal("marked erased despite the failure")
	}
	// Each database statement of an erasure failing in turn is reported; a store without an eraser keeps its objects.
	plain := worker(t, pool, plainStore{store}, &clock)
	for k := 1; k <= 2; k++ {
		plain.Begin = beginFailing(pool, k)
		if _, err := plain.Erase(ctx, tenant, offboarding.Actor, nil); err == nil || !strings.Contains(err.Error(), "pool down") {
			t.Fatalf("begin %d: %v", k, err)
		}
	}
	// A round whose erasure fails reports it.
	if _, _, err := broken.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), "erasing tenant") {
		t.Fatalf("round with a failing erasure: %v", err)
	}
	// Commits that fail, and a tenant erased by another round between the transactions.
	for k := 1; k <= 2; k++ {
		plain.Begin = brokenCommitAt(pool, k)
		if _, err := plain.Erase(ctx, tenant, offboarding.Actor, nil); err == nil || !strings.Contains(err.Error(), "commit failed") {
			t.Fatalf("erase commit %d: %v", k, err)
		}
	}
	plain.Begin = interfering(pool, 2, func() {
		pgtest.Exec(t, `UPDATE tenancy.tenant SET erased_at = now(), version = version + 1 WHERE id = $1`, tenant)
	})
	if _, err := plain.Erase(ctx, tenant, offboarding.Actor, nil); problem.From(err).Code != "tenancy.invalid_transition" {
		t.Fatalf("erased by another round: %v", err)
	}
	pgtest.Exec(t, `UPDATE tenancy.tenant SET erased_at = NULL, version = version + 1 WHERE id = $1`, tenant)
	for n := 1; n < 200; n++ {
		count := 0
		plain.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			if err != nil {
				return nil, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, nil
		}
		_, err := plain.Erase(ctx, tenant, offboarding.Actor, map[string]any{"operator": "ops|alice"})
		if err == nil {
			if n == 1 {
				t.Fatal("erased without a statement to fail")
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: %v", n, err)
		}
	}
	if n := count(t, `SELECT count(*) FROM tenancy.tenant WHERE id = $1 AND erased_at IS NOT NULL`, tenant); n != 1 {
		t.Fatal("not erased after the loop")
	}
	if n := count(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'tenancy.tenant.erase' AND detail ->> 'operator' = 'ops|alice'`, tenant); n != 1 {
		t.Fatalf("%d erase entries naming the operator", n)
	}
	// The pending queries themselves failing, and the transaction not starting.
	for n := 1; n <= 2; n++ {
		w.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			if err != nil {
				return nil, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: n}, nil
		}
		if _, _, err := w.RunOnce(ctx); !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("pending statement %d: %v", n, err)
		}
	}
	w.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("pool down") }
	if _, _, err := w.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), "pool down") {
		t.Fatalf("pending: %v", err)
	}
	w.Begin = nil
	w.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	w.Interval = time.Millisecond
	runCtx, cancel := context.WithCancel(ctx)
	// A second transaction begun means the first round failed, was logged
	// and the worker waited its interval: the failing round has run whole.
	var begun atomic.Int32
	w.Begin = func(context.Context, database.Scope) (pgx.Tx, error) {
		begun.Add(1)
		return nil, errors.New("pool down")
	}
	done := make(chan struct{})
	go func() { w.Run(runCtx); close(done) }()
	pgtest.Eventually(t, 10*time.Second, func() bool { return begun.Load() >= 2 }, "the running worker did not begin a second round")
	cancel()
	<-done
}

// existsUnreadable says every key exists and cannot be read.
type existsUnreadable struct{}

func (existsUnreadable) Put(context.Context, string, []byte, blobstore.PutOptions) error {
	return blobstore.ErrExists
}
func (existsUnreadable) Get(context.Context, string) ([]byte, string, error) {
	return nil, "", errors.New("store down")
}
func (existsUnreadable) Stat(context.Context, string) (int64, string, error) { return 0, "", nil }

// manifestless stores everything but a manifest.
type manifestless struct{ blobstore.Store }

func (m manifestless) Put(ctx context.Context, key string, content []byte, opts blobstore.PutOptions) error {
	if strings.HasSuffix(key, "/manifest.json") {
		return errors.New("store full")
	}
	return m.Store.Put(ctx, key, content, opts)
}

// brokenCommit is a transaction whose commit fails.
type brokenCommit struct{ pgx.Tx }

func (b brokenCommit) Commit(ctx context.Context) error {
	b.Rollback(ctx)
	return errors.New("commit failed")
}

// brokenCommitAt breaks the commit of the k-th transaction a worker starts.
func brokenCommitAt(pool *database.Pool, k int) func(context.Context, database.Scope) (pgx.Tx, error) {
	calls := 0
	return func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		calls++
		tx, err := pool.Begin(ctx, s)
		if err != nil || calls != k {
			return tx, err
		}
		return brokenCommit{tx}, nil
	}
}

// interfering runs fn (another round's work) before the k-th transaction.
func interfering(pool *database.Pool, k int, fn func()) func(context.Context, database.Scope) (pgx.Tx, error) {
	calls := 0
	return func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		calls++
		if calls == k {
			fn()
		}
		return pool.Begin(ctx, s)
	}
}

// secondPartFails stores everything but a table's second part.
type secondPartFails struct{ blobstore.Store }

func (f secondPartFails) Put(ctx context.Context, key string, content []byte, opts blobstore.PutOptions) error {
	if strings.HasSuffix(key, ".00002.jsonl") {
		return errors.New("store full")
	}
	return f.Store.Put(ctx, key, content, opts)
}
