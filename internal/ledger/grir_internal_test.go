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

// TestGRIRMoveShares: a position that shrinks keeps the share of its local
// and group value that its transaction balance keeps, or by quantity at a
// zero order price; a quantity with more decimals than a line keeps is an
// internal error.
func TestGRIRMoveShares(t *testing.T) {
	r := rates{toLC: one, toGC: one}
	tc, lc, gc, err := r.move(&position{qty: big.NewRat(-4, 1), tc: -300, lc: -270, gc: -250}, GRIRItem{Quantity: "1", UnitPrice: 75, PriceUnit: 1})
	if got, want := [4]any{tc, lc, gc, err}, [4]any{money.Minor(75), money.Minor(67), money.Minor(62), nil}; got != want {
		t.Errorf("priced: got %v, want %v (three quarters of each kept, -202.5 and -187.5 rounded away from zero)", got, want)
	}
	tc, lc, gc, err = r.move(&position{qty: big.NewRat(-2, 1), lc: -10, gc: -9}, GRIRItem{Quantity: "1", UnitPrice: 0, PriceUnit: 1})
	if got, want := [4]any{tc, lc, gc, err}, [4]any{money.Minor(0), money.Minor(5), money.Minor(4), nil}; got != want {
		t.Errorf("free: got %v, want %v (half of each kept by quantity, -4.5 rounded to -5)", got, want)
	}
	if _, _, _, err := r.move(zero(), GRIRItem{Quantity: "-1.0000001", UnitPrice: 1, PriceUnit: 1}); err == nil || problemCode(err) != "" {
		t.Errorf("seven decimals: got %v, want an internal error", err)
	}
}

// TestGRIRBalancesOutOfRange: a local balance moved, or a reversal's or a
// position's total, out of the range of an amount is too large to post.
func TestGRIRBalancesOutOfRange(t *testing.T) {
	r := rates{toLC: one, toGC: one}
	if _, _, _, err := r.move(&position{qty: big.NewRat(-1, 1), tc: -1, lc: math.MinInt64}, GRIRItem{Quantity: "-1", UnitPrice: 1, PriceUnit: 1}); problemCode(err) != "ledger.amount_too_large" {
		t.Errorf("local balance: got %v, want ledger.amount_too_large", err)
	}
	order, item, qty := uuid.New(), int32(10), "-1"
	grir := uuid.New()
	line := Line{AccountID: grir, TC: math.MaxInt64, Quantity: &qty, PurchaseOrderID: &order, POItemNo: &item}
	if _, err := reversalMoves([]Line{line, line}, grir); problemCode(err) != "ledger.amount_too_large" {
		t.Errorf("reversal total: got %v, want ledger.amount_too_large", err)
	}
	moves, err := reversalMoves([]Line{line}, grir)
	if err != nil {
		t.Fatal(err)
	}
	k := orderItem{order, item}
	pos := map[orderItem]*position{k: {qty: big.NewRat(1, 1), tc: math.MaxInt64}}
	if _, err := appendAdjustments(context.Background(), uuid.Nil, nil, []orderItem{k}, moves, pos); problemCode(err) != "ledger.amount_too_large" {
		t.Errorf("position total: got %v, want ledger.amount_too_large", err)
	}
}
