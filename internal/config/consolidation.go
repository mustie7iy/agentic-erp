package config

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// Consolidation is the tenant's group reporting (ADR-0059 §3): the group
// chart of accounts, the eliminations of intercompany balances and the
// consolidation groups. Each company's accounts map to the group chart
// (Account.GroupAccount). It is configuration, not master data: short,
// rarely changed, and it decides what the consolidated statements say.
type Consolidation struct {
	// RateType is the rate type of a run's closing rate (ADR-0059 §7);
	// empty: average, as ADR-0042's valuation (RateTypeOrAverage).
	RateType     string               `yaml:"rate_type,omitempty" json:"rate_type,omitempty"`
	Accounts     []GroupAccount       `yaml:"accounts,omitempty" json:"accounts,omitempty"`
	Eliminations []Elimination        `yaml:"eliminations,omitempty" json:"eliminations,omitempty"`
	Groups       []ConsolidationGroup `yaml:"groups,omitempty" json:"groups,omitempty"`
}

// GroupAccount is an account of the group chart (SAP's financial
// statement item): its number, name and type, and the role a run gives it,
// if any (GroupRoles).
type GroupAccount struct {
	Number string      `yaml:"number" json:"number"`
	Name   string      `yaml:"name" json:"name"`
	Type   AccountType `yaml:"type" json:"type"`
	Role   string      `yaml:"role,omitempty" json:"role,omitempty"`
}

// Elimination is a set of group accounts whose intercompany balances a run
// matches between members and eliminates (ADR-0059 §8), with the account
// a difference goes to and the tolerance, in the group currency, within
// which a difference needs no person.
type Elimination struct {
	Code              string   `yaml:"code" json:"code"`
	Name              string   `yaml:"name" json:"name"`
	Accounts          []string `yaml:"accounts" json:"accounts"`
	DifferenceAccount string   `yaml:"difference_account" json:"difference_account"`
	Tolerance         string   `yaml:"tolerance" json:"tolerance"`
}

// ConsolidationGroup is a parent company and the subsidiaries it holds
// directly (ADR-0059 §3; multi-level holdings are out of scope, §1).
type ConsolidationGroup struct {
	Code    string        `yaml:"code" json:"code"`
	Name    string        `yaml:"name" json:"name"`
	Parent  string        `yaml:"parent" json:"parent"`
	Members []GroupMember `yaml:"members" json:"members"`
}

// GroupMember is a subsidiary of a group: its company, the parent's
// ownership in percent, how the parent came to hold it (Origin), the
// fiscal year and regular period from whose first day it is consolidated
// (YYYY-PP), and the parent's account holding the investment in it.
type GroupMember struct {
	Company           string `yaml:"company" json:"company"`
	OwnershipPercent  string `yaml:"ownership_percent" json:"ownership_percent"`
	Origin            string `yaml:"origin" json:"origin"`
	FirstPeriod       string `yaml:"first_period" json:"first_period"`
	InvestmentAccount string `yaml:"investment_account" json:"investment_account"`
}

// How the parent came to hold a member (ADR-0059 As built T68h; the
// accounting specialist's B5, po:2026-11-09): incorporated by it, so IFRS
// 3 does not apply and the investment is eliminated against the member's
// share capital (As built T68h2), or acquired, a business combination
// with goodwill and fair values. A run consolidates only a member the
// parent incorporated and holds whole; the others wait for the steps that
// compute goodwill and the non-controlling interest.
const (
	OriginIncorporated = "incorporated"
	OriginAcquired     = "acquired"
)

// WhollyOwned reports whether the parent holds all of the member.
func (m GroupMember) WhollyOwned() bool {
	o, err := money.ParseDecimal(m.OwnershipPercent, ownershipDecimals)
	return err == nil && o == ownershipMax
}

// The roles of group accounts (ADR-0059 §3): the currency translation
// reserve, retained earnings (prior years' results), non-controlling
// interests and goodwill, which a run computes; no company account maps to
// one.
const (
	RoleCTA              = "cta"
	RoleRetainedEarnings = "retained_earnings"
	RoleNCI              = "nci"
	RoleGoodwill         = "goodwill"
)

// GroupRoles are the roles with the account type each needs, in the order
// the ADR lists them.
var GroupRoles = []struct {
	Role string
	Type AccountType
}{{RoleCTA, Equity}, {RoleRetainedEarnings, Equity}, {RoleNCI, Equity}, {RoleGoodwill, Asset}}

