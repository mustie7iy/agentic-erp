package apitest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// errSetup is what Serialized reports for a transaction whose setup
// failed: Committed then fails the test and ends its goroutine, which
// still has to answer, or Serialized would wait for it forever.
var errSetup = errors.New("apitest: transaction setup failed")

// Serialized runs first as actor a in a committed transaction and holds it
// open while second runs in another; it lets first commit once second
// waits for a lock first holds (first's backend is among
// pg_blocking_pids of second's), and returns both results. Second
// finishing while first is still open fails the test: the two did not
// serialize. The interleaving is deterministic, with no sleeps: second
// starts only after first has done its work, and first commits only once
// second is seen waiting.
func Serialized(tb testing.TB, ten pgtest.Tenant, a pgtest.Actor, first, second func(ctx context.Context) error) (firstErr, secondErr error) {
	tb.Helper()
	held, release := make(chan struct{}), make(chan struct{})
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	// Deferred too, so that a failed wait does not leave first holding its transaction.
	letGo := sync.OnceFunc(func() { close(release) })
	defer letGo()
	var firstPID, secondPID atomic.Int32
	go func() {
		err := errSetup
		defer func() { firstDone <- err }()
		err = Committed(tb, ten, a, func(ctx context.Context) error {
			if err := backend(ctx, &firstPID); err != nil {
				return err
			}
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
		err = Committed(tb, ten, a, func(ctx context.Context) error {
			if err := backend(ctx, &secondPID); err != nil {
				return err
			}
			return second(ctx)
		})
	}()
	finished := false
	pgtest.Eventually(tb, 10*time.Second, func() bool {
		select {
		case secondErr = <-secondDone:
			finished = true
			return true
		default:
		}
		if secondPID.Load() == 0 {
			return false
		}
		return Blocks(tb, firstPID.Load(), secondPID.Load())
	}, "the second transaction neither waited for the first nor finished")
	if finished {
		tb.Errorf("second transaction: finished with %v while the first was still open, want it to wait for the first's lock", secondErr)
	}
	letGo()
	firstErr = <-firstDone
	if !finished {
		secondErr = <-secondDone
	}
	return firstErr, secondErr
}

// Started runs work as actor a in a committed unit of work in the
// background. It sends the transaction's backend pid before work starts,
// and work's result once the transaction ends: a test that drives an
// interleaving waits for the pid to block (WaitBlocked) before it starts
// the next transaction.
func Started(tb testing.TB, ten pgtest.Tenant, a pgtest.Actor, work func(ctx context.Context) error) (<-chan int32, <-chan error) {
	tb.Helper()
	pid, done := make(chan int32, 1), make(chan error, 1)
	go func() {
		err := errSetup
		defer func() { done <- err }()
		err = Committed(tb, ten, a, func(ctx context.Context) error {
			var n atomic.Int32
			if err := backend(ctx, &n); err != nil {
				return err
			}
			pid <- n.Load()
			return work(ctx)
		})
	}()
	return pid, done
}

// WaitBlocked waits until backend pid waits for a lock blocker holds; the
// transaction must not finish before (done, as Started returns it).
func WaitBlocked(tb testing.TB, pid, blocker int32, done <-chan error, what string) {
	tb.Helper()
	pgtest.Eventually(tb, 10*time.Second, func() bool {
		select {
		case err := <-done:
			tb.Fatalf("%s finished with %v, want it waiting for backend %d", what, err, blocker)
		default:
		}
		return Blocks(tb, blocker, pid)
	}, "%s neither waited for backend %d nor finished", what, blocker)
}

// Blocks reports whether backend pid waits for a lock blocker holds.
func Blocks(tb testing.TB, blocker, pid int32) bool {
	tb.Helper()
	var waits bool
	pgtest.Scan(tb, `SELECT $1::int = ANY(pg_blocking_pids($2::int))`, []any{blocker, pid}, &waits)
	return waits
}

// backend stores the backend of the request's transaction in pid.
func backend(ctx context.Context, pid *atomic.Int32) error {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	var n int32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&n); err != nil {
		return fmt.Errorf("apitest: read the backend: %w", err)
	}
	pid.Store(n)
	return nil
}
