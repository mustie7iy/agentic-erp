package money

import (
	"errors"
	"maps"
	"math"
	"testing"
)

func TestParseDecimal(t *testing.T) {
	cases := []struct {
		in   string
		dec  int
		want Minor
		err  bool
	}{
		{"1234.56", 2, 123456, false},
		{"-0.5", 2, -50, false},
		{"0.05", 2, 5, false},
		{"12", 2, 1200, false},
		{"123.45-", 2, -12345, false}, // SAP trailing minus
		{"1.230", 2, 123, false},      // trailing zero decimals are fine
		{"1.234", 2, 0, true},         // would lose a cent
		{"100", 0, 100, false},        // JPY
		{"", 2, 0, true},
		{"12a", 2, 0, true},
		{"--1", 2, 0, true},
		{"-1-", 2, 0, true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := ParseDecimal(c.in, c.dec)
			if (err != nil) != c.err {
				t.Errorf("ParseDecimal(%q,%d) error = %v, want error %v", c.in, c.dec, err, c.err)
			}
			if got != c.want {
				t.Errorf("ParseDecimal(%q,%d) = %d, want %d", c.in, c.dec, got, c.want)
			}
		})
	}
}

func TestParseQty(t *testing.T) {
	q, err := ParseQty("12.5")
	if err != nil || q != 12500 {
		t.Fatalf("got %d %v", q, err)
	}
	if q.String() != "12.5" {
		t.Errorf("String = %q", q.String())
	}
}

func TestExtendRoundsHalfAwayFromZero(t *testing.T) {
	// 1.5 units at 0.01 = 0.015 -> 0.02
	if got, _ := Extend(1500, 1, 1); got != 2 {
		t.Errorf("got %d", got)
	}
	if got, _ := Extend(-1500, 1, 1); got != -2 {
		t.Errorf("got %d", got)
	}
	// 2400 units at 12.34 per 100 units = 296.16
	if got, _ := Extend(2400*QtyScale, 1234, 100); got != 29616 {
		t.Errorf("price unit: got %d", got)
	}
}

// Products are exact: quantity × price and amount × percentage were formed
// in int64 and wrapped while the result was well in range.
func TestExtendAndPercentLarge(t *testing.T) {
	// 10,000,000 units at 10,000,000.00 is 100,000,000,000,000.00.
	if got, err := Extend(10_000_000*QtyScale, 1_000_000_000, 1); err != nil || got != 10_000_000_000_000_000 {
		t.Errorf("Extend: got %d, %v, want 10000000000000000", got, err)
	}
	if got, err := Percent(1_000_000_000_000_000_000, 50); err != nil || got != 500_000_000_000_000_000 {
		t.Errorf("Percent: got %d, %v, want 500000000000000000", got, err)
	}
	if _, err := Extend(2*QtyScale, math.MaxInt64, 1); !errors.Is(err, ErrOverflow) {
		t.Errorf("Extend out of range: got %v, want ErrOverflow", err)
	}
	if _, err := Percent(math.MaxInt64, 101); !errors.Is(err, ErrOverflow) {
		t.Errorf("Percent out of range: got %v, want ErrOverflow", err)
	}
}

func TestFormat(t *testing.T) {
	if s := Minor(-123456789).String(); s != "-1,234,567.89" {
		t.Errorf("got %s", s)
	}
	if s := Minor(5).Decimal(); s != "0.05" {
		t.Errorf("got %s", s)
	}
	if s := Minor(-5).Decimal(); s != "-0.05" {
		t.Errorf("got %s", s)
	}
}

func TestEventAmount(t *testing.T) {
	cases := map[string]struct {
		minor    Minor
		currency string
		decimals int
		want     map[string]string
	}{
		"two decimals, no separators": {-123456789, "EUR", 2, map[string]string{"amount": "-1234567.89", "currency": "EUR"}},
		"no decimals":                 {1500, "JPY", 0, map[string]string{"amount": "1500", "currency": "JPY"}},
		"three decimals":              {5, "KWD", 3, map[string]string{"amount": "0.005", "currency": "KWD"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := EventAmount(c.minor, c.currency, c.decimals); !maps.Equal(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}
