package i18n

import (
	"regexp"
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

// DecimalSign is the locale's decimal sign, as Decimal writes it.
func (l Locale) DecimalSign() string {
	r := []rune(l.Decimal("0.0"))
	return string(r[1 : len(r)-1])
}

// typedAmount is what Amount takes once the sign is set aside and the
// decimal sign is a point: the shape of the API's Money.amount.
var typedAmount = regexp.MustCompile(`^(0|[1-9][0-9]{0,17})(\.[0-9]{1,4})?$`)

// Amount reads an amount a person typed (ADR-0052 §7): an optional minus,
// the digits and at most one decimal sign, the locale's or a point, with
// no grouping, as the API's decimal string with a point. ok is false for
// anything else, and for the shapes the API's amount never takes (a
// leading zero, more than eighteen whole or four fraction digits). Whether
// the fraction has the currency's minor-unit digits is the API's to say:
// no operation names a currency's digits.
func (l Locale) Amount(typed string) (string, bool) {
	s := strings.TrimSpace(typed)
	neg := strings.HasPrefix(s, "-")
	s, ok := l.decimal(strings.TrimPrefix(s, "-"), typedAmount)
	if ok && neg {
		s = "-" + s
	}
	return s, ok
}

// typedQuantity is what Quantity takes once the decimal sign is a point:
// the shape of the API's Quantity.value without its sign.
var typedQuantity = regexp.MustCompile(`^(0|[1-9][0-9]{0,13})(\.[0-9]{1,6})?$`)

// Quantity reads a quantity a person typed, as Amount reads an amount:
// the digits and at most one decimal sign, the locale's or a point, with
// no grouping and no sign (an invoice's quantities are not negative), as
// the API's decimal string with a point; ok is false for anything else.
func (l Locale) Quantity(typed string) (string, bool) {
	return l.decimal(strings.TrimSpace(typed), typedQuantity)
}

// decimal is s with the locale's decimal sign read as a point, when it
// then has the shape.
func (l Locale) decimal(s string, shape *regexp.Regexp) (string, bool) {
	if sign := l.DecimalSign(); sign != "." {
		s = strings.Replace(s, sign, ".", 1)
	}
	if !shape.MatchString(s) {
		return "", false
	}
	return s, true
}

// AmountInput is an API amount as a person would type it: with the
// locale's decimal sign and no grouping, so Amount reads it back.
func (l Locale) AmountInput(amount string) string {
	return strings.Replace(amount, ".", l.DecimalSign(), 1)
}
