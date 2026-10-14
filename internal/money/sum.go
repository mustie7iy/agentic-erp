package money

import (
	"errors"
	"math/bits"
)

// ErrOverflow reports an amount outside the range of Minor (±9.2×10^18
// minor units, the database's bigint).
var ErrOverflow = errors.New("money: amount out of range")

// Diff is a - b, or ErrOverflow when that is out of range.
func Diff(a, b Minor) (Minor, error) {
	d := a - b
	// Overflow flips the sign: a and b differ in sign, and d's sign is b's.
	if (a^b)&(a^d) < 0 {
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
	if hi != int64(lo)>>63 {
		return 0, ErrOverflow
	}
	return Minor(lo), nil
}
