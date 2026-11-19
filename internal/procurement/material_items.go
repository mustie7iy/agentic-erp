package procurement

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Purchasing items by material (S1f2; ADR-0082 As built S1f): a
// requisition's item, an RFQ's item and a catalog item may name a
// material of the tenant, checked as a sales item's is
// (materials.Material.ForItem): an RFQ item's material key and a catalog
// item's key are the material's number, an item's description is the
// material's unless it gives one, and its unit is one of the material's,
// a quantity converting to its base unit exactly. The material is
// carried from a catalog item to the requisition item taken from it, from
// a requisition item to the RFQ item that asks for it and from an RFQ item
// to its award's contract item.

// materialFields are the fields a material item's refusals name: its
// material, its key ("" for an item without one, a requisition's) and its
// unit.
type materialFields struct{ material, key, unit string }

// itemFields are the fields of item i of a document's items, whose key
// is keyField.
func itemFields(field, keyField string) materialFields {
	key := ""
	if keyField != "" {
		key = field + "." + keyField
	}
	return materialFields{material: field + ".material_id", key: key, unit: field + ".quantity.unit"}
}

// materialsOf reads the materials the ids name; a nil id names none.
func (e *Endpoints) materialsOf(ctx context.Context, ids []*uuid.UUID) (map[uuid.UUID]materials.Material, error) {
	var named []uuid.UUID
	for _, id := range ids {
		if id != nil {
			named = append(named, *id)
		}
	}
	ms, err := e.Materials.Materials(ctx, named)
	if err != nil {
		return nil, fmt.Errorf("procurement: materials of the items: %w", err)
	}
	return ms, nil
}

// inputMaterials reads the materials the items name.
func (e *Endpoints) inputMaterials(ctx context.Context, in []ItemInput) (map[uuid.UUID]materials.Material, error) {
	ids := make([]*uuid.UUID, len(in))
	for i, it := range in {
		ids[i] = it.MaterialID
	}
	return e.materialsOf(ctx, ids)
}

// materialText is what an item that names a material is known by
// (materials.Material.ForItem), checked as a new item: a material of the
// tenant (422 request.reference_not_found), active (422
// procurement.material_not_usable), the key its number (422
// procurement.key_not_material for another), and a unit of it into which
// a quantity converts exactly (422 procurement.invalid_material_quantity).
// ok is false for an item that names no material.
func materialText(ms map[uuid.UUID]materials.Material, f materialFields, id *uuid.UUID, key *string, description, quantity, unit string) (materials.ItemText, bool, error) {
	if id == nil {
		return materials.ItemText{}, false, nil
	}
	m, ok := ms[*id]
	if !ok {
		return materials.ItemText{}, false, problem.ReferenceNotFound(f.material, "material", "no material with this id")
	}
	text, err := m.ForItem(key, description, quantity, unit, true)
	switch {
	case errors.Is(err, materials.ErrNotUsable):
		return materials.ItemText{}, false, problem.Invalid("procurement.material_not_usable", f.material, "Material "+m.Number+" is "+m.Status+
			"; a new item is of an active material.")
	case errors.Is(err, materials.ErrOtherKey):
		return materials.ItemText{}, false, problem.Invalid("procurement.key_not_material", f.key, "An item of material "+m.Number+
			" has its number as its key; send that or none.")
	case err != nil:
		return materials.ItemText{}, false, problem.Invalid("procurement.invalid_material_quantity", f.unit, m.UnitRefusal(quantity, unit, err))
	}
	return text, true, nil
}

// materialItem completes item i of a requisition or an RFQ from the
// material it names (materialText): its description and its key, the
// material's number, which a requisition's item keeps beside the
// material and an RFQ's sends as material_key (keyField; "" for a
// requisition's, which sends none). An item that names no material is
// returned as it is.
func materialItem(ms map[uuid.UUID]materials.Material, i int, keyField string, it ItemInput) (ItemInput, error) {
	var key *string
	if keyField != "" && it.MaterialKey != "" {
		key = &it.MaterialKey
	}
	text, ok, err := materialText(ms, itemFields(fmt.Sprintf("items[%d]", i), keyField), it.MaterialID, key, it.Description, it.Quantity, it.Unit)
	if err != nil || !ok {
		return it, err
	}
	it.Description, it.MaterialKey = text.Description, text.Key
	return it, nil
}

// materialItems completes the items that name a material (materialItem).
func (e *Endpoints) materialItems(ctx context.Context, in []ItemInput, keyField string) ([]ItemInput, error) {
	ms, err := e.inputMaterials(ctx, in)
	if err != nil {
		return nil, err
	}
	out := make([]ItemInput, len(in))
	for i, it := range in {
		if out[i], err = materialItem(ms, i, keyField, it); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// catalogMaterial completes a catalog item that names a material from it
// (materialText): its key the material's number, its description the
// material's unless it gives one, its unit one of the material's. An item
// that names no material is returned as it is.
func (e *Endpoints) catalogMaterial(ctx context.Context, in NewCatalogItem) (NewCatalogItem, error) {
	ms, err := e.materialsOf(ctx, []*uuid.UUID{in.MaterialID})
	if err != nil {
		return NewCatalogItem{}, err
	}
	var key *string
	if in.ItemKey != "" {
		key = &in.ItemKey
	}
	text, ok, err := materialText(ms, materialFields{material: "material_id", key: "item_key", unit: "unit"}, in.MaterialID, key, in.Description, "", in.Unit)
	if err != nil || !ok {
		return in, err
	}
	in.ItemKey, in.Description = text.Key, text.Description
	return in, nil
}
