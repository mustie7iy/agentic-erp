package analytics

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
)

// byAccount is a result by month and account: each entry
// "year-month account=cents".
func byAccount(t *testing.T, entries ...string) *Result {
	t.Helper()
	res := &Result{}
	for _, e := range entries {
		var y, m int
		var a string
		var c int64
		if _, err := fmt.Sscanf(e, "%d-%d %4s=%d", &y, &m, &a, &c); err != nil {
			t.Fatal(err)
		}
		res.Rows = append(res.Rows, Row{fmt.Sprint(y), fmt.Sprint(m), a, cad(c)})
	}
	return res
}

// scenarioOf is a scenario's periods as "label baseline scenario
// difference" and its totals.
func scenarioOf(f *Forecast) string {
	var out []string
	for _, p := range f.Scenario.Points {
		out = append(out, fmt.Sprintf("%s %s %s %s", p.Period.Label(f.Spec.Grain), p.Baseline, p.Scenario, p.Difference))
	}
	sc := f.Scenario
	return strings.Join(out, ", ") + fmt.Sprintf("; totals %s %s %s", sc.BaselineTotal, sc.ScenarioTotal, sc.DifferenceTotal)
}

// scenarioSpec is three months' moving average two months ahead with the
// adjustments.
func scenarioSpec(adj ...Adjustment) ForecastSpec {
	return ForecastSpec{Metric: "revenue", Grain: "month", Ahead: 2, Method: MovingAverage, Window: 3, Scenario: adj}
}

// TestScenario: each adjustment's rows and the rest are forecast on their
// own; the baseline is their sum and the forecast, the scenario the same
// with each adjusted part changed and rounded half away from zero, and
// both add up (T97b).
func TestScenario(t *testing.T) {
	m := model(t)
	months := byAccount(t, "2026-7 4000=10000", "2026-7 4100=1000", "2026-8 4000=20000", "2026-8 4100=1000", "2026-9 4000=30001", "2026-9 4100=1001")
	for name, c := range map[string]struct {
		adj   []Adjustment
		res   *Result
		want  string
		parts string
	}{
		// 4000 averages 200.003 (200.00), 4100 10.003 (10.00): the
		// baseline is 210.00, where the total's own average is 210.01.
		"ten percent more of one account": {[]Adjustment{{Dim: "account", Value: "4000", Percent: "10"}}, months,
			"2026-10 210.00 230.00 20.00, 2026-11 210.00 230.00 20.00; totals 420.00 460.00 40.00", "account 4000 10: 400.00 440.00"},
		"two accounts, one less": {[]Adjustment{{Dim: "account", Value: "4000", Percent: "10"}, {Dim: "account", Value: "4100", Percent: "-100"}}, months,
			"2026-10 210.00 220.00 10.00, 2026-11 210.00 220.00 10.00; totals 420.00 440.00 20.00", "account 4000 10: 400.00 440.00; account 4100 -100: 20.00 0.00"},
		"a value with no rows": {[]Adjustment{{Dim: "account", Value: "4999", Percent: "50"}}, months,
			"2026-10 210.01 210.01 0.00, 2026-11 210.01 210.01 0.00; totals 420.02 420.02 0.00", "account 4999 50: 0.00 0.00"},
		"half a cent away from zero": {[]Adjustment{{Dim: "account", Value: "4000", Percent: "10"}},
			byAccount(t, "2026-7 4000=5", "2026-8 4000=5", "2026-9 4000=-5", "2026-9 4100=0"),
			"2026-10 0.02 0.02 0.00, 2026-11 0.02 0.02 0.00; totals 0.04 0.04 0.00", "account 4000 10: 0.04 0.04"},
	} {
		t.Run(name, func(t *testing.T) {
			s := scenarioSpec(c.adj...)
			if err := s.Check(m); err != nil {
				t.Fatal(err)
			}
			f, err := NewForecast(m, s, c.res)
			if err != nil {
				t.Fatal(err)
			}
			if got := scenarioOf(f); got != c.want {
				t.Errorf("scenario: got %s, want %s", got, c.want)
			}
			var parts []string
			for _, p := range f.Scenario.Parts {
				parts = append(parts, fmt.Sprintf("%s %s %s: %s %s", p.Adjustment.Dim, p.Adjustment.Value, p.Adjustment.Percent, p.BaselineTotal, p.Adjusted))
			}
			if got := strings.Join(parts, "; "); got != c.parts {
				t.Errorf("parts: got %s, want %s", got, c.parts)
			}
			if got, want := projection(f), fmt.Sprintf("2026-10 %s, 2026-11 %s", f.Scenario.Points[0].Baseline, f.Scenario.Points[1].Baseline); got != want {
				t.Errorf("forecast: got %s, want the baseline %s", got, want)
			}
			if f.ProjectionTotal != f.Scenario.BaselineTotal {
				t.Errorf("forecast total: got %s, want the baseline's %s", f.ProjectionTotal, f.Scenario.BaselineTotal)
			}
		})
	}
	t.Run("the history is the rows summed by period", func(t *testing.T) {
		f, err := NewForecast(m, scenarioSpec(Adjustment{Dim: "account", Value: "4000", Percent: "10"}), months)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, pt := range f.History {
			got = append(got, pt.Value.String())
		}
		if strings.Join(got, " ") != "110.00 210.00 310.02" || f.HistoryTotal.String() != "630.02" {
			t.Errorf("history: got %v, total %s; want 110.00 210.00 310.02, 630.02", got, f.HistoryTotal)
		}
		if used := strings.Join(f.UsedLabels(), " "); used != "2026-07 2026-08 2026-09" {
			t.Errorf("used: got %s", used)
		}
	})
	t.Run("the same period a year before", func(t *testing.T) {
		s := ForecastSpec{Metric: "revenue", Grain: "month", Ahead: 1, Method: SamePeriodLastYear, Scenario: []Adjustment{{Dim: "account", Value: "4000", Percent: "-5"}}}
		f, err := NewForecast(m, s, byAccount(t, "2025-10 4000=10000", "2025-10 4100=500", "2026-9 4000=1"))
		if err != nil {
			t.Fatal(err)
		}
		if got := scenarioOf(f); got != "2026-10 105.00 100.00 -5.00; totals 105.00 100.00 -5.00" {
			t.Errorf("scenario: got %s", got)
		}
	})
	t.Run("the query reads the adjustments' dimensions once each", func(t *testing.T) {
		s := scenarioSpec(Adjustment{Dim: "account", Value: "4000", Percent: "1"}, Adjustment{Dim: "company", Value: "3000", Percent: "1"},
			Adjustment{Dim: "account", Value: "4100", Percent: "1"})
		if by := strings.Join(s.Query().By, " "); by != "fiscal_year fiscal_month account company" {
			t.Errorf("by: got %s", by)
		}
	})
}