// Consolidation bounds (ADR-0059 §3).
const (
	maxGroupAccounts = 5000
	maxEliminations  = 50
	maxGroups        = 50
	// ownership in ten-thousandths of a percent: above 0, at most 100.
	ownershipDecimals = 4
	ownershipMax      = 1_000_000
)

var (
	groupAccountRe      = regexp.MustCompile(`^[0-9A-Z]{1,10}$`)
	consolidationCodeRe = regexp.MustCompile(`^[A-Z0-9_]{1,30}$`)
	firstPeriodRe       = regexp.MustCompile(`^([0-9]{4})-([0-9]{2})$`)
)

// RateTypeOrAverage is the closing rate's type, average when none is
// given.
func (c Consolidation) RateTypeOrAverage() string {
	if c.RateType == "" {
		return "average"
	}
	return c.RateType
}

// GroupAccount is the group chart's account with the number, or nil.
func (c Consolidation) GroupAccount(number string) *GroupAccount {
	i := slices.IndexFunc(c.Accounts, func(a GroupAccount) bool { return a.Number == number })
	if i < 0 {
		return nil
	}
	return &c.Accounts[i]
}

// EliminationOf is the code of the elimination naming the group account,
// "" when none does.
func (c Consolidation) EliminationOf(number string) string {
	for _, e := range c.Eliminations {
		if slices.Contains(e.Accounts, number) {
			return e.Code
		}
	}
	return ""
}

// Group is the group with the code, or nil.
func (c Consolidation) Group(code string) *ConsolidationGroup {
	i := slices.IndexFunc(c.Groups, func(g ConsolidationGroup) bool { return g.Code == code })
	if i < 0 {
		return nil
	}
	return &c.Groups[i]
}

// GroupOf is the code of the group a company is the parent or a member
// of, "" when it is in none.
func (c Consolidation) GroupOf(company string) string {
	for _, g := range c.Groups {
		if g.Parent == company || slices.ContainsFunc(g.Members, func(m GroupMember) bool { return m.Company == company }) {
			return g.Code
		}
	}
	return ""
}

// First is the member's first fiscal year and period, ok false when
// FirstPeriod is not YYYY-PP with a year of 1900-2999 and a regular period
// (1-12).
func (m GroupMember) First() (year, period int, ok bool) {
	parts := firstPeriodRe.FindStringSubmatch(m.FirstPeriod)
	if parts == nil {
		return 0, 0, false
	}
	year, _ = strconv.Atoi(parts[1])   // four digits: always a number
	period, _ = strconv.Atoi(parts[2]) // two digits: always a number
	return year, period, year >= 1900 && year <= 2999 && period >= 1 && period <= 12
}

// consolidation checks the group reporting settings (ADR-0059 §3): the
// group chart, the eliminations, the groups, and each company's mapping to
// the group chart.
func (v *validator) consolidation(c *Config) {
	cs := c.Consolidation
	if !slices.Contains(rateTypes, cs.RateTypeOrAverage()) {
		v.err("consolidation.rate_type", "rate type %q must be average, bank_buying, bank_selling or planning", cs.RateType)
	}
	v.groupChart(cs)
	v.eliminations(cs)
	v.groups(c)
	for i := range c.Companies {
		v.groupMapping(c, &c.Companies[i])
	}
}

// groupChart checks the group chart: at most 5,000 accounts, each with a
// unique number of up to ten upper-case letters or digits, a name and a
// type; each role at most once, on an account of the type it needs, and
// every role present once a group exists, since a run posts to them.
func (v *validator) groupChart(cs Consolidation) {
	if len(cs.Accounts) > maxGroupAccounts {
		v.err("consolidation.accounts", "at most %d group accounts", maxGroupAccounts)
	}
	seen := map[string]bool{}
	roles := map[string]string{}
	for _, a := range cs.Accounts {
		p := "consolidation.accounts[" + a.Number + "]"
		switch {
		case !groupAccountRe.MatchString(a.Number):
			v.err(p+".number", "a group account's number is 1-10 upper-case letters or digits")
		case seen[a.Number]:
			v.err(p, "duplicate group account")
		}
		seen[a.Number] = true
		if strings.TrimSpace(a.Name) == "" {
			v.err(p+".name", "name is required")
		}
		if !slices.Contains([]AccountType{Asset, Liability, Equity, Revenue, Expense}, a.Type) {
			v.err(p+".type", "type %q must be asset, liability, equity, revenue or expense", a.Type)
		}
		v.groupRole(p, a, roles)
	}
	if len(cs.Groups) == 0 {
		return
	}
	for _, r := range GroupRoles {
		if roles[r.Role] == "" {
			v.err("consolidation.accounts", "a group needs an account with the role %s (a group account of type %s)", r.Role, r.Type)
		}
	}
}

