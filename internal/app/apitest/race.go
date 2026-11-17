package apitest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

// Race runs two requests of the tenant that overlap, as a write-skew test
// needs: first runs in a transaction left open; second then runs in
// another, as b, while the first is open. Once the second waits for a lock
// or has finished, the first commits (when it succeeded), then the second
// (when it succeeded). It returns both results. A second that finishes
// without waiting did not see the first's work: a test asserting that one
// of the two is refused then fails, as it should without a lock.
func Race(tb testing.TB, ten pgtest.Tenant, a, b pgtest.Actor, first, second func(ctx context.Context) error) (firstErr, secondErr error) {
	tb.Helper()
	ctx := context.Background()
	tx1, err := pool(tb).Begin(ctx, database.Scope{Role: "erp_app", TenantID: ten.ID})
	if err != nil {
		tb.Fatal(err)
	}
	defer tx1.Rollback(ctx)
	firstErr = uow.Act(ctx, tx1, reqctx.Principal{ID: a.ID, Kind: a.Kind, TenantID: ten.ID}, nil, first)
	tx2, err := pool(tb).Begin(ctx, database.Scope{Role: "erp_app", TenantID: ten.ID})
	if err != nil {
		tb.Fatal(err)
	}
	defer tx2.Rollback(ctx)
	var pid int32
	if err := tx2.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		tb.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		err := uow.Act(ctx, tx2, reqctx.Principal{ID: b.ID, Kind: b.Kind, TenantID: ten.ID}, nil, second)
		if err == nil {
			if err = tx2.Commit(ctx); err != nil {
				err = fmt.Errorf("apitest: commit the second request: %w", err)
			}
		}
		done <- err
	}()
	var finished bool
	pgtest.Eventually(tb, 30*time.Second, func() bool {
		select {
		case secondErr = <-done:
			finished = true
			return true
		default:
		}
		var waiting bool
		pgtest.Scan(tb, `SELECT EXISTS (SELECT FROM pg_stat_activity WHERE pid = $1 AND wait_event_type = 'Lock')`, []any{pid}, &waiting)
		return waiting
	}, "the second request neither waited for a lock nor finished")
	if firstErr == nil {
		if err := tx1.Commit(ctx); err != nil {
			firstErr = fmt.Errorf("apitest: commit the first request: %w", err)
		}
	}
	if !finished {
		secondErr = <-done
	}
	return firstErr, secondErr
}
