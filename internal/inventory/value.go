package inventory

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	platformcurrency "github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// valuationKey is what a valuation values: a material in a plant, the
// valuation area (ADR-0082 §7).
type valuationKey struct{ material, plant uuid.UUID }

func (k valuationKey) compare(o valuationKey) int {
	return cmp.Or(bytes.Compare(k.material[:], o.material[:]), bytes.Compare(k.plant[:], o.plant[:]))
}

// lockValuations locks the valuations of the lines' materials in their
// plants, in valuationKey order, before the stock rows (ADR-0008): a
// price change locks its valuation first too. A line whose material has
// no valuation in its plant is refused (422 inventory.no_valuation).
func lockValuations(ctx context.Context, tx pgx.Tx, lines []Line, r refs) (map[valuationKey]*Valuation, error) {
	var keys []valuationKey
	for _, l := range lines {
		if k := (valuationKey{l.MaterialID, l.PlantID}); !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, valuationKey.compare)
	ms, plants := make([]uuid.UUID, len(keys)), make([]uuid.UUID, len(keys))
	for i, k := range keys {
		ms[i], plants[i] = k.material, k.plant
	}
	rows, _ := tx.Query(ctx, valuationSelect+` JOIN unnest($1::uuid[], $2::uuid[]) AS x (m, p) ON v.material_id = x.m AND v.plant_id = x.p
		ORDER BY v.material_id, v.plant_id FOR UPDATE OF v`, ms, plants)
	vs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Valuation, error) { return scanValuation(row) })
	if err != nil {
		return nil, fmt.Errorf("inventory: lock the valuations of %d materials: %w", len(keys), err)
	}
	out := map[valuationKey]*Valuation{}
	for i := range vs {
		out[valuationKey{vs[i].MaterialID, vs[i].PlantID}] = &vs[i]
	}
	for _, l := range lines {
		if out[valuationKey{l.MaterialID, l.PlantID}] != nil {
			continue
		}
		return nil, problem.Unprocessable("inventory.no_valuation", "Material "+r.materials[l.MaterialID].Number+" is not valued in plant "+
			r.units[l.PlantID].Code+"; it moves once it is (createMaterialValuation).")
	}
	return out, nil
}

// valueLines values each line in line order, the order movement_stock
// applies them in, against its valuation as the lines before it left it
// (ADR-0082 §8):
//
//   - a line out of stock is worth what it takes from the value left: at
//     a standard price the remaining quantity is worth its quantity at the
//     price, at a moving price the value in proportion to the quantity
//     left, so an issue of the whole stock takes its whole value and the
//     rounding of an issue is taken by the value left;
//   - a line into stock at a standard price adds what the stock is worth
//     at the price after it; at a moving price it adds what it is valued
//     at: the amount a receipt names, what a transfer took out of the
//     other location or a reversal gives back, or else its quantity at
//     the moving price, which is refused while that is zero;
//   - what a line is valued at beyond what its stock takes is its price
//     difference: a reversal is valued at what its original line was, and
//     a transfer's receipt at what its issue took; within a plant (311,
//     312) a reversal's too, so it leaves the plant's value as it was,
//     while a 302 gives each plant back what its 301 moved.
func valueLines(lines []Line, mt movementType, vals map[valuationKey]*Valuation, amounts map[int16]money.Minor, r refs) error {
	for i := range lines {
		l := &lines[i]
		q := money.ParseRat(l.Quantity)
		target := l.target
		if a, ok := amounts[l.ItemNo]; ok && target == nil {
			target = &a
		}
		if mt.transfer == betweenStorageLocations || (mt.transfer == betweenPlants && mt.reverses == "") {
			target = nil
			if issue, ok := issueOf(lines, i); ok && q.Sign() > 0 {
				took := -issue.Amount
				target = &took
			}
		}
		amount, err := vals[valuationKey{l.MaterialID, l.PlantID}].move(q, target)
		if err != nil {
			return lineErr(err, l, r)
		}
		l.Amount, l.Difference = amount, 0
		if target != nil {
			l.Difference = *target - amount
		}
	}
	return nil
}

// issueOf is the issue of the transfer whose receipt is lines[i]: the
// line before it, of the same item. items gives each transfer's item its
// issue and then its receipt, and negated, taking a reversal's lines last
// first, gives the reversal of the receipt, an issue, before that of the
// issue.
func issueOf(lines []Line, i int) (Line, bool) {
	if i == 0 || lines[i-1].ItemNo != lines[i].ItemNo {
		return Line{}, false
	}
	return lines[i-1], true
}

// errNoPrice is move's refusal of a receipt at a moving price of zero
// that names no amount.
var errNoPrice = errors.New("no price")

