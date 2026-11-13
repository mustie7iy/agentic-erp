package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// controlled is company 1000 with GR/IR, FX valuation and statement
// postings, cost and profit center accounting on and a default cost center
// for each expense the system posts to: price differences (590000), the
// unrealised loss (610000) and bank charges (690000).
func controlled(t *testing.T) *Config {
	t.Helper()
	c := withFXValuation(t)
	reconciling(c)
	co := &c.Companies[0]
	co.CostCenterAccounting, co.ProfitCenterAccounting, co.DefaultProfitCenter = true, true, "PC-DUMMY"
	co.Account("300000").CostElement = CostElementNone
	co.Account("400000").CostElement = CostElementRevenue
	co.Account("520000").CostElement = CostElementPrimaryCost
	co.DefaultAssignments = []DefaultAssignment{{Account: "590000", CostCenter: "1000"}, {Account: "610000", CostCenter: "1000"},
		{Account: "690000", CostCenter: "ADMIN-1"}}
	return c
}

func TestControllingValid(t *testing.T) {
	c := controlled(t)
	if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
		t.Fatalf("controlled company: got %v, want no errors", fs)
	}
	t.Run("a secondary cost element on an expense nothing else posts to", func(t *testing.T) {
		c := controlled(t)
		c.Companies[0].Account("520000").CostElement = CostElementSecondary
		if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
			t.Fatalf("got %v, want no errors", fs)
		}
		if got := c.Companies[0].Account("520000").CostElementOrDefault(); got != CostElementSecondary {
			t.Errorf("the account's cost element: got %s, want secondary", got)
		}
	})
	t.Run("an uncontrolled expense needs no default", func(t *testing.T) {
		c := controlled(t)
		c.Companies[0].Account("690000").CostElement = CostElementNone
		c.Companies[0].DefaultAssignments = c.Companies[0].DefaultAssignments[:2]
		if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
			t.Fatalf("got %v, want no errors", fs)
		}
	})
	t.Run("without cost center accounting no default is needed", func(t *testing.T) {
		c := controlled(t)
		c.Companies[0].CostCenterAccounting, c.Companies[0].DefaultAssignments = false, nil
		if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
			t.Fatalf("got %v, want no errors", fs)
		}
	})
}

