package uow

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Consistent reads as of one database state beside a transaction that
// writes: a read-only transaction at REPEATABLE READ in the tenant of the
// caller's principal, as that principal. A write that records what it read
// from several contexts (a snapshot of the liquidity forecast, ADR-0051 §6)
// reads in it and writes in its own transaction at READ COMMITTED: run
// whole at REPEATABLE READ, its write would fail whenever another write of
// the tenant committed during the read (every write moves the tenant's
// audit chain head), and x-snapshot keeps that level for safe requests.
type Consistent struct {
	Begin database.BeginFunc
	Role  string // erp_app: in the tenant's scope
}

// Read begins the consistent read and returns a context that carries its
// transaction, the principal kept, and the transaction, which the caller
// rolls back once it has read (it only reads). It holds a second
// connection until then.
func (c Consistent) Read(ctx context.Context) (context.Context, pgx.Tx, error) {
	_, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, nil, err
	}
	tx, err := c.Begin(ctx, database.Scope{Role: c.Role, TenantID: p.TenantID, ReadOnly: true, Isolation: pgx.RepeatableRead})
	if err != nil {
		return nil, nil, fmt.Errorf("uow: consistent read: %w", err)
	}
	if err := database.SetActor(ctx, tx, p.ID); err != nil {
		// Read only: nothing to keep; the actor's failure is the one reported.
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return nil, nil, err
	}
	return reqctx.WithTx(ctx, tx), tx, nil
}