// TestScenarioRefusals: an adjustment of a period's dimension or of one
// the metric lacks, a percentage that is none or takes off more than
// all, and two adjustments on one row are refused naming the adjustment.
func TestScenarioRefusals(t *testing.T) {
	m := model(t)
	adj := func(dim, value, pct string) Adjustment { return Adjustment{Dim: dim, Value: value, Percent: pct} }
	for name, c := range map[string]struct {
		adj         []Adjustment
		code, field string
	}{
		"a period's dimension":       {[]Adjustment{adj("fiscal_month", "9", "10")}, ForecastInvalid, "scenario.adjustments[0].dimension"},
		"a dimension it lacks":       {[]Adjustment{adj("account", "4000", "1"), adj("supplier", "S1", "10")}, ForecastInvalid, "scenario.adjustments[1].dimension"},
		"no percentage":              {[]Adjustment{adj("account", "4000", "ten")}, ForecastInvalid, "scenario.adjustments[0].percent"},
		"more than all off":          {[]Adjustment{adj("account", "4000", "-100.5")}, ForecastInvalid, "scenario.adjustments[0].percent"},
		"one value named twice":      {[]Adjustment{adj("account", "4000", "1"), adj("company", "3000", "1"), adj("account", "4000", "2")}, ForecastOverlap, "scenario.adjustments[2]"},
		"a period's dimension, year": {[]Adjustment{adj("fiscal_year", "2026", "10")}, ForecastInvalid, "scenario.adjustments[0].dimension"},
	} {
		t.Run(name, func(t *testing.T) {
			var fe *ForecastError
			if err := scenarioSpec(c.adj...).Check(m); !errors.As(err, &fe) || fe.Code != c.code || fe.Field != c.field {
				t.Errorf("got %v, want %s on %s", err, c.code, c.field)
			}
		})
	}
	if err := scenarioSpec(adj("account", "4000", "-100")).Check(m); err != nil {
		t.Errorf("all off: got %v, want it allowed", err)
	}
	if err := (ForecastSpec{Metric: "nonesuch", Grain: "month", Ahead: 1, Method: MovingAverage, Window: 1, Scenario: []Adjustment{adj("account", "1", "1")}}).Check(m); err != nil {
		t.Errorf("an unknown metric: got %v, want it left to the query", err)
	}
	two := scenarioSpec(adj("account", "4000", "10"), adj("company", "3000", "5"))
	rows := &Result{Rows: []Row{{"2026", "9", "4000", "3000", cad(1)}}}
	var fe *ForecastError
	if _, err := NewForecast(m, two, rows); !errors.As(err, &fe) || fe.Code != ForecastOverlap || fe.Field != "scenario.adjustments[1]" ||
		!strings.Contains(fe.Detail, "Adjustments 0 (account 4000) and 1 (company 3000)") {
		t.Errorf("two adjustments meeting in a row: got %v", err)
	}
	if _, err := NewForecast(m, two, &Result{Rows: []Row{{"2026", "9", "4000", "1000", cad(1)}, {"2026", "9", "4100", "3000", cad(1)}}}); err == nil {
		t.Errorf("two adjustments on rows of their own and a short history: got no error, want the history refused")
	}
}

