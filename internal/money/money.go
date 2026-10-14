// Package money handles amounts as integer minor units and quantities as
// integer milli-units (ADR-0005). Floats are never used for financial
// values.
package money

import (
	"fmt"
	"strconv"
	"strings"
)

// Minor is an amount in minor currency units (e.g. cents).
type Minor int64

// Qty is a quantity in thousandths of a unit (SAP MENGE has 3 decimals).
type Qty int64

const QtyScale = 1000

// ParseDecimal parses a decimal string such as SAP's "1234.56" or "-0.5"
// into minor units. Extra decimals are accepted only if they are zero.
func ParseDecimal(s string, decimals int) (Minor, error) {
	v, err := parseScaled(s, decimals)
	return Minor(v), err
}

// ParseQty parses a decimal quantity string into milli-units.
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

func ParseQty(s string) (Qty, error) {
	v, err := parseScaled(s, 3)
	return Qty(v), err
}

func parseScaled(raw string, decimals int) (int64, error) {
	s := strings.TrimSpace(raw)
	neg := false
	// SAP table extracts sometimes carry a trailing minus ("123.45-").
	if strings.HasSuffix(s, "-") {
		neg, s = true, strings.TrimSuffix(s, "-")
	}
	if strings.HasPrefix(s, "-") {
		if neg {
			return 0, fmt.Errorf("invalid decimal %q", raw)
		}
		neg, s = true, s[1:]
	} else if strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	intPart, frac, _ := strings.Cut(s, ".")
	if intPart == "" && frac == "" {
		return 0, fmt.Errorf("invalid decimal %q", raw)
	}
	if !digits(intPart) || !digits(frac) {
		return 0, fmt.Errorf("invalid decimal %q", raw)
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

func (m Minor) String() string { return m.Format(2) }

// Plain renders minor units with the currency's minor-unit digits and no
// separators, "-1234.56" or "100": the API format (ADR-0005).
func (m Minor) Plain(decimals int) string { return strings.ReplaceAll(m.Format(decimals), ",", "") }

// EventAmount is an amount as event payloads carry it, the same shape as in
// the API: {"amount": "1234.56", "currency": "EUR"} with the currency's
// minor-unit digits (ADR-0005, ADR-0007).
func EventAmount(amount Minor, currency string, decimals int) map[string]string {
	return map[string]string{"amount": amount.Plain(decimals), "currency": currency}
}

// Decimal renders minor units as a plain decimal string ("-1234.56"), the
// format SAP extracts and OData use.
func (m Minor) Decimal() string {
	v := int64(m)
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	return fmt.Sprintf("%s%d.%02d", sign, v/100, v%100)
}

func (q Qty) String() string {
	v := int64(q)
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	if v%QtyScale == 0 {
		return fmt.Sprintf("%s%d", sign, v/QtyScale)
	}
	return strings.TrimRight(fmt.Sprintf("%s%d.%03d", sign, v/QtyScale, v%QtyScale), "0")
}

// Extend computes qty * price / priceUnit, rounded half away from zero.
// priceUnit is SAP's PEINH (price per N units); 0 is treated as 1.
func Extend(qty Qty, price Minor, priceUnit int64) Minor {
	if priceUnit <= 0 {
		priceUnit = 1
	}
	num := int64(qty) * int64(price)
	den := int64(QtyScale) * priceUnit
	return Minor(divRound(num, den))
}

// Percent returns amount * pct / 100 rounded half away from zero (pct 8 = 8%).
func Percent(amount Minor, pct int64) Minor {
	return Minor(divRound(int64(amount)*pct, 100))
}

func divRound(num, den int64) int64 {
	if den < 0 {
		num, den = -num, -den
	}
	if num >= 0 {
		return (num + den/2) / den
	}
	return -((-num + den/2) / den)
}

// Abs returns the absolute value.
func (m Minor) Abs() Minor {
	if m < 0 {
		return -m
	}
	return m
}
