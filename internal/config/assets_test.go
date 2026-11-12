package config

import (
	"slices"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// withAssets is the fixture with the local-GAAP ledger 2L kept by company
// 1000 and an asset class MACHINES with terms in both ledgers, whose
// accounts the company names (ADR-0057 §3).
func withAssets(t *testing.T) *Config {
	t.Helper()
	c := fixture(t)
	c.Ledgers = []Ledger{{Code: LeadingLedger, Name: "Leading ledger", AccountingPrinciple: "IFRS"}, {Code: "2L", Name: "HGB", AccountingPrinciple: "HGB"}}
	c.Companies[0].Ledgers = []string{LeadingLedger, "2L"}
	c.AssetClasses = []AssetClass{{Code: "MACHINES", Name: "Machinery and equipment", Terms: []AssetTerms{
		{Ledger: LeadingLedger, Method: StraightLine, UsefulLifeMonths: 120},
		{Ledger: "2L", Method: DecliningBalance, UsefulLifeMonths: 120, DecliningFactor: "2"}}}}
	co := &c.Companies[0]
	co.ChartOfAccounts = append(co.ChartOfAccounts,
		Account{Number: "160000", Name: "Machinery", Type: Asset, AssetReconciliation: true},
		Account{Number: "160900", Name: "Machinery, accumulated depreciation", Type: Asset, AssetReconciliation: true},
		Account{Number: "620000", Name: "Depreciation", Type: Expense},
		Account{Number: "169000", Name: "Asset clearing", Type: Asset},
		Account{Number: "490000", Name: "Gain on asset retirements", Type: Revenue},
		Account{Number: "690000", Name: "Loss on asset retirements", Type: Expense},
		Account{Number: "169500", Name: "Asset sale clearing", Type: Asset})
	co.AssetAccounts = []AssetAccounts{{Class: "MACHINES", AcquisitionCost: "160000", AccumulatedDepreciation: "160900", Depreciation: "620000",
		Clearing: "169000", Gain: "490000", Loss: "690000", SaleClearing: "169500"}}
	return c
}

// TestAssetClassesValid: the classes and accounts of ADR-0057 §3 validate,
// and each rule of the section refuses its field.
func TestAssetClassesValid(t *testing.T) {
	if fs := Validate(withAssets(t), currency.Seeded()); HasErrors(fs) {
		t.Fatalf("asset classes configured: got %v, want no errors", fs)
	}
	const cls, acc = "asset_classes[MACHINES]", "companies[1000].asset_accounts[MACHINES]"
	c0tax := withAssets(t).Companies[0].TaxCodes[0].Code
	terms := func(i int, mutate func(*AssetTerms)) func(c *Config) {
		return func(c *Config) { mutate(&c.AssetClasses[0].Terms[i]) }
	}
	accounts := func(mutate func(*AssetAccounts)) func(c *Config) {
		return func(c *Config) { mutate(&c.Companies[0].AssetAccounts[0]) }
	}
	flag := func(number string, mutate func(*Account)) func(c *Config) {
		return func(c *Config) { mutate(c.Companies[0].Account(number)) }
	}
	cases := []struct {
		name   string
		mutate func(c *Config)
		path   string
	}{
		{"a lower-case code", func(c *Config) { c.AssetClasses[0].Code = "machines" }, "asset_classes[machines].code"},
		{"a class twice", func(c *Config) { c.AssetClasses = append(c.AssetClasses, c.AssetClasses[0]) }, cls},
		{"no name", func(c *Config) { c.AssetClasses[0].Name = " " }, cls + ".name"},
		{"no terms", func(c *Config) { c.AssetClasses[0].Terms = nil }, cls + ".terms"},
		{"an unknown ledger", terms(1, func(t *AssetTerms) { t.Ledger = "9L" }), cls + ".terms[9L].ledger"},
		{"a ledger twice", terms(1, func(t *AssetTerms) { t.Ledger = LeadingLedger }), cls + ".terms[0L]"},
		{"an unknown method", terms(0, func(t *AssetTerms) { t.Method = "sum_of_digits" }), cls + ".terms[0L].method"},
		{"no useful life", terms(0, func(t *AssetTerms) { t.UsefulLifeMonths = 0 }), cls + ".terms[0L].useful_life_months"},
		{"a life above 1200 months", terms(0, func(t *AssetTerms) { t.UsefulLifeMonths = 1201 }), cls + ".terms[0L].useful_life_months"},
		{"declining balance without its factor", terms(1, func(t *AssetTerms) { t.DecliningFactor = "" }), cls + ".terms[2L].declining_factor"},
		{"a factor with straight-line", terms(0, func(t *AssetTerms) { t.DecliningFactor = "2" }), cls + ".terms[0L].declining_factor"},
		{"a factor of 1", terms(1, func(t *AssetTerms) { t.DecliningFactor = "1" }), cls + ".terms[2L].declining_factor"},
		{"a factor above 10", terms(1, func(t *AssetTerms) { t.DecliningFactor = "10.0001" }), cls + ".terms[2L].declining_factor"},
		{"a factor that is no decimal", terms(1, func(t *AssetTerms) { t.DecliningFactor = "double" }), cls + ".terms[2L].declining_factor"},
		{"an unknown class", accounts(func(a *AssetAccounts) { a.Class = "VEHICLES" }), "companies[1000].asset_accounts[VEHICLES].class"},
		{"a class twice in a company", func(c *Config) {
			c.Companies[0].AssetAccounts = append(c.Companies[0].AssetAccounts, c.Companies[0].AssetAccounts[0])
		}, acc},
		{"a kept ledger without terms", func(c *Config) { c.AssetClasses[0].Terms = c.AssetClasses[0].Terms[:1] }, acc + ".class"},
		{"no acquisition cost account", accounts(func(a *AssetAccounts) { a.AcquisitionCost = "" }), acc + ".acquisition_cost"},
		{"an account not in the chart", accounts(func(a *AssetAccounts) { a.Gain = "999999" }), acc + ".gain"},
		{"a gain that is no revenue", accounts(func(a *AssetAccounts) { a.Gain = "610000" }), acc + ".gain"},
		{"a loss that is no expense", accounts(func(a *AssetAccounts) { a.Loss = "400000" }), acc + ".loss"},
		{"a depreciation account only allocations post to", flag("620000", func(a *Account) { a.CostElement = CostElementSecondary }), acc + ".depreciation"},
		{"a loss account only allocations post to", flag("690000", func(a *Account) { a.CostElement = CostElementSecondary }), acc + ".loss"},
		{"a vendor reconciliation clearing account", func(c *Config) {
			c.Companies[0].Account("211000").Type = Asset
			c.Companies[0].AssetAccounts[0].Clearing = "211000"
		}, acc + ".clearing"},
		{"a cost account that does not reconcile", flag("160000", func(a *Account) { a.AssetReconciliation = false }), acc + ".acquisition_cost"},
		{"a reconciling clearing account", flag("169000", func(a *Account) { a.AssetReconciliation = true }), acc + ".clearing"},
		{"one account for cost and depreciation", accounts(func(a *AssetAccounts) { a.AccumulatedDepreciation = "160000" }), acc + ".accumulated_depreciation"},
		{"one clearing account for acquisitions and sales", accounts(func(a *AssetAccounts) { a.SaleClearing = "169000" }), acc + ".sale_clearing"},
		{"a liability reconciling assets", func(c *Config) {
			c.Companies[0].ChartOfAccounts = append(c.Companies[0].ChartOfAccounts, Account{Number: "250000", Name: "Leases", Type: Liability, AssetReconciliation: true})
		}, "companies[1000].chart_of_accounts[250000].asset_reconciliation"},
		{"an account reconciling suppliers and assets", flag("160000", func(a *Account) { a.VendorReconciliation = true }),
			"companies[1000].chart_of_accounts[160000].asset_reconciliation"},
		{"open items on a reconciliation account", flag("160000", func(a *Account) { a.OpenItems = true }), "companies[1000].chart_of_accounts[160000].open_items"},
		{"revaluation of a reconciliation account", flag("160900", func(a *Account) { a.RevalueForeignCurrency = true }),
			"companies[1000].chart_of_accounts[160900].revalue_foreign_currency"},
		{"open items on the clearing account", flag("169000", func(a *Account) { a.OpenItems = true }), acc + ".clearing"},
		{"revaluation of the clearing account", flag("169000", func(a *Account) { a.RevalueForeignCurrency = true }), acc + ".clearing"},
		{"open items on the sale clearing account", flag("169500", func(a *Account) { a.OpenItems = true }), acc + ".sale_clearing"},
		{"revaluation of the sale clearing account", flag("169500", func(a *Account) { a.RevalueForeignCurrency = true }),
			acc + ".sale_clearing"},
		{"a reconciliation account as the bank", func(c *Config) { c.Companies[0].AccountDetermination.Bank = "160000" },
			"companies[1000].account_determination.bank"},
		{"a reconciliation account for statements' charges", func(c *Config) {
			c.Companies[0].BankStatements.PostingRules = []StatementPostingRule{{TransactionCode: "NCHG", Description: "Charges", Account: "160000"}}
		}, "companies[1000].bank_statements.posting_rules[NCHG].account"},
		{"a clearing account not in the chart", accounts(func(a *AssetAccounts) { a.Clearing = "999999" }), acc + ".clearing"},
		{"a reconciliation account for a tax code", func(c *Config) { c.Companies[0].TaxCodes[0].Account = "160000" },
			"companies[1000].tax_codes[" + c0tax + "].account"},
		{"a reconciliation account for a tax component", func(c *Config) {
			c.Companies[0].TaxCodes[0].Components = []TaxComponent{{Key: "gst", AccountKey: KeyInput, Account: "160000", Rates: []TaxRate{{RatePercent: "5"}}}}
		}, "companies[1000].tax_codes[" + c0tax + "].components[gst].account"},
		{"a reconciliation account as an FX adjustment", func(c *Config) {
			c.Companies[0].FXValuation.Accounts = []FXValuationAccount{{Account: "211000", Adjustment: "160000", UnrealisedGain: "400000", UnrealisedLoss: "610000"}}
		}, "companies[1000].fx_valuation.accounts[211000].adjustment"},
		{"a reconciliation account for withholding", func(c *Config) {
			c.Companies[0].Account("160000").Type = Liability
			c.Companies[0].WithholdingTaxCodes = []WithholdingTaxCode{{Code: "W1", Description: "Withholding", RatePercent: "10", Account: "160000",
				Posting: WithholdingAtInvoice}}
		}, "companies[1000].withholding_tax_codes[W1].account"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := withAssets(t)
			tc.mutate(c)
			fs := Validate(c, currency.Seeded())
			if !slices.ContainsFunc(fs, func(f Finding) bool { return f.Severity == SevError && f.Path == tc.path }) {
				t.Fatalf("no error at %s; got %v", tc.path, fs)
			}
		})
	}
}

