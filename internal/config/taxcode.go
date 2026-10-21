package config

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// TaxCode is a tax code of a company (SAP T007A, ADR-0049 §3): its
// direction, kind and UN/CEFACT category, and its components, each with
// dated rates. Before T61a a code had one rate and one account; that form
// (RatePercent and Account, no Components) still reads as one input
// component, LegacyComponent, whose rate has no dates: the ledger keeps it
// in force from the company's first fiscal period and starts a changed rate
// on the day the configuration is activated (ledger.Setup).
type TaxCode struct {
	Code        string `yaml:"code" json:"code"`
	Description string `yaml:"description" json:"description"`
	// Direction is TaxInput (also when empty) or TaxOutput.
	Direction string `yaml:"direction,omitempty" json:"direction,omitempty"`
	// Kind is TaxStandard (also when empty), zero, exempt, out_of_scope,
	// reverse_charge or import.
	Kind string `yaml:"kind,omitempty" json:"kind,omitempty"`
	// Category is the UN/CEFACT 5305 tax category an e-invoice carries
	// (S, Z, E, AE, O, ...; T65), none when empty.
	Category    string         `yaml:"category,omitempty" json:"category,omitempty"`
	RatePercent string         `yaml:"rate_percent,omitempty" json:"rate_percent,omitempty"`
	Account     string         `yaml:"account,omitempty" json:"account,omitempty"`
	Components  []TaxComponent `yaml:"components,omitempty" json:"components,omitempty"`
}

// TaxComponent is one tax a code levies (SAP's condition type): GST and BC
// PST are two components of one code. Returns map components, not codes.
type TaxComponent struct {
	Key string `yaml:"key" json:"key"`
	// Jurisdiction is the country or subdivision that levies it (ISO 3166:
	// CA, CA-BC, AE-DU); empty when the code's country is meant.
	Jurisdiction string `yaml:"jurisdiction,omitempty" json:"jurisdiction,omitempty"`
	// AccountKey says where the tax posts (SAP MWS, VST, NVV, ESA/ESE).
	AccountKey string `yaml:"account_key" json:"account_key"`
	// RecoverablePercent is the share of input tax that is recoverable: 100
	// when empty, 0 for a non-deductible component (P8: a fixed share).
	RecoverablePercent string `yaml:"recoverable_percent,omitempty" json:"recoverable_percent,omitempty"`
	// Account is the tax account; a non-deductible component has none, its
	// tax is added to the base line's account.
	Account string    `yaml:"account,omitempty" json:"account,omitempty"`
	Rates   []TaxRate `yaml:"rates" json:"rates"`
}

// TaxRate is a rate in force from ValidFrom through ValidTo (inclusive;
// open when empty). Dates are YYYY-MM-DD. The undated rate of the
// single-rate form has neither.
type TaxRate struct {
	RatePercent string `yaml:"rate_percent" json:"rate_percent"`
	ValidFrom   string `yaml:"valid_from" json:"valid_from"`
	ValidTo     string `yaml:"valid_to,omitempty" json:"valid_to,omitempty"`
}

// Tax code directions, kinds and account keys (ADR-0049 §3).
const (
	TaxInput  = "input"
	TaxOutput = "output"

	TaxStandard      = "standard"
	TaxZero          = "zero"
	TaxExempt        = "exempt"
	TaxOutOfScope    = "out_of_scope"
	TaxReverseCharge = "reverse_charge"
	TaxImport        = "import"

	KeyInput         = "input"
	KeyOutput        = "output"
	KeyNonDeductible = "non_deductible"
	KeySelfOutput    = "self_output"
	KeySelfInput     = "self_input"

	// LegacyComponent is the one component of a code in the single-rate form.
	LegacyComponent = "tax"
)

