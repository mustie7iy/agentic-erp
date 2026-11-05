package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// withGroup is the fixture with a group chart, an elimination of
// intercompany balances and the group ACME: company 2000 holding 80% of
// company 1000 from 2026-11, its investment on a new asset account 150000.
// Every account of both companies maps to the group account of its type.
func withGroup(t *testing.T) *Config {
	t.Helper()
	c := fixture(t)
	c.Consolidation = Consolidation{
		Accounts: []GroupAccount{
			{Number: "G1000", Name: "Other assets", Type: Asset},
			{Number: "G1250", Name: "Intercompany receivables", Type: Asset},
			{Number: "G1500", Name: "Investments in subsidiaries", Type: Asset},
			{Number: "G1900", Name: "Goodwill", Type: Asset, Role: RoleGoodwill},
			{Number: "G2000", Name: "Liabilities", Type: Liability},
			{Number: "G2250", Name: "Intercompany payables", Type: Liability},
			{Number: "G3000", Name: "Share capital", Type: Equity},
			{Number: "G3800", Name: "Retained earnings", Type: Equity, Role: RoleRetainedEarnings},
			{Number: "G3900", Name: "Currency translation reserve", Type: Equity, Role: RoleCTA},
			{Number: "G3950", Name: "Non-controlling interests", Type: Equity, Role: RoleNCI},
			{Number: "G4000", Name: "Revenue", Type: Revenue},
			{Number: "G5000", Name: "Expenses", Type: Expense},
			{Number: "G6950", Name: "Intercompany differences", Type: Expense},
		},
		Eliminations: []Elimination{{Code: "IC_BALANCES", Name: "Intercompany receivables and payables", Accounts: []string{"G1250", "G2250"},
			DifferenceAccount: "G6950", Tolerance: "10.00"}},
		Groups: []ConsolidationGroup{{Code: "ACME", Name: "Acme group", Parent: "2000",
			Members: []GroupMember{{Company: "1000", OwnershipPercent: "80", FirstPeriod: "2026-11", InvestmentAccount: "150000"}}}},
	}
	parent := c.Company("2000")
	parent.ChartOfAccounts = append(parent.ChartOfAccounts, Account{Number: "150000", Name: "Investment in Acme Corp US", Type: Asset, GroupAccount: "G1500"})
	byType := map[AccountType]string{Asset: "G1000", Liability: "G2000", Equity: "G3000", Revenue: "G4000", Expense: "G5000"}
	for i := range c.Companies {
		for j := range c.Companies[i].ChartOfAccounts {
			if a := &c.Companies[i].ChartOfAccounts[j]; a.GroupAccount == "" {
				a.GroupAccount = byType[a.Type]
			}
		}
	}
	return c
}

// TestConsolidationValid: the settings of ADR-0059 §3 validate, and each
// rule refuses its field.
func TestConsolidationValid(t *testing.T) {
	if fs := Validate(withGroup(t), currency.Seeded()); HasErrors(fs) {
		t.Fatalf("group reporting configured: got %v, want no errors", fs)
	}
	for _, tc := range consolidationCases() {
		t.Run(tc.name, func(t *testing.T) {
			c := withGroup(t)
			tc.mutate(c)
			if fs := Validate(c, currency.Seeded()); !hasPath(fs, tc.path) {
				t.Errorf("findings: got %v, want one at %s", fs, tc.path)
			}
		})
	}
}

type consolidationCase struct {
	name   string
	mutate func(c *Config)
	path   string
}

func consolidationCases() []consolidationCase {
	return slices.Concat(groupChartCases(), eliminationCases(), groupCases(), mappingCases())
}

func account(number string, mutate func(*GroupAccount)) func(c *Config) {
	return func(c *Config) { mutate(c.Consolidation.GroupAccount(number)) }
}

