package gtin_test

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/gtin"
)

// TestValid: a GTIN of each length with its check digit (computed by hand
// in the comments), a wrong check digit, other lengths and characters.
func TestValid(t *testing.T) {
	for in, want := range map[string]bool{
		"96385074":        true,  // 9·3+6+3·3+8+5·3+0+7·3 = 86, check 4
		"036000291452":    true,  // (0+6+0+2+1+5)·3 + (3+0+0+9+4) = 58, check 2
		"4006381333931":   true,  // 4+0·3+0+6·3+3+8·3+1+3·3+3+3·3+9+3·3 = 89, check 1
		"10036000291459":  true,  // 036000291452's item at packaging level 1: 61, check 9
		"0036000291452":   true,  // the GTIN-12 with a leading zero
		"4006381333932":   false, // check digit off by one
		"4006381333930":   false,
		"400638133393":    false, // twelve digits, its check digit wrong
		"1234567":         false, // seven digits
		"123456789012345": false,
		"":                false,
		"4006381333A31":   false,
		"400638133393A":   false,
		"40063813339 1":   false,
	} {
		t.Run(in, func(t *testing.T) {
			if got := gtin.Valid(in); got != want {
				t.Errorf("Valid(%q): got %v, want %v", in, got, want)
			}
		})
	}
}

// TestKey: every length right-justified in 14 digits, so the GTIN-12 and
// its GTIN-13 with a leading zero share a key.
func TestKey(t *testing.T) {
	for in, want := range map[string]string{
		"96385074":       "00000096385074",
		"036000291452":   "00036000291452",
		"0036000291452":  "00036000291452",
		"10036000291459": "10036000291459",
	} {
		t.Run(in, func(t *testing.T) {
			if got := gtin.Key(in); got != want {
				t.Errorf("Key(%q): got %q, want %q", in, got, want)
			}
		})
	}
}
