package config

import (
	"slices"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// withForeignBank is company 1000 (USD) with a further house bank, CA, at
// a Canadian bank in CAD: its clearing, main and incoming accounts flagged
// for revaluation, each with a valuation, and payment method T ranking it
// in CAD (ADR-0051 §3, T53a).
func withForeignBank(t *testing.T) *Config {
	t.Helper()
	c := fixture(t)
	co := &c.Companies[0]
	co.ChartOfAccounts = append(co.ChartOfAccounts,
		Account{Number: "105000", Name: "Bank CAD - clearing", Type: Asset, OpenItems: true, RevalueForeignCurrency: true},
		Account{Number: "106000", Name: "Bank CAD", Type: Asset, RevalueForeignCurrency: true},
		Account{Number: "107000", Name: "Bank CAD - incoming", Type: Asset, OpenItems: true, RevalueForeignCurrency: true})
	co.HouseBanks = append(co.HouseBanks, HouseBank{Name: "CA", InstitutionNumber: "003", TransitNumber: "00012", AccountNumber: "1234567",
		BIC: "ROYCCAT2", GLAccount: "105000", MainGLAccount: "106000", IncomingGLAccount: "107000", Currency: "CAD"})
	for _, n := range []string{"105000", "106000", "107000"} {
		co.FXValuation.Accounts = append(co.FXValuation.Accounts, FXValuationAccount{Account: n, UnrealisedGain: "400000", UnrealisedLoss: "610000"})
	}
	co.PaymentMethods[0].Ranking = append(co.PaymentMethods[0].Ranking, BankRanking{Currency: "CAD", HouseBanks: []string{"CA"}})
	return c
}

// TestForeignCurrencyHouseBank: a house bank may hold another currency
// than its company's, a known one, with every account of it revalued at
// period end and ranked only in its currency; without a currency a bank is
// in the company's (ADR-0051 §3, T53a).
func TestForeignCurrencyHouseBank(t *testing.T) {
	c := withForeignBank(t)
	if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
		t.Fatalf("a CAD bank of a USD company: got %v, want no errors", fs)
	}
	co := &c.Companies[0]
	if got := co.HouseBankCurrency(co.HouseBank); got != "USD" {
		t.Errorf("currency of a bank without one: got %s, want the company's USD", got)
	}
	if got := co.HouseBankCurrency(co.HouseBanks[len(co.HouseBanks)-1]); got != "CAD" {
		t.Errorf("currency of the CAD bank: got %s, want CAD", got)
	}
	t.Run("the company's currency named needs no revaluation", func(t *testing.T) {
		c := fixture(t)
		c.Companies[0].HouseBank.Currency = "USD"
		if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
			t.Errorf("got %v, want no errors", fs)
		}
	})
	const p = "companies[1000]"
	last := func(c *Config) *HouseBank { return &c.Companies[0].HouseBanks[len(c.Companies[0].HouseBanks)-1] }
	cases := []struct {
		name   string
		mutate func(c *Config)
		path   string
	}{
		{"an unknown currency", func(c *Config) { last(c).Currency = "XXY" }, p + ".house_banks[CA].currency"},
		{"the clearing account not revalued", func(c *Config) { c.Companies[0].Account("105000").RevalueForeignCurrency = false },
			p + ".house_banks[CA].gl_account"},
		{"the main bank account not revalued", func(c *Config) { c.Companies[0].Account("106000").RevalueForeignCurrency = false },
			p + ".house_banks[CA].main_gl_account"},
		{"the incoming account not revalued", func(c *Config) { c.Companies[0].Account("107000").RevalueForeignCurrency = false },
			p + ".house_banks[CA].incoming_gl_account"},
		{"the main bank in CAD on the determination's unrevalued bank account", func(c *Config) { c.Companies[0].HouseBank.Currency = "CAD" },
			p + ".house_bank.gl_account"},
		{"the CAD bank ranked in USD", func(c *Config) {
			r := &c.Companies[0].PaymentMethods[0].Ranking[0]
			r.HouseBanks = append(r.HouseBanks, "CA")
		}, p + ".payment_methods[T].ranking[USD].house_banks"},
		{"the USD bank ranked in CAD", func(c *Config) {
			r := &c.Companies[0].PaymentMethods[0].Ranking[1]
			r.HouseBanks = append(r.HouseBanks, "MAIN")
		}, p + ".payment_methods[T].ranking[CAD].house_banks"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := withForeignBank(t)
			tc.mutate(c)
			fs := Validate(c, currency.Seeded())
			if !slices.ContainsFunc(fs, func(f Finding) bool { return f.Severity == SevError && f.Path == tc.path }) {
				t.Errorf("got findings %v, want an error at %s", fs, tc.path)
			}
		})
	}
	t.Run("a currency change is posting-related", func(t *testing.T) {
		changed := withForeignBank(t)
		last(changed).Currency = "EUR"
		got, _ := Diff(withForeignBank(t), changed)
		if len(got) != 1 {
			t.Fatalf("changes: got %v, want 1", got)
		}
		if got[0].Path != p+".house_banks[CA].currency" || got[0].Tier != TierPostingRelated {
			t.Errorf("change: got %s %v, want %s.house_banks[CA].currency posting-related", got[0].Path, got[0].Tier, p)
		}
	})
}
