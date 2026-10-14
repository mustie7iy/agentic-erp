package uow_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

// actIn runs uow.Act in a transaction of the tenant, through a FailingTx
// failing at failAt (0: none), and commits when it succeeds.
func actIn(t *testing.T, pool *database.Pool, ten pgtest.Tenant, failAt int, now func() time.Time, fn func(ctx context.Context) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx, database.Scope{Role: "erp_app", TenantID: ten.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	ftx := &pgtest.FailingTx{Tx: tx, FailAt: failAt}
	actor := reqctx.Principal{ID: ten.Human.ID, Kind: ten.Human.Kind, TenantID: ten.ID}
	if err := uow.Act(ctx, ftx, actor, now, fn); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// audited is the number of audit entries of the tenant with the action.
func audited(t *testing.T, ten pgtest.Tenant, action string) int {
	t.Helper()
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = $2`, []any{ten.ID, action}, &n)
	return n
}

// TestAct: fn runs as the actor with the transaction, principal and an
// audit buffer in its context, and its audit entries are written stamped by
// the clock; a failing fn writes none.
func TestAct(t *testing.T) {
	at := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	pool := pgtest.Pool(t)
	t.Run("runs as the actor and writes the audit entries", func(t *testing.T) {
		ten := pgtest.NewTenant(t)
		id := uuid.Must(uuid.NewV7())
		err := actIn(t, pool, ten, 0, func() time.Time { return at }, func(ctx context.Context) error {
			tx, ok := reqctx.TxFrom(ctx)
			if !ok {
				t.Error("got no transaction in the context, want one")
				return nil
			}
			if p, _ := reqctx.PrincipalFrom(ctx); p.ID != ten.Human.ID {
				t.Errorf("principal: got %v, want %v", p.ID, ten.Human.ID)
			}
			var actor string
			if err := tx.QueryRow(ctx, `SELECT current_setting('app.actor_id')`).Scan(&actor); err != nil {
				return err
			}
			if actor != ten.Human.ID.String() {
				t.Errorf("actor: got %s, want %s", actor, ten.Human.ID)
			}
			return audit.Add(ctx, audit.Entry{Action: "test.act", ObjectType: "test", ObjectID: id, Outcome: "succeeded"})
		})
		if err != nil {
			t.Fatal(err)
		}
		var occurred time.Time
		pgtest.Scan(t, `SELECT occurred_at FROM audit.audit_event WHERE tenant_id = $1 AND action = 'test.act'`, []any{ten.ID}, &occurred)
		if !occurred.Equal(at) {
			t.Errorf("occurred_at: got %v, want %v", occurred, at)
		}
	})
	t.Run("a failing step writes no audit entry", func(t *testing.T) {
		ten := pgtest.NewTenant(t)
		boom := errors.New("boom")
		err := actIn(t, pool, ten, 0, nil, func(ctx context.Context) error {
			if err := audit.Add(ctx, audit.Entry{Action: "test.act", ObjectType: "test", Outcome: "succeeded"}); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Errorf("got %v, want %v", err, boom)
		}
		if n := audited(t, ten, "test.act"); n != 0 {
			t.Errorf("got %d audit entries, want 0", n)
		}
	})
}

// TestActDatabaseFailures: a failure of any statement Act issues (the
// actor, the audit write) is returned and nothing is committed.
func TestActDatabaseFailures(t *testing.T) {
	pool := pgtest.Pool(t)
	ten := pgtest.NewTenant(t)
	step := func(ctx context.Context) error {
		return audit.Add(ctx, audit.Entry{Action: "test.act_failure", ObjectType: "test", Outcome: "succeeded"})
	}
	for failAt := 1; ; failAt++ {
		err := actIn(t, pool, ten, failAt, nil, step)
		if err == nil {
			if failAt == 1 {
				t.Fatal("got success with the first statement failing, want an error")
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: got %v, want the injected failure", failAt, err)
		}
		if n := audited(t, ten, "test.act_failure"); n != 0 {
			t.Fatalf("statement %d: got %d audit entries, want 0", failAt, n)
		}
	}
	if n := audited(t, ten, "test.act_failure"); n != 1 {
		t.Errorf("got %d audit entries, want 1", n)
	}
}
