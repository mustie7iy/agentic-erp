package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// withDunning is the fixture with company 1000's dunning levels: 1 from a
// day overdue, 2 from 15 days, 3 from 30 (ADR-0065 §2).
func withDunning(t *testing.T) *Config {
	t.Helper()
	c := fixture(t)
	c.Company("1000").Dunning.Levels = []DunningLevel{{Level: 1, DaysOverdue: 1}, {Level: 2, DaysOverdue: 15}, {Level: 3, DaysOverdue: 30}}
	return c
}

// TestDunningLevelsValid: levels numbered from 1, each with more days
// overdue than the one before, validate; a company without any validates
// too; each rule refuses the level's own field.
func TestDunningLevelsValid(t *testing.T) {
	if fs := Validate(withDunning(t), currency.Seeded()); HasErrors(fs) {
		t.Fatalf("three dunning levels: got %v, want no errors", fs)
	}
	if fs := Validate(fixture(t), currency.Seeded()); HasErrors(fs) {
		t.Fatalf("no dunning levels: got %v, want no errors", fs)
	}
	const levels = "companies[1000].dunning.levels"
	ten := make([]DunningLevel, MaxDunningLevels+1)
	for i := range ten {
		ten[i] = DunningLevel{Level: i + 1, DaysOverdue: 10 * (i + 1)}
	}
	cases := []struct {
		name          string
		levels        []DunningLevel
		path, message string
	}{
		{"numbered from 2", []DunningLevel{{Level: 2, DaysOverdue: 1}}, levels + "[2].level", "this is level 1"},
		{"a level skipped", []DunningLevel{{Level: 1, DaysOverdue: 1}, {Level: 3, DaysOverdue: 9}}, levels + "[3].level", "this is level 2"},
		{"no days overdue", []DunningLevel{{Level: 1}}, levels + "[1].days_overdue", "must be 1-3650 days"},
		{"more than ten years overdue", []DunningLevel{{Level: 1, DaysOverdue: 3651}}, levels + "[1].days_overdue", "must be 1-3650 days"},
		{"the days of the level before", []DunningLevel{{Level: 1, DaysOverdue: 10}, {Level: 2, DaysOverdue: 10}}, levels + "[2].days_overdue",
			"more days overdue than the level before (10)"},
		{"fewer days than the level before", []DunningLevel{{Level: 1, DaysOverdue: 10}, {Level: 2, DaysOverdue: 9}}, levels + "[2].days_overdue",
			"more days overdue than the level before (10)"},
		{"ten levels", ten, levels, "at most 9 dunning levels"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture(t)
			c.Company("1000").Dunning.Levels = tc.levels
			fs := Validate(c, currency.Seeded())
			if !slices.ContainsFunc(fs, func(f Finding) bool {
				return f.Severity == SevError && f.Path == tc.path && strings.Contains(f.Message, tc.message)
			}) {
				t.Errorf("findings: got %v, want an error at %s saying %q", fs, tc.path, tc.message)
			}
		})
	}
}

// TestDunningLevelsTier: dunning levels post nothing, so setting them, a
// level's days and a level more are tier 2, operational (ADR-0065 §2),
// each change named by its level.
func TestDunningLevelsTier(t *testing.T) {
	later := withDunning(t)
	later.Company("1000").Dunning.Levels[1].DaysOverdue = 20
	fourth := withDunning(t)
	fourth.Company("1000").Dunning.Levels = append(fourth.Company("1000").Dunning.Levels, DunningLevel{Level: 4, DaysOverdue: 60})
	cases := []struct {
		name      string
		old, new  *Config
		wantPaths string
	}{
		{"set", fixture(t), withDunning(t), "companies[1000].dunning.levels[1] companies[1000].dunning.levels[2] companies[1000].dunning.levels[3]"},
		{"a level's days", withDunning(t), later, "companies[1000].dunning.levels[2].days_overdue"},
		{"a level more", withDunning(t), fourth, "companies[1000].dunning.levels[4]"},
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
