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
	pgtest.Scan(t, sql, args, &n) // a connection per call, closed after: count runs in polling waits
	return n
}

// round runs one round of w and fails unless it exported and erased as many
// tenants as wanted.
func round(t *testing.T, w *offboarding.Worker, what string, wantExported, wantErased int) {
	t.Helper()
	ex, er, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("%s round: %v", what, err)
	}
	if ex != wantExported || er != wantErased {
		t.Fatalf("%s round: got %d exported, %d erased, want %d, %d", what, ex, er, wantExported, wantErased)
	}
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
	sc := &lifecycleScenario{ctx: ctx, pool: pool, e: e, other: other, clock: &clock, store: store, w: w, tenant: tenant, prefix: prefix}
	if !t.Run("active tenant is left alone", sc.activeTenantIsLeftAlone) {
		t.FailNow()
	}
	if !t.Run("offboarded tenant is exported", sc.offboardedTenantIsExported) {
		t.FailNow()
	}
	if !t.Run("manifest describes locked parts", sc.manifestDescribesLockedParts) {
		t.FailNow()
	}
	if !t.Run("export is audited and published", sc.exportIsAuditedAndPublished) {
		t.FailNow()
	}
	if !t.Run("grace period", sc.gracePeriod) {
		t.FailNow()
	}
	if !t.Run("erasure after the grace period", sc.erasureAfterTheGracePeriod) {
		t.FailNow()
	}
	if !t.Run("nothing left to do", sc.nothingLeftToDo) {
		t.FailNow()
	}
	t.Run("running worker exports a tenant offboarded meanwhile", sc.runningWorkerExportsATenantOffboardedMeanwhile)
}

// lifecycleScenario is the state TestOffboardingLifecycle's steps share.
type lifecycleScenario struct {
	ctx        context.Context
	pool       *database.Pool
	e          fixture.Env // the tenant offboarded
	other      fixture.Env // a tenant that stays
	clock      *time.Time  // the store's and the worker's
	store      blobstore.Dir
	w          *offboarding.Worker
	tenant     uuid.UUID
	prefix     string // the tenant's objects
	principals int64  // the tenant's, before the export
	events     int64  // the tenant's audit events, before the export
	ref        string // the export's manifest
}

// activeTenantIsLeftAlone is the step "active tenant is left alone" of TestOffboardingLifecycle.
func (sc *lifecycleScenario) activeTenantIsLeftAlone(t *testing.T) {
	// A document of the tenant, and an anchor locked for years.
	if err := sc.store.Put(sc.ctx, sc.prefix+"documents/abc", []byte("%PDF"), blobstore.PutOptions{ContentType: "application/pdf"}); err != nil {
		t.Fatalf("store a document: %v", err)
	}
	if err := sc.store.Put(sc.ctx, sc.prefix+"audit/anchors/000000000001.json", []byte("{}"), blobstore.PutOptions{ContentType: "application/json", RetainUntil: sc.clock.Add(24 * time.Hour)}); err != nil {
		t.Fatalf("store an anchor: %v", err)
	}
	// Its analytical store: a file, and the pointer and superseded rows (ADR-0044).
	analytics := "analytics/v1/tenant=" + sc.tenant.String() + "/"
	if err := sc.store.Put(sc.ctx, analytics+"manifest-00000000000000000007.json", []byte("{}"), blobstore.PutOptions{ContentType: "application/json"}); err != nil {
		t.Fatalf("store a manifest: %v", err)
	}
	pgtest.Exec(t, `INSERT INTO analytics.snapshot (tenant_id, manifest_key, watermark, as_of) VALUES ($1, $2, '7', now())`,
		sc.tenant, analytics+"manifest-00000000000000000007.json")
	pgtest.Exec(t, `INSERT INTO analytics.superseded (tenant_id, manifest_key) VALUES ($1, $2)`, sc.tenant, analytics+"manifest-00000000000000000006.json")
	// Nothing to do while the tenant is active; an active tenant is not exported or erased on request either.
	round(t, sc.w, "active", 0, 0)
	if _, err := sc.w.Export(sc.ctx, sc.tenant); problem.From(err).Code != "tenancy.invalid_transition" {
		t.Fatalf("export of an active tenant: %v", err)
	}
	if _, err := sc.w.Erase(sc.ctx, sc.tenant, offboarding.Actor, nil); problem.From(err).Code != "tenancy.invalid_transition" {
		t.Fatalf("erasure of an active tenant: %v", err)
	}
}

