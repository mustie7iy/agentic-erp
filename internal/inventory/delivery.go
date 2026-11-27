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
// goods issue through IssueForDelivery (S2b1; ADR-0083 §5), naming the
// batches and serial numbers it issues (S2f2, S2g2; ADR-0083 As built
// S2f2), and its reversal through ReverseForDelivery (S2b3), which gives
// them back.

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
// storage location of a plant; of a batch-managed material, the number of
// the batch it issues from, and of a serial-managed one, the serial
// number of each unit (S2f2, S2g2).
type DeliveryItem struct {
	MaterialID, PlantID, StorageLocationID uuid.UUID
	Quantity, Unit                         string
	Batch                                  string
	SerialNumbers                          []string
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
// amount, negative). An item of a batch-managed material names its batch
// by number and one of a serial-managed material its units' serial
// numbers, each in stock at the location (deliveryBatches, takeBatches,
// takeSerials; S2f2, S2g2).
func (e *Endpoints) IssueForDelivery(ctx context.Context, reference string, in []DeliveryItem) (Document, error) {
	return e.issueAgainstCost(ctx, "601", reference, in)
}

// IssueForIntercompany posts an intercompany stock transport order's goods
// issue (643, S2i1; SAP's goods issue for a cross-company stock transport
// order, from memory) as a material document in the request's
// transaction, its reference the order's number: each item out of a
// storage location of the supplying plant, checked, valued and posted as
// IssueForDelivery's are, against the valuation class's cost of goods
// sold account, in the supplying company. The stock leaves the supplying
// company; it enters the receiving company's only with its goods receipt
// (a purchase order's 101), so in between it is in no company's stock or
// valuation (transit valuation is Later, ADR-0082 As built S2i). The 643
// carries no batch or serial number yet: it refuses a batch-managed or
// serial-managed material, which procurement's order refuses first.
func (e *Endpoints) IssueForIntercompany(ctx context.Context, reference string, in []DeliveryItem) (Document, error) {
	return e.issueAgainstCost(ctx, "643", reference, in)
}

// issueAgainstCost posts a goods issue of movement type mt against cost
// of goods sold, its reference the document it issues for.
func (e *Endpoints) issueAgainstCost(ctx context.Context, mt, reference string, in []DeliveryItem) (Document, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Document{}, err
	}
	items := make([]ItemInput, len(in))
	for i, it := range in {
		items[i] = ItemInput{MaterialID: it.MaterialID, PlantID: it.PlantID, StorageLocationID: it.StorageLocationID, Quantity: it.Quantity, Unit: it.Unit,
			SerialNumbers: it.SerialNumbers}
	}
	r, err := e.inputRefs(ctx, items)
	if err != nil {
		return Document{}, err
	}
	if err := deliveryBatches(ctx, tx, movementTypes[mt], in, items, &r); err != nil {
		return Document{}, err
	}
	company, lines, err := e.items(items, movementTypes[mt], r)
	if err != nil {
		return Document{}, err
	}
	c, err := e.companyOf(ctx, company)
	if err != nil {
		return Document{}, err
	}
	d, err := e.write(ctx, tx, p, posting{company: c.le, movementType: mt, day: c.day.Date, reference: &reference, lines: lines, items: items}, r)
	if err != nil {
		return Document{}, err
	}
	return d, e.record(ctx, "inventory.material_document.post", d, r, map[string]any{"has_header_text": false})
}
