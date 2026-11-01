package config

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
)

// TaxRegistration is a company's registration with a tax authority
// (ADR-0049 §10, T61g): the number the authority gave it, the return it
// files and how often, from its first period on. The tax filing periods
// follow from it (TaxPeriods), independent of fiscal periods. A UAE tax
// group is one registration across several companies of the tenant: the
// representative member's, listing the other members.
//
// Which numbers, returns and frequencies a company has are its facts; the
// document holds no rule about them (the format of a BN or TRN included,
// which is for the tax specialist). The number is sealed where the tax
// filing context keeps it and shown by its last four characters.
type TaxRegistration struct {
	// Code names the registration in the tenant, across companies.
	Code string `yaml:"code" json:"code"`
	// Authority is the tax administration's code (CRA, RQ, FTA, a
	// province's).
	Authority string `yaml:"authority" json:"authority"`
	Number    string `yaml:"number" json:"number"`
	// ReturnType is the return the registration files, as the country
	// pack names its returns (T61h checks it against them).
	ReturnType string `yaml:"return_type" json:"return_type"`
	// Frequency is monthly, quarterly or annual.
	Frequency string `yaml:"frequency" json:"frequency"`
	// FirstPeriodStart is the first day of the first period (YYYY-MM-DD);
	// every later period starts on the same day of a month.
	FirstPeriodStart string `yaml:"first_period_start" json:"first_period_start"`
	// LastPeriodEnd, when set, is the last day of the registration's last
	// period: it ended, or its frequency changes and a registration of a
	// new code takes over from the next day.
	LastPeriodEnd string `yaml:"last_period_end,omitempty" json:"last_period_end,omitempty"`
	// GroupMembers are the codes of the other companies a tax group's
	// registration covers.
	GroupMembers []string `yaml:"group_members,omitempty" json:"group_members,omitempty"`
}

// TaxPeriod is one filing period: its first and last day.
type TaxPeriod struct {
	Start, End time.Time
}

// Tax filing frequencies.
const (
	TaxMonthly   = "monthly"
	TaxQuarterly = "quarterly"
	TaxAnnual    = "annual"
)

// taxFrequencyMonths is how many months a period of each frequency spans.
var taxFrequencyMonths = map[string]int{TaxMonthly: 1, TaxQuarterly: 3, TaxAnnual: 12}

// maxTaxPeriodDay is the last day of a month a period may start on: every
// month has it, so every period starts on the same day.
const maxTaxPeriodDay = 28

