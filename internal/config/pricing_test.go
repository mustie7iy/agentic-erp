package config

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// withPricing is the fixture with a price, a customer discount and a
// promotion discount, applied in that order (ADR-0067 §11).
func withPricing(t *testing.T) *Config {
	t.Helper()
	c := fixture(t)
	c.Pricing = Pricing{
		ConditionTypes: []ConditionType{{Code: "PR00", Name: "Price", Kind: ConditionPrice}, {Code: "K007", Name: "Customer discount", Kind: ConditionDiscountPercent},
			{Code: "KA00", Name: "Promotion", Kind: ConditionDiscountPercent}},
		Procedure: []ProcedureStep{{ConditionType: "PR00"}, {ConditionType: "K007"}, {ConditionType: "KA00"}},
	}
	return c
}

// TestPricingValid: condition types with a procedure that names each
// once, a price first, validate, as do types without a procedure and a
// document without the section; each rule refuses the setting's own path
// (T80b acceptance).
func TestPricingValid(t *testing.T) {
	if fs := Validate(withPricing(t), currency.Seeded()); HasErrors(fs) {
		t.Fatalf("a price and two discounts: got %v, want no errors", fs)
	}
	if fs := Validate(fixture(t), currency.Seeded()); HasErrors(fs) {
		t.Fatalf("no pricing section: got %v, want no errors", fs)
	}
	many := make([]ConditionType, MaxConditionTypes+1)
	for i := range many {
		many[i] = ConditionType{Code: fmt.Sprintf("K%03d", i), Name: "Discount", Kind: ConditionDiscountPercent}
	}
	cases := []struct {
		name          string
		change        func(*Pricing)
		path, message string
	}{
		{"types without a procedure", func(p *Pricing) { p.Procedure = nil }, "", ""},
		{"a type left out of the procedure", func(p *Pricing) { p.Procedure = p.Procedure[:2] }, "", ""},
		{"a code in small letters", func(p *Pricing) { p.ConditionTypes[1].Code = "k007" }, "pricing.condition_types[1].code", "1-10 capital letters"},
		{"no code", func(p *Pricing) { p.ConditionTypes[2].Code = "" }, "pricing.condition_types[2].code", "1-10 capital letters"},
		{"a code of eleven characters", func(p *Pricing) { p.ConditionTypes[2].Code = "K0000000007" }, "pricing.condition_types[2].code", "1-10 capital letters"},
		{"a code twice", func(p *Pricing) { p.ConditionTypes[2].Code = "K007" }, "pricing.condition_types[K007]", "duplicate condition type code"},
		{"no name", func(p *Pricing) { p.ConditionTypes[0].Name = "  " }, "pricing.condition_types[PR00].name", "is required"},
		{"a name of 61 characters", func(p *Pricing) { p.ConditionTypes[0].Name = strings.Repeat("n", 61) }, "pricing.condition_types[PR00].name", "at most 60"},
		{"an unknown kind", func(p *Pricing) { p.ConditionTypes[1].Kind = "surcharge" }, "pricing.condition_types[K007].kind", "must be price or discount_percent"},
		{"too many types", func(p *Pricing) { p.ConditionTypes = many; p.Procedure = nil }, "pricing.condition_types", "at most 50 condition types"},
		{"a step of no type", func(p *Pricing) { p.Procedure[2].ConditionType = "ZZ" }, "pricing.procedure[2].condition_type", `"ZZ" is not a condition type`},
		{"a type named twice", func(p *Pricing) { p.Procedure[2].ConditionType = "K007" }, "pricing.procedure[2].condition_type", "each condition type once"},
		{"a discount first", func(p *Pricing) { p.Procedure = p.Procedure[1:] }, "pricing.procedure[0].condition_type", "starts with a condition type of kind price"},
		{"a second price", func(p *Pricing) {
			p.ConditionTypes = append(p.ConditionTypes, ConditionType{Code: "PR01", Name: "List price", Kind: ConditionPrice})
			p.Procedure = append(p.Procedure, ProcedureStep{ConditionType: "PR01"})
		}, "pricing.procedure[3].condition_type", "one price, its first step"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := withPricing(t)
			tc.change(&c.Pricing)
			fs := Validate(c, currency.Seeded())
			if tc.path == "" {
				if HasErrors(fs) {
					t.Errorf("findings: got %v, want no errors", fs)
				}
				return
			}
			if !slices.ContainsFunc(fs, func(f Finding) bool {
				return f.Severity == SevError && f.Path == tc.path && strings.Contains(f.Message, tc.message)
			}) {
				t.Errorf("findings: got %v, want an error at %s saying %q", fs, tc.path, tc.message)
			}
		})
	}
}

