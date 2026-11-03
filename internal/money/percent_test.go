package money

import (
	"math"
	"testing"
)

// TestPercentOf: two places, exact, halves away from zero on either side,
// a negative zero without its sign, and the extremes of int64 without
// overflow.
func TestPercentOf(t *testing.T) {
	cases := map[string]struct {
		part, whole Minor
		want        string
	}{
		"whole":                    {100, 100, "100.00"},
		"a third":                  {1, 3, "33.33"},
		"two thirds":               {2, 3, "66.67"},
		"a half up":                {1, 8000, "0.01"},   // 0.0125
		"a half away below zero":   {-1, 8000, "-0.01"}, // -0.0125
		"below a half, negative":   {-1, 30000, "0.00"},
		"over a negative whole":    {50, -200, "-25.00"},
		"the largest over the one": {math.MaxInt64, 1, "922337203685477580700.00"},
		"the smallest over itself": {math.MinInt64, math.MinInt64, "100.00"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := PercentOf(c.part, c.whole); got != c.want {
				t.Errorf("PercentOf(%d, %d): got %s, want %s", c.part, c.whole, got, c.want)
			}
		})
	}
}

// TestPercentOfMagnitude: the percent of a whole's absolute value keeps
// the part's sign, the smallest int64 included.
func TestPercentOfMagnitude(t *testing.T) {
	cases := map[string]struct {
		part, whole Minor
		want        string
	}{
		"over a debit":             {50, 200, "25.00"},
		"over a credit":            {50, -200, "25.00"},
		"a decrease over a credit": {-50, -200, "-25.00"},
		"over the smallest":        {math.MinInt64, math.MinInt64, "-100.00"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := PercentOfMagnitude(c.part, c.whole); got != c.want {
				t.Errorf("PercentOfMagnitude(%d, %d): got %s, want %s", c.part, c.whole, got, c.want)
			}
		})
	}
}
