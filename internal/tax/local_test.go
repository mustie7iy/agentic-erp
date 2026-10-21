package tax

import (
	"errors"
	"math/big"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// TestLocalSharesOutOfRange: local amounts that add up out of range are
// reported, never wrapped: the recoverable parts of lines whose taxes
// cancel, and a tax beside bases that cancel.
func TestLocalSharesOutOfRange(t *testing.T) {
	var lines []Amounts
	for range 5 {
		lines = append(lines, Amounts{Tax: 1, Recoverable: 1}, Amounts{Tax: -1})
	}
	g := group{conv: new(big.Rat).SetInt64(4e18)}
	if _, _, err := g.local(lines); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("shares: got %v, want money.ErrOverflow", err)
	}
	// Bases that cancel with a tax that does not (a calculation never
	// produces them; local checks each conversion all the same).
	if _, _, err := g.local([]Amounts{{Base: 1, Tax: 3}, {Base: -1}}); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("taxes: got %v, want money.ErrOverflow", err)
	}
}
