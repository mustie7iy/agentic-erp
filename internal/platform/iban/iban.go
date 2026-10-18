// Package iban checks International Bank Account Numbers (ISO 13616): the
// bank files' accounts (bankfile), the partners' and the house banks', and
// the text the model gateway redacts before it leaves the cell (redact,
// ADR-0036).
package iban

import (
	"fmt"
	"math/big"
	"strings"
)

// Valid checks an IBAN, spaces allowed, with the ISO 13616 mod-97
// algorithm.
func Valid(iban string) bool {
	s := strings.ToUpper(strings.ReplaceAll(iban, " ", ""))
	if len(s) < 15 || len(s) > 34 || s[0] < 'A' || s[0] > 'Z' || s[1] < 'A' || s[1] > 'Z' {
		return false
	}
	var digits strings.Builder
	for _, r := range s[4:] + s[:4] {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			fmt.Fprintf(&digits, "%d", r-'A'+10)
		default:
			return false
		}
	}
	n, _ := new(big.Int).SetString(digits.String(), 10) // only digits were written, at least 15 (the length check above)
	return new(big.Int).Mod(n, big.NewInt(97)).Int64() == 1
}
