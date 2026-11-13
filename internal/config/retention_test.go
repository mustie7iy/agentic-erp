package config

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// years is n as a rule's years.
func years(n int) *int { return &n }

// withRetention is the fixture with company 1000's rules: journal
// entries kept 10 years, personal data 2 (ADR-0079 §2).
func withRetention(t *testing.T) *Config {
	t.Helper()
	c := fixture(t)
	c.Company("1000").Retention = []RetentionRule{{Kind: RetentionJournalEntry, Years: years(10)}, {Kind: RetentionPersonalData, Years: years(2)}}
	return c
}

// TestRetentionRulesValid: a rule per kind with 0 to 99 years validates,
// as a company without any does; each rule refuses its own field.
func TestRetentionRulesValid(t *testing.T) {
	if fs := Validate(withRetention(t), currency.Seeded()); HasErrors(fs) {
		t.Fatalf("two rules: got %v, want no errors", fs)
	}
	if fs := Validate(fixture(t), currency.Seeded()); HasErrors(fs) {
		t.Fatalf("no rules: got %v, want no errors", fs)
	}
	zero := fixture(t)
	zero.Company("1000").Retention = []RetentionRule{{Kind: RetentionJournalEntry, Years: years(0)}, {Kind: RetentionPersonalData, Years: years(99)}}
	if fs := Validate(zero, currency.Seeded()); HasErrors(fs) {
		t.Fatalf("0 and 99 years: got %v, want no errors", fs)
	}
	const at = "companies[1000].retention"
	cases := []struct {
		name          string
		rules         []RetentionRule
		path, message string
	}{
		{"an unknown kind", []RetentionRule{{Kind: "invoice", Years: years(10)}}, at + "[invoice].kind", "one of journal_entry, personal_data"},
		{"a kind twice", []RetentionRule{{Kind: RetentionJournalEntry, Years: years(10)}, {Kind: RetentionJournalEntry, Years: years(8)}},
			at + "[journal_entry]", "one retention rule per company"},
		{"no years", []RetentionRule{{Kind: RetentionJournalEntry}}, at + "[journal_entry].years", "from 0 to 99"},
		{"years below zero", []RetentionRule{{Kind: RetentionJournalEntry, Years: years(-1)}}, at + "[journal_entry].years", "from 0 to 99"},
		{"100 years", []RetentionRule{{Kind: RetentionPersonalData, Years: years(100)}}, at + "[personal_data].years", "from 0 to 99"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture(t)
			c.Company("1000").Retention = tc.rules
			fs := Validate(c, currency.Seeded())
			if !slices.ContainsFunc(fs, func(f Finding) bool {
				return f.Severity == SevError && f.Path == tc.path && strings.Contains(f.Message, tc.message)
			}) {
				t.Errorf("findings: got %v, want an error at %s saying %q", fs, tc.path, tc.message)
			}
		})
	}
}

// TestRetentionYearsParsed: a rule's years are read as given, zero
// included, and a rule without them reads as having none, not zero.
func TestRetentionYearsParsed(t *testing.T) {
	for _, tc := range []struct{ name, rule, want string }{
		{"zero years", "{kind: journal_entry, years: 0}", "0"},
		{"ten years", "{kind: journal_entry, years: 10}", "10"},
		{"no years", "{kind: journal_entry}", "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r RetentionRule
			if err := yaml.Unmarshal([]byte(tc.rule), &r); err != nil {
				t.Fatal(err)
			}
			got := "none"
			if r.Years != nil {
				got = strconv.Itoa(*r.Years)
			}
			if got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// TestRetentionFor: a company's rule is found by its kind; a kind
// without one has none.
func TestRetentionFor(t *testing.T) {
	co := withRetention(t).Company("1000")
	if r := co.RetentionFor(RetentionPersonalData); r == nil || *r.Years != 2 {
		t.Errorf("personal data: got %v, want 2 years", r)
	}
	if r := fixture(t).Company("1000").RetentionFor(RetentionJournalEntry); r != nil {
		t.Errorf("a company without rules: got %v, want none", r)
	}
}

// TestRetentionEnds: retention ends the stated years after the fiscal
// year's last day; a year ending on 29 February ends on the last day of
// February of a year without one.
func TestRetentionEnds(t *testing.T) {
	day := func(s string) time.Time { d, _ := time.Parse(time.DateOnly, s); return d } // the cases' dates parse
	for _, tc := range []struct {
		end   string
		years int
		want  string
	}{
		{"2025-12-31", 0, "2025-12-31"},
		{"2025-12-31", 10, "2035-12-31"},
		{"2026-03-31", 7, "2033-03-31"},
		{"2024-02-29", 1, "2025-02-28"},
		{"2024-02-29", 4, "2028-02-29"},
	} {
		t.Run(tc.end, func(t *testing.T) {
			if got := RetentionEnds(day(tc.end), tc.years).Format(time.DateOnly); got != tc.want {
				t.Errorf("%s plus %d years: got %s, want %s", tc.end, tc.years, got, tc.want)
			}
		})
	}
}

// TestRetentionTier: retention rules post nothing, so setting them and a
// rule's years are tier 2, operational (ADR-0079 §2), each change named
// by its kind.
func TestRetentionTier(t *testing.T) {
	longer := withRetention(t)
	longer.Company("1000").Retention[0].Years = years(11)
	cases := []struct {
		name      string
		old, new  *Config
		wantPaths string
	}{
		{"set", fixture(t), withRetention(t), "companies[1000].retention[journal_entry] companies[1000].retention[personal_data]"},
		{"a rule's years", withRetention(t), longer, "companies[1000].retention[journal_entry].years"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diffs, _ := Diff(tc.old, tc.new) // Diff never fails
			var paths []string
			for _, d := range diffs {
				paths = append(paths, d.Path)
				if d.Tier != TierOperational {
					t.Errorf("tier of %s: got %v, want %v", d.Path, d.Tier, TierOperational)
				}
			}
			if got := strings.Join(paths, " "); got != tc.wantPaths {
				t.Errorf("changes: got %s, want %s", got, tc.wantPaths)
			}
		})
	}
}
