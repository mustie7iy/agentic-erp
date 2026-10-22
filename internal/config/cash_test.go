package config

import (
	"slices"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// withCash is the fixture with cash management (ADR-0051 §4): an operating,
// investing and financing tree, two planning groups, rules of every kind
// and defaults, a posting rule for bank charges in company 1000, and
// company 1000's settings all given.
func withCash(t *testing.T) *Config {
	t.Helper()
	c := fixture(t)
	c.Cash = Cash{
		LiquidityItems: []LiquidityItem{
			{Code: "OPERATING", Name: "Operating", Direction: DirectionBoth},
			{Code: "INVESTING", Name: "Investing", Direction: DirectionBoth},
			{Code: "FINANCING", Name: "Financing", Direction: DirectionBoth},
			{Code: "OP_SUPPLIERS", Name: "Supplier payments", Direction: DirectionOutflow, Parent: "OPERATING"},
			{Code: "OP_SALARIES", Name: "Salaries", Direction: DirectionOutflow, Parent: "OPERATING"},
			{Code: "TAX", Name: "Taxes", Direction: DirectionOutflow, Parent: "OPERATING"},
			{Code: "BANK_CHARGES", Name: "Bank charges", Direction: DirectionBoth, Parent: "OPERATING"},
			{Code: "REC_CUSTOMERS", Name: "Customer receipts", Direction: DirectionInflow, Parent: "OPERATING"},
			{Code: "CAPEX", Name: "Capital expenditure", Direction: DirectionOutflow, Parent: "INVESTING"},
			{Code: "FIN_LOAN", Name: "Loans", Direction: DirectionBoth, Parent: "FINANCING"},
		},
		PlanningGroups: []PlanningGroup{{Code: "TAX_AUTHORITY", Name: "Tax authorities"}, {Code: "DOMESTIC", Name: "Domestic suppliers"}},
		Derivation: Derivation{
			Accounts:       []AccountDerivation{{From: "610000", To: "610000", Item: "OP_SALARIES"}, {From: "500000", To: "599999", Item: "OP_SUPPLIERS"}},
			PlanningGroups: []PlanningGroupDerivation{{Group: "TAX_AUTHORITY", Item: "TAX"}},
			PostingRules:   []PostingRuleDerivation{{Rule: "NCHG", Item: "BANK_CHARGES"}},
			Defaults:       DerivationDefaults{Outflow: "OP_SUPPLIERS", Inflow: "REC_CUSTOMERS"},
		},
	}
	co := &c.Companies[0]
	co.BankStatements.PostingRules = []StatementPostingRule{{TransactionCode: "NCHG", Description: "Charges", Account: "610000"}}
	no, seven, lead := false, 7, 45
	co.Cash = CompanyCash{TakeDiscounts: &no, PaymentDays: []string{"tue", "thu"},
		ValueDays: []ValueDays{{HouseBank: "MAIN", Days: 1}, {HouseBank: "MAIN", Method: "T", Days: 2}},
		Buckets:   []Bucket{{GrainDay, 7}, {GrainWeek, 4}, {GrainMonth, 12}}, RateType: "average", RateMaxAgeDays: &seven, OrderLeadDays: &lead,
		CreditLines: []CreditLine{{HouseBank: "MAIN", Amount: "250000.00"}}, SnapshotTime: "06:30"}
	return c
}

// TestCashValid: cash management settings are optional, and with all of
// them the fixture is valid without warnings about them.
func TestCashValid(t *testing.T) {
	fs := Validate(withCash(t), currency.Seeded())
	if HasErrors(fs) {
		t.Fatalf("with cash: got %v, want no errors", fs)
	}
	for _, f := range fs {
		if len(f.Path) >= 4 && f.Path[:4] == "cash" {
			t.Errorf("warning %v: want none about cash", f)
		}
	}
}

// TestCashValidation: each cash setting is checked (ADR-0051 §4, §5, §8-§10).
func TestCashValidation(t *testing.T) {
	const cp = "companies[1000].cash"
	item := func(c *Config, code string) *LiquidityItem {
		return &c.Cash.LiquidityItems[slices.IndexFunc(c.Cash.LiquidityItems, func(it LiquidityItem) bool { return it.Code == code })]
	}
	cash := func(c *Config) *CompanyCash { return &c.Companies[0].Cash }
	n := func(i int) *int { return &i }
	cases := []struct {
		name   string
		mutate func(c *Config)
		path   string
	}{
		{"an item code", func(c *Config) { item(c, "TAX").Code = "tax" }, "cash.liquidity_items[tax].code"},
		{"an item twice", func(c *Config) { c.Cash.LiquidityItems = append(c.Cash.LiquidityItems, *item(c, "TAX")) }, "cash.liquidity_items[TAX].code"},
		{"an item without a name", func(c *Config) { item(c, "TAX").Name = " " }, "cash.liquidity_items[TAX].name"},
		{"an item's direction", func(c *Config) { item(c, "TAX").Direction = "out" }, "cash.liquidity_items[TAX].direction"},
		{"an unknown parent", func(c *Config) { item(c, "TAX").Parent = "NONE" }, "cash.liquidity_items[TAX].parent"},
		{"an unknown grandparent", func(c *Config) { item(c, "OPERATING").Parent = "TOP" }, "cash.liquidity_items[OPERATING].parent"},
		{"a parent of the other direction", func(c *Config) { item(c, "OP_SUPPLIERS").Parent = "REC_CUSTOMERS" }, "cash.liquidity_items[OP_SUPPLIERS].parent"},
		{"a cycle", func(c *Config) { item(c, "OPERATING").Parent = "BANK_CHARGES" }, "cash.liquidity_items[OPERATING].parent"},
		{"five levels", func(c *Config) {
			c.Cash.LiquidityItems = append(c.Cash.LiquidityItems, LiquidityItem{Code: "L3", Name: "3", Direction: DirectionBoth, Parent: "FIN_LOAN"},
				LiquidityItem{Code: "L4", Name: "4", Direction: DirectionBoth, Parent: "L3"}, LiquidityItem{Code: "L5", Name: "5", Direction: DirectionBoth, Parent: "L4"})
		}, "cash.liquidity_items[L5].parent"},
		{"a planning group code", func(c *Config) { c.Cash.PlanningGroups[0].Code = "1X" }, "cash.planning_groups[1X].code"},
		{"a planning group twice", func(c *Config) { c.Cash.PlanningGroups[1].Code = "TAX_AUTHORITY" }, "cash.planning_groups[TAX_AUTHORITY].code"},
		{"a planning group without a name", func(c *Config) { c.Cash.PlanningGroups[0].Name = "" }, "cash.planning_groups[TAX_AUTHORITY].name"},
		{"an account rule's numbers", func(c *Config) { c.Cash.Derivation.Accounts[0].From = "6100-00" }, "cash.derivation.accounts[0]"},
		{"an account rule ending before it starts", func(c *Config) { c.Cash.Derivation.Accounts[1].To = "499999" }, "cash.derivation.accounts[1].to"},
		{"an account rule's unknown item", func(c *Config) { c.Cash.Derivation.Accounts[0].Item = "RENT" }, "cash.derivation.accounts[0].item"},
		{"an account rule's item with children", func(c *Config) { c.Cash.Derivation.Accounts[0].Item = "OPERATING" }, "cash.derivation.accounts[0].item"},
		{"an unknown planning group", func(c *Config) { c.Cash.Derivation.PlanningGroups[0].Group = "FOREIGN" }, "cash.derivation.planning_groups[FOREIGN].group"},
		{"a planning group's rule twice", func(c *Config) {
			c.Cash.Derivation.PlanningGroups = append(c.Cash.Derivation.PlanningGroups, c.Cash.Derivation.PlanningGroups[0])
		}, "cash.derivation.planning_groups[TAX_AUTHORITY].group"},
		{"a planning group's item", func(c *Config) { c.Cash.Derivation.PlanningGroups[0].Item = "INVESTING" }, "cash.derivation.planning_groups[TAX_AUTHORITY].item"},
		{"a planning group's item for inflows", func(c *Config) { c.Cash.Derivation.PlanningGroups[0].Item = "REC_CUSTOMERS" },
			"cash.derivation.planning_groups[TAX_AUTHORITY].item"},
		{"a posting rule no company has", func(c *Config) { c.Cash.Derivation.PostingRules[0].Rule = "NINT" }, "cash.derivation.posting_rules[NINT].rule"},
		{"a posting rule's rule twice", func(c *Config) {
			c.Cash.Derivation.PostingRules = append(c.Cash.Derivation.PostingRules, c.Cash.Derivation.PostingRules[0])
		}, "cash.derivation.posting_rules[NCHG].rule"},
		{"a posting rule's item", func(c *Config) { c.Cash.Derivation.PostingRules[0].Item = "X" }, "cash.derivation.posting_rules[NCHG].item"},
		{"an outflow default for inflows", func(c *Config) { c.Cash.Derivation.Defaults.Outflow = "REC_CUSTOMERS" }, "cash.derivation.defaults.outflow"},
		{"an inflow default for outflows", func(c *Config) { c.Cash.Derivation.Defaults.Inflow = "CAPEX" }, "cash.derivation.defaults.inflow"},
		{"a payment day", func(c *Config) { cash(c).PaymentDays = []string{"tuesday"} }, cp + ".payment_days[0]"},
		{"a payment day twice", func(c *Config) { cash(c).PaymentDays = []string{"tue", "tue"} }, cp + ".payment_days[1]"},
		{"value days of an unknown bank", func(c *Config) { cash(c).ValueDays[0].HouseBank = "OTHER" }, cp + ".value_days[OTHER/].house_bank"},
		{"value days twice", func(c *Config) { cash(c).ValueDays[1].Method = "" }, cp + ".value_days[MAIN/].house_bank"},
		{"value days of an unknown method", func(c *Config) { cash(c).ValueDays[1].Method = "X" }, cp + ".value_days[MAIN/X].method"},
		{"too many value days", func(c *Config) { cash(c).ValueDays[0].Days = 31 }, cp + ".value_days[MAIN/].days"},
		{"negative value days", func(c *Config) { cash(c).ValueDays[0].Days = -1 }, cp + ".value_days[MAIN/].days"},
		{"a credit line of an unknown bank", func(c *Config) { cash(c).CreditLines[0].HouseBank = "OTHER" }, cp + ".credit_lines[OTHER].house_bank"},
		{"a credit line twice", func(c *Config) { cash(c).CreditLines = append(cash(c).CreditLines, cash(c).CreditLines[0]) }, cp + ".credit_lines[MAIN].house_bank"},
		{"a credit line's amount", func(c *Config) { cash(c).CreditLines[0].Amount = "1.001" }, cp + ".credit_lines[MAIN].amount"},
		{"a credit line of nothing", func(c *Config) { cash(c).CreditLines[0].Amount = "0" }, cp + ".credit_lines[MAIN].amount"},
		{"a bucket grain", func(c *Config) { cash(c).Buckets[0].Grain = "quarter" }, cp + ".buckets[0].grain"},
		{"buckets out of order", func(c *Config) { cash(c).Buckets[2].Grain = GrainDay }, cp + ".buckets[2].grain"},
		{"a grain twice", func(c *Config) { cash(c).Buckets[1].Grain = GrainDay }, cp + ".buckets[1].grain"},
		{"no periods", func(c *Config) { cash(c).Buckets[0].Count = 0 }, cp + ".buckets[0].count"},
		{"too many days", func(c *Config) { cash(c).Buckets[0].Count = 93 }, cp + ".buckets[0].count"},
		{"a rate type", func(c *Config) { cash(c).RateType = "spot" }, cp + ".rate_type"},
		{"a rate age of none", func(c *Config) { cash(c).RateMaxAgeDays = n(0) }, cp + ".rate_max_age_days"},
		{"a rate age of over a year", func(c *Config) { cash(c).RateMaxAgeDays = n(366) }, cp + ".rate_max_age_days"},
		{"negative lead days", func(c *Config) { cash(c).OrderLeadDays = n(-1) }, cp + ".order_lead_days"},
		{"lead days of over a year", func(c *Config) { cash(c).OrderLeadDays = n(366) }, cp + ".order_lead_days"},
		{"a snapshot time", func(c *Config) { cash(c).SnapshotTime = "7am" }, cp + ".snapshot_time"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := withCash(t)
			tc.mutate(c)
			fs := Validate(c, currency.Seeded())
			if !slices.ContainsFunc(fs, func(f Finding) bool { return f.Severity == SevError && f.Path == tc.path }) {
				t.Errorf("got findings %v, want an error at %s", fs, tc.path)
			}
		})
	}
}

