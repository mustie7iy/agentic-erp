package ledger

import (
	"fmt"
	"math"
	"testing"

	"github.com/google/uuid"

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

// TestSplitTax: an entry owing nothing, or whose tax lines run against its
// payment, is not split; tax shares adding up beyond an amount are refused.
func TestSplitTax(t *testing.T) {
	v1 := taxKey{code: "V1"}
	for name, c := range map[string]taxedItem{
		"owing nothing":           {discount: 100, paid: 0, taxes: map[taxKey]money.Minor{v1: 19}},
		"tax against the payment": {discount: 100, paid: 119, taxes: map[taxKey]money.Minor{v1: -19}},
		"no tax lines":            {discount: 100, paid: 119},
	} {
		t.Run(name, func(t *testing.T) {
			shares, total, err := splitTax([]taxedItem{c})
			if err != nil || total != 0 || len(shares) != 0 {
				t.Errorf("got %v %d %v, want no tax share", shares, total, err)
			}
		})
	}
	const near = money.Minor(9_000_000_000_000_000_000)
	huge := taxedItem{discount: near, paid: near + 1, taxes: map[taxKey]money.Minor{v1: near}}
	if _, _, err := splitTax([]taxedItem{huge, huge}); err == nil {
		t.Error("tax shares beyond an amount: got none, want an error")
	}
}

// TestItemTaxShares: a credit memo's discount gives back tax of its sign,
// and tax lines on different accounts each take their share.
func TestItemTaxShares(t *testing.T) {
	a, b := taxKey{account: uuid.UUID{1}, code: "V1"}, taxKey{account: uuid.UUID{2}, code: "V1"}
	for name, c := range map[string]struct {
		it   taxedItem
		want string
	}{
		"a credit memo":           {taxedItem{discount: -238, paid: -11900, taxes: map[taxKey]money.Minor{a: -1900}}, "map[{[1 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0] V1}:-38 minor]"},
		"two accounts, remainder": {taxedItem{discount: 500, paid: 10012, taxes: map[taxKey]money.Minor{b: 5, a: 7}}, "map[{[1 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0] V1}:1 minor]"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := fmt.Sprint(itemTaxShares(c.it)); got != c.want {
				t.Errorf("shares: got %s, want %s", got, c.want)
			}
		})
	}
}
