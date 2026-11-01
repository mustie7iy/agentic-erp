package payments_test

import (
	"slices"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

// TestRunsAcrossYearEnd: a payment run's number, and the end-to-end ids
// of its payments made from it, are unique in the legal entity across
// fiscal years, so the first run of a new fiscal year is numbered from a
// range that cannot repeat last year's: with the next year's range given
// the year in its prefix, as the configuration now requires of a range a
// country pack would otherwise create afresh, both runs are proposed.
// With the pack's range repeated, the second run was refused as a
// duplicate (review:T73g2; config.RangeRepeats refuses that range now).
func TestRunsAcrossYearEnd(t *testing.T) {
	f := fixture.NewWith(t, nil, func(c *config.Config) {
		c.NumberRanges = slices.DeleteFunc(c.NumberRanges, func(n config.NumberRange) bool {
			return n.FiscalYear == 2027 && n.DocumentType == "payment_run" // the fixture's own when it is run in 2027
		})
		c.NumberRanges = append(c.NumberRanges, config.NumberRange{Company: "2000", DocumentType: "payment_run", FiscalYear: 2027, Prefix: "PR2027-",
			First: 100000, Last: 999999})
	}, perms...)
	e := setupIn(t, f, func(*app.Config) {})
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier GmbH", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "INV-Y1", "100.00", "2026-03-01")
	last := e.run(t, "2026-12-30", "2026-12-30")
	e.invoice(t, e.Supplier, "INV-Y2", "50.00", "2026-12-30")
	next := e.run(t, "2027-01-04", "2027-12-31")
	for name, c := range map[string]struct {
		run  map[string]any
		want []string // the run's number and its payment's end-to-end id
	}{
		"last year's run": {last, []string{"PR100000", "2000-PR100000-000001"}},
		"this year's run": {next, []string{"PR2027-100000", "2000-PR2027-100000-000001"}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := c.run["document_number"]; got != c.want[0] {
				t.Errorf("document number: got %v, want %s", got, c.want[0])
			}
			pays := e.in(t).payments(t, c.run)
			if len(pays) != 1 {
				t.Fatalf("payments: got %d, want 1", len(pays))
			}
			if got := pays[0].(map[string]any)["end_to_end_id"]; got != c.want[1] {
				t.Errorf("end-to-end id: got %v, want %s", got, c.want[1])
			}
		})
	}
}