// TestCashWarnings: a rule covering no account and missing defaults are
// allowed, shown to approvers; credit lines of a bank in an unknown
// currency are left to that currency's finding.
func TestCashWarnings(t *testing.T) {
	c := withCash(t)
	c.Cash.Derivation.Accounts[0] = AccountDerivation{From: "900000", To: "999999", Item: "CAPEX"}
	c.Cash.Derivation.Defaults = DerivationDefaults{}
	fs := Validate(c, currency.Seeded())
	for _, path := range []string{"cash.derivation.accounts[0]", "cash.derivation.defaults.outflow", "cash.derivation.defaults.inflow"} {
		if !slices.ContainsFunc(fs, func(f Finding) bool { return f.Severity == SevWarning && f.Path == path }) {
			t.Errorf("got findings %v, want a warning at %s", fs, path)
		}
	}
	t.Run("no items, no defaults to miss", func(t *testing.T) {
		fs := Validate(fixture(t), currency.Seeded())
		if slices.ContainsFunc(fs, func(f Finding) bool { return f.Path == "cash.derivation.defaults.outflow" }) {
			t.Errorf("got findings %v, want none about defaults", fs)
		}
	})
	t.Run("a credit line of a bank in an unknown currency", func(t *testing.T) {
		c := withCash(t)
		c.Companies[0].HouseBank.Currency = "XXY"
		c.Companies[0].Cash.CreditLines[0].Amount = "x"
		fs := Validate(c, currency.Seeded())
		if slices.ContainsFunc(fs, func(f Finding) bool { return f.Path == "companies[1000].cash.credit_lines[MAIN].amount" }) {
			t.Errorf("got findings %v, want none about the amount", fs)
		}
	})
}

