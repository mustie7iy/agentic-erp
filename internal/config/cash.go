package config

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Cash is the tenant's cash management (ADR-0051 §4): its liquidity items,
// its suppliers' planning groups and the rules that give a flow its item.
// The cash context keeps the items and groups (cash.Setup); the rules and
// the companies' settings are read from the active document.
type Cash struct {
	LiquidityItems []LiquidityItem `yaml:"liquidity_items,omitempty" json:"liquidity_items,omitempty"`
	PlanningGroups []PlanningGroup `yaml:"planning_groups,omitempty" json:"planning_groups,omitempty"`
	Derivation     Derivation      `yaml:"derivation,omitempty" json:"derivation,omitzero"`
}

// LiquidityItem classifies cash by purpose (SAP liquidity item): where it
// comes from or goes to, in one tree of at most MaxLiquidityDepth levels.
// Flows are assigned leaves; a node sums its children.
type LiquidityItem struct {
	Code      string `yaml:"code" json:"code"`
	Name      string `yaml:"name" json:"name"`
	Direction string `yaml:"direction" json:"direction"` // DirectionInflow, DirectionOutflow or DirectionBoth
	Parent    string `yaml:"parent,omitempty" json:"parent,omitempty"`
}

// Directions of liquidity items and flows.
const (
	DirectionInflow  = "inflow"
	DirectionOutflow = "outflow"
	DirectionBoth    = "both"
)

// MaxLiquidityDepth is the deepest a liquidity item may sit, its top node
// at level 1 (ADR-0051 §4).
const MaxLiquidityDepth = 4

// PlanningGroup classifies suppliers by how they are paid (SAP planning
// group, LFB1-FDGRV): a supplier's company data names one (partner).
type PlanningGroup struct {
	Code string `yaml:"code" json:"code"`
	Name string `yaml:"name" json:"name"`
}

// Derivation gives a flow its liquidity item: the first account rule
// covering each offsetting line of its document, the amount split over
// the lines' items in proportion; what no account rule covers takes the
// supplier's planning group's item, then the posting rule's, then the
// default of its direction (SAP query sequence; ADR-0051 §4).
type Derivation struct {
	Accounts       []AccountDerivation       `yaml:"accounts,omitempty" json:"accounts,omitempty"` // in order: the first covering rule wins
	PlanningGroups []PlanningGroupDerivation `yaml:"planning_groups,omitempty" json:"planning_groups,omitempty"`
	PostingRules   []PostingRuleDerivation   `yaml:"posting_rules,omitempty" json:"posting_rules,omitempty"`
	Defaults       DerivationDefaults        `yaml:"defaults,omitempty" json:"defaults,omitzero"`
}

// AccountDerivation assigns the accounts From to To, inclusive, an item.
type AccountDerivation struct {
	From string `yaml:"from" json:"from"`
	To   string `yaml:"to" json:"to"`
	Item string `yaml:"item" json:"item"`
}

// Covers reports whether the account number lies in the rule's range.
// Numbers compare as SAP's ALPHA conversion keeps them: all-digit numbers
// padded with leading zeros to ten places, so 6100 lies between 1500 and
// 15990, and other numbers as they are.
func (a AccountDerivation) Covers(number string) bool {
	n := alpha(number)
	return alpha(a.From) <= n && n <= alpha(a.To)
}

var digitsRe = regexp.MustCompile(`^[0-9]+$`)

// alpha is an account number as SAP's ALPHA conversion stores it.
func alpha(number string) string {
	if digitsRe.MatchString(number) && len(number) < 10 {
		return strings.Repeat("0", 10-len(number)) + number
	}
	return number
}

// PlanningGroupDerivation assigns the flows of suppliers in a planning
// group an item.
type PlanningGroupDerivation struct {
	Group string `yaml:"group" json:"group"`
	Item  string `yaml:"item" json:"item"`
}

// PostingRuleDerivation assigns statement lines posted by the posting rule
// of a bank transaction code (ADR-0047) an item.
type PostingRuleDerivation struct {
	Rule string `yaml:"rule" json:"rule"`
	Item string `yaml:"item" json:"item"`
}

// DerivationDefaults are the items of flows no rule assigns, by direction
// (SAP's default liquidity items).
type DerivationDefaults struct {
	Outflow string `yaml:"outflow,omitempty" json:"outflow,omitempty"`
	Inflow  string `yaml:"inflow,omitempty" json:"inflow,omitempty"`
}

