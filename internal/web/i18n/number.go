package i18n

import (
	"strconv"
	"strings"

	"golang.org/x/text/number"
)

// Money is an amount as the API gives it (ADR-0005: a decimal string with
// exactly the currency's minor-unit digits, "1284330.00", "-5", "1.250")
// in the locale's digits, grouping and decimal sign (CLDR, through
// x/text), beside its ISO 4217 code in the locale's order. It never goes
// through a float: the whole part is an integer, and the fraction keeps
// the API's digits. A string that is not such a decimal is shown as it
// is, as an open value would be.
func (l Locale) Money(amount, currency string) string {
	return l.T(MoneyAmount, l.Decimal(amount), currency)
}

// Decimal is a decimal string (an optional minus, at most 18 integer and
// 18 fraction digits) in the locale's digits and signs; any other string
// as it is.
func (l Locale) Decimal(s string) string {
	neg := strings.HasPrefix(s, "-")
	whole, frac, dotted := strings.Cut(strings.TrimPrefix(s, "-"), ".")
	if !digits(whole) || len(whole) > 18 || (dotted && (!digits(frac) || len(frac) > 18)) {
		return s
	}
	n, _ := strconv.ParseInt(whole, 10, 64) // 18 digits or fewer: within int64
	// The whole part with as many fraction places as the amount has, so
	// the locale's decimal sign and grouping are CLDR's; its zeros are
	// then replaced by the fraction's own digits, in the locale's digits.
	out := l.printer.Sprint(number.Decimal(n, number.Scale(len(frac))))
	if frac != "" {
		f, _ := strconv.ParseInt(frac, 10, 64) // at most 18 digits, as checked
		fd := []rune(l.printer.Sprint(number.Decimal(f, number.MinIntegerDigits(len(frac)), number.NoSeparator())))
		r := []rune(out)
		out = string(append(r[:len(r)-len(fd)], fd...))
	}
	if neg {
		out = "-" + out
	}
	return out
}

// digits reports whether s is one or more ASCII digits.
func digits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
