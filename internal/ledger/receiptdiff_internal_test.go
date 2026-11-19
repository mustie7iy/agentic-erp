package ledger

import (
	"context"
	"math"
	"math/big"
	"testing"

	"github.com/google/uuid"

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
	for name, c := range map[string]struct {
		a       application
		receipt item
		d       money.Minor
	}{
		"the items applied":                       {application{}, item{tc: -math.MaxInt64}, math.MaxInt64},
		"the difference and what was written off": {application{writtenOff: math.MaxInt64}, item{tc: -1}, 1},
		"the difference at the receipt's rate":    {application{}, item{tc: -1, lc: -math.MaxInt64 / 2}, 3},
	} {
		t.Run("withinTolerance: "+name, func(t *testing.T) {
			if err := c.a.withinTolerance(context.Background(), nil, ReceiptApplication{}, c.receipt, c.d, false); err == nil {
				t.Error("got no error, want the amounts too large")
			}
		})
	}
	t.Run("residuals: a write-off at the receipt's rate", func(t *testing.T) {
		a := application{residualLines: make([]int32, 1), writeOffLines: make([]int32, 1)}
		in := ReceiptApplication{Residuals: []Residual{{Amount: 3, WriteOff: uuid.New()}}}
		if err := a.residuals(in, item{tc: -1, lc: -math.MaxInt64 / 2}, []item{{tc: 10}}); err == nil {
			t.Error("got no error, want the write-off's local amount too large")
		}
	})
	t.Run("residuals: what is written off", func(t *testing.T) {
		a := application{residualLines: make([]int32, 2), writeOffLines: make([]int32, 2)}
		in := ReceiptApplication{Residuals: []Residual{{Amount: math.MaxInt64 - 1, WriteOff: uuid.New()}, {Amount: math.MaxInt64 - 1, WriteOff: uuid.New()}}}
		if err := a.residuals(in, item{tc: -1}, []item{{tc: math.MaxInt64}, {tc: math.MaxInt64}}); err == nil {
			t.Error("got no error, want the total written off too large")
		}
	})
	for name, c := range map[string]struct {
		receipt item
		offsets []Line
		in      ReceiptApplication
	}{
		"the offsets":                          {item{tc: -1}, []Line{{TC: math.MaxInt64}, {TC: math.MaxInt64}}, ReceiptApplication{}},
		"the difference at the receipt's rate": {item{tc: -1, lc: -math.MaxInt64 / 2}, []Line{{TC: 1}, {TC: -4}}, ReceiptApplication{}},
		"what is left on account":              {item{tc: -4, lc: 8}, []Line{{TC: 4, LC: math.MaxInt64 - 1}, {TC: -1}}, ReceiptApplication{OnAccount: uuid.New()}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := applicationLines(context.Background(), nil, c.in, uuid.Nil, []item{c.receipt, {}}, c.offsets); err == nil {
				t.Error("got no error, want the amounts too large")
			}
		})
	}
	if _, _, err := receiptRate(item{tc: -1, lc: -math.MaxInt64 / 2}, 3); err == nil {
		t.Error("receiptRate in local currency: got no error, want the amount too large")
	}
	if _, _, err := receiptRate(item{tc: -1, lc: -1, gc: -math.MaxInt64 / 2}, 3); err == nil {
		t.Error("receiptRate in group currency: got no error, want the amount too large")
	}
}

// TestWithinLimit: a difference is within a limit when both its
// amount and its percent of the items applied allow it.
func TestWithinLimit(t *testing.T) {
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
			if got := withinLimit(c.limit, c.d, c.lc, c.base); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}
