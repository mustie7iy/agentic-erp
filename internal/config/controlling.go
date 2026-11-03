package config

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Controlling is the tenant's rules about cost and profit centers
// (ADR-0050 §4, T56c): the centers are master data maintained through the
// API, the rules about them are configuration.
type Controlling struct {
	// CostCenterCategories are the categories a cost center may have (SAP
	// KOSAR): what may be posted to it and the locks a new one starts with.
	CostCenterCategories []CostCenterCategory `yaml:"cost_center_categories,omitempty" json:"cost_center_categories,omitempty"`
}

// CostCenterCategory is a kind of cost center, such as administration or
// sales. RevenueAllowed lets revenue lines carry a cost center of the
// category (statistically, T56d); the locks are the defaults a new cost
// center of the category starts with (SAP's category settings).
type CostCenterCategory struct {
	Code              string `yaml:"code" json:"code"`
	Name              string `yaml:"name" json:"name"`
	RevenueAllowed    bool   `yaml:"revenue_allowed,omitempty" json:"revenue_allowed,omitempty"`
	LockActualCost    bool   `yaml:"lock_actual_cost,omitempty" json:"lock_actual_cost,omitempty"`
	LockActualRevenue bool   `yaml:"lock_actual_revenue,omitempty" json:"lock_actual_revenue,omitempty"`
	LockPlan          bool   `yaml:"lock_plan,omitempty" json:"lock_plan,omitempty"`
}

// DefaultAssignment is the cost center lines on an account take when none
// is entered or derived from the source document (SAP OKB9), per company
// and account.
type DefaultAssignment struct {
	Account    string `yaml:"account" json:"account"`
	CostCenter string `yaml:"cost_center" json:"cost_center"`
}

// CostElement is an account's cost element category (SAP KATYP): which cost
// object a line on the account needs (ADR-0050 §5).
type CostElement string

const (
	// CostElementNone needs no cost object: balance sheet accounts, and P&L
	// accounts that are not controlled (income taxes, for instance).
	CostElementNone CostElement = "none"
	// CostElementPrimaryCost (SAP 01) needs a cost center.
	CostElementPrimaryCost CostElement = "primary_cost"
	// CostElementRevenue (SAP 11/12) needs a profit center; a cost center
	// only of a category that allows revenue.
	CostElementRevenue CostElement = "revenue"
	// costElementSecondary is reserved for allocations (T60).
	costElementSecondary CostElement = "secondary"
)

// CostElementOrDefault is the account's cost element, or the default of its
// type when none is given (decision P6): an expense is a primary cost, a
// revenue a revenue element, a balance sheet account none.
func (a Account) CostElementOrDefault() CostElement {
	switch {
	case a.CostElement != "":
		return a.CostElement
	case a.Type == Expense:
		return CostElementPrimaryCost
	case a.Type == Revenue:
		return CostElementRevenue
	}
	return CostElementNone
}

// DefaultAssignment is the company's default assignment of the account, or
// nil.
func (c *Company) DefaultAssignment(account string) *DefaultAssignment {
	for i := range c.DefaultAssignments {
		if c.DefaultAssignments[i].Account == account {
			return &c.DefaultAssignments[i]
		}
	}
	return nil
}

// Category is the tenant's cost center category with the code, or nil.
func (c *Controlling) Category(code string) *CostCenterCategory {
	for i := range c.CostCenterCategories {
		if c.CostCenterCategories[i].Code == code {
			return &c.CostCenterCategories[i]
		}
	}
	return nil
}

// AutomaticAccount is an account the system posts to without a person
// choosing it, with the setting, relative to the company, that names it.
type AutomaticAccount struct {
	Path    string
	Account string
}