// TestAssetClassesDiff: classes are compared by code and their terms by
// ledger, a company's asset accounts by class; a class's name is cosmetic,
// its terms and the accounts posting-related, and asset_reconciliation is
// the chart's, structural.
func TestAssetClassesDiff(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(c *Config)
		path   string
		tier   Tier
	}{
		{"a class's name", func(c *Config) { c.AssetClasses[0].Name = "Machines" }, "asset_classes[MACHINES].name", TierCosmetic},
		{"a ledger's useful life", func(c *Config) { c.AssetClasses[0].Terms[1].UsefulLifeMonths = 96 }, "asset_classes[MACHINES].terms[2L].useful_life_months",
			TierPostingRelated},
		{"a new class", func(c *Config) {
			c.AssetClasses = append(c.AssetClasses, AssetClass{Code: "VEHICLES", Name: "Vehicles", Terms: c.AssetClasses[0].Terms})
		}, "asset_classes[VEHICLES]", TierPostingRelated},
		{"a company's account", func(c *Config) { c.Companies[0].AssetAccounts[0].Loss = "610000" }, "companies[1000].asset_accounts[MACHINES].loss",
			TierPostingRelated},
		{"a reconciliation flag", func(c *Config) { c.Companies[0].Account("169000").AssetReconciliation = true },
			"companies[1000].chart_of_accounts[169000].asset_reconciliation", TierStructural},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, c := withAssets(t), withAssets(t)
			tc.mutate(c)
			changes, err := Diff(base, c)
			if err != nil {
				t.Fatal(err)
			}
			if len(changes) != 1 || changes[0].Path != tc.path {
				t.Fatalf("changes: got %v, want one at %s", changes, tc.path)
			}
			if changes[0].Tier != tc.tier {
				t.Errorf("tier of %s: got %v, want %v", tc.path, changes[0].Tier, tc.tier)
			}
		})
	}
}

// TestAssetLookups: a class and its terms are found by code and ledger, a
// company's accounts by class, and none for codes the document lacks.
func TestAssetLookups(t *testing.T) {
	c := withAssets(t)
	cls := c.AssetClass("MACHINES")
	if cls == nil || cls.TermsIn("2L") == nil || cls.TermsIn("2L").DecliningFactor != "2" {
		t.Fatalf("class MACHINES in 2L: got %+v, want declining balance at 2", cls)
	}
	if got := cls.TermsIn("9L"); got != nil {
		t.Errorf("terms in 9L: got %+v, want none", got)
	}
	if got := c.AssetClass("VEHICLES"); got != nil {
		t.Errorf("class VEHICLES: got %+v, want none", got)
	}
	if got := c.Companies[0].AssetAccountsOf("MACHINES"); got == nil || got.AcquisitionCost != "160000" {
		t.Errorf("accounts of MACHINES: got %+v, want acquisition cost 160000", got)
	}
	if got := c.Companies[0].AssetAccountsOf("VEHICLES"); got != nil {
		t.Errorf("accounts of VEHICLES: got %+v, want none", got)
	}
}