// groupRole checks an account's role: known, on an account of its type,
// and not already another account's; roles records each role's account.
func (v *validator) groupRole(p string, a GroupAccount, roles map[string]string) {
	if a.Role == "" {
		return
	}
	i := slices.IndexFunc(GroupRoles, func(r struct {
		Role string
		Type AccountType
	}) bool {
		return r.Role == a.Role
	})
	switch {
	case i < 0:
		v.err(p+".role", "role %q must be cta, retained_earnings, nci or goodwill", a.Role)
		return
	case GroupRoles[i].Type != a.Type:
		v.err(p+".role", "the role %s is on a group account of type %s", a.Role, GroupRoles[i].Type)
	case roles[a.Role] != "":
		v.err(p+".role", "the role %s is already account %s's", a.Role, roles[a.Role])
		return
	}
	roles[a.Role] = a.Number
}

// eliminations checks the eliminations: at most 50, each with a unique
// code, a name, at least one account of the group chart, no account in two
// eliminations, a difference account of the group chart in none, and a
// tolerance, a plain decimal of at least zero (its decimals against the
// group currency's are checked by the consolidation context's section,
// which knows the tenant's group currency).
func (v *validator) eliminations(cs Consolidation) {
	if len(cs.Eliminations) > maxEliminations {
		v.err("consolidation.eliminations", "at most %d eliminations", maxEliminations)
	}
	seen := map[string]bool{}
	owner := map[string]string{} // group account -> elimination
	for _, e := range cs.Eliminations {
		p := "consolidation.eliminations[" + e.Code + "]"
		switch {
		case !consolidationCodeRe.MatchString(e.Code):
			v.err(p+".code", "an elimination's code is 1-30 upper-case letters, digits or underscores")
		case seen[e.Code]:
			v.err(p, "duplicate elimination")
		}
		seen[e.Code] = true
		if strings.TrimSpace(e.Name) == "" {
			v.err(p+".name", "name is required")
		}
		v.eliminationAccounts(cs, p, e, owner)
		if tol, err := money.ParseDecimalRat(e.Tolerance); err != nil || tol.Sign() < 0 {
			v.err(p+".tolerance", "a plain decimal of at least zero, in the group currency")
		}
	}
	for _, e := range cs.Eliminations {
		p := "consolidation.eliminations[" + e.Code + "].difference_account"
		switch {
		case cs.GroupAccount(e.DifferenceAccount) == nil:
			v.err(p, "group account %q is not in the group chart", e.DifferenceAccount)
		case owner[e.DifferenceAccount] != "":
			v.err(p, "group account %s is an account of elimination %s; a difference goes to an account outside every elimination",
				e.DifferenceAccount, owner[e.DifferenceAccount])
		}
	}
}

// eliminationAccounts checks an elimination's accounts: at least one, each
// of the group chart and in no other elimination, nor named twice; owner
// records each account's elimination.
func (v *validator) eliminationAccounts(cs Consolidation, p string, e Elimination, owner map[string]string) {
	if len(e.Accounts) == 0 {
		v.err(p+".accounts", "at least one group account")
	}
	for _, n := range e.Accounts {
		switch {
		case cs.GroupAccount(n) == nil:
			v.err(p+".accounts", "group account %q is not in the group chart", n)
		case owner[n] == e.Code:
			v.err(p+".accounts", "group account %s is named twice", n)
		case owner[n] != "":
			v.err(p+".accounts", "group account %s is already in elimination %s", n, owner[n])
		default:
			owner[n] = e.Code
		}
	}
}

// groups checks the groups: at most 50, each with a unique code, a name, a
// parent of the document and at least one member; a company in one group
// at most, as parent or member.
func (v *validator) groups(c *Config) {
	cs := c.Consolidation
	if len(cs.Groups) > maxGroups {
		v.err("consolidation.groups", "at most %d groups", maxGroups)
	}
	seen := map[string]bool{}
	in := map[string]string{} // company -> group
	for _, g := range cs.Groups {
		p := "consolidation.groups[" + g.Code + "]"
		switch {
		case !consolidationCodeRe.MatchString(g.Code):
			v.err(p+".code", "a group's code is 1-30 upper-case letters, digits or underscores")
		case seen[g.Code]:
			v.err(p, "duplicate group")
		}
		seen[g.Code] = true
		if strings.TrimSpace(g.Name) == "" {
			v.err(p+".name", "name is required")
		}
		v.groupCompany(c, p+".parent", g.Code, g.Parent, in)
		if len(g.Members) == 0 {
			v.err(p+".members", "at least one member")
		}
		investments := map[string]string{} // investment account -> member
		for _, m := range g.Members {
			v.groupMember(c, p, g, m, in, investments)
		}
	}
}

