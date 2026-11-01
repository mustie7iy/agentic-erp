package configuration_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TestHoldOrdersALoadBeforeAnActivation: a transaction that holds the
// active configuration (Settings.Hold), then takes the partner context's
// payment method lock and later draws a number, as a migration load
// giving suppliers payment methods does, runs to its commit while an
// activation, which locks the ranges and then the method lock, waits for
// it; the activation then completes. Without the hold the two take the
// locks in opposite orders and PostgreSQL aborts one of them (ADR-0008).
func TestHoldOrdersALoadBeforeAnActivation(t *testing.T) {
	e := setup(t, false)
	raised := document(t)
	raised.NumberRanges[0].Last = 199999
	cr := e.propose(t, 1, raised)
	if code, m := e.transition(t, cr, "submit"); code != 200 {
		t.Fatalf("submit: got %d %v, want 200", code, m)
	}
	_, tag := e.Must(200, "GET", "/v1/change-requests/"+cr["id"].(string), "", "")

	var pid int32
	held, release := make(chan struct{}), make(chan struct{})
	letGo := sync.OnceFunc(func() { close(release) })
	defer letGo()
	loadDone, activated := make(chan error, 1), make(chan int, 1)
	go func() {
		err := errors.New("the load did not run")
		defer func() { loadDone <- err }()
		err = apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			tx, _, err := reqctx.Request(ctx)
			if err != nil {
				return err
			}
			if err := (configuration.Settings{}).Hold(ctx); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('partner.payment_method.' || platform.require_tenant()::text, 0))`); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return err
			}
			close(held)
			<-release
			_, err = configuration.Numbers{}.Next(ctx, e.le, "journal_kg", 2026)
			return err
		})
	}()
	select {
	case <-held:
	case err := <-loadDone:
		t.Fatalf("the load: got %v before it held its locks", err)
	}
	go func() {
		activated <- e.API.Do(apitest.Req{Method: http.MethodPost, Path: "/v1/change-requests/" + cr["id"].(string) + ":activate",
			Tenant: e.ten.ID, As: e.ten.Human, IfMatch: tag, Key: apitest.NewKey()}).Code
	}()
	pgtest.Eventually(t, 10*time.Second, func() bool {
		var n int
		pgtest.Scan(t, `SELECT count(*) FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))`, []any{pid}, &n)
		return n > 0
	}, "the activation did not wait for the load")
	letGo()
	if err := <-loadDone; err != nil {
		t.Errorf("the load: got %v, want it committed", err)
	}
	if code := <-activated; code != 200 {
		t.Errorf("the activation: got %d, want 200 after the load", code)
	}
}

// TestHoldFailure: a failed hold is reported, not hidden.
func TestHoldFailure(t *testing.T) {
	e := setup(t, false)
	apitest.EveryFailure(t, e.ten, e.ten.Human, "hold", func(ctx context.Context) error { return configuration.Settings{}.Hold(ctx) })
}
