package config

import (
	"strings"
	"unicode/utf8"
)

// PostalAddress is a company's postal address, in the shape of a business
// partner's address (partner.address; the contract's Address) without its
// purpose and validity: a configuration version is the address's history.
// The purchase order document prints it as the buyer's address and, while
// no item names a delivery address of its own, as the address to deliver
// to (ADR-0054 As built, product owner 2026-11-04). Line2, PostalCode and
// Region are optional, as a partner's are.
type PostalAddress struct {
	Line1       string `yaml:"line1" json:"line1"`
	Line2       string `yaml:"line2,omitempty" json:"line2,omitempty"`
	City        string `yaml:"city" json:"city"`
	PostalCode  string `yaml:"postal_code,omitempty" json:"postal_code,omitempty"`
	Region      string `yaml:"region,omitempty" json:"region,omitempty"`
	CountryCode string `yaml:"country_code" json:"country_code"`
}

// postalField is one text of an address with the bounds a partner's
// address field has in the contract (Address: line1 and city 1-200 and
// 1-100 characters, line2 and region up to 200 and 100, postal_code up to
// 20).
type postalField struct {
	key      string
	value    string
	required bool
	max      int
}

// postalAddress checks the company's postal address when it has one: an
// address is optional, and a document without one prints that none is
// configured rather than an address of its own invention.
func (v *validator) postalAddress(p string, a *PostalAddress) {
	if a == nil {
		return
	}
	p += ".postal_address"
	for _, f := range []postalField{{"line1", a.Line1, true, 200}, {"line2", a.Line2, false, 200}, {"city", a.City, true, 100},
		{"postal_code", a.PostalCode, false, 20}, {"region", a.Region, false, 100}} {
		switch n := utf8.RuneCountInString(f.value); {
		case f.required && strings.TrimSpace(f.value) == "":
			v.err(p+"."+f.key, "%s is required", f.key)
		case n > f.max:
			v.err(p+"."+f.key, "%s is at most %d characters, not %d", f.key, f.max, n)
		}
	}
	if !countryRe.MatchString(a.CountryCode) {
		v.err(p+".country_code", "country %q must be an ISO 3166 two-letter code", a.CountryCode)
	}
}
