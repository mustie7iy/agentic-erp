package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// withDunning is the fixture with company 1000's dunning levels: 1 from a
// day overdue, 2 from 15 days, 3 from 30 (ADR-0065 §2).
func withDunning(t *testing.T) *Config {
	t.Helper()
	c := fixture(t)
	c.Company("1000").Dunning.Levels = []DunningLevel{{Level: 1, DaysOverdue: 1}, {Level: 2, DaysOverdue: 15}, {Level: 3, DaysOverdue: 30}}
	return c
}

// TestDunningLevelsValid: levels numbered from 1, each with more days
// overdue than the one before, validate; a company without any validates
// too; each rule refuses the level's own field.
func TestDunningLevelsValid(t *testing.T) {
	if fs := Validate(withDunning(t), currency.Seeded()); HasErrors(fs) {
		t.Fatalf("three dunning levels: got %v, want no errors", fs)
	}
	if fs := Validate(fixture(t), currency.Seeded()); HasErrors(fs) {
		t.Fatalf("no dunning levels: got %v, want no errors", fs)
	}
	const levels = "companies[1000].dunning.levels"
	ten := make([]DunningLevel, MaxDunningLevels+1)
	for i := range ten {
		ten[i] = DunningLevel{Level: i + 1, DaysOverdue: 10 * (i + 1)}
	}
	cases := []struct {
		name          string
		levels        []DunningLevel
		path, message string
	}{
		{"numbered from 2", []DunningLevel{{Level: 2, DaysOverdue: 1}}, levels + "[2].level", "this is level 1"},
		{"a level skipped", []DunningLevel{{Level: 1, DaysOverdue: 1}, {Level: 3, DaysOverdue: 9}}, levels + "[3].level", "this is level 2"},
		{"no days overdue", []DunningLevel{{Level: 1}}, levels + "[1].days_overdue", "must be 1-3650 days"},
		{"more than ten years overdue", []DunningLevel{{Level: 1, DaysOverdue: 3651}}, levels + "[1].days_overdue", "must be 1-3650 days"},
		{"the days of the level before", []DunningLevel{{Level: 1, DaysOverdue: 10}, {Level: 2, DaysOverdue: 10}}, levels + "[2].days_overdue",
			"more days overdue than the level before (10)"},
		{"fewer days than the level before", []DunningLevel{{Level: 1, DaysOverdue: 10}, {Level: 2, DaysOverdue: 9}}, levels + "[2].days_overdue",
			"more days overdue than the level before (10)"},
		{"ten levels", ten, levels, "at most 9 dunning levels"},
		{"a charge that is no amount", []DunningLevel{{Level: 1, DaysOverdue: 1, Charge: "5.001"}}, levels + "[1].charge", "decimal"},
		{"a negative charge", []DunningLevel{{Level: 1, DaysOverdue: 1, Charge: "-5.00"}}, levels + "[1].charge", "must not be negative"},
		{"interest without a rate", []DunningLevel{{Level: 1, DaysOverdue: 1, Interest: true}}, levels + "[1].interest", "set dunning.interest_rate_percent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture(t)
			c.Company("1000").Dunning.Levels = tc.levels
			fs := Validate(c, currency.Seeded())
			if !slices.ContainsFunc(fs, func(f Finding) bool {
				return f.Severity == SevError && f.Path == tc.path && strings.Contains(f.Message, tc.message)
			}) {
				t.Errorf("findings: got %v, want an error at %s saying %q", fs, tc.path, tc.message)
			}
		})
	}
}

// TestDunningProcedure: grace days, a minimum amount, an interest rate
// and the levels' charges and interest validate within their bounds and
// are read in minor units and thousandths of a percent (T76d); each rule
// refuses its own field.
func TestDunningProcedure(t *testing.T) {
	c := withDunning(t)
	d := &c.Company("1000").Dunning
	d.GraceDays, d.MinimumAmount, d.InterestRatePercent = MaxGraceDays, "25.00", "9.125"
	d.Levels[1].Charge, d.Levels[1].Interest = "15.00", true
	if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
		t.Fatalf("a full procedure: got %v, want no errors", fs)
	}
	if got := d.Minimum(2); got != 2500 {
		t.Errorf("minimum: got %d, want 2500", got)
	}
	if got := d.InterestRate(); got != 9125 {
		t.Errorf("interest rate: got %d, want 9125", got)
	}
	if got := d.Levels[1].ChargeOf(2); got != 1500 {
		t.Errorf("level 2's charge: got %d, want 1500", got)
	}
	if got, rate := d.Levels[0].ChargeOf(2), (Dunning{}).InterestRate(); got != 0 || rate != 0 || (Dunning{}).Minimum(2) != 0 {
		t.Errorf("none set: got charge %d, rate %d, minimum %d, want 0 each", got, rate, (Dunning{}).Minimum(2))
	}
	const p = "companies[1000].dunning"
	cases := []struct {
		name          string
		change        func(*Dunning)
		path, message string
	}{
		{"negative grace days", func(d *Dunning) { d.GraceDays = -1 }, p + ".grace_days", "must be 0-365 days"},
		{"more than a year's grace", func(d *Dunning) { d.GraceDays = MaxGraceDays + 1 }, p + ".grace_days", "must be 0-365 days"},
		{"a minimum that is no amount", func(d *Dunning) { d.MinimumAmount = "ten" }, p + ".minimum_amount", "decimal"},
		{"a negative minimum", func(d *Dunning) { d.MinimumAmount = "-1.00" }, p + ".minimum_amount", "must not be negative"},
		{"a rate of four places", func(d *Dunning) { d.InterestRatePercent = "9.1234" }, p + ".interest_rate_percent", "decimal"},
		{"a rate of 0", func(d *Dunning) { d.InterestRatePercent = "0" }, p + ".interest_rate_percent", "above 0% and below 100%"},
		{"a rate of 100", func(d *Dunning) { d.InterestRatePercent = "100" }, p + ".interest_rate_percent", "above 0% and below 100%"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := withDunning(t)
			tc.change(&c.Company("1000").Dunning)
			fs := Validate(c, currency.Seeded())
			if !slices.ContainsFunc(fs, func(f Finding) bool {
				return f.Severity == SevError && f.Path == tc.path && strings.Contains(f.Message, tc.message)
			}) {
				t.Errorf("findings: got %v, want an error at %s saying %q", fs, tc.path, tc.message)
			}
		})
	}
}

