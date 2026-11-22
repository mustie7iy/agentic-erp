package luhn_test

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/luhn"
)

// TestValid: numbers passing and failing the check, separators skipped,
// other characters and an empty number refused.
func TestValid(t *testing.T) {
	for in, want := range map[string]bool{
		"4111111111111111":    true, // the card schemes' published test number
		"4111 1111 1111 1111": true,
		"4111-1111-1111-1111": true,
		"4111111111111112":    false,
		"123456782":           true, // a synthesized nine-digit business number
		"123456789":           false,
		"0":                   true,
		"18":                  true, // 1 doubled is 2, and 2 + 8 = 10
		"59":                  true, // 5 doubled is 10, less 9 is 1, and 1 + 9 = 10
		"":                    false,
		" - ":                 false,
		"12345678A":           false,
		"4111.1111.1111.1111": false,
	} {
		t.Run(in, func(t *testing.T) {
			if got := luhn.Valid(in); got != want {
				t.Errorf("Valid(%q): got %v, want %v", in, got, want)
			}
		})
	}
}

// FuzzValid: a check digit computed for any payload passes, and changing
// the check digit fails it.
func FuzzValid(f *testing.F) {
	f.Add("12345678")
	f.Add("411111111111111")
	f.Fuzz(func(t *testing.T, payload string) {
		for i := 0; i < len(payload); i++ {
			if payload[i] < '0' || payload[i] > '9' {
				t.Skip("digits only")
			}
		}
		for d := byte('0'); d <= '9'; d++ {
			if !luhn.Valid(payload + string(d)) {
				continue
			}
			for o := byte('0'); o <= '9'; o++ {
				if o != d && luhn.Valid(payload+string(o)) {
					t.Fatalf("%s: check digits %c and %c both pass, want exactly one", payload, d, o)
				}
			}
			return
		}
		t.Fatalf("%s: no check digit passes, want exactly one", payload)
	})
}
