package money

import (
	"math"
	"math/big"
	"strings"
	"testing"
)

// Parsing never panics, and whatever parses formats back to the same value.
func FuzzParseDecimal(f *testing.F) {
	for _, s := range []string{"0", "1.5", "-12.34", "123.45-", "1,000.00", "", ".", "9999999999999999999", "1.239", "+7"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v, err := ParseDecimal(s, 2)
		if err != nil {
			return
		}
		back, err := ParseDecimal(v.Decimal(), 2)
		if err != nil || back != v {
			t.Fatalf("%q -> %d -> %q -> %d (%v)", s, v, v.Decimal(), back, err)
		}
		if q, err := ParseQty(s); err == nil && strings.Contains(s, "e") {
			t.Fatalf("exponent accepted: %q -> %d", s, q)
		}
	})
}

// Conversion never panics, is within half a minor unit of the exact
// product, rounds ties away from zero, and is symmetric in the sign.
func FuzzConvert(f *testing.F) {
	f.Add(int64(1), "1.5", uint8(2), uint8(2))
	f.Add(int64(-1), "0.5", uint8(2), uint8(2))
	f.Add(int64(1000), "0.0062", uint8(0), uint8(2))
	f.Add(int64(-12345), "161.5", uint8(2), uint8(0))
	f.Add(int64(math.MaxInt64), "2", uint8(2), uint8(2))
	f.Add(int64(5), "1e9", uint8(3), uint8(2))
	f.Fuzz(func(t *testing.T, amount int64, rate string, from, to uint8) {
		fromExp, toExp := int(from%5), int(to%5)
		got, err := Convert(Minor(amount), rate, fromExp, toExp)
		if err != nil {
			return
		}
		r, _ := new(big.Rat).SetString(rate) // Convert accepted it: a plain decimal
		exact := new(big.Rat).Mul(new(big.Rat).SetInt64(amount), r)
		exact.Mul(exact, new(big.Rat).SetFrac(pow10(toExp), pow10(fromExp)))
		diff := new(big.Rat).Sub(new(big.Rat).SetInt64(int64(got)), exact)
		switch diff.Abs(diff).Cmp(big.NewRat(1, 2)) {
		case 1:
			t.Fatalf("Convert(%d, %q, %d, %d) = %d, more than half off %s", amount, rate, fromExp, toExp, got, exact.RatString())
		case 0:
			if new(big.Rat).Abs(new(big.Rat).SetInt64(int64(got))).Cmp(new(big.Rat).Abs(exact)) < 0 {
				t.Fatalf("Convert(%d, %q, %d, %d) = %d, a tie rounded toward zero from %s", amount, rate, fromExp, toExp, got, exact.RatString())
			}
		}
		if amount == math.MinInt64 {
			return
		}
		neg, err := Convert(Minor(-amount), rate, fromExp, toExp)
		if err != nil || neg != -got {
			t.Fatalf("Convert(%d) = %d but Convert(%d) = %d, %v", amount, got, -amount, neg, err)
		}
	})
}

func pow10(n int) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil) }
