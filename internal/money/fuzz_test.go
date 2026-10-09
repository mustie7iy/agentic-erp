package money

import (
	"strings"
	"testing"
)

// Parsing never panics, and whatever parses formats back to the same value.
func FuzzParseDecimal(f *testing.F) {
	for _, s := range []string{"0", "1.5", "-12.34", "123.45-", "1,000.00", "", ".", "9999999999999999999", "1.239", "+7"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v, err := ParseDecimal(s, 2)
		if err != nil {
			return
		}
		back, err := ParseDecimal(v.Decimal(), 2)
		if err != nil || back != v {
			t.Fatalf("%q -> %d -> %q -> %d (%v)", s, v, v.Decimal(), back, err)
		}
		if q, err := ParseQty(s); err == nil && strings.Contains(s, "e") {
			t.Fatalf("exponent accepted: %q -> %d", s, q)
		}
	})
}
