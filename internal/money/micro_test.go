package money_test

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// TestTokenCost: a call's cost is its tokens at a price per million tokens,
// in millionths of the minor unit, rounded up at that millionth: exact for
// any whole-minor price, never under the price for fractional ones (ADR-0005
// as amended, ADR-0036).
func TestTokenCost(t *testing.T) {
	for _, c := range []struct {
		name   string
		tokens int64
		price  money.Micro // per million tokens
		want   money.Micro
	}{
		{"opus input, 1,234 tokens at $4.00", 1234, 400 * 1_000_000, 1234 * 400},
		{"a fractional price rounds up", 3, 12_500_000, 38}, // 3 × 12.5 = 37.5
		{"no tokens", 0, 400 * 1_000_000, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := money.TokenCost(c.tokens, c.price)
			if err != nil || got != c.want {
				t.Errorf("got %d, %v, want %d", got, err, c.want)
			}
		})
	}
	t.Run("out of range", func(t *testing.T) {
		if _, err := money.TokenCost(math.MaxInt64/2, 400*1_000_000); !errors.Is(err, money.ErrOverflow) {
			t.Errorf("got %v, want ErrOverflow", err)
		}
	})
	t.Run("negative tokens", func(t *testing.T) {
		if _, err := money.TokenCost(-1, 1); err == nil {
			t.Error("got a cost, want an error")
		}
	})
}

// TestMicroMinor: millionths show as minor units rounded half up, near
// the most an int64 holds too.
func TestMicroMinor(t *testing.T) {
	for _, c := range []struct {
		micro money.Micro
		want  money.Minor
	}{{0, 0}, {499_999, 0}, {500_000, 1}, {1_499_999, 1}, {1_500_000, 2}, {123_000_000, 123}, {math.MaxInt64, 9_223_372_036_855}} {
		t.Run(fmt.Sprint(int64(c.micro)), func(t *testing.T) {
			if got := c.micro.Minor(); got != c.want {
				t.Errorf("got %d minor, want %d", got, c.want)
			}
		})
	}
}

// TestMicroAdd: costs add up exactly, held at the most an int64 holds
// rather than wrapped.
func TestMicroAdd(t *testing.T) {
	for _, c := range []struct {
		name       string
		a, b, want money.Micro
	}{{"exact", 4400, 5000, 9400}, {"held at the most", math.MaxInt64 - 1, 2, math.MaxInt64}} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.a.Add(c.b); got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}

// TestMicroOf: a budget in minor units is the same amount in Micro, held
// at the most an int64 holds (ADR-0036).
func TestMicroOf(t *testing.T) {
	for _, c := range []struct {
		name  string
		minor money.Minor
		want  money.Micro
	}{{"a hundred dollars", 10000, 10_000_000_000}, {"nothing", 0, 0}, {"held at the most", math.MaxInt64 / 1000, math.MaxInt64}} {
		t.Run(c.name, func(t *testing.T) {
			if got := money.MicroOf(c.minor); got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}

// TestMicroReaches: whether spent has reached pct percent of a cap, exact
// at the edge and at the largest amounts.
func TestMicroReaches(t *testing.T) {
	for _, c := range []struct {
		name         string
		spent, limit money.Micro
		pct          int64
		want         bool
	}{
		{"just under 80%", 799, 1000, 80, false},
		{"at 80%", 800, 1000, 80, true},
		{"past 100%", 1001, 1000, 100, true},
		{"at the largest amounts", math.MaxInt64, math.MaxInt64, 100, true},
		{"just under at the largest", math.MaxInt64 - 1, math.MaxInt64, 100, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.spent.Reaches(c.limit, c.pct); got != c.want {
				t.Errorf("%d of %d at %d%%: got %v, want %v", c.spent, c.limit, c.pct, got, c.want)
			}
		})
	}
}
