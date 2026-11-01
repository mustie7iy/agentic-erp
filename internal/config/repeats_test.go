package config

import (
	"strings"
	"testing"
)

// TestRepeated: two ranges share a number when their prefixes are equal
// and the intervals meet, or when one prefix is the other followed by
// digits that begin a value of the shorter range; the number named is the
// smallest shared one.
func TestRepeated(t *testing.T) {
	r := func(prefix string, first, last int64) NumberRange {
		return NumberRange{Prefix: prefix, First: first, Last: last}
	}
	for name, c := range map[string]struct {
		a, b NumberRange
		want string // "" for none
	}{
		"the pack's ranges of two years":                {r("PY", 1, 999999), r("PY", 1, 999999), "PY1"},
		"intervals that meet":                           {r("SI", 1, 500), r("SI", 500, 900), "SI500"},
		"disjoint intervals":                            {r("SI", 100000, 999999), r("SI", 1000000, 1999999), ""},
		"another prefix":                                {r("PY", 1, 999999), r("PZ", 1, 999999), ""},
		"a prefix ending in a dash":                     {r("PY", 1, 999999), r("PY2027-", 1, 999999), ""},
		"a prefix of digits after the other":            {r("PY", 1, 999999), r("PY27", 1, 999999), "PY271"},
		"in either order":                               {r("PY27", 1, 999999), r("PY", 1, 999999), "PY271"},
		"no value of the longer range is 00":            {r("PY", 2700, 2799), r("PY27", 1, 999999), "PY2710"},
		"digits beginning with zero":                    {r("PY", 1, 999999), r("PY0", 1, 999999), ""},
		"values too large for the shorter range":        {r("PY", 1, 999), r("PY1", 1, 999999), "PY11"},
		"the shorter range below every such value":      {r("PY", 1, 9), r("PY1", 1, 999999), ""},
		"the shorter range above every such value":      {r("PY", 300, 999), r("PY2", 1, 9), ""},
		"the empty prefix and one of digits":            {r("", 100000, 999999), r("2025", 10, 99), "202510"},
		"a last number near the largest":                {r("", 1, 9223372036854775807), r("9", 1, 9223372036854775807), "91"},
		"a head too large for the shorter range's last": {r("", 1, 50), r("6", 1, 9), ""},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := repeated(c.a, c.b)
			if got != c.want || ok != (c.want != "") {
				t.Errorf("repeated: got %q %v, want %q", got, ok, c.want)
			}
		})
	}
}

// TestRangeRepeats: a range the change adds or changes that repeats
// another year's numbers of its company and document type is refused, on
// that range, the later one when both are new; journal ranges, other
// companies and types, and two activated ranges that already repeated
// each other are not.
func TestRangeRepeats(t *testing.T) {
	r := func(company, dt string, year int, prefix string) NumberRange {
		return NumberRange{Company: company, DocumentType: dt, FiscalYear: year, Prefix: prefix, First: 1, Last: 999999}
	}
	py26, py27 := r("1000", "payment_run", 2026, "PY"), r("1000", "payment_run", 2027, "PY")
	doc := func(ranges ...NumberRange) *Config { return &Config{NumberRanges: ranges} }
	span := func(n NumberRange, first, last int64) NumberRange {
		n.First, n.Last = first, last
		return n
	}
	for name, c := range map[string]struct {
		base     *Config
		proposed *Config
		want     string // the paths found, space-separated
	}{
		"both added, by bootstrap": {nil, doc(py27, py26), "number_ranges[1000/payment_run/2027].prefix"},
		"the later year added":     {doc(py26), doc(py26, py27), "number_ranges[1000/payment_run/2027].prefix"},
		"the earlier year added":   {doc(py27), doc(py26, py27), "number_ranges[1000/payment_run/2026].prefix"},
		"both activated before":    {doc(py26, py27), doc(py26, py27), ""},
		"a year-specific prefix":   {doc(py26), doc(py26, r("1000", "payment_run", 2027, "PY2027-")), ""},
		"journal entries":          {nil, doc(r("1000", "journal_zp", 2026, ""), r("1000", "journal_zp", 2027, "")), ""},
		"another company":          {nil, doc(py26, r("2000", "payment_run", 2027, "PY")), ""},
		"another document type":    {nil, doc(py26, r("1000", "supplier_invoice", 2027, "PY")), ""},
		"a last raised into another year's numbers": {doc(span(py26, 1, 9), span(py27, 11, 19)), doc(span(py26, 1, 15), span(py27, 11, 19)),
			"number_ranges[1000/payment_run/2026].prefix"},
		"apart, activated before": {doc(span(py26, 1, 9), span(py27, 11, 19)), doc(span(py26, 1, 9), span(py27, 11, 29)), ""},
	} {
		t.Run(name, func(t *testing.T) {
			var paths []string
			for _, f := range RangeRepeats(c.base, c.proposed) {
				paths = append(paths, f.Path)
				if f.Severity != SevError || !strings.Contains(f.Message, "would issue PY1") {
					t.Errorf("finding: got %v, want an error naming PY1 or PY11", f)
				}
			}
			if got := strings.Join(paths, " "); got != c.want {
				t.Errorf("findings: got %q, want %q", got, c.want)
			}
		})
	}
}
