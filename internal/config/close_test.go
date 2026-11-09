package config

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// withClose is the fixture with a close template of three tasks: two for
// every company, the second depending on the first, and one for company
// 1000 only depending on both.
func withClose(t *testing.T) *Config {
	t.Helper()
	c := fixture(t)
	c.Close.Tasks = []CloseTask{
		{Code: "AP_CUTOFF", Name: "Supplier invoices of the period entered", Kind: CloseManual, DueDay: 2},
		{Code: "ACCRUALS", Name: "Accruals reviewed", Kind: CloseManual, DueDay: 3, DependsOn: []string{"AP_CUTOFF"}},
		{Code: "REPORT", Name: "Management report", Kind: CloseManual, DueDay: -1, DependsOn: []string{"AP_CUTOFF", "ACCRUALS"}, Companies: []string{"1000"}},
		{Code: "NO_PARKED", Name: "No entry left parked", Kind: CloseCheck, Check: "no_parked_entries", DueDay: 3},
		{Code: "REVALUED", Name: "Foreign currency revalued", Kind: CloseCheck, Check: "revaluation_posted", DueDay: 1},
	}
	return c
}

// TestCloseTemplateValid: the template of ADR-0058 §3 validates, and each
// rule of the section refuses its field.
func TestCloseTemplateValid(t *testing.T) {
	if fs := Validate(withClose(t), currency.Seeded()); HasErrors(fs) {
		t.Fatalf("close template configured: got %v, want no errors", fs)
	}
	task := func(i int, mutate func(*CloseTask)) func(c *Config) {
		return func(c *Config) { mutate(&c.Close.Tasks[i]) }
	}
	cases := []struct {
		name   string
		mutate func(c *Config)
		path   string
	}{
		{"a lower-case code", task(0, func(t *CloseTask) { t.Code = "ap_cutoff" }), "close.tasks[ap_cutoff].code"},
		{"a code above 30 characters", task(0, func(t *CloseTask) { t.Code = strings.Repeat("A", 31) }), "close.tasks[" + strings.Repeat("A", 31) + "].code"},
		{"a task twice", func(c *Config) { c.Close.Tasks = append(c.Close.Tasks, c.Close.Tasks[0]) }, "close.tasks[AP_CUTOFF]"},
		{"no name", task(0, func(t *CloseTask) { t.Name = " " }), "close.tasks[AP_CUTOFF].name"},
		{"an unknown kind", task(0, func(t *CloseTask) { t.Kind = "approval" }), "close.tasks[AP_CUTOFF].kind"},
		{"a check on a manual task", task(0, func(t *CloseTask) { t.Check = "no_parked_entries" }), "close.tasks[AP_CUTOFF].check"},
		{"a check this build does not run", task(0, func(t *CloseTask) { t.Kind, t.Check = CloseCheck, "bank_reconciled" }), "close.tasks[AP_CUTOFF].check"},
		{"a check task without its check", task(0, func(t *CloseTask) { t.Kind = CloseCheck }), "close.tasks[AP_CUTOFF].check"},
		{"a due day after 31", task(0, func(t *CloseTask) { t.DueDay = 32 }), "close.tasks[AP_CUTOFF].due_day"},
		{"a due day before -31", task(0, func(t *CloseTask) { t.DueDay = -32 }), "close.tasks[AP_CUTOFF].due_day"},
		{"a company not in the document", task(2, func(t *CloseTask) { t.Companies = []string{"9000"} }), "close.tasks[REPORT].companies"},
		{"a company twice", task(2, func(t *CloseTask) { t.Companies = []string{"1000", "1000"} }), "close.tasks[REPORT].companies"},
		{"a dependency not in the template", task(1, func(t *CloseTask) { t.DependsOn = []string{"BANK"} }), "close.tasks[ACCRUALS].depends_on"},
		{"a dependency twice", task(1, func(t *CloseTask) { t.DependsOn = []string{"AP_CUTOFF", "AP_CUTOFF"} }), "close.tasks[ACCRUALS].depends_on"},
		{"a dependency on a task some of its companies lack", task(0, func(t *CloseTask) { t.Companies = []string{"1000"} }),
			"close.tasks[ACCRUALS].depends_on"},
		{"a dependency on a task of other companies", func(c *Config) {
			c.Close.Tasks[0].Companies = []string{"1000"}
			c.Close.Tasks[1].Companies = []string{"2000"}
		}, "close.tasks[ACCRUALS].depends_on"},
		{"a task depending on itself", task(0, func(t *CloseTask) { t.DependsOn = []string{"AP_CUTOFF"} }), "close.tasks[AP_CUTOFF].depends_on"},
		{"a cycle of two", task(0, func(t *CloseTask) { t.DependsOn = []string{"ACCRUALS"} }), "close.tasks[ACCRUALS].depends_on"},
		{"more than 200 tasks", func(c *Config) {
			for i := range 199 {
				c.Close.Tasks = append(c.Close.Tasks, CloseTask{Code: "T" + strings.Repeat("X", i%29) + string(rune('A'+i/29)), Name: "Task", Kind: CloseManual})
			}
		}, "close.tasks"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := withClose(t)
			tc.mutate(c)
			fs := Validate(c, currency.Seeded())
			if !hasPath(fs, tc.path) {
				t.Errorf("findings: got %v, want one at %s", fs, tc.path)
			}
		})
	}
}