// offboardedTenantIsExported is the step "offboarded tenant is exported" of TestOffboardingLifecycle.
func (sc *lifecycleScenario) offboardedTenantIsExported(t *testing.T) {
	pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'offboarded', version = version + 1 WHERE id = $1`, sc.tenant)
	sc.principals = count(t, `SELECT count(*) FROM identity.principal WHERE tenant_id = $1`, sc.tenant)
	sc.events = count(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1`, sc.tenant)
	if sc.principals == 0 || sc.events == 0 {
		t.Fatalf("fixture: %d principals, %d audit events", sc.principals, sc.events)
	}
	// The round exports: locked objects per table and a manifest describing them; the tenant records it.
	round(t, sc.w, "export", 1, 0)
	var exportedAt, eraseAfter time.Time
	var erasedAt *time.Time
	if err := pgtest.Admin(t).QueryRow(sc.ctx, `SELECT exported_at, export_ref, erase_after, erased_at FROM tenancy.tenant WHERE id = $1`, sc.tenant).Scan(&exportedAt, &sc.ref, &eraseAfter, &erasedAt); err != nil {
		t.Fatalf("read the tenant after export: %v", err)
	}
	if !exportedAt.Equal(*sc.clock) {
		t.Errorf("exported_at: got %v, want %v", exportedAt, *sc.clock)
	}
	if !eraseAfter.Equal(sc.clock.Add(time.Hour)) {
		t.Errorf("erase_after: got %v, want %v", eraseAfter, sc.clock.Add(time.Hour))
	}
	if erasedAt != nil {
		t.Errorf("erased_at: got %v, want nil", erasedAt)
	}
	if !strings.HasPrefix(sc.ref, sc.prefix+"exports/") || !strings.HasSuffix(sc.ref, "/manifest.json") {
		t.Fatalf("export_ref: got %s, want %sexports/.../manifest.json", sc.ref, sc.prefix)
	}
}

// manifestDescribesLockedParts is the step "manifest describes locked parts" of TestOffboardingLifecycle.
func (sc *lifecycleScenario) manifestDescribesLockedParts(t *testing.T) {
	body, ct, err := sc.store.Get(sc.ctx, sc.ref)
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
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("manifest: %s %v", body, err)
	}
	if m.TenantID != sc.tenant {
		t.Errorf("manifest tenant: got %v, want %v", m.TenantID, sc.tenant)
	}
	if m.ChainHead == nil {
		t.Errorf("manifest chain head: got nil, want seq %d", sc.events)
	} else if m.ChainHead.Seq != sc.events {
		t.Errorf("manifest chain head seq: got %d, want %d", m.ChainHead.Seq, sc.events)
	}
	if len(m.Tables) < 5 {
		t.Fatalf("manifest tables: got %d, want at least 5: %s", len(m.Tables), body)
	}
	found := false
	var principalRows int64
	for _, tb := range m.Tables {
		if tb.Name == "identity.principal" {
			found = true
			lines, ct, err := sc.store.Get(sc.ctx, tb.Key)
			if err != nil {
				t.Fatalf("principal export part %d: %v", tb.Part, err)
			}
			if ct != "application/x-ndjson" {
				t.Errorf("principal export part %d content type: got %q, want application/x-ndjson", tb.Part, ct)
			}
			if n := int64(strings.Count(string(lines), "\n")); n != tb.Rows {
				t.Errorf("principal export part %d: got %d lines, want the manifest's %d", tb.Part, n, tb.Rows)
			}
			if tb.Rows != 1 {
				t.Errorf("principal export part %d: got %d rows, want 1", tb.Part, tb.Rows)
			}
			if want := fmt.Sprintf(".%05d.jsonl", tb.Part); !strings.HasSuffix(tb.Key, want) {
				t.Errorf("principal export part %d key: got %s, want suffix %s", tb.Part, tb.Key, want)
			}
			principalRows += tb.Rows
			if err := sc.store.Delete(sc.ctx, tb.Key); !errors.Is(err, blobstore.ErrLocked) {
				t.Fatalf("the export is not locked: %v", err)
			}
		}
	}
	if principalRows != sc.principals { // one row per part, with ChunkRows 1
		t.Fatalf("%d principal rows exported, want %d", principalRows, sc.principals)
	}
	if !found {
		t.Fatalf("manifest without principals: %v", m.Tables)
	}
}

