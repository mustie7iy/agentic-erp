package payables

import (
	"math/big"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// bhdInEUR converts BHD (3 digits) into EUR (2 digits) at par, so a BHD
// amount's EUR value has a third digit that rounding to cents would drop.
var bhdInEUR = ledger.Conversion{From: "BHD", To: "EUR", Rate: "1", FromExp: 3, ToExp: 2}

// The absolute price tolerance is compared with the variance's exact value
// in the company's currency, not its value rounded to cents (F13): a
// variance worth 5.004 EUR is beyond a 5.00 EUR tolerance. Before, it was
// rounded to 5.00 EUR and passed.
func TestToleranceComparedExactly(t *testing.T) {
	no := int32(1)
	lim := noLimits(t)
	lim.priceAbsolute, lim.absoluteText = big.NewRat(5, 1), "5.00"
	order := procurement.OrderItem{ItemNo: 1, UnitPrice: 100_000, PriceUnit: 1} // 100.000 BHD
	for _, c := range []struct {
		name   string
		amount money.Minor
		beyond bool
	}{
		{"at the tolerance", 105_000, false},
		{"5.004 above", 105_004, true},
		{"5.004 below", 94_996, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			why := priceVariance(Item{ItemNo: 1, POItemNo: &no, Quantity: "1", Amount: c.amount}, order, lim, bhdInEUR,
				func(m money.Minor) string { return m.Plain(3) })
			if got := why != ""; got != c.beyond {
				t.Errorf("beyond the tolerance: got %v (%q), want %v", got, why, c.beyond)
			}
		})
	}
}

// The auto-post limit is compared with the gross amount's exact value in
// the company's currency (F13): 50,000.004 EUR is above a 50,000.00 limit,
// although it shows as 50000.00 EUR in the reason. Before, the gross was
// rounded to 50,000.00 EUR and posted automatically.
func TestAutoPostLimitComparedExactly(t *testing.T) {
	lim := noLimits(t)
	lim.autoPost, lim.autoPostText = big.NewRat(50_000, 1), "50000.00"
	for gross, want := range map[money.Minor]string{50_000_000: DecisionAutoPost, 50_000_004: DecisionReview} {
		t.Run(gross.Plain(3), func(t *testing.T) {
			r := replayed{Invoice: Invoice{Exponent: 3, Gross: gross, Items: []Item{{ItemNo: 1, Amount: gross}}}, Order: procurement.Order{Status: "released"}}
			r.convert(bhdInEUR)
			if got, _ := decide(lim, r); got.Outcome != want {
				t.Errorf("decision: got %s (%s), want %s", got.Outcome, got.Reason, want)
			}
		})
	}
}
