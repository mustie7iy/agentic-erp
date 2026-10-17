package ledger

import (
	"math"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestSettlementDiscountsOutOfRange: discounts each in range whose total
// is not, or whose payment is not, are refused rather than wrapped.
func TestSettlementDiscountsOutOfRange(t *testing.T) {
	const near = money.Minor(6_000_000_000_000_000_000)
	items := []item{{tc: -near}, {tc: -near}, {tc: 9_000_000_000_000_000_000}}
	for name, c := range map[string]struct {
		discounts []money.Minor
		tc        money.Minor
	}{
		"discounts adding up beyond an amount": {[]money.Minor{near - 1, near - 1, -1}, -3_000_000_000_000_000_000},
		"a payment beyond an amount":           {[]money.Minor{1, 1, -8_999_999_999_999_999_999}, -9_000_000_000_000_000_000},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := settlementDiscount(Settlement{Discounts: c.discounts}, items, c.tc)
			if p := problem.From(err); p == nil || p.Code != "ledger.amount_too_large" {
				t.Errorf("got %v, want ledger.amount_too_large", err)
			}
		})
	}
	_, _, err := settlementDiscount(Settlement{Discounts: []money.Minor{math.MinInt64}}, []item{{tc: 5}}, 5)
	if p := problem.From(err); p == nil || p.Code != "ledger.invalid_discount" {
		t.Errorf("a discount of the smallest amount: got %v, want ledger.invalid_discount", err)
	}
}
