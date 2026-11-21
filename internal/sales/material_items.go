package sales

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Items by material (S1f1; ADR-0082 §2-§3 and As built S1f): a
// quotation's or an order's item may name a material of the tenant
// instead of a free-text key alone. Its item key is then the material's
// number, so the price records, the pricing procedure's discounts, the
// revenue obligations and the analytics find it by key as they find any
// item (SAP names an item's material by its number, MATNR, from memory);
// its description is the material's unless the item gives its own (SAP's
// item text); and its quantity is in a unit of the material that
// converts to its base unit exactly. The converted quantity is not
// stored: it is taken where stock moves, as a stock receipt takes it
// (S1e).

// Materials reads materials by id (the materials context's published
// API, ADR-0082 §2).
type Materials interface {
	Materials(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]materials.Material, error)
}

// itemMaterials reads the materials the items name.
func (e *Endpoints) itemMaterials(ctx context.Context, in []ItemInput) (map[uuid.UUID]materials.Material, error) {
	var ids []uuid.UUID
	for _, it := range in {
		if it.MaterialID != nil {
			ids = append(ids, *it.MaterialID)
		}
	}
	ms, err := e.Materials.Materials(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("sales: materials of the items: %w", err)
	}
	return ms, nil
}

// material completes an item that names a material from it, before the
// item is checked as any other (materials.Material.ForItem): a material
// of the tenant (422 request.reference_not_found naming
// items[i].material_id), active when the item is new (422
// sales.material_not_usable; an item an order change keeps keeps its
// material, whatever its status since), not managed in batches when the
// item is new (422 sales.material_not_usable: a delivery carries no
// batch yet, ADR-0082 As built S2f), the item key its number (422
// sales.item_key_not_material for another), the description its own when
// the item gives none, and the quantity in one of its units that
// converts exactly to its base unit (422 sales.invalid_material_quantity
// naming items[i].quantity.unit). An item that names no material is
// returned as it is.
func (c itemCheck) material(field string, it ItemInput) (ItemInput, error) {
	if it.MaterialID == nil {
		return it, nil
	}
	m, ok := c.materials[*it.MaterialID]
	if !ok {
		return ItemInput{}, problem.ReferenceNotFound(field+".material_id", "material", "no material with this id")
	}
	if m.BatchManaged && !it.Kept {
		return ItemInput{}, problem.Invalid("sales.material_not_usable", field+".material_id", "Material "+m.Number+
			" is managed in batches; a delivery carries no batch yet, so it cannot be sold yet.")
	}
	text, err := m.ForItem(it.ItemKey, it.Description, it.Quantity, it.Unit, !it.Kept)
	switch {
	case errors.Is(err, materials.ErrNotUsable):
		return ItemInput{}, problem.Invalid("sales.material_not_usable", field+".material_id", "Material "+m.Number+" is "+m.Status+
			"; a new item is of an active material.")
	case errors.Is(err, materials.ErrOtherKey):
		return ItemInput{}, problem.Invalid("sales.item_key_not_material", field+".item_key", "An item of material "+m.Number+
			" has its number as its item key; send that or none.")
	case err != nil:
		return ItemInput{}, problem.Invalid("sales.invalid_material_quantity", field+".quantity.unit", m.UnitRefusal(it.Quantity, it.Unit, err))
	}
	it.ItemKey, it.Description = &text.Key, text.Description
	return it, nil
}