// TestCloseCycleNamedOnce: a cycle reached from several tasks names each
// task on it once.
func TestCloseCycleNamedOnce(t *testing.T) {
	c := withClose(t)
	c.Close.Tasks[0].DependsOn = []string{"REPORT"}
	c.Close.Tasks[0].Companies = []string{"1000"}
	c.Close.Tasks[1].Companies = []string{"1000"}
	var got []string
	for _, f := range Validate(c, currency.Seeded()) {
		got = append(got, f.Path)
	}
	want := []string{"close.tasks[ACCRUALS].depends_on", "close.tasks[AP_CUTOFF].depends_on", "close.tasks[REPORT].depends_on"}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("findings: got %v, want %v", got, want)
	}
}

// TestCloseTasksOf: a company's close is given the tasks for every company
// and those listing it, in the template's order.
func TestCloseTasksOf(t *testing.T) {
	c := withClose(t)
	for company, want := range map[string]string{"1000": "AP_CUTOFF ACCRUALS REPORT NO_PARKED REVALUED", "2000": "AP_CUTOFF ACCRUALS NO_PARKED REVALUED"} {
		t.Run(company, func(t *testing.T) {
			var got []string
			for _, task := range c.CloseTasksOf(company) {
				got = append(got, task.Code)
			}
			if strings.Join(got, " ") != want {
				t.Errorf("CloseTasksOf(%s): got %v, want %s", company, got, want)
			}
		})
	}
}

// TestCloseTiers: a task's name is cosmetic, the rest of the template and
// a company's weekend operational (ADR-0058 §3).
func TestCloseTiers(t *testing.T) {
	base := withClose(t)
	changed := withClose(t)
	changed.Close.Tasks[0].Name = "Supplier invoices entered"
	changed.Close.Tasks[1].DueDay = 4
	changed.Close.Tasks = append(changed.Close.Tasks, CloseTask{Code: "BANK", Name: "Bank reconciled", Kind: CloseManual})
	changed.Calendars = []Calendar{{Company: "1000", Weekend: []string{"friday", "saturday"}}}
	diffs, _ := Diff(base, changed) // Diff never fails
	want := map[string]Tier{
		"close.tasks[AP_CUTOFF].name":   TierCosmetic,
		"close.tasks[ACCRUALS].due_day": TierOperational,
		"close.tasks[BANK]":             TierOperational,
		"calendars[1000]":               TierOperational,
	}
	if len(diffs) != len(want) {
		t.Errorf("Diff: got %v, want %d changes", diffs, len(want))
	}
	for _, d := range diffs {
		if tier, ok := want[d.Path]; !ok || d.Tier != tier {
			t.Errorf("Diff %s: got %v, want %v", d.Path, d.Tier, tier)
		}
	}
}

