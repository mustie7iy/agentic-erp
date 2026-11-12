package revenue

import (
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// TestAllocate: the transaction price is shared in proportion to the
// stand-alone selling prices; the shares add up to the price to the minor
// unit; a unit left by the rounding goes to the largest remainder, the
// earlier obligation first; with prices that add up to the transaction
// price each share is its price (T81b acceptance).
func TestAllocate(t *testing.T) {
	for _, c := range []struct {
		name   string
		price  money.Minor
		prices []money.Minor
		want   []money.Minor
	}{
		{"the prices are the order's item values", 270000, []money.Minor{150000, 120000}, []money.Minor{150000, 120000}},
		{"one obligation takes the whole price", 270000, []money.Minor{1}, []money.Minor{270000}},
		// 270000 × 150000 ÷ 280000 is 144642.857…, × 130000 is 125357.142…: the unit left goes to the larger remainder.
		{"a price above the order value", 270000, []money.Minor{150000, 130000}, []money.Minor{144643, 125357}},
		{"the larger remainder is the later obligation's", 270000, []money.Minor{130000, 150000}, []money.Minor{125357, 144643}},
		// A third each is 33.33…: equal remainders, so the unit left goes to the first.
		{"equal remainders: the earlier obligation first", 100, []money.Minor{1, 1, 1}, []money.Minor{34, 33, 33}},
		// 6.66… each: two units are left for the first two.
		{"two units left", 20, []money.Minor{5, 5, 5}, []money.Minor{7, 7, 6}},
		{"equal prices in half", 270000, []money.Minor{150000, 150000}, []money.Minor{135000, 135000}},
		// 1 × 1 ÷ 3 is a third of a minor unit each: the one unit goes to the first, the others get none.
		{"a price of one minor unit", 1, []money.Minor{1, 1, 1}, []money.Minor{1, 0, 0}},
		// 10 × 1 ÷ 1001 is under half a unit: the small obligation's share rounds to nothing.
		{"a share below a minor unit", 10, []money.Minor{1000, 1}, []money.Minor{10, 0}},
		{"amounts at the top of the range", math.MaxInt64, []money.Minor{math.MaxInt64, math.MaxInt64}, []money.Minor{math.MaxInt64/2 + 1, math.MaxInt64 / 2}},
		{"no obligation", 100, nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := allocate(c.price, c.prices)
			if !slices.Equal(got, c.want) {
				t.Errorf("shares: got %v, want %v", got, c.want)
			}
		})
	}
}

// TestAllocateAddsUp: whatever the prices, the shares add up to the
// transaction price exactly, none is negative or above its exact share
// rounded up, and the same prices give the same shares.
func TestAllocateAddsUp(t *testing.T) {
	// A fixed sequence, so every run checks the same contracts.
	next := uint64(20261112)
	draw := func(limit int64) money.Minor {
		next = next*6364136223846793005 + 1442695040888963407
		return money.Minor(int64(next>>33)%limit + 1)
	}
	for i := range 500 {
		t.Run(fmt.Sprintf("contract %d", i+1), func(t *testing.T) {
			price := draw(100_000_000)
			prices := make([]money.Minor, int(draw(12)))
			for j := range prices {
				prices[j] = draw(50_000_000)
			}
			shares := allocate(price, prices)
			sum, err := money.Sum(shares...)
			if err != nil {
				t.Fatalf("sum of the shares: %v", err)
			}
			if sum != price {
				t.Errorf("shares %v of prices %v: they add up to %d, want the transaction price %d", shares, prices, sum, price)
			}
			if i := slices.IndexFunc(shares, func(s money.Minor) bool { return s < 0 || s > price }); i >= 0 {
				t.Errorf("share %d: got %d, want it between zero and the transaction price %d", i+1, shares[i], price)
			}
			if again := allocate(price, prices); !slices.Equal(again, shares) {
				t.Errorf("allocated again: got %v, want the same shares %v", again, shares)
			}
		})
	}
}
