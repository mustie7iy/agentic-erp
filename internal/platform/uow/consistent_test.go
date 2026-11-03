package uow_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

// TestConsistentRead: a consistent read runs in a transaction of its own,
// read-only at REPEATABLE READ, in the caller's tenant as the caller, and
// its context carries that transaction; outside a request, or when the
// transaction cannot begin or take the actor, it fails.
func TestConsistentRead(t *testing.T) {
	pool := pgtest.Pool(t)
	ten := pgtest.NewTenant(t)
	ctx := context.Background()
	outer, err := pool.Begin(ctx, database.Scope{Role: "erp_app", TenantID: ten.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer outer.Rollback(ctx)
	in := reqctx.WithPrincipal(reqctx.WithTx(ctx, outer), reqctx.Principal{ID: ten.Human.ID, Kind: reqctx.KindHuman, TenantID: ten.ID})
	c := uow.Consistent{Begin: pool.Begin, Role: "erp_app"}
	read, tx, err := c.Read(in)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if tx == outer {
		t.Error("the read runs in the request's transaction, want one of its own")
	}
	if got, _, err := reqctx.Request(read); err != nil || got != tx {
		t.Errorf("the context's transaction: got %v %v, want the read's", got, err)
	}
	var isolation, readOnly, tenant, actor string
	if err := tx.QueryRow(ctx, `SELECT current_setting('transaction_isolation'), current_setting('transaction_read_only'),
		platform.current_tenant()::text, current_setting('app.actor_id')`).Scan(&isolation, &readOnly, &tenant, &actor); err != nil {
		t.Fatal(err)
	}
	for what, c := range map[string][2]string{"isolation": {isolation, "repeatable read"}, "read only": {readOnly, "on"},
		"tenant": {tenant, ten.ID.String()}, "actor": {actor, ten.Human.ID.String()}} {
		if c[0] != c[1] {
			t.Errorf("%s: got %s, want %s", what, c[0], c[1])
		}
	}
	if _, _, err := c.Read(ctx); !errors.Is(err, reqctx.ErrOutsideRequest) {
		t.Errorf("outside a request: got %v, want reqctx.ErrOutsideRequest", err)
	}
	down := uow.Consistent{Begin: func(context.Context, database.Scope) (pgx.Tx, error) { return nil, errors.New("database down") }, Role: "erp_app"}
	if _, _, err := down.Read(in); err == nil {
		t.Error("a transaction that does not begin: got no error, want it reported")
	}
	failing := uow.Consistent{Role: "erp_app", Begin: func(ctx context.Context, s database.Scope) (pgx.Tx, error) {
		tx, err := pool.Begin(ctx, s)
		return &pgtest.FailingTx{Tx: tx, FailAt: 1}, err
	}}
	if _, _, err := failing.Read(in); !errors.Is(err, pgtest.ErrInjected) {
		t.Errorf("the actor not set: got %v, want the injected failure", err)
	}
}
