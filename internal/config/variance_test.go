package config

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// withVariance is the fixture with company 1000's variance report against
// the prior period, the prior year and the BUDGET category's cost center
// lines, flagging variances of 10,000.00 and 10 % (ADR-0058 §3).
func withVariance(t *testing.T) *Config {
	t.Helper()
	c := fixture(t)
	c.Company("1000").Close.Variance = &CloseVariance{Bases: []string{BasisPriorPeriod, BasisPriorYear, BasisPlan}, ThresholdAmount: "10000.00",
		ThresholdPercent: "10", PlanCategory: "BUDGET", PlanCenters: PlanCentersCost}
	return c
}

// TestCloseVarianceValid: ADR-0058 §3's variance settings validate, so do
// prior bases alone without a percent, and each rule refuses its field.
func TestCloseVarianceValid(t *testing.T) {
	if fs := Validate(withVariance(t), currency.Seeded()); HasErrors(fs) {
		t.Fatalf("variance configured: got %v, want no errors", fs)
	}
	prior := withVariance(t)
	prior.Company("1000").Close.Variance = &CloseVariance{Bases: []string{BasisPriorYear}, ThresholdAmount: "0.01"}
	if fs := Validate(prior, currency.Seeded()); HasErrors(fs) {
		t.Fatalf("the prior year alone: got %v, want no errors", fs)
	}
	const p = "companies[1000].close.variance."
	cases := []struct {
		name   string
		mutate func(v *CloseVariance)
		path   string
	}{
		{"no basis", func(v *CloseVariance) { v.Bases = nil }, p + "bases"},
		{"an unknown basis", func(v *CloseVariance) { v.Bases = append(v.Bases, "forecast") }, p + "bases"},
		{"a basis twice", func(v *CloseVariance) { v.Bases = append(v.Bases, BasisPriorYear) }, p + "bases"},
		{"no amount threshold", func(v *CloseVariance) { v.ThresholdAmount = "" }, p + "threshold_amount"},
		{"a zero amount threshold", func(v *CloseVariance) { v.ThresholdAmount = "0.00" }, p + "threshold_amount"},
		{"a negative amount threshold", func(v *CloseVariance) { v.ThresholdAmount = "-5.00" }, p + "threshold_amount"},
		{"an amount threshold finer than cents", func(v *CloseVariance) { v.ThresholdAmount = "5.001" }, p + "threshold_amount"},
		{"a percent that is no number", func(v *CloseVariance) { v.ThresholdPercent = "ten" }, p + "threshold_percent"},
		{"a zero percent", func(v *CloseVariance) { v.ThresholdPercent = "0" }, p + "threshold_percent"},
		{"a negative percent", func(v *CloseVariance) { v.ThresholdPercent = "-1" }, p + "threshold_percent"},
		{"a percent of three places", func(v *CloseVariance) { v.ThresholdPercent = "10.125" }, p + "threshold_percent"},
		{"the plan without its category", func(v *CloseVariance) { v.PlanCategory = "" }, p + "plan_category"},
		{"the plan with a lower-case category", func(v *CloseVariance) { v.PlanCategory = "budget" }, p + "plan_category"},
		{"the plan without its centers", func(v *CloseVariance) { v.PlanCenters = "" }, p + "plan_centers"},
		{"the plan over both kinds of center", func(v *CloseVariance) { v.PlanCenters = "both" }, p + "plan_centers"},
		{"a category without the plan", func(v *CloseVariance) { v.Bases, v.PlanCenters = []string{BasisPriorPeriod}, "" }, p + "plan_category"},
		{"centers without the plan", func(v *CloseVariance) { v.Bases, v.PlanCategory = []string{BasisPriorPeriod}, "" }, p + "plan_centers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := withVariance(t)
			tc.mutate(c.Company("1000").Close.Variance)
			if fs := Validate(c, currency.Seeded()); !hasPath(fs, tc.path) {
				t.Errorf("findings: got %v, want one at %s", fs, tc.path)
			}
		})
	}
	unknown := withVariance(t)
	unknown.Company("1000").Currency = "XXQ"
	for _, f := range Validate(unknown, currency.Seeded()) {
		if f.Path == p+"threshold_amount" {
			t.Errorf("a company in an unknown currency: got %v, want its currency's finding alone", f)
		}
	}
}

// TestCloseVarianceOf: a company's setting is given for a basis it
// configures, none for another or a company without variance settings.
func TestCloseVarianceOf(t *testing.T) {
	c := withVariance(t)
	c.Company("1000").Close.Variance.Bases = []string{BasisPriorPeriod}
	cases := []struct {
		name, company, basis string
		want                 bool
	}{
		{"a configured basis", "1000", BasisPriorPeriod, true},
		{"another basis", "1000", BasisPlan, false},
		{"a company without settings", "2000", BasisPriorPeriod, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.Company(tc.company).Variance(tc.basis) != nil; got != tc.want {
				t.Errorf("Variance(%s) of %s: got %v, want %v", tc.basis, tc.company, got, tc.want)
			}
		})
	}
}

// TestCloseVarianceTier: a company's variance settings are operational
// (ADR-0058 §3), set for the first time or changed.
func TestCloseVarianceTier(t *testing.T) {
	base := fixture(t)
	set := withVariance(t)
	changed := withVariance(t)
	changed.Company("1000").Close.Variance.ThresholdPercent = "5"
	for name, pair := range map[string][2]*Config{"set": {base, set}, "changed": {set, changed}} {
		t.Run(name, func(t *testing.T) {
			diffs, _ := Diff(pair[0], pair[1]) // Diff never fails
			if len(diffs) == 0 {
				t.Fatal("Diff: got no change, want the variance settings")
			}
			for _, d := range diffs {
				if d.Tier != TierOperational {
					t.Errorf("Diff %s: got tier %v, want %v", d.Path, d.Tier, TierOperational)
				}
			}
		})
	}
}
