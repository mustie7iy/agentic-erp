package countrypack

import (
	"reflect"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/packs"
)

// uaeTier is the UAE's documentary tier as the UAE pack (T62f) states it,
// pending the tax specialist (ADR-0049 §6).
func uaeTier() config.DocumentaryRequirement {
	return config.DocumentaryRequirement{Above: "10000", Requires: []string{config.RequiresSupplierTaxID},
		Source: "UAE Executive Regulation (Cabinet Decision No. 52 of 2017) Art. 59", PendingSignOff: true}
}

// withSettings is the test pack's version with tax settings s.
func withSettings(lib *Library, version string, s config.TaxSettings) *Pack {
	p := *lib.Get("ZZ", version)
	p.Tax.Settings = s
	return &p
}

// TestApplyTaxSettings: a pack's tax settings become the company's on
// first adoption; an upgrade takes the pack's change where the document
// kept the adopted pack's, and keeps the document's own with a note.
func TestApplyTaxSettings(t *testing.T) {
	lib := testLibrary(t)
	first := config.TaxSettings{RateType: "bank_selling", DocumentaryRequirements: []config.DocumentaryRequirement{uaeTier()}}
	adopted := withSettings(lib, "2026.1", first)
	lib.packs[adopted.Dir()] = adopted // the base an upgrade merges from
	doc, _, err := Apply(testDoc(), adopted, lib, Options{Company: "C1", On: day("2026-10-20"), FiscalYear: 2026})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := doc.Company("C1").TaxSettings; !reflect.DeepEqual(got, first) {
		t.Fatalf("first adoption: got %+v, want the pack's %+v", got, first)
	}
	next := first
	next.Rounding = config.TaxRoundLine
	t.Run("changed only in the pack", func(t *testing.T) {
		got, _, err := Apply(doc, withSettings(lib, "2026.2", next), lib, Options{Company: "C1", On: day("2026-10-21"), FiscalYear: 2026})
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if s := got.Company("C1").TaxSettings; !reflect.DeepEqual(s, next) {
			t.Errorf("settings: got %+v, want the pack's %+v", s, next)
		}
	})
	t.Run("changed in both", func(t *testing.T) {
		own := *doc
		own.Companies = append([]config.Company(nil), doc.Companies...)
		own.Companies[0].TaxSettings.RateType = "average"
		got, notes, err := Apply(&own, withSettings(lib, "2026.2", next), lib, Options{Company: "C1", On: day("2026-10-21"), FiscalYear: 2026})
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if s := got.Company("C1").TaxSettings; s.RateType != "average" || s.Rounding != "" {
			t.Errorf("settings: got %+v, want the document's", s)
		}
		if !hasNote(notes, "companies[C1].tax_settings[settings]", "conflict") {
			t.Errorf("notes: got %v, want the conflict", notes)
		}
	})
	t.Run("dropped by the pack", func(t *testing.T) {
		got, _, err := Apply(doc, withSettings(lib, "2026.2", config.TaxSettings{}), lib, Options{Company: "C1", On: day("2026-10-21"), FiscalYear: 2026})
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if s := got.Company("C1").TaxSettings; !reflect.DeepEqual(s, first) {
			t.Errorf("settings: got %+v, want the document's kept", s)
		}
	})
}

// TestValidateTaxSettings: a pack's tax settings are checked as a
// document's, in whole units, and a pack signed off holds no tier
// pending sign-off.
func TestValidateTaxSettings(t *testing.T) {
	for name, tc := range map[string]struct {
		s    config.TaxSettings
		path string
	}{
		"rate type":          {config.TaxSettings{RateType: "spot"}, "tax.yaml: tax_settings.rate_type"},
		"decimals":           {config.TaxSettings{DocumentaryRequirements: []config.DocumentaryRequirement{{AtLeast: "100.50", Requires: []string{"supplier_tax_id"}, Source: "s"}}}, "tax.yaml: tax_settings.documentary_requirements[0]"},
		"pending signed off": {config.TaxSettings{DocumentaryRequirements: []config.DocumentaryRequirement{uaeTier()}}, "tax.yaml: tax_settings.documentary_requirements[0].pending_sign_off"},
	} {
		t.Run(name, func(t *testing.T) {
			p := testPack(t, "2026.2") // signed off
			p.Tax.Settings = tc.s
			if fs := Validate(p); !hasError(fs, tc.path) {
				t.Errorf("findings: got %v, want an error at %s", fs, tc.path)
			}
		})
	}
	p := testPack(t, "2026.2")
	p.Manifest.SignOff = nil
	p.Tax.Settings = config.TaxSettings{DocumentaryRequirements: []config.DocumentaryRequirement{uaeTier()}}
	for _, f := range Validate(p) {
		if f.Severity == config.SevError {
			t.Errorf("a pending tier in a pack not signed off: got %v, want no error", f)
		}
	}
}

// Canada 2026.3 is 2026.2 with the tax settings: SOR/91-45 s. 3's $100
// and $500 tiers, the average rate and rounding per document (ADR-0049
// §6, §8).
func TestCanadaPackTaxSettings(t *testing.T) {
	lib, err := LoadAll(packs.FS)
	if err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
	prev, ca := lib.Get("CA", "2026.2"), lib.Get("CA", "2026.3")
	if ca == nil {
		t.Fatal("Get(CA, 2026.3): got nil, want the Canada pack")
	}
	s := ca.Tax.Settings
	if s.RateType != "average" || s.Rounding != config.TaxRoundDocument {
		t.Errorf("rate type and rounding: got %s %s, want average document", s.RateType, s.Rounding)
	}
	var tiers []string
	for _, r := range s.DocumentaryRequirements {
		amount, inclusive := r.Threshold()
		tiers = append(tiers, amount+" "+map[bool]string{true: "at least", false: "above"}[inclusive]+" "+r.Requires[len(r.Requires)-1])
	}
	if want := []string{"100 at least supplier_tax_id", "500 at least item_descriptions"}; !reflect.DeepEqual(tiers, want) {
		t.Errorf("tiers: got %v, want %v", tiers, want)
	}
	if !reflect.DeepEqual(prev.Accounts, ca.Accounts) || !reflect.DeepEqual(prev.Cash, ca.Cash) || !reflect.DeepEqual(prev.Tax.TaxCodes, ca.Tax.TaxCodes) {
		t.Error("2026.3: got accounts, liquidity items or tax codes other than 2026.2's, want them unchanged")
	}
	draft, _, err := Apply(companyIn(t, "CA", "CAD"), ca, lib, Options{Company: "1000", On: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), FiscalYear: 2026})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := draft.Company("1000").TaxSettings; !reflect.DeepEqual(got, s) {
		t.Errorf("company's tax settings: got %+v, want the pack's", got)
	}
}