// determinationKeys are the account determination's settings by name, in
// the order the document lists them.
func determinationKeys(ad AccountDetermination) [][2]string {
	return [][2]string{{"ap_reconciliation", ad.APReconciliation}, {"input_tax", ad.InputTax}, {"bank", ad.Bank},
		{"fx_gain", ad.FXGain}, {"fx_loss", ad.FXLoss}, {"gr_ir", ad.GRIR}, {"price_difference", ad.PriceDifference},
		{"cash_discount", ad.CashDiscount}, {"down_payment", ad.DownPayment}}
}

// AutomaticAccounts are the accounts the company's automatic postings use
// (ADR-0050 §4): the account determination (payables, payment runs, FX
// differences, GR/IR, price differences, cash discounts, down payments),
// tax codes in the single-rate form and each component's account (T61a: a
// multi-component code posts each tax to its component's account),
// withholding tax codes, the revaluation's accounts, the house banks'
// accounts and the bank statement posting rules. Every check of automatic
// accounts reads this one list, so a new kind of setting is added here.
func (c *Company) AutomaticAccounts() []AutomaticAccount {
	var out []AutomaticAccount
	add := func(path, number string) {
		if number != "" {
			out = append(out, AutomaticAccount{Path: path, Account: number})
		}
	}
	for _, k := range determinationKeys(c.AccountDetermination) {
		add("account_determination."+k[0], k[1])
	}
	for _, t := range c.TaxCodes {
		add("tax_codes["+t.Code+"].account", t.Account)
		for _, part := range t.Components {
			add("tax_codes["+t.Code+"].components["+part.Key+"].account", part.Account)
		}
	}
	for _, w := range c.WithholdingTaxCodes {
		add("withholding_tax_codes["+w.Code+"].account", w.Account)
	}
	for _, f := range c.FXValuation.Accounts {
		fp := "fx_valuation.accounts[" + f.Account + "]"
		add(fp+".adjustment", f.Adjustment)
		add(fp+".unrealised_gain", f.UnrealisedGain)
		add(fp+".unrealised_loss", f.UnrealisedLoss)
	}
	for i, hb := range c.AllHouseBanks() {
		hp := strings.TrimPrefix(houseBankPath("", i, hb), ".")
		add(hp+".gl_account", hb.GLAccount)
		add(hp+".main_gl_account", hb.MainGLAccount)
		add(hp+".incoming_gl_account", hb.IncomingGLAccount)
	}
	for _, r := range c.BankStatements.PostingRules {
		add("bank_statements.posting_rules["+r.TransactionCode+"].account", r.Account)
	}
	return out
}

var (
	categoryRe   = regexp.MustCompile(`^[0-9A-Z_]{1,10}$`)  // ledger.cost_center_period.category
	centerCodeRe = regexp.MustCompile(`^[0-9A-Z_-]{1,10}$`) // ledger.cost_center.code, ledger.profit_center.code
)

// controlling checks the cost center categories: codes as cost centers
// carry them, each once, with a name.
func (v *validator) controlling(c *Config) {
	seen := map[string]bool{}
	for _, cat := range c.Controlling.CostCenterCategories {
		p := "controlling.cost_center_categories[" + cat.Code + "]"
		if !categoryRe.MatchString(cat.Code) || seen[cat.Code] {
			v.err(p, "a category code is 1-10 upper-case letters, digits or underscores, each once")
		}
		seen[cat.Code] = true
		if strings.TrimSpace(cat.Name) == "" {
			v.err(p+".name", "is required")
		}
	}
}

// companyControlling checks a company's controlling rules: cost elements,
// default assignments, profit center accounting, and that every account
// the system posts to automatically that needs a cost center has a
// default one once cost center accounting is on, so that a payment run, a
// revaluation or a statement posting is never refused for a missing cost
// center (ADR-0050 §4).
func (v *validator) companyControlling(p string, co *Company) {
	v.costElements(p, co)
	v.defaultAssignments(p, co)
	switch {
	case co.ProfitCenterAccounting && co.DefaultProfitCenter == "":
		v.err(p+".default_profit_center", "is required with profit_center_accounting: it takes the P&L lines nothing else assigns a profit center")
	case co.DefaultProfitCenter != "" && !co.ProfitCenterAccounting:
		v.err(p+".default_profit_center", "is used only with profit_center_accounting")
	case co.DefaultProfitCenter != "" && !centerCodeRe.MatchString(co.DefaultProfitCenter):
		v.err(p+".default_profit_center", "profit center code %q must be 1-10 upper-case letters, digits, underscores or dashes", co.DefaultProfitCenter)
	}
	if co.CostCenterAccounting {
		v.automaticAssignments(p, co)
	}
}

