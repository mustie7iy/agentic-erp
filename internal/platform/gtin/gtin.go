// Package gtin checks GS1 Global Trade Item Numbers (GTIN-8, GTIN-12,
// GTIN-13 and GTIN-14; SAP's EAN/UPC, MARA-EAN11 and MARM-EAN11, from
// memory), which a material's units of measure carry (materials, ADR-0082
// As built S1a3). The check digit is GS1's mod 10 (GS1 General
// Specifications §7.9, from memory): from the right, the digits before
// the check digit weighted 3, 1, 3, …, and the check digit the sum's
// complement to the next multiple of ten.
package gtin

// Lengths a GTIN has: GTIN-8, -12, -13 and -14.
var lengths = map[int]bool{8: true, 12: true, 13: true, 14: true}

// Valid reports whether s is a GTIN: 8, 12, 13 or 14 digits, nothing
// else, with its check digit right.
func Valid(s string) bool {
	if !lengths[len(s)] {
		return false
	}
	sum := 0
	for i := len(s) - 2; i >= 0; i-- {
		c := s[i]
		if c < '0' || c > '9' {
			return false
		}
		d := int(c - '0')
		if (len(s)-2-i)%2 == 0 { // the digit next to the check digit weighs 3
			d *= 3
		}
		sum += d
	}
	check := s[len(s)-1]
	return check >= '0' && check <= '9' && int(check-'0') == (10-sum%10)%10
}

// Key is a valid GTIN as GS1 compares them: right-justified in 14 digits,
// so a GTIN-12 and the GTIN-13 of one leading zero more are one number.
func Key(s string) string {
	const zeros = "00000000000000"
	return zeros[:14-len(s)] + s
}
