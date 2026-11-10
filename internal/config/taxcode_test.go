package config

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// bcCode is a BC purchase code: GST 5% on 140000, then 4% from 2027-04-01,
// and BC PST 7% non-deductible.
func bcCode() TaxCode {
	return TaxCode{Code: "I5", Description: "GST + BC PST", Category: "S", Components: []TaxComponent{
		{Key: "gst", Jurisdiction: "CA", AccountKey: KeyInput, Account: "140000", Rates: []TaxRate{
			{RatePercent: "5", ValidFrom: "2026-01-01", ValidTo: "2027-03-31"}, {RatePercent: "4", ValidFrom: "2027-04-01"}}},
		{Key: "pst_bc", Jurisdiction: "CA-BC", AccountKey: KeyNonDeductible, Rates: []TaxRate{{RatePercent: "7", ValidFrom: "2026-01-01"}}},
	}}
}

// reverseCharge is a UAE reverse-charge code on imported services.
func reverseCharge() TaxCode {
	return TaxCode{Code: "RC", Description: "Reverse charge 5%", Kind: TaxReverseCharge, Category: "AE", Components: []TaxComponent{
		{Key: "vat_out", AccountKey: KeySelfOutput, Account: "211000", Rates: []TaxRate{{RatePercent: "5", ValidFrom: "2026-01-01"}}},
		{Key: "vat_in", AccountKey: KeySelfInput, Account: "140000", Rates: []TaxRate{{RatePercent: "5", ValidFrom: "2026-01-01"}}},
	}}
}

// withCode adds the code, changed by mod, to company 1000.
func withCode(code TaxCode, mod func(*TaxCode)) func(c *Config) {
	return func(c *Config) {
		mod(&code)
		c.Companies[0].TaxCodes = append(c.Companies[0].TaxCodes, code)
	}
}

// TestTaxCodesValid: codes with components (GST with a future rate and a
// non-deductible PST), a reverse charge pair, output codes (two levying
// their lines' emirate, one listing those it accepts) and a zero code are valid beside the single-rate
// codes.
func TestTaxCodesValid(t *testing.T) {
	c := fixture(t)
	co := &c.Companies[0]
	co.TaxCodes = append(co.TaxCodes, bcCode(), reverseCharge(),
		TaxCode{Code: "A1", Description: "Output 5%", Direction: TaxOutput, Components: []TaxComponent{
			{Key: "gst", AccountKey: KeyOutput, Account: "211000", RecoverablePercent: "100", Rates: []TaxRate{{RatePercent: "5", ValidFrom: "2026-01-01"}}}}},
		TaxCode{Code: "S5", Description: "UAE standard, by emirate", Direction: TaxOutput, Components: []TaxComponent{
			{Key: "vat", Jurisdiction: "AE", JurisdictionOnLine: true, AccountKey: KeyOutput, Account: "211000",
				Rates: []TaxRate{{RatePercent: "5", ValidFrom: "2026-01-01"}}}}},
		TaxCode{Code: "S5L", Description: "UAE standard, in the emirates listed", Direction: TaxOutput, Components: []TaxComponent{
			{Key: "vat", Jurisdiction: "AE", JurisdictionOnLine: true, Subdivisions: []string{"AE-AZ", "AE-DU"}, AccountKey: KeyOutput, Account: "211000",
				Rates: []TaxRate{{RatePercent: "5", ValidFrom: "2026-01-01"}}}}},
		TaxCode{Code: "Z0", Description: "Zero-rated", Kind: TaxZero, Category: "Z", RatePercent: "0", Account: "140000"})
	if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
		t.Fatalf("valid codes: got %v, want no errors", fs)
	}
}

