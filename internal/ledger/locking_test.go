package ledger_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestReversalAndClearingSerialize runs a reversal and a clearing of the
// same entry in two transactions: whichever comes second waits for the
// first to commit, then sees its effect and is refused, so an entry never
// ends up both reversed and cleared.
func TestReversalAndClearingSerialize(t *testing.T) {
	e := setup(t)
	bank, expense, ap := e.account(t, "2000", "100000"), e.account(t, "2000", "520000"), e.account(t, "2000", "211000")
	sup := e.supplier.String()
	h := endpoints()
	type pair struct{ invoice, payment map[string]any }
	newPair := func(t *testing.T) pair {
		t.Helper()
		e := e.in(t)
		inv, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-01", "EUR", line{expense, "100.00", ""}, line{ap, "-100.00", sup}), "")
		pay, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-20", "EUR", line{ap, "100.00", sup}, line{bank, "-100.00", ""}), "")
		return pair{inv, pay}
	}
	id := func(m map[string]any) uuid.UUID { return uuid.MustParse(m["id"].(string)) }
	reverse := func(p pair) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := postings().Reverse(ctx, id(p.invoice), nil, nil)
			return err
		}
	}
	clear := func(p pair) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := h.CreateClearing(ctx, &gen.ClearingCreate{LegalEntityID: e.de, ClearingDate: date("2026-03-31"), Items: []gen.ClearingItemRef{
				{FiscalYear: 2026, JournalEntryID: id(p.invoice), LineNo: 2}, {FiscalYear: 2026, JournalEntryID: id(p.payment), LineNo: 1}}},
				gen.CreateClearingParams{})
			return err
		}
	}

	t.Run("clearing first", func(t *testing.T) {
		p := newPair(t)
		first, second := serialize(t, e.ten, id(p.invoice), clear(p), reverse(p))
		if first != nil {
			t.Fatalf("clearing: got %v, want it committed", first)
		}
		if got := problem.From(second).Code; got != "ledger.entry_cleared" {
			t.Errorf("reversal after the clearing: got %v, want ledger.entry_cleared", second)
		}
	})
	t.Run("reversal first", func(t *testing.T) {
		p := newPair(t)
		first, second := serialize(t, e.ten, id(p.invoice), reverse(p), clear(p))
		if first != nil {
			t.Fatalf("reversal: got %v, want it committed", first)
		}
		if got := problem.From(second).Code; got != "ledger.entry_reversed" {
			t.Errorf("clearing after the reversal: got %v, want ledger.entry_reversed", second)
		}
	})
}

// errSetup is what serialize reports for a transaction whose setup failed:
// apitest.Committed then fails the test and ends its goroutine, which still
// has to answer, or serialize would wait for it forever.
var errSetup = errors.New("transaction setup failed")

// serialize runs first in a transaction and holds it open while second
// runs in another; it lets first commit once second waits for the lock on
// the entry (or has finished, which it must not), and returns both
// results.
func serialize(t *testing.T, ten pgtest.Tenant, entry uuid.UUID, first, second func(ctx context.Context) error) (firstErr, secondErr error) {
	t.Helper()
	held, release := make(chan struct{}), make(chan struct{})
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	// Deferred too, so that a failed wait does not leave first holding its transaction.
	letGo := sync.OnceFunc(func() { close(release) })
	defer letGo()
	go func() {
		err := errSetup
		defer func() { firstDone <- err }()
		err = apitest.Committed(t, ten, ten.Human, func(ctx context.Context) error {
			if err := first(ctx); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	select {
	case <-held:
	case err := <-firstDone:
		return err, nil
	}
	go func() {
		err := errSetup
		defer func() { secondDone <- err }()
		err = apitest.Committed(t, ten, ten.Human, second)
	}()
	admin := pgtest.Admin(t)
	finished := false
	pgtest.Eventually(t, 10*time.Second, func() bool {
		select {
		case secondErr = <-secondDone:
			finished = true
			return true
		default:
		}
		var waiting int
		if err := admin.QueryRow(context.Background(), `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted
			AND ((classid::bigint << 32) | objid::bigint) = hashtextextended('ledger.journal_entry:' || $2::uuid || ':' || $1::int, 0)`, ledger.EntryLockBucket(entry), ten.ID).Scan(&waiting); err != nil {
			t.Fatalf("read the lock waits: %v", err)
		}
		return waiting > 0
	}, "the second transaction neither waited for the entry nor finished")
	if finished {
		t.Errorf("second transaction: finished with %v while the first was still open, want it to wait for the entry", secondErr)
	}
	letGo()
	firstErr = <-firstDone
	if !finished {
		secondErr = <-secondDone
	}
	return firstErr, secondErr
}