var (
	taxCodeRe      = regexp.MustCompile(`^[A-Z0-9]{1,4}$`)
	componentRe    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,19}$`)
	jurisdictionRe = regexp.MustCompile(`^[A-Z]{2}(-[A-Z0-9]{1,3})?$`)
	taxKinds       = []string{TaxStandard, TaxZero, TaxExempt, TaxOutOfScope, TaxReverseCharge, TaxImport}
	// The UN/CEFACT 5305 categories EN 16931 and PINT use.
	taxCategories = []string{"S", "Z", "E", "AE", "K", "G", "O", "L", "M", "B"}
)

// DirectionOrInput is the code's direction, input when none is given.
func (t TaxCode) DirectionOrInput() string {
	if t.Direction == "" {
		return TaxInput
	}
	return t.Direction
}

// KindOrStandard is the code's kind, standard when none is given.
func (t TaxCode) KindOrStandard() string {
	if t.Kind == "" {
		return TaxStandard
	}
	return t.Kind
}

// Parts are the code's components: its own, or the one of the single-rate
// form, which posts to the code's account with an undated rate.
func (t TaxCode) Parts() []TaxComponent {
	if len(t.Components) > 0 {
		return t.Components
	}
	key := KeyInput
	if t.DirectionOrInput() == TaxOutput {
		key = KeyOutput
	}
	return []TaxComponent{{Key: LegacyComponent, AccountKey: key, Account: t.Account, Rates: []TaxRate{{RatePercent: t.RatePercent}}}}
}

// Recoverable is the component's recoverable share in percent: as given,
// else 100, or 0 for a non-deductible component.
func (c TaxComponent) Recoverable() string {
	switch {
	case c.RecoverablePercent != "":
		return c.RecoverablePercent
	case c.AccountKey == KeyNonDeductible:
		return "0"
	}
	return "100"
}

// RateOn is the component's rate in force on the date; an undated rate is
// in force on every date.
func (c TaxComponent) RateOn(d time.Time) (string, bool) {
	day := d.Format(time.DateOnly)
	for _, r := range c.Rates {
		if (r.ValidFrom == "" || r.ValidFrom <= day) && (r.ValidTo == "" || day <= r.ValidTo) {
			return r.RatePercent, true
		}
	}
	return "", false
}

// CheckTaxCodes checks a list of tax codes on their own (ADR-0049 §3), for
// config.Validate and countrypack.Validate: unique codes of 1-4 upper-case
// letters or digits, a known direction, kind and category, and either the
// single-rate form or components, each checked by checkComponent. prefix
// is the list's path; account reports what is wrong with an account
// number a component posts to, "" when nothing is.
func CheckTaxCodes(prefix string, codes []TaxCode, account func(number string) string) []Finding {
	v := &validator{}
	seen := map[string]bool{}
	for _, t := range codes {
		tp := prefix + "[" + t.Code + "]"
		if !taxCodeRe.MatchString(t.Code) || seen[t.Code] {
			v.err(tp, "a tax code is 1-4 upper-case letters or digits, unique in the company")
		}
		seen[t.Code] = true
		v.taxCode(tp, t, account)
	}
	return v.out
}

func (v *validator) taxCode(tp string, t TaxCode, account func(string) string) {
	if d := t.DirectionOrInput(); d != TaxInput && d != TaxOutput {
		v.err(tp+".direction", "input or output")
	}
	kind := t.KindOrStandard()
	if !slices.Contains(taxKinds, kind) {
		v.err(tp+".kind", "one of %s", strings.Join(taxKinds, ", "))
	}
	if t.Category != "" && !slices.Contains(taxCategories, t.Category) {
		v.err(tp+".category", "a UN/CEFACT 5305 tax category: %s", strings.Join(taxCategories, ", "))
	}
	if len(t.Components) == 0 {
		v.singleRate(tp, t, account)
	} else {
		if t.RatePercent != "" || t.Account != "" {
			v.err(tp, "a tax code has either rate_percent and account or components, not both")
		}
		v.components(tp, t, account)
	}
}

// singleRate checks the single-rate form: a rate below 100% with up
// to three decimals and an account; it is one input or output component,
// so no reverse charge.
func (v *validator) singleRate(tp string, t TaxCode, account func(string) string) {
	if r, ok := v.amount(tp+".rate_percent", t.RatePercent, 3); ok && r >= 100_000 {
		v.err(tp+".rate_percent", "a rate below 100%%")
	}
	if msg := account(t.Account); msg != "" {
		v.err(tp+".account", "%s", msg)
	}
	switch t.KindOrStandard() {
	case TaxReverseCharge:
		v.err(tp+".components", "a reverse_charge code needs self_output and self_input components")
	case TaxZero, TaxExempt, TaxOutOfScope:
		if r, err := money.ParseDecimal(t.RatePercent, 3); err == nil && r != 0 {
			v.err(tp+".rate_percent", "a %s code has a rate of 0", t.KindOrStandard())
		}
	}
}

// components checks a code's components: unique keys, account keys of the
// code's direction, self-assessed pairs only and always on reverse charge
// and import codes, and each component's own settings.
func (v *validator) components(tp string, t TaxCode, account func(string) string) {
	allowed := []string{KeyInput, KeyNonDeductible, KeySelfOutput, KeySelfInput}
	if t.DirectionOrInput() == TaxOutput {
		allowed = []string{KeyOutput}
	}
	kind := t.KindOrStandard()
	seen := map[string]bool{}
	pairs := map[string]int{}
	for _, c := range t.Components {
		cp := tp + ".components[" + c.Key + "]"
		if !componentRe.MatchString(c.Key) || seen[c.Key] {
			v.err(cp, "a component key is 1-20 lower-case letters, digits or underscores starting with a letter, unique in the code")
		}
		seen[c.Key] = true
		if !slices.Contains(allowed, c.AccountKey) {
			v.err(cp+".account_key", "a %s code's components are %s", t.DirectionOrInput(), strings.Join(allowed, ", "))
		}
		pairs[c.AccountKey]++
		v.component(cp, kind, c, account)
	}
	selfAssessed := kind == TaxReverseCharge || kind == TaxImport
	switch {
	case !selfAssessed && pairs[KeySelfOutput]+pairs[KeySelfInput] > 0:
		v.err(tp+".kind", "self_output and self_input components are for reverse_charge and import codes")
	case pairs[KeySelfOutput] != pairs[KeySelfInput]:
		v.err(tp+".components", "self_output and self_input components come in pairs")
	case kind == TaxReverseCharge && pairs[KeySelfOutput] == 0:
		v.err(tp+".components", "a reverse_charge code needs self_output and self_input components")
	}
}

// component checks one component: its jurisdiction, recoverable share, its
// account (none when non-deductible) and its rates.
func (v *validator) component(cp, kind string, c TaxComponent, account func(string) string) {
	if c.Jurisdiction != "" && !jurisdictionRe.MatchString(c.Jurisdiction) {
		v.err(cp+".jurisdiction", "an ISO 3166 country or subdivision code (CA, CA-BC)")
	}
	if r, ok := v.amount(cp+".recoverable_percent", c.Recoverable(), 3); ok && r > 100_000 {
		v.err(cp+".recoverable_percent", "a share from 0 to 100%%")
	}
	switch {
	case c.AccountKey == KeyNonDeductible && c.Account != "":
		v.err(cp+".account", "a non-deductible component has no account: its tax is added to the base line's")
	case c.AccountKey == KeyNonDeductible && c.Recoverable() != "0":
		v.err(cp+".recoverable_percent", "a non-deductible component recovers nothing")
	case c.AccountKey != KeyNonDeductible:
		if msg := account(c.Account); msg != "" {
			v.err(cp+".account", "%s", msg)
		}
	}
	v.rates(cp+".rates", kind, c.Rates)
}

// period is the dates a rate is in force.
type period struct {
	from, to time.Time
	open     bool
}

// rates checks each rate (rate) and that they do not overlap: in date
// order, each ends before the next begins.
func (v *validator) rates(p, kind string, rates []TaxRate) {
	if len(rates) == 0 {
		v.err(p, "at least one rate is required")
	}
	var periods []period
	for _, r := range rates {
		if per, ok := v.rate(p+"["+r.ValidFrom+"]", kind, r); ok {
			periods = append(periods, per)
		}
	}
	slices.SortFunc(periods, func(a, b period) int { return a.from.Compare(b.from) })
	for i := 1; i < len(periods); i++ {
		if prev := periods[i-1]; prev.open || !prev.to.Before(periods[i].from) {
			v.err(p, "rates overlap: one in force from %s has not ended by %s", prev.from.Format(time.DateOnly), periods[i].from.Format(time.DateOnly))
		}
	}
}

// rate checks one rate, 0 to below 100% with up to three decimals (0 on a zero,
// exempt or out-of-scope code) and its dates, and returns its period when
// the dates read.
func (v *validator) rate(rp, kind string, r TaxRate) (period, bool) {
	switch m, err := money.ParseDecimal(r.RatePercent, 3); {
	case err != nil || m < 0 || m >= 100_000: // the ledger keeps rates below 100% (ledger.tax_rate)
		v.err(rp+".rate_percent", "rate %q must be a percentage from 0 to below 100 with at most three decimals", r.RatePercent)
	case m != 0 && (kind == TaxZero || kind == TaxExempt || kind == TaxOutOfScope):
		v.err(rp+".rate_percent", "a %s code has a rate of 0", kind)
	}
	from, ok := v.date(rp+".valid_from", r.ValidFrom)
	if !ok {
		return period{}, false
	}
	per := period{from: from, open: r.ValidTo == ""}
	if per.open {
		return per, true
	}
	if per.to, ok = v.date(rp+".valid_to", r.ValidTo); !ok {
		return period{}, false
	}
	if per.to.Before(from) {
		v.err(rp+".valid_to", "valid_to %s is before valid_from %s", r.ValidTo, r.ValidFrom)
	}
	return per, true
}

// date parses a YYYY-MM-DD date, reporting it at path when it is not one.
func (v *validator) date(path, s string) (time.Time, bool) {
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		v.err(path, "%q is not a date (YYYY-MM-DD)", s)
		return time.Time{}, false
	}
	return d, true
}

// taxCodes checks the company's tax codes (CheckTaxCodes) with accounts of
// its chart other than the GR/IR account, which only goods receipts and
// invoices' order items post.
func (v *validator) taxCodes(p string, co *Company) {
	v.out = append(v.out, CheckTaxCodes(p+".tax_codes", co.TaxCodes, func(number string) string {
		switch {
		case co.Account(number) == nil:
			return fmt.Sprintf("account %s is not in the chart of accounts", number)
		case number == co.AccountDetermination.GRIR:
			return fmt.Sprintf("account %s is the GR/IR account, which only goods receipts and invoices' order items post", number)
		}
		return ""
	})...)
	if len(co.TaxCodes) == 0 {
		v.warn(p+".tax_codes", "no tax codes; captured invoices cannot be checked for tax rate")
	}
}
