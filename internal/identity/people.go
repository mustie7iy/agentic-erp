package identity

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Named is a principal as another context shows it beside its id: its
// name, kind (human, agent, service) and status (active, blocked,
// deprovisioned).
type Named struct {
	Name, Kind, Status string
}

// People answers other contexts' questions about principals, in the
// request's transaction: a period close names its tasks' assignees and
// the people who completed them, and assigns tasks to people only
// (ADR-0058 §2, closing.People).
type People struct{}

// Named returns the principals of the tenant with the ids, keyed by id;
// an id the tenant has no principal of is absent.
func (People) Named(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Named, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT id, display_name, kind, status FROM identity.principal WHERE id = ANY($1)`, ids)
	out := map[uuid.UUID]Named{}
	var id uuid.UUID
	var n Named
	if _, err := pgx.ForEachRow(rows, []any{&id, &n.Name, &n.Kind, &n.Status}, func() error { out[id] = n; return nil }); err != nil {
		return nil, fmt.Errorf("identity: names of %d principals: %w", len(ids), err)
	}
	return out, nil
}
