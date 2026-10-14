package money

import (
	"errors"
	"math"
	"math/big"
	"math/bits"
)

// ErrOverflow reports an amount outside the range of an amount: ±(2^63-1)
// minor units. The range is symmetric, one short of int64 at the bottom,
// so every amount can be negated (reversals, offsets) and rendered.
var ErrOverflow = errors.New("money: amount out of range")

// minMinor is int64's most negative value, which has no positive
// counterpart: never an amount.
const minMinor = Minor(math.MinInt64)

// fromBig converts an exact result, refusing what is out of range.
func fromBig(q *big.Int) (Minor, error) {
	if !q.IsInt64() || q.Int64() == math.MinInt64 {
		return 0, ErrOverflow
	}
	return Minor(q.Int64()), nil
}

// Diff is a - b, or ErrOverflow when that is out of range.
func Diff(a, b Minor) (Minor, error) {
	d := a - b
	// Overflow flips the sign: a and b differ in sign, and d's sign is b's.
	if (a^b)&(a^d) < 0 || d == minMinor {
		return 0, ErrOverflow
	}
	return d, nil
}

// Sum adds amounts exactly: it fails with ErrOverflow only when the total
// is out of range, whatever the order of the terms, so a set of large
// amounts that nets to zero sums to zero.
func Sum(amounts ...Minor) (Minor, error) {
	// A 128-bit two's complement accumulator: hi is the carry word.
	var hi int64
	var lo uint64
	for _, a := range amounts {
		var carry uint64
		lo, carry = bits.Add64(lo, uint64(a), 0)
		hi += int64(carry)
		if a < 0 {
			hi-- // sign extension of a negative term
		}
	}
	if hi != int64(lo)>>63 || Minor(lo) == minMinor {
		return 0, ErrOverflow
	}
	return Minor(lo), nil
}