// TestDunningLevelsTier: dunning levels post nothing, so setting them, a
// level's days and a level more are tier 2, operational (ADR-0065 §2),
// each change named by its level.
func TestDunningLevelsTier(t *testing.T) {
	later := withDunning(t)
	later.Company("1000").Dunning.Levels[1].DaysOverdue = 20
	fourth := withDunning(t)
	fourth.Company("1000").Dunning.Levels = append(fourth.Company("1000").Dunning.Levels, DunningLevel{Level: 4, DaysOverdue: 60})
	charged := withDunning(t)
	charged.Company("1000").Dunning.GraceDays, charged.Company("1000").Dunning.Levels[1].Charge = 3, "15.00"
	cases := []struct {
		name      string
		old, new  *Config
		wantPaths string
	}{
		{"set", fixture(t), withDunning(t), "companies[1000].dunning.levels[1] companies[1000].dunning.levels[2] companies[1000].dunning.levels[3]"},
		{"grace days and a charge", withDunning(t), charged, "companies[1000].dunning.grace_days companies[1000].dunning.levels[2].charge"},
		{"a level's days", withDunning(t), later, "companies[1000].dunning.levels[2].days_overdue"},
		{"a level more", withDunning(t), fourth, "companies[1000].dunning.levels[4]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diffs, _ := Diff(tc.old, tc.new) // Diff never fails
			var paths []string
			for _, d := range diffs {
				paths = append(paths, d.Path)
				if d.Tier != TierOperational {
					t.Errorf("tier of %s: got %v, want %v", d.Path, d.Tier, TierOperational)
				}
			}
			if got := strings.Join(paths, " "); got != tc.wantPaths {
				t.Errorf("changes: got %s, want %s", got, tc.wantPaths)
			}
		})
	}
}

// TestDunningAccounts: a company naming neither dunning account posts
// nothing and validates whatever it charges; one naming either posts,
// and then names a revenue account of its chart for each its levels
// charge (T76h); the accounts are posting-relevant, tier 3.
func TestDunningAccounts(t *testing.T) {
	charging := func(t *testing.T) *Config {
		t.Helper()
		c := withDunning(t)
		co := c.Company("1000")
		co.Dunning.InterestRatePercent = "9.000"
		co.Dunning.Levels[1].Charge, co.Dunning.Levels[2].Interest = "15.00", true
		return c
	}
	none := charging(t)
	if fs := Validate(none, currency.Seeded()); HasErrors(fs) || none.Company("1000").AccountDetermination.PostsCharges() {
		t.Fatalf("charges without accounts: got %v, posts %v, want no errors and no posting", fs, none.Company("1000").AccountDetermination.PostsCharges())
	}
	both := charging(t)
	both.Company("1000").AccountDetermination.DunningCharges, both.Company("1000").AccountDetermination.DunningInterest = "400000", "400000"
	if fs := Validate(both, currency.Seeded()); HasErrors(fs) || !both.Company("1000").AccountDetermination.PostsCharges() {
		t.Fatalf("both accounts: got %v, posts %v, want no errors and posting", fs, both.Company("1000").AccountDetermination.PostsCharges())
	}
	const p = "companies[1000].account_determination."
	cases := []struct {
		name              string
		charges, interest string
		noCharge          bool
		path, message     string
	}{
		{"interest charged without its account", "400000", "", false, p + "dunning_interest", "is required"},
		{"a charge without its account", "", "400000", false, p + "dunning_charges", "is required"},
		{"a charge account not in the chart", "499999", "400000", false, p + "dunning_charges", "not in the chart"},
		{"an interest account that is an expense", "400000", "510000", false, p + "dunning_interest", "expected revenue"},
		{"an account named though nothing is charged", "100000", "", true, p + "dunning_charges", "expected revenue"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := charging(t)
			co := c.Company("1000")
			co.AccountDetermination.DunningCharges, co.AccountDetermination.DunningInterest = tc.charges, tc.interest
			if tc.noCharge {
				co.Dunning.Levels[1].Charge, co.Dunning.Levels[2].Interest = "", false
			}
			fs := Validate(c, currency.Seeded())
			if !slices.ContainsFunc(fs, func(f Finding) bool {
				return f.Severity == SevError && f.Path == tc.path && strings.Contains(f.Message, tc.message)
			}) {
				t.Errorf("findings: got %v, want an error at %s saying %q", fs, tc.path, tc.message)
			}
		})
	}
	diffs, _ := Diff(none, both) // Diff never fails
	for _, d := range diffs {
		if d.Tier != TierPostingRelated {
			t.Errorf("tier of %s: got %v, want %v", d.Path, d.Tier, TierPostingRelated)
		}
	}
	if len(diffs) != 2 {
		t.Errorf("changes: got %d, want the two accounts", len(diffs))
	}
}
