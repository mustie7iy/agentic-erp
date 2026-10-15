package money

import (
	"fmt"
	"math/big"
	"strings"
)

// maxExp bounds currency exponents: ISO 4217 has none above 4, and 10^18
// is the largest power of ten a Minor holds.
const maxExp = 18

// Convert translates an amount from a currency with fromExp minor-unit
// digits to one with toExp at the rate (a plain non-negative decimal such
// as "1.085" or "0.006200000000", as numeric columns render it), rounding
// half away from zero: 0.5 becomes 1 and -0.5 becomes -1. Conversions are
// exact rational arithmetic; floats are never involved (ADR-0005).
func Convert(amount Minor, rate string, fromExp, toExp int) (Minor, error) {
	if toExp < 0 || toExp > maxExp {
		return 0, fmt.Errorf("money: convert from exponent %d to %d: exponents are 0 to %d", fromExp, toExp, maxExp)
	}
	v, err := Exact(amount, rate, fromExp)
	if err != nil {
		return 0, err
	}
	return fromBig(roundHalfAway(v.Mul(v, new(big.Rat).SetInt(pow10(toExp)))))
}

// Exact is an amount of fromExp minor-unit digits at the rate (as for
// Convert) in major units of the other currency, unrounded: for comparing
// a converted amount with a configured one, where rounding to the other
// currency's digits first would let 5.004 pass a limit of 5.00 (F13).
func Exact(amount Minor, rate string, fromExp int) (*big.Rat, error) {
	r, err := parsePlain(rate)
	if err != nil {
		return nil, err
	}
	if fromExp < 0 || fromExp > maxExp {
		return nil, fmt.Errorf("money: convert from exponent %d: exponents are 0 to %d", fromExp, maxExp)
	}
	return r.Mul(Rat(amount, fromExp), r), nil
}

// Scale returns amount × factor / divisor rounded half away from zero, in
// exact rational arithmetic: an extended price, quantity × unit price per
// price unit. The factor is a plain decimal such as "2.5" or "-1"; the
// divisor is positive. A result out of range is ErrOverflow, never wrapped.
func Scale(amount Minor, factor string, divisor int64) (Minor, error) {
	f, err := parsePlain(strings.TrimPrefix(factor, "-"))
	if err != nil {
		return 0, err
	}
	if divisor <= 0 {
		return 0, fmt.Errorf("money: scale by %s/%d: the divisor must be positive", factor, divisor)
	}
	if strings.HasPrefix(factor, "-") {
		f.Neg(f)
	}
	v := new(big.Rat).Mul(new(big.Rat).SetInt64(int64(amount)), f)
	return fromBig(roundHalfAway(v.Quo(v, new(big.Rat).SetInt64(divisor))))
}

// Round returns a rational number of major units in minor units with exp
// digits, rounding half away from zero: a quotient of amounts, or a ratio
// kept to a fixed number of digits, without a float in between. A result
// out of range is ErrOverflow.
func Round(major *big.Rat, exp int) (Minor, error) {
	if exp < 0 || exp > maxExp {
		return 0, fmt.Errorf("money: round to exponent %d: exponents are 0 to %d", exp, maxExp)
	}
	return fromBig(roundHalfAway(new(big.Rat).Mul(major, new(big.Rat).SetInt(pow10(exp)))))
}

// Rat is an amount of exp minor-unit digits as an exact rational in major
// units: 123456 at exponent 3 is 123456/1000. A negative exponent scales
// up, so Rat(5, -2) is 500; a currency exponent is never negative.
func Rat(m Minor, exp int) *big.Rat {
	if exp < 0 {
		return new(big.Rat).SetInt(new(big.Int).Mul(big.NewInt(int64(m)), pow10(-exp)))
	}
	return new(big.Rat).SetFrac(big.NewInt(int64(m)), pow10(exp))
}

// pow10 is 10^n for n >= 0.
func pow10(n int) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil) }

// parsePlain accepts only plain non-negative decimals: big.Rat would also take
// fractions and exponents ("1e1000000"), whose expansion is unbounded.
func parsePlain(s string) (*big.Rat, error) {
	if _, _, ok := plainParts(s); !ok {
		return nil, fmt.Errorf("money: invalid rate or factor %q: want a plain decimal", s)
	}
	r, _ := new(big.Rat).SetString(s) // digits with at most one dot always parse
	return r, nil
}

// plainParts splits a plain non-negative decimal: digits, and an optional
// "." with digits. No sign, no space, no bare "." side: the form a numeric
// column prints and Scale reads.
func plainParts(s string) (intPart, frac string, ok bool) {
	intPart, frac, hasFrac := strings.Cut(s, ".")
	if intPart == "" || !digits(intPart) || !digits(frac) || (hasFrac && frac == "") {
		return "", "", false
	}
	return intPart, frac, true
}

// roundHalfAway rounds a rational to the nearest integer, ties away from
// zero.
func roundHalfAway(v *big.Rat) *big.Int {
	num, den := new(big.Int).Abs(v.Num()), v.Denom()
	q, m := new(big.Int).QuoRem(num, den, new(big.Int))
	if m.Mul(m, big.NewInt(2)).Cmp(den) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if v.Sign() < 0 {
		q.Neg(q)
	}
	return q
}

// ConvertAll converts each amount (Convert) and puts the rounding
// difference on the largest converted amount, the first of equals, so the
// results add up to the converted total of the amounts. A balanced set of
// amounts, adding up to zero, stays balanced: what a journal entry needs in
// each of its currencies.
func ConvertAll(amounts []Minor, rate string, fromExp, toExp int) ([]Minor, error) {
	total, err := Sum(amounts...)
	if err != nil {
		return nil, err
	}
	want, err := Convert(total, rate, fromExp, toExp)
	if err != nil {
		return nil, err
	}
	out := make([]Minor, len(amounts))
	largest := 0
	for i, a := range amounts {
		if out[i], err = Convert(a, rate, fromExp, toExp); err != nil {
			return nil, err
		}
		if magnitude(out[i]) > magnitude(out[largest]) {
			largest = i
		}
	}
	if len(out) == 0 {
		return out, nil
	}
	// The largest takes want less the others, in big integers: the sum of
	// the others alone may be out of range when the result is not.
	adjusted := big.NewInt(int64(want))
	for i, o := range out {
		if i != largest {
			adjusted.Sub(adjusted, big.NewInt(int64(o)))
		}
	}
	if out[largest], err = fromBig(adjusted); err != nil {
		return nil, err
	}
	return out, nil
}

// magnitude is |m| without overflow at the most negative amount.
func magnitude(m Minor) uint64 {
	if m < 0 {
		return uint64(-(m + 1)) + 1
	}
	return uint64(m)
}

// ParseRat reads a decimal a numeric column rendered as text ("12.500"),
// exactly. A decimal always parses; anything else is nil, so it is for
// database values and input already checked as a decimal.
func ParseRat(s string) *big.Rat {
	r, _ := new(big.Rat).SetString(s) // a nil result is the documented outcome for non-decimal text
	return r
}
