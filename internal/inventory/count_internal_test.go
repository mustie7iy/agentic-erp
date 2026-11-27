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

// TestRecountLimit: an item counted maxCounts times is not recounted again
// (count_item_recounts_check holds the database to it).
func TestRecountLimit(t *testing.T) {
	counted := "5"
	d := CountDocument{Number: "PI1", Items: []CountItem{{ItemNo: 1, Counted: &counted, Recounts: maxCounts - 2}, {ItemNo: 2, Counted: &counted,
		Recounts: maxCounts - 1}}}
	if _, err := checkRecounts(d, []int{1}, refs{}); err != nil {
		t.Errorf("an item counted %d times: got %v, want it recounted", maxCounts-1, err)
	}
	_, err := checkRecounts(d, []int{2}, refs{})
	if p := problem.From(err); p.Code != "inventory.recount_limit" || p.Status != 409 {
		t.Errorf("an item counted %d times: got %v, want 409 inventory.recount_limit", maxCounts, err)
	}
}
