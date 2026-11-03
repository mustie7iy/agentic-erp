package closing

import (
	"math"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// TestVarianceOverflow: a variance or a total beyond int64's minor units
// fails the report rather than wrapping round; no ledger holds such
// figures, so they are given here directly.
func TestVarianceOverflow(t *testing.T) {
	account := func(number string, net money.Minor) AccountNet {
		return AccountNet{AccountID: uuid.New(), Number: number, Name: "Account " + number, Net: net}
	}
	r := report{setting: &config.CloseVariance{ThresholdAmount: "1.00"}, threshold: 100}
	big := account("1", math.MaxInt64)
	if err := r.compute([]AccountNet{big}, map[uuid.UUID]money.Minor{big.AccountID: -1}, nil); err == nil || !strings.Contains(err.Error(), "variance of account 1") {
		t.Errorf("a variance past int64: got %v, want the account's failure", err)
	}
	two := []AccountNet{account("1", math.MaxInt64), account("2", 1)}
	if err := r.compute(two, map[uuid.UUID]money.Minor{}, nil); err == nil || !strings.Contains(err.Error(), "totals of the variances") {
		t.Errorf("totals past int64: got %v, want the totals' failure", err)
	}
}
