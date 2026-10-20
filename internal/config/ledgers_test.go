package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// parallel is the fixture with a local-GAAP ledger 2L kept by company 1000
// and a group of both ledgers.
func parallel(t *testing.T) *Config {
	t.Helper()
	c := fixture(t)
	c.Ledgers = []Ledger{{Code: "0L", Name: "Leading ledger", AccountingPrinciple: "IFRS"}, {Code: "2L", Name: "US GAAP", AccountingPrinciple: "US_GAAP"}}
	c.LedgerGroups = []LedgerGroup{{Code: "ALL", Name: "Every ledger", Representative: "0L", Ledgers: []string{"0L", "2L"}}}
	c.Companies[0].Ledgers = []string{"0L", "2L"}
	return c
}

func TestLedgersDefault(t *testing.T) {
	c := fixture(t)
	if got := c.LedgerList(); len(got) != 1 || got[0].Code != LeadingLedger || got[0].AccountingPrinciple != "IFRS" {
		t.Errorf("default ledgers: got %v, want 0L under IFRS", got)
	}
	if got := c.Companies[0].LedgerCodes(); !slices.Equal(got, []string{LeadingLedger}) {
		t.Errorf("default company ledgers: got %v, want [0L]", got)
	}
	p := parallel(t)
	if got := len(p.LedgerList()); got != 2 {
		t.Errorf("named ledgers: got %d, want 2", got)
	}
	if got := p.Companies[0].LedgerCodes(); !slices.Equal(got, []string{"0L", "2L"}) {
		t.Errorf("named company ledgers: got %v, want [0L 2L]", got)
	}
	if fs := Validate(p, currency.Seeded()); HasErrors(fs) {
		t.Fatalf("parallel ledgers: got %v, want no errors", fs)
	}
}

func TestLedgerValidation(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(c *Config)
		want   string
	}{
		{"ledger code", func(c *Config) { c.Ledgers[1].Code = "2" }, "ledgers[2].code: ledger codes are two"},
		{"duplicate ledger", func(c *Config) { c.Ledgers = append(c.Ledgers, c.Ledgers[1]) }, "ledgers[2L]: duplicate ledger"},
		{"ledger name", func(c *Config) { c.Ledgers[1].Name = " " }, "ledgers[2L].name: name is required"},
		{"principle", func(c *Config) { c.Ledgers[1].AccountingPrinciple = "us gaap" }, "ledgers[2L].accounting_principle"},
		{"no leading ledger", func(c *Config) { c.Ledgers = c.Ledgers[1:] }, "ledgers: the leading ledger 0L is required"},
		{"unknown company ledger", func(c *Config) { c.Companies[1].Ledgers = []string{"0L", "3L"} }, "companies[2000].ledgers[3L]: no ledger 3L"},
		{"company ledger twice", func(c *Config) { c.Companies[0].Ledgers = []string{"0L", "2L", "2L"} }, "companies[1000].ledgers[2L]: ledger 2L is named twice"},
		{"company without 0L", func(c *Config) { c.Companies[0].Ledgers = []string{"2L"} }, "companies[1000].ledgers: every company keeps the leading ledger 0L"},
		{"group code", func(c *Config) { c.LedgerGroups[0].Code = "all" }, "ledger_groups[all].code: ledger group codes"},
		{"duplicate group", func(c *Config) { c.LedgerGroups = append(c.LedgerGroups, c.LedgerGroups[0]) }, "ledger_groups[ALL]: duplicate ledger group"},
		{"group named as a ledger", func(c *Config) { c.LedgerGroups[0].Code = "2L" }, "ledger_groups[2L].code: a ledger group's code differs"},
		{"group name", func(c *Config) { c.LedgerGroups[0].Name = "" }, "ledger_groups[ALL].name: name is required"},
		{"unknown member", func(c *Config) { c.LedgerGroups[0].Ledgers = []string{"0L", "9L"} }, "ledger_groups[ALL].ledgers[9L]: no ledger 9L"},
		{"representative not a member", func(c *Config) { c.LedgerGroups[0].Ledgers = []string{"2L"} }, "ledger_groups[ALL].representative: the representative ledger"},
		{"leading not representative", func(c *Config) { c.LedgerGroups[0].Representative = "2L" }, "ledger_groups[ALL].representative: a group with the leading ledger"},
	} {
		t.Run(c.name, func(t *testing.T) {
			doc := parallel(t)
			c.mutate(doc)
			var got []string
			for _, f := range Validate(doc, currency.Seeded()) {
				got = append(got, f.String())
			}
			if all := strings.Join(got, "\n"); !strings.Contains(all, c.want) {
				t.Errorf("findings: got\n%s\nwant one containing %q", all, c.want)
			}
		})
	}
}

func TestLedgerDiffTiers(t *testing.T) {
	base := parallel(t)
	for _, c := range []struct {
		name   string
		mutate func(c *Config)
		path   string
		tier   Tier
	}{
		{"ledger name", func(c *Config) { c.Ledgers[1].Name = "Local GAAP" }, "ledgers[2L].name", TierCosmetic},
		{"new ledger", func(c *Config) {
			c.Ledgers = append(c.Ledgers, Ledger{Code: "3L", Name: "Tax", AccountingPrinciple: "TAX"})
		}, "ledgers[3L]", TierStructural},
		{"company ledger", func(c *Config) { c.Companies[1].Ledgers = []string{"0L", "2L"} }, "companies[2000].ledgers[2L]", TierStructural},
		{"another company ledger", func(c *Config) { c.Companies[0].Ledgers = []string{"0L"} }, "companies[1000].ledgers[2L]", TierStructural},
		{"group name", func(c *Config) { c.LedgerGroups[0].Name = "All" }, "ledger_groups[ALL].name", TierCosmetic},
		{"group member", func(c *Config) { c.LedgerGroups[0].Ledgers = []string{"0L"} }, "ledger_groups[ALL].ledgers[2L]", TierPostingRelated},
	} {
		t.Run(c.name, func(t *testing.T) {
			changed := parallel(t)
			c.mutate(changed)
			changes, err := Diff(base, changed)
			if err != nil {
				t.Fatalf("diff: %v", err)
			}
			i := slices.IndexFunc(changes, func(ch Change) bool { return ch.Path == c.path })
			if i < 0 {
				t.Fatalf("changes: got %v, want one at %s", changes, c.path)
			}
			if got := changes[i].Tier; got != c.tier {
				t.Errorf("tier of %s: got %v, want %v", c.path, got, c.tier)
			}
		})
	}
}
