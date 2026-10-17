package money_test

import (
	"errors"
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
	if _, err := money.TokenCost(math.MaxInt64/2, 400*1_000_000); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("overflow: got %v, want ErrOverflow", err)
	}
	if _, err := money.TokenCost(-1, 1); err == nil {
		t.Error("negative tokens: got a cost, want an error")
	}
}

// TestMicroMinor: millionths show as minor units rounded half up.
func TestMicroMinor(t *testing.T) {
	for _, c := range []struct {
		micro money.Micro
		want  money.Minor
	}{{0, 0}, {499_999, 0}, {500_000, 1}, {1_499_999, 1}, {1_500_000, 2}, {123_000_000, 123}} {
		if got := c.micro.Minor(); got != c.want {
			t.Errorf("%d micro: got %d minor, want %d", c.micro, got, c.want)
		}
	}
}
