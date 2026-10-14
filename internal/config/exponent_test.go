package config

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// A company currency is valid only when the currency reference data knows
// it (ADR-0005: the exponent lives in ref.currency): VND is ISO 4217 but
// not in ref.currency, so its amounts have no known digits.
func TestCurrencyMustBeInTheReferenceData(t *testing.T) {
	c := fixture(t)
	c.Companies[0].Currency = "VND"
	var got []Finding
	for _, f := range Validate(c, currency.Seeded()) {
		if f.Path == "companies[1000].currency" {
			got = append(got, f)
		}
	}
	if len(got) != 1 || got[0].Severity != SevError {
		t.Fatalf("got %v, want one error on companies[1000].currency", got)
	}
}
