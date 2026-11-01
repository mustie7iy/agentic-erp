package config

import (
	"slices"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// canadaTiers are SOR/91-45 s. 3's tiers as a pack states them.
func canadaTiers() []DocumentaryRequirement {
	return []DocumentaryRequirement{
		{AtLeast: "100", Requires: []string{RequiresSupplierTaxID}, Source: "SOR/91-45 s. 3(b)"},
		{AtLeast: "500", Requires: []string{RequiresSupplierTaxID, RequiresItemDescriptions}, Source: "SOR/91-45 s. 3(c)"},
	}
}

// TestTaxSettings: a company's tax settings validate (ADR-0049 §6, §8),
// the defaults are the average rate and no settings, and the section is
// tier 3.
func TestTaxSettings(t *testing.T) {
	c := fixture(t)
	if got := c.Companies[0].TaxSettings.RateTypeOrAverage(); got != "average" {
		t.Errorf("default rate type: got %s, want average", got)
	}
	if !c.Companies[0].TaxSettings.IsZero() {
		t.Errorf("fixture's tax settings: got %+v, want none", c.Companies[0].TaxSettings)
	}
	c.Companies[0].TaxSettings = TaxSettings{RateType: "bank_selling", Rounding: TaxRoundLine, DocumentaryRequirements: append(canadaTiers(),
		DocumentaryRequirement{Above: "10000.00", Requires: []string{RequiresSupplierTaxID}, Source: "UAE Executive Regulation Art. 59", PendingSignOff: true})}
	if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
		t.Fatalf("validate: got %v, want no errors", fs)
	}
	if got := classify("companies[1000].tax_settings.documentary_requirements[0].at_least"); got != TierPostingRelated {
		t.Errorf("tier: got %v, want %v", got, TierPostingRelated)
	}
	if amount, inclusive := canadaTiers()[0].Threshold(); amount != "100" || !inclusive {
		t.Errorf("at_least threshold: got %s %v, want 100 true", amount, inclusive)
	}
	if amount, inclusive := (DocumentaryRequirement{Above: "10000"}).Threshold(); amount != "10000" || inclusive {
		t.Errorf("above threshold: got %s %v, want 10000 false", amount, inclusive)
	}
}

// TestTaxSettingsRules: each mutation is refused at its path.
func TestTaxSettingsRules(t *testing.T) {
	const p = "companies[1000].tax_settings"
	tier := func(f func(r *DocumentaryRequirement)) TaxSettings {
		r := canadaTiers()[0]
		f(&r)
		return TaxSettings{DocumentaryRequirements: []DocumentaryRequirement{r}}
	}
	cases := map[string]struct {
		s    TaxSettings
		path string
	}{
		"unknown rate type":   {TaxSettings{RateType: "spot"}, p + ".rate_type"},
		"unknown rounding":    {TaxSettings{Rounding: "invoice"}, p + ".rounding"},
		"no threshold":        {tier(func(r *DocumentaryRequirement) { r.AtLeast = "" }), p + ".documentary_requirements[0]"},
		"both thresholds":     {tier(func(r *DocumentaryRequirement) { r.Above = "100" }), p + ".documentary_requirements[0]"},
		"negative threshold":  {tier(func(r *DocumentaryRequirement) { r.AtLeast = "-1" }), p + ".documentary_requirements[0]"},
		"too many decimals":   {tier(func(r *DocumentaryRequirement) { r.AtLeast = "1.001" }), p + ".documentary_requirements[0]"},
		"no requirement":      {tier(func(r *DocumentaryRequirement) { r.Requires = nil }), p + ".documentary_requirements[0].requires"},
		"unknown requirement": {tier(func(r *DocumentaryRequirement) { r.Requires = []string{"recipient_name"} }), p + ".documentary_requirements[0].requires"},
		"a requirement twice": {tier(func(r *DocumentaryRequirement) { r.Requires = []string{RequiresSupplierTaxID, RequiresSupplierTaxID} }), p + ".documentary_requirements[0].requires"},
		"no source":           {tier(func(r *DocumentaryRequirement) { r.Source = "" }), p + ".documentary_requirements[0].source"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := fixture(t)
			c.Companies[0].TaxSettings = tc.s
			if got := errorPaths(Validate(c, currency.Seeded())); !slices.Contains(got, tc.path) {
				t.Errorf("error paths: got %v, want %s", got, tc.path)
			}
		})
	}
	if fs := CheckTaxSettings("tax.yaml", TaxSettings{DocumentaryRequirements: canadaTiers()}, 0); len(fs) != 0 {
		t.Errorf("CheckTaxSettings of whole amounts: got %v, want none", fs)
	}
}
