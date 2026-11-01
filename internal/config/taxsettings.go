package config

import (
	"slices"
	"strconv"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// TaxSettings are how a company's tax is converted, rounded and documented
// (ADR-0049 §6, §8): settings of its country, which a country pack brings
// (countrypack.Tax), kept by the company like its tax codes. The zero value
// is the default: the average rate, rounding per document, no documentary
// requirements.
type TaxSettings struct {
	// RateType is the rate type the tax authority converts tax into local
	// currency at (the Bank of Canada's, the UAE Central Bank's, recorded
	// as a ledger rate type), on the tax date; "" is average.
	RateType string `yaml:"rate_type,omitempty" json:"rate_type,omitempty"`
	// Rounding is document (once per document, code and component, the
	// default when "") or line.
	Rounding string `yaml:"rounding,omitempty" json:"rounding,omitempty"`
	// DocumentaryRequirements are what a supplier's document must show for
	// its input tax to be recovered, by the document's total in the
	// company's currency: warnings on the invoice, not refusals (§6).
	DocumentaryRequirements []DocumentaryRequirement `yaml:"documentary_requirements,omitempty" json:"documentary_requirements,omitempty"`
}

// DocumentaryRequirement is one tier of documentary requirements: a
// document whose total, in the company's currency, is at least AtLeast or
// more than Above (exactly one of them) must show what Requires names.
type DocumentaryRequirement struct {
	AtLeast  string   `yaml:"at_least,omitempty" json:"at_least,omitempty"`
	Above    string   `yaml:"above,omitempty" json:"above,omitempty"`
	Requires []string `yaml:"requires" json:"requires"`
	// Source is the law or regulation the tier rests on.
	Source string `yaml:"source" json:"source"`
	// PendingSignOff marks a tier the tax specialist has not yet confirmed
	// (its source was read only through secondary summaries); a pack signed
	// off holds none.
	PendingSignOff bool `yaml:"pending_sign_off,omitempty" json:"pending_sign_off,omitempty"`
}

// What a documentary requirement may name: the ERP checks each against
// what it holds.
const (
	// RequiresSupplierTaxID: the supplier's tax registration number, held
	// as one of its partner's tax identifiers.
	RequiresSupplierTaxID = "supplier_tax_id"
	// RequiresItemDescriptions: a description of each supply, an item's
	// own or its order item's.
	RequiresItemDescriptions = "item_descriptions"
)

// Rounding modes of TaxSettings.Rounding.
const (
	TaxRoundDocument = "document"
	TaxRoundLine     = "line"
)

var documentaryRequirements = []string{RequiresSupplierTaxID, RequiresItemDescriptions}

// RateTypeOrAverage is the rate type tax is converted at, average when none
// is given.
func (s TaxSettings) RateTypeOrAverage() string {
	if s.RateType == "" {
		return "average"
	}
	return s.RateType
}

// IsZero reports whether the settings are all defaults.
func (s TaxSettings) IsZero() bool {
	return s.RateType == "" && s.Rounding == "" && len(s.DocumentaryRequirements) == 0
}

// Threshold is the tier's amount and whether a total equal to it is in the
// tier (AtLeast) or not (Above).
func (r DocumentaryRequirement) Threshold() (amount string, inclusive bool) {
	if r.AtLeast != "" {
		return r.AtLeast, true
	}
	return r.Above, false
}

// CheckTaxSettings validates tax settings at path, their amounts with the
// currency's decimals: a known rate type, document or line rounding, and
// tiers each with one non-negative threshold, known requirements, each
// once, and a source.
func CheckTaxSettings(path string, s TaxSettings, decimals int) []Finding {
	v := &validator{}
	v.taxSettings(path, s, decimals)
	return v.out
}

func (v *validator) taxSettings(path string, s TaxSettings, decimals int) {
	if !slices.Contains(rateTypes, s.RateTypeOrAverage()) {
		v.err(path+".rate_type", "rate type %q must be one of %v", s.RateType, rateTypes)
	}
	if s.Rounding != "" && s.Rounding != TaxRoundDocument && s.Rounding != TaxRoundLine {
		v.err(path+".rounding", "rounding %q must be document or line", s.Rounding)
	}
	for i, r := range s.DocumentaryRequirements {
		rp := path + ".documentary_requirements[" + strconv.Itoa(i) + "]"
		amount, _ := r.Threshold()
		if (r.AtLeast == "") == (r.Above == "") {
			v.err(rp, "exactly one of at_least and above is required")
		} else if m, err := money.ParseDecimal(amount, decimals); err != nil || m < 0 {
			v.err(rp, "threshold %q must be a non-negative amount with at most %d decimals", amount, decimals)
		}
		if len(r.Requires) == 0 {
			v.err(rp+".requires", "at least one of %v is required", documentaryRequirements)
		}
		for j, q := range r.Requires {
			if !slices.Contains(documentaryRequirements, q) || slices.Index(r.Requires, q) != j {
				v.err(rp+".requires", "%q must be one of %v, each once", q, documentaryRequirements)
			}
		}
		if r.Source == "" {
			v.err(rp+".source", "the law or regulation the tier rests on is required")
		}
	}
}