func TestTaxCodeFindings(t *testing.T) {
	bc := func(mod func(*TaxCode)) func(c *Config) { return withCode(bcCode(), mod) }
	gst := func(mod func(*TaxComponent)) func(c *Config) { return bc(func(t *TaxCode) { mod(&t.Components[0]) }) }
	pst := func(mod func(*TaxComponent)) func(c *Config) { return bc(func(t *TaxCode) { mod(&t.Components[1]) }) }
	rc := func(mod func(*TaxCode)) func(c *Config) { return withCode(reverseCharge(), mod) }
	onLine := func(mod func(*TaxComponent)) func(c *Config) {
		return withCode(TaxCode{Code: "S5", Description: "UAE standard, by emirate", Direction: TaxOutput, Components: []TaxComponent{
			{Key: "vat", Jurisdiction: "AE", JurisdictionOnLine: true, AccountKey: KeyOutput, Account: "211000",
				Rates: []TaxRate{{RatePercent: "5", ValidFrom: "2026-01-01"}}}}}, func(t *TaxCode) { mod(&t.Components[0]) })
	}
	p, s5 := "companies[1000].tax_codes[I5]", "companies[1000].tax_codes[S5]"
	tests := []struct {
		name   string
		change func(c *Config)
		path   string
	}{
		{"code", bc(func(t *TaxCode) { t.Code = "i5" }), "companies[1000].tax_codes[i5]"},
		{"direction", bc(func(t *TaxCode) { t.Direction = "both" }), p + ".direction"},
		{"kind", bc(func(t *TaxCode) { t.Kind = "luxury" }), p + ".kind"},
		{"category", bc(func(t *TaxCode) { t.Category = "X" }), p + ".category"},
		{"both forms", bc(func(t *TaxCode) { t.RatePercent = "5" }), p},
		{"component key", gst(func(c *TaxComponent) { c.Key = "GST" }), p + ".components[GST]"},
		{"component twice", bc(func(t *TaxCode) { t.Components[1].Key = "gst" }), p + ".components[gst]"},
		{"output key on an input code", gst(func(c *TaxComponent) { c.AccountKey = KeyOutput }), p + ".components[gst].account_key"},
		{"input key on an output code", bc(func(t *TaxCode) { t.Direction = TaxOutput }), p + ".components[gst].account_key"},
		{"jurisdiction", gst(func(c *TaxComponent) { c.Jurisdiction = "Canada" }), p + ".components[gst].jurisdiction"},
		{"recoverable above 100", gst(func(c *TaxComponent) { c.RecoverablePercent = "100.5" }), p + ".components[gst].recoverable_percent"},
		{"recoverable text", gst(func(c *TaxComponent) { c.RecoverablePercent = "half" }), p + ".components[gst].recoverable_percent"},
		{"account unknown", gst(func(c *TaxComponent) { c.Account = "999999" }), p + ".components[gst].account"},
		{"account missing", gst(func(c *TaxComponent) { c.Account = "" }), p + ".components[gst].account"},
		{"non-deductible with an account", pst(func(c *TaxComponent) { c.Account = "140000" }), p + ".components[pst_bc].account"},
		{"non-deductible recovering", pst(func(c *TaxComponent) { c.RecoverablePercent = "50" }), p + ".components[pst_bc].recoverable_percent"},
		{"no rates", gst(func(c *TaxComponent) { c.Rates = nil }), p + ".components[gst].rates"},
		{"rate of 100", gst(func(c *TaxComponent) { c.Rates[0].RatePercent = "100" }), p + ".components[gst].rates[2026-01-01].rate_percent"},
		{"single rate of 100", withCode(TaxCode{Code: "V9", Description: "All", RatePercent: "100", Account: "140000"}, func(*TaxCode) {}),
			"companies[1000].tax_codes[V9].rate_percent"},
		{"rate decimals", gst(func(c *TaxComponent) { c.Rates[0].RatePercent = "5.0001" }), p + ".components[gst].rates[2026-01-01].rate_percent"},
		{"rate from", gst(func(c *TaxComponent) { c.Rates[0].ValidFrom = "soon" }), p + ".components[gst].rates[soon].valid_from"},
		{"rate to", gst(func(c *TaxComponent) { c.Rates[0].ValidTo = "later" }), p + ".components[gst].rates[2026-01-01].valid_to"},
		{"rate ends before it starts", gst(func(c *TaxComponent) { c.Rates[0].ValidTo = "2025-12-31" }), p + ".components[gst].rates[2026-01-01].valid_to"},
		{"rates overlap", gst(func(c *TaxComponent) { c.Rates[0].ValidTo = "2027-04-01" }), p + ".components[gst].rates"},
		{"open rate followed by another", gst(func(c *TaxComponent) { c.Rates[0].ValidTo = "" }), p + ".components[gst].rates"},
		{"zero code with a rate", bc(func(t *TaxCode) { t.Kind = TaxZero }), p + ".components[gst].rates[2026-01-01].rate_percent"},
		{"adjustment code with a rate", bc(func(t *TaxCode) { t.Kind = TaxAdjustment }), p + ".components[gst].rates[2026-01-01].rate_percent"},
		{"single-rate adjustment code with a rate", withCode(TaxCode{Code: "A1", Description: "Adjustment", Kind: TaxAdjustment, RatePercent: "5", Account: "140000"},
			func(*TaxCode) {}), "companies[1000].tax_codes[A1].rate_percent"},
		{"single-rate zero code with a rate", withCode(TaxCode{Code: "Z0", Description: "Zero", Kind: TaxExempt, RatePercent: "1", Account: "140000"},
			func(*TaxCode) {}), "companies[1000].tax_codes[Z0].rate_percent"},
		{"single-rate reverse charge", withCode(TaxCode{Code: "R1", Description: "RC", Kind: TaxReverseCharge, RatePercent: "5", Account: "140000"},
			func(*TaxCode) {}), "companies[1000].tax_codes[R1].components"},
		{"self-assessed on a standard code", rc(func(t *TaxCode) { t.Kind = TaxStandard }), "companies[1000].tax_codes[RC].kind"},
		{"self-assessed without its pair", rc(func(t *TaxCode) { t.Components[1].AccountKey = KeyInput }), "companies[1000].tax_codes[RC].components"},
		{"jurisdiction on lines of an input code", gst(func(c *TaxComponent) { c.JurisdictionOnLine = true }), p + ".components[gst].jurisdiction_on_line"},
		{"jurisdiction on lines of a subdivision", pst(func(c *TaxComponent) { c.JurisdictionOnLine = true }), p + ".components[pst_bc].jurisdiction_on_line"},
		{"subdivisions of a component not levying its lines'", gst(func(c *TaxComponent) { c.Subdivisions = []string{"CA-ON"} }),
			p + ".components[gst].subdivisions"},
		{"another country's subdivision", onLine(func(c *TaxComponent) { c.Subdivisions = []string{"AE-DU", "CA-ON"} }), s5 + ".components[vat].subdivisions"},
		{"a country as a subdivision", onLine(func(c *TaxComponent) { c.Subdivisions = []string{"AE"} }), s5 + ".components[vat].subdivisions"},
		{"a malformed subdivision", onLine(func(c *TaxComponent) { c.Subdivisions = []string{"AE-dubai"} }), s5 + ".components[vat].subdivisions"},
		{"a subdivision twice", onLine(func(c *TaxComponent) { c.Subdivisions = []string{"AE-DU", "AE-DU"} }), s5 + ".components[vat].subdivisions"},
		{"reverse charge without a pair", rc(func(t *TaxCode) {
			t.Components[0].AccountKey, t.Components[1].AccountKey = KeyInput, KeyInput
		}), "companies[1000].tax_codes[RC].components"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fixture(t)
			tt.change(c)
			fs := Validate(c, currency.Seeded())
			if !slices.ContainsFunc(fs, func(f Finding) bool { return f.Severity == SevError && f.Path == tt.path }) {
				t.Errorf("findings: got %v, want an error at %s", fs, tt.path)
			}
		})
	}
}

