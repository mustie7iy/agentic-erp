package payments_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// TestProposalAndReversalSerialize runs a proposal and the reversal of an
// invoice it would pay in two transactions: both take the legal entity's
// proposals lock, so whichever comes second waits for the first to commit
// and then sees its effect. A reversal never commits under a proposal that
// already chose the invoice, and a proposal never chooses an invoice a
// reversal took.
func TestProposalAndReversalSerialize(t *testing.T) {
	e := setup(t)
	h := endpoints(nil)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
	lock := "payments.propose." + e.DE.String()
	build := func(run uuid.UUID) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			tx, _, err := reqctx.Request(ctx)
			if err != nil {
				return err
			}
			j, ok, err := h.JobFor(ctx, tx, run)
			if err != nil {
				return fmt.Errorf("proposal job of run %s: %w", run, err)
			}
			if !ok {
				return fmt.Errorf("proposal job of run %s: got none, want one", run)
			}
			return h.Process(ctx, j)
		}
	}
	invoices := &payables.Endpoints{Postings: ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}},
		Payments: payments.Reservations{}, LegalEntities: tenancy.Directory{}, Settings: configuration.Settings{}}
	reverse := func(inv map[string]any) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := invoices.ReverseInvoice(ctx, uuidOf(id(inv)), func(int32) error { return nil }, "Wrong amount", nil)
			return err
		}
	}

	t.Run("proposal first", func(t *testing.T) {
		e := e.in(t)
		inv := e.invoice(t, e.Supplier, "INV-P", "100.00", "2026-03-01")
		op := e.propose(t, "2026-04-15", "2026-04-15")
		run := uuidOf(op["resource"].(string)[len("/v1/payment-runs/"):])
		first, second := serializeOn(t, e.Tenant, lock, build(run), reverse(inv))
		if first != nil {
			t.Fatalf("proposal: got %v, want it committed", first)
		}
		pgtest.WantErr(t, second, "payables.invoice_in_payment_run")
	})
	t.Run("reversal first", func(t *testing.T) {
		e := e.in(t)
		inv := e.invoice(t, e.Supplier, "INV-R", "100.00", "2026-03-01")
		op := e.propose(t, "2026-04-15", "2026-04-15")
		run := uuidOf(op["resource"].(string)[len("/v1/payment-runs/"):])
		first, second := serializeOn(t, e.Tenant, lock, reverse(inv), build(run))
		if first != nil {
			t.Fatalf("reversal: got %v, want it committed", first)
		}
		if second != nil {
			t.Fatalf("proposal after the reversal: got %v, want it committed", second)
		}
		got, _ := e.Must(200, "GET", "/v1/payment-runs/"+run.String(), "", "")
		if got["status"] != "proposed" {
			t.Errorf("run after the reversal: got %v, want proposed", got["status"])
		}
		if pays := e.payments(t, got); len(pays) != 0 {
			t.Errorf("run payments: got %v, want none for the reversed invoice", pays)
		}
	})
}

// errSetup is what serializeOn reports for a transaction whose setup
// failed: apitest.Committed then fails the test and ends its goroutine,
// which still has to answer, or serializeOn would wait for it forever.
var errSetup = errors.New("transaction setup failed")

// serializeOn runs first in a transaction and holds it open while second
// runs in another; it lets first commit once second waits for an advisory
// lock (or has finished, which it must not), and returns both results. The
// lock second waits for must be the one named key: a wait for another
// fails the test.
func serializeOn(t *testing.T, ten pgtest.Tenant, key string, first, second func(ctx context.Context) error) (firstErr, secondErr error) {
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
	var pid atomic.Int32 // the second transaction's backend
	go func() {
		err := errSetup
		defer func() { secondDone <- err }()
		err = apitest.Committed(t, ten, ten.Human, func(ctx context.Context) error {
			tx, _, err := reqctx.Request(ctx)
			if err != nil {
				return err
			}
			var backend int32
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&backend); err != nil {
				return fmt.Errorf("read the backend: %w", err)
			}
			pid.Store(backend)
			return second(ctx)
		})
	}()
	finished := false
	var onKey, waits int
	pgtest.Eventually(t, 10*time.Second, func() bool {
		select {
		case secondErr = <-secondDone:
			finished = true
			return true
		default:
		}
		if pid.Load() == 0 {
			return false
		}
		pgtest.Scan(t, `SELECT count(*) FILTER (WHERE ((classid::bigint << 32) | objid::bigint) = hashtextextended($2, 0)), count(*)
			FROM pg_locks WHERE locktype = 'advisory' AND NOT granted AND pid = $1`, []any{pid.Load(), key}, &onKey, &waits)
		return waits > 0
	}, "the second transaction neither waited for a lock nor finished")
	if finished {
		t.Errorf("second transaction: finished with %v while the first was still open, want it to wait for %s", secondErr, key)
	} else if onKey == 0 {
		t.Errorf("second transaction: waits for another advisory lock, want it to wait for %s", key)
	}
	letGo()
	firstErr = <-firstDone
	if !finished {
		secondErr = <-secondDone
	}
	return firstErr, secondErr
}
