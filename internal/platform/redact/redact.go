// Package redact replaces, in text bound for a model, what must not leave
// the cell (ADR-0036): IBANs (ISO 13616 mod 97), payment card numbers
// (Luhn) and the tax identifiers of the tenant's countries, each with a
// placeholder, and counts the replacements. It is deterministic: the same
// text and countries always give the same result. Names and e-mail
// addresses pass; the work needs them.
package redact

import (
	"regexp"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/platform/iban"
)

// The placeholders a redaction leaves.
const (
	IBAN  = "[IBAN]"
	Card  = "[CARD]"
	TaxID = "[TAX_ID]"
)

var (
	// ibanLike is an IBAN as people write it: country, check digits, then
	// 11 to 30 letters and digits, run together or in groups of four with a
	// shorter last one. mod 97 decides.
	ibanLike = regexp.MustCompile(`\b[A-Z]{2}[0-9]{2}(?:[A-Z0-9]{11,30}|(?: [A-Z0-9]{4}){2,7}(?: [A-Z0-9]{1,4})?)\b`)
	// cardLike is 13 to 19 digits, spaces or dashes between them; Luhn
	// decides.
	cardLike = regexp.MustCompile(`\b[0-9](?:[ -]?[0-9]){12,18}\b`)
)

// taxIDs are the tax-identifier patterns per country: the VAT number with
// its country prefix, and the national forms a document shows.
var taxIDs = map[string][]string{
	"AE": {`\b100[0-9]{12}\b`},
	"AU": {`\b[0-9]{2} ?[0-9]{3} ?[0-9]{3} ?[0-9]{3}\b`},
	"BR": {`\b[0-9]{2}\.[0-9]{3}\.[0-9]{3}/[0-9]{4}-[0-9]{2}\b`, `\b[0-9]{3}\.[0-9]{3}\.[0-9]{3}-[0-9]{2}\b`},
	"CA": {`\b[0-9]{9} ?RT ?[0-9]{4}\b`},
	"CH": {`\bCHE[- ]?[0-9]{3}\.?[0-9]{3}\.?[0-9]{3}(?: ?(?:MWST|TVA|IVA))?\b`},
	"CN": {`\b[0-9A-HJ-NPQRTUWXY]{2}[0-9]{6}[0-9A-HJ-NPQRTUWXY]{10}\b`},
	"DE": {`\bDE ?[0-9]{9}\b`, `\b[0-9]{2,3}/[0-9]{3,4}/[0-9]{4,5}\b`},
	"DK": {`\bDK ?[0-9]{8}\b`},
	"ES": {`\bES ?[A-Z0-9][0-9]{7}[A-Z0-9]\b`},
	"FR": {`\bFR ?[0-9A-HJ-NP-Z]{2} ?[0-9]{9}\b`},
	"GB": {`\bGB ?(?:[0-9]{9}(?:[0-9]{3})?|GD[0-9]{3}|HA[0-9]{3})\b`},
	"IE": {`\bIE ?[0-9][0-9A-Z+*][0-9]{5}[A-W][A-I]?\b`},
	"IN": {`\b[0-9]{2}[A-Z]{5}[0-9]{4}[A-Z][1-9A-Z]Z[0-9A-Z]\b`, `\b[A-Z]{5}[0-9]{4}[A-Z]\b`},
	"IT": {`\bIT ?[0-9]{11}\b`},
	"JP": {`\bT[0-9]{13}\b`},
	"MX": {`\b[A-Z&Ñ]{3,4}[0-9]{6}[A-Z0-9]{3}\b`},
	"NL": {`\bNL ?[0-9]{9}B[0-9]{2}\b`},
	"NO": {`\bNO ?[0-9]{9}(?: ?MVA)?\b`},
	"PL": {`\bPL ?[0-9]{10}\b`},
	"SA": {`\b3[0-9]{13}3\b`},
	"SE": {`\bSE ?[0-9]{10}01\b`},
	"SG": {`\b(?:[0-9]{8,9}|[MN][0-9]{8})[A-Z]\b`, `\b[TSR][0-9]{2}[A-Z]{2}[0-9]{4}[A-Z]\b`},
	"US": {`\b[0-9]{2}-[0-9]{7}\b`, `\b[0-9]{3}-[0-9]{2}-[0-9]{4}\b`},
	"ZA": {`\b4[0-9]{9}\b`},
}

// Redactor redacts for one tenant's countries.
type Redactor struct {
	taxIDs []*regexp.Regexp
}

// For is the redactor of the given countries (ISO 3166 alpha-2); a country
// without known patterns adds none.
func For(countries []string) *Redactor {
	r := &Redactor{}
	sorted := slices.Clone(countries)
	slices.Sort(sorted)
	for _, c := range slices.Compact(sorted) {
		for _, p := range taxIDs[c] {
			r.taxIDs = append(r.taxIDs, regexp.MustCompile(p))
		}
	}
	return r
}

// Text is s with its IBANs, card numbers and tax identifiers replaced,
// and how many were.
func (r *Redactor) Text(s string) (string, int) {
	n := 0
	s = replace(s, ibanLike, &n, func(m string) string {
		if iban.Valid(m) {
			return IBAN
		}
		// Grouped, the last group may be the next word ("... 0130 00 EUR").
		if i := strings.LastIndexByte(m, ' '); i > 0 && iban.Valid(m[:i]) {
			return IBAN + m[i:]
		}
		return m
	})
	s = replace(s, cardLike, &n, func(m string) string {
		if luhn(m) {
			return Card
		}
		return m
	})
	for _, re := range r.taxIDs {
		s = replace(s, re, &n, func(string) string { return TaxID })
	}
	return s, n
}

// replace replaces each match of re by what with says, counting in n the
// matches it changes.
func replace(s string, re *regexp.Regexp, n *int, with func(m string) string) string {
	return re.ReplaceAllStringFunc(s, func(m string) string {
		out := with(m)
		if out != m {
			*n++
		}
		return out
	})
}

// luhn checks a card number, spaces and dashes allowed, with the Luhn
// algorithm.
func luhn(s string) bool {
	digits := strings.NewReplacer(" ", "", "-", "").Replace(s)
	sum := 0
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if (len(digits)-i)%2 == 0 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}