// An import code may carry its self-assessed pair or only input tax.
func TestImportCodes(t *testing.T) {
	for name, keys := range map[string][2]string{"with a pair": {KeySelfOutput, KeySelfInput}, "input only": {KeyInput, KeyInput}} {
		t.Run(name, func(t *testing.T) {
			c := fixture(t)
			withCode(reverseCharge(), func(t *TaxCode) {
				t.Kind = TaxImport
				t.Components[0].AccountKey, t.Components[1].AccountKey = keys[0], keys[1]
			})(c)
			if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
				t.Errorf("findings: got %v, want none", fs)
			}
		})
	}
}

// TestTaxCodeParts: the single-rate form reads as one component, input or
// output by the code's direction, whose undated rate is always in force;
// dated rates are in force from their first through their last day.
func TestTaxCodeParts(t *testing.T) {
	legacy := TaxCode{Code: "V1", RatePercent: "19", Account: "140000"}
	if got := legacy.Parts(); len(got) != 1 || got[0].Key != LegacyComponent || got[0].AccountKey != KeyInput || got[0].Account != "140000" {
		t.Errorf("single-rate parts: got %+v, want one input component tax on 140000", got)
	}
	if r, ok := legacy.Parts()[0].RateOn(time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)); !ok || r != "19" {
		t.Errorf("undated rate: got %q %v, want 19", r, ok)
	}
	out := TaxCode{Code: "A1", Direction: TaxOutput, RatePercent: "5", Account: "211000"}
	if got := out.Parts()[0].AccountKey; got != KeyOutput {
		t.Errorf("output single-rate key: got %s, want output", got)
	}
	if got := (TaxCode{}).KindOrStandard() + " " + (TaxCode{}).DirectionOrInput(); got != "standard input" {
		t.Errorf("defaults: got %s, want standard input", got)
	}
	gst := bcCode().Components[0]
	for _, c := range []struct {
		day, want string
		ok        bool
	}{{"2025-12-31", "", false}, {"2026-01-01", "5", true}, {"2027-03-31", "5", true}, {"2027-04-01", "4", true}, {"2040-01-01", "4", true}} {
		t.Run(c.day, func(t *testing.T) {
			d, _ := time.Parse(time.DateOnly, c.day) // literal dates
			if got, ok := gst.RateOn(d); got != c.want || ok != c.ok {
				t.Errorf("rate on %s: got %q %v, want %q %v", c.day, got, ok, c.want, c.ok)
			}
		})
	}
	for _, c := range []struct {
		component TaxComponent
		want      string
	}{{bcCode().Components[0], "100"}, {bcCode().Components[1], "0"}, {TaxComponent{AccountKey: KeyInput, RecoverablePercent: "50"}, "50"}} {
		if got := c.component.Recoverable(); got != c.want {
			t.Errorf("recoverable of %s: got %s, want %s", c.component.Key, got, c.want)
		}
	}
}

