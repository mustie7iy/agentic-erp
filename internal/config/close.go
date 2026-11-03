package config

import (
	"regexp"
	"slices"
	"strings"
	"time"
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
// revaluation posted at the period's last day.
var CloseChecks = []string{"no_parked_entries", "revaluation_posted"}

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
