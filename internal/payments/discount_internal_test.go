package payments

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
)

func day(s string) time.Time {
	d, _ := time.Parse(time.DateOnly, s) // constant test dates
	return d
}

// TestDiscountRounding: a discount is the tier's percent of the item,
// rounded half away from zero, and an item always pays at least a minor
// unit.
func TestDiscountRounding(t *testing.T) {
	terms := func(pct string) *ledger.ItemTerms {
		return &ledger.ItemTerms{BaselineDate: day("2026-03-01"), NetDays: 30, Discounts: []ledger.Discount{{Days: 10, Percent: pct}}}
	}
	for name, c := range map[string]struct {
		pct        string
		paid, want money.Minor
	}{
		"half away from zero":           {"2.5", 1, 0},
		"half away from zero, up":       {"2.5", 20, 1},
		"a credit, half away from zero": {"2.5", -20, -1},
		"the whole amount, less a unit": {"99.9", 1, 0},
		"a credit, less a unit":         {"99.9", -2, -1},
		"three decimals":                {"2.125", 10000, 213},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := discountOn(terms(c.pct), c.paid, day("2026-03-05"))
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("discount of %s%% on %d: got %d, want %d", c.pct, c.paid, got, c.want)
			}
		})
	}
	if _, err := discountOn(terms("x"), 100, day("2026-03-05")); err == nil {
		t.Error("discount at a percent that is not one: got none, want an error")
	}
}

// TestProposalDiscountsOutOfRange: a proposal whose discounts are not a
// percent, or add up beyond an amount, fails rather than paying a wrong
// amount.
func TestProposalDiscountsOutOfRange(t *testing.T) {
	supplier, bank := uuid.New(), uuid.New()
	payees := map[uuid.UUID]partner.Payee{supplier: {Status: partner.Active, BankAccountID: &bank}}
	bad := &ledger.ItemTerms{BaselineDate: day("2026-03-01"), NetDays: 30, Discounts: []ledger.Discount{{Days: 10, Percent: "x"}}}
	_, err := propose([]candidate{{supplier: supplier, items: []Item{{Amount: 100, terms: bad}}}}, payees, Run{RunDate: day("2026-03-05")}, "2000", 0)
	if err == nil || !strings.Contains(err.Error(), "the discount on item") {
		t.Errorf("a percent that is not one: got %v, want the discount refused", err)
	}
	huge := money.Minor(9_000_000_000_000_000_000)
	c := candidate{supplier: supplier, items: []Item{{Amount: huge, Discount: huge - 1}, {Amount: huge, Discount: huge - 1}}}
	var pr proposal
	if err := pr.pay(c, bank, 1, Run{}, "2000"); err == nil {
		t.Error("a supplier's discounts beyond an amount: got none, want an error")
	}
	pr = proposal{discount: huge}
	if err := pr.pay(candidate{supplier: supplier, items: []Item{{Amount: huge, Discount: huge - 1}}}, bank, 1, Run{}, "2000"); err == nil {
		t.Error("a run's discounts beyond an amount: got none, want an error")
	}
}