func groupChartCases() []consolidationCase {
	return []consolidationCase{
		{"an unknown rate type", func(c *Config) { c.Consolidation.RateType = "closing" }, "consolidation.rate_type"},
		{"a lower-case number", account("G1000", func(a *GroupAccount) { a.Number = "g1000" }), "consolidation.accounts[g1000].number"},
		{"a number above ten characters", account("G1000", func(a *GroupAccount) { a.Number = "G1000000000" }), "consolidation.accounts[G1000000000].number"},
		{"an account twice", func(c *Config) {
			c.Consolidation.Accounts = append(c.Consolidation.Accounts, c.Consolidation.Accounts[0])
		},
			"consolidation.accounts[G1000]"},
		{"no name", account("G1000", func(a *GroupAccount) { a.Name = " " }), "consolidation.accounts[G1000].name"},
		{"an unknown type", account("G1000", func(a *GroupAccount) { a.Type = "contra" }), "consolidation.accounts[G1000].type"},
		{"an unknown role", account("G3000", func(a *GroupAccount) { a.Role = "capital" }), "consolidation.accounts[G3000].role"},
		{"goodwill on an equity account", account("G3000", func(a *GroupAccount) { a.Role = RoleGoodwill }), "consolidation.accounts[G3000].role"},
		{"the translation reserve on an asset", account("G1900", func(a *GroupAccount) { a.Role = RoleCTA }), "consolidation.accounts[G1900].role"},
		{"a role twice", account("G3000", func(a *GroupAccount) { a.Role = RoleNCI }), "consolidation.accounts[G3950].role"},
		{"a group without a goodwill account", account("G1900", func(a *GroupAccount) { a.Role = "" }), "consolidation.accounts"},
		{"more than 5,000 accounts", func(c *Config) {
			for i := range 4990 {
				c.Consolidation.Accounts = append(c.Consolidation.Accounts, GroupAccount{Number: "X" + strings.Repeat("9", i%9) + string(rune('A'+i/9%26)) +
					string(rune('A'+i/234)), Name: "Account", Type: Asset})
			}
		}, "consolidation.accounts"},
	}
}

func elimination(mutate func(*Elimination)) func(c *Config) {
	return func(c *Config) { mutate(&c.Consolidation.Eliminations[0]) }
}

func eliminationCases() []consolidationCase {
	const p = "consolidation.eliminations[IC_BALANCES]"
	return []consolidationCase{
		{"a lower-case code", elimination(func(e *Elimination) { e.Code = "ic" }), "consolidation.eliminations[ic].code"},
		{"an elimination twice", func(c *Config) {
			c.Consolidation.Eliminations = append(c.Consolidation.Eliminations, c.Consolidation.Eliminations[0])
		}, p},
		{"no name", elimination(func(e *Elimination) { e.Name = "" }), p + ".name"},
		{"no accounts", elimination(func(e *Elimination) { e.Accounts = nil }), p + ".accounts"},
		{"an account not in the group chart", elimination(func(e *Elimination) { e.Accounts = []string{"G9999"} }), p + ".accounts"},
		{"an account twice", elimination(func(e *Elimination) { e.Accounts = []string{"G1250", "G1250"} }), p + ".accounts"},
		{"an account of another elimination", func(c *Config) {
			c.Consolidation.Eliminations = append(c.Consolidation.Eliminations, Elimination{Code: "IC_RESULTS", Name: "Results",
				Accounts: []string{"G4000", "G1250"}, DifferenceAccount: "G6950", Tolerance: "0"})
		}, "consolidation.eliminations[IC_RESULTS].accounts"},
		{"a difference account not in the group chart", elimination(func(e *Elimination) { e.DifferenceAccount = "G9999" }), p + ".difference_account"},
		{"a difference account in an elimination", elimination(func(e *Elimination) { e.DifferenceAccount = "G2250" }), p + ".difference_account"},
		{"a negative tolerance", elimination(func(e *Elimination) { e.Tolerance = "-1.00" }), p + ".tolerance"},
		{"no tolerance", elimination(func(e *Elimination) { e.Tolerance = "" }), p + ".tolerance"},
		{"more than 50 eliminations", func(c *Config) {
			for i := range 50 {
				c.Consolidation.Eliminations = append(c.Consolidation.Eliminations, Elimination{Code: "E" + strings.Repeat("X", i), Name: "Elimination",
					Accounts: []string{}, DifferenceAccount: "G6950", Tolerance: "0"})
			}
		}, "consolidation.eliminations"},
	}
}

func member(mutate func(*GroupMember)) func(c *Config) {
	return func(c *Config) { mutate(&c.Consolidation.Groups[0].Members[0]) }
}

