package inventory

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// An outbound delivery's stock (ADR-0083 §5): shipping checks a pick
// against what a storage location holds (Held) and posts the delivery's
// goods issue through IssueForDelivery (S2b1; ADR-0083 §5).

// Held is what a storage location of a plant holds of a material now, in
// its base unit as a plain decimal, "0" when it has never held any. It
// reads the stock as it is, locking nothing: a pick reserves nothing, and
// the goods issue takes the stock under its locks.
func (e *Endpoints) Held(ctx context.Context, material, plant, storageLocation uuid.UUID) (string, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return "", err
	}
	var q string
	err = tx.QueryRow(ctx, `SELECT trim_scale(quantity)::text FROM inventory.stock WHERE material_id = $1 AND plant_id = $2 AND storage_location_id = $3`,
		material, plant, storageLocation).Scan(&q)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "0", nil
	case err != nil:
		return "", fmt.Errorf("inventory: stock of material %s at storage location %s: %w", material, storageLocation, err)
	}
	return q, nil
}

// DeliveryItem is an item of an outbound delivery's goods issue: a
// positive quantity of a material in the order item's unit, out of a
// storage location of a plant.
type DeliveryItem struct {
	MaterialID, PlantID, StorageLocationID uuid.UUID
	Quantity, Unit                         string
}

// IssueForDelivery posts an outbound delivery's goods issue (601) as a
// material document in the request's transaction, its number the
// document's reference: each item checked as an issue's is (an active
// material kept at the storage location of a plant of a company the
// caller may act in, every plant of one company), its quantity converted
// to the base unit exactly, posted on the company's date today, the
// valuations and stock locked and refused below zero, each line valued as
// an issue is (at a standard price the stock left at the price, at a
// moving price the value in proportion), and the value posted in one WA
// entry from the valuation class's stock account to its cost of goods
// sold account (422 inventory.no_inventory_account when it has none). The
// audit record and MaterialDocumentPosted are written in the request's
// transaction. What each line took from the stock is its cost (the line's
// amount, negative).
func (e *Endpoints) IssueForDelivery(ctx context.Context, reference string, in []DeliveryItem) (Document, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Document{}, err
	}
	items := make([]ItemInput, len(in))
	for i, it := range in {
		items[i] = ItemInput{MaterialID: it.MaterialID, PlantID: it.PlantID, StorageLocationID: it.StorageLocationID, Quantity: it.Quantity, Unit: it.Unit}
	}
	r, err := e.inputRefs(ctx, items)
	if err != nil {
		return Document{}, err
	}
	company, lines, err := e.items(items, movementTypes["601"], r)
	if err != nil {
		return Document{}, err
	}
	c, err := e.companyOf(ctx, company)
	if err != nil {
		return Document{}, err
	}
	d, err := e.write(ctx, tx, p, posting{company: c.le, movementType: "601", day: c.day.Date, reference: &reference, lines: lines, items: items}, r)
	if err != nil {
		return Document{}, err
	}
	return d, e.record(ctx, "inventory.material_document.post", d, r, map[string]any{"has_header_text": false})
}
