package config

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// withRetainedEarnings is the fixture with company 1000's retained earnings
// account 330000, an equity account added to its chart (ADR-0060 §4).
func withRetainedEarnings(t *testing.T) *Config {
	t.Helper()
	c := fixture(t)
	co := c.Company("1000")
	co.ChartOfAccounts = append(co.ChartOfAccounts, Account{Number: "330000", Name: "Retained earnings", Type: Equity})
	co.YearEnd.RetainedEarningsAccount = "330000"
	return c
}

// TestRetainedEarningsValid: an equity account of the chart validates, no
// account at all does too, and each rule refuses the field.
func TestRetainedEarningsValid(t *testing.T) {
	if fs := Validate(withRetainedEarnings(t), currency.Seeded()); HasErrors(fs) {
		t.Fatalf("retained earnings configured: got %v, want no errors", fs)
	}
	if fs := Validate(fixture(t), currency.Seeded()); HasErrors(fs) {
		t.Fatalf("no retained earnings account: got %v, want no errors", fs)
	}
	const path = "companies[1000].year_end.retained_earnings_account"
	cases := []struct {
		name   string
		mutate func(co *Company)
	}{
		{"an account not in the chart", func(co *Company) { co.YearEnd.RetainedEarningsAccount = "399999" }},
		{"a revenue account", func(co *Company) { co.YearEnd.RetainedEarningsAccount = "400000" }},
		{"a reconciliation account", func(co *Company) { co.Account("330000").AssetReconciliation = true }},
		{"an open-item managed account", func(co *Company) { co.Account("330000").OpenItems = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := withRetainedEarnings(t)
			tc.mutate(c.Company("1000"))
			if fs := Validate(c, currency.Seeded()); !hasPath(fs, path) {
				t.Errorf("findings: got %v, want one at %s", fs, path)
			}
		})
	}
}

// TestRetainedEarningsTier: setting or changing the account is tier 3,
// posting-relevant (ADR-0060 §4).
func TestRetainedEarningsTier(t *testing.T) {
	base := withRetainedEarnings(t)
	base.Company("1000").YearEnd.RetainedEarningsAccount = ""
	set := withRetainedEarnings(t)
	changed := withRetainedEarnings(t)
	changed.Company("1000").YearEnd.RetainedEarningsAccount = "300000"
	for name, pair := range map[string][2]*Config{"set": {base, set}, "changed": {set, changed}} {
		t.Run(name, func(t *testing.T) {
			diffs, _ := Diff(pair[0], pair[1]) // Diff never fails
			if len(diffs) != 1 {
				t.Fatalf("Diff: got %v, want the retained earnings account alone", diffs)
			}
			if d := diffs[0]; d.Path != "companies[1000].year_end.retained_earnings_account" || d.Tier != TierPostingRelated {
				t.Errorf("Diff: got %s at %v, want the account at %v", d.Path, d.Tier, TierPostingRelated)
			}
		})
	}
}
