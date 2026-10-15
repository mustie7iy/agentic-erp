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

// TestPositiveQuantity: what numeric(20,6) holds exactly and is above
// zero, in the plain grammar Scale reads; nothing big.Rat or ParseDecimal
// alone would read (a sign, space, a bare "." side).
func TestPositiveQuantity(t *testing.T) {
	for in, ok := range map[string]bool{
		"12.5": true, "0.000001": true, "99999999999999.999999": true, "007": true,
		"1.0000000": true, // the seventh decimal is a zero: nothing is rounded away
		"1/3":       false, "1e3": false, "0x10": false, "Inf": false, "": false, "abc": false,
		"0.1234567": false, "123456789012345": false, "0": false, "0.000000": false, "-1": false, "1-": false,
		"+3": false, ".5": false, "5.": false, " 5": false, "5.000 ": false,
	} {
		t.Run(in, func(t *testing.T) {
			err := PositiveQuantity(in)
			if got := err == nil; got != ok {
				t.Errorf("accepted: got %v (%v), want %v", got, err, ok)
			}
		})
	}
}

func TestExtendRoundsHalfAwayFromZero(t *testing.T) {
	// 1.5 units at 0.01 = 0.015 -> 0.02
	if got, err := Extend(1500, 1, 1); err != nil || got != 2 {
		t.Errorf("got %d, %v, want 2", got, err)
	}
	if got, err := Extend(-1500, 1, 1); err != nil || got != -2 {
		t.Errorf("got %d, %v, want -2", got, err)
	}
	// 2400 units at 12.34 per 100 units = 296.16
	if got, err := Extend(2400*QtyScale, 1234, 100); err != nil || got != 29616 {
		t.Errorf("price unit: got %d, %v, want 29616", got, err)
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

// TestRatAtCurrencyDigits: an amount as an exact rational in major units,
// what a figure read with ParseDecimalRat is compared with.
func TestRatAtCurrencyDigits(t *testing.T) {
	for _, c := range []struct {
		m    Minor
		exp  int
		want string
	}{{123456, 2, "1234.56"}, {-5, 3, "-0.005"}, {7, 0, "7"}, {math.MinInt64, 2, "-92233720368547758.08"}} {
		t.Run(c.want, func(t *testing.T) {
			if got := Rat(c.m, c.exp).FloatString(c.exp); got != c.want {
				t.Errorf("%d with %d digits: got %s, want %s", c.m, c.exp, got, c.want)
			}
		})
	}
}

func TestFormat(t *testing.T) {
	if s := Minor(-123456789).Format(2); s != "-1,234,567.89" {
		t.Errorf("got %s", s)
	}
	if s := Minor(-123456789).String(); s != "-123456789 minor" {
		t.Errorf("got %s", s)
	}
	if s := Minor(5).Plain(2); s != "0.05" {
		t.Errorf("got %s", s)
	}
	if s := Minor(-5).Plain(2); s != "-0.05" {
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

// TestParseDecimalRat: the grammar of ParseDecimal, read exactly at any
// number of decimals; what big.Rat would also accept (exponents,
// fractions, hex, underscores) is refused like ParseDecimal refuses it.
func TestParseDecimalRat(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"1000.125", "8001/8"},
		{" 1000.00 ", "1000"}, // surrounding space, as ParseDecimal trims it
		{"1000.00-", "-1000"},
		{"-0.5", "-1/2"},
		{"+.25", "1/4"},
		{"7", "7"},
		{"7.", "7"},
		{"0.0000001", "1/10000000"},
	} {
		t.Run(c.in, func(t *testing.T) {
			got, err := ParseDecimalRat(c.in)
			if err != nil {
				t.Fatalf("got %v, want %s", err, c.want)
			}
			if got.RatString() != c.want {
				t.Errorf("got %s, want %s", got.RatString(), c.want)
			}
		})
	}
	for _, in := range []string{"1e3", "1/3", "0x10", "1_000", "", "-", ".", "--1", "-1-", "1.2.3", "abc"} {
		t.Run("refused "+in, func(t *testing.T) {
			if got, err := ParseDecimalRat(in); err == nil {
				t.Errorf("got %v, want an error", got)
			}
		})
	}
}
