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
//     a transfer's receipt at what its issue took.
func valueLines(lines []Line, mt movementType, vals map[valuationKey]*Valuation, amounts map[int16]money.Minor, r refs) error {
	for i := range lines {
		l := &lines[i]
		v := vals[valuationKey{l.MaterialID, l.PlantID}]
		target := l.target
		if a, ok := amounts[l.ItemNo]; ok && target == nil {
			target = &a
		}
		if mt.transfer != noTransfer && mt.reverses == "" && i > 0 && lines[i-1].ItemNo == l.ItemNo { // a transfer's receipt follows its issue
			took := -lines[i-1].Amount
			target = &took
		}
		amount, err := v.move(money.ParseRat(l.Quantity), target)
		if err != nil {
			return lineErr(err, l, r)
		}
		l.Amount, l.Difference = amount, amount
		if target != nil {
			l.Difference = *target
		}
		l.Difference -= amount
	}
	return nil
}

// errNoPrice is move's refusal of a receipt at a moving price of zero
// that names no amount.
var errNoPrice = errors.New("no price")

// move values a movement of the quantity into (positive) or out of the
// valuation's stock, the value target when it is given, and applies it
// as movement_stock does: every amount is rounded once, half away from
// zero.
func (v *Valuation) move(q *big.Rat, target *money.Minor) (money.Minor, error) {
	before := money.ParseRat(v.Quantity)
	after := new(big.Rat).Add(before, q)
	value, price, unit := rat(v.Value), rat(v.Price), rat(money.Minor(v.PriceUnit))
	// The amount is worth, rounded, less offset.
	worth, offset := new(big.Rat), v.Value
	switch {
	case v.PriceControl == Standard: // what the stock after it is worth at the price
		worth.Quo(worth.Mul(after, price), unit)
	case q.Sign() < 0: // the value left in proportion to the quantity left
		if after.Sign() > 0 {
			worth.Quo(worth.Mul(value, after), before)
		}
	case target != nil:
		worth, offset = rat(*target), 0
	case v.Price == 0:
		return 0, errNoPrice
	default: // its quantity at the moving price
		worth.Quo(worth.Mul(q, price), unit)
		offset = 0
	}
	rounded, err := money.Round(worth, 0)
	amount := rounded - offset
	newPrice := v.Price
	if v.PriceControl == MovingAverage && after.Sign() > 0 {
		var perr error
		newPrice, perr = money.Round(new(big.Rat).Quo(new(big.Rat).Mul(rat(v.Value+amount), unit), after), 0)
		err = errors.Join(err, perr)
	}
	if err != nil {
		return 0, err
	}
	v.Quantity, v.Value, v.Price = money.PlainQuantity(after), v.Value+amount, newPrice
	return amount, nil
}

func rat(m money.Minor) *big.Rat { return new(big.Rat).SetInt64(int64(m)) }

// lineErr is the refusal of a line move could not value.
func lineErr(err error, l *Line, r refs) error {
	m := r.materials[l.MaterialID]
	if errors.Is(err, errNoPrice) {
		return problem.Invalid("inventory.no_price", fmt.Sprintf("items[%d].amount", l.ItemNo-1), "Material "+m.Number+" has no moving price in plant "+
			r.units[l.PlantID].Code+" yet; its receipt names the amount it is worth.")
	}
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
		if it.Amount.Currency != v.Currency {
			return nil, problem.Invalid("inventory.currency_mismatch", field+".currency", "Amounts are in the company's currency "+v.Currency+".")
		}
		a, err := platformcurrency.ParseAmount(it.Amount.Value, v.Currency, v.Exponent, field+".amount")
		if err != nil {
			return nil, err
		}
		if a < 0 {
			return nil, problem.Invalid("inventory.invalid_amount", field+".amount", "A receipt is worth an amount that is not negative.")
		}
		out[int16(i+1)] = a // at most 100 items (the contract's)
	}
	return out, nil
}
