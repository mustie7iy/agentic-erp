package audit_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	trail "github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// slowStore holds the Put of one tenant's anchor until released, as a
// slow object store would.
type slowStore struct {
	blobstore.Store
	tenant  string
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (s *slowStore) Put(ctx context.Context, key string, content []byte, opts blobstore.PutOptions) error {
	if strings.Contains(key, s.tenant) {
		s.once.Do(func() { close(s.entered) })
		<-s.release
	}
	return s.Store.Put(ctx, key, content, opts)
}

// TestAnchoringHoldsNoLockAcrossTheStore: while the anchor object is being
// written, the tenant's audited writes go on: the anchorer holds no lock
// on the chain head across the store's call (every audited write takes
// it, so a slow store would turn them into lock timeouts).
func TestAnchoringHoldsNoLockAcrossTheStore(t *testing.T) {
	c := setup(t) // three events
	store := &slowStore{Store: blobstore.Dir{Root: t.TempDir()}, tenant: c.Tenant.ID.String(), entered: make(chan struct{}), release: make(chan struct{})}
	w := anchorer(t, store)
	ctx := context.Background()
	done := make(chan error, 1)
	go func() { _, err := w.RunOnce(ctx); done <- err }()
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(store.release) }) }) // the round ends should the test fail first
	<-store.entered
	if err := apitest.Committed(t, c.Tenant, c.Tenant.Human, func(ctx context.Context) error {
		return trail.Add(ctx, trail.Entry{Action: "test.thing.do", ObjectType: "test.thing", Outcome: "succeeded"})
	}); err != nil {
		t.Fatalf("got %v writing an audit event while the anchor is stored, want it written", err)
	}
	release.Do(func() { close(store.release) })
	if err := <-done; err != nil {
		t.Fatalf("anchoring: %v", err)
	}
	if got := anchors(t, c.Tenant.ID); len(got) != 1 || got[0] != 3 {
		t.Errorf("anchors: got %v, want [3] (the head read before the write)", got)
	}
	if n, err := w.RunOnce(ctx); err != nil || n < 1 {
		t.Fatalf("next round: got %d %v, want the moved head anchored", n, err)
	}
	if got := anchors(t, c.Tenant.ID); len(got) != 2 || got[1] != 4 {
		t.Errorf("anchors after the next round: got %v, want [3 4]", got)
	}
}

// TestAnchoringRecordFails: the row's insert failing after the object is
// written is reported, and the next round records the object it finds.
func TestAnchoringRecordFails(t *testing.T) {
	c := setup(t)
	pool := pgtest.Pool(t)
	w := anchorer(t, blobstore.Dir{Root: t.TempDir()})
	w.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, s)
		if err != nil || s.TenantID != c.Tenant.ID || s.ReadOnly {
			return tx, err
		}
		return &pgtest.FailingTx{Tx: tx, FailAt: 1}, nil
	}
	_, err := w.RunOnce(context.Background())
	if !errors.Is(err, pgtest.ErrInjected) || !strings.Contains(err.Error(), "record anchor") {
		t.Fatalf("got %v, want the injected failure of the row's insert", err)
	}
	// The row's transaction failing to begin is reported too.
	w.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		if s.TenantID == c.Tenant.ID && !s.ReadOnly {
			return nil, errors.New("database down for the record")
		}
		return pool.Begin(ctx, s)
	}
	_, err = w.RunOnce(context.Background())
	pgtest.WantErr(t, err, "database down for the record")
	w.Begin = nil
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("next round: %v", err)
	}
	if got := anchors(t, c.Tenant.ID); len(got) != 1 || got[0] != 3 {
		t.Errorf("anchors: got %v, want [3]", got)
	}
}
