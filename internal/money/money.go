// Package money handles amounts as integer minor units and document
// quantities as plain decimal strings of up to six decimals, the
// numeric(20,6) columns (ADR-0005), extended with Scale. Floats are never
// used for financial values.
package money

import (
	"cmp"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// Minor is an amount in minor currency units (e.g. cents).
type Minor int64

// ParseDecimal parses a decimal string such as SAP's "1234.56" or "-0.5"
// into minor units. Extra decimals are accepted only if they are zero.
func ParseDecimal(s string, decimals int) (Minor, error) {
	v, err := parseScaled(s, decimals)
	return Minor(v), err
}

// ParseAmount parses an API amount (ADR-0005): a decimal string with
// exactly the currency's minor-unit digits, "1080.00" for EUR and "100" for
// JPY. Fewer or more digits are an error, even if they are zeros.
func ParseAmount(s string, decimals int) (Minor, error) {
	intPart, frac, hasFrac := strings.Cut(strings.TrimPrefix(s, "-"), ".")
	if intPart == "" || hasFrac == (decimals == 0) || len(frac) != decimals {
		return 0, fmt.Errorf("amount %q must have exactly %d decimals", s, decimals)
	}
	return ParseDecimal(s, decimals)
}

// QuantityDecimals and QuantityIntDigits bound a document quantity: the
// quantity columns are numeric(20,6), which round a seventh decimal away
// and refuse a fifteenth integer digit.
const (
	QuantityDecimals  = 6
	QuantityIntDigits = 14
)

// PositiveQuantity checks a document quantity (an order, receipt or
// invoice line): a plain positive decimal (digits, an optional "." with
// digits) that a quantity column stores exactly and Scale reads when the
// line is extended. A fraction ("1/3"), an exponent ("1e3"), a sign,
// surrounding space or a bare "." side, which big.Rat or ParseDecimal
// would read, is refused, as are significant decimals beyond the sixth and
// integer digits beyond the fourteenth.
func PositiveQuantity(s string) error {
	intPart, frac, ok := plainParts(s)
	switch {
	case !ok:
		return fmt.Errorf("quantity %q is not a plain decimal", s)
	case len(strings.TrimRight(frac, "0")) > QuantityDecimals:
		return fmt.Errorf("quantity %q has more than %d significant decimals", s, QuantityDecimals)
	case len(strings.TrimLeft(intPart, "0")) > QuantityIntDigits:
		return fmt.Errorf("quantity %q has more than %d integer digits", s, QuantityIntDigits)
	case strings.Trim(intPart+frac, "0") == "":
		return fmt.Errorf("quantity %q is not positive", s)
	}
	return nil
}

// decimalParts splits a decimal in the grammar every amount is read with:
// optional surrounding space, one sign (a leading "-" or "+", or the
// trailing "-" of SAP table extracts), digits, and an optional "." with
// digits. It refuses anything else.
func decimalParts(raw string) (neg bool, intPart, frac string, err error) {
	s := strings.TrimSpace(raw)
	// SAP table extracts sometimes carry a trailing minus ("123.45-").
	if strings.HasSuffix(s, "-") {
		neg, s = true, strings.TrimSuffix(s, "-")
	}
	if strings.HasPrefix(s, "-") {
		if neg {
			return false, "", "", fmt.Errorf("invalid decimal %q", raw)
		}
		neg, s = true, s[1:]
	} else if strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	intPart, frac, _ = strings.Cut(s, ".")
	if intPart == "" && frac == "" {
		return false, "", "", fmt.Errorf("invalid decimal %q", raw)
	}
	if !digits(intPart) || !digits(frac) {
		return false, "", "", fmt.Errorf("invalid decimal %q", raw)
	}
	return neg, intPart, frac, nil
}

// ParseDecimalRat reads a decimal in the grammar of ParseDecimal exactly,
// with any number of decimals: for a figure validated with ParseDecimal at
// one currency's digits and compared with an amount in another.
func ParseDecimalRat(raw string) (*big.Rat, error) {
	neg, intPart, frac, err := decimalParts(raw)
	if err != nil {
		return nil, err
	}
	r, _ := new(big.Rat).SetString(cmp.Or(intPart, "0") + "." + frac + "0") // digits only: always a decimal
	if neg {
		r.Neg(r)
	}
	return r, nil
}

func parseScaled(raw string, decimals int) (int64, error) {
	neg, intPart, frac, err := decimalParts(raw)
	if err != nil {
		return 0, err
	}
	if len(frac) > decimals {
		if strings.Trim(frac[decimals:], "0") != "" {
			return 0, fmt.Errorf("decimal %q has more than %d significant decimals", raw, decimals)
		}
		frac = frac[:decimals]
	}
	frac += strings.Repeat("0", decimals-len(frac))
	if intPart == "" {
		intPart = "0"
	}
	v, err := strconv.ParseInt(intPart+frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("decimal %q out of range", raw)
	}
	if neg {
		v = -v
	}
	return v, nil
}

func digits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Format renders minor units with thousands separators, e.g. "-1,234.56".
func (m Minor) Format(decimals int) string {
	sign := ""
	v := int64(m)
	if v < 0 {
		sign, v = "-", -v
	}
	scale := int64(1)
	for range decimals {
		scale *= 10
	}
	intStr := strconv.FormatInt(v/scale, 10)
	var b strings.Builder
	for i, r := range intStr {
		if i > 0 && (len(intStr)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if decimals == 0 {
		return sign + b.String()
	}
	return fmt.Sprintf("%s%s.%0*d", sign, b.String(), decimals, v%scale)
}

// String renders the raw minor units, "123456 minor", for logs and %v. A
// Minor does not know its currency, so String cannot place the decimal
// point: with two digits assumed, a JPY or BHD amount printed wrong. Use
// Format or Plain with the currency's exponent for anything a person reads.
func (m Minor) String() string { return strconv.FormatInt(int64(m), 10) + " minor" }

// Plain renders minor units with the currency's minor-unit digits and no
// separators, "-1234.56" or "100": the API format (ADR-0005).
func (m Minor) Plain(decimals int) string { return strings.ReplaceAll(m.Format(decimals), ",", "") }

// EventAmount is an amount as event payloads carry it, the same shape as in
// the API: {"amount": "1234.56", "currency": "EUR"} with the currency's
// minor-unit digits (ADR-0005, ADR-0007).
func EventAmount(amount Minor, currency string, decimals int) map[string]string {
	return map[string]string{"amount": amount.Plain(decimals), "currency": currency}
}

// Percent returns amount * pct / 100 rounded half away from zero (pct 8 =
// 8%), exactly; a result out of range is ErrOverflow.
func Percent(amount Minor, pct int64) (Minor, error) {
	return Scale(amount, strconv.FormatInt(pct, 10), 100)
}

// Abs returns the absolute value.
func (m Minor) Abs() Minor {
	if m < 0 {
		return -m
	}
	return m
}

// PercentOf returns part in percent of whole, part * 100 / whole, as a
// decimal string of two places computed exactly and rounded half away from
// zero ("12.35", "-0.40"); a negative result that rounds to zero is "0.00",
// without a sign. whole must not be zero: a percent of nothing has no
// value, and callers show none (ADR-0050 §8).
func PercentOf(part, whole Minor) string {
	return percent(big.NewInt(int64(part)), big.NewInt(int64(whole)))
}

// PercentOfMagnitude is PercentOf over whole's absolute value, so the
// result has part's sign whatever whole's (ADR-0058 §9: a variance in
// percent of a comparison that may be a credit). whole must not be zero.
func PercentOfMagnitude(part, whole Minor) string {
	return percent(big.NewInt(int64(part)), new(big.Int).Abs(big.NewInt(int64(whole))))
}

func percent(part, whole *big.Int) string {
	s := new(big.Rat).SetFrac(part.Mul(part, big.NewInt(100)), whole).FloatString(2)
	if s == "-0.00" {
		return "0.00"
	}
	return s
}
