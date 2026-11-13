package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// withExpenseTypes is the fixture with company 1000's expense types:
// travel and meals, both on its expense account 520000 (ADR-0074 §2).
func withExpenseTypes(t *testing.T) *Config {
	t.Helper()
	c := fixture(t)
	c.Company("1000").ExpenseTypes = []ExpenseType{{Code: "TRAVEL", Name: "Travel", Account: "520000"}, {Code: "MEALS", Name: "Meals", Account: "520000"}}
	return c
}

// TestExpenseTypesValid: expense types with codes of their own, names and
// expense accounts validate, and so does a company without any; each rule
// refuses the type's own field.
func TestExpenseTypesValid(t *testing.T) {
	if fs := Validate(withExpenseTypes(t), currency.Seeded()); HasErrors(fs) {
		t.Fatalf("two expense types: got %v, want no errors", fs)
	}
	co := withExpenseTypes(t).Company("1000")
	if got, ok := co.ExpenseType("MEALS"); !ok || got.Name != "Meals" {
		t.Errorf("expense type MEALS: got %v %v, want Meals", got, ok)
	}
	if _, ok := co.ExpenseType("GIFTS"); ok {
		t.Errorf("expense type GIFTS: got found, want not configured")
	}
	const types = "companies[1000].expense_types"
	many := make([]ExpenseType, MaxExpenseTypes+1)
	for i := range many {
		many[i] = ExpenseType{Code: "T" + strings.Repeat("X", i%19) + string(rune('A'+i%26)), Name: "Type", Account: "520000"}
	}
	cases := []struct {
		name          string
		types         []ExpenseType
		path, message string
	}{
		{"a code in lower case", []ExpenseType{{Code: "travel", Name: "Travel", Account: "520000"}}, types + "[0].code", "capital letters"},
		{"a code too long", []ExpenseType{{Code: strings.Repeat("A", 21), Name: "Travel", Account: "520000"}}, types + "[0].code", "1-20"},
		{"a code twice", []ExpenseType{{Code: "TRAVEL", Name: "Travel", Account: "520000"}, {Code: "TRAVEL", Name: "Trips", Account: "520000"}},
			types + "[TRAVEL]", "duplicate expense type code"},
		{"no name", []ExpenseType{{Code: "TRAVEL", Name: " ", Account: "520000"}}, types + "[TRAVEL].name", "is required"},
		{"a name too long", []ExpenseType{{Code: "TRAVEL", Name: strings.Repeat("n", 61), Account: "520000"}}, types + "[TRAVEL].name", "at most 60"},
		{"no account", []ExpenseType{{Code: "TRAVEL", Name: "Travel"}}, types + "[TRAVEL].account", "is required"},
		{"an account not in the chart", []ExpenseType{{Code: "TRAVEL", Name: "Travel", Account: "999999"}}, types + "[TRAVEL].account",
			"not in the chart of accounts"},
		{"a liability account", []ExpenseType{{Code: "TRAVEL", Name: "Travel", Account: "211000"}}, types + "[TRAVEL].account", "expected expense"},
		{"too many", many, types, "at most 100 expense types"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture(t)
			c.Company("1000").ExpenseTypes = tc.types
			fs := Validate(c, currency.Seeded())
			if !slices.ContainsFunc(fs, func(f Finding) bool {
				return f.Severity == SevError && f.Path == tc.path && strings.Contains(f.Message, tc.message)
			}) {
				t.Errorf("findings: got %v, want an error at %s saying %q", fs, tc.path, tc.message)
			}
		})
	}
}

// TestExpenseTypesTier: an expense type's name is cosmetic; a type added
// and a type's account are posting-related, since they decide what an
// expense report's lines post to (ADR-0074 §2). Each change is named by
// the type's code.
func TestExpenseTypesTier(t *testing.T) {
	renamed := withExpenseTypes(t)
	renamed.Company("1000").ExpenseTypes[1].Name = "Meals and drinks"
	moved := withExpenseTypes(t)
	moved.Company("1000").ExpenseTypes[0].Account = "530000"
	cases := []struct {
		name     string
		old, new *Config
		path     string
		tier     Tier
	}{
		{"a name", withExpenseTypes(t), renamed, "companies[1000].expense_types[MEALS].name", TierCosmetic},
		{"an account", withExpenseTypes(t), moved, "companies[1000].expense_types[TRAVEL].account", TierPostingRelated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diffs, _ := Diff(tc.old, tc.new) // Diff never fails
			if len(diffs) != 1 || diffs[0].Path != tc.path || diffs[0].Tier != tc.tier {
				t.Errorf("changes: got %v, want one at %s of tier %v", diffs, tc.path, tc.tier)
			}
		})
	}
	diffs, _ := Diff(fixture(t), withExpenseTypes(t)) // Diff never fails
	var paths []string
	for _, d := range diffs {
		paths = append(paths, d.Path)
		if d.Tier != TierPostingRelated {
			t.Errorf("tier of %s: got %v, want %v", d.Path, d.Tier, TierPostingRelated)
		}
	}
	if got, want := strings.Join(paths, " "), "companies[1000].expense_types[MEALS] companies[1000].expense_types[TRAVEL]"; got != want {
		t.Errorf("types set: got %s, want %s", got, want)
	}
}
