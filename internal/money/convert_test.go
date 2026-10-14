package money

import (
	"errors"
	"math"
	"math/big"
	"slices"
	"testing"
)

func TestConvert(t *testing.T) {
	cases := map[string]struct {
		amount           Minor
		rate             string
		fromExp, toExp   int
		want             Minor
		overflow, failed bool
	}{
		"half rounds away from zero":           {1, "1.5", 2, 2, 2, false, false},
		"negative half rounds away from zero":  {-1, "1.5", 2, 2, -2, false, false},
		"below half rounds toward zero":        {1, "0.4999", 2, 2, 0, false, false},
		"negative below half":                  {-1, "0.4999", 2, 2, 0, false, false},
		"exactly half":                         {1, "0.5", 2, 2, 1, false, false},
		"negative exactly half":                {-1, "0.5", 2, 2, -1, false, false},
		"JPY to EUR adds digits":               {1000, "0.0062", 0, 2, 620, false, false},
		"EUR to JPY drops digits":              {100, "161.5", 2, 0, 162, false, false},
		"EUR to JPY negative":                  {-100, "161.5", 2, 0, -162, false, false},
		"KWD to EUR":                           {1000, "3", 3, 2, 300, false, false},
		"KWD half a cent":                      {5, "1", 3, 2, 1, false, false},
		"KWD minus half a cent":                {-5, "1", 3, 2, -1, false, false},
		"rate with trailing zeros":             {12345, "1.000000000000", 2, 2, 12345, false, false},
		"zero":                                 {0, "1.2", 2, 0, 0, false, false},
		"most negative int64 is not an amount": {math.MinInt64, "1", 2, 2, 0, true, false},
		"most negative amount at par":          {-math.MaxInt64, "1", 2, 2, -math.MaxInt64, false, false},
		"over the largest":                     {math.MaxInt64, "2", 2, 2, 0, true, false},
		"under the smallest":                   {math.MinInt64, "1", 2, 3, 0, true, false},
		"empty rate":                           {1, "", 2, 2, 0, false, true},
		"rate with an exponent":                {1, "1e5", 2, 2, 0, false, true},
		"fraction rate":                        {1, "1/3", 2, 2, 0, false, true},
		"negative rate":                        {1, "-1", 2, 2, 0, false, true},
		"rate ending in a dot":                 {1, "1.", 2, 2, 0, false, true},
		"rate starting with a dot":             {1, ".5", 2, 2, 0, false, true},
		"negative exponent":                    {1, "1", -1, 2, 0, false, true},
		"exponent too large":                   {1, "1", 2, 19, 0, false, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Convert(c.amount, c.rate, c.fromExp, c.toExp)
			if errors.Is(err, ErrOverflow) != c.overflow {
				t.Errorf("Convert(%d, %q, %d, %d) overflow: got %v, want %v", c.amount, c.rate, c.fromExp, c.toExp, err, c.overflow)
			}
			if failed := err != nil && !errors.Is(err, ErrOverflow); failed != c.failed {
				t.Errorf("Convert(%d, %q, %d, %d) invalid input: got %v, want an error %v", c.amount, c.rate, c.fromExp, c.toExp, err, c.failed)
			}
			if got != c.want {
				t.Errorf("Convert(%d, %q, %d, %d): got %d, want %d", c.amount, c.rate, c.fromExp, c.toExp, got, c.want)
			}
		})
	}
}

func TestConvertAll(t *testing.T) {
	const top = math.MaxInt64
	cases := map[string]struct {
		in             []Minor
		rate           string
		fromExp, toExp int
		want           []Minor
		overflow       bool
	}{
		"none":     {nil, "1.5", 2, 2, []Minor{}, false},
		"balanced": {[]Minor{1, 1, -2}, "1.5", 2, 2, []Minor{2, 2, -4}, false},
		// Each 0.5 rounds away from zero; the total, 1.5, rounds to 2.
		"unbalanced": {[]Minor{1, 1, 1}, "0.5", 2, 2, []Minor{0, 1, 1}, false},
		"first of equally large takes the difference": {[]Minor{-1, 1, 1, -1}, "0.5", 2, 2, []Minor{-1, 1, 1, -1}, false},
		"JPY to EUR":                          {[]Minor{333, 333, -666}, "0.0062", 0, 2, []Minor{206, 206, -412}, false},
		"others out of range on their own":    {[]Minor{-top, top, top}, "1", 2, 2, []Minor{-top, top, top}, false},
		"adjusted to the most negative int64": {[]Minor{-top, -1}, "1", 2, 2, nil, true},
		"total out of range":                  {[]Minor{top, 1}, "1", 2, 2, nil, true},
		"converted total out of range":        {[]Minor{top / 2, top / 2}, "3", 2, 2, nil, true},
		"an amount out of range":              {[]Minor{top, -top}, "2", 2, 2, nil, true},
		// 6148914691236517204 × 1.5 is 2^63-2; the others' rounding adds 1.5.
		"adjusted largest out of range": {[]Minor{6148914691236517204, -1, -1, -1}, "1.5", 2, 2, nil, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ConvertAll(c.in, c.rate, c.fromExp, c.toExp)
			if errors.Is(err, ErrOverflow) != c.overflow {
				t.Fatalf("ConvertAll(%v) error: got %v, want overflow %v", c.in, err, c.overflow)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("ConvertAll(%v): got %v, want %v", c.in, got, c.want)
			}
		})
	}
	if _, err := ConvertAll([]Minor{1, -1}, "x", 2, 2); err == nil || errors.Is(err, ErrOverflow) {
		t.Errorf("invalid rate: got %v, want an error other than overflow", err)
	}
}

