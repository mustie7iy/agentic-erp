package inventory

import (
	"math"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// TestABCTallyRange (S2c3): a plant's consumption beyond an amount's range
// is refused, never wrapped (ADR-0005).
func TestABCTallyRange(t *testing.T) {
	a := ABCAnalysis{Plant: tenancy.Unit{Code: "P1"}, consumption: []abcRow{{uuid.New(), money.Minor(math.MaxInt64), "A"}, {uuid.New(), 1, "B"}}}
	err := a.tally(func(uuid.UUID) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "consumption at plant P1") {
		t.Errorf("got %v, want the plant's consumption refused", err)
	}
}
