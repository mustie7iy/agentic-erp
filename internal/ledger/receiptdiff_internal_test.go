package ledger

import (
	"math"
	"math/big"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// TestApplicationAmountsOutOfRange: a share or a total out of the range
// of an amount is refused as too large, never wrapped.
func TestApplicationAmountsOutOfRange(t *testing.T) {
	if _, err := prorate(math.MaxInt64, 3, 2); err == nil {
		t.Error("prorate: got no error, want the amount too large")
	}
	if _, _, _, err := lineTotals([]Line{{TC: math.MaxInt64}, {TC: math.MaxInt64}}); err == nil {
		t.Error("lineTotals: got no error, want the total too large")
	}
	if _, _, err := receiptRate(item{tc: -1, lc: -math.MaxInt64 / 2}, 3); err == nil {
		t.Error("receiptRate in local currency: got no error, want the amount too large")
	}
	if _, _, err := receiptRate(item{tc: -1, lc: -1, gc: -math.MaxInt64 / 2}, 3); err == nil {
		t.Error("receiptRate in group currency: got no error, want the amount too large")
	}
}

// TestWithinTolerance: a difference is within a limit when both its
// amount and its percent of the items applied allow it.
func TestWithinTolerance(t *testing.T) {
	limit := config.ToleranceLimit{Amount: 500, Percent: big.NewRat(1, 1)}
	for _, c := range []struct {
		name        string
		limit       config.ToleranceLimit
		d, lc, base money.Minor
		want        bool
	}{
		{"within both", limit, 5, 5, 10000, true},
		{"over the percent", limit, 101, 101, 10000, false},
		{"over the amount", limit, 600, 600, 1000000, false},
		{"no items applied", limit, 5, 5, 0, false},
		{"no amount", config.ToleranceLimit{Percent: big.NewRat(1, 1)}, 5, 5, 10000, false},
		{"no percent", config.ToleranceLimit{Amount: 500}, 5, 5, 10000, false},
		{"an overpayment", limit, -5, -5, 10000, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := withinTolerance(c.limit, c.d, c.lc, c.base); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}