// CompanyCash is how a company's cash is planned (ADR-0051 §5, §8-§10).
// Every setting has a default; an empty one is the default.
type CompanyCash struct {
	// TakeDiscounts expects a supplier item on the last day of its earliest
	// open discount tier, as payment runs pay it; default true.
	TakeDiscounts *bool `yaml:"take_discounts,omitempty" json:"take_discounts,omitempty"`
	// PaymentDays are the weekdays the company runs payments (mon ... sun);
	// none: any day.
	PaymentDays []string `yaml:"payment_days,omitempty" json:"payment_days,omitempty"`
	// ValueDays are the days a house bank's payments take to leave the
	// account, by method or for every method; default 0.
	ValueDays []ValueDays `yaml:"value_days,omitempty" json:"value_days,omitempty"`
	// Buckets are the forecast's columns, consecutive from the read date;
	// default DefaultBuckets.
	Buckets []Bucket `yaml:"buckets,omitempty" json:"buckets,omitempty"`
	// RateType converts other currencies for totals; default planning.
	RateType string `yaml:"rate_type,omitempty" json:"rate_type,omitempty"`
	// RateMaxAgeDays flags a rate older than this; default 7.
	RateMaxAgeDays *int `yaml:"rate_max_age_days,omitempty" json:"rate_max_age_days,omitempty"`
	// OrderLeadDays is how long after its order date an order item without
	// a delivery date is expected to be invoiced; default 30.
	OrderLeadDays *int `yaml:"order_lead_days,omitempty" json:"order_lead_days,omitempty"`
	// CreditLines are the house banks' credit lines, shown as available
	// liquidity, never enforced.
	CreditLines []CreditLine `yaml:"credit_lines,omitempty" json:"credit_lines,omitempty"`
	// SnapshotTime is when the daily forecast snapshot is taken, the
	// company's local time (HH:MM); default 07:00.
	SnapshotTime string `yaml:"snapshot_time,omitempty" json:"snapshot_time,omitempty"`
}

// ValueDays is the value days of a house bank's payments by Method, or
// by every method of the bank when Method is empty.
type ValueDays struct {
	HouseBank string `yaml:"house_bank" json:"house_bank"`
	Method    string `yaml:"method,omitempty" json:"method"` // in JSON always, so the diff keys it bank/method
	Days      int    `yaml:"days" json:"days"`
}

// Bucket is Count consecutive periods of one grain.
type Bucket struct {
	Grain string `yaml:"grain" json:"grain"` // GrainDay, GrainWeek or GrainMonth
	Count int    `yaml:"count" json:"count"`
}

// Bucket grains, shortest first.
const (
	GrainDay   = "day"
	GrainWeek  = "week"
	GrainMonth = "month"
)

// grains are the bucket grains in the order a forecast's buckets run, each
// with the most periods of it a forecast may have.
var grains = []grain{{GrainDay, 92}, {GrainWeek, 104}, {GrainMonth, 36}}

type grain struct {
	name string
	max  int
}

// MaxPeriods is the most periods of the grain a forecast may have, false
// for a grain that is not one: the limits of a company's buckets, which a
// forecast asked for one grain over a range keeps too (ADR-0051 §9, T53e4).
func MaxPeriods(name string) (int, bool) {
	i := slices.IndexFunc(grains, func(g grain) bool { return g.name == name })
	if i < 0 {
		return 0, false
	}
	return grains[i].max, true
}

// DefaultBuckets are the "13-week cash flow" with a year's tail: daily for
// two weeks, weekly to week 13, monthly to month 12.
var DefaultBuckets = []Bucket{{GrainDay, 14}, {GrainWeek, 11}, {GrainMonth, 9}}

// CreditLine is a house bank's credit line, an amount in the bank's
// currency.
type CreditLine struct {
	HouseBank string `yaml:"house_bank" json:"house_bank"`
	Amount    string `yaml:"amount" json:"amount"`
}

// Defaults of a company's cash settings (ADR-0051).
const (
	DefaultCashRateType   = "planning"
	DefaultRateMaxAgeDays = 7
	DefaultOrderLeadDays  = 30
	DefaultSnapshotTime   = "07:00"
)

// TakesDiscounts reports whether the company takes cash discounts.
func (c CompanyCash) TakesDiscounts() bool { return c.TakeDiscounts == nil || *c.TakeDiscounts }