func groupCases() []consolidationCase {
	const p = "consolidation.groups[ACME]"
	const m = p + ".members[1000]"
	return []consolidationCase{
		{"a lower-case code", func(c *Config) { c.Consolidation.Groups[0].Code = "acme" }, "consolidation.groups[acme].code"},
		{"a group twice", func(c *Config) { c.Consolidation.Groups = append(c.Consolidation.Groups, c.Consolidation.Groups[0]) }, p},
		{"no name", func(c *Config) { c.Consolidation.Groups[0].Name = "" }, p + ".name"},
		{"a parent not in the document", func(c *Config) { c.Consolidation.Groups[0].Parent = "9000" }, p + ".parent"},
		{"no members", func(c *Config) { c.Consolidation.Groups[0].Members = nil }, p + ".members"},
		{"a member not in the document", member(func(m *GroupMember) { m.Company = "9000" }), p + ".members[9000].company"},
		{"the parent as its own member", member(func(m *GroupMember) { m.Company = "2000" }), p + ".members[2000].company"},
		{"a member twice", func(c *Config) { g := &c.Consolidation.Groups[0]; g.Members = append(g.Members, g.Members[0]) }, m + ".company"},
		{"a company in two groups", func(c *Config) {
			c.Consolidation.Groups = append(c.Consolidation.Groups, ConsolidationGroup{Code: "US", Name: "US group", Parent: "1000"})
		}, "consolidation.groups[US].parent"},
		{"no ownership", member(func(m *GroupMember) { m.OwnershipPercent = "" }), m + ".ownership_percent"},
		{"an ownership of 0", member(func(m *GroupMember) { m.OwnershipPercent = "0" }), m + ".ownership_percent"},
		{"an ownership above 100", member(func(m *GroupMember) { m.OwnershipPercent = "100.0001" }), m + ".ownership_percent"},
		{"an ownership with five decimals", member(func(m *GroupMember) { m.OwnershipPercent = "80.00001" }), m + ".ownership_percent"},
		{"a first period without its year", member(func(m *GroupMember) { m.FirstPeriod = "11" }), m + ".first_period"},
		{"a special period", member(func(m *GroupMember) { m.FirstPeriod = "2026-13" }), m + ".first_period"},
		{"period 0", member(func(m *GroupMember) { m.FirstPeriod = "2026-00" }), m + ".first_period"},
		{"a year before 1900", member(func(m *GroupMember) { m.FirstPeriod = "1899-12" }), m + ".first_period"},
		{"an investment account not in the parent's chart", member(func(m *GroupMember) { m.InvestmentAccount = "999999" }), m + ".investment_account"},
		{"an investment on a liability", member(func(m *GroupMember) { m.InvestmentAccount = "211000" }), m + ".investment_account"},
		{"an investment mapped into an elimination", func(c *Config) {
			c.Company("2000").Account("150000").GroupAccount = "G1250"
		}, m + ".investment_account"},
		{"an investment two members name", func(c *Config) {
			c.Companies = append(c.Companies, *c.Company("1000"))
			c.Companies[2].Code = "4000"
			g := &c.Consolidation.Groups[0]
			g.Members = append(g.Members, GroupMember{Company: "4000", OwnershipPercent: "100", FirstPeriod: "2026-11", InvestmentAccount: "150000"})
		}, p + ".members[4000].investment_account"},
		{"more than 50 groups", func(c *Config) {
			for i := range 50 {
				c.Consolidation.Groups = append(c.Consolidation.Groups, ConsolidationGroup{Code: "G" + strings.Repeat("X", i), Name: "Group"})
			}
		}, "consolidation.groups"},
	}
}

func mappingCases() []consolidationCase {
	const p = "companies[1000].chart_of_accounts[400000].group_account"
	mapping := func(group string) func(c *Config) {
		return func(c *Config) { c.Company("1000").Account("400000").GroupAccount = group }
	}
	return []consolidationCase{
		{"an account of a member without its group account", mapping(""), p},
		{"an account of the parent without its group account", func(c *Config) { c.Company("2000").Account("300000").GroupAccount = "" },
			"companies[2000].chart_of_accounts[300000].group_account"},
		{"a group account not in the group chart", mapping("G9999"), p},
		{"a revenue account on an asset group account", mapping("G1000"), p},
		{"an asset account on an expense group account", func(c *Config) { c.Company("1000").Account("100000").GroupAccount = "G5000" },
			"companies[1000].chart_of_accounts[100000].group_account"},
		{"an account on a role account", func(c *Config) { c.Company("1000").Account("300000").GroupAccount = "G3800" },
			"companies[1000].chart_of_accounts[300000].group_account"},
		{"a company outside every group mapped to an unknown group account", func(c *Config) {
			c.Consolidation.Groups = nil
			c.Company("1000").Account("400000").GroupAccount = "G9999"
		}, p},
	}
}