// exportIsAuditedAndPublished is the step "export is audited and published" of TestOffboardingLifecycle.
func (sc *lifecycleScenario) exportIsAuditedAndPublished(t *testing.T) {
	if n := count(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'tenancy.tenant.export' AND actor_id = $2`, sc.tenant, offboarding.Actor); n != 1 {
		t.Fatalf("%d export audit entries", n)
	}
	if n := count(t, `SELECT count(*) FROM tenancy.outbox WHERE tenant_id = $1 AND event_type LIKE '%tenant_exported%'`, sc.tenant); n != 1 {
		t.Fatalf("%d export events", n)
	}
}

// gracePeriod is the step "grace period" of TestOffboardingLifecycle.
func (sc *lifecycleScenario) gracePeriod(t *testing.T) {
	// Within the grace period nothing more happens; exporting again is refused.
	round(t, sc.w, "grace", 0, 0)
	if _, err := sc.w.Export(sc.ctx, sc.tenant); problem.From(err).Code != "tenancy.invalid_transition" {
		t.Fatalf("second export: %v", err)
	}
}

// erasureAfterTheGracePeriod is the step "erasure after the grace period" of TestOffboardingLifecycle.
func (sc *lifecycleScenario) erasureAfterTheGracePeriod(t *testing.T) {
	var erasedAt *time.Time
	// After it the tenant is erased: rows gone but the audit trail, objects gone but the locked ones.
	*sc.clock = sc.clock.Add(2 * time.Hour)
	round(t, sc.w, "erase", 0, 1)
	if n := count(t, `SELECT count(*) FROM identity.principal WHERE tenant_id = $1`, sc.tenant); n != 0 {
		t.Fatalf("%d principals left", n)
	}
	if n := count(t, `SELECT count(*) FROM config.config_version WHERE tenant_id = $1`, sc.tenant); n != 0 {
		t.Fatalf("%d insert-only configuration versions left", n)
	}
	if n := count(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'tenancy.tenant.erase'`, sc.tenant); n != 1 {
		t.Fatalf("%d erase audit entries", n)
	}
	if n := count(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1`, sc.tenant); n != sc.events+2 {
		t.Fatalf("audit trail: %d events, want %d", n, sc.events+2)
	}
	if n := count(t, `SELECT count(*) FROM tenancy.outbox WHERE tenant_id = $1`, sc.tenant); n != 1 { // the erasure's own event, after the rest went
		t.Fatalf("%d events left", n)
	}
	if err := pgtest.Admin(t).QueryRow(sc.ctx, `SELECT erased_at FROM tenancy.tenant WHERE id = $1`, sc.tenant).Scan(&erasedAt); err != nil {
		t.Fatalf("read erased_at: %v", err)
	}
	if erasedAt == nil || !erasedAt.Equal(*sc.clock) {
		t.Fatalf("erased_at %v", erasedAt)
	}
	keys, _ := sc.store.List(sc.ctx, sc.prefix)
	for _, k := range keys {
		if !strings.Contains(k, "/exports/") && !strings.Contains(k, "/audit/anchors/") {
			t.Fatalf("got object %s left, want only exports and anchors", k)
		}
	}
	if len(keys) < 3 {
		t.Fatalf("exports and anchor gone: %v", keys)
	}
	if n := count(t, `SELECT count(*) FROM identity.principal WHERE tenant_id = $1`, sc.other.Tenant.ID); n == 0 {
		t.Fatal("the other tenant lost its principals")
	}
	if left, _ := sc.store.List(sc.ctx, "analytics/v1/tenant="+sc.tenant.String()+"/"); len(left) != 0 {
		t.Errorf("analytical store: got %v left, want nothing", left)
	}
	if n := count(t, `SELECT (SELECT count(*) FROM analytics.snapshot WHERE tenant_id = $1) + (SELECT count(*) FROM analytics.superseded WHERE tenant_id = $1)`, sc.tenant); n != 0 {
		t.Errorf("analytical store pointer rows: got %d, want 0", n)
	}
}

// nothingLeftToDo is the step "nothing left to do" of TestOffboardingLifecycle.
func (sc *lifecycleScenario) nothingLeftToDo(t *testing.T) {
	// Nothing left to do; erasing again is refused.
	round(t, sc.w, "after", 0, 0)
	if _, err := sc.w.Erase(sc.ctx, sc.tenant, offboarding.Actor, nil); problem.From(err).Code != "tenancy.invalid_transition" {
		t.Fatalf("second erasure: %v", err)
	}
	for _, id := range []uuid.UUID{uuid.New()} {
		if _, err := sc.w.Export(sc.ctx, id); problem.From(err).Status != 404 {
			t.Fatalf("export of nobody: %v", err)
		}
		if _, err := sc.w.Erase(sc.ctx, id, offboarding.Actor, nil); problem.From(err).Status != 404 {
			t.Fatalf("erasure of nobody: %v", err)
		}
	}
}

// runningWorkerExportsATenantOffboardedMeanwhile is the step "running worker exports a tenant offboarded meanwhile" of TestOffboardingLifecycle.
func (sc *lifecycleScenario) runningWorkerExportsATenantOffboardedMeanwhile(t *testing.T) {
	// Run rounds until stopped: a tenant offboarded while it runs is exported.
	third := fixture.New(t, "payables.invoice.read")
	pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'offboarded', version = version + 1 WHERE id = $1`, third.Tenant.ID)
	sc.w.Interval = time.Millisecond
	runCtx, cancel := context.WithCancel(sc.ctx)
	done := make(chan struct{})
	go func() { sc.w.Run(runCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done }) // stops the worker should the wait fail
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
	sc := &failuresScenario{ctx: ctx, pool: pool, clock: &clock, e: e, tenant: tenant}
	if !t.Run("without a working store nothing is exported", sc.withoutAWorkingStoreNothingIsExported) {
		t.FailNow()
	}
	if !t.Run("export taken over by another round", sc.exportTakenOverByAnotherRound) {
		t.FailNow()
	}
	if !t.Run("failing stores keep earlier objects", sc.failingStoresKeepEarlierObjects) {
		t.FailNow()
	}
	if !t.Run("export commit and begin failing", sc.exportCommitAndBeginFailing) {
		t.FailNow()
	}
	if !t.Run("each export statement failing", sc.eachExportStatementFailing) {
		t.FailNow()
	}
	if !t.Run("manifest describes the earlier object", sc.manifestDescribesTheEarlierObject) {
		t.FailNow()
	}
	if !t.Run("erasure store failures", sc.erasureStoreFailures) {
		t.FailNow()
	}
	if !t.Run("erase begin failing", sc.eraseBeginFailing) {
		t.FailNow()
	}
	if !t.Run("erase commits and takeover", sc.eraseCommitsAndTakeover) {
		t.FailNow()
	}
	if !t.Run("each erase statement failing", sc.eachEraseStatementFailing) {
		t.FailNow()
	}
	if !t.Run("pending queries failing", sc.pendingQueriesFailing) {
		t.FailNow()
	}
	t.Run("running worker logs and retries", sc.runningWorkerLogsAndRetries)
}

