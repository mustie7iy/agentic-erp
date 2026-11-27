package inventory

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Batches in an outbound delivery's goods issue (S2f2; ADR-0083 As built
// S2f2): an item of a batch-managed material names the batch it issues
// from by its number, as a warehouse clerk reads it off the goods, and
// the batch must be one of the material's; it is then checked and moved
// as a person's goods movement's batch is (batchOf, takeBatches), so a
// line takes no batch below zero at the storage location. Its reversal
// (602) gives each line's batch back (reversal). The batch is the
// person's choice: batch determination is Later.

// deliveryBatches gives each item of a goods issue (items[k] is in[k]'s)
// the batch it names, by number, and puts the batches in r.batches. Of a
// movement type that carries batches, an item of a batch-managed material
// names one (422 inventory.batch_required); an item of another material
// names none (422 inventory.batch_not_allowed); the number is one of the
// material's batches (422 inventory.batch_invalid), all naming
// items[k].batch_number. An item whose material is not found is left to
// items to refuse, and of a movement type that carries no batch (643)
// batchOf refuses a batch-managed material.
func deliveryBatches(ctx context.Context, tx pgx.Tx, mt movementType, in []DeliveryItem, items []ItemInput, r *refs) error {
	var names []batchName
	for k, it := range in {
		m, ok := r.materials[it.MaterialID]
		if !ok {
			continue
		}
		at := fmt.Sprintf("items[%d].batch_number", k)
		switch {
		case !m.BatchManaged && it.Batch != "":
			return problem.Invalid("inventory.batch_not_allowed", at, "Material "+m.Number+" is not managed in batches; the item names no batch.")
		case m.BatchManaged && it.Batch == "" && mt.tracked:
			return problem.Invalid("inventory.batch_required", at, "Material "+m.Number+" is managed in batches; the item names the batch it issues from.")
		}
		if n := (batchName{it.MaterialID, it.Batch}); it.Batch != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	r.batches = map[uuid.UUID]Batch{}
	if len(names) == 0 {
		return nil
	}
	slices.SortFunc(names, batchName.compare)
	found, err := batchesNamed(ctx, tx, names)
	if err != nil {
		return err
	}
	for k, it := range in {
		if it.Batch == "" {
			continue
		}
		b, ok := found[batchName{it.MaterialID, it.Batch}]
		if !ok {
			return problem.Invalid("inventory.batch_invalid", fmt.Sprintf("items[%d].batch_number", k), "Material "+r.materials[it.MaterialID].Number+
				" has no batch "+it.Batch+" (GET /v1/batch-stock?material_id="+it.MaterialID.String()+").")
		}
		items[k].BatchID, r.batches[b.ID] = &b.ID, b
	}
	return nil
}
