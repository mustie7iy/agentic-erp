package money

import "testing"

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
	if got := Extend(1500, 1, 1); got != 2 {
		t.Errorf("got %d", got)
	}
	if got := Extend(-1500, 1, 1); got != -2 {
		t.Errorf("got %d", got)
	}
	// 2400 units at 12.34 per 100 units = 296.16
	if got := Extend(2400*QtyScale, 1234, 100); got != 29616 {
		t.Errorf("price unit: got %d", got)
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