// Each mutation of the controlled configuration must produce an error
// finding at the expected path.
func TestControllingRules(t *testing.T) {
	const p = "companies[1000]"
	cases := []struct {
		name   string
		mutate func(c *Config)
		path   string
	}{
		{"category code", func(c *Config) { c.Controlling.CostCenterCategories[0].Code = "admin" }, "controlling.cost_center_categories[admin]"},
		{"category twice", func(c *Config) { c.Controlling.CostCenterCategories[1].Code = "ADMIN" }, "controlling.cost_center_categories[ADMIN]"},
		{"category without a name", func(c *Config) { c.Controlling.CostCenterCategories[0].Name = " " }, "controlling.cost_center_categories[ADMIN].name"},
		{"unknown cost element", func(c *Config) { c.Companies[0].Account("520000").CostElement = "costs" }, p + ".chart_of_accounts[520000].cost_element"},
		{"secondary cost element on a revenue", func(c *Config) {
			c.Companies[0].Account("400000").CostElement = CostElementSecondary
		}, p + ".chart_of_accounts[400000].cost_element"},
		{"secondary cost element on an asset", func(c *Config) {
			c.Companies[0].Account("100000").CostElement = CostElementSecondary
		}, p + ".chart_of_accounts[100000].cost_element"},
		{"secondary cost element with a default cost center", func(c *Config) {
			c.Companies[0].Account("590000").CostElement = CostElementSecondary
		}, p + ".default_assignments[590000].account"},
		{"secondary cost element an automatic posting uses", func(c *Config) {
			c.Companies[0].Account("610000").CostElement = CostElementSecondary
		}, p + ".fx_valuation.accounts[100000].unrealised_loss"},
		{"secondary cost element with open items", func(c *Config) {
			c.Companies[0].Account("520000").CostElement, c.Companies[0].Account("520000").OpenItems = CostElementSecondary, true
		}, p + ".chart_of_accounts[520000].open_items"},
		{"primary cost on a revenue", func(c *Config) {
			c.Companies[0].Account("400000").CostElement = CostElementPrimaryCost
		}, p + ".chart_of_accounts[400000].cost_element"},
		{"primary cost on an asset", func(c *Config) {
			c.Companies[0].Account("100000").CostElement = CostElementPrimaryCost
		}, p + ".chart_of_accounts[100000].cost_element"},
		{"revenue element on an expense", func(c *Config) {
			c.Companies[0].Account("520000").CostElement = CostElementRevenue
		}, p + ".chart_of_accounts[520000].cost_element"},
		{"default for an account twice", func(c *Config) {
			c.Companies[0].DefaultAssignments[1].Account = "590000"
		}, p + ".default_assignments[590000]"},
		{"default for an account not in the chart", func(c *Config) {
			c.Companies[0].DefaultAssignments[0].Account = "999999"
		}, p + ".default_assignments[999999].account"},
		{"default for a revenue", func(c *Config) {
			c.Companies[0].DefaultAssignments[0].Account = "400000"
		}, p + ".default_assignments[400000].account"},
		{"default for an uncontrolled expense", func(c *Config) {
			c.Companies[0].Account("590000").CostElement = CostElementNone
		}, p + ".default_assignments[590000].account"},
		{"default cost center code", func(c *Config) { c.Companies[0].DefaultAssignments[0].CostCenter = "cc 1" }, p + ".default_assignments[590000].cost_center"},
		{"profit center accounting without default", func(c *Config) { c.Companies[0].DefaultProfitCenter = "" }, p + ".default_profit_center"},
		{"default profit center without accounting", func(c *Config) { c.Companies[0].ProfitCenterAccounting = false }, p + ".default_profit_center"},
		{"default profit center code", func(c *Config) { c.Companies[0].DefaultProfitCenter = "dummy" }, p + ".default_profit_center"},
		{"price differences without default", func(c *Config) {
			c.Companies[0].DefaultAssignments = c.Companies[0].DefaultAssignments[1:]
		}, p + ".default_assignments[590000]"},
		{"bank charges without default", func(c *Config) {
			c.Companies[0].DefaultAssignments = c.Companies[0].DefaultAssignments[:2]
		}, p + ".default_assignments[690000]"},
		{"FX loss without default", func(c *Config) {
			c.Companies[0].ChartOfAccounts = append(c.Companies[0].ChartOfAccounts, Account{Number: "680000", Name: "FX losses", Type: Expense})
			c.Companies[0].AccountDetermination.FXLoss = "680000"
		}, p + ".default_assignments[680000]"},
		{"a tax account that is an expense", func(c *Config) { c.Companies[0].TaxCodes[0].Account = "530000" }, p + ".default_assignments[530000]"},
		{"a tax component's account that is an expense", func(c *Config) {
			c.Companies[0].TaxCodes = append(c.Companies[0].TaxCodes, componentCode("CA", "530000"))
		}, p + ".default_assignments[530000]"},
		{"cash discount on an expense", func(c *Config) {
			c.Companies[0].Account("400000").Type = Expense
			c.Companies[0].Account("400000").CostElement = ""
			c.Companies[0].AccountDetermination.CashDiscount = "400000"
		}, p + ".default_assignments[400000]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := controlled(t)
			tc.mutate(c)
			fs := Validate(c, currency.Seeded())
			if !slices.ContainsFunc(fs, func(f Finding) bool { return f.Severity == SevError && f.Path == tc.path }) {
				t.Fatalf("no error at %s; got %v", tc.path, fs)
			}
		})
	}
}

// One finding per account names every automatic setting posting to it.
func TestAutomaticAccountFindingNamesEveryUse(t *testing.T) {
	c := controlled(t)
	c.Companies[0].DefaultAssignments = c.Companies[0].DefaultAssignments[:1]
	var got []Finding
	for _, f := range Validate(c, currency.Seeded()) {
		if f.Path == "companies[1000].default_assignments[610000]" {
			got = append(got, f)
		}
	}
	if len(got) != 1 {
		t.Fatalf("findings for 610000: got %v, want one", got)
	}
	for _, use := range []string{"fx_valuation.accounts[100000].unrealised_loss", "fx_valuation.accounts[211000].unrealised_loss"} {
		if !strings.Contains(got[0].Message, use) {
			t.Errorf("message %q: want it to name %s", got[0].Message, use)
		}
	}
}

// componentCode is an input tax code of two components: one posting to
// account, one non-deductible, which posts to no tax account.
func componentCode(code, account string) TaxCode {
	return TaxCode{Code: code, Description: "GST and PST", Components: []TaxComponent{
		{Key: "gst", AccountKey: KeyInput, Account: account, Rates: []TaxRate{{RatePercent: "5", ValidFrom: "2026-01-01"}}},
		{Key: "pst_bc", AccountKey: KeyNonDeductible, Rates: []TaxRate{{RatePercent: "7", ValidFrom: "2026-01-01"}}}}}
}