// TestWeekendValid: a calendar's weekend names weekdays, each once, and
// leaves at least one business day.
func TestWeekendValid(t *testing.T) {
	weekend := func(days ...string) func(c *Config) {
		return func(c *Config) { c.Calendars = []Calendar{{Company: "1000", Weekend: days}} }
	}
	c := fixture(t)
	weekend("friday", "saturday")(c)
	if fs := Validate(c, currency.Seeded()); HasErrors(fs) {
		t.Fatalf("a Friday and Saturday weekend: got %v, want no errors", fs)
	}
	cases := []struct {
		name   string
		mutate func(c *Config)
	}{
		{"an abbreviation", weekend("sat")},
		{"a capital", weekend("Saturday")},
		{"a day twice", weekend("sunday", "sunday")},
		{"every day", weekend("monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture(t)
			tc.mutate(c)
			if fs := Validate(c, currency.Seeded()); !hasPath(fs, "calendars[1000].weekend") {
				t.Errorf("findings: got %v, want one at calendars[1000].weekend", fs)
			}
		})
	}
}

// TestBusinessDay: a business day is no weekend day of the company's
// calendar (Saturday and Sunday when it names none) and none of its
// holidays, judged on the date in the time's own location.
func TestBusinessDay(t *testing.T) {
	c := fixture(t)
	c.Calendars = []Calendar{
		{Company: "1000", Holidays: []Holiday{{Date: "2026-12-25", Name: "Christmas Day"}}},
		{Company: "2000", Weekend: []string{"friday", "saturday"}, Holidays: []Holiday{{Date: "2026-12-27", Name: "A Sunday holiday"}}},
	}
	toronto, err := time.LoadLocation("America/Toronto")
	if err != nil {
		t.Fatal(err)
	}
	day := func(d int) time.Time { return time.Date(2026, 12, d, 0, 0, 0, 0, time.UTC) } // 2026-12-21 is a Monday
	cases := []struct {
		name    string
		company string
		on      time.Time
		want    bool
	}{
		{"a Monday", "1000", day(21), true},
		{"a Friday", "1000", day(18), true},
		{"a Saturday", "1000", day(19), false},
		{"a Sunday", "1000", day(20), false},
		{"a holiday on a Friday", "1000", day(25), false},
		{"a holiday's date in another zone", "1000", time.Date(2026, 12, 26, 3, 0, 0, 0, time.UTC).In(toronto), false},
		{"a company without a calendar: Saturday", "3000", day(19), false},
		{"a company without a calendar: Monday", "3000", day(21), true},
		{"a Friday weekend", "2000", day(18), false},
		{"a Saturday weekend", "2000", day(19), false},
		{"a Sunday business day", "2000", day(20), true},
		{"a holiday on a Sunday", "2000", day(27), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.BusinessDay(tc.company, tc.on); got != tc.want {
				t.Errorf("BusinessDay(%s, %s): got %v, want %v", tc.company, tc.on.Format(time.DateOnly), got, tc.want)
			}
		})
	}
	got := c.Weekend("2000")
	got[0] = "monday"
	if w := c.Weekend("2000"); !slices.Equal(w, []string{"friday", "saturday"}) {
		t.Errorf("Weekend after changing a copy: got %v, want [friday saturday]", w)
	}
	if w := c.Weekend("1000"); !slices.Equal(w, DefaultWeekend) {
		t.Errorf("Weekend of a calendar naming none: got %v, want %v", w, DefaultWeekend)
	}
}

// withYearTasks is withClose with year tasks (ADR-0060 §8) whose codes
// repeat the period tasks': a list's codes are its own.
func withYearTasks(t *testing.T) *Config {
	t.Helper()
	c := withClose(t)
	c.Close.YearTasks = []CloseTask{
		{Code: "AP_CUTOFF", Name: "Auditors' adjustments entered", Kind: CloseManual, DueDay: 20},
		{Code: "INVENTORY", Name: "Inventory count reconciled", Kind: CloseManual, DueDay: 10, DependsOn: []string{"AP_CUTOFF"}, Companies: []string{"1000"}},
	}
	return c
}

