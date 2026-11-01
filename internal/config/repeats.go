package config

import (
	"fmt"
	"strconv"
	"strings"
)

// RangeRepeats refuses a number range that can issue a number another
// fiscal year's range of its company and document type issues too. Only
// journal entries are numbered per fiscal year (journal_entry_document,
// 000134). Every other document's number is unique in its legal entity
// across fiscal years: requisitions, orders, contracts and goods receipts
// (000006), supplier invoices (000007), payment runs (000008) and down
// payment requests (000079). A range that repeats an earlier year's
// numbers would have the later year's first document refused, as the
// first payment run of a new fiscal year was, by the ranges a country pack
// creates afresh for each year (review:T73g2). The ranges of base (nil
// before the first version) are not checked against each other: an
// activated range never changes its prefix or first number
// (rangeTransitions), so a pair activated before this check would refuse
// every later change request of the tenant.
func RangeRepeats(base, proposed *Config) []Finding {
	activated := map[string]bool{}
	if base != nil {
		for _, n := range base.NumberRanges {
			activated[n.Key()] = true
		}
	}
	var out []Finding
	ranges := proposed.NumberRanges
	for i, a := range ranges {
		for _, b := range ranges[i+1:] {
			if a.Company != b.Company || a.DocumentType != b.DocumentType || a.FiscalYear == b.FiscalYear ||
				strings.HasPrefix(a.DocumentType, "journal_") || activated[a.Key()] && activated[b.Key()] {
				continue
			}
			number, ok := repeated(a, b)
			if !ok {
				continue
			}
			// The finding is on the range the change adds, the later
			// year's when it adds both.
			n, other := b, a
			if activated[b.Key()] || !activated[a.Key()] && a.FiscalYear > b.FiscalYear {
				n, other = a, b
			}
			out = append(out, Finding{Severity: SevError, Path: "number_ranges[" + n.Key() + "].prefix", Message: fmt.Sprintf(
				"%s numbers are unique across fiscal years, and this range would issue %s, which fiscal year %d's range issues too; "+
					"give it a prefix that fiscal year %d's numbers cannot begin with, or numbers outside that range's",
				strings.ReplaceAll(n.DocumentType, "_", " "), number, other.FiscalYear, other.FiscalYear)})
		}
	}
	return out
}

// repeated is the smallest number of a that b can issue too, and whether
// there is one. A number is the prefix followed by the value in decimal,
// without leading zeros (config.next_document_number), so two ranges
// share a number only when their prefixes are equal and the intervals
// meet, or when one prefix is the other followed by digits s and the
// shorter range holds a value whose decimal begins with s, the rest a
// value of the longer range.
func repeated(a, b NumberRange) (string, bool) {
	if a.Prefix == b.Prefix {
		lo, hi := max(a.First, b.First), min(a.Last, b.Last)
		if lo > hi {
			return "", false
		}
		return a.Prefix + strconv.FormatInt(lo, 10), true
	}
	short, long := a, b
	if len(long.Prefix) < len(short.Prefix) {
		short, long = b, a
	}
	s, found := strings.CutPrefix(long.Prefix, short.Prefix)
	if !found || s[0] == '0' || strings.Trim(s, "0123456789") != "" {
		return "", false
	}
	head, _ := strconv.ParseInt(s, 10, 64) // at most ten digits (prefixRe)
	// The longer range's values v of d digits are head·10^d + v in the
	// shorter one; d stops where that has more digits than its last.
	for d := digits(long.First); d <= digits(long.Last) && len(s)+d <= digits(short.Last); d++ {
		scale := pow10(d)
		if head > short.Last/scale { // head·10^d alone is above the last (and might not fit)
			break
		}
		off := head * scale
		lo, hi := max(long.First, scale/10, short.First-off), min(long.Last, scale-1, short.Last-off)
		if lo <= hi {
			return short.Prefix + strconv.FormatInt(off+lo, 10), true
		}
	}
	return "", false
}

// digits is the number of decimal digits of a positive n.
func digits(n int64) int { return len(strconv.FormatInt(n, 10)) }

// pow10 is 10^d, for d up to 18.
func pow10(d int) int64 {
	p := int64(1)
	for range d {
		p *= 10
	}
	return p
}