func TestCostElementOrDefault(t *testing.T) {
	for _, tc := range []struct {
		account Account
		want    CostElement
	}{
		{Account{Type: Expense}, CostElementPrimaryCost},
		{Account{Type: Revenue}, CostElementRevenue},
		{Account{Type: Asset}, CostElementNone},
		{Account{Type: Liability}, CostElementNone},
		{Account{Type: Equity}, CostElementNone},
		{Account{Type: Expense, CostElement: CostElementNone}, CostElementNone},
	} {
		t.Run(string(tc.account.Type)+"/"+string(tc.account.CostElement), func(t *testing.T) {
			if got := tc.account.CostElementOrDefault(); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestAutomaticAccounts(t *testing.T) {
	c := controlled(t)
	furtherBank("101000")(c)
	co := c.Companies[0]
	co.WithholdingTaxCodes = []WithholdingTaxCode{{Code: "W1", Account: "211099"}}
	co.HouseBank.GLAccount = "100000"
	co.TaxCodes = append(co.TaxCodes, componentCode("CA", "530000"))
	got := map[string]string{}
	for _, a := range co.AutomaticAccounts() {
		got[a.Path] = a.Account
	}
	for path, want := range map[string]string{"account_determination.ap_reconciliation": "211000", "account_determination.price_difference": "590000",
		"tax_codes[V1].account": "140000", "withholding_tax_codes[W1].account": "211099", "fx_valuation.accounts[211000].adjustment": "211099",
		"fx_valuation.accounts[100000].unrealised_gain": "400000", "fx_valuation.accounts[100000].unrealised_loss": "610000",
		"house_bank.gl_account": "100000", "house_bank.main_gl_account": "102000", "house_bank.incoming_gl_account": "103000",
		"bank_statements.posting_rules[NCHG].account": "690000", "tax_codes[CA].components[gst].account": "530000"} {
		if got[path] != want {
			t.Errorf("%s: got %q, want %s", path, got[path], want)
		}
	}
	for _, empty := range []string{"account_determination.fx_gain", "tax_codes[CA].account", "tax_codes[CA].components[pst_bc].account"} {
		if _, ok := got[empty]; ok {
			t.Errorf("an empty setting, %s, is listed: %v", empty, got)
		}
	}
	further := c.Companies[1].AutomaticAccounts()
	if !slices.Contains(further, AutomaticAccount{Path: "house_banks[DB].gl_account", Account: "101000"}) {
		t.Errorf("further house bank: got %v", further)
	}
}

func TestControllingLookups(t *testing.T) {
	c := controlled(t)
	if cat := c.Controlling.Category("SALES"); cat == nil || !cat.RevenueAllowed {
		t.Errorf("category SALES: got %v, want one allowing revenue", cat)
	}
	if cat := c.Controlling.Category("NONE"); cat != nil {
		t.Errorf("category NONE: got %v, want nil", cat)
	}
	if d := c.Companies[0].DefaultAssignment("520000"); d != nil {
		t.Errorf("default of 520000: got %v, want nil", d)
	}
}

// TestControllingDiffTiers: a category's name is cosmetic, the rest of the
// categories, cost elements and defaults posting-related, turning center
// accounting on structural.
func TestControllingDiffTiers(t *testing.T) {
	base, changed := fixture(t), controlled(t)
	base.Companies[0] = changed.Companies[0]
	base.Companies[0].CostCenterAccounting, base.Companies[0].ProfitCenterAccounting = false, false
	base.Companies[0].DefaultProfitCenter, base.Companies[0].DefaultAssignments = "", nil
	base.Companies[0].ChartOfAccounts = slices.Clone(changed.Companies[0].ChartOfAccounts)
	base.Companies[0].Account("520000").CostElement = ""
	changed.Controlling.CostCenterCategories[0].Name = "Admin"
	changed.Controlling.CostCenterCategories[1].RevenueAllowed = false
	changed.Controlling.CostCenterCategories = append(changed.Controlling.CostCenterCategories, CostCenterCategory{Code: "PROD", Name: "Production"})
	changes, err := Diff(base, changed)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	tiers := map[string]Tier{}
	for _, ch := range changes {
		tiers[ch.Path] = ch.Tier
	}
	const p = "companies[1000]"
	for path, want := range map[string]Tier{
		"controlling.cost_center_categories[ADMIN].name":            TierCosmetic,
		"controlling.cost_center_categories[SALES].revenue_allowed": TierPostingRelated,
		"controlling.cost_center_categories[PROD]":                  TierPostingRelated,
		p + ".chart_of_accounts[520000].cost_element":               TierPostingRelated,
		p + ".default_assignments[590000]":                          TierPostingRelated,
		p + ".default_profit_center":                                TierPostingRelated,
		p + ".cost_center_accounting":                               TierStructural,
		p + ".profit_center_accounting":                             TierStructural,
	} {
		t.Run(path, func(t *testing.T) {
			if got, ok := tiers[path]; !ok || got != want {
				t.Errorf("got %v (listed %v), want %v; all %v", got, ok, want, tiers)
			}
		})
	}
}

// margined is the controlled configuration whose margin report counts
// company 1000's accounts 510000 to 530000 and 540000 as cost of sales.
func margined(t *testing.T) *Config {
	t.Helper()
	c := controlled(t)
	c.Controlling.MarginReport = []MarginChart{{Chart: "1000", CostOfSalesAccounts: []AccountRange{{From: "510000", To: "530000"}, {From: "540000"}}}}
	return c
}

func TestMarginReportValid(t *testing.T) {
	c := margined(t)
	if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
		t.Fatalf("got %v, want no errors", fs)
	}
	numbers, named := c.CostOfSalesAccounts("1000")
	if got, want := strings.Join(numbers, " "), "510000 520000 530000 540000"; got != want || !named {
		t.Errorf("company 1000's cost of sales accounts: got %q named %v, want %q named true", got, named, want)
	}
	for _, company := range []string{"2000", "9999"} {
		t.Run("none named for "+company, func(t *testing.T) {
			if numbers, named := c.CostOfSalesAccounts(company); numbers == nil || len(numbers) != 0 || named {
				t.Errorf("got %v named %v, want an empty list, not named", numbers, named)
			}
		})
	}
	if got, want := (AccountRange{From: "510000", To: "530000"}).String()+", "+(AccountRange{From: "540000"}).String(), "510000 to 530000, 540000"; got != want {
		t.Errorf("ranges as text: got %q, want %q", got, want)
	}
}

// Each mutation of the margin report's section must produce an error
// finding at the expected path.
func TestMarginReportRules(t *testing.T) {
	const p = "controlling.margin_report[1000]"
	set := func(r ...AccountRange) func(*Config) {
		return func(c *Config) { c.Controlling.MarginReport[0].CostOfSalesAccounts = r }
	}
	for _, tc := range []struct {
		name   string
		mutate func(c *Config)
		path   string
	}{
		{"a chart that is no company", func(c *Config) { c.Controlling.MarginReport[0].Chart = "9999" }, "controlling.margin_report[9999].chart"},
		{"a chart twice", func(c *Config) {
			c.Controlling.MarginReport = append(c.Controlling.MarginReport, c.Controlling.MarginReport[0])
		}, p + ".chart"},
		{"no accounts", set(), p + ".cost_of_sales_accounts"},
		{"from is no account number", set(AccountRange{From: "5-1"}), p + ".cost_of_sales_accounts[0]"},
		{"to is no account number", set(AccountRange{From: "510000", To: "5 3"}), p + ".cost_of_sales_accounts[0]"},
		{"a range ending before it starts", set(AccountRange{From: "530000", To: "510000"}), p + ".cost_of_sales_accounts[0].to"},
		{"an account the chart lacks", set(AccountRange{From: "510000"}, AccountRange{From: "777777"}), p + ".cost_of_sales_accounts[1]"},
		{"a range covering no account", set(AccountRange{From: "700000", To: "799999"}), p + ".cost_of_sales_accounts[0]"},
		{"a revenue account", set(AccountRange{From: "400000"}), p + ".cost_of_sales_accounts[0]"},
		{"a range reaching a balance sheet account", set(AccountRange{From: "300000", To: "520000"}), p + ".cost_of_sales_accounts[0]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := margined(t)
			tc.mutate(c)
			fs := Validate(c, currency.Seeded())
			if !slices.ContainsFunc(fs, func(f Finding) bool { return f.Severity == SevError && f.Path == tc.path }) {
				t.Errorf("got %v, want an error at %s", fs, tc.path)
			}
		})
	}
}

// The margin report's accounts change what a report says and post
// nothing: operational, not posting-relevant as the section's other rules.
func TestMarginReportTier(t *testing.T) {
	before, after := margined(t), margined(t)
	after.Controlling.MarginReport[0].CostOfSalesAccounts[1].From = "610000"
	changes, err := Diff(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Path != "controlling.margin_report[1000].cost_of_sales_accounts[1].from" || changes[0].Tier != TierOperational {
		t.Errorf("got %v, want one operational change of controlling.margin_report[1000].cost_of_sales_accounts[1].from", changes)
	}
}
