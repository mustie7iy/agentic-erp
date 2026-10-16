package config

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// A requisition approval threshold is read with the digits of every
// company's currency: a fractional JPY threshold cannot be read (and an
// approval routing that drops the failed parse misroutes), while a BHD
// threshold may use its three decimals.
func TestThresholdsUseTheCompanyCurrencyDigits(t *testing.T) {
	for _, c := range []struct {
		name, currency, upTo string
		refused              bool
	}{
		{"JPY with decimals", "JPY", "1000.50", true},
		{"JPY whole", "JPY", "1000", false},
		{"BHD with three decimals", "BHD", "1000.125", false},
		{"BHD with four", "BHD", "1000.1255", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			doc := fixture(t)
			for i := range doc.Companies {
				doc.Companies[i].Currency = c.currency
				doc.Companies[i].AutoPostLimit, doc.Companies[i].InvoiceTolerance.PriceAbsolute = "1000", "5"
				doc.Companies[i].ManualEntryApprovalLimit = "1000"
			}
			doc.Purchasing.RequisitionApproval[0].UpTo = c.upTo
			var got []Finding
			for _, f := range Validate(doc, currency.Seeded()) {
				if f.Severity == SevError {
					got = append(got, f)
				}
			}
			if refused := len(got) == 1 && got[0].Path == "purchasing.requisition_approval[0].up_to"; refused != c.refused || (!c.refused && len(got) > 0) {
				t.Errorf("got errors %v, want up_to refused %v", got, c.refused)
			}
		})
	}
}

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