// TestYearTasksValid: year tasks validate as the period's tasks do, on
// their own path and codes, and name no check until T64m5 builds the year
// close's: a period check is refused there.
func TestYearTasksValid(t *testing.T) {
	if fs := Validate(withYearTasks(t), currency.Seeded()); HasErrors(fs) {
		t.Fatalf("year tasks configured: got %v, want no errors", fs)
	}
	task := func(i int, mutate func(*CloseTask)) func(c *Config) {
		return func(c *Config) { mutate(&c.Close.YearTasks[i]) }
	}
	cases := []struct {
		name   string
		mutate func(c *Config)
		path   string
	}{
		{"a lower-case code", task(0, func(t *CloseTask) { t.Code = "ap_cutoff" }), "close.year_tasks[ap_cutoff].code"},
		{"a task twice", func(c *Config) { c.Close.YearTasks = append(c.Close.YearTasks, c.Close.YearTasks[0]) }, "close.year_tasks[AP_CUTOFF]"},
		{"no name", task(0, func(t *CloseTask) { t.Name = "" }), "close.year_tasks[AP_CUTOFF].name"},
		{"a period check", task(0, func(t *CloseTask) { t.Kind, t.Check = CloseCheck, "accruals_decided" }), "close.year_tasks[AP_CUTOFF].check"},
		{"a due day after 31", task(0, func(t *CloseTask) { t.DueDay = 32 }), "close.year_tasks[AP_CUTOFF].due_day"},
		{"a company not in the document", task(1, func(t *CloseTask) { t.Companies = []string{"9000"} }), "close.year_tasks[INVENTORY].companies"},
		{"a dependency on a period task", task(1, func(t *CloseTask) { t.DependsOn = []string{"REVALUED"} }), "close.year_tasks[INVENTORY].depends_on"},
		{"a cycle", task(0, func(t *CloseTask) { t.DependsOn = []string{"INVENTORY"}; t.Companies = []string{"1000"} }),
			"close.year_tasks[AP_CUTOFF].depends_on"},
		{"more than 200 tasks", func(c *Config) {
			for i := range 199 {
				c.Close.YearTasks = append(c.Close.YearTasks, CloseTask{Code: "Y" + strings.Repeat("X", i%29) + string(rune('A'+i/29)), Name: "Task", Kind: CloseManual})
			}
		}, "close.year_tasks"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := withYearTasks(t)
			tc.mutate(c)
			if fs := Validate(c, currency.Seeded()); !hasPath(fs, tc.path) {
				t.Errorf("findings: got %v, want one at %s", fs, tc.path)
			}
		})
	}
}

// TestYearCloseTasksOf: a company's year close is given the year tasks for
// every company and those listing it, in the template's order, and none of
// the period's.
func TestYearCloseTasksOf(t *testing.T) {
	c := withYearTasks(t)
	for company, want := range map[string]string{"1000": "AP_CUTOFF INVENTORY", "2000": "AP_CUTOFF"} {
		t.Run(company, func(t *testing.T) {
			var got []string
			for _, task := range c.YearCloseTasksOf(company) {
				got = append(got, task.Code)
			}
			if strings.Join(got, " ") != want {
				t.Errorf("YearCloseTasksOf(%s): got %v, want %s", company, got, want)
			}
		})
	}
}

// TestYearTaskTiers: a year task's name is cosmetic, the rest of the year
// tasks operational, as the period's (ADR-0060 §8).
func TestYearTaskTiers(t *testing.T) {
	base := withYearTasks(t)
	changed := withYearTasks(t)
	changed.Close.YearTasks[0].Name = "Audit adjustments entered"
	changed.Close.YearTasks[1].DueDay = 15
	changed.Close.YearTasks = append(changed.Close.YearTasks, CloseTask{Code: "TAX", Name: "Tax provision booked", Kind: CloseManual})
	diffs, _ := Diff(base, changed) // Diff never fails
	want := map[string]Tier{
		"close.year_tasks[AP_CUTOFF].name":    TierCosmetic,
		"close.year_tasks[INVENTORY].due_day": TierOperational,
		"close.year_tasks[TAX]":               TierOperational,
	}
	if len(diffs) != len(want) {
		t.Errorf("Diff: got %v, want %d changes", diffs, len(want))
	}
	for _, d := range diffs {
		if tier, ok := want[d.Path]; !ok || d.Tier != tier {
			t.Errorf("Diff %s: got %v, want %v", d.Path, d.Tier, tier)
		}
	}
}
