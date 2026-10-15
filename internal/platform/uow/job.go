package uow

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Job is the transaction of a job without a request, a workflow activity or
// a worker round: begun in Scope, rolled back unless the work succeeds, and
// committed with Name as the context of a failed commit, so the caller's
// message is the same as when it wrote the steps out ("workflows: propose
// payment run <id>: commit: ...").
type Job struct {
	Begin database.BeginFunc
	Scope database.Scope
	Name  string
	Now   func() time.Time // stamps the audit entries of As; nil: the wall clock
}

// Tx runs fn in the job's transaction and commits when fn returns true
// without an error; false (nothing to do) or an error rolls it back. A
// failed begin and fn's error are returned as they are: the caller's own
// steps add their context.
func (j Job) Tx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) (commit bool, err error)) error {
	tx, err := j.Begin(ctx, j.Scope)
	if err != nil {
		return err
	}
	// A no-op after a commit; otherwise the returned error says what failed.
	defer tx.Rollback(context.WithoutCancel(ctx))
	commit, err := fn(ctx, tx)
	if err != nil || !commit {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%s: commit: %w", j.Name, err)
	}
	return nil
}

// As runs fn through Act as the principal actor reads in the job's
// transaction, then commits. actor reporting false means there is nothing
// to do: the transaction is rolled back and As returns nil.
func (j Job) As(ctx context.Context, actor func(ctx context.Context, tx pgx.Tx) (reqctx.Principal, bool, error), fn func(ctx context.Context) error) error {
	return j.Tx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		p, ok, err := actor(ctx, tx)
		if err != nil || !ok {
			return false, err
		}
		return true, Act(ctx, tx, p, j.Now, fn)
	})
}
