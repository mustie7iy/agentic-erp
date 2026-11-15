package config

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// The document kinds a retention rule is for (ADR-0079 §2): a company's
// journal entries and the personal data it holds.
const (
	RetentionJournalEntry = "journal_entry"
	RetentionPersonalData = "personal_data"
)

// RetentionKinds are the document kinds of the first version, in the
// order they are listed.
var RetentionKinds = []string{RetentionJournalEntry, RetentionPersonalData}

// MaxRetentionYears is the longest a rule keeps a document: 99 years
// after its fiscal year ends.
const MaxRetentionYears = 99

// RetentionRule is how long a company keeps the documents of one kind
// (ADR-0079 §2): the whole years after the end of the fiscal year they
// belong to. The rules are the tenant's own; none is shipped or defaulted
// for any jurisdiction, and a kind without a rule has nothing past
// retention. Years is a pointer so that a rule without it is refused
// rather than read as zero years, which would put every ended year past
// retention.
type RetentionRule struct {
	Kind  string `yaml:"kind" json:"kind"`
	Years *int   `yaml:"years" json:"years"`
}

// RetentionFor is the company's rule for the document kind, or nil when
// it has none.
func (co *Company) RetentionFor(kind string) *RetentionRule {
	for i := range co.Retention {
		if co.Retention[i].Kind == kind {
			return &co.Retention[i]
		}
	}
	return nil
}

// RetentionEnds is the last day a document of a fiscal year ending on
// yearEnd is kept under a rule of years: the same day years later, or
// the last day of February when yearEnd is the 29th and that year has
// none (ADR-0079 §3).
func RetentionEnds(yearEnd time.Time, years int) time.Time {
	end := yearEnd.AddDate(years, 0, 0)
	if end.Day() != yearEnd.Day() { // 29 February to a year without one
		end = end.AddDate(0, 0, -end.Day())
	}
	return end
}

// retention checks a company's retention rules: each for a known kind,
// once, with a whole number of years from 0 to MaxRetentionYears.
func (v *validator) retention(p string, co *Company) {
	seen := map[string]bool{}
	for _, r := range co.Retention {
		rp := fmt.Sprintf("%s.retention[%s]", p, r.Kind)
		switch {
		case !slices.Contains(RetentionKinds, r.Kind):
			v.err(rp+".kind", "must be one of %s", strings.Join(RetentionKinds, ", "))
		case seen[r.Kind]:
			v.err(rp, "a kind has one retention rule per company")
		}
		seen[r.Kind] = true
		if r.Years == nil || *r.Years < 0 || *r.Years > MaxRetentionYears {
			v.err(rp+".years", "must be a whole number of years from 0 to %d", MaxRetentionYears)
		}
	}
}