func TestScale(t *testing.T) {
	cases := map[string]struct {
		amount           Minor
		factor           string
		divisor          int64
		want             Minor
		overflow, failed bool
	}{
		"quantity times price":             {1234, "2400", 100, 29616, false, false},
		"fractional quantity":              {400, "2.5", 2, 500, false, false},
		"half rounds away from zero":       {1, "1.5", 1, 2, false, false},
		"negative half rounds away":        {-1, "1.5", 1, -2, false, false},
		"negative factor":                  {1, "-1.5", 1, -2, false, false},
		"below half":                       {1, "0.001", 3, 0, false, false},
		"largest":                          {math.MaxInt64, "1", 1, math.MaxInt64, false, false},
		"over the largest":                 {math.MaxInt64, "2", 1, 0, true, false},
		"under the smallest":               {math.MinInt64, "1.000001", 1, 0, true, false},
		"exponent":                         {1, "1e3", 1, 0, false, true},
		"two signs":                        {1, "--1", 1, 0, false, true},
		"zero divisor":                     {1, "1", 0, 0, false, true},
		"negative divisor":                 {1, "1", -2, 0, false, true},
		"large quantity, in range divided": {math.MaxInt64, "1000", 1000, math.MaxInt64, false, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Scale(c.amount, c.factor, c.divisor)
			if errors.Is(err, ErrOverflow) != c.overflow {
				t.Errorf("Scale(%d, %q, %d) overflow: got %v, want %v", c.amount, c.factor, c.divisor, err, c.overflow)
			}
			if failed := err != nil && !errors.Is(err, ErrOverflow); failed != c.failed {
				t.Errorf("Scale(%d, %q, %d) invalid input: got %v, want an error %v", c.amount, c.factor, c.divisor, err, c.failed)
			}
			if got != c.want {
				t.Errorf("Scale(%d, %q, %d): got %d, want %d", c.amount, c.factor, c.divisor, got, c.want)
			}
		})
	}
}

func TestRound(t *testing.T) {
	cases := map[string]struct {
		num, den         int64
		exp              int
		want             Minor
		overflow, failed bool
	}{
		"to three digits":           {1234567, 1000, 3, 1234567, false, false},
		"half up, away from zero":   {5, 2, 0, 3, false, false},
		"half down, away from zero": {-5, 2, 0, -3, false, false},
		"a third at four digits":    {1, 3, 4, 3333, false, false},
		"out of range":              {math.MaxInt64, 1, 2, 0, true, false},
		"negative exponent":         {1, 1, -1, 0, false, true},
		"exponent too large":        {1, 1, 19, 0, false, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Round(big.NewRat(c.num, c.den), c.exp)
			if errors.Is(err, ErrOverflow) != c.overflow {
				t.Errorf("overflow: got %v, want %v", err, c.overflow)
			}
			if failed := err != nil && !errors.Is(err, ErrOverflow); failed != c.failed {
				t.Errorf("invalid exponent: got %v, want an error %v", err, c.failed)
			}
			if got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}

func TestParseRat(t *testing.T) {
	if got := ParseRat("12.500"); got == nil || got.Cmp(big.NewRat(25, 2)) != 0 {
		t.Errorf("ParseRat(12.500): got %v, want 25/2", got)
	}
	if got := ParseRat("x"); got != nil {
		t.Errorf("ParseRat(x): got %v, want nil", got)
	}
}

func TestRat(t *testing.T) {
	cases := map[string]struct {
		m    Minor
		exp  int
		want string
	}{
		"BHD, three digits":         {123456, 3, "15432/125"},
		"EUR, two digits":           {-1050, 2, "-21/2"},
		"JPY, none":                 {5000, 0, "5000/1"},
		"zero":                      {0, 4, "0/1"},
		"the largest amount":        {math.MaxInt64, 18, "9223372036854775807/1000000000000000000"},
		"a negative exponent":       {5, -2, "500/1"},
		"beyond any float's digits": {math.MaxInt64 - 1, 0, "9223372036854775806/1"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := Rat(c.m, c.exp).String(); got != c.want {
				t.Errorf("Rat(%d, %d): got %s, want %s", c.m, c.exp, got, c.want)
			}
		})
	}
}
