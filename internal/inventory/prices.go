package inventory

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Price is a material's valuation price at a plant: in the company's
// currency, its valuation's (ADR-0082 §8), per PriceUnit of its base unit.
type Price struct {
	Price     money.Minor
	PriceUnit int32
}

// PlantPrices is the valuation price of each of the materials at the
// plant (SAP MBEW's standard or moving price), keyed by material: the
// price a planning run's proposed requisition is estimated at (S2k;
// ADR-0084 §2). A material not valued there has no entry. It reads the
// prices as they are, locking nothing.
func (e *Endpoints) PlantPrices(ctx context.Context, plant uuid.UUID, materials []uuid.UUID) (map[uuid.UUID]Price, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	type priced struct {
		Material uuid.UUID
		Price
	}
	rows, _ := tx.Query(ctx, `SELECT material_id, price, price_unit FROM inventory.valuation
		WHERE plant_id = $1 AND material_id = ANY($2) ORDER BY material_id`, plant, materials)
	prices, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (priced, error) {
		var p priced
		return p, r.Scan(&p.Material, &p.Price.Price, &p.PriceUnit)
	})
	if err != nil {
		return nil, fmt.Errorf("inventory: prices of %d materials at plant %s: %w", len(materials), plant, err)
	}
	out := make(map[uuid.UUID]Price, len(prices))
	for _, p := range prices {
		out[p.Material] = p.Price
	}
	return out, nil
}
