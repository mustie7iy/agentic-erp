package countrypack

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/config"
)

var (
	countryRe      = regexp.MustCompile(`^[A-Z]{2}$`)
	documentTypeRe = regexp.MustCompile(`^[a-z_]+$`)
	prefixRe       = regexp.MustCompile(`^[A-Z0-9-]{0,10}$`)
)

// Validate checks a pack on its own: everything in it that a document's
// validation (config.Validate) cannot see once the pack is applied, such as
// rate schedules and sources, and the references inside it. Settings that
// arrive in the document unchanged (a payment term's discounts, say) are
// checked again by config.Validate when the change is submitted.
func Validate(p *Pack) []config.Finding {
	v := &validator{}
	v.manifest(&p.Manifest)
	v.accounts(&p.Accounts)
	v.taxCodes(p)
	v.holidays(p.Calendar.Holidays)
	v.numberRanges(p.Numbering.NumberRanges)
	v.paymentTerms(p.Payments.PaymentTerms)
	v.out = append(v.out, config.CheckLiquidityItems("cash.yaml: liquidity_items", p.Cash.LiquidityItems)...)
	return v.out
}

type validator struct{ out []config.Finding }

func (v *validator) err(path, format string, a ...any) {
	v.out = append(v.out, config.Finding{Severity: config.SevError, Path: path, Message: fmt.Sprintf(format, a...)})
}

// date parses a YYYY-MM-DD date, reporting it at path when it is not one.
func (v *validator) date(path, s string) {
	if _, err := time.Parse(time.DateOnly, s); err != nil {
		v.err(path, "%q is not a date (YYYY-MM-DD)", s)
	}
}

func (v *validator) manifest(m *Manifest) {
	if !countryRe.MatchString(m.Country) {
		v.err("pack.yaml: country", "country %q must be an ISO 3166 code of two upper-case letters", m.Country)
	}
	if _, _, ok := ParseVersion(m.Version); !ok {
		v.err("pack.yaml: version", "version %q must be YYYY.N", m.Version)
	}
	if strings.TrimSpace(m.Name) == "" {
		v.err("pack.yaml: name", "name is required")
	}
	v.date("pack.yaml: valid_from", m.ValidFrom)
	if len(m.Sources) == 0 {
		v.err("pack.yaml: sources", "at least one source is required: every setting rests on a law or an authority's publication")
	}
	for i, s := range m.Sources {
		p := fmt.Sprintf("pack.yaml: sources[%d]", i)
		if strings.TrimSpace(s.Title) == "" || !strings.HasPrefix(s.URL, "https://") {
			v.err(p, "a source needs a title and an https URL")
		}
		v.date(p+".retrieved", s.Retrieved)
	}
	switch {
	case !m.signedOff():
		v.out = append(v.out, config.Finding{Severity: config.SevWarning, Path: "pack.yaml: sign_off",
			Message: "no tax specialist has signed the pack off; it may not be used in the pilot"})
	default:
		v.date("pack.yaml: sign_off.date", m.SignOff.Date)
	}
}

func (v *validator) accounts(a *Accounts) {
	seen := map[string]bool{}
	for _, acc := range a.Accounts {
		p := "accounts.yaml: accounts[" + acc.Number + "]"
		if acc.Number == "" || seen[acc.Number] {
			v.err(p, "account number must be present and unique")
		}
		seen[acc.Number] = true
		if strings.TrimSpace(acc.Name) == "" {
			v.err(p+".name", "name is required")
		}
		if !slices.Contains([]config.AccountType{config.Asset, config.Liability, config.Equity, config.Revenue, config.Expense}, acc.Type) {
			v.err(p+".type", "type %q must be asset, liability, equity, revenue or expense", acc.Type)
		}
		// A pack sets none for the accounts it does not control (ADR-0050 §4).
		if msg := acc.CostElementProblem(); msg != "" {
			v.err(p+".cost_element", "%s", msg)
		}
	}
	ad := determination(&a.AccountDetermination)
	for _, field := range slices.Sorted(maps.Keys(ad)) {
		if number := *ad[field]; number != "" && !seen[number] {
			v.err("accounts.yaml: account_determination."+field, "account %s is not in the pack's accounts", number)
		}
	}
}

// determination is the account determination by field name: what account
// references are checked against and what applying a pack merges field by
// field.
func determination(ad *config.AccountDetermination) map[string]*string {
	return map[string]*string{
		"ap_reconciliation": &ad.APReconciliation, "input_tax": &ad.InputTax, "bank": &ad.Bank,
		"fx_gain": &ad.FXGain, "fx_loss": &ad.FXLoss, "gr_ir": &ad.GRIR, "price_difference": &ad.PriceDifference,
		"cash_discount": &ad.CashDiscount, "down_payment": &ad.DownPayment,
	}
}

// taxCodes checks the pack's tax codes as the configuration does
// (config.CheckTaxCodes), with accounts of the pack, and that each has a
// description and components: a pack names every rate with its dates, so
// the undated single-rate form is not one of its.
func (v *validator) taxCodes(p *Pack) {
	accounts := map[string]bool{}
	for _, a := range p.Accounts.Accounts {
		accounts[a.Number] = true
	}
	v.out = append(v.out, config.CheckTaxCodes("tax.yaml: tax_codes", p.Tax.TaxCodes, func(number string) string {
		if !accounts[number] {
			return "account " + number + " is not in the pack's accounts"
		}
		return ""
	})...)
	for _, t := range p.Tax.TaxCodes {
		tp := "tax.yaml: tax_codes[" + t.Code + "]"
		if strings.TrimSpace(t.Description) == "" {
			v.err(tp+".description", "description is required")
		}
		if len(t.Components) == 0 {
			v.err(tp+".components", "a pack's tax code has components with dated rates")
		}
	}
}

func (v *validator) holidays(hs []config.Holiday) {
	seen := map[string]bool{}
	for i, h := range hs {
		p := fmt.Sprintf("calendar.yaml: holidays[%d]", i)
		v.date(p+".date", h.Date)
		if seen[h.Date] {
			v.err(p, "duplicate holiday on %s", h.Date)
		}
		seen[h.Date] = true
		if strings.TrimSpace(h.Name) == "" {
			v.err(p+".name", "name is required")
		}
	}
}

func (v *validator) numberRanges(rs []NumberRange) {
	seen := map[string]bool{}
	for _, r := range rs {
		p := "numbering.yaml: number_ranges[" + r.DocumentType + "]"
		if !documentTypeRe.MatchString(r.DocumentType) || seen[r.DocumentType] {
			v.err(p, "document type must be unique lower-case letters and underscores")
		}
		seen[r.DocumentType] = true
		if !prefixRe.MatchString(r.Prefix) {
			v.err(p+".prefix", "prefix %q must be up to 10 upper-case letters, digits or dashes", r.Prefix)
		}
		if r.First < 1 || r.Last < r.First {
			v.err(p, "the range %d-%d must start at 1 or above and not end below its start", r.First, r.Last)
		}
	}
}

func (v *validator) paymentTerms(ts []config.PaymentTerm) {
	seen := map[string]bool{}
	for _, t := range ts {
		if strings.TrimSpace(t.Key) == "" || seen[t.Key] {
			v.err("payments.yaml: payment_terms["+t.Key+"]", "payment term key must be present and unique")
		}
		seen[t.Key] = true
	}
}