// TestCheckLiquidityItems: a country pack's items are checked as the
// document's are, at the pack's path.
func TestCheckLiquidityItems(t *testing.T) {
	if fs := CheckLiquidityItems("cash.yaml: liquidity_items", []LiquidityItem{{Code: "OPERATING", Name: "Operating", Direction: DirectionBoth}}); len(fs) != 0 {
		t.Errorf("a valid tree: got %v, want none", fs)
	}
	fs := CheckLiquidityItems("cash.yaml: liquidity_items", []LiquidityItem{{Code: "OP", Name: "Op", Direction: DirectionBoth, Parent: "TOP"}})
	if len(fs) != 1 || fs[0].Path != "cash.yaml: liquidity_items[OP].parent" {
		t.Errorf("an unknown parent: got %v, want one finding at its parent", fs)
	}
}

// TestCompanyCashDefaults: an empty setting is its default (ADR-0051).
func TestCompanyCashDefaults(t *testing.T) {
	var none CompanyCash
	set := withCash(t).Companies[0].Cash
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"take discounts by default", none.TakesDiscounts(), true},
		{"take discounts set", set.TakesDiscounts(), false},
		{"rate type by default", none.RateTypeOrPlanning(), "planning"},
		{"rate type set", set.RateTypeOrPlanning(), "average"},
		{"rate age by default", none.RateMaxAge(), 7},
		{"rate age set", set.RateMaxAge(), 7},
		{"lead days by default", none.OrderLead(), 30},
		{"lead days set", set.OrderLead(), 45},
		{"snapshot time by default", none.SnapshotAt(), "07:00"},
		{"snapshot time set", set.SnapshotAt(), "06:30"},
		{"buckets by default", len(none.BucketsOrDefault()), 3},
		{"buckets set", set.BucketsOrDefault()[2].Count, 12},
		{"value days by method", set.ValueDaysFor("MAIN", "T"), 2},
		{"value days for every method", set.ValueDaysFor("MAIN", "C"), 1},
		{"value days of another bank", set.ValueDaysFor("OTHER", "T"), 0},
		{"value days by default", none.ValueDaysFor("MAIN", "T"), 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got %v, want %v", c.got, c.want)
			}
		})
	}
	t.Run("the default buckets are a copy", func(t *testing.T) {
		b := none.BucketsOrDefault()
		b[0].Count = 1
		if DefaultBuckets[0].Count != 14 {
			t.Errorf("DefaultBuckets[0].Count: got %d, want 14", DefaultBuckets[0].Count)
		}
	})
}

