package ledger

import (
	"errors"
	"math"
	"math/big"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// A valued amount, difference or total out of the bigint range refuses the
// revaluation rather than wrapping, and an item worth what it was booked at
// has no direction and no total.
func TestRevaluationOutOfRange(t *testing.T) {
	one, two := exchange{exact: big.NewRat(1, 1), shown: "1"}, exchange{exact: big.NewRat(2, 1), shown: "2"}
	for _, c := range []struct {
		name    string
		item    RevaluedItem
		x, toGC exchange
	}{
		{"valued", RevaluedItem{ExpTC: 2, AmountTC: math.MaxInt64/2 + 1}, two, one},
		{"difference", RevaluedItem{ExpTC: 2, AmountTC: math.MaxInt64, AmountLC: -1}, one, one},
		{"group value", RevaluedItem{ExpTC: 2, AmountTC: math.MaxInt64/2 + 1, AmountLC: 0}, one, two},
		{"group difference", RevaluedItem{ExpTC: 2, AmountTC: math.MaxInt64, AmountGC: -1}, one, one},
	} {
		t.Run(c.name, func(t *testing.T) {
			var p *problem.Problem
			if err := c.item.value(c.x, c.toGC, 2, 2); !errors.As(err, &p) || p.Code != "ledger.amount_too_large" {
				t.Errorf("value: got %v, want ledger.amount_too_large", err)
			}
		})
	}
	t.Run("total", func(t *testing.T) {
		big := RevaluedItem{AccountID: uuid.New(), CurrencyTC: "USD", ExpTC: 2, AmountTC: math.MaxInt64, DifferenceLC: 1}
		var p *problem.Problem
		if _, err := (Revaluation{Items: []RevaluedItem{big, big}}).Totals(); !errors.As(err, &p) || p.Code != "ledger.amount_too_large" {
			t.Errorf("totals: got %v, want ledger.amount_too_large", err)
		}
	})
	t.Run("no difference", func(t *testing.T) {
		it := RevaluedItem{ExpTC: 2, AmountTC: 100, AmountLC: 100, AmountGC: 100}
		if err := it.value(one, one, 2, 2); err != nil {
			t.Fatal(err)
		}
		if d := it.Direction(); d != "none" {
			t.Errorf("direction: got %s, want none", d)
		}
		if d := (RevaluedItem{DifferenceGC: 1}).Direction(); d != "gain" {
			t.Errorf("direction of a group difference alone: got %s, want gain", d)
		}
		totals, err := (Revaluation{Items: []RevaluedItem{it}}).Totals()
		if err != nil || len(totals) != 0 {
			t.Errorf("totals: got %v %v, want none", totals, err)
		}
	})
}
