package money

import (
	"math/big"
	"slices"
)

// Allocate rounds the sum of exact amounts (in minor units) once, half
// away from zero, and divides it among them by largest remainder (ties to
// the earlier), so the results add up to the rounded sum and each lies
// between the floor and the ceiling of its exact amount: of its sign, and
// no larger than an integer bound the exact amount respects. Tax rounded
// once per document and shared out to its lines (ADR-0049 §8) and the
// ledger's cash discount split (T36d) are such allocations.
//
// It works on the amounts in the direction of their sum (of the first
// non-zero amount when they cancel), so Allocate(-x) is -Allocate(x): a
// credit memo is the negated invoice. A sum or result out of range is
// ErrOverflow.
func Allocate(exact []*big.Rat) ([]Minor, error) {
	sum := new(big.Rat)
	for _, x := range exact {
		sum.Add(sum, x)
	}
	sign := int64(sum.Sign())
	if sign == 0 {
		sign = 1
		if i := slices.IndexFunc(exact, func(x *big.Rat) bool { return x.Sign() != 0 }); i >= 0 {
			sign = int64(exact[i].Sign())
		}
	}
	s := big.NewRat(sign, 1)
	total, err := Round(new(big.Rat).Mul(sum, s), 0)
	if err != nil {
		return nil, err
	}
	floors, rems := make([]*big.Int, len(exact)), make([]*big.Rat, len(exact))
	left := big.NewInt(int64(total))
	for i, x := range exact {
		y := new(big.Rat).Mul(x, s)
		// Euclidean division by the positive denominator is the floor, the
		// remainder in [0, 1).
		q, m := new(big.Int).DivMod(y.Num(), y.Denom(), new(big.Int))
		floors[i], rems[i] = q, new(big.Rat).SetFrac(m, y.Denom())
		left.Sub(left, q)
	}
	order := make([]int, len(exact))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return rems[b].Cmp(rems[a]) })
	// The rounded sum is within half a unit of the exact one, which exceeds
	// the floors' sum by less than the number of amounts with a remainder:
	// left is at least 0 and at most that number, so only amounts with a
	// remainder get a unit.
	for _, i := range order[:left.Int64()] {
		floors[i].Add(floors[i], big.NewInt(1))
	}
	out := make([]Minor, len(exact))
	for i, f := range floors {
		// Amounts of both signs can sum in range while one is not.
		if out[i], err = fromBig(f.Mul(f, big.NewInt(sign))); err != nil {
			return nil, err
		}
	}
	return out, nil
}