// TestConsolidationOutsideGroups: without groups, a group chart needs no
// roles and a company's accounts need no group account.
func TestConsolidationOutsideGroups(t *testing.T) {
	c := withGroup(t)
	c.Consolidation.Groups = nil
	for i := range c.Consolidation.Accounts {
		c.Consolidation.Accounts[i].Role = ""
	}
	c.Company("1000").Account("400000").GroupAccount = ""
	if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
		t.Errorf("a group chart without groups: got %v, want no errors", fs)
	}
}

// TestConsolidationReads: the reads other contexts use: the closing rate's
// type, average by default; an account and the elimination naming it; a
// group and the group a company is in; a member's first period.
func TestConsolidationReads(t *testing.T) {
	c := withGroup(t)
	cs := c.Consolidation
	checks := []struct{ what, got, want string }{
		{"the default rate type", cs.RateTypeOrAverage(), "average"},
		{"the elimination of G2250", cs.EliminationOf("G2250"), "IC_BALANCES"},
		{"the elimination of G1000", cs.EliminationOf("G1000"), ""},
		{"the group of the parent", cs.GroupOf("2000"), "ACME"},
		{"the group of the member", cs.GroupOf("1000"), "ACME"},
		{"the group of a company outside", cs.GroupOf("3000"), ""},
		{"the group ACME", cs.Group("ACME").Name, "Acme group"},
	}
	cs.RateType = "planning"
	checks = append(checks, struct{ what, got, want string }{"a configured rate type", cs.RateTypeOrAverage(), "planning"})
	for _, ch := range checks {
		if ch.got != ch.want {
			t.Errorf("%s: got %q, want %q", ch.what, ch.got, ch.want)
		}
	}
	if cs.Group("NONE") != nil || cs.GroupAccount("G9999") != nil {
		t.Errorf("an unknown group or group account: got one, want nil")
	}
	if y, p, ok := cs.Groups[0].Members[0].First(); y != 2026 || p != 11 || !ok {
		t.Errorf("first period: got %d-%d %v, want 2026-11 true", y, p, ok)
	}
}

// TestConsolidationTiers: names are cosmetic, the rate type and
// tolerances operational, the eliminations' accounts posting-related, the
// group chart, the groups and the mapping structural (ADR-0059 §3).
func TestConsolidationTiers(t *testing.T) {
	base := withGroup(t)
	changed := withGroup(t)
	cs := &changed.Consolidation
	cs.RateType = "planning"
	cs.GroupAccount("G1000").Name = "Assets"
	cs.Eliminations[0].Name = "Intercompany balances"
	cs.Eliminations[0].Tolerance = "5.00"
	cs.Eliminations[0].Accounts = []string{"G1250"}
	cs.Eliminations[0].DifferenceAccount = "G5000"
	cs.Groups[0].Name = "Acme"
	cs.Groups[0].Members[0].OwnershipPercent = "90"
	cs.GroupAccount("G4000").Type = Expense
	cs.Accounts = append(cs.Accounts, GroupAccount{Number: "G4100", Name: "Other revenue", Type: Revenue})
	cs.Eliminations = append(cs.Eliminations, Elimination{Code: "IC_RESULTS", Name: "Results", Accounts: []string{"G4100"}, DifferenceAccount: "G6950", Tolerance: "0"})
	changed.Company("1000").Account("400000").GroupAccount = "G4100"
	diffs, _ := Diff(base, changed) // Diff never fails
	want := map[string]Tier{
		"consolidation.rate_type":                                    TierOperational,
		"consolidation.accounts[G1000].name":                         TierCosmetic,
		"consolidation.accounts[G4000].type":                         TierStructural,
		"consolidation.accounts[G4100]":                              TierStructural,
		"consolidation.eliminations[IC_BALANCES].name":               TierCosmetic,
		"consolidation.eliminations[IC_BALANCES].tolerance":          TierOperational,
		"consolidation.eliminations[IC_BALANCES].accounts[G2250]":    TierPostingRelated,
		"consolidation.eliminations[IC_BALANCES].difference_account": TierPostingRelated,
		"consolidation.eliminations[IC_RESULTS]":                     TierPostingRelated,
		"consolidation.groups[ACME].name":                            TierCosmetic,
		"consolidation.groups[ACME].members[1000].ownership_percent": TierStructural,
		"companies[1000].chart_of_accounts[400000].group_account":    TierStructural,
	}
	if len(diffs) != len(want) {
		t.Errorf("Diff: got %v, want %d changes", diffs, len(want))
	}
	for _, d := range diffs {
		if tier, ok := want[d.Path]; !ok || d.Tier != tier {
			t.Errorf("Diff %s: got %v, want %v", d.Path, d.Tier, tier)
		}
	}
}
