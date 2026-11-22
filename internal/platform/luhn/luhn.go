// Package luhn checks numbers with the Luhn (mod 10) algorithm: the
// payment card numbers the model gateway redacts (redact, ADR-0036) and
// the nine digits of a Canadian business number on a GST/HST registration
// (config, ADR-0049 §10, PO 2026-11-21).
package luhn

// Valid reports whether s, spaces and dashes allowed, is a number of at
// least one digit passing the Luhn check: from the right, every second
// digit doubled (less 9 when that exceeds 9) and the sum divisible by 10.
// Any other character fails it.
func Valid(s string) bool {
	sum, pos := 0, 0 // pos: the digit's place from the right, separators skipped
	for i := len(s) - 1; i >= 0; i-- {
		switch c := s[i]; {
		case c == ' ' || c == '-':
			continue
		case c < '0' || c > '9':
			return false
		}
		d := int(s[i] - '0')
		if pos%2 == 1 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum, pos = sum+d, pos+1
	}
	return pos > 0 && sum%10 == 0
}