// failuresScenario is the state TestOffboardingFailures's steps share.
type failuresScenario struct {
	ctx    context.Context
	pool   *database.Pool
	clock  *time.Time  // the workers'
	e      fixture.Env // the tenant offboarded
	tenant uuid.UUID
	store  blobstore.Dir       // the export's objects
	w      *offboarding.Worker // exports
	broken *offboarding.Worker // erases with a store that fails
	plain  *offboarding.Worker // erases with a store without an eraser
	stale  string              // an object of an earlier attempt
}

// withoutAWorkingStoreNothingIsExported is the step "without a working store nothing is exported" of TestOffboardingFailures.
func (sc *failuresScenario) withoutAWorkingStoreNothingIsExported(t *testing.T) {
	var err error
	// Without a store, or with one that fails, nothing is exported and the tenant stays unexported.
	bare := &offboarding.Worker{Pool: sc.pool, AppRole: "erp_app", WorkerRole: "erp_worker", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if _, err := bare.Export(sc.ctx, sc.tenant); problem.From(err).Status != 503 {
		t.Fatalf("without a store: %v", err)
	}
	if _, _, err := bare.RunOnce(sc.ctx); problem.From(err).Status != 503 {
		t.Fatalf("round without a store: %v", err)
	}
	stopped, cancelNow := context.WithCancel(sc.ctx)
	cancelNow()
	bare.Run(stopped) // the default interval, one round, then the stop
	_, _, err = worker(t, sc.pool, failingStore{err: errors.New("store down")}, sc.clock).RunOnce(sc.ctx)
	pgtest.WantErr(t, err, "store down")
	if n := count(t, `SELECT count(*) FROM tenancy.tenant WHERE id = $1 AND exported_at IS NULL`, sc.tenant); n != 1 {
		t.Fatal("exported despite the failure")
	}
}

// exportTakenOverByAnotherRound is the step "export taken over by another round" of TestOffboardingFailures.
func (sc *failuresScenario) exportTakenOverByAnotherRound(t *testing.T) {
	// A tenant exported by another round between the two transactions; the reset moves its offboarding time.
	sc.store = blobstore.Dir{Root: t.TempDir(), Now: func() time.Time { return *sc.clock }}
	sc.w = worker(t, sc.pool, blobstore.Dir{Root: t.TempDir()}, sc.clock) // its objects go to a store of their own
	sc.w.Begin = interfering(sc.pool, 2, func() {
		pgtest.Exec(t, `UPDATE tenancy.tenant SET exported_at = now(), export_ref = 'elsewhere', erase_after = now() + interval '1 hour', version = version + 1 WHERE id = $1`, sc.tenant)
	})
	if _, err := sc.w.Export(sc.ctx, sc.tenant); problem.From(err).Code != "tenancy.invalid_transition" {
		t.Fatalf("exported by another round: %v", err)
	}
	pgtest.Exec(t, `UPDATE tenancy.tenant SET exported_at = NULL, export_ref = NULL, erase_after = NULL, version = version + 1 WHERE id = $1`, sc.tenant)
}

// failingStoresKeepEarlierObjects is the step "failing stores keep earlier objects" of TestOffboardingFailures.
func (sc *failuresScenario) failingStoresKeepEarlierObjects(t *testing.T) {
	var err error
	// Objects of an earlier attempt that did not get to record itself are kept, and described as stored.
	var offboardedAt time.Time
	if err := pgtest.Admin(t).QueryRow(sc.ctx, `SELECT updated_at FROM tenancy.tenant WHERE id = $1`, sc.tenant).Scan(&offboardedAt); err != nil {
		t.Fatalf("read the offboarding time: %v", err)
	}
	sc.stale = "tenants/" + sc.tenant.String() + "/exports/" + strconv.FormatInt(offboardedAt.Unix(), 10) + "/identity.principal.00001.jsonl"
	if err := sc.store.Put(sc.ctx, sc.stale, []byte("{\"stale\":true}\n"), blobstore.PutOptions{ContentType: "application/x-ndjson"}); err != nil {
		t.Fatal(err)
	}
	// A store whose objects exist but cannot be read back fails the export.
	_, err = worker(t, sc.pool, existsUnreadable{}, sc.clock).Export(sc.ctx, sc.tenant)
	pgtest.WantErr(t, err, "reading the stored")
	// A store that takes the first part of a table but not the second.
	partial := worker(t, sc.pool, secondPartFails{sc.store}, sc.clock)
	partial.ChunkRows = 1
	if _, err := partial.Export(sc.ctx, sc.tenant); err == nil {
		t.Fatal("second part failing: got nil error, want a storing error")
	} else {
		for _, want := range []string{"storing", ".00002.jsonl"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("second part failing: got %v, want it to mention %q", err, want)
			}
		}
	}
	// A store that takes the tables but not the manifest.
	if _, err := worker(t, sc.pool, manifestless{sc.store}, sc.clock).Export(sc.ctx, sc.tenant); err == nil {
		t.Fatal("manifest failure: got nil error, want a storing error")
	} else {
		for _, want := range []string{"storing", "manifest.json"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("manifest failure: got %v, want it to mention %q", err, want)
			}
		}
	}
}

