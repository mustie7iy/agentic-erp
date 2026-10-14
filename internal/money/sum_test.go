package money

import (
	"errors"
	"math"
	"testing"
)

func TestSum(t *testing.T) {
	const top, bottom = Minor(math.MaxInt64), Minor(math.MinInt64)
	cases := map[string]struct {
		in   []Minor
		want Minor
		err  error
	}{
		"none":                          {nil, 0, nil},
		"mixed signs":                   {[]Minor{150, -25, -125, 7}, 7, nil},
		"the largest":                   {[]Minor{top - 1, 1}, top, nil},
		"the smallest":                  {[]Minor{bottom + 1, -1}, bottom, nil},
		"nets to zero through overflow": {[]Minor{top, top, -top, -top}, 0, nil},
		"negatives first":               {[]Minor{bottom, bottom, top, top, 2}, 0, nil},
		"one over the largest":          {[]Minor{top, 1}, 0, ErrOverflow},
		"one under the smallest":        {[]Minor{bottom, -1}, 0, ErrOverflow},
		"wraps to exactly zero":         {[]Minor{top, top, 2}, 0, ErrOverflow},
		"wraps far below":               {[]Minor{bottom, bottom, bottom}, 0, ErrOverflow},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Sum(c.in...)
			if !errors.Is(err, c.err) {
				t.Fatalf("Sum(%v) error: got %v, want %v", c.in, err, c.err)
			}
			if got != c.want {
				t.Errorf("Sum(%v): got %d, want %d", c.in, got, c.want)
			}
		})
	}
}
