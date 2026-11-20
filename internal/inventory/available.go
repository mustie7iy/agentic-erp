package inventory

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// PlantStock is what the storage locations of a plant hold now of each of
// the materials, in its base unit as a plain decimal, keyed by material:
// the stock a sales order's availability check promises from (S2j;
// ADR-0082 As built S2j). A material the plant holds none of has no
// entry; stock in transit to the plant (S2h) is not there yet and is not
// counted. It reads the stock as it is, locking nothing: the check
// serializes on its own lock, and a movement it does not wait for only
// changes what the next check finds.
func (e *Endpoints) PlantStock(ctx context.Context, plant uuid.UUID, materials []uuid.UUID) (map[uuid.UUID]string, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	type held struct {
		Material uuid.UUID
		Quantity string
	}
	rows, _ := tx.Query(ctx, `SELECT material_id, trim_scale(sum(quantity))::text FROM inventory.stock
		WHERE plant_id = $1 AND material_id = ANY($2) GROUP BY material_id HAVING sum(quantity) > 0 ORDER BY material_id`, plant, materials)
	stock, err := pgx.CollectRows(rows, pgx.RowToStructByPos[held])
	if err != nil {
		return nil, fmt.Errorf("inventory: stock of %d materials at plant %s: %w", len(materials), plant, err)
	}
	out := make(map[uuid.UUID]string, len(stock))
	for _, s := range stock {
		out[s.Material] = s.Quantity
	}
	return out, nil
}