// TestScenarioBounds: what the history cannot be read as and figures out
// of an amount's range are errors, never a scenario.
func TestScenarioBounds(t *testing.T) {
	m := model(t)
	const half = math.MaxInt64 / 2
	pct := func(p string) Adjustment { return Adjustment{Dim: "account", Value: "4000", Percent: p} }
	usd := Value{Units: 1, Scale: 2, Currency: "USD"}
	// rows are September's rows of accounts 4000, 4100, ... with the
	// amounts, in that order.
	rows := func(amounts ...int64) *Result {
		res := &Result{}
		for i, a := range amounts {
			res.Rows = append(res.Rows, Row{"2026", "9", fmt.Sprint(4000 + 100*i), cad(a)})
		}
		return res
	}
	off := func(accounts ...int) []Adjustment {
		var out []Adjustment
		for _, a := range accounts {
			out = append(out, Adjustment{Dim: "account", Value: fmt.Sprint(a), Percent: "-100"})
		}
		return out
	}
	for name, c := range map[string]struct {
		s   ForecastSpec
		res *Result
	}{
		"a period that is no number":  {scenarioSpec(pct("1")), &Result{Rows: []Row{{"2026", "May", "4000", cad(1)}}}},
		"two currencies in a part":    {scenarioSpec(pct("1")), &Result{Rows: []Row{{"2026", "9", "4000", cad(1)}, {"2026", "9", "4000", usd}}}},
		"two currencies in a period":  {scenarioSpec(pct("1")), &Result{Rows: []Row{{"2026", "9", "4000", cad(1)}, {"2026", "9", "4100", usd}}}},
		"a period out of range":       {scenarioSpec(pct("1")), &Result{Rows: []Row{{"2026", "9", "4000", cad(math.MaxInt64)}, {"2026", "9", "4000", cad(1)}}}},
		"too short a history":         {scenarioSpec(pct("1")), rows(1)},
		"a change out of range":       {ahead(1, pct("100")), rows(half + 1)},
		"a part's total out of range": {ahead(2, pct("0")), rows(half + 1)},
		"a part changed out of range": {ahead(2, pct("50")), rows(half / 4 * 3)},
		"a baseline out of range": {ForecastSpec{Metric: "revenue", Grain: "month", Ahead: 1, Method: MovingAverage, Window: 2, Scenario: []Adjustment{pct("0")}},
			// Each part's average rounds up half a cent: the parts sum to a cent more than either month.
			&Result{Rows: []Row{{"2026", "8", "4000", cad(math.MaxInt64)}, {"2026", "9", "4100", cad(math.MaxInt64)}}}},
		"a scenario out of range":   {ahead(1, pct("100")), rows(half/2+2, half)},
		"a difference out of range": {ahead(1, Adjustment{Dim: "account", Value: "4000", Percent: "99999"}, Adjustment{Dim: "account", Value: "4100", Percent: "99999"}), rows(-math.MaxInt64/1500, -math.MaxInt64/1500, math.MaxInt64/10*9)},
		"the baselines' total":      {ahead(2, pct("0")), rows(1, half+1)},
		"the scenarios' total":      {ahead(2, pct("100")), rows(half/4, half/10*6)},
		"the differences' total":    {ahead(2, off(4000, 4100, 4200, 4300)...), rows(-math.MaxInt64/100*15, -math.MaxInt64/100*15, -math.MaxInt64/100*15, -math.MaxInt64/100*15, math.MaxInt64/10*3)},
	} {
		t.Run(name, func(t *testing.T) {
			if f, err := NewForecast(m, c.s, c.res); err == nil {
				t.Errorf("got %s, want an error", scenarioOf(f))
			}
		})
	}
	// A row whose measure cannot be computed has no data.
	f, err := NewForecast(m, ahead(1, pct("10")), &Result{Rows: []Row{{"2026", "8", "4000", nil}, {"2026", "9", "4000", cad(100)}}})
	if err != nil || scenarioOf(f) != "2026-10 1.00 1.10 0.10; totals 1.00 1.10 0.10" {
		t.Errorf("a row without data: got %v, %v", f, err)
	}
}

// ahead forecasts the periods ahead by the last month alone.
func ahead(n int, adj ...Adjustment) ForecastSpec {
	s := scenarioSpec(adj...)
	s.Window, s.Ahead = 1, n
	return s
}
