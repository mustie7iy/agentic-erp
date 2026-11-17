package offboarding_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/offboarding"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore/blobstoretest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestErasureKeepsExportAndAnchors: the erasure leaves the tenant's export
// and its audit anchors readable whatever the store does with a delete:
// a versioned S3 bucket hides a locked object behind a delete marker, and
// a store without retention (ERP_OBJECT_RETENTION 0) deletes it outright,
// so the store's lock cannot be what keeps them (ADR-0021 As built).
func TestErasureKeepsExportAndAnchors(t *testing.T) {
	pool := pgtest.Pool(t)
	clock := time.Now().UTC().Truncate(time.Second)
	cases := map[string]struct {
		store     blobstore.Store
		retention time.Duration
	}{
		"versioned S3 bucket under Object Lock": {blobstoretest.New(t).Store(), 24 * time.Hour},
		"directory without retention":           {blobstore.Dir{Root: t.TempDir(), Now: func() time.Time { return clock }}, 0},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			w := worker(t, pool, c.store, &clock)
			w.Retention = c.retention
			tenant := fixture.New(t, "payables.invoice.read").Tenant.ID
			document := blobstore.Key(tenant, blobstore.KindDocuments, "abc")
			anchor := blobstore.Key(tenant, blobstore.KindAnchors, "000000000001.json")
			opts := blobstore.PutOptions{ContentType: "application/json"}
			if c.retention > 0 {
				opts.RetainUntil = clock.Add(c.retention)
			}
			for _, key := range []string{document, anchor} {
				if err := c.store.Put(ctx, key, []byte("{}"), opts); err != nil {
					t.Fatalf("store %s: %v", key, err)
				}
			}
			pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'offboarded', version = version + 1 WHERE id = $1`, tenant)
			ex, err := w.Export(ctx, tenant)
			if err != nil {
				t.Fatalf("export: %v", err)
			}
			exported, err := c.store.(blobstore.Eraser).List(ctx, blobstore.Key(tenant, blobstore.KindExports, ""))
			if err != nil || len(exported) < 2 {
				t.Fatalf("export objects: got %v %v, want a manifest and its parts", exported, err)
			}
			er, err := w.Erase(ctx, tenant, offboarding.Actor, nil)
			if err != nil {
				t.Fatalf("erase: %v", err)
			}
			if want := len(exported) + 1; er.Kept != want {
				t.Errorf("kept: got %d, want %d (the export's objects and the anchor)", er.Kept, want)
			}
			if er.Objects != 1 {
				t.Errorf("deleted: got %d, want 1 (the document)", er.Objects)
			}
			for _, key := range append(exported, anchor, ex.Ref) {
				if _, _, err := c.store.Get(ctx, key); err != nil {
					t.Errorf("got %v reading %s after the erasure, want it kept", err, key)
				}
			}
			if _, _, err := c.store.Get(ctx, document); !errors.Is(err, blobstore.ErrNotFound) {
				t.Errorf("got %v reading the document after the erasure, want ErrNotFound", err)
			}
		})
	}
}
