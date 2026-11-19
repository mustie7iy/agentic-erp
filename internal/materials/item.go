package materials

import (
	"errors"
	"fmt"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// Items by material (S1f; ADR-0082 As built S1f): an item of a sales or
// purchasing document, or a catalog item, that names a material takes
// its number as its key and, when it gives none, its description, and is
// in one of its units, converting to its base unit exactly. Each caller
// maps the refusals below to its own problem codes and fields.

// ErrNotUsable refuses a material that is not active on a new item.
var ErrNotUsable = errors.New("not active")

// ErrOtherKey refuses an item that names a material and another key than
// the material's number.
var ErrOtherKey = errors.New("its number is the item's key")

// ItemText is what an item naming the material is known by: the
// material's number as its key, and its own description, else the
// material's.
type ItemText struct{ Key, Description string }

// ForItem checks an item that names the material: active when added (an
// item a change keeps keeps its material, whatever its status since,
// ErrNotUsable otherwise), keyed by the material's number or by nothing
// (ErrOtherKey), and in one of its units (ErrUnknownUnit): a quantity
// given as a positive plain decimal must convert to the base unit
// exactly (ErrInexact). A quantity that is no such decimal is left to the
// caller's own check of it; an empty one checks the unit alone (a catalog
// item has no quantity). It returns the item's key and description.
func (m Material) ForItem(key *string, description, quantity, unit string, added bool) (ItemText, error) {
	if added && m.Status != Active {
		return ItemText{}, fmt.Errorf("materials: material %s is %s: %w", m.Number, m.Status, ErrNotUsable)
	}
	if key != nil && *key != m.Number {
		return ItemText{}, fmt.Errorf("materials: key %s of material %s: %w", *key, m.Number, ErrOtherKey)
	}
	if description == "" {
		description = m.Description
	}
	var err error
	switch {
	case quantity == "":
		_, err = m.Factor(unit)
	case money.PositiveQuantity(quantity) == nil:
		_, err = m.Convert(money.ParseRat(quantity), unit)
	}
	if err != nil {
		return ItemText{}, err
	}
	return ItemText{Key: m.Number, Description: description}, nil
}

// UnitRefusal says, for a refusal by ForItem of the quantity or unit,
// why: the unit is none of the material's, or the quantity does not
// convert to its base unit exactly.
func (m Material) UnitRefusal(quantity, unit string, err error) string {
	what := unit
	if quantity != "" {
		what = quantity + " " + unit
	}
	if errors.Is(err, ErrInexact) {
		return what + " does not convert exactly to material " + m.Number + "'s base unit " + m.BaseUnit + " in six decimals."
	}
	return what + " is no unit of material " + m.Number + " (its base unit " + m.BaseUnit + " or an alternative unit)."
}
