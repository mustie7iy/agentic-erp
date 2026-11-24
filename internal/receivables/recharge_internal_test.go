package receivables

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
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

// TestPartOf: a part of nothing is nothing, the whole is all, and a half
// rounds away from zero, either sign.
func TestPartOf(t *testing.T) {
	for _, c := range []struct {
		total, part, whole, want money.Minor
	}{{500, 0, 10500, 0}, {500, 10500, 10500, 500}, {1, 1, 2, 1}, {-1, 1, 2, -1}, {500, 10490, 10500, 500}} {
		t.Run(fmt.Sprint(c.total, c.part, c.whole), func(t *testing.T) {
			if got := partOf(c.total, c.part, c.whole); got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}

// TestRecovered: what is paid of a relieved invoice counts only beyond
// what its write-offs left owed, and never more than they wrote off.
func TestRecovered(t *testing.T) {
	v, s := Invoice{Gross: 10500}, reliefState{written: 3500}
	for _, c := range []struct {
		paid, want money.Minor
	}{{0, 0}, {7000, 0}, {7001, 1}, {10500, 3500}, {20000, 3500}} {
		t.Run(fmt.Sprint(c.paid), func(t *testing.T) {
			if got := recovered(v, s, c.paid); got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}

// TestChargeLinesShareOfNothing: a group whose share of the tax rounds to
// nothing makes no line, so the lines are the other group's and the
// payment differences line.
func TestChargeLinesShareOfNothing(t *testing.T) {
	groups := []reliefGroup{{code: "AS", account: uuid.New(), tax: 300, lc: 1100}, {code: "AS", account: uuid.New(), tax: 1, lc: 4}}
	lines := chargeLines(charge{tax: 3, lc: 11}, groups, uuid.New(), nil)
	var got []string
	for _, l := range lines {
		got = append(got, fmt.Sprintf("%d/%d", l.Amount, *l.LC))
	}
	if s, want := fmt.Sprint(got), "[-3/-11 3/11]"; s != want {
		t.Errorf("the lines: got %s, want %s", s, want)
	}
}
