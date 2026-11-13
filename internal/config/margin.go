package config

import (
	"slices"
	"strconv"
	"strings"
)

// MarginChart names what the margin report counts as cost of sales on one
// chart of accounts (ADR-0075 §2). The document keeps one chart per
// company, named after the company's code, so Chart is a company's code.
// Revenue needs no setting: it is the chart's accounts of type revenue.
type MarginChart struct {
	Chart string `yaml:"chart" json:"chart"`
	// CostOfSalesAccounts are the expense accounts whose balance is the
	// cost of sales, as single numbers or ranges.
	CostOfSalesAccounts []AccountRange `yaml:"cost_of_sales_accounts" json:"cost_of_sales_accounts"`
}

// AccountRange is one account number (From alone) or the accounts From to
// To, inclusive, compared as AccountDerivation compares them.
type AccountRange struct {
	From string `yaml:"from" json:"from"`
	To   string `yaml:"to,omitempty" json:"to,omitempty"`
}

// Covers reports whether the account number is the range's one account or
// lies in it.
func (r AccountRange) Covers(number string) bool {
	to := r.To
	if to == "" {
		to = r.From
	}
	return AccountDerivation{From: r.From, To: to}.Covers(number)
}

// String is the range as a finding names it.
func (r AccountRange) String() string {
	if r.To == "" {
		return r.From
	}
	return r.From + " to " + r.To
}

// CostOfSalesAccounts are the numbers of the company's accounts the margin
// report counts as cost of sales, in number order, and whether the
// document names any for the company's chart: a chart without an entry
// has none, and the report says so (ADR-0075 §2).
func (c *Config) CostOfSalesAccounts(company string) (numbers []string, named bool) {
	co := c.Company(company)
	i := slices.IndexFunc(c.Controlling.MarginReport, func(m MarginChart) bool { return m.Chart == company })
	if co == nil || i < 0 {
		return []string{}, false
	}
	numbers = []string{}
	for _, a := range co.ChartOfAccounts {
		if slices.ContainsFunc(c.Controlling.MarginReport[i].CostOfSalesAccounts, func(r AccountRange) bool { return r.Covers(a.Number) }) {
			numbers = append(numbers, a.Number)
		}
	}
	slices.SortFunc(numbers, func(a, b string) int { return strings.Compare(alpha(a), alpha(b)) })
	return numbers, true
}

// marginReport checks the margin report's charts: each a company's, once,
// naming at least one account; each range account numbers in order that
// cover an account of the chart; and every account covered an expense,
// since a revenue account would count on both sides of the margin and a
// balance sheet account is no cost.
func (v *validator) marginReport(c *Config) {
	seen := map[string]bool{}
	for _, m := range c.Controlling.MarginReport {
		p := "controlling.margin_report[" + m.Chart + "]"
		co := c.Company(m.Chart)
		if co == nil || seen[m.Chart] {
			v.err(p+".chart", "a chart is a company's code, each once")
			continue
		}
		seen[m.Chart] = true
		if len(m.CostOfSalesAccounts) == 0 {
			v.err(p+".cost_of_sales_accounts", "name at least one account, or leave the chart out")
		}
		for i, r := range m.CostOfSalesAccounts {
			v.costOfSalesRange(p+".cost_of_sales_accounts["+strconv.Itoa(i)+"]", co, r)
		}
	}
}

// costOfSalesRange checks one account or range of a chart's cost of sales.
func (v *validator) costOfSalesRange(p string, co *Company, r AccountRange) {
	if !accountRe.MatchString(r.From) || (r.To != "" && !accountRe.MatchString(r.To)) {
		v.err(p, "from, and to if given, must be account numbers")
		return
	}
	if r.To != "" && alpha(r.From) > alpha(r.To) {
		v.err(p+".to", "the range ends before it starts")
		return
	}
	covered := false
	for _, a := range co.ChartOfAccounts {
		if !r.Covers(a.Number) {
			continue
		}
		covered = true
		if a.Type != Expense {
			v.err(p, "account %s is of type %s; cost of sales is on expense accounts", a.Number, a.Type)
		}
	}
	if !covered {
		v.err(p, "company %s has no account %s", co.Code, r)
	}
}
