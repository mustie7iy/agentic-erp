package config

import (
	"slices"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// withFXValuation is the GR/IR fixture with a foreign-currency valuation:
// the bank revalued as a balance, the payables account through its items
// with an adjustment account, both to the unrealised gain and loss.
func withFXValuation(t *testing.T) *Config {
	t.Helper()
	c := withGRIR(t)
	co := &c.Companies[0]
	co.ChartOfAccounts = append(co.ChartOfAccounts, Account{Number: "211099", Name: "Payables valuation", Type: Liability},
		Account{Number: "159000", Name: "Down payments made", Type: Asset, VendorReconciliation: true})
	co.ChartOfAccounts[0].RevalueForeignCurrency = true // 100000, the bank
	co.AccountDetermination.DownPayment = "159000"
	co.FXValuation = FXValuation{Accounts: []FXValuationAccount{
		{Account: "100000", UnrealisedGain: "400000", UnrealisedLoss: "610000"},
		{Account: "211000", Adjustment: "211099", UnrealisedGain: "400000", UnrealisedLoss: "610000"},
	}}
	return c
}

// TestFXValuation: a company values at one known rate type, average by
// default; a flagged account is a balance-sheet account that reconciles no
// supplier, is not the GR/IR account and has a valuation; a valuation is
// of a vendor reconciliation account other than the down payment account,
// with a balance-sheet adjustment account that reconciles nothing, or of a
// flagged account without one, with a revenue gain and an expense loss.
func TestFXValuation(t *testing.T) {
	c := withFXValuation(t)
	if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
		t.Fatalf("FX valuation configured: got %v, want no errors", fs)
	}
	if got := c.Companies[0].FXValuation.RateTypeOrAverage(); got != "average" {
		t.Errorf("default rate type: got %s, want average", got)
	}
	const fx = "companies[1000].fx_valuation"
	bank := func(c *Config) *Account { return &c.Companies[0].ChartOfAccounts[0] }
	at := func(i int) func(c *Config) *FXValuationAccount {
		return func(c *Config) *FXValuationAccount { return &c.Companies[0].FXValuation.Accounts[i] }
	}
	cases := []struct {
		name   string
		mutate func(c *Config)
		path   string
	}{
		{"another rate type", func(c *Config) { c.Companies[0].FXValuation.RateType = "spot" }, fx + ".rate_type"},
		{"a flagged expense", func(c *Config) {
			c.Companies[0].Account("610000").RevalueForeignCurrency = true
		}, "companies[1000].chart_of_accounts[610000].revalue_foreign_currency"},
		{"a flagged reconciliation account", func(c *Config) {
			c.Companies[0].Account("211000").RevalueForeignCurrency = true
		}, "companies[1000].chart_of_accounts[211000].revalue_foreign_currency"},
		{"a flagged GR/IR account", func(c *Config) {
			c.Companies[0].Account("219000").RevalueForeignCurrency = true
		}, "companies[1000].chart_of_accounts[219000].revalue_foreign_currency"},
		{"a flagged account without valuation", func(c *Config) {
			c.Companies[0].FXValuation.Accounts = c.Companies[0].FXValuation.Accounts[1:]
		}, "companies[1000].chart_of_accounts[100000].revalue_foreign_currency"},
		{"an account twice", func(c *Config) {
			c.Companies[0].FXValuation.Accounts = append(c.Companies[0].FXValuation.Accounts, *at(0)(c))
		}, fx + ".accounts[100000]"},
		{"an account not in the chart", func(c *Config) { at(0)(c).Account = "999999" }, fx + ".accounts[999999]"},
		{"the down payment account", func(c *Config) {
			c.Companies[0].FXValuation.Accounts = append(c.Companies[0].FXValuation.Accounts,
				FXValuationAccount{Account: "159000", Adjustment: "211099", UnrealisedGain: "400000", UnrealisedLoss: "610000"})
		}, fx + ".accounts[159000]"},
		{"an unflagged G/L account", func(c *Config) { bank(c).RevalueForeignCurrency = false }, fx + ".accounts[100000]"},
		{"a reconciliation account without adjustment", func(c *Config) { at(1)(c).Adjustment = "" }, fx + ".accounts[211000].adjustment"},
		{"an adjustment off the balance sheet", func(c *Config) { at(1)(c).Adjustment = "520000" }, fx + ".accounts[211000].adjustment"},
		{"an adjustment that reconciles", func(c *Config) { at(1)(c).Adjustment = "211000" }, fx + ".accounts[211000].adjustment"},
		{"a G/L account with adjustment", func(c *Config) { at(0)(c).Adjustment = "211099" }, fx + ".accounts[100000].adjustment"},
		{"a gain that is no revenue", func(c *Config) { at(0)(c).UnrealisedGain = "610000" }, fx + ".accounts[100000].unrealised_gain"},
		{"a gain not in the chart", func(c *Config) { at(0)(c).UnrealisedGain = "999999" }, fx + ".accounts[100000].unrealised_gain"},
		{"a loss that is no expense", func(c *Config) { at(0)(c).UnrealisedLoss = "400000" }, fx + ".accounts[100000].unrealised_loss"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := withFXValuation(t)
			tc.mutate(c)
			fs := Validate(c, currency.Seeded())
			if !slices.ContainsFunc(fs, func(f Finding) bool { return f.Severity == SevError && f.Path == tc.path }) {
				t.Fatalf("no error at %s; got %v", tc.path, fs)
			}
		})
	}
}

// TestFXValuationDiff: valuation accounts are compared by account, and a
// change to them is posting-related.
func TestFXValuationDiff(t *testing.T) {
	base, changed := withFXValuation(t), withFXValuation(t)
	changed.Companies[0].FXValuation.Accounts = changed.Companies[0].FXValuation.Accounts[1:]
	changed.Companies[0].FXValuation.Accounts[0].UnrealisedLoss = "520000"
	changes, err := Diff(base, changed)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	tiers := map[string]Tier{}
	for _, ch := range changes {
		tiers[ch.Path] = ch.Tier
	}
	for _, path := range []string{"companies[1000].fx_valuation.accounts[100000]", "companies[1000].fx_valuation.accounts[211000].unrealised_loss"} {
		if got, ok := tiers[path]; !ok || got != TierPostingRelated {
			t.Errorf("change %s: got %v (%v), want %v; all %v", path, got, ok, TierPostingRelated, tiers)
		}
	}
}
