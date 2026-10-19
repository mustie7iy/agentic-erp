package offboarding_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/offboarding"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// unreadable lists what its store lists and reads nothing.
type unreadable struct{ blobstore.Dir }

func (u unreadable) Get(context.Context, string) ([]byte, string, error) {
	return nil, "", errors.New("store down")
}

// TestAdopt: a cell restored to before it exported and erased a tenant
// takes the export still in the store as the tenant's, due at once, and
// erases the tenant against it; without one there is nothing to adopt.
func TestAdopt(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Pool(t)
	clock := time.Now().UTC().Truncate(time.Second)
	store := blobstore.Dir{Root: t.TempDir(), Now: func() time.Time { return clock }}
	w := worker(t, pool, store, &clock)
	tenant := fixture.New(t, "payables.invoice.read").Tenant.ID
	pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'offboarded', version = version + 1 WHERE id = $1`, tenant)
	if _, err := w.Adopt(ctx, tenant); !errors.Is(err, offboarding.ErrNoExport) {
		t.Fatalf("adopt before any export: got %v, want ErrNoExport", err)
	}
	// An older export of an earlier offboarding, which is not the one.
	if err := store.Put(ctx, blobstore.Key(tenant, blobstore.KindExports, "1600000000/manifest.json"), []byte(`{"tenant_id":"`+tenant.String()+`"}`), blobstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	exported, err := w.Export(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	// The restore: the cell forgets the export.
	pgtest.Exec(t, `UPDATE tenancy.tenant SET exported_at = NULL, export_ref = NULL, erase_after = NULL, version = version + 1 WHERE id = $1`, tenant)
	clock = clock.Add(time.Minute)
	adopted, err := w.Adopt(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if adopted.Ref != exported.Ref {
		t.Errorf("adopted export: got %s, want the newest, %s", adopted.Ref, exported.Ref)
	}
	if adopted.Rows != exported.Rows {
		t.Errorf("adopted rows: got %d, want %d", adopted.Rows, exported.Rows)
	}
	if !adopted.ExportedAt.Equal(exported.ExportedAt) {
		t.Errorf("adopted exported at: got %s, want the manifest's %s", adopted.ExportedAt, exported.ExportedAt)
	}
	if !adopted.EraseAfter.Equal(clock) {
		t.Errorf("adopted erase after: got %s, want now, %s", adopted.EraseAfter, clock)
	}
	if _, err := w.Adopt(ctx, tenant); err == nil {
		t.Error("adopt twice: got nil, want the conflict of an export already recorded")
	}
	if _, err := w.Erase(ctx, tenant, offboarding.Actor, nil); err != nil {
		t.Errorf("erase against the adopted export: %v", err)
	}

	stranger := uuid.New()
	if err := store.Put(ctx, blobstore.Key(stranger, blobstore.KindExports, "1700000000/manifest.json"), []byte(`{"tenant_id":"`+tenant.String()+`"}`), blobstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		store  blobstore.Store
		tenant uuid.UUID
	}{
		{"a store that cannot list", plainStore{store}, tenant},
		{"a store that fails to list", brokenEraser{Store: store, list: errors.New("list down")}, tenant},
		{"a manifest that cannot be read", unreadable{store}, tenant},
		{"another tenant's manifest", store, stranger},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := worker(t, pool, c.store, &clock)
			if _, err := w.Adopt(ctx, c.tenant); err == nil || errors.Is(err, offboarding.ErrNoExport) {
				t.Errorf("got %v, want an error other than ErrNoExport", err)
			}
		})
	}
}
