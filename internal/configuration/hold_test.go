package configuration_test

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

// holding is a transaction that holds the active configuration
// (Settings.Hold) and then takes the partner context's payment method
// lock, as a migration load giving suppliers payment methods does, until
// it is released; then it draws a number and commits. It runs in its own
// goroutine and reports every failure on done, never through the test.
type holding struct {
	pid           int32
	held, release chan struct{}
	done          chan error
	letGo         func()
}

func hold(t *testing.T, e env) *holding {
	t.Helper()
	h := &holding{held: make(chan struct{}), release: make(chan struct{}), done: make(chan error, 1)}
	h.letGo = sync.OnceFunc(func() { close(h.release) })
	t.Cleanup(h.letGo)
	pool := pgtest.Pool(t)
	go func() { h.done <- h.run(pool, e) }()
	select {
	case <-h.held:
	case err := <-h.done:
		t.Fatalf("the holding transaction: got %v before it held its locks", err)
	}
	return h
}

func (h *holding) run(pool *database.Pool, e env) error {
	ctx := context.Background()
	tx, err := pool.Begin(ctx, database.Scope{Role: "erp_app", TenantID: e.ten.ID})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	err = uow.Act(ctx, tx, reqctx.Principal{ID: e.ten.Human.ID, Kind: e.ten.Human.Kind, TenantID: e.ten.ID}, nil, func(ctx context.Context) error {
		if err := (configuration.Settings{}).Hold(ctx); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('partner.payment_method.' || platform.require_tenant()::text, 0))`); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&h.pid); err != nil {
			return err
		}
		close(h.held)
		<-h.release
		_, err := configuration.Numbers{}.Next(ctx, e.le, "journal_kg", 2026)
		return err
	})
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// blocking is whether a session waits for the holding transaction.
func (h *holding) blocking(t *testing.T) bool {
	t.Helper()
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))`, []any{h.pid}, &n)
	return n > 0
}

// TestHoldOrdersALoadBeforeAnActivation: while a transaction holds the
// active configuration and the payment method lock, an activation, which
// locks the ranges and then the method lock, waits for it; the holder
// draws its number and commits, and the activation then completes.
// Without the hold the two take the locks in opposite orders and
// PostgreSQL aborts one of them (ADR-0008).
func TestHoldOrdersALoadBeforeAnActivation(t *testing.T) {
	e := setup(t, false)
	raised := document(t)
	raised.NumberRanges[0].Last = 199999
	cr := e.propose(t, 1, raised)
	if code, m := e.transition(t, cr, "submit"); code != 200 {
		t.Fatalf("submit: got %d %v, want 200", code, m)
	}
	id := cr["id"].(string)
	_, tag := e.Must(200, "GET", "/v1/change-requests/"+id, "", "")
	h := hold(t, e)
	activated := make(chan int, 1)
	go func() {
		activated <- e.API.Do(apitest.Req{Method: http.MethodPost, Path: "/v1/change-requests/" + id + ":activate",
			Tenant: e.ten.ID, As: e.ten.Human, IfMatch: tag, Key: apitest.NewKey()}).Code
	}()
	// Within the requests' lock timeout, after which the activation is refused.
	pgtest.Eventually(t, 4*time.Second, func() bool { return h.blocking(t) }, "the activation did not wait for the holding transaction")
	h.letGo()
	if err := <-h.done; err != nil {
		t.Errorf("the holding transaction: got %v, want it committed", err)
	}
	if code := <-activated; code != 200 {
		t.Errorf("the activation: got %d, want 200 after the holding transaction", code)
	}
}

// TestHoldLetsChangeRequestsBeDrafted: holding the configuration holds
// back only its activation; a change request is drafted meanwhile.
func TestHoldLetsChangeRequestsBeDrafted(t *testing.T) {
	e := setup(t, false)
	h := hold(t, e)
	code, m, _ := e.Do("POST", "/v1/change-requests", fmt.Sprintf(`{"title": "Change", "rationale": "Because", "base_version": 1, "proposed": %s}`,
		jsonOf(t, document(t))), "")
	if code != 201 {
		t.Errorf("draft a change request while the configuration is held: got %d %v, want 201", code, m)
	}
	h.letGo()
	if err := <-h.done; err != nil {
		t.Errorf("the holding transaction: got %v, want it committed", err)
	}
}

// TestHoldFailure: a failed hold is reported, not hidden, and a hold
// outside a request is refused.
func TestHoldFailure(t *testing.T) {
	e := setup(t, false)
	apitest.EveryFailure(t, e.ten, e.ten.Human, "hold", func(ctx context.Context) error { return configuration.Settings{}.Hold(ctx) })
	if err := (configuration.Settings{}).Hold(context.Background()); err == nil {
		t.Error("hold outside a request: got no error, want one")
	}
}
