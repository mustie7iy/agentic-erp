package approvals

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Evidence reads the approval requests about documents, whatever their
// kind, with their decisions, for the audit context's evidence exports
// (T101a): the requests on a journal entry's source document are its
// approval evidence. It reads in the caller's transaction and changes
// nothing.
type Evidence struct{}

// About returns the requests whose object is one of the ids, by their
// creation and then id, each with its decisions in the order they were
// taken.
func (Evidence) About(ctx context.Context, objectIDs []uuid.UUID) ([]Request, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, requestSelect+` WHERE r.object_id = ANY($1) ORDER BY r.created_at, r.id`, objectIDs)
	rs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Request, error) { return scanRequest(r) })
	if err != nil {
		return nil, fmt.Errorf("approvals: requests about %d documents: %w", len(objectIDs), err)
	}
	return rs, nil
}
