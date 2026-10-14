package ledger_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TestEntryLocksNeedATenant: the entry locks are keyed by the tenant of the
// transaction; a transaction without one must fail rather than take no
// lock at all (a NULL key makes the strict lock functions skip silently).
func TestEntryLocksNeedATenant(t *testing.T) {
	ctx := context.Background()
	tx, err := pgtest.Pool(t).Begin(ctx, database.Scope{Role: "erp_app"})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	ctx = reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), reqctx.Principal{ID: uuid.New(), Kind: "human"})
	_, err = ledger.OpenItems{}.Unpayable(ctx, []ledger.ItemRef{{FiscalYear: 2026, EntryID: uuid.Must(uuid.NewV7()), LineNo: 1}})
	pgtest.WantErr(t, err, "no tenant")
}

// TestEntryIDsAreVersion7: the entry lock buckets take the low bits of
// entry ids as random (EntryLockBucket), which holds for version 7 ids. A
// manual entry, a port's posting, a reversal and a clearing entry are all
// numbered by post; each must keep a version 7 id.
func TestEntryIDsAreVersion7(t *testing.T) {
	e := setup(t)
	bank, expense, ap := e.account(t, "2000", "100000"), e.account(t, "2000", "520000"), e.account(t, "2000", "211000")
	sup := e.supplier.String()
	invoice, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-01", "EUR", line{expense, "100.00", ""}, line{ap, "-100.00", sup}), "")
	payment, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-20", "EUR", line{ap, "100.00", sup}, line{bank, "-100.00", ""}), "")
	cl, _ := e.Must(201, "POST", "/v1/clearings", clearing(e.de, "2026-03-31", ref{invoice, 2}, ref{payment, 1}), "")
	other, otherTag := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-02", "EUR", line{bank, "1.00", ""}, line{expense, "-1.00", ""}), "")
	reversal, _ := e.Must(200, "POST", "/v1/journal-entries/"+other["id"].(string)+":reverse", "", otherTag)
	posted := settlePost(t, e, e.de, "2000", "EUR", 500, "INV-V7")
	for name, id := range map[string]string{"manual entry": invoice["id"].(string), "clearing entry": cl["clearing_entry_id"].(string),
		"reversal": reversal["id"].(string), "port posting": posted.EntryID.String()} {
		t.Run(name, func(t *testing.T) {
			if got := uuid.MustParse(id).Version(); got != 7 {
				t.Errorf("entry id version: got %d, want 7", got)
			}
		})
	}
}

// TestBucketsAreTenantScoped: an entry lock one tenant holds never holds up
// another tenant whose entry falls in the same bucket number; the same
// tenant's entry in that bucket waits, which shows the probe would see it.
func TestBucketsAreTenantScoped(t *testing.T) {
	e := setup(t)
	other := pgtest.NewTenant(t, perms...)
	bank, revenue := e.account(t, "2000", "100000"), e.account(t, "2000", "400000")
	je, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-15", "EUR", line{bank, "5.00", ""}, line{revenue, "-5.00", ""}), "")
	held := uuid.MustParse(je["id"].(string))
	twin := uuid.Must(uuid.NewV7())
	twin[14], twin[15] = held[14], held[15]
	if got, want := ledger.EntryLockBucket(twin), ledger.EntryLockBucket(held); got != want {
		t.Fatalf("twin bucket: got %d, want %d", got, want)
	}
	probe := func(ctx context.Context) error {
		_, err := ledger.OpenItems{}.Unpayable(ctx, []ledger.ItemRef{{FiscalYear: 2026, EntryID: twin, LineNo: 1}})
		return err
	}
	locked, release := make(chan struct{}), make(chan struct{})
	letGo := sync.OnceFunc(func() { close(release) })
	defer letGo()
	firstDone := make(chan error, 1)
	go func() {
		err := errSetup
		defer func() { firstDone <- err }()
		err = apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			if _, err := postings().Reverse(ctx, held, nil, nil); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	select {
	case <-locked:
	case err := <-firstDone:
		t.Fatalf("reversal holding the bucket: %v", err)
	}
	// Another tenant's entry in the same bucket number goes ahead at once.
	if err := apitest.InRequest(t, other, other.Human, probe); err != nil {
		t.Fatalf("another tenant's entry in the same bucket: got %v, want it not to wait", err)
	}
	// The same tenant's entry in the bucket waits for the reversal.
	sameDone := make(chan error, 1)
	go func() { sameDone <- apitest.InRequest(t, e.ten, e.ten.Human, probe) }()
	pgtest.Eventually(t, 10*time.Second, func() bool {
		var waiting int
		pgtest.Scan(t, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted
			AND ((classid::bigint << 32) | objid::bigint) = hashtextextended('ledger.journal_entry:' || $1::uuid || ':' || $2::int, 0)`,
			[]any{e.ten.ID, ledger.EntryLockBucket(held)}, &waiting)
		return waiting > 0
	}, "the same tenant's entry in the bucket did not wait for the reversal")
	letGo()
	if err := <-firstDone; err != nil {
		t.Fatalf("reversal: %v", err)
	}
	if err := <-sameDone; err != nil {
		t.Fatalf("same tenant's probe after the reversal: %v", err)
	}
}
