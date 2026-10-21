package money

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"testing"
)

func rats(s ...string) []*big.Rat {
	out := make([]*big.Rat, len(s))
	for i, v := range s {
		out[i], _ = new(big.Rat).SetString(v) // test literals
	}
	return out
}

func TestAllocate(t *testing.T) {
	for _, c := range []struct {
		name  string
		exact []*big.Rat
		want  string
	}{
		{"none", nil, "[]"},
		{"exact", rats("3", "4"), "[3 4]"},
		{"largest remainder", rats("166.65", "166.65", "166.7"), "[167 166 167]"},
		{"ties to the earlier", rats("1.3", "1.3", "1.3"), "[2 1 1]"},
		{"half away from zero", rats("0.25", "0.25"), "[1 0]"},
		{"negated", rats("-166.65", "-166.65", "-166.7"), "[-167 -166 -167]"},
		{"cancelling: in the first amount's direction", rats("1.95", "-1.95"), "[2 -2]"},
		{"cancelling, negated", rats("-1.95", "1.95"), "[-2 2]"},
		{"a line against the sum keeps its sign", rats("5.5", "-0.4"), "[5 0]"},
		{"zeros", rats("0", "0"), "[0 0]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := Allocate(c.exact)
			if err != nil {
				t.Fatal(err)
			}
			if s := fmt.Sprint(minors(got)); s != c.want {
				t.Errorf("got %s, want %s", s, c.want)
			}
		})
	}
}

func minors(m []Minor) []int64 {
	out := make([]int64, len(m))
	for i, v := range m {
		out[i] = int64(v)
	}
	return out
}

func TestAllocateOverflow(t *testing.T) {
	big1 := new(big.Rat).SetInt64(math.MaxInt64)
	for name, exact := range map[string][]*big.Rat{
		"sum":    {big1, big1},
		"amount": {new(big.Rat).Add(big1, big1), new(big.Rat).Neg(big1)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Allocate(exact); !errors.Is(err, ErrOverflow) {
				t.Errorf("got %v, want ErrOverflow", err)
			}
		})
	}
}
