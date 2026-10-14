package ledger_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// migratedPair is a migrated invoice and the payment that cleared it in
// the source system: two entries whose supplier lines net to zero.
type migratedPair struct{ invoice, payment uuid.UUID }

func (p migratedPair) items() []ledger.ItemRef {
	return []ledger.ItemRef{{FiscalYear: 2026, EntryID: p.invoice, LineNo: 2}, {FiscalYear: 2026, EntryID: p.payment, LineNo: 1}}
}

// lockingPairs posts pairs until two of them, first and second, lock in an
// order a reservation can cut across: the second's invoice is in a bucket
// below the first's invoice and in neither of the first's buckets.
func lockingPairs(t *testing.T, e env) (first, second migratedPair) {
	t.Helper()
	bank, expense, ap := e.account(t, "2000", "100000"), e.account(t, "2000", "520000"), e.account(t, "2000", "211000")
	sup := e.supplier.String()
	var pairs []migratedPair
	for range 12 {
		inv, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-01", "EUR", line{expense, "10.00", ""}, line{ap, "-10.00", sup}), "")
		pay, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-20", "EUR", line{ap, "10.00", sup}, line{bank, "-10.00", ""}), "")
		pairs = append(pairs, migratedPair{uuid.MustParse(inv["id"].(string)), uuid.MustParse(pay["id"].(string))})
	}
	b := ledger.EntryLockBucket
	for _, f := range pairs {
		for _, s := range pairs {
			if b(s.invoice) < b(f.invoice) && b(s.invoice) != b(f.payment) {
				return f, s
			}
		}
	}
	t.Fatal("no two pairs lock in the order the test needs; post more")
	return migratedPair{}, migratedPair{}
}

func TestLockAllEntriesFailures(t *testing.T) {
	e := setup(t)
	call := func(ctx context.Context) error { return postings().LockAllEntries(ctx) }
	if call(context.Background()) == nil {
		t.Error("outside a unit of work: got no error, want one")
	}
	apitest.EveryFailure(t, e.ten, e.ten.Human, "lock all entries", call)
}

// TestMigrationLoadAgainstAReservation: a migration load clears the
// documents SAP cleared one clearing at a time, in one transaction, after
// posting them. A proposal reserving items of both clearings in the
// meantime must neither deadlock with it nor be refused for anything but
// what the load did: the load takes every entry bucket of the tenant before
// it posts (Postings.LockAllEntries), so the reservation waits for it.
func TestMigrationLoadAgainstAReservation(t *testing.T) {
	e := setup(t)
	first, second := lockingPairs(t, e)
	clear := func(ctx context.Context, p migratedPair) error {
		_, err := postings().Clear(ctx, e.de, date("2026-03-31"), ledger.ItemRef{FiscalYear: 2026, EntryID: p.payment, LineNo: 1}, p.items())
		return err
	}
	cleared, goOn := make(chan struct{}), make(chan struct{})
	letGo := sync.OnceFunc(func() { close(goOn) })
	defer letGo()
	loadDone := make(chan error, 1)
	go func() {
		err := errSetup
		defer func() { loadDone <- err }()
		err = apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			if err := postings().LockAllEntries(ctx); err != nil { // as the integration loader does first
				return err
			}
			if err := clear(ctx, first); err != nil {
				return err
			}
			close(cleared)
			<-goOn
			return clear(ctx, second)
		})
	}()
	select {
	case <-cleared:
	case err := <-loadDone:
		t.Fatalf("load, first clearing: %v", err)
	}
	reserveDone := make(chan error, 1)
	go func() {
		reserveDone <- apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			return postings().Reserve(ctx, ledger.Owner{Context: "payments", ID: uuid.New(), Reference: "PR1"},
				[]ledger.ItemRef{second.items()[0], first.items()[0]})
		})
	}()
	pgtest.Eventually(t, 10*time.Second, func() bool {
		var waiting int
		pgtest.Scan(t, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted
			AND ((classid::bigint << 32) | objid::bigint) IN (SELECT hashtextextended('ledger.journal_entry:' || $1::uuid || ':' || b, 0) FROM unnest($2::int[]) AS x(b))`,
			[]any{e.ten.ID, []int32{ledger.EntryLockBucket(first.invoice), ledger.EntryLockBucket(second.invoice)}}, &waiting)
		return waiting > 0
	}, "the reservation did not wait for the load")
	letGo()
	if err := <-loadDone; err != nil {
		t.Fatalf("load: got %v, want both clearings committed", err)
	}
	reserved := <-reserveDone
	if got := problem.From(reserved).Code; got != "ledger.item_already_cleared" {
		t.Errorf("reservation after the load: got %s (%v), want ledger.item_already_cleared", got, reserved)
	}
}