// RateTypeOrPlanning is the rate type totals convert at.
func (c CompanyCash) RateTypeOrPlanning() string { return orDefault(c.RateType, DefaultCashRateType) }

// RateMaxAge is the age in days beyond which a rate is flagged.
func (c CompanyCash) RateMaxAge() int { return intOr(c.RateMaxAgeDays, DefaultRateMaxAgeDays) }

// OrderLead is the days from an order's date to its expected invoice.
func (c CompanyCash) OrderLead() int { return intOr(c.OrderLeadDays, DefaultOrderLeadDays) }

// SnapshotAt is the local time the daily snapshot is taken (HH:MM).
func (c CompanyCash) SnapshotAt() string { return orDefault(c.SnapshotTime, DefaultSnapshotTime) }

// BucketsOrDefault are the forecast's buckets.
func (c CompanyCash) BucketsOrDefault() []Bucket {
	if len(c.Buckets) == 0 {
		return slices.Clone(DefaultBuckets)
	}
	return slices.Clone(c.Buckets)
}

// ValueDaysFor is the value days of a payment from the house bank by the
// method: the method's own, else the bank's for every method, else 0.
func (c CompanyCash) ValueDaysFor(houseBank, method string) int {
	days := 0
	for _, v := range c.ValueDays {
		switch {
		case v.HouseBank != houseBank:
		case v.Method == method:
			return v.Days
		case v.Method == "":
			days = v.Days
		}
	}
	return days
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func intOr(n *int, def int) int {
	if n == nil {
		return def
	}
	return *n
}

// weekdays are the payment days a company may name, Monday first.
var weekdays = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// cashCodeRe is a liquidity item's or planning group's code.
var cashCodeRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,29}$`)

// ValidCashCode reports whether code is a liquidity item's or planning
// group's code: an upper-case letter, then up to 29 upper-case letters,
// digits or underscores.
func ValidCashCode(code string) bool { return cashCodeRe.MatchString(code) }

// cash checks the tenant's cash management (ADR-0051 §4): a tree of
// liquidity items, planning groups each once, and rules naming them.
func (v *validator) cash(c *Config) {
	items := v.liquidityItems("cash.liquidity_items", c.Cash.LiquidityItems)
	groups := map[string]bool{}
	for _, g := range c.Cash.PlanningGroups {
		gp := "cash.planning_groups[" + g.Code + "]"
		if !ValidCashCode(g.Code) || groups[g.Code] {
			v.err(gp+".code", "a code of upper-case letters, digits and underscores, starting with a letter, up to 30, each once")
		}
		groups[g.Code] = true
		if strings.TrimSpace(g.Name) == "" {
			v.err(gp+".name", "name is required")
		}
	}
	v.derivation(c, items, groups)
}

// CheckLiquidityItems reports what is wrong with a tree of liquidity
// items at path: what config.Validate checks of them, for a country pack
// that brings items (countrypack).
func CheckLiquidityItems(path string, items []LiquidityItem) []Finding {
	v := &validator{}
	v.liquidityItems(path, items)
	return v.out
}

// liquidityItems checks each item's code, name and direction, and that its
// parent is another item of a direction it fits, at most
// MaxLiquidityDepth levels deep and in no cycle. It returns the items by
// code.
func (v *validator) liquidityItems(path string, items []LiquidityItem) map[string]LiquidityItem {
	byCode := map[string]LiquidityItem{}
	for _, it := range items {
		ip := path + "[" + it.Code + "]"
		if _, dup := byCode[it.Code]; dup || !ValidCashCode(it.Code) {
			v.err(ip+".code", "a code of upper-case letters, digits and underscores, starting with a letter, up to 30, each once")
		}
		byCode[it.Code] = it
		if strings.TrimSpace(it.Name) == "" {
			v.err(ip+".name", "name is required")
		}
		if !slices.Contains([]string{DirectionInflow, DirectionOutflow, DirectionBoth}, it.Direction) {
			v.err(ip+".direction", "must be inflow, outflow or both")
		}
	}
	for _, it := range items {
		if it.Parent != "" {
			v.liquidityParent(path+"["+it.Code+"].parent", it, byCode)
		}
	}
	return byCode
}

// liquidityParent checks an item's parent: an item, of the item's
// direction or both, and the chain above it ending within the depth.
func (v *validator) liquidityParent(pp string, it LiquidityItem, byCode map[string]LiquidityItem) {
	parent, ok := byCode[it.Parent]
	switch {
	case !ok:
		v.err(pp, "parent %q is not a liquidity item", it.Parent)
		return
	case parent.Direction != DirectionBoth && parent.Direction != it.Direction:
		v.err(pp, "parent %s is for %s, so its children are too", parent.Code, parent.Direction)
	}
	seen := map[string]bool{it.Code: true}
	depth := 1
	for at := it; at.Parent != ""; at = byCode[at.Parent] {
		if seen[at.Parent] {
			v.err(pp, "the items above %s form a cycle", it.Code)
			return
		}
		if _, ok := byCode[at.Parent]; !ok {
			return // reported at that item
		}
		seen[at.Parent] = true
		depth++
	}
	if depth > MaxLiquidityDepth {
		v.err(pp, "item %s is at level %d; liquidity items are at most %d levels deep", it.Code, depth, MaxLiquidityDepth)
	}
}

// derivation checks the rules giving flows their items (ADR-0051 §4):
// each names a leaf item, a planning group's one for outflows or both; account rules valid ranges, planning group
// rules groups of the document and posting rule rules the transaction
// code of a company's posting rule, each once; the defaults items of
// their direction. Without defaults a flow no rule assigns has no item.
func (v *validator) derivation(c *Config, items map[string]LiquidityItem, groups map[string]bool) {
	d := c.Cash.Derivation
	leaf := v.leafCheck(items)
	for i, r := range d.Accounts {
		rp := fmt.Sprintf("cash.derivation.accounts[%d]", i)
		v.accountRule(rp, c, r)
		leaf(rp+".item", r.Item, "")
	}
	seen := map[string]bool{}
	for _, r := range d.PlanningGroups {
		rp := "cash.derivation.planning_groups[" + r.Group + "]"
		if !groups[r.Group] || seen[r.Group] {
			v.err(rp+".group", "a planning group of cash.planning_groups, each once")
		}
		seen[r.Group] = true
		leaf(rp+".item", r.Item, DirectionOutflow) // a supplier's flows are payments
	}
	clear(seen)
	for _, r := range d.PostingRules {
		rp := "cash.derivation.posting_rules[" + r.Rule + "]"
		if !hasPostingRule(c, r.Rule) || seen[r.Rule] {
			v.err(rp+".rule", "the transaction code of a company's bank_statements.posting_rules, each once")
		}
		seen[r.Rule] = true
		leaf(rp+".item", r.Item, "")
	}
	for _, def := range []struct{ direction, item string }{{DirectionOutflow, d.Defaults.Outflow}, {DirectionInflow, d.Defaults.Inflow}} {
		dp := "cash.derivation.defaults." + def.direction
		switch {
		case def.item != "":
			leaf(dp, def.item, def.direction)
		case len(items) > 0:
			v.warn(dp, "no default %s item: a flow no rule assigns has no liquidity item", def.direction)
		}
	}
}

// leafCheck returns a check that an item named at a path is a liquidity
// item without children and, when direction is given, of it or both.
func (v *validator) leafCheck(items map[string]LiquidityItem) func(path, code, direction string) {
	parents := map[string]bool{}
	for _, it := range items {
		parents[it.Parent] = true
	}
	return func(path, code, direction string) {
		it, ok := items[code]
		switch {
		case !ok:
			v.err(path, "%q is not a liquidity item", code)
		case parents[code]:
			v.err(path, "item %s has children; flows are assigned items without children", code)
		case direction != "" && it.Direction != direction && it.Direction != DirectionBoth:
			v.err(path, "item %s is for %s, not %s", code, it.Direction, direction)
		}
	}
}

// accountRule checks an account rule's range: account numbers, From not
// after To, and covering an account of some company's chart.
func (v *validator) accountRule(rp string, c *Config, r AccountDerivation) {
	if !accountRe.MatchString(r.From) || !accountRe.MatchString(r.To) {
		v.err(rp, "from and to must be account numbers")
		return
	}
	if alpha(r.From) > alpha(r.To) {
		v.err(rp+".to", "the range ends before it starts")
		return
	}
	for _, co := range c.Companies {
		for _, a := range co.ChartOfAccounts {
			if r.Covers(a.Number) {
				return
			}
		}
	}
	v.warn(rp, "no company's chart of accounts has an account from %s to %s", r.From, r.To)
}

// hasPostingRule reports whether some company posts statement lines by a
// posting rule of the transaction code.
func hasPostingRule(c *Config, code string) bool {
	return slices.ContainsFunc(c.Companies, func(co Company) bool {
		return slices.ContainsFunc(co.BankStatements.PostingRules, func(r StatementPostingRule) bool { return r.TransactionCode == code })
	})
}

// companyCash checks a company's cash settings: payment days weekdays each
// once; value days and credit lines of its house banks, each once, value
// days by a method of it or none, and credit lines positive amounts of
// the bank's currency; buckets of known grains, shortest first, each grain
// once and bounded; a known rate type; bounded ages and lead days; and a
// snapshot time of day.
func (v *validator) companyCash(p string, co *Company) {
	cp := p + ".cash"
	cc := co.Cash
	seen := map[string]bool{}
	for i, d := range cc.PaymentDays {
		if !slices.Contains(weekdays, d) || seen[d] {
			v.err(fmt.Sprintf("%s.payment_days[%d]", cp, i), "a weekday (mon, tue, wed, thu, fri, sat, sun), each once")
		}
		seen[d] = true
	}
	v.valueDays(cp, co)
	v.creditLines(cp, co)
	v.buckets(cp, cc.Buckets)
	if cc.RateType != "" && !slices.Contains(rateTypes, cc.RateType) {
		v.err(cp+".rate_type", "rate type %q must be average, bank_buying, bank_selling or planning", cc.RateType)
	}
	if cc.RateMaxAgeDays != nil && (*cc.RateMaxAgeDays < 1 || *cc.RateMaxAgeDays > 365) {
		v.err(cp+".rate_max_age_days", "must be 1-365")
	}
	if cc.OrderLeadDays != nil && (*cc.OrderLeadDays < 0 || *cc.OrderLeadDays > 365) {
		v.err(cp+".order_lead_days", "must be 0-365")
	}
	if cc.SnapshotTime != "" {
		if _, err := time.Parse("15:04", cc.SnapshotTime); err != nil {
			v.err(cp+".snapshot_time", "%q is not a time of day (HH:MM)", cc.SnapshotTime)
		}
	}
}

// houseBank is the company's house bank of the name, or nil.
func (c *Company) houseBank(name string) *HouseBank {
	for _, hb := range c.AllHouseBanks() {
		if hb.Name == name {
			return &hb
		}
	}
	return nil
}

func (v *validator) valueDays(cp string, co *Company) {
	seen := map[string]bool{}
	for _, d := range co.Cash.ValueDays {
		key := d.HouseBank + "/" + d.Method
		dp := cp + ".value_days[" + key + "]"
		if co.houseBank(d.HouseBank) == nil || seen[key] {
			v.err(dp+".house_bank", "a house bank of the company, once per method")
		}
		seen[key] = true
		if d.Method != "" && !slices.ContainsFunc(co.PaymentMethods, func(m PaymentMethod) bool { return m.Code == d.Method }) {
			v.err(dp+".method", "method %q is not a payment method of the company", d.Method)
		}
		if d.Days < 0 || d.Days > 30 {
			v.err(dp+".days", "must be 0-30")
		}
	}
}

func (v *validator) creditLines(cp string, co *Company) {
	seen := map[string]bool{}
	for _, l := range co.Cash.CreditLines {
		lp := cp + ".credit_lines[" + l.HouseBank + "]"
		hb := co.houseBank(l.HouseBank)
		if hb == nil || seen[l.HouseBank] {
			v.err(lp+".house_bank", "a house bank of the company, each once")
			continue
		}
		seen[l.HouseBank] = true
		exp, known := v.exponents[co.HouseBankCurrency(*hb)]
		if !known { // the bank's or company's currency has its own finding
			continue
		}
		if m, ok := v.amount(lp+".amount", l.Amount, exp); ok && m == 0 {
			v.err(lp+".amount", "must be positive")
		}
	}
}

func (v *validator) buckets(cp string, buckets []Bucket) {
	next := 0 // the first grain the next bucket may have
	for i, b := range buckets {
		bp := fmt.Sprintf("%s.buckets[%d]", cp, i)
		g := slices.IndexFunc(grains, func(g grain) bool { return g.name == b.Grain })
		switch {
		case g < 0:
			v.err(bp+".grain", "must be day, week or month")
			continue
		case g < next:
			v.err(bp+".grain", "buckets run from days to weeks to months, each grain once")
		}
		next = g + 1
		if b.Count < 1 || b.Count > grains[g].max {
			v.err(bp+".count", "must be 1-%d", grains[g].max)
		}
	}
}