// TestAccountRuleCovers: account numbers compare as SAP's ALPHA
// conversion keeps them.
func TestAccountRuleCovers(t *testing.T) {
	r := AccountDerivation{From: "1500", To: "15990"}
	for number, want := range map[string]bool{"1500": true, "6100": true, "15990": true, "1499": false, "16000": false, "A100": false} {
		t.Run(number, func(t *testing.T) {
			if got := r.Covers(number); got != want {
				t.Errorf("Covers(%s): got %v, want %v", number, got, want)
			}
		})
	}
	t.Run("alphanumeric numbers as they are", func(t *testing.T) {
		if !(AccountDerivation{From: "A100", To: "A199"}).Covers("A150") {
			t.Error("Covers(A150) of A100-A199: got false, want true")
		}
	})
}

// TestCashDiffTiers: cash settings post nothing; names and the hierarchy
// are cosmetic, the rest operational (ADR-0051 §4). The account rules are
// compared by position, their order being a setting.
func TestCashDiffTiers(t *testing.T) {
	base := withCash(t)
	cases := []struct {
		name   string
		mutate func(c *Config)
		path   string
		tier   Tier
	}{
		{"an item's name", func(c *Config) { c.Cash.LiquidityItems[3].Name = "Suppliers" }, "cash.liquidity_items[OP_SUPPLIERS].name", TierCosmetic},
		{"an item's parent", func(c *Config) { c.Cash.LiquidityItems[9].Parent = "INVESTING" }, "cash.liquidity_items[FIN_LOAN].parent", TierCosmetic},
		{"an item's direction", func(c *Config) { c.Cash.LiquidityItems[9].Direction = DirectionInflow }, "cash.liquidity_items[FIN_LOAN].direction", TierOperational},
		{"a new item", func(c *Config) {
			c.Cash.LiquidityItems = append(c.Cash.LiquidityItems, LiquidityItem{Code: "RENT", Name: "Rent", Direction: DirectionOutflow, Parent: "OPERATING"})
		}, "cash.liquidity_items[RENT]", TierOperational},
		{"a group's name", func(c *Config) { c.Cash.PlanningGroups[0].Name = "Tax" }, "cash.planning_groups[TAX_AUTHORITY].name", TierCosmetic},
		{"an account rule's item", func(c *Config) { c.Cash.Derivation.Accounts[0].Item = "TAX" }, "cash.derivation.accounts[0].item", TierOperational},
		{"a planning group's rule", func(c *Config) { c.Cash.Derivation.PlanningGroups[0].Item = "CAPEX" },
			"cash.derivation.planning_groups[TAX_AUTHORITY].item", TierOperational},
		{"a posting rule's rule", func(c *Config) { c.Cash.Derivation.PostingRules[0].Item = "TAX" }, "cash.derivation.posting_rules[NCHG].item", TierOperational},
		{"a default", func(c *Config) { c.Cash.Derivation.Defaults.Inflow = "FIN_LOAN" }, "cash.derivation.defaults.inflow", TierOperational},
		{"payment days", func(c *Config) { c.Companies[0].Cash.PaymentDays[1] = "fri" }, "companies[1000].cash.payment_days[1]", TierOperational},
		{"value days", func(c *Config) { c.Companies[0].Cash.ValueDays[1].Days = 3 }, "companies[1000].cash.value_days[MAIN/T].days", TierOperational},
		{"a credit line", func(c *Config) { c.Companies[0].Cash.CreditLines[0].Amount = "300000.00" }, "companies[1000].cash.credit_lines[MAIN].amount", TierOperational},
		{"the snapshot time", func(c *Config) { c.Companies[0].Cash.SnapshotTime = "08:00" }, "companies[1000].cash.snapshot_time", TierOperational},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := withCash(t)
			tc.mutate(c)
			changes, _ := Diff(base, c)
			if len(changes) != 1 {
				t.Fatalf("changes: got %v, want 1", changes)
			}
			if changes[0].Path != tc.path || changes[0].Tier != tc.tier {
				t.Errorf("change: got %s %v, want %s %v", changes[0].Path, changes[0].Tier, tc.path, tc.tier)
			}
		})
	}
	t.Run("account rules swapped", func(t *testing.T) {
		c := withCash(t)
		a := c.Cash.Derivation.Accounts
		a[0], a[1] = a[1], a[0]
		changes, _ := Diff(base, c)
		if len(changes) == 0 || MaxTier(changes) != TierOperational {
			t.Errorf("changes: got %v, want operational changes", changes)
		}
	})
}