// costElements checks each account's cost element (CostElementProblem).
func (v *validator) costElements(p string, co *Company) {
	for _, a := range co.ChartOfAccounts {
		if msg := a.CostElementProblem(); msg != "" {
			v.err(fmt.Sprintf("%s.chart_of_accounts[%s].cost_element", p, a.Number), "%s", msg)
		}
	}
}

// CostElementProblem says what is wrong with the account's cost element,
// "" when nothing: it must be a known one, a primary cost only on an
// expense and a revenue element only on a revenue, so a balance sheet
// account has none. Country packs (ADR-0048) check their accounts with it.
func (a Account) CostElementProblem() string {
	switch a.CostElement {
	case "", CostElementNone:
	case CostElementPrimaryCost:
		if a.Type != Expense {
			return fmt.Sprintf("account %s is %s; only an expense account carries primary costs", a.Number, a.Type)
		}
	case CostElementRevenue:
		if a.Type != Revenue {
			return fmt.Sprintf("account %s is %s; only a revenue account is a revenue element", a.Number, a.Type)
		}
	case costElementSecondary:
		return "secondary cost elements come with allocations (T60)"
	default:
		return fmt.Sprintf("cost element %q must be none, primary_cost or revenue", a.CostElement)
	}
	return ""
}

// defaultAssignments checks the company's default assignments: one per
// account, of an account of the chart that needs a cost center, to a
// cost center code. That the cost center exists in the company, is active
// and valid is checked against the ledger (ledger.Setup.Check).
func (v *validator) defaultAssignments(p string, co *Company) {
	seen := map[string]bool{}
	for _, d := range co.DefaultAssignments {
		dp := fmt.Sprintf("%s.default_assignments[%s]", p, d.Account)
		if seen[d.Account] {
			v.err(dp, "duplicate default assignment")
		}
		seen[d.Account] = true
		switch a := co.Account(d.Account); {
		case a == nil:
			v.err(dp+".account", "account %s is not in the chart of accounts", d.Account)
		case a.CostElementOrDefault() != CostElementPrimaryCost:
			v.err(dp+".account", "account %s has cost element %s; a default cost center is only for accounts that need one (primary_cost)",
				d.Account, a.CostElementOrDefault())
		}
		if !centerCodeRe.MatchString(d.CostCenter) {
			v.err(dp+".cost_center", "cost center code %q must be 1-10 upper-case letters, digits, underscores or dashes", d.CostCenter)
		}
	}
}

// automaticAssignments refuses an account the system posts to
// automatically that needs a cost center and has no default one: one
// finding per account, naming every setting that posts to it.
func (v *validator) automaticAssignments(p string, co *Company) {
	uses := map[string][]string{}
	var order []string
	for _, a := range co.AutomaticAccounts() {
		acc := co.Account(a.Account)
		if acc == nil || acc.CostElementOrDefault() != CostElementPrimaryCost || co.DefaultAssignment(a.Account) != nil {
			continue // an account not in the chart has its own finding
		}
		if !slices.Contains(order, a.Account) {
			order = append(order, a.Account)
		}
		uses[a.Account] = append(uses[a.Account], a.Path)
	}
	for _, number := range order {
		v.err(fmt.Sprintf("%s.default_assignments[%s]", p, number),
			"account %s needs a cost center (cost_element primary_cost) and the system posts to it automatically (%s); "+
				"add a default assignment of a cost center for it, or set its cost_element to none", number, strings.Join(uses[number], ", "))
	}
}
