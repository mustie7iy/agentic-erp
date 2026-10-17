package money

import (
	"errors"
	"math"
	"math/big"
)

// Micro is millionths of a currency's minor unit: what a model call costs,
// a few thousand tokens at a few dollars per million (ADR-0005 as amended,
// ADR-0036). Micro amounts are summed exactly and shown as Minor, never
// mixed with Minor amounts unconverted.
type Micro int64

// microPerMinor is a minor unit in Micro.
const microPerMinor = 1_000_000

// Minor is the amount in minor units, rounded half up; costs are never
// negative.
func (m Micro) Minor() Minor {
	q, r := int64(m)/microPerMinor, int64(m)%microPerMinor
	if r >= microPerMinor/2 {
		q++
	}
	return Minor(q)
}

// Add is m+n held at the most an int64 holds rather than wrapped: a sum of
// costs past that (some ninety billion dollars) stays at it.
func (m Micro) Add(n Micro) Micro {
	if n > math.MaxInt64-m {
		return math.MaxInt64
	}
	return m + n
}

// TokenCost is tokens at perMillion, a price per million tokens: exact in
// Micro for any price of whole minor units, and rounded up at the
// millionth of a minor unit for finer ones, so a cost is never under its
// price. A result out of range is ErrOverflow.
func TokenCost(tokens int64, perMillion Micro) (Micro, error) {
	if tokens < 0 || perMillion < 0 {
		return 0, errors.New("money: token costs are of non-negative tokens and prices")
	}
	product := new(big.Int).Mul(big.NewInt(tokens), big.NewInt(int64(perMillion)))
	million := big.NewInt(1_000_000)
	cost, rest := new(big.Int).QuoRem(product, million, new(big.Int))
	if rest.Sign() > 0 {
		cost.Add(cost, big.NewInt(1))
	}
	if !cost.IsInt64() {
		return 0, ErrOverflow
	}
	return Micro(cost.Int64()), nil
}

// MicroOf is m, an amount in minor units, in Micro: a budget compared with
// costs. An amount past what Micro holds (some ninety billion dollars) is
// held at that most.
func MicroOf(m Minor) Micro {
	if m > math.MaxInt64/microPerMinor {
		return math.MaxInt64
	}
	return Micro(m * microPerMinor)
}

// Reaches reports whether m is at least pct percent of limit, exactly: the
// products are taken in big integers, so the largest amounts compare
// right.
func (m Micro) Reaches(limit Micro, pct int64) bool {
	spent := new(big.Int).Mul(big.NewInt(int64(m)), big.NewInt(100))
	share := new(big.Int).Mul(big.NewInt(int64(limit)), big.NewInt(pct))
	return spent.Cmp(share) >= 0
}
