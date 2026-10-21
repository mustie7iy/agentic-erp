package countrypack

import (
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
)

// vat is the test pack's first tax code's component.
func vat(p *Pack) *config.TaxComponent { return &p.Tax.TaxCodes[0].Components[0] }

// testPack is a valid pack, loaded fresh for each mutation.
func testPack(t *testing.T, version string) *Pack {
	t.Helper()
	p, _, err := Load(testFS(t), "zz/"+version)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return p
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		change func(p *Pack)
		path   string
	}{
		{"country", func(p *Pack) { p.Manifest.Country = "zz" }, "pack.yaml: country"},
		{"version", func(p *Pack) { p.Manifest.Version = "2026" }, "pack.yaml: version"},
		{"name", func(p *Pack) { p.Manifest.Name = " " }, "pack.yaml: name"},
		{"valid from", func(p *Pack) { p.Manifest.ValidFrom = "1 Jan 2026" }, "pack.yaml: valid_from"},
		{"no sources", func(p *Pack) { p.Manifest.Sources = nil }, "pack.yaml: sources"},
		{"source without https", func(p *Pack) { p.Manifest.Sources[0].URL = "http://example.org" }, "pack.yaml: sources[0]"},
		{"source retrieved", func(p *Pack) { p.Manifest.Sources[0].Retrieved = "" }, "pack.yaml: sources[0].retrieved"},
		{"sign-off date", func(p *Pack) { p.Manifest.SignOff.Date = "soon" }, "pack.yaml: sign_off.date"},
		{"duplicate account", func(p *Pack) { p.Accounts.Accounts[1].Number = "1000" }, "accounts.yaml: accounts[1000]"},
		{"account name", func(p *Pack) { p.Accounts.Accounts[0].Name = "" }, "accounts.yaml: accounts[1000].name"},
		{"account type", func(p *Pack) { p.Accounts.Accounts[0].Type = "cash" }, "accounts.yaml: accounts[1000].type"},
		{"cost element", func(p *Pack) { p.Accounts.Accounts[0].CostElement = config.CostElementPrimaryCost }, "accounts.yaml: accounts[1000].cost_element"},
		{"determination unresolved", func(p *Pack) { p.Accounts.AccountDetermination.Bank = "1999" }, "accounts.yaml: account_determination.bank"},
		{"duplicate tax code", func(p *Pack) { p.Tax.TaxCodes[1].Code = "V1" }, "tax.yaml: tax_codes[V1]"},
		{"tax description", func(p *Pack) { p.Tax.TaxCodes[0].Description = "" }, "tax.yaml: tax_codes[V1].description"},
		{"tax account unresolved", func(p *Pack) { vat(p).Account = "1999" }, "tax.yaml: tax_codes[V1].components[vat].account"},
		{"single-rate form", func(p *Pack) {
			p.Tax.TaxCodes[0].Components, p.Tax.TaxCodes[0].RatePercent, p.Tax.TaxCodes[0].Account = nil, "15", "1400"
		}, "tax.yaml: tax_codes[V1].components"},
		{"no rates", func(p *Pack) { vat(p).Rates = nil }, "tax.yaml: tax_codes[V1].components[vat].rates"},
		{"rate above 100", func(p *Pack) { vat(p).Rates[0].RatePercent = "100.001" }, "tax.yaml: tax_codes[V1].components[vat].rates[2020-01-01].rate_percent"},
		{"rate ends before it starts", func(p *Pack) { vat(p).Rates[0].ValidTo = "2019-12-31" }, "tax.yaml: tax_codes[V1].components[vat].rates[2020-01-01].valid_to"},
		{"rates overlap", func(p *Pack) { vat(p).Rates[0].ValidTo = "2027-01-01" }, "tax.yaml: tax_codes[V1].components[vat].rates"},
		{"zero code with a rate", func(p *Pack) { p.Tax.TaxCodes[1].Components[0].Rates[0].RatePercent = "1" },
			"tax.yaml: tax_codes[V0].components[vat].rates[2020-01-01].rate_percent"},
		{"holiday date", func(p *Pack) { p.Calendar.Holidays[0].Date = "2026-02-30" }, "calendar.yaml: holidays[0].date"},
		{"duplicate holiday", func(p *Pack) { p.Calendar.Holidays[1].Date = "2026-01-01" }, "calendar.yaml: holidays[1]"},
		{"holiday name", func(p *Pack) { p.Calendar.Holidays[0].Name = "" }, "calendar.yaml: holidays[0].name"},
		{"document type", func(p *Pack) { p.Numbering.NumberRanges[1].DocumentType = "supplier_invoice" }, "numbering.yaml: number_ranges[supplier_invoice]"},
		{"prefix", func(p *Pack) { p.Numbering.NumberRanges[0].Prefix = "si" }, "numbering.yaml: number_ranges[supplier_invoice].prefix"},
		{"range bounds", func(p *Pack) { p.Numbering.NumberRanges[0].Last = 0 }, "numbering.yaml: number_ranges[supplier_invoice]"},
		{"duplicate payment term", func(p *Pack) { p.Payments.PaymentTerms = append(p.Payments.PaymentTerms, p.Payments.PaymentTerms[0]) }, "payments.yaml: payment_terms[NET30]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := testPack(t, "2026.1")
			tt.change(p)
			fs := Validate(p)
			if !hasError(fs, tt.path) {
				t.Errorf("Validate: got %v, want an error at %q", fs, tt.path)
			}
		})
	}
}

func hasError(fs []config.Finding, path string) bool {
	for _, f := range fs {
		if f.Severity == config.SevError && f.Path == path {
			return true
		}
	}
	return false
}

func TestValidPackHasNoFindings(t *testing.T) {
	for _, v := range []string{"2026.1", "2026.2"} {
		t.Run(v, func(t *testing.T) {
			if fs := Validate(testPack(t, v)); len(fs) != 0 {
				t.Errorf("Validate: got %v, want no findings", fs)
			}
		})
	}
}

func TestParse(t *testing.T) {
	files := func() map[string][]byte {
		m := map[string][]byte{}
		for _, f := range Files {
			m[f] = nil // empty files are empty sections
		}
		return m
	}
	if _, err := Parse(files()); err != nil {
		t.Errorf("Parse of empty files: got %v, want nil", err)
	}
	tests := []struct {
		name   string
		change func(m map[string][]byte)
		want   string
	}{
		{"unknown file", func(m map[string][]byte) { m["vat.yaml"] = nil }, "unknown file vat.yaml"},
		{"missing file", func(m map[string][]byte) { delete(m, "tax.yaml") }, "missing file tax.yaml"},
		{"unknown field", func(m map[string][]byte) { m["tax.yaml"] = []byte("tax_code: []\n") }, "tax.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := files()
			tt.change(m)
			if _, err := Parse(m); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse: got %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

func TestVersions(t *testing.T) {
	for v, want := range map[string]bool{"2026.1": true, "2026.12": true, "2026": false, "2026.0": false, "2026.01": false,
		"202.1": false, "abcd.1": false, "2026.x": false, "1899.1": false, "+026.1": false, "2026.+1": false} {
		t.Run(v, func(t *testing.T) {
			if _, _, ok := ParseVersion(v); ok != want {
				t.Errorf("ParseVersion(%q): got %v, want %v", v, ok, want)
			}
		})
	}
	for _, c := range []struct {
		a, b string
		want int
	}{{"2026.2", "2026.10", -1}, {"2027.1", "2026.9", 1}, {"2026.3", "2026.3", 0}} {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%s, %s): got %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
