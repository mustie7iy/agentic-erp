package drverify_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/db"
	"github.com/mustie7iy/agentic-erp/internal/audit"
	"github.com/mustie7iy/agentic-erp/internal/controlplane"
	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/drverify"
	"github.com/mustie7iy/agentic-erp/internal/offboarding"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// cellDirectory is the directory's entries of the cell, or its failure.
type cellDirectory struct {
	entries []directory.Entry
	err     error
}

func (d cellDirectory) CellTenants(context.Context, string) ([]directory.Entry, error) {
	return d.entries, d.err
}

// listless is a store that cannot list; failingList one whose listing or
// reading fails.
type listless struct{ blobstore.Store }

type failingStore struct {
	blobstore.Dir
	list, get error
}

func (f failingStore) List(ctx context.Context, prefix string) ([]string, error) {
	if f.list != nil {
		return nil, f.list
	}
	return f.Dir.List(ctx, prefix)
}

func (f failingStore) Get(ctx context.Context, key string) ([]byte, string, error) {
	if f.get != nil {
		return nil, "", f.get
	}
	return f.Dir.Get(ctx, key)
}

func (c cell) reconciler(store blobstore.Store, dir drverify.CellDirectory, workerRole string) *drverify.Reconciler {
	return &drverify.Reconciler{Verifier: c.verifier(db.LatestVersion()), Store: store, Directory: dir, Cell: "eu1",
		Control: &controlplane.Service{Pool: c.pool, AppRole: "erp_app", ControlRole: "erp_control"},
		Offboarding: &offboarding.Worker{Pool: c.pool, Store: store, AppRole: "erp_app", WorkerRole: workerRole,
			Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
}

// anchored gives the tenant two audit events, both anchored: what a cell
// holds before the restore point.
func (c cell) anchored(t *testing.T, store blobstore.Store, tenant uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	v := c.verifier(db.LatestVersion())
	a := &audit.Anchorer{Pool: c.pool, Store: store, AppRole: "erp_app", WorkerRole: "erp_worker", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, name := range []string{"one", "two"} {
		if _, err := v.Mark(ctx, tenant, name); err != nil {
			t.Fatal(err)
		}
		if _, err := a.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// changes counts the cell's audit events and the tenancy events it
// published: what a write through the control plane or the worker adds.
func (c cell) changes(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := c.pool.WithConn(context.Background(), func(conn *pgx.Conn) error {
		return conn.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM audit.audit_event) + (SELECT count(*) FROM tenancy.outbox)`).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func put(t *testing.T, store blobstore.Store, tenant uuid.UUID, seq int64, body string) {
	t.Helper()
	if err := store.Put(context.Background(), blobstore.Key(tenant, blobstore.KindAnchors, audit.ObjectName(seq)), []byte(body), blobstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
}

func contains(problems []string, want string) bool {
	return slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, want) })
}

// TestReconcileAnchors: the restored chains against their anchors in the
// object store.
func TestReconcileAnchors(t *testing.T) {
	ctx := context.Background()
	c := newCell(t)
	store := blobstore.Dir{Root: t.TempDir()}
	good, lost, torn, missing, forged := c.tenant(t), c.tenant(t), c.tenant(t), c.tenant(t), c.tenant(t)
	for _, id := range []uuid.UUID{good, lost, torn, missing, forged} {
		c.anchored(t, store, id)
	}
	// lost: the restore kept the events but not the second anchor's row,
	// and an anchor of an event after the point is in the store.
	c.exec(t, true, `DELETE FROM audit.anchor WHERE tenant_id = $1 AND seq = (SELECT max(seq) FROM audit.anchor WHERE tenant_id = $1)`, lost)
	put(t, store, lost, 99, `{"tenant_id":"`+lost.String()+`","seq":99,"hash":"00"}`)
	c.exec(t, true, `UPDATE audit.anchor SET hash = sha256('torn') WHERE tenant_id = $1 AND seq = 1`, torn)
	c.exec(t, true, `UPDATE audit.anchor SET object_ref = 'tenants/'||tenant_id||'/audit/anchors/gone.json' WHERE tenant_id = $1 AND seq = 1`, missing)
	put(t, store, forged, 3, `not json`)
	c.exec(t, true, `DELETE FROM audit.anchor WHERE tenant_id = $1 AND seq = 1`, forged)
	if err := store.Delete(ctx, blobstore.Key(forged, blobstore.KindAnchors, audit.ObjectName(1))); err != nil {
		t.Fatal(err)
	}
	put(t, store, forged, 1, `{"tenant_id":"`+forged.String()+`","seq":1,"hash":"00"}`)

	rep, err := c.reconciler(store, nil, "erp_worker").Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"tenant " + torn.String() + ": anchor at seq 1 disagrees",
		"tenant " + missing.String() + ": anchor tenants/" + missing.String() + "/audit/anchors/gone.json is not in the object store",
		"tenant " + forged.String() + ": the object store's anchor at seq 1 disagrees",
		"tenant " + forged.String() + ": tenants/" + forged.String() + "/audit/anchors/" + audit.ObjectName(3) + " is not an anchor",
	} {
		if !contains(rep.Problems, want) {
			t.Errorf("problems: got %q, want one containing %q", rep.Problems, want)
		}
	}
	if len(rep.Problems) != 4 || rep.AnchorsAfter != 1 || rep.Tenants != 5 || rep.Directory != 0 {
		t.Errorf("report: got %d problems, %d anchors after, %d tenants, %d entries, want 4, 1, 5, 0", len(rep.Problems), rep.AnchorsAfter, rep.Tenants, rep.Directory)
	}

	for name, s := range map[string]blobstore.Store{
		"a store that cannot list": listless{store},
		"a listing that fails":     failingStore{Dir: store, list: errors.New("list down")},
		"an object unread":         failingStore{Dir: store, get: errors.New("get down")},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := c.reconciler(s, nil, "erp_worker").Reconcile(ctx); err == nil {
				t.Error("got nil, want the store's failure")
			}
		})
	}
	t.Run("database failures", func(t *testing.T) {
		for n := 1; ; n++ {
			count := 0
			rc := c.reconciler(store, nil, "erp_worker")
			rc.Verifier.Begin = func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
				tx, err := c.pool.Begin(ctx, s)
				return &pgtest.FailingTx{Tx: tx, FailAt: n, Count: &count}, err
			}
			_, err := rc.Reconcile(ctx)
			if count < n {
				if err != nil {
					t.Fatalf("all %d statements ran: %v", count, err)
				}
				break
			}
			if !errors.Is(err, pgtest.ErrInjected) {
				t.Fatalf("statement %d: got %v, want the injected failure", n, err)
			}
		}
	})
}

// TestReconcileDirectory: the tenants the directory places in the cell,
// offboarded and erased again as the directory has them.
func TestReconcileDirectory(t *testing.T) {
	ctx := context.Background()
	c := newCell(t)
	store := blobstore.Dir{Root: t.TempDir()}
	active, offboard, erase, noExport, provisioning, erased, unerasable := c.tenant(t), c.tenant(t), c.tenant(t), c.tenant(t), c.tenant(t), c.tenant(t), c.tenant(t)
	absent := uuid.New()
	// erase: exported and erased after the restore point; the cell is back
	// before its offboarding, the export still in the store.
	w := c.reconciler(store, nil, "erp_worker").Offboarding
	c.exec(t, false, `UPDATE tenancy.tenant SET status = 'offboarded', version = version + 1 WHERE id = ANY($1)`, []uuid.UUID{erase, erased, unerasable})
	for _, id := range []uuid.UUID{erase, erased, unerasable} {
		if _, err := w.Export(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Erase(ctx, erased, offboarding.Actor, nil); err != nil {
		t.Fatal(err)
	}
	c.exec(t, false, `UPDATE tenancy.tenant SET status = 'active', exported_at = NULL, export_ref = NULL, erase_after = NULL, version = version + 1 WHERE id = $1`, erase)
	c.exec(t, false, `UPDATE tenancy.tenant SET status = 'provisioning', version = version + 1 WHERE id = $1`, provisioning)
	entry := func(id uuid.UUID, status string) directory.Entry {
		return directory.Entry{TenantID: id, Cell: "eu1", Status: status}
	}
	dir := cellDirectory{entries: []directory.Entry{entry(active, directory.Active), entry(offboard, directory.Offboarded), entry(erase, directory.Erased),
		entry(noExport, directory.Erased), entry(provisioning, directory.Offboarded), entry(erased, directory.Erased), entry(absent, directory.Active)}}

	rep, err := c.reconciler(store, dir, "erp_worker").Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Offboarded, []uuid.UUID{offboard, erase, noExport}) {
		t.Errorf("offboarded again: got %v, want %v", rep.Offboarded, []uuid.UUID{offboard, erase, noExport})
	}
	if !slices.Equal(rep.Erased, []uuid.UUID{erase}) {
		t.Errorf("erased again: got %v, want %v", rep.Erased, []uuid.UUID{erase})
	}
	for _, want := range []string{
		"tenant " + noExport.String() + ": erase again: offboarding: no export",
		"tenant " + provisioning.String() + ": offboard again:",
		"tenant " + absent.String() + ": the directory places it in cell eu1, which lacks it",
	} {
		if !contains(rep.Problems, want) {
			t.Errorf("problems: got %q, want one containing %q", rep.Problems, want)
		}
	}
	if len(rep.Problems) != 3 || rep.Directory != 7 {
		t.Errorf("report: got %d problems and %d entries, want 3 and 7", len(rep.Problems), rep.Directory)
	}
	var erasedAt *string
	if err := c.pool.WithConn(ctx, func(conn *pgx.Conn) error {
		return conn.QueryRow(ctx, `SELECT erased_at::text FROM tenancy.tenant WHERE id = $1`, erase).Scan(&erasedAt)
	}); err != nil {
		t.Fatal(err)
	}
	if erasedAt == nil {
		t.Error("the tenant the directory had erased: got not erased, want erased")
	}

	t.Run("twice", func(t *testing.T) {
		before := c.changes(t)
		again, err := c.reconciler(store, dir, "erp_worker").Reconcile(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(again.Offboarded)+len(again.Erased) != 0 {
			t.Errorf("second run: got %v offboarded and %v erased, want none", again.Offboarded, again.Erased)
		}
		if !slices.Equal(again.Problems, rep.Problems) {
			t.Errorf("second run's problems: got %q, want %q", again.Problems, rep.Problems)
		}
		if after := c.changes(t); after != before {
			t.Errorf("audit events and events published: got %d, want %d (nothing new)", after, before)
		}
	})
	t.Run("an erasure that fails", func(t *testing.T) {
		dir := cellDirectory{entries: []directory.Entry{entry(unerasable, directory.Erased)}}
		rep, err := c.reconciler(store, dir, "nobody").Reconcile(ctx)
		if err != nil || !contains(rep.Problems, "tenant "+unerasable.String()+": erase again:") {
			t.Errorf("got %v and %q, want the erasure's failure as a problem", err, rep.Problems)
		}
	})
	t.Run("a directory that fails", func(t *testing.T) {
		if _, err := c.reconciler(store, cellDirectory{err: errors.New("directory down")}, "erp_worker").Reconcile(ctx); err == nil {
			t.Error("got nil, want the directory's failure")
		}
	})
}
