package payables

import (
	"math"
	"slices"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// The match flags amounts whose sums are out of range instead of comparing
// wrapped values. Before, items of 92233720368547758.07 twice and 0.02
// summed to zero and matched a zero gross amount; an order price whose
// extension was out of range was truncated, and an invoice item at the
// truncated amount passed without a variance.
func TestMatchOutOfRange(t *testing.T) {
	top := money.Minor(math.MaxInt64)
	one, two, three := int32(1), int32(2), int32(3)
	order := procurement.Order{Status: "released", Items: []procurement.OrderItem{
		{ItemNo: 1, Quantity: "2", UnitPrice: top, PriceUnit: 1, Received: "2"},
		{ItemNo: 2, Quantity: "1", UnitPrice: top, PriceUnit: 1, Received: "1"},
		{ItemNo: 3, Quantity: "1", UnitPrice: top / 2, PriceUnit: 1, Received: "1"},
	}}
	cases := map[string]struct {
		items      []Item
		gross, tax money.Minor
		tolerance  config.Tolerance
		want       string
	}{
		"items wrap to the gross amount": {[]Item{{ItemNo: 1, Amount: top}, {ItemNo: 2, Amount: top}, {ItemNo: 3, Amount: 2}}, 0, 0,
			config.Tolerance{}, IssueTotalMismatch},
		"items plus tax out of range": {[]Item{{ItemNo: 1, Amount: top}}, top, 1, config.Tolerance{}, IssueTotalMismatch},
		// 2 × (2^63-1) truncated to int64 is -2.
		"extended price out of range": {[]Item{{ItemNo: 1, POItemNo: &one, Quantity: "2", Amount: -2}}, -2, 0, config.Tolerance{}, IssuePriceVariance},
		"variance out of range":       {[]Item{{ItemNo: 1, POItemNo: &two, Quantity: "1", Amount: -2}}, -2, 0, config.Tolerance{}, IssuePriceVariance},
		"percentage out of range": {[]Item{{ItemNo: 1, POItemNo: &three, Quantity: "1", Amount: 0}}, 0, 0,
			config.Tolerance{PricePercent: 300}, IssuePriceVariance},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			v := Invoice{Exponent: 2, Items: c.items, Gross: c.gross, Tax: c.tax}
			var codes []string
			for _, is := range evaluate(v, order, map[int32]string{}, c.tolerance) {
				codes = append(codes, is.Code)
			}
			if !slices.Contains(codes, c.want) {
				t.Errorf("issues: got %v, want %s", codes, c.want)
			}
		})
	}
	// Items plus tax are summed exactly: items out of range alone, with a
	// credit tax bringing the total in range, match their gross amount.
	v := Invoice{Exponent: 2, Items: []Item{{ItemNo: 1, Amount: top}, {ItemNo: 2, Amount: 1}}, Tax: -1, Gross: top}
	if why := totalMismatch(v, money.Minor.String); why != "" {
		t.Errorf("exact total: got %q, want a match", why)
	}
}
