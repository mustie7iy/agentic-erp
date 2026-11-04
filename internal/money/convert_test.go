package money

import (
	"errors"
	"fmt"
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
		"six-decimal quantity":             {100000, "0.0005", 1, 50, false, false},
		"product beyond int64, in range":   {1_000_000_000, "10000000", 1, 10_000_000_000_000_000, false, false},
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

func TestPlainQuantity(t *testing.T) {
	for in, want := range map[string]string{"12.500": "12.5", "-3": "-3", "0.000000": "0", "10": "10", "0.000001": "0.000001", "-0.25": "-0.25"} {
		t.Run(in, func(t *testing.T) {
			if got := PlainQuantity(ParseRat(in)); got != want {
				t.Errorf("PlainQuantity(%s): got %q, want %q", in, got, want)
			}
		})
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

// Exact is the converted value unrounded: 105,004 fils at par is 105.004
// euros, where Convert gives 105.00.
func TestExact(t *testing.T) {
	cases := map[string]struct {
		amount  Minor
		rate    string
		fromExp int
		want    string
		failed  bool
	}{
		"third digit kept":   {105_004, "1", 3, "26251/250", false},
		"negative":           {-5_004, "1", 3, "-1251/250", false},
		"at a rate":          {8_064_517, "0.0062", 0, "500000054/10000", false},
		"rate not a decimal": {1, "1e5", 2, "", true},
		"negative exponent":  {1, "1", -1, "", true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Exact(c.amount, c.rate, c.fromExp)
			if (err != nil) != c.failed {
				t.Fatalf("Exact(%d, %q, %d) error: got %v, want an error %v", c.amount, c.rate, c.fromExp, err, c.failed)
			}
			if err == nil && got.Cmp(ParseRat(c.want)) != 0 {
				t.Errorf("Exact(%d, %q, %d): got %s, want %s", c.amount, c.rate, c.fromExp, got.RatString(), c.want)
			}
		})
	}
}

// The At forms take the rate as an exact rational, so a rate kept the
// other way round is divided by rather than multiplied by a rounded
// inverse: 1/3 has no decimal form, and 1/3e12 rounded to 12 decimals is
// zero (F19). The rate is the caller's and is left as it was.
func TestConvertAtARational(t *testing.T) {
	third := big.NewRat(1, 3)
	cases := map[string]struct {
		amount         Minor
		rate           *big.Rat
		fromExp, toExp int
		want           Minor
	}{
		"a third of 1.00":           {100, third, 2, 2, 33},
		"two thirds of 0.01":        {2, third, 2, 2, 1},
		"a third, large":            {300_000_000_000_002, third, 2, 2, 100_000_000_000_001},
		"below 12 decimals":         {3_000_000_000_000, big.NewRat(1, 3_000_000_000_000), 0, 2, 100},
		"negative, away from zero":  {-1, big.NewRat(3, 2), 2, 2, -2},
		"zero digits to two digits": {1000, big.NewRat(62, 10_000), 0, 2, 620},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			before := new(big.Rat).Set(c.rate)
			got, err := ConvertAt(c.amount, c.rate, c.fromExp, c.toExp)
			if err != nil {
				t.Fatalf("ConvertAt: got %v, want no error", err)
			}
			if got != c.want {
				t.Errorf("ConvertAt(%d, %s): got %d, want %d", c.amount, c.rate.RatString(), got, c.want)
			}
			if c.rate.Cmp(before) != 0 {
				t.Errorf("rate after ConvertAt: got %s, want %s", c.rate.RatString(), before.RatString())
			}
		})
	}
	exact, err := ExactAt(100, third, 2)
	if err != nil {
		t.Fatalf("ExactAt: got %v, want no error", err)
	}
	if exact.Cmp(third) != 0 {
		t.Errorf("ExactAt(100, 1/3, 2): got %s, want 1/3", exact.RatString())
	}
	if third.Cmp(big.NewRat(1, 3)) != 0 {
		t.Errorf("rate after ExactAt: got %s, want 1/3", third.RatString())
	}
	all, err := ConvertAllAt([]Minor{100, 100, -200}, third, 2, 2)
	if err != nil {
		t.Fatalf("ConvertAllAt: got %v, want no error", err)
	}
	if want := []Minor{33, 33, -66}; !slices.Equal(all, want) {
		t.Errorf("ConvertAllAt: got %v, want %v", all, want)
	}
	for name, r := range map[string]*big.Rat{"nil": nil, "negative": big.NewRat(-1, 3)} {
		t.Run(name+" rate", func(t *testing.T) {
			if _, err := ConvertAt(1, r, 2, 2); err == nil || errors.Is(err, ErrOverflow) {
				t.Errorf("ConvertAt: got %v, want an error other than overflow", err)
			}
			if _, err := ExactAt(1, r, 2); err == nil {
				t.Errorf("ExactAt: got %v, want an error", err)
			}
			if _, err := ConvertAllAt([]Minor{1, -1}, r, 2, 2); err == nil || errors.Is(err, ErrOverflow) {
				t.Errorf("ConvertAllAt: got %v, want an error other than overflow", err)
			}
		})
	}
}

// TestDivide: Divide rounds as Scale(a, "1", n) does, half away from zero,
// on both signs and at the ends of the range, and refuses what Scale
// refuses.
func TestDivide(t *testing.T) {
	amounts := []Minor{0, 1, -1, 2, -2, 5, -5, 7, -7, 999, -999, 1000000, -1000000, 1199999, -1199999, math.MaxInt64, math.MinInt64,
		math.MaxInt64 - 1, math.MinInt64 + 1}
	for _, a := range amounts {
		for _, n := range []int64{-1, 0, 1, 2, 3, 4, 7, 120, 599, 1200, math.MaxInt64} {
			t.Run(fmt.Sprintf("%d/%d", a, n), func(t *testing.T) {
				want, wantErr := Scale(a, "1", n)
				got, err := Divide(a, n)
				if (err != nil) != (wantErr != nil) || got != want {
					t.Errorf("got %d %v, want %d %v", got, err, want, wantErr)
				}
			})
		}
	}
}
