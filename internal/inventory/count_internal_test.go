package inventory

import (
	"math"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestAbsTotal: the differences' values total gains and losses alike, and
// a total beyond an amount's range is refused.
func TestAbsTotal(t *testing.T) {
	if got, err := absTotal("PI1", []Line{{Amount: 200}, {Amount: -150}}); err != nil || got != 350 {
		t.Errorf("total: got %d %v, want 350", got, err)
	}
	_, err := absTotal("PI1", []Line{{Amount: math.MaxInt64}, {Amount: -math.MaxInt64}})
	if p := problem.From(err); p.Code != "inventory.amount_too_large" {
		t.Errorf("beyond the range: got %v, want inventory.amount_too_large", err)
	}
}
