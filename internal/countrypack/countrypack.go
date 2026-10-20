// Package countrypack loads, validates and applies country packs
// (ADR-0048): versioned YAML bundles of a country's chart of accounts, tax
// codes, holidays, number ranges and payment terms, embedded in the image by
// the packs package. Like delivery/sapconfig it produces a draft
// configuration document and notes for an ordinary change request (03 §3.5);
// it imports only config and the shared kernel, and touches no database.
package countrypack

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mustie7iy/agentic-erp/internal/config"
)

// Pack is one version of a country's pack, one field per file.
type Pack struct {
	Manifest  Manifest
	Accounts  Accounts
	Tax       Tax
	Calendar  Calendar
	Numbering Numbering
	Payments  Payments
}

// Manifest is pack.yaml: what the pack is, from when it holds, the legal
// sources its settings rest on and the tax specialist who signed it off.
type Manifest struct {
	Country   string   `yaml:"country"`
	Version   string   `yaml:"version"`
	Name      string   `yaml:"name"`
	ValidFrom string   `yaml:"valid_from"`
	Sources   []Source `yaml:"sources"`
	// SignOff is required before the pilot (pilot gate); a pack without it
	// loads with a warning.
	SignOff *SignOff `yaml:"sign_off,omitempty"`
}

// Source is a law, regulation or authority publication a pack follows.
type Source struct {
	Title     string `yaml:"title"`
	URL       string `yaml:"url"`
	Retrieved string `yaml:"retrieved"`
}

// SignOff records the tax specialist who reviewed the pack, and when.
type SignOff struct {
	TaxSpecialist string `yaml:"tax_specialist"`
	Date          string `yaml:"date"`
}

// Accounts is accounts.yaml: the chart of accounts and which of its
// accounts automatic postings use.
type Accounts struct {
	Accounts             []config.Account            `yaml:"accounts"`
	AccountDetermination config.AccountDetermination `yaml:"account_determination"`
}

// Tax is tax.yaml.
type Tax struct {
	TaxCodes []TaxCode `yaml:"tax_codes"`
}

// TaxCode is a tax code with its rates over time; config.TaxCode has one
// rate until T61, so applying a pack writes the rate in force.
type TaxCode struct {
	Code        string `yaml:"code"`
	Description string `yaml:"description"`
	Account     string `yaml:"account"`
	Rates       []Rate `yaml:"rates"`
}

// Rate is a rate in force from ValidFrom through ValidTo (inclusive; open
// when empty). Dates are YYYY-MM-DD.
type Rate struct {
	RatePercent string `yaml:"rate_percent"`
	ValidFrom   string `yaml:"valid_from"`
	ValidTo     string `yaml:"valid_to,omitempty"`
}

// Calendar is calendar.yaml: the country's public holidays.
type Calendar struct {
	Holidays []Holiday `yaml:"holidays"`
}

// Holiday is a public holiday on Date (YYYY-MM-DD).
type Holiday struct {
	Date string `yaml:"date"`
	Name string `yaml:"name"`
}

// Numbering is numbering.yaml: number range templates, one per document
// type, which applying a pack creates for the company and fiscal year.
type Numbering struct {
	NumberRanges []NumberRange `yaml:"number_ranges"`
}

// NumberRange is a number range template for one document type.
type NumberRange struct {
	DocumentType string `yaml:"document_type"`
	Prefix       string `yaml:"prefix,omitempty"`
	First        int64  `yaml:"first"`
	Last         int64  `yaml:"last"`
	Gapless      bool   `yaml:"gapless,omitempty"`
}

// Payments is payments.yaml: the payment terms customary in the country.
type Payments struct {
	PaymentTerms []config.PaymentTerm `yaml:"payment_terms"`
}

// Files are the files of a pack directory, all required.
var Files = []string{"pack.yaml", "accounts.yaml", "tax.yaml", "calendar.yaml", "numbering.yaml", "payments.yaml"}

// Parse reads a pack's files by name strictly: an unknown or missing file
// and an unknown field are errors, so a typo never drops a setting.
func Parse(files map[string][]byte) (*Pack, error) {
	for name := range files {
		if !slices.Contains(Files, name) {
			return nil, fmt.Errorf("countrypack: unknown file %s", name)
		}
	}
	p := &Pack{}
	targets := []any{&p.Manifest, &p.Accounts, &p.Tax, &p.Calendar, &p.Numbering, &p.Payments}
	for i, name := range Files {
		b, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("countrypack: missing file %s", name)
		}
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		if err := dec.Decode(targets[i]); err != nil && !errors.Is(err, io.EOF) { // an empty file is an empty section
			return nil, fmt.Errorf("countrypack: %s: %w", name, err)
		}
		if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) { // a second document would be dropped
			return nil, fmt.Errorf("countrypack: %s: one YAML document expected", name)
		}
	}
	return p, nil
}

// Dir is the pack's directory: lower-case country, then version.
func (p *Pack) Dir() string { return strings.ToLower(p.Manifest.Country) + "/" + p.Manifest.Version }

// SignedOff reports whether a tax specialist signed the pack off, which the
// pilot requires.
func (p *Pack) SignedOff() bool { return p.Manifest.signedOff() }

func (m *Manifest) signedOff() bool {
	return m.SignOff != nil && strings.TrimSpace(m.SignOff.TaxSpecialist) != ""
}

// ParseVersion splits a version YYYY.N into its year and sequence number.
func ParseVersion(v string) (year, seq int, ok bool) {
	y, n, found := strings.Cut(v, ".")
	if !found || len(y) != 4 || n == "" || n[0] == '0' {
		return 0, 0, false
	}
	year, err1 := strconv.Atoi(y)
	seq, err2 := strconv.Atoi(n)
	if err1 != nil || err2 != nil || year < 1900 || seq < 1 || strings.ContainsAny(y+n, "+-") {
		return 0, 0, false
	}
	return year, seq, true
}

// CompareVersions orders two valid versions as cmp.Compare does.
func CompareVersions(a, b string) int {
	ay, an, _ := ParseVersion(a) // invalid versions never pass validation, so both are valid here
	by, bn, _ := ParseVersion(b)
	if c := cmp.Compare(ay, by); c != 0 {
		return c
	}
	return cmp.Compare(an, bn)
}
