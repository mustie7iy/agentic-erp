package ledger

import (
	"context"
	"math"
	"math/big"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// one is a rate of 1, two a rate of 2.
var one, two = exchange{exact: big.NewRat(1, 1), shown: "1"}, exchange{exact: big.NewRat(2, 1), shown: "2"}

func zero() *position { return &position{qty: new(big.Rat)} }

// TestGRIRMoveOutOfRange: a position moved out of the range of an amount,
// in transaction, local or group currency, is too large to post, and a
// quantity that is not a plain decimal is an internal error.
func TestGRIRMoveOutOfRange(t *testing.T) {
	full := &position{qty: big.NewRat(-1, 1), tc: math.MaxInt64}
	for name, c := range map[string]struct {
		r    rates
		p    *position
		it   GRIRItem
		code string
	}{
		"quantity":          {rates{toLC: one, toGC: one}, zero(), GRIRItem{Quantity: "x", UnitPrice: 1, PriceUnit: 1}, ""},
		"order value":       {rates{toLC: one, toGC: one}, zero(), GRIRItem{Quantity: "-10", UnitPrice: math.MaxInt64, PriceUnit: 1}, "ledger.amount_too_large"},
		"line":              {rates{toLC: one, toGC: one}, full, GRIRItem{Quantity: "-8", UnitPrice: 1_000_000_000_000_000_000, PriceUnit: 1}, "ledger.amount_too_large"},
		"local value":       {rates{toLC: two, toGC: one}, zero(), GRIRItem{Quantity: "-1", UnitPrice: 6_000_000_000_000_000_000, PriceUnit: 1}, "ledger.amount_too_large"},
		"local value grown": {rates{toLC: two, toGC: one}, &position{qty: big.NewRat(-1, 1)}, GRIRItem{Quantity: "-1", UnitPrice: 3_000_000_000_000_000_000, PriceUnit: 1}, "ledger.amount_too_large"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, _, err := c.r.move(c.p, c.it)
			if err == nil || problemCode(err) != c.code {
				t.Errorf("got %v, want %q", err, c.code)
			}
		})
	}
}

// TestGRIRLinesOutOfRange: an invoice's price difference, a converted line
// or the local total out of range is too large to post.
func TestGRIRLinesOutOfRange(t *testing.T) {
	lowest := money.Minor(math.MinInt64 + 1)
	g := grirPosting{in: Posting{Currency: "EUR", GRIR: []GRIRItem{{Quantity: "1", UnitPrice: 1_000_000_000_000_000_000, PriceUnit: 1, Invoiced: &lowest}}},
		rates: rates{toLC: one, toGC: one, expTC: 2, expLC: 2, expGC: 2}}
	if _, _, err := g.itemLines(map[orderItem]*position{{}: zero()}, []orderItem{{}}); problemCode(err) != "ledger.amount_too_large" {
		t.Errorf("price difference: got %v, want ledger.amount_too_large", err)
	}
	g.rates.toLC = two
	huge := money.Minor(6_000_000_000_000_000_000)
	if _, err := g.convert(context.Background(), []Line{{TC: huge}, {TC: -huge}}, []bool{false, false}); problemCode(err) != "ledger.amount_too_large" {
		t.Errorf("converted line: got %v, want ledger.amount_too_large", err)
	}
	g.rates.toLC = one
	top := Line{AccountID: uuid.New(), LC: math.MaxInt64, GC: math.MaxInt64}
	if _, err := g.convert(context.Background(), []Line{top, top}, []bool{true, true}); problemCode(err) != "ledger.amount_too_large" {
		t.Errorf("local total: got %v, want ledger.amount_too_large", err)
	}
}
