package mrp

import (
	"fmt"
	"math/big"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Lot sizing (ADR-0084 As built, lot sizing and period lot sizes; SAP
// MARC-DISLS, BSTFE, MABST, BSTMI, BSTMA and BSTRF, from memory): how a
// run sizes the quantity it proposes for a material's shortage, in its
// base unit; the period procedures size it by period (period.go).

// Lot sizing procedures.
const (
	LotForLot          = "lot_for_lot"          // SAP EX: the shortage itself
	FixedLot           = "fixed"                // SAP FX: lots of the fixed size
	ReplenishToMaximum = "replenish_to_maximum" // SAP HB: up to the maximum stock level
	Daily              = "daily"                // SAP TB: a lot per day of net requirements
	Weekly             = "weekly"               // SAP WB: a lot per ISO week, Monday to Sunday
	Monthly            = "monthly"              // SAP MB: a lot per calendar month
)

// procedures are the lot sizing procedures a plan may name.
var procedures = map[string]bool{LotForLot: true, FixedLot: true, ReplenishToMaximum: true, Daily: true, Weekly: true, Monthly: true}

// periodic reports whether the procedure sizes by period.
func (l LotSizing) periodic() bool {
	return l.Procedure == Daily || l.Procedure == Weekly || l.Procedure == Monthly
}

// MaxLots is the most lots a run proposes of one material: a fixed or
// maximum lot size so small against the requirement that it needs more
// fails the run (409 mrp.too_many_lots) rather than raise a requisition
// of thousands of items.
const MaxLots = 100

// LotSizing is a plan's lot sizing procedure and the quantities it uses,
// plain decimals in the material's base unit, nil when not given: Fixed
// the fixed procedure's lot, MaximumLevel the replenish-to-maximum
// procedure's stock level, and the bounds and rounding value of the
// other two.
type LotSizing struct {
	Procedure                                       string
	Fixed, MaximumLevel, Minimum, Maximum, Rounding *string
}

// lotFields are a lot sizing's quantities by the field the API names
// them with, in the order they are checked.
func (l LotSizing) lotFields() []lotField {
	return []lotField{{"fixed_lot_size", l.Fixed}, {"maximum_stock_level", l.MaximumLevel}, {"minimum_lot_size", l.Minimum},
		{"maximum_lot_size", l.Maximum}, {"rounding_value", l.Rounding}}
}

type lotField struct {
	name  string
	value *string
}

// auditOf is a lot sizing as an audit record holds it: its procedure and
// the quantities given.
func (l LotSizing) auditOf() map[string]any {
	out := map[string]any{"lot_size": l.Procedure}
	for _, f := range l.lotFields() {
		if f.value != nil {
			out[f.name] = *f.value
		}
	}
	return out
}

// same reports whether two lot sizings are one: the same procedure and
// the same quantities given ("40" and "40.0" alike).
func (l LotSizing) same(o LotSizing) bool {
	if l.Procedure != o.Procedure {
		return false
	}
	a, b := l.lotFields(), o.lotFields()
	for i := range a {
		if (a[i].value == nil) != (b[i].value == nil) || a[i].value != nil && !sameQuantity(*a[i].value, *b[i].value) {
			return false
		}
	}
	return true
}

// check checks a lot sizing of a plan with the reorder point: a known
// procedure (422 mrp.invalid_lot_size); its quantities positive decimals
// of a quantity's scale (422 mrp.invalid_quantity); the fixed lot size
// given exactly with the fixed procedure and the maximum stock level,
// not below the reorder point, exactly with replenishing to it; no
// bounds or rounding with a fixed lot; the minimum lot size not above
// the maximum, and both multiples of the rounding value (422
// mrp.invalid_lot_size naming the field).
func (l LotSizing) check(reorderPoint string) error {
	if !procedures[l.Procedure] {
		return problem.Invalid("mrp.invalid_lot_size", "lot_size", "lot_for_lot, fixed, replenish_to_maximum, daily, weekly or monthly")
	}
	for _, f := range l.lotFields() {
		if f.value != nil && money.PositiveQuantity(*f.value) != nil {
			return problem.Invalid("mrp.invalid_quantity", f.name, fmt.Sprintf("a positive plain decimal of at most %d integer digits and %d decimals",
				money.QuantityIntDigits, money.QuantityDecimals))
		}
	}
	return l.consistent(reorderPoint)
}

// consistent checks which quantities the procedure takes and how they
// bound each other.
func (l LotSizing) consistent(reorderPoint string) error {
	invalid := func(field, why string) error { return problem.Invalid("mrp.invalid_lot_size", field, why) }
	switch {
	case (l.Procedure == FixedLot) != (l.Fixed != nil):
		return invalid("fixed_lot_size", "Given with lot_size fixed, and only with it (send null to remove it).")
	case (l.Procedure == ReplenishToMaximum) != (l.MaximumLevel != nil):
		return invalid("maximum_stock_level", "Given with lot_size replenish_to_maximum, and only with it (send null to remove it).")
	case l.MaximumLevel != nil && money.ParseRat(*l.MaximumLevel).Cmp(money.ParseRat(reorderPoint)) < 0:
		return invalid("maximum_stock_level", "Not below the reorder point "+reorderPoint+".")
	}
	if l.Procedure == FixedLot {
		for _, f := range l.lotFields()[2:] {
			if f.value != nil {
				return invalid(f.name, "A fixed lot is not bounded or rounded: not given with lot_size fixed (send null to remove it).")
			}
		}
		return nil
	}
	if l.Minimum != nil && l.Maximum != nil && money.ParseRat(*l.Minimum).Cmp(money.ParseRat(*l.Maximum)) > 0 {
		return invalid("minimum_lot_size", "Not above the maximum lot size "+*l.Maximum+".")
	}
	if l.Rounding != nil {
		for _, f := range []lotField{{"minimum_lot_size", l.Minimum}, {"maximum_lot_size", l.Maximum}} {
			if f.value != nil && !multiple(money.ParseRat(*f.value), money.ParseRat(*l.Rounding)) {
				return invalid("rounding_value", "The minimum and maximum lot sizes are multiples of it ("+f.name+" "+*f.value+").")
			}
		}
	}
	return nil
}

// multiple reports whether q is a whole multiple of r.
func multiple(q, r *big.Rat) bool { return new(big.Rat).Quo(q, r).IsInt() }

// roundUp is q rounded up to a whole multiple of r.
func roundUp(q, r *big.Rat) *big.Rat {
	n := new(big.Rat).Quo(q, r)
	whole := new(big.Int).Quo(n.Num(), n.Denom()) // n is not negative: Quo truncates down
	if !n.IsInt() {
		whole.Add(whole, big.NewInt(1))
	}
	return new(big.Rat).Mul(new(big.Rat).SetInt(whole), r)
}

// lots sizes the shortage of a material, with available its stock plus
// receipts less demand: the lots a run proposes, none without a
// shortage. A fixed procedure proposes as many lots of its size as cover
// the shortage; the others size a quantity, the shortage itself (lot for
// lot) or what brings the available quantity up to the maximum stock
// level, raise it to the minimum lot size, round it up to a multiple of
// the rounding value and split it into lots of the maximum lot size and
// a last lot of the rest, itself raised to the minimum. More than
// MaxLots lots is 409 mrp.too_many_lots, naming the material.
func (l LotSizing) lots(shortage, available *big.Rat, material string) ([]*big.Rat, error) {
	if shortage.Sign() <= 0 {
		return nil, nil
	}
	if l.Procedure == FixedLot {
		size := rat(l.Fixed)
		return repeat(size, roundUp(shortage, size), nil, material)
	}
	q := shortage
	if l.Procedure == ReplenishToMaximum {
		q = money.CeilQuantity(new(big.Rat).Sub(rat(l.MaximumLevel), available))
	}
	return l.bounded(q, material)
}

// bounded raises q to the minimum lot size, rounds it up to a multiple
// of the rounding value and splits it into lots of the maximum lot size
// and a last lot of the rest, itself raised to the minimum: the lots of
// a lot-for-lot, replenish-to-maximum or period quantity. More than
// MaxLots lots is 409 mrp.too_many_lots, naming the material.
func (l LotSizing) bounded(q *big.Rat, material string) ([]*big.Rat, error) {
	minimum, maximum, rounding := rat(l.Minimum), rat(l.Maximum), rat(l.Rounding)
	if minimum != nil {
		q = money.MaxRat(q, minimum)
	}
	if rounding != nil {
		q = roundUp(q, rounding)
	}
	if maximum == nil {
		return []*big.Rat{q}, nil
	}
	return repeat(maximum, q, minimum, material)
}

// rat is a quantity given, nil for none.
func rat(s *string) *big.Rat {
	if s == nil {
		return nil
	}
	return money.ParseRat(*s)
}

// repeat splits total into lots of size and a last lot of the rest,
// raised to minimum when one is given; more than MaxLots is refused.
func repeat(size, total, minimum *big.Rat, material string) ([]*big.Rat, error) {
	n := new(big.Rat).Quo(total, size)
	full := new(big.Int).Quo(n.Num(), n.Denom())
	rest := new(big.Rat).Sub(total, new(big.Rat).Mul(new(big.Rat).SetInt(full), size))
	count := new(big.Int).Set(full)
	if rest.Sign() > 0 {
		count.Add(count, big.NewInt(1))
	}
	if count.Cmp(big.NewInt(MaxLots)) > 0 {
		return nil, problem.Conflict("mrp.too_many_lots", fmt.Sprintf("Material %s needs %s lots of %s, more than the %d a run proposes; raise its "+
			"fixed or maximum lot size (updateMaterialPlan).", material, count, money.PlainQuantity(size), MaxLots))
	}
	out := make([]*big.Rat, 0, count.Int64())
	for range full.Int64() {
		out = append(out, size)
	}
	if rest.Sign() > 0 {
		if minimum != nil {
			rest = money.MaxRat(rest, minimum)
		}
		out = append(out, rest)
	}
	return out, nil
}
