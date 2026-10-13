package audit_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/audit"
	trail "github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// failingStore refuses every write and read.
type failingStore struct{ err error }

func (f failingStore) Put(context.Context, string, []byte, blobstore.PutOptions) error { return f.err }
func (f failingStore) Get(context.Context, string) ([]byte, string, error)             { return nil, "", f.err }
func (f failingStore) Stat(context.Context, string) (int64, string, error)             { return 0, "", f.err }

func anchorer(t *testing.T, store blobstore.Store) *audit.Anchorer {
	return &audit.Anchorer{Pool: pgtest.Pool(t), Store: store, AppRole: "erp_app", WorkerRole: "erp_worker", Retention: 10 * 365 * 24 * time.Hour,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC) }}
}

// existsThenFails says every key exists and cannot be read.
type existsThenFails struct{}

func (existsThenFails) Put(context.Context, string, []byte, blobstore.PutOptions) error {
	return blobstore.ErrExists
}
func (existsThenFails) Get(context.Context, string) ([]byte, string, error) {
	return nil, "", errors.New("store down")
}
func (existsThenFails) Stat(context.Context, string) (int64, string, error) {
	return 0, "", errors.New("store down")
}

// verify runs the verification operation with the store and returns its result.
func verify(t *testing.T, c apitest.Client, store blobstore.Store, from, to *int64) map[string]any {
	t.Helper()
	h := endpoints()
	h.Anchors = store
	var op gen.VerifyAuditChainRes
	req := &gen.AuditVerify{}
	if from != nil {
		req.FromSeq = gen.NewOptInt64(*from)
	}
	if to != nil {
		req.ToSeq = gen.NewOptInt64(*to)
	}
	if err := apitest.Committed(t, c.Tenant, c.Tenant.Human, func(ctx context.Context) (err error) {
		op, err = h.VerifyAuditChain(ctx, req, gen.VerifyAuditChainParams{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	done, _ := c.Must(200, "GET", "/v1/operations/"+op.(*gen.OperationHeaders).Response.ID.String(), "", "")
	return done["result"].(map[string]any)
}

func anchors(t *testing.T, tenant uuid.UUID) []int64 {
	t.Helper()
	rows, _ := pgtest.Admin(t).Query(context.Background(), `SELECT seq FROM audit.anchor WHERE tenant_id = $1 ORDER BY seq`, tenant)
	seqs, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatal(err)
	}
	return seqs
}

func TestAnchoring(t *testing.T) {
	c := setup(t) // three events
	root := t.TempDir()
	store := blobstore.Dir{Root: root}
	w := anchorer(t, store)
	ctx := context.Background()
	n, err := w.RunOnce(ctx)
	if err != nil || n < 1 {
		t.Fatalf("first round: %d %v", n, err)
	}
	if got := anchors(t, c.Tenant.ID); len(got) != 1 || got[0] != 3 {
		t.Fatalf("anchors: %v", got)
	}
	// The object says what the row says, and is locked for the retention.
	key := blobstore.Key(c.Tenant.ID, blobstore.KindAnchors, audit.ObjectName(3))
	content, ct, err := store.Get(ctx, key)
	var obj audit.Anchor
	if err != nil || ct != "application/json" || json.Unmarshal(content, &obj) != nil || obj.Seq != 3 || obj.TenantID != c.Tenant.ID || len(obj.Hash) != 64 {
		t.Fatalf("object: %s %s %v", content, ct, err)
	}
	want := obj.AnchoredAt.Add(w.Retention).UTC().Format(time.RFC3339)
	if meta, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(key)) + ".meta"); !strings.Contains(string(meta), want) {
		t.Fatalf("retention: %s, want %s", meta, want)
	}
	// Nothing moved: nothing to anchor. One more event: one more anchor, at the new head.
	if n, err := w.RunOnce(ctx); err != nil || n != 0 {
		t.Fatalf("idle round: %d %v", n, err)
	}
	verify(t, c, store, nil, nil) // writes an audit event of its own (seq 4)
	if n, err := w.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("after a change: %d %v", n, err)
	}
	if got := anchors(t, c.Tenant.ID); len(got) != 2 || got[1] != 4 {
		t.Fatalf("anchors: %v", got)
	}
	// Verification starts from the last anchor by default and checks the anchors in range.
	res := verify(t, c, store, nil, nil)
	if res["verified"] != true || res["anchors_checked"] != float64(1) || res["checked"] != float64(1) {
		t.Fatalf("default range: %v", res)
	}
	one := int64(1)
	res = verify(t, c, store, &one, nil)
	if res["verified"] != true || res["anchors_checked"] != float64(2) {
		t.Fatalf("whole chain: %v", res)
	}
	// Without a store, anchors are checked against the chain only.
	if res := verify(t, c, nil, &one, nil); res["verified"] != true || res["anchors_checked"] != float64(2) {
		t.Fatalf("no store: %v", res)
	}
	// An anchor object that no longer says what the row says breaks the chain at its sequence.
	path := filepath.Join(root, filepath.FromSlash(key))
	os.WriteFile(path, []byte(`{"seq": 3, "hash": "00"}`), 0o600)
	if res := verify(t, c, store, &one, nil); res["verified"] != false || res["first_broken_seq"] != float64(3) {
		t.Fatalf("tampered object: %v", res)
	}
	os.Remove(path)
	if res := verify(t, c, store, &one, nil); res["verified"] != false || res["first_broken_seq"] != float64(3) { // a removed anchor object is a finding, not an outage
		t.Fatalf("missing object: %v", res)
	}
	if err := apitest.InRequest(t, c.Tenant, c.Tenant.Human, func(ctx context.Context) error {
		h := endpoints()
		h.Anchors = failingStore{err: errors.New("store down")}
		_, err := h.VerifyAuditChain(ctx, &gen.AuditVerify{FromSeq: gen.NewOptInt64(1)}, gen.VerifyAuditChainParams{})
		return err
	}); err == nil || problem.From(err).Status < 500 {
		t.Fatalf("store down: %v", err)
	}
	os.WriteFile(path, content, 0o600)
	// A rewritten event hash disagrees with its anchor (the chain check sees it too, later or at the same place).
	pgtest.Exec(t, `DO $$ BEGIN SET LOCAL session_replication_role = replica;
		UPDATE audit.audit_event SET hash = '\x`+strings.Repeat("ab", 32)+`' WHERE tenant_id = '`+c.Tenant.ID.String()+`' AND seq = 3; END $$`)
	if res := verify(t, c, store, &one, nil); res["verified"] != false || res["first_broken_seq"] != float64(3) {
		t.Fatalf("tampered event: %v", res)
	}
	pgtest.Exec(t, `DO $$ BEGIN SET LOCAL session_replication_role = replica;
		UPDATE audit.audit_event SET hash = '\x`+obj.Hash+`' WHERE tenant_id = '`+c.Tenant.ID.String()+`' AND seq = 3; END $$`)

	// An object left by a round that did not commit is recorded next time, when it agrees with the chain.
	other := setup(t)
	head := headOf(t, other.Tenant.ID)
	left := blobstore.Key(other.Tenant.ID, blobstore.KindAnchors, audit.ObjectName(head.Seq))
	body, _ := json.Marshal(audit.Anchor{TenantID: other.Tenant.ID, Seq: head.Seq, Hash: head.Hash, AnchoredAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err := store.Put(ctx, left, body, blobstore.PutOptions{ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var at time.Time
	pgtest.Admin(t).QueryRow(ctx, `SELECT anchored_at FROM audit.anchor WHERE tenant_id = $1 AND seq = $2`, other.Tenant.ID, head.Seq).Scan(&at)
	if !at.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("recorded the earlier anchor at %v", at)
	}
	// One that disagrees with the chain is an error: someone rewrote the chain or the object.
	third := setup(t)
	head = headOf(t, third.Tenant.ID)
	wrong, _ := json.Marshal(audit.Anchor{TenantID: third.Tenant.ID, Seq: head.Seq, Hash: strings.Repeat("00", 32)})
	store.Put(ctx, blobstore.Key(third.Tenant.ID, blobstore.KindAnchors, audit.ObjectName(head.Seq)), wrong, blobstore.PutOptions{})
	if _, err := w.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), "tampered") {
		t.Fatalf("disagreeing object: %v", err)
	}
	if got := anchors(t, third.Tenant.ID); len(got) != 0 {
		t.Fatalf("anchored anyway: %v", got)
	}
}

type head struct {
	Seq  int64
	Hash string
}

func headOf(t *testing.T, tenant uuid.UUID) head {
	t.Helper()
	var h head
	var hash []byte
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT seq, hash FROM audit.chain_head WHERE tenant_id = $1`, tenant).Scan(&h.Seq, &hash); err != nil {
		t.Fatal(err)
	}
	h.Hash = hex.EncodeToString(hash)
	return h
}

func TestAnchoringFailures(t *testing.T) {
	c := setup(t)
	ctx := context.Background()
	// A store that refuses writes anchors nothing and says so; reads failing during a check are reported.
	w := anchorer(t, failingStore{err: errors.New("store down")})
	if n, err := w.RunOnce(ctx); err == nil || n != 0 || !strings.Contains(err.Error(), "store down") {
		t.Fatalf("%d %v", n, err)
	}
	if got := anchors(t, c.Tenant.ID); len(got) != 0 {
		t.Fatalf("anchored with a failing store: %v", got)
	}
	// An object that exists but cannot be read back is reported.
	if _, err := anchorer(t, existsThenFails{}).RunOnce(ctx); err == nil || !strings.Contains(err.Error(), "store down") {
		t.Fatalf("exists then fails: %v", err)
	}
	// Database failures: the transaction failing to start, and each statement in turn.
	store := blobstore.Dir{Root: t.TempDir()}
	w = anchorer(t, store)
	w.Begin = func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }
	if _, err := w.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), "database down") {
		t.Fatalf("begin: %v", err)
	}
	pool := pgtest.Pool(t)
	w.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		if s.TenantID != uuid.Nil {
			return nil, errors.New("tenant database down")
		}
		return pool.Begin(ctx, s)
	}
	if _, err := w.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), "tenant database down") {
		t.Fatalf("tenant begin: %v", err)
	}
	// The listing of the tenants to anchor failing.
	w.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, s)
		if err != nil || s.TenantID != uuid.Nil {
			return tx, err
		}
		return &pgtest.FailingTx{Tx: tx, FailAt: 1}, nil
	}
	if _, err := w.RunOnce(ctx); !errors.Is(err, pgtest.ErrInjected) || !strings.Contains(err.Error(), "list tenants to anchor") {
		t.Fatalf("tenant listing: got %v, want the injected failure", err)
	}
	// Each statement of the anchoring transaction failing in turn.
	for n := 1; n < 20; n++ {
		w.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx, s)
			if err != nil || s.TenantID == uuid.Nil {
				return tx, err
			}
			return &pgtest.FailingTx{Tx: tx, FailAt: n}, nil
		}
		_, err := w.RunOnce(ctx)
		if err == nil {
			if n == 1 {
				t.Fatal("succeeded without a statement to fail")
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: %v", n, err)
		}
	}
	w.Begin = nil
	if got := anchors(t, c.Tenant.ID); len(got) != 1 {
		t.Fatalf("anchors after the failures: %v", got)
	}
	// An anchor made by another replica between the listing and the write is left alone.
	second := setup(t)
	w.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, s)
		if err != nil || s.TenantID != second.Tenant.ID {
			return tx, err
		}
		racer := anchorer(t, store)
		if _, err := racer.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		return tx, nil
	}
	if n, err := w.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("raced: %d %v", n, err)
	}
	w.Begin = nil
	if got := anchors(t, second.Tenant.ID); len(got) != 1 {
		t.Fatalf("raced anchors: %v", got)
	}
	// Run loops, logs failures and stops with its context.
	logs := &lockedWriter{}
	broken := anchorer(t, failingStore{err: errors.New("store down")})
	broken.Log = slog.New(slog.NewTextHandler(logs, nil)) // the default interval: the first round runs at once
	setup(t)                                              // a tenant to fail on
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { broken.Run(runCtx); close(done) }()
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(logs.String(), "audit anchoring failed"); {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("logs: %s", logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	// Verification against a store that cannot read is an error of the platform.
	w = anchorer(t, store)
	w.RunOnce(ctx)
	if err := apitest.InRequest(t, c.Tenant, c.Tenant.Human, func(ctx context.Context) error {
		h := endpoints()
		h.Anchors = failingStore{err: errors.New("store down")}
		_, err := h.VerifyAuditChain(ctx, &gen.AuditVerify{}, gen.VerifyAuditChainParams{})
		return err
	}); err == nil || problem.From(err).Status < 500 {
		t.Fatalf("verify with a failing store: %v", err)
	}
	// Every database statement of a verification without an explicit range failing in turn.
	apitest.EveryFailure(t, c.Tenant, c.Tenant.Human, "verify from the last anchor", func(ctx context.Context) error {
		h := endpoints()
		h.Anchors = store
		_, err := h.VerifyAuditChain(ctx, &gen.AuditVerify{}, gen.VerifyAuditChainParams{})
		return err
	})
	// An anchor whose event is gone disagrees with the chain.
	pgtest.Exec(t, `DO $$ BEGIN SET LOCAL session_replication_role = replica;
		DELETE FROM audit.audit_event WHERE tenant_id = '`+c.Tenant.ID.String()+`' AND seq = 3; END $$`)
	one := int64(1)
	if res := verify(t, c, store, &one, nil); res["verified"] != false || res["first_broken_seq"] != float64(3) {
		t.Fatalf("deleted event: %v", res)
	}
	_ = trail.Entry{}
}

// lockedWriter is a log sink the test may read while the worker writes.
type lockedWriter struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedWriter) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
