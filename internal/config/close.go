package config

import (
	"math/big"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// Close is the tenant's close template (ADR-0058 §3): the tasks a
// company's period close is given when it is created (T64b), in order.
// It is configuration, not master data: short, rarely changed, and it
// decides what a sign-off requires.
type Close struct {
	Tasks []CloseTask `yaml:"tasks,omitempty" json:"tasks,omitempty"`
}

// CloseTask is one task of the close template: a manual task a person
// completes, or a check the system runs; due DueDay business days after
// the period's last day (0: that day, negative: before it); open until the
// tasks it depends on are done; given to every company's close, or to the
// listed companies' only.
type CloseTask struct {
	Code      string   `yaml:"code" json:"code"`
	Name      string   `yaml:"name" json:"name"`
	Kind      string   `yaml:"kind" json:"kind"`
	Check     string   `yaml:"check,omitempty" json:"check,omitempty"`
	DueDay    int      `yaml:"due_day" json:"due_day"`
	DependsOn []string `yaml:"depends_on,omitempty" json:"depends_on,omitempty"`
	Companies []string `yaml:"companies,omitempty" json:"companies,omitempty"`
}

// Close task kinds (ADR-0058 §3).
const (
	CloseManual = "manual"
	CloseCheck  = "check"
)

// CloseChecks are the checks this build runs (ADR-0058 §6); each slice
// that builds a check adds it, and a template naming another is refused.
// T64c: no parked entry left in the period; each covered ledger's
// revaluation posted at the period's last day. T64i: every variance
// requiring a comment explained by an accepted, current one. T64k: each
// covered ledger's depreciation of the period posted, or nothing to
// depreciate. T64f: no accrual proposal of the close left undecided; every
// accrual posted for the previous period's close reversed.
var CloseChecks = []string{"no_parked_entries", "revaluation_posted", "variances_explained", "depreciation_posted", "accruals_decided", "accruals_reversed"}

// Close template bounds (ADR-0058 §3).
const (
	maxCloseTasks = 200
	maxDueDay     = 31
)

var closeTaskRe = regexp.MustCompile(`^[A-Z0-9_]{1,30}$`)

// For reports whether the company's close is given the task: one that
// lists no companies is every company's.
func (t CloseTask) For(company string) bool {
	return len(t.Companies) == 0 || slices.Contains(t.Companies, company)
}

// CloseTasksOf is the template's tasks a company's close is given, in
// the template's order.
func (c *Config) CloseTasksOf(company string) []CloseTask {
	var out []CloseTask
	for _, t := range c.Close.Tasks {
		if t.For(company) {
			out = append(out, t)
		}
	}
	return out
}

// weekendDays are the names a weekend gives its days by (ADR-0058 §3),
// written out, as the ADR's example has them.
var weekendDays = map[string]time.Weekday{
	"sunday": time.Sunday, "monday": time.Monday, "tuesday": time.Tuesday, "wednesday": time.Wednesday,
	"thursday": time.Thursday, "friday": time.Friday, "saturday": time.Saturday,
}

// DefaultWeekend is a company's weekend when its calendar names none.
var DefaultWeekend = []string{"saturday", "sunday"}

// Weekend is the company's weekend days by name: its calendar's, or
// DefaultWeekend. It returns a copy, so a reader cannot change the active
// document.
func (c *Config) Weekend(company string) []string {
	if cal := c.Calendar(company); cal != nil && len(cal.Weekend) > 0 {
		return slices.Clone(cal.Weekend)
	}
	return slices.Clone(DefaultWeekend)
}

// BusinessDay reports whether the company does business on the day: not
// one of its weekend days, not one of its holidays (ADR-0048, ADR-0058
// §3). The day is d's calendar date in d's own location, so callers pass
// the company's business date, as for Holiday.
func (c *Config) BusinessDay(company string, d time.Time) bool {
	for _, w := range c.Weekend(company) {
		if weekendDays[w] == d.Weekday() {
			return false
		}
	}
	_, holiday := c.Holiday(company, d)
	return !holiday
}

// weekend checks a calendar's weekend: weekday names, each once, and at
// least one business weekday left.
func (v *validator) weekend(p string, days []string) {
	seen := map[string]bool{}
	for _, w := range days {
		switch _, ok := weekendDays[w]; {
		case !ok:
			v.err(p+".weekend", "%q is not a weekday (monday to sunday, in lower case)", w)
		case seen[w]:
			v.err(p+".weekend", "%s is named twice", w)
		default:
			seen[w] = true
		}
	}
	if len(seen) == len(weekendDays) {
		v.err(p+".weekend", "a weekend of every day leaves the company no business day")
	}
}

// closeTemplate checks the close template (ADR-0058 §3): at most 200
// tasks, each with a unique code of up to 30 upper-case letters, digits or
// underscores, a name, a kind, a check this build runs exactly when the
// kind is check, a due day from -31 to 31, companies of the document each
// once, and dependencies on other tasks of the template without a cycle,
// each given to every company the dependent task is given to.
func (v *validator) closeTemplate(c *Config) {
	tasks := c.Close.Tasks
	if len(tasks) > maxCloseTasks {
		v.err("close.tasks", "at most %d tasks", maxCloseTasks)
	}
	byCode := map[string]*CloseTask{}
	for i := range tasks {
		t := &tasks[i]
		p := "close.tasks[" + t.Code + "]"
		switch {
		case !closeTaskRe.MatchString(t.Code):
			v.err(p+".code", "a task's code is 1-30 upper-case letters, digits or underscores")
		case byCode[t.Code] != nil:
			v.err(p, "duplicate task")
		}
		byCode[t.Code] = t
		v.closeTask(c, p, t)
	}
	for i := range tasks {
		v.closeDependencies(byCode, &tasks[i])
	}
	v.closeCycles(tasks, byCode)
}

// closeTask checks one task's own fields.
func (v *validator) closeTask(c *Config, p string, t *CloseTask) {
	if strings.TrimSpace(t.Name) == "" {
		v.err(p+".name", "name is required")
	}
	switch {
	case t.Kind != CloseManual && t.Kind != CloseCheck:
		v.err(p+".kind", "manual or check")
	case t.Kind == CloseManual && t.Check != "":
		v.err(p+".check", "only a check task names a check")
	case t.Kind == CloseCheck && t.Check == "":
		v.err(p+".check", "a check task names its check")
	case t.Kind == CloseCheck && !slices.Contains(CloseChecks, t.Check):
		v.err(p+".check", "%q is not a check this build runs", t.Check)
	}
	if t.DueDay < -maxDueDay || t.DueDay > maxDueDay {
		v.err(p+".due_day", "business days from -%d to %d around the period's last day", maxDueDay, maxDueDay)
	}
	seen := map[string]bool{}
	for _, co := range t.Companies {
		switch {
		case c.Company(co) == nil:
			v.err(p+".companies", "company %q is not in the document", co)
		case seen[co]:
			v.err(p+".companies", "company %s is named twice", co)
		}
		seen[co] = true
	}
}

// closeDependencies checks that a task depends on other tasks of the
// template, each once, and given to every company the task is, so no
// close holds a task whose dependency it lacks.
func (v *validator) closeDependencies(byCode map[string]*CloseTask, t *CloseTask) {
	p := "close.tasks[" + t.Code + "].depends_on"
	seen := map[string]bool{}
	for _, code := range t.DependsOn {
		d := byCode[code]
		switch {
		case d == nil:
			v.err(p, "%q is not a task of the template", code)
		case seen[code]:
			v.err(p, "%s is named twice", code)
		case !covers(d, t):
			v.err(p, "%s is not given to every company %s is given to", code, t.Code)
		}
		seen[code] = true
	}
}

// covers reports whether every company given t is given d.
func covers(d, t *CloseTask) bool {
	if len(d.Companies) == 0 {
		return true
	}
	if len(t.Companies) == 0 {
		return false
	}
	for _, co := range t.Companies {
		if !slices.Contains(d.Companies, co) {
			return false
		}
	}
	return true
}

// closeCycles refuses each task whose dependencies lead back to it (one
// depending on itself included). A template has at most 200 tasks, so a
// search from each is cheap.
func (v *validator) closeCycles(tasks []CloseTask, byCode map[string]*CloseTask) {
	for _, t := range tasks {
		if reaches(byCode, t.DependsOn, t.Code, map[string]bool{}) {
			v.err("close.tasks["+t.Code+"].depends_on", "the dependencies of %s lead back to it", t.Code)
		}
	}
}

// reaches reports whether target is among the tasks from, or the tasks
// they depend on, at any depth.
func reaches(byCode map[string]*CloseTask, from []string, target string, seen map[string]bool) bool {
	for _, code := range from {
		if code == target {
			return true
		}
		d := byCode[code]
		if d == nil || seen[code] {
			continue
		}
		seen[code] = true
		if reaches(byCode, d.DependsOn, target, seen) {
			return true
		}
	}
	return false
}

// CompanyClose is how a company's period close works (ADR-0058 §3): the
// account its accruals are credited to (T64e; empty: none, so no accrual
// is proposed) and the variance report's settings (T64h; nil Variance, no
// basis configured).
type CompanyClose struct {
	AccrualAccount string         `yaml:"accrual_account,omitempty" json:"accrual_account,omitempty"`
	Variance       *CloseVariance `yaml:"variance,omitempty" json:"variance,omitempty"`
}

// CloseVariance is a company's variance report (ADR-0058 §9): the bases it
// compares the period against, and the thresholds from which a row needs a
// comment: its variance at least ThresholdAmount (local currency, a
// decimal string) and, when ThresholdPercent is set and the row has a
// percent, that percent at least it. The plan basis is PlanCategory's
// amounts summed over its cost center lines or its profit center lines
// (PlanCenters), never both, since a category may plan both and summing
// them would count one amount twice.
type CloseVariance struct {
	Bases            []string `yaml:"bases" json:"bases"`
	ThresholdAmount  string   `yaml:"threshold_amount" json:"threshold_amount"`
	ThresholdPercent string   `yaml:"threshold_percent,omitempty" json:"threshold_percent,omitempty"`
	PlanCategory     string   `yaml:"plan_category,omitempty" json:"plan_category,omitempty"`
	PlanCenters      string   `yaml:"plan_centers,omitempty" json:"plan_centers,omitempty"`
}

// Variance bases (ADR-0058 §9) and the plan's kinds of center.
const (
	BasisPriorPeriod  = "prior_period"
	BasisPriorYear    = "prior_year"
	BasisPlan         = "plan"
	PlanCentersCost   = "cost"
	PlanCentersProfit = "profit"
)

// VarianceBases are the bases a variance report compares against.
var VarianceBases = []string{BasisPriorPeriod, BasisPriorYear, BasisPlan}

// planCategoryRe is a plan category's code, as controlling keeps it.
var planCategoryRe = regexp.MustCompile(`^[0-9A-Z_]{1,10}$`)

// Variance is the company's variance setting for the basis, nil when the
// basis is not one it is configured for.
func (co *Company) Variance(basis string) *CloseVariance {
	if v := co.Close.Variance; v != nil && slices.Contains(v.Bases, basis) {
		return v
	}
	return nil
}

// companyClose checks a company's close settings: its accrual account
// (accrualAccount), the variance report's bases known and each once, its
// amount threshold positive in the company's currency, its percent
// threshold, if any, positive with at most two places (the report's
// percents have two), and a plan category and kind of center exactly when
// plan is a basis. Whether the category exists is controlling's, checked
// by the closing context's section.
func (v *validator) companyClose(p string, co *Company, exp int, known bool) {
	v.accrualAccount(p+".close.accrual_account", co)
	cv := co.Close.Variance
	if cv == nil {
		return
	}
	p += ".close.variance"
	if len(cv.Bases) == 0 {
		v.err(p+".bases", "at least one basis: %s", strings.Join(VarianceBases, ", "))
	}
	for i, b := range cv.Bases {
		switch {
		case !slices.Contains(VarianceBases, b):
			v.err(p+".bases", "%q is not a basis (%s)", b, strings.Join(VarianceBases, ", "))
		case slices.Index(cv.Bases, b) < i:
			v.err(p+".bases", "%s is named twice", b)
		}
	}
	if known {
		if m, ok := v.amount(p+".threshold_amount", cv.ThresholdAmount, exp); ok && m == 0 {
			v.err(p+".threshold_amount", "must be more than zero")
		}
	}
	if cv.ThresholdPercent != "" {
		pct, err := money.ParseDecimalRat(cv.ThresholdPercent)
		switch {
		case err != nil:
			v.err(p+".threshold_percent", "%v", err)
		case pct.Sign() <= 0:
			v.err(p+".threshold_percent", "must be more than zero")
		case !new(big.Rat).Mul(pct, big.NewRat(100, 1)).IsInt():
			v.err(p+".threshold_percent", "at most two decimal places, as the report's percents have")
		}
	}
	v.variancePlan(p, cv)
}

// accrualAccount checks the account a company's accruals are credited to
// (ADR-0058 §3): a liability of its chart of accounts, posted by hand, so
// no reconciliation account of any kind (a supplier's, an asset's, the
// down payments' or the GR/IR account, which only their own postings
// reach).
func (v *validator) accrualAccount(ap string, co *Company) {
	number := co.Close.AccrualAccount
	if number == "" {
		return
	}
	a := co.Account(number)
	ad := co.AccountDetermination
	switch {
	case a == nil:
		v.err(ap, "account %s is not in the chart of accounts", number)
	case a.Type != Liability:
		v.err(ap, "account %s is of type %q; accruals are credited to a liability", number, a.Type)
	case a.VendorReconciliation, a.AssetReconciliation, number == ad.GRIR, number == ad.DownPayment:
		v.err(ap, "account %s is a reconciliation account, posted only through its own postings; accruals are credited to one posted by hand", number)
	}
}

// variancePlan checks the plan basis's settings: a category code and cost
// or profit centers with the basis, neither without it.
func (v *validator) variancePlan(p string, cv *CloseVariance) {
	if !slices.Contains(cv.Bases, BasisPlan) {
		if cv.PlanCategory != "" {
			v.err(p+".plan_category", "only with the basis plan")
		}
		if cv.PlanCenters != "" {
			v.err(p+".plan_centers", "only with the basis plan")
		}
		return
	}
	if !planCategoryRe.MatchString(cv.PlanCategory) {
		v.err(p+".plan_category", "the plan basis names a plan category's code (1-10 upper-case letters, digits or underscores)")
	}
	if cv.PlanCenters != PlanCentersCost && cv.PlanCenters != PlanCentersProfit {
		v.err(p+".plan_centers", "cost or profit: which of the category's plan lines the plan basis sums")
	}
}
