package ledger_test

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// TestDiscountRounding: a discount is the tier's percent of the item,
// rounded half away from zero, and an item always pays at least a minor
// unit (moved from the payment proposer with the rule, T53e).
func TestDiscountRounding(t *testing.T) {
	terms := func(pct string) *ledger.ItemTerms {
		return &ledger.ItemTerms{BaselineDate: date("2026-03-01"), NetDays: 30, Discounts: []ledger.Discount{{Days: 10, Percent: pct}}}
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
		"a zero item":                   {"3", 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := terms(c.pct).DiscountOn(c.paid, date("2026-03-05"))
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("discount of %s%% on %d: got %d, want %d", c.pct, c.paid, got, c.want)
			}
		})
	}
	two := &ledger.ItemTerms{BaselineDate: date("2026-03-01"), NetDays: 45, Discounts: []ledger.Discount{{Days: 14, Percent: "3"}, {Days: 30, Percent: "2"}}}
	for name, c := range map[string]struct {
		run, dueBy string
		want       bool
	}{
		"a tier lapsing before the due-by date": {"2026-03-10", "2026-03-20", true},
		"the same tier on the due-by date":      {"2026-03-10", "2026-03-12", false},
		"a due-by date before the run date":     {"2026-03-20", "2026-03-10", false},
		"no tier on the run date":               {"2026-04-10", "2026-04-12", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := two.LosesDiscount(date(c.run), date(c.dueBy)); got != c.want {
				t.Errorf("loses discount paying on %s rather than %s: got %v, want %v", c.dueBy, c.run, got, c.want)
			}
		})
	}
	if _, err := terms("x").DiscountOn(100, date("2026-03-05")); err == nil {
		t.Error("discount at a percent that is not one: got none, want an error")
	}
	var none *ledger.ItemTerms
	if got := none.TierOn(date("2026-03-05")); got != -1 {
		t.Errorf("the tier of an item without terms: got %d, want -1", got)
	}
}

// TestDueDate: an invoice is due by the terms it was posted with, else by
// its supplier's net days now; a debit (a credit memo) on its document
// date, so it nets with the next run.
func TestDueDate(t *testing.T) {
	terms := &ledger.ItemTerms{BaselineDate: date("2026-03-02"), NetDays: 30}
	for name, c := range map[string]struct {
		item ledger.PayableItem
		want string
	}{
		"an invoice by its terms":       {ledger.PayableItem{DocumentDate: date("2026-03-01"), Amount: -100, Terms: terms}, "2026-04-01"},
		"an invoice by the supplier's":  {ledger.PayableItem{DocumentDate: date("2026-03-01"), Amount: -100}, "2026-03-15"},
		"a credit memo on its own date": {ledger.PayableItem{DocumentDate: date("2026-03-01"), Amount: 100, Terms: terms}, "2026-03-01"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := c.item.DueDate(14).Format("2006-01-02"); got != c.want {
				t.Errorf("due date: got %s, want %s", got, c.want)
			}
		})
	}
}