// TestPricingSteps: the procedure's condition types in its order, none
// for a tenant without a procedure, and a type by its code.
func TestPricingSteps(t *testing.T) {
	p := withPricing(t).Pricing
	var codes []string
	for _, s := range p.Steps() {
		codes = append(codes, s.Code+" "+s.Kind)
	}
	if got, want := strings.Join(codes, ", "), "PR00 price, K007 discount_percent, KA00 discount_percent"; got != want {
		t.Errorf("steps: got %s, want %s", got, want)
	}
	if got := len(fixture(t).Pricing.Steps()); got != 0 {
		t.Errorf("steps without a procedure: got %d, want none", got)
	}
	if ct, ok := p.ConditionType("K007"); !ok || ct.Name != "Customer discount" {
		t.Errorf("condition type K007: got %v %v, want Customer discount", ct, ok)
	}
	if _, ok := p.ConditionType("ZZ"); ok {
		t.Error("condition type ZZ: got one, want none")
	}
	// A step of no configured type is left out: validation refuses the
	// document, and a reader of one that slipped through prices without it.
	p.Procedure = append(p.Procedure, ProcedureStep{ConditionType: "ZZ"})
	if got := len(p.Steps()); got != 3 {
		t.Errorf("steps with one of no type: got %d, want 3", got)
	}
}

// TestPricingTransitions: a condition type is never removed once
// activated and keeps its kind; its name may change, it may leave the
// procedure, and types may be added (T80b acceptance).
func TestPricingTransitions(t *testing.T) {
	for name, c := range map[string]struct {
		base   bool
		change func(*Pricing)
		path   string // of the expected error; "" when none
	}{
		"first version": {false, func(*Pricing) {}, ""},
		"unchanged":     {true, func(*Pricing) {}, ""},
		"renamed":       {true, func(p *Pricing) { p.ConditionTypes[1].Name = "Loyalty discount" }, ""},
		"added": {true, func(p *Pricing) {
			p.ConditionTypes = append(p.ConditionTypes, ConditionType{Code: "KB00", Name: "Volume", Kind: ConditionDiscountPercent})
		}, ""},
		"out of the procedure": {true, func(p *Pricing) { p.Procedure = p.Procedure[:2] }, ""},
		"removed": {true, func(p *Pricing) {
			p.ConditionTypes, p.Procedure = p.ConditionTypes[:2], p.Procedure[:2]
		}, "pricing.condition_types[KA00]"},
		"the section removed": {true, func(p *Pricing) { *p = Pricing{} }, "pricing.condition_types[PR00]"},
		"another kind":        {true, func(p *Pricing) { p.ConditionTypes[2].Kind = ConditionPrice }, "pricing.condition_types[KA00].kind"},
	} {
		t.Run(name, func(t *testing.T) {
			var base *Config
			if c.base {
				base = withPricing(t)
			}
			proposed := withPricing(t)
			c.change(&proposed.Pricing)
			fs := PricingTransitions(base, proposed)
			if c.path == "" && len(fs) != 0 {
				t.Errorf("got %v, want none", fs)
			}
			if c.path != "" && !slices.ContainsFunc(fs, func(f Finding) bool { return f.Severity == SevError && f.Path == c.path }) {
				t.Errorf("got %v, want an error at %s", fs, c.path)
			}
		})
	}
}

// TestPricingTier: the pricing section posts nothing: it decides the net
// value new sales items take, as payment terms decide due dates, so its
// changes are tier 2, operational, and a type's name is cosmetic
// (ADR-0067 §11).
func TestPricingTier(t *testing.T) {
	renamed := withPricing(t)
	renamed.Pricing.ConditionTypes[1].Name = "Loyalty discount"
	swapped := withPricing(t)
	swapped.Pricing.Procedure[1], swapped.Pricing.Procedure[2] = swapped.Pricing.Procedure[2], swapped.Pricing.Procedure[1]
	cases := []struct {
		name      string
		old, new  *Config
		tier      Tier
		wantPaths string
	}{
		{"set", fixture(t), withPricing(t), TierOperational,
			"pricing.condition_types[K007] pricing.condition_types[KA00] pricing.condition_types[PR00] pricing.procedure[0] pricing.procedure[1] pricing.procedure[2]"},
		{"a name", withPricing(t), renamed, TierCosmetic, "pricing.condition_types[K007].name"},
		{"the order", withPricing(t), swapped, TierOperational, "pricing.procedure[1].condition_type pricing.procedure[2].condition_type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diffs, _ := Diff(tc.old, tc.new) // Diff never fails
			var paths []string
			for _, d := range diffs {
				paths = append(paths, d.Path)
				if d.Tier != tc.tier {
					t.Errorf("tier of %s: got %v, want %v", d.Path, d.Tier, tc.tier)
				}
			}
			slices.Sort(paths)
			if got := strings.Join(paths, " "); got != tc.wantPaths {
				t.Errorf("changes: got %s, want %s", got, tc.wantPaths)
			}
		})
	}
}
