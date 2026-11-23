package sales

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Requirement is what a plant's confirmed sales orders ordered of a
// material in one unit and have not delivered: the demand a planning run
// counts (S2k; ADR-0084 §2), as a plain decimal in that unit.
type Requirement struct {
	MaterialID uuid.UUID
	Unit       string
	Quantity   string
}

// PlantRequirements is the open demand of the confirmed orders that name
// the plant ($1) for the materials ($2), by material and unit in that
// order: each standing item's ordered quantity less what is delivered of
// it, an item delivered in full counting nothing. It is the ordered
// quantity, not the confirmed one (S2j), so that a planning run procures
// what the availability check could not confirm (SAP's sales
// requirements, from memory). Draft and cancelled orders and removed
// items demand nothing; an order that names no plant is no plant's
// demand. It reads the orders as they are, locking nothing: a run counts
// what was confirmed when it read. Company access is not applied: the
// caller reads a plant of a company it may act in.
func (Orders) PlantRequirements(ctx context.Context, plant uuid.UUID, materials []uuid.UUID) ([]Requirement, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT i.material_id, i.uom_code, trim_scale(sum(i.quantity - i.delivered_quantity))::text
		FROM sales.sales_order_item i JOIN sales.sales_order o ON o.tenant_id = i.tenant_id AND o.id = i.sales_order_id
		WHERE o.plant_id = $1 AND o.status = 'confirmed' AND i.material_id = ANY($2) AND i.removed_in_change IS NULL
			AND i.quantity > i.delivered_quantity
		GROUP BY i.material_id, i.uom_code ORDER BY i.material_id, i.uom_code`, plant, materials)
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Requirement])
	if err != nil {
		return nil, fmt.Errorf("sales: requirements of %d materials at plant %s: %w", len(materials), plant, err)
	}
	return out, nil
}

// DueRequirement is a Requirement due on a day: what the plant's
// confirmed orders due then ordered of a material in one unit and have
// not delivered.
type DueRequirement struct {
	MaterialID uuid.UUID
	Unit       string
	Due        time.Time
	Quantity   string
}

// PlantRequirementsDue is PlantRequirements by the day each order is due,
// the day its customer asked for, else its date, by material, unit and
// day in that order: the open orders S&OP's review buckets by month (S2n;
// ADR-0084 As built S2n), as S2m's agent buckets them by week.
func (Orders) PlantRequirementsDue(ctx context.Context, plant uuid.UUID, materials []uuid.UUID) ([]DueRequirement, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT i.material_id, i.uom_code, coalesce(o.requested_delivery_date, o.order_date) AS due,
			trim_scale(sum(i.quantity - i.delivered_quantity))::text
		FROM sales.sales_order_item i JOIN sales.sales_order o ON o.tenant_id = i.tenant_id AND o.id = i.sales_order_id
		WHERE o.plant_id = $1 AND o.status = 'confirmed' AND i.material_id = ANY($2) AND i.removed_in_change IS NULL
			AND i.quantity > i.delivered_quantity
		GROUP BY i.material_id, i.uom_code, due ORDER BY i.material_id, i.uom_code, due`, plant, materials)
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[DueRequirement])
	if err != nil {
		return nil, fmt.Errorf("sales: requirements due of %d materials at plant %s: %w", len(materials), plant, err)
	}
	return out, nil
}