var (
	taxRegistrationCodeRe = regexp.MustCompile(`^[A-Z0-9_]{1,20}$`)
	taxAuthorityRe        = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,19}$`)
	taxReturnTypeRe       = regexp.MustCompile(`^[A-Z0-9_-]{1,20}$`)
)

// maxTaxNumber is the longest registration number, as it is sealed.
const maxTaxNumber = 40

// TaxPeriods are the periods of a registration of frequency starting on
// first, up to the one starting on or before through, and none after the
// one ending on last (nil: the registration has not ended). first is a
// valid first_period_start and frequency a valid frequency.
func TaxPeriods(first time.Time, frequency string, last *time.Time, through time.Time) []TaxPeriod {
	months := taxFrequencyMonths[frequency]
	var out []TaxPeriod
	for i := 0; ; i++ {
		start := first.AddDate(0, i*months, 0)
		if start.After(through) || (last != nil && start.After(*last)) {
			return out
		}
		out = append(out, TaxPeriod{Start: start, End: first.AddDate(0, (i+1)*months, -1)})
	}
}

// taxRegistrationDates are a registration's first day and, when it has
// ended, its last.
type taxRegistrationDates struct {
	first time.Time
	last  *time.Time
}

// taxRegistrations checks the company's registrations on their own and
// returns their dates by code, for the checks across companies.
func (v *validator) taxRegistrations(p string, co *Company, companies map[string]bool) map[string]taxRegistrationDates {
	out := map[string]taxRegistrationDates{}
	for i, r := range co.TaxRegistrations {
		rp := fmt.Sprintf("%s.tax_registrations[%s]", p, r.Code)
		if !taxRegistrationCodeRe.MatchString(r.Code) {
			rp = fmt.Sprintf("%s.tax_registrations[%d]", p, i)
			v.err(rp+".code", "a registration's code is 1-20 upper-case letters, digits or underscores")
		}
		if !taxAuthorityRe.MatchString(r.Authority) {
			v.err(rp+".authority", "the authority is its code: an upper-case letter, then up to 19 upper-case letters, digits or underscores (CRA, FTA)")
		}
		v.taxNumber(rp, r.Number)
		if !taxReturnTypeRe.MatchString(r.ReturnType) {
			v.err(rp+".return_type", "the return type is the country pack's code of the return: 1-20 upper-case letters, digits, dashes or underscores")
		}
		if _, ok := taxFrequencyMonths[r.Frequency]; !ok {
			v.err(rp+".frequency", "must be monthly, quarterly or annual")
		}
		v.groupMembers(rp, co.Code, r.GroupMembers, companies)
		if d, ok := v.taxRegistrationDates(rp, r); ok {
			out[r.Code] = d
		}
	}
	return out
}

// taxNumber checks that a registration number is given, in printable
// ASCII (so its last four characters, which are shown, are four bytes)
// and, as it is sealed, at most maxTaxNumber characters without spaces.
// Its format is the authority's, which the tax specialist states (T61g
// leaves it unchecked).
func (v *validator) taxNumber(rp, number string) {
	n := fieldcrypt.Normalize(number)
	switch {
	case n == "":
		v.err(rp+".number", "the registration number is required")
	case strings.IndexFunc(n, func(r rune) bool { return r > unicode.MaxASCII || !unicode.IsPrint(r) }) >= 0:
		v.err(rp+".number", "the registration number has a character other than printable ASCII letters, digits and punctuation")
	case len(n) > maxTaxNumber:
		v.err(rp+".number", "the registration number has more than %d characters without spaces", maxTaxNumber)
	}
}

// groupMembers checks a tax group's other members: companies of the
// document, each once, the registrant not among them.
func (v *validator) groupMembers(rp, registrant string, members []string, companies map[string]bool) {
	seen := map[string]bool{}
	for _, m := range members {
		switch {
		case m == registrant:
			v.err(rp+".group_members", "company %s holds the registration; the members are the group's other companies", m)
		case !companies[m]:
			v.err(rp+".group_members", "company %s is not in the configuration", m)
		case seen[m]:
			v.err(rp+".group_members", "company %s is listed twice", m)
		}
		seen[m] = true
	}
}

// taxRegistrationDates checks the first period's start and the last
// period's end, which must end one of the registration's periods.
func (v *validator) taxRegistrationDates(rp string, r TaxRegistration) (taxRegistrationDates, bool) {
	first, err := time.Parse(time.DateOnly, r.FirstPeriodStart)
	if err != nil {
		v.err(rp+".first_period_start", "must be a date, YYYY-MM-DD")
		return taxRegistrationDates{}, false
	}
	if first.Day() > maxTaxPeriodDay {
		v.err(rp+".first_period_start", "a period starts on day 1-%d of a month, which every month has", maxTaxPeriodDay)
		return taxRegistrationDates{}, false
	}
	if r.LastPeriodEnd == "" {
		return taxRegistrationDates{first: first}, true
	}
	last, err := time.Parse(time.DateOnly, r.LastPeriodEnd)
	if err != nil {
		v.err(rp+".last_period_end", "must be a date, YYYY-MM-DD")
		return taxRegistrationDates{}, false
	}
	if _, ok := taxFrequencyMonths[r.Frequency]; !ok {
		return taxRegistrationDates{}, false // the frequency's finding says why
	}
	periods := TaxPeriods(first, r.Frequency, &last, last)
	if len(periods) == 0 || !periods[len(periods)-1].End.Equal(last) {
		v.err(rp+".last_period_end", "%s does not end one of the registration's %s periods from %s", r.LastPeriodEnd, r.Frequency, r.FirstPeriodStart)
		return taxRegistrationDates{}, false
	}
	return taxRegistrationDates{first: first, last: &last}, true
}

// taxRegistrationScope is a registration as the checks across companies
// see it: where it is, which companies it covers and when.
type taxRegistrationScope struct {
	path, authority, number string
	companies               []string
	dates                   taxRegistrationDates
}

// overlaps reports whether two registrations' periods share a day.
func (a taxRegistrationDates) overlaps(b taxRegistrationDates) bool {
	aEnds := a.last != nil && a.last.Before(b.first)
	bEnds := b.last != nil && b.last.Before(a.first)
	return !aEnds && !bEnds
}

// taxRegistrationsAcross checks what spans companies: a code once in the
// tenant, a company covered by at most one registration of an authority at
// a time, and a number registered once at a time, so a return reports
// each item once (ADR-0049 §11).
func (v *validator) taxRegistrationsAcross(c *Config, dates map[string]map[string]taxRegistrationDates) {
	scopes := v.taxRegistrationScopes(c, dates)
	for i, a := range scopes {
		for _, b := range scopes[:i] {
			if a.authority != b.authority || !a.dates.overlaps(b.dates) {
				continue
			}
			if shared := sharedCompany(a.companies, b.companies); shared != "" {
				v.err(a.path, "company %s is also covered by %s with authority %s in the same periods: a company files one return per authority for a period",
					shared, b.path, a.authority)
			} else if a.number == b.number {
				v.err(a.path+".number", "the number is also %s's in the same periods: give the registration one code, its group members listed on it", b.path)
			}
		}
	}
}

// taxRegistrationScopes are the registrations with valid dates, a code
// used before in the tenant reported and left out.
func (v *validator) taxRegistrationScopes(c *Config, dates map[string]map[string]taxRegistrationDates) []taxRegistrationScope {
	var scopes []taxRegistrationScope
	codes := map[string]string{}
	for _, co := range c.Companies {
		for _, r := range co.TaxRegistrations {
			path := fmt.Sprintf("companies[%s].tax_registrations[%s]", co.Code, r.Code)
			if other, dup := codes[r.Code]; dup && taxRegistrationCodeRe.MatchString(r.Code) {
				v.err(path+".code", "registration %s is also company %s's: a code names one registration in the tenant", r.Code, other)
				continue
			}
			codes[r.Code] = co.Code
			if d, ok := dates[co.Code][r.Code]; ok {
				scopes = append(scopes, taxRegistrationScope{path: path, authority: r.Authority, number: fieldcrypt.Normalize(r.Number),
					companies: append([]string{co.Code}, r.GroupMembers...), dates: d})
			}
		}
	}
	return scopes
}

// sharedCompany is a company in both lists, or "".
func sharedCompany(a, b []string) string {
	for _, c := range a {
		if slices.Contains(b, c) {
			return c
		}
	}
	return ""
}