// TestTaxCodeTiers: a component's dated rate added or changed is
// posting-relevant, as rates were; a description is cosmetic. Rates are
// compared by their start, components by key.
func TestTaxCodeTiers(t *testing.T) {
	base := fixture(t)
	withCode(bcCode(), func(*TaxCode) {})(base)
	changed := fixture(t)
	withCode(bcCode(), func(t *TaxCode) {
		t.Description = "GST and PST"
		t.Components[0].Rates[1].ValidTo = "2027-12-31"
		t.Components[0].Rates = append(t.Components[0].Rates, TaxRate{RatePercent: "3", ValidFrom: "2028-01-01"})
	})(changed)
	changes, err := Diff(base, changed)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	got := map[string]Tier{}
	for _, c := range changes {
		got[c.Path] = c.Tier
	}
	p := "companies[1000].tax_codes[I5]"
	want := map[string]Tier{p + ".description": TierCosmetic, p + ".components[gst].rates[2027-04-01].valid_to": TierPostingRelated,
		p + ".components[gst].rates[2028-01-01]": TierPostingRelated}
	for path, tier := range want {
		if got[path] != tier {
			t.Errorf("tier of %s: got %v, want %v (changes %s)", path, got[path], tier, strings.Join(paths(changes), ", "))
		}
	}
	if len(got) != len(want) {
		t.Errorf("changes: got %v, want %v", got, want)
	}
}

func paths(cs []Change) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Path
	}
	return out
}

// TestTaxItemValues: the forms a country pack's return selects tax items
// by (T61h) are the configuration's own.
func TestTaxItemValues(t *testing.T) {
	for name, c := range map[string]struct {
		is   func(string) bool
		good string
		bad  string
	}{
		"kind":          {IsTaxKind, TaxReverseCharge, "reduced"},
		"account key":   {IsTaxAccountKey, KeySelfInput, "tax"},
		"return type":   {IsTaxReturnType, "VAT201", "vat 201"},
		"component key": {IsTaxComponentKey, "pst_bc", "PST"},
		"jurisdiction":  {IsTaxJurisdiction, "AE-DU", "ae"},
	} {
		t.Run(name, func(t *testing.T) {
			if !c.is(c.good) {
				t.Errorf("%q: got false, want true", c.good)
			}
			if c.is(c.bad) {
				t.Errorf("%q: got true, want false", c.bad)
			}
		})
	}
}
