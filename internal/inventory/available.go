package inventory

import (
	"context"
	"fmt"
	"time"

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

// PlantIssues is what the plant's goods issues of each of the materials
// posted from ($3) to ($4), both days included, took out, in its base
// unit as a plain decimal, keyed by material: its issues to a cost center
// (201), a delivery's (601) and an intercompany one (643), less their
// reversals (202, 602), so negative when the reversals are more. A
// material with none has no entry. Lines in transit are not counted;
// scrapping, transfers, receipts and count differences are no issues
// (SAP's consumption, from memory). It is the demand S2m's agent
// forecasts from (internal/demandagent's issues, which keeps the same
// movement types: a change to one is a change to both), read for S&OP's
// review (S2n; ADR-0084 As built S2n), locking nothing. Company access is not applied: the caller reads a
// plant of a company it may act in.
func (e *Endpoints) PlantIssues(ctx context.Context, plant uuid.UUID, materials []uuid.UUID, from, to time.Time) (map[uuid.UUID]string, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	type issued struct {
		Material uuid.UUID
		Quantity string
	}
	rows, _ := tx.Query(ctx, `SELECT m.material_id, trim_scale(-sum(m.quantity))::text
		FROM inventory.movement m JOIN inventory.material_document d ON d.tenant_id = m.tenant_id AND d.id = m.material_document_id
		WHERE m.plant_id = $1 AND m.material_id = ANY($2) AND NOT m.in_transit AND d.movement_type IN ('201', '202', '601', '602', '643')
			AND d.posting_date BETWEEN $3 AND $4
		GROUP BY m.material_id ORDER BY m.material_id`, plant, materials, from, to)
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[issued])
	if err != nil {
		return nil, fmt.Errorf("inventory: goods issues of %d materials at plant %s: %w", len(materials), plant, err)
	}
	out := make(map[uuid.UUID]string, len(got))
	for _, g := range got {
		out[g.Material] = g.Quantity
	}
	return out, nil
}
