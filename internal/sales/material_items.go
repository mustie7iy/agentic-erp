package sales

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/money"
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

// itemMaterials reads the materials the items name, once each; none when
// no item names one.
func (e *Endpoints) itemMaterials(ctx context.Context, in []ItemInput) (map[uuid.UUID]materials.Material, error) {
	var ids []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for _, it := range in {
		if it.MaterialID != nil && !seen[*it.MaterialID] {
			seen[*it.MaterialID] = true
			ids = append(ids, *it.MaterialID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	ms, err := e.Materials.Materials(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("sales: materials of the items: %w", err)
	}
	return ms, nil
}

// material completes an item that names a material from it, before the
// item is checked as any other: a material of the tenant (422
// request.reference_not_found naming items[i].material_id), active when
// the item is new (422 sales.material_not_usable; an item an order change
// keeps keeps its material, whatever its status since), the item key its
// number (422 sales.item_key_not_material for another), the description
// its own when the item gives none, and the quantity in one of its units
// that converts exactly to its base unit (422
// sales.invalid_material_quantity naming items[i].quantity.unit). An
// item that names no material is returned as it is.
func (c itemCheck) material(field string, it ItemInput) (ItemInput, error) {
	if it.MaterialID == nil {
		return it, nil
	}
	m, ok := c.materials[*it.MaterialID]
	if !ok {
		return ItemInput{}, problem.ReferenceNotFound(field+".material_id", "material", "no material with this id")
	}
	if !it.Kept && m.Status != materials.Active {
		return ItemInput{}, problem.Invalid("sales.material_not_usable", field+".material_id", "Material "+m.Number+" is "+m.Status+
			"; a new item is of an active material.")
	}
	if it.ItemKey != nil && *it.ItemKey != m.Number {
		return ItemInput{}, problem.Invalid("sales.item_key_not_material", field+".item_key", "An item of material "+m.Number+
			" has its number as its item key; send that or none.")
	}
	number := m.Number
	it.ItemKey = &number
	if it.Description == "" {
		it.Description = m.Description
	}
	if !validQuantity(it.Quantity) {
		return it, nil // refused as any item's quantity, by itemCheck.item
	}
	if _, err := m.Convert(money.ParseRat(it.Quantity), it.Unit); err != nil {
		why := " is no unit of material " + m.Number + " (its base unit " + m.BaseUnit + " or an alternative unit)"
		if errors.Is(err, materials.ErrInexact) {
			why = " does not convert exactly to material " + m.Number + "'s base unit " + m.BaseUnit + " in six decimals"
		}
		return ItemInput{}, problem.Invalid("sales.invalid_material_quantity", field+".quantity.unit", it.Quantity+" "+it.Unit+why+".")
	}
	return it, nil
}

// validQuantity says whether a quantity is a plain decimal more than zero
// that can be converted; any other is refused by itemCheck.item.
func validQuantity(q string) bool { return money.PositiveQuantity(q) == nil }
