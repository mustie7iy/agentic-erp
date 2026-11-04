package taxfiling

import (
	"errors"
	"math"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// TestDifferenceOverflow: a box's difference out of the range of an
// amount fails, naming the box, rather than wrapping; the return's
// computation then fails (an overflow fails it, ADR-0049 §11 As built).
func TestDifferenceOverflow(t *testing.T) {
	got, err := difference(map[string]money.Minor{"a": 5, "b": math.MaxInt64}, map[string]money.Minor{"a": 7, "b": -1})
	if !errors.Is(err, money.ErrOverflow) || got != nil {
		t.Errorf("difference: got %v, %v, want ErrOverflow", got, err)
	}
	got, err = difference(map[string]money.Minor{"a": 5}, map[string]money.Minor{"a": 7})
	if err != nil || got["a"] != -2 {
		t.Errorf("difference: got %v, %v, want a=-2", got, err)
	}
}