// qty is the valuation's stock quantity, exactly.
func (v *Valuation) qty() *big.Rat { return money.ParseRat(v.Quantity) }

// move values a movement of the quantity q into (positive) or out of the
// valuation's stock, at target when it is given, and applies it as
// movement_stock does: every amount is rounded once, half away from zero.
func (v *Valuation) move(q *big.Rat, target *money.Minor) (money.Minor, error) {
	before := v.qty()
	after := new(big.Rat).Add(before, q)
	price, unit := rat(v.Price), rat(money.Minor(v.PriceUnit))
	var amount money.Minor
	var err error
	switch {
	case v.PriceControl == Standard: // the stock after it at the price, less the value before
		amount, err = valueLeft(new(big.Rat).Quo(new(big.Rat).Mul(after, price), unit), v.Value)
	case q.Sign() < 0: // the value in proportion to the quantity left, less the value before
		left := new(big.Rat)
		if after.Sign() > 0 {
			left.Quo(left.Mul(rat(v.Value), after), before)
		}
		amount, err = valueLeft(left, v.Value)
	case target != nil: // what it is worth
		amount = *target
	case v.Price == 0:
		return 0, errNoPrice
	default: // its quantity at the moving price
		amount, err = money.Round(new(big.Rat).Quo(new(big.Rat).Mul(q, price), unit), 0)
	}
	if err != nil {
		return 0, err
	}
	value, err := money.Sum(v.Value, amount)
	if err != nil {
		return 0, err
	}
	if v.PriceControl == MovingAverage && after.Sign() > 0 {
		if v.Price, err = money.Round(new(big.Rat).Quo(new(big.Rat).Mul(rat(value), unit), after), 0); err != nil {
			return 0, err
		}
	}
	v.Quantity, v.Value = money.PlainQuantity(after), value
	return amount, nil
}

// valueLeft is what a movement takes from or adds to the value before:
// the value it leaves, rounded, less that.
func valueLeft(left *big.Rat, before money.Minor) (money.Minor, error) {
	l, err := money.Round(left, 0)
	return l - before, err
}

func rat(m money.Minor) *big.Rat { return new(big.Rat).SetInt64(int64(m)) }

// lineErr is the refusal of a line move could not value: a receipt
// without a price, or a value beyond the range of an amount.
func lineErr(err error, l *Line, r refs) error {
	m := r.materials[l.MaterialID]
	if errors.Is(err, errNoPrice) {
		return problem.Invalid("inventory.no_price", fmt.Sprintf("items[%d].amount", l.ItemNo-1), "Material "+m.Number+" has no moving price in plant "+
			r.units[l.PlantID].Code+" yet; its receipt names the amount it is worth.")
	}
	// Else money.ErrOverflow: move's roundings and sums fail only beyond
	// an amount's range.
	return problem.Unprocessable("inventory.amount_too_large", "The value of material "+m.Number+"'s line "+fmt.Sprint(l.LineNo)+
		" is beyond the range of an amount.")
}

// itemAmounts reads the amounts the request's receipts are worth, in the
// valuation's currency, by item number; only a 501 or 561 names one.
func itemAmounts(items []ItemInput, mt movementType, code string, vals map[valuationKey]*Valuation) (map[int16]money.Minor, error) {
	out := map[int16]money.Minor{}
	for i, it := range items {
		if it.Amount == nil {
			continue
		}
		field := fmt.Sprintf("items[%d].amount", i)
		if mt.sign < 0 || mt.transfer != noTransfer {
			return nil, problem.Invalid("inventory.amount_not_allowed", field, "Only a receipt ("+code+" is not one) names what it is worth.")
		}
		v := vals[valuationKey{it.MaterialID, it.PlantID}]
		a, err := amountOf(field, *it.Amount, v.Currency, v.Exponent, "inventory.invalid_amount")
		if err != nil {
			return nil, err
		}
		out[int16(i+1)] = a // at most 100 items (the contract's)
	}
	return out, nil
}

// amountOf reads an amount or price a request gives in the company's
// currency, exp decimals: in its form (400 request.invalid_amount) and
// not negative (422 code).
func amountOf(field string, a Amount, currency string, exp int, code string) (money.Minor, error) {
	if a.Currency != currency {
		return 0, problem.Invalid("inventory.currency_mismatch", field+".currency", "Amounts are in the company's currency "+currency+".")
	}
	m, err := platformcurrency.ParseAmount(a.Value, currency, exp, field+".amount")
	if err != nil {
		return 0, err
	}
	if m < 0 {
		return 0, problem.Invalid(code, field+".amount", "The amount is not negative.")
	}
	return m, nil
}
