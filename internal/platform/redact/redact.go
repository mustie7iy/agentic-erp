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
	// ibanLike is an IBAN as people write it: country and check digits,
	// then capitals and digits run together or in groups of up to four, at
	// most eight groups (an IBAN has at most 30 characters after its check
	// digits), so a match is short and its prefixes few. The groups may go
	// on into the next words ("... 1332 EUR"), so each prefix is tried,
	// longest first; mod 97 decides. Lower case is not taken: "on12 this
	// week" would too often pass mod 97 by chance.
	ibanLike = regexp.MustCompile(`\b[A-Z]{2}[0-9]{2}(?:[A-Z0-9]{11,30}|(?: [A-Z0-9]{1,4}){1,8})\b`)
	// digitRun is digits in groups, spaces or dashes between them: a card
	// number is 13 to 19 of them in consecutive groups that pass Luhn,
	// wherever in the run they are ("Ref 2026 4111 1111 1111 1111 12/28").
	digitRun = regexp.MustCompile(`\b[0-9]+(?:[ -][0-9]+)*\b`)
)

// compiled are the tax-identifier patterns, compiled once.
var compiled = map[string][]*regexp.Regexp{}

func init() {
	for c, ps := range taxIDs {
		for _, p := range ps {
			compiled[c] = append(compiled[c], regexp.MustCompile(p))
		}
	}
}

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
		r.taxIDs = append(r.taxIDs, compiled[c]...)
	}
	return r
}

// Text is s with its IBANs, card numbers and tax identifiers replaced,
// and how many were.
func (r *Redactor) Text(s string) (string, int) {
	n := 0
	s = replace(s, ibanLike, &n, ibanIn)
	s = replace(s, digitRun, &n, cardsIn)
	for _, re := range r.taxIDs {
		s = replace(s, re, &n, func(string) (string, int) { return TaxID, 1 })
	}
	return s, n
}

// replace replaces each match of re by what with says, adding to n the
// replacements it counts.
func replace(s string, re *regexp.Regexp, n *int, with func(m string) (string, int)) string {
	return re.ReplaceAllStringFunc(s, func(m string) string {
		out, k := with(m)
		*n += k
		return out
	})
}

// ibanIn replaces the IBANs in a match: the longest prefix ending at a
// group's end that is valid, then the same in what follows it; a match
// with none goes on from its next group.
func ibanIn(m string) (string, int) {
	for end := len(m); end > 4; {
		if iban.Valid(m[:end]) {
			rest, n := ibansAfter(m[end:])
			return IBAN + rest, n + 1
		}
		i := strings.LastIndexByte(m[:end], ' ')
		if i < 0 {
			break
		}
		end = i
	}
	if i := strings.IndexByte(m, ' '); i >= 0 {
		rest, n := ibansAfter(m[i:])
		return m[:i] + rest, n
	}
	return m, 0
}

// ibansAfter replaces the IBANs in the rest of a match.
func ibansAfter(rest string) (string, int) {
	n := 0
	return replace(rest, ibanLike, &n, ibanIn), n
}

// cardsIn replaces the card numbers in a run of digit groups: from the
// left, the longest consecutive groups of 13 to 19 digits that pass Luhn.
func cardsIn(run string) (string, int) {
	starts := []int{0} // where each group starts
	for i := 0; i < len(run); i++ {
		if run[i] == ' ' || run[i] == '-' {
			starts = append(starts, i+1)
		}
	}
	var out strings.Builder
	from, n := 0, 0 // where the search goes on from; the cards replaced
	for g := 0; g < len(starts); g++ {
		if end, ok := cardFrom(run, starts, g); ok {
			out.WriteString(run[from:starts[g]] + Card)
			from, n = end, n+1
			for g+1 < len(starts) && starts[g+1] <= end {
				g++
			}
		}
	}
	return out.String() + run[from:], n
}

// cardFrom is where the longest card number starting at group g ends: the
// groups from g are counted on only while they hold at most 19 digits, so
// the search is short whatever the run's length.
func cardFrom(run string, starts []int, g int) (int, bool) {
	var ends []int // where a candidate of 13 to 19 digits ends
	digits := 0
	for last := g; last < len(starts); last++ {
		end := len(run)
		if last+1 < len(starts) {
			end = starts[last+1] - 1
		}
		if digits += end - starts[last]; digits > 19 {
			break
		}
		if digits >= 13 {
			ends = append(ends, end)
		}
	}
	for i := len(ends) - 1; i >= 0; i-- {
		if luhn(run[starts[g]:ends[i]]) {
			return ends[i], true
		}
	}
	return 0, false
}

// luhn checks a card number, spaces and dashes allowed, with the Luhn
// algorithm.
func luhn(s string) bool {
	sum, pos := 0, 0 // pos: the digit's place from the right, separators skipped
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ' ' || s[i] == '-' {
			continue
		}
		d := int(s[i] - '0')
		if pos%2 == 1 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum, pos = sum+d, pos+1
	}
	return sum%10 == 0
}
