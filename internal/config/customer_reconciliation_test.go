package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// withReceivables is the asset fixture with company 1000's customer
// reconciliation account 120000 (T74a).
func withReceivables(t *testing.T) *Config {
	t.Helper()
	c := withAssets(t)
	co := &c.Companies[0]
	co.ChartOfAccounts = append(co.ChartOfAccounts, Account{Number: "120000", Name: "Trade receivables", Type: Asset, CustomerReconciliation: true})
	return c
}

// TestCustomerReconciliationValid: an asset flagged customer_reconciliation
// validates, the flag is a structural change, as the chart's are, its
// open items are valued as a supplier's (fx_valuation.accounts with an
// adjustment account), and each use of an account that a partner's items
// alone may post refuses it.
func TestCustomerReconciliationValid(t *testing.T) {
	if fs := Validate(withReceivables(t), currency.Seeded()); HasErrors(fs) {
		t.Fatalf("a customer reconciliation account: got %v, want no errors", fs)
	}
	diffs, _ := Diff(withAssets(t), withReceivables(t)) // Diff never fails
	if got := MaxTier(diffs); got != TierStructural {
		t.Errorf("tier of a new customer reconciliation account: got %d, want %d", got, TierStructural)
	}
	valued := withReceivables(t)
	valued.Companies[0].FXValuation.Accounts = []FXValuationAccount{{Account: "120000", Adjustment: "169000", UnrealisedGain: "400000", UnrealisedLoss: "610000"}}
	if fs := Validate(valued, currency.Seeded()); HasErrors(fs) {
		t.Errorf("a customer reconciliation account valued through its items into an adjustment account: got %v, want no errors", fs)
	}
	const chart, co = "companies[1000].chart_of_accounts[120000]", "companies[1000]"
	flag := func(mutate func(*Account)) func(c *Config) {
		return func(c *Config) { mutate(c.Companies[0].Account("120000")) }
	}
	cases := []struct {
		name          string
		mutate        func(c *Config)
		path, message string
	}{
		{"a liability", flag(func(a *Account) { a.Type = Liability }), chart + ".customer_reconciliation", "is an asset account"},
		{"reconciling suppliers too", flag(func(a *Account) { a.VendorReconciliation = true }), chart + ".customer_reconciliation", "one of them"},
		{"reconciling assets too", flag(func(a *Account) { a.AssetReconciliation = true }), chart + ".customer_reconciliation", "one of them"},
		{"revalued as a balance", flag(func(a *Account) { a.RevalueForeignCurrency = true }), chart + ".revalue_foreign_currency", "no supplier or customer"},
		{"as the bank", func(c *Config) { c.Companies[0].AccountDetermination.Bank = "120000" }, co + ".account_determination.bank",
			"customer reconciliation account"},
		{"for statements' charges", func(c *Config) {
			c.Companies[0].BankStatements.PostingRules = []StatementPostingRule{{TransactionCode: "NCHG", Description: "Charges", Account: "120000"}}
		}, co + ".bank_statements.posting_rules[NCHG].account", "posted only by its own documents"},
		{"as an asset clearing account", func(c *Config) { c.Companies[0].AssetAccounts[0].Clearing = "120000" },
			co + ".asset_accounts[MACHINES].clearing", "supplier or customer reconciliation account"},
		{"as an FX adjustment", func(c *Config) {
			c.Companies[0].FXValuation.Accounts = []FXValuationAccount{{Account: "211000", Adjustment: "120000", UnrealisedGain: "400000", UnrealisedLoss: "610000"}}
		}, co + ".fx_valuation.accounts[211000].adjustment", "reconciles no supplier or customer"},
		{"valued through its items without an adjustment account", func(c *Config) {
			c.Companies[0].FXValuation.Accounts = []FXValuationAccount{{Account: "120000", UnrealisedGain: "400000", UnrealisedLoss: "610000"}}
		}, co + ".fx_valuation.accounts[120000].adjustment", "reconciles no supplier or customer"},
		{"for a tax code", func(c *Config) { c.Companies[0].TaxCodes[0].Account = "120000" },
			co + ".tax_codes[" + withReceivables(t).Companies[0].TaxCodes[0].Code + "].account", "customer reconciliation account"},
		{"for a tax component", func(c *Config) {
			c.Companies[0].TaxCodes[0].Components = []TaxComponent{{Key: "gst", AccountKey: KeyInput, Account: "120000", Rates: []TaxRate{{RatePercent: "5"}}}}
		}, co + ".tax_codes[" + withReceivables(t).Companies[0].TaxCodes[0].Code + "].components[gst].account", "customer reconciliation account"},
		{"for withholding", func(c *Config) {
			c.Companies[0].Account("120000").Type = Liability
			c.Companies[0].WithholdingTaxCodes = []WithholdingTaxCode{{Code: "W1", Description: "Withholding", RatePercent: "10", Account: "120000",
				Posting: WithholdingAtInvoice}}
		}, co + ".withholding_tax_codes[W1].account", "reconciles no supplier"},
		{"as retained earnings", func(c *Config) {
			c.Companies[0].Account("120000").Type = Equity
			c.Companies[0].YearEnd.RetainedEarningsAccount = "120000"
		}, co + ".year_end.retained_earnings_account", "is a reconciliation account"},
		{"for accruals", func(c *Config) {
			c.Companies[0].Account("120000").Type = Liability
			c.Companies[0].Close.AccrualAccount = "120000"
		}, co + ".close.accrual_account", "is a reconciliation account"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := withReceivables(t)
			tc.mutate(c)
			fs := Validate(c, currency.Seeded())
			if !slices.ContainsFunc(fs, func(f Finding) bool {
				return f.Severity == SevError && f.Path == tc.path && strings.Contains(f.Message, tc.message)
			}) {
				t.Fatalf("findings: got %v, want an error at %s saying %q", fs, tc.path, tc.message)
			}
		})
	}
}