// exportCommitAndBeginFailing is the step "export commit and begin failing" of TestOffboardingFailures.
func (sc *failuresScenario) exportCommitAndBeginFailing(t *testing.T) {
	// A commit that fails.
	sc.w = worker(t, sc.pool, sc.store, sc.clock)
	sc.w.Begin = brokenCommitAt(sc.pool, 2)
	if _, err := sc.w.Export(sc.ctx, sc.tenant); !errors.Is(err, pgtest.ErrInjected) {
		t.Fatalf("export commit: got %v, want %v", err, pgtest.ErrInjected)
	}
	// Each database statement of an export failing in turn is reported.
	// The transactions themselves failing to start, the first and the second.
	for k := 1; k <= 2; k++ {
		if !t.Run(fmt.Sprintf("export begin %d failing", k), func(t *testing.T) {
			sc.w.Begin = beginFailing(sc.pool, k)
			_, err := sc.w.Export(sc.ctx, sc.tenant)
			pgtest.WantErr(t, err, "pool down")
		}) {
			t.FailNow()
		}
	}
}

// eachExportStatementFailing is the step "each export statement failing" of TestOffboardingFailures.
func (sc *failuresScenario) eachExportStatementFailing(t *testing.T) {
	for n := 1; n < 200; n++ {
		count := 0
		sc.w.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := sc.pool.Begin(ctx, s)
			if err != nil {
				return nil, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, nil
		}
		_, err := sc.w.Export(sc.ctx, sc.tenant)
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
	sc.w.Begin = nil
	ex, err := sc.w.Export(sc.ctx, sc.tenant)
	if problem.From(err).Code != "tenancy.invalid_transition" { // the loop's last iteration exported it
		t.Fatalf("after the loop: %+v %v", ex, err)
	}
}

// manifestDescribesTheEarlierObject is the step "manifest describes the earlier object" of TestOffboardingFailures.
func (sc *failuresScenario) manifestDescribesTheEarlierObject(t *testing.T) {
	content, _, _ := sc.store.Get(sc.ctx, sc.stale)
	if string(content) != "{\"stale\":true}\n" {
		t.Fatal("the earlier object was replaced")
	}
	var manifest struct {
		Tables []struct{ Name, SHA256 string }
	}
	var ref string
	if err := pgtest.Admin(t).QueryRow(sc.ctx, `SELECT export_ref FROM tenancy.tenant WHERE id = $1`, sc.tenant).Scan(&ref); err != nil {
		t.Fatalf("read the export reference: %v", err)
	}
	body, _, _ := sc.store.Get(sc.ctx, ref)
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
}

// erasureStoreFailures is the step "erasure store failures" of TestOffboardingFailures.
func (sc *failuresScenario) erasureStoreFailures(t *testing.T) {
	var err error
	// Erasure: before the grace period on request works; the store's listing and deletion failing stop it before the row is marked.
	pgtest.Exec(t, `UPDATE tenancy.tenant SET erase_after = now() - interval '1 hour', version = version + 1 WHERE id = $1`, sc.tenant)
	sc.broken = worker(t, sc.pool, brokenEraser{Store: sc.store, list: errors.New("list down")}, sc.clock)
	_, err = sc.broken.Erase(sc.ctx, sc.tenant, offboarding.Actor, nil)
	pgtest.WantErr(t, err, "list down")
	sc.broken = worker(t, sc.pool, brokenEraser{Store: sc.store, del: errors.New("delete down")}, sc.clock)
	_, err = sc.broken.Erase(sc.ctx, sc.tenant, offboarding.Actor, nil)
	pgtest.WantErr(t, err, "delete down")
	if n := count(t, `SELECT count(*) FROM tenancy.tenant WHERE id = $1 AND erased_at IS NULL`, sc.tenant); n != 1 {
		t.Fatal("marked erased despite the failure")
	}
}

// eraseBeginFailing is the step "erase begin failing" of TestOffboardingFailures.
func (sc *failuresScenario) eraseBeginFailing(t *testing.T) {
	var err error
	// Each database statement of an erasure failing in turn is reported; a store without an eraser keeps its objects.
	sc.plain = worker(t, sc.pool, plainStore{sc.store}, sc.clock)
	for k := 1; k <= 2; k++ {
		if !t.Run(fmt.Sprintf("erase begin %d failing", k), func(t *testing.T) {
			sc.plain.Begin = beginFailing(sc.pool, k)
			_, err := sc.plain.Erase(sc.ctx, sc.tenant, offboarding.Actor, nil)
			pgtest.WantErr(t, err, "pool down")
		}) {
			t.FailNow()
		}
	}
	// A round whose erasure fails reports it.
	_, _, err = sc.broken.RunOnce(sc.ctx)
	pgtest.WantErr(t, err, "erasing tenant")
}

// eraseCommitsAndTakeover is the step "erase commits and takeover" of TestOffboardingFailures.
func (sc *failuresScenario) eraseCommitsAndTakeover(t *testing.T) {
	// Commits that fail, and a tenant erased by another round between the transactions.
	for k := 1; k <= 2; k++ {
		if !t.Run(fmt.Sprintf("erase commit %d failing", k), func(t *testing.T) {
			sc.plain.Begin = brokenCommitAt(sc.pool, k)
			if _, err := sc.plain.Erase(sc.ctx, sc.tenant, offboarding.Actor, nil); !errors.Is(err, pgtest.ErrInjected) {
				t.Fatalf("got %v, want %v", err, pgtest.ErrInjected)
			}
		}) {
			t.FailNow()
		}
	}
	sc.plain.Begin = interfering(sc.pool, 2, func() {
		pgtest.Exec(t, `UPDATE tenancy.tenant SET erased_at = now(), version = version + 1 WHERE id = $1`, sc.tenant)
	})
	if _, err := sc.plain.Erase(sc.ctx, sc.tenant, offboarding.Actor, nil); problem.From(err).Code != "tenancy.invalid_transition" {
		t.Fatalf("erased by another round: %v", err)
	}
	pgtest.Exec(t, `UPDATE tenancy.tenant SET erased_at = NULL, version = version + 1 WHERE id = $1`, sc.tenant)
}

// eachEraseStatementFailing is the step "each erase statement failing" of TestOffboardingFailures.
func (sc *failuresScenario) eachEraseStatementFailing(t *testing.T) {
	for n := 1; n < 200; n++ {
		count := 0
		sc.plain.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := sc.pool.Begin(ctx, s)
			if err != nil {
				return nil, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, nil
		}
		_, err := sc.plain.Erase(sc.ctx, sc.tenant, offboarding.Actor, map[string]any{"operator": "ops|alice"})
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
	if n := count(t, `SELECT count(*) FROM tenancy.tenant WHERE id = $1 AND erased_at IS NOT NULL`, sc.tenant); n != 1 {
		t.Fatal("not erased after the loop")
	}
	if n := count(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'tenancy.tenant.erase' AND detail ->> 'operator' = 'ops|alice'`, sc.tenant); n != 1 {
		t.Fatalf("%d erase entries naming the operator", n)
	}
}

// pendingQueriesFailing is the step "pending queries failing" of TestOffboardingFailures.
func (sc *failuresScenario) pendingQueriesFailing(t *testing.T) {
	var err error
	// The pending queries themselves failing, and the transaction not starting.
	for n := 1; n <= 2; n++ {
		if !t.Run(fmt.Sprintf("pending statement %d failing", n), func(t *testing.T) {
			sc.w.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
				tx, err := sc.pool.Begin(ctx, s)
				if err != nil {
					return nil, err
				}
				return &pgtest.FailingTx{Tx: tx, FailAt: n}, nil
			}
			if _, _, err := sc.w.RunOnce(sc.ctx); !errors.Is(err, pgtest.ErrInjected) {
				t.Fatalf("got %v, want %v", err, pgtest.ErrInjected)
			}
		}) {
			t.FailNow()
		}
	}
	sc.w.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("pool down") }
	_, _, err = sc.w.RunOnce(sc.ctx)
	pgtest.WantErr(t, err, "pool down")
}

// runningWorkerLogsAndRetries is the step "running worker logs and retries" of TestOffboardingFailures.
func (sc *failuresScenario) runningWorkerLogsAndRetries(t *testing.T) {
	sc.w.Begin = nil
	sc.w.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	sc.w.Interval = time.Millisecond
	runCtx, cancel := context.WithCancel(sc.ctx)
	// A second transaction begun means the first round failed, was logged
	// and the worker waited its interval: the failing round has run whole.
	var begun atomic.Int32
	sc.w.Begin = func(context.Context, database.Scope) (pgx.Tx, error) {
		begun.Add(1)
		return nil, errors.New("pool down")
	}
	done := make(chan struct{})
	go func() { sc.w.Run(runCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done }) // stops the worker should the wait fail
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

// brokenCommitAt breaks the commit of the k-th transaction a worker starts.
// It counts transactions, which pgtest.FailingTx does not, and lets
// FailingTx fail the commit.
func brokenCommitAt(pool *database.Pool, k int) func(context.Context, database.Scope) (pgx.Tx, error) {
	calls := 0
	return func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		calls++
		tx, err := pool.Begin(ctx, s)
		if err != nil || calls != k {
			return tx, err
		}
		return &pgtest.FailingTx{Tx: tx, FailCommit: true}, nil
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