// groupCompany checks a company a group names, as parent or member: a
// company of the document, in no other group nor twice in this one; in
// records each company's group.
func (v *validator) groupCompany(c *Config, p, group, company string, in map[string]string) {
	switch {
	case c.Company(company) == nil:
		v.err(p, "company %q is not in the document", company)
	case in[company] == group:
		v.err(p, "company %s is already in the group", company)
	case in[company] != "":
		v.err(p, "company %s is already in group %s; a company is in one group at most", company, in[company])
	default:
		in[company] = group
	}
}

// groupMember checks one member: its company (groupCompany), its ownership
// above 0 and at most 100 percent with up to four decimals, its origin,
// its first period, and its investment account (memberInvestment).
func (v *validator) groupMember(c *Config, gp string, g ConsolidationGroup, m GroupMember, in, investments map[string]string) {
	p := gp + ".members[" + m.Company + "]"
	v.groupCompany(c, p+".company", g.Code, m.Company, in)
	if o, err := money.ParseDecimal(m.OwnershipPercent, ownershipDecimals); err != nil || o <= 0 || o > ownershipMax {
		v.err(p+".ownership_percent", "a plain decimal above 0 and at most 100, with up to four decimals")
	}
	if m.Origin != OriginIncorporated && m.Origin != OriginAcquired {
		v.err(p+".origin", "%q is not how the parent came to hold the member: incorporated or acquired", m.Origin)
	}
	if _, _, ok := m.First(); !ok {
		v.err(p+".first_period", "%q is not a fiscal year and regular period (YYYY-PP, period 01-12)", m.FirstPeriod)
	}
	v.memberInvestment(c, p+".investment_account", g, m, investments)
}

// memberInvestment checks a member's investment account (ADR-0059 §3,
// §9): an asset account of the parent's chart, mapped to a group account
// in no elimination, and named by no other member, since each member's
// investment is that account's whole balance.
func (v *validator) memberInvestment(c *Config, p string, g ConsolidationGroup, m GroupMember, investments map[string]string) {
	parent := c.Company(g.Parent)
	if parent == nil { // the parent's own finding reports it
		return
	}
	a := parent.Account(m.InvestmentAccount)
	switch {
	case a == nil:
		v.err(p, "account %q is not in the chart of accounts of the parent %s", m.InvestmentAccount, g.Parent)
	case a.Type != Asset:
		v.err(p, "account %s is %s; an investment is an asset", a.Number, a.Type)
	case c.Consolidation.EliminationOf(a.GroupAccount) != "":
		v.err(p, "account %s maps to group account %s, an account of elimination %s; an investment maps outside every elimination",
			a.Number, a.GroupAccount, c.Consolidation.EliminationOf(a.GroupAccount))
	case investments[a.Number] != "":
		v.err(p, "account %s is already member %s's investment; each member's investment is an account of its own", a.Number, investments[a.Number])
	default:
		investments[a.Number] = m.Company
	}
}

// groupMapping checks a company's mapping to the group chart: every
// account of a company in a group names a group account, and every
// account naming one names an account of the group chart of a compatible
// type (balance sheet to balance sheet, profit and loss to profit and
// loss) and without a role, which only a run posts to.
func (v *validator) groupMapping(c *Config, co *Company) {
	inGroup := c.Consolidation.GroupOf(co.Code) != ""
	for _, a := range co.ChartOfAccounts {
		p := fmt.Sprintf("companies[%s].chart_of_accounts[%s].group_account", co.Code, a.Number)
		if a.GroupAccount == "" {
			if inGroup {
				v.err(p, "company %s is in group %s, so each of its accounts names its group account", co.Code, c.Consolidation.GroupOf(co.Code))
			}
			continue
		}
		g := c.Consolidation.GroupAccount(a.GroupAccount)
		switch {
		case g == nil:
			v.err(p, "group account %q is not in the group chart", a.GroupAccount)
		case g.Role != "":
			v.err(p, "group account %s has the role %s, which only a consolidation run posts to", g.Number, g.Role)
		case g.Type.BalanceSheet() != a.Type.BalanceSheet():
			v.err(p, "account %s is %s and group account %s is %s: a balance sheet account maps to a balance sheet group account, "+
				"a profit and loss account to a profit and loss one", a.Number, a.Type, g.Number, g.Type)
		}
	}
}
