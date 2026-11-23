package receivables

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// TestChargeLinesWithoutLocalTax: reliefs whose tax items carried no
// local tax (none was booked in the company's currency) charge again
// none: every line's local amount is nothing, the tax shared as ever.
func TestChargeLinesWithoutLocalTax(t *testing.T) {
	dubai, expensed := "AE-DU", uuid.New()
	groups := []reliefGroup{{code: "AS", account: uuid.New(), jurisdiction: &dubai, tax: 300}, {code: "AS", account: uuid.New(), tax: 100}}
	lines := chargeLines(charge{tax: 4}, groups, expensed, nil)
	var got []string
	for _, l := range lines {
		got = append(got, fmt.Sprintf("%d/%d", l.Amount, *l.LC))
	}
	if s, want := fmt.Sprint(got), "[-3/0 -1/0 4/0]"; s != want {
		t.Errorf("the lines: got %s, want %s", s, want)
	}
	if lines[2].AccountID != expensed {
		t.Errorf("the last line's account: got %s, want %s", lines[2].AccountID, expensed)
	}
	if lines[0].TaxJurisdiction != &dubai {
		t.Errorf("the first line's emirate: got %v, want %s", lines[0].TaxJurisdiction, dubai)
	}
	if lines[1].TaxJurisdiction != nil {
		t.Errorf("the second line's emirate: got %v, want none", *lines[1].TaxJurisdiction)
	}
}
