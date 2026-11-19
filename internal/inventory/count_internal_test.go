package inventory

import (
	"errors"
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

// TestCountValueErr: a refusal other than a missing price is passed on as
// it is.
func TestCountValueErr(t *testing.T) {
	other := problem.Unprocessable("inventory.amount_too_large", "too large")
	if got := countValueErr(other, nil, nil, refs{}); !errors.Is(got, other) {
		t.Errorf("got %v, want the refusal as it is", got)
	}
}

// TestCountValueErrUnfound: a missing price no gain of the count has is
// passed on as it is.
func TestCountValueErrUnfound(t *testing.T) {
	missing := problem.Unprocessable("inventory.no_price", "no price")
	if got := countValueErr(missing, nil, nil, refs{}); !errors.Is(got, missing) {
		t.Errorf("got %v, want the refusal as it is", got)
	}
}
