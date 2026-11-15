package analytics

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// cad is an amount of CAD in cents.
func cad(cents int64) Value { return Value{Units: money.Minor(cents), Scale: 2, Currency: "CAD"} }

// model is the embedded semantic model.
func model(t *testing.T) *Model {
	t.Helper()
	m, err := DefaultModel()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// history is a month-by-month result: each entry "year-month=cents".
func history(t *testing.T, entries ...string) *Result {
	t.Helper()
	res := &Result{}
	for _, e := range entries {
		var y, m int
		var c int64
		if _, err := fmt.Sscanf(e, "%d-%d=%d", &y, &m, &c); err != nil {
			t.Fatal(err)
		}
		res.Rows = append(res.Rows, Row{fmt.Sprint(y), fmt.Sprint(m), cad(c)})
	}
	return res
}

// projection is the forecast's periods as "label value".
func projection(f *Forecast) string {
	var out []string
	for _, pt := range f.Projection {
		out = append(out, pt.Period.Label(f.Spec.Grain)+" "+pt.Value.String())
	}
	return strings.Join(out, ", ")
}

// TestForecastMovingAverage: each period ahead is the average of the
// last periods, exactly and rounded half away from zero; a month without
// data is zero; the history runs across a fiscal year's end.
func TestForecastMovingAverage(t *testing.T) {
	m := model(t)
	for name, c := range map[string]struct {
		res              *Result
		window, ahead    int
		want, used, hist string
	}{
		"exact":                         {history(t, "2026-7=10000", "2026-8=20000", "2026-9=30000"), 3, 2, "2026-10 200.00, 2026-11 200.00", "2026-07 2026-09", "600.00"},
		"half up":                       {history(t, "2026-8=1", "2026-9=2"), 2, 1, "2026-10 0.02", "2026-08 2026-09", "0.03"},
		"half away from zero, negative": {history(t, "2026-8=-1", "2026-9=-2"), 2, 1, "2026-10 -0.02", "2026-08 2026-09", "-0.03"},
		"a month without data is zero":  {history(t, "2026-7=30000", "2026-9=30000"), 3, 1, "2026-10 200.00", "2026-07 2026-09", "600.00"},
		"across the year's end":         {history(t, "2026-11=100", "2026-12=200", "2027-1=300"), 2, 2, "2027-02 2.50, 2027-03 2.50", "2026-12 2027-01", "6.00"},
		"the last periods alone":        {history(t, "2026-1=99999", "2026-8=100", "2026-9=300"), 2, 1, "2026-10 2.00", "2026-08 2026-09", "1003.99"},
	} {
		t.Run(name, func(t *testing.T) {
			s := ForecastSpec{Metric: "revenue", Grain: "month", Ahead: c.ahead, Method: MovingAverage, Window: c.window}
			f, err := NewForecast(m, s, c.res)
			if err != nil {
				t.Fatal(err)
			}
			if got := projection(f); got != c.want {
				t.Errorf("forecast: got %s, want %s", got, c.want)
			}
			if used := f.UsedLabels(); used[0]+" "+used[len(used)-1] != c.used || len(used) != c.window {
				t.Errorf("used: got %v, want %d periods %s", used, c.window, c.used)
			}
			if got := f.HistoryTotal.String(); got != c.hist {
				t.Errorf("history total: got %s, want %s", got, c.hist)
			}
			if want := fmt.Sprintf("each period is the average of the last %d months of history (%s to %s)", c.window, c.used[:7], c.used[8:]); !strings.Contains(f.Basis(), want) {
				t.Errorf("basis: got %s, want it to say %s", f.Basis(), want)
			}
		})
	}
}

// TestForecastSamePeriod: each period ahead is the same period a fiscal
// year before; a quarter and a year are periods too.
func TestForecastSamePeriod(t *testing.T) {
	m := model(t)
	f, err := NewForecast(m, ForecastSpec{Metric: "revenue", Grain: "month", Ahead: 2, Method: SamePeriodLastYear},
		history(t, "2025-10=500", "2025-11=700", "2026-9=100"))
	if err != nil {
		t.Fatal(err)
	}
	if got := projection(f); got != "2026-10 5.00, 2026-11 7.00" {
		t.Errorf("forecast: got %s, want 2025-10's and 2025-11's", got)
	}
	if got := strings.Join(f.UsedLabels(), " "); got != "2025-10 2025-11" || f.ProjectionTotal.String() != "12.00" {
		t.Errorf("used: got %s, total %s; want 2025-10 2025-11 and 12.00", got, f.ProjectionTotal)
	}
	if !strings.Contains(f.Basis(), "same month a fiscal year before (2025-10 to 2025-11)") {
		t.Errorf("basis: got %s", f.Basis())
	}
	quarters := &Result{Rows: []Row{{"2025", "4", cad(400)}, {"2026", "3", cad(300)}}}
	f, err = NewForecast(m, ForecastSpec{Metric: "revenue", Grain: "quarter", Ahead: 1, Method: SamePeriodLastYear}, quarters)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection(f); got != "2026-Q4 4.00" {
		t.Errorf("quarter: got %s, want 2025-Q4's", got)
	}
	years := &Result{Rows: []Row{{"2025", cad(400)}, {"2026", cad(300)}}}
	f, err = NewForecast(m, ForecastSpec{Metric: "revenue", Grain: "year", Ahead: 1, Method: SamePeriodLastYear}, years)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection(f); got != "2027 3.00" {
		t.Errorf("year: got %s, want 2026's", got)
	}
}

// TestForecastCount: a count is forecast as a whole number.
func TestForecastCount(t *testing.T) {
	res := &Result{Rows: []Row{{"2026", "1", Value{Units: 3}}, {"2026", "2", Value{Units: 4}}}}
	f, err := NewForecast(model(t), ForecastSpec{Metric: "invoice_count", Grain: "month", Ahead: 1, Method: MovingAverage, Window: 2}, res)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection(f); got != "2026-03 4" || f.Currency != "" || f.Type != Count {
		t.Errorf("count: got %s in %q of %s, want 4 (3.5 rounded) with no currency", got, f.Currency, f.Type)
	}
}

// TestForecastRefusals: what the method or the metric cannot forecast is
// refused naming the field or what is missing.
func TestForecastRefusals(t *testing.T) {
	m := model(t)
	ok := ForecastSpec{Metric: "revenue", Grain: "month", Ahead: 3, Method: MovingAverage, Window: 3}
	for name, c := range map[string]struct {
		change      func(s *ForecastSpec)
		code, field string
	}{
		"a week":                          {func(s *ForecastSpec) { s.Grain = "week" }, ForecastInvalid, "grain"},
		"a trend":                         {func(s *ForecastSpec) { s.Method = "trend" }, ForecastInvalid, "method"},
		"a moving average without window": {func(s *ForecastSpec) { s.Window = 0 }, ForecastInvalid, "window"},
		"a window for last year":          {func(s *ForecastSpec) { s.Method = SamePeriodLastYear }, ForecastInvalid, "window"},
		"13 months a year before": {func(s *ForecastSpec) { s.Method, s.Window, s.Ahead = SamePeriodLastYear, 0, 13 },
			ForecastInvalid, "periods_ahead"},
		"a ratio": {func(s *ForecastSpec) { s.Metric = "operating_margin" }, ForecastRatio, "metric"},
	} {
		t.Run(name, func(t *testing.T) {
			s := ok
			c.change(&s)
			var fe *ForecastError
			if err := s.Check(m); !errors.As(err, &fe) || fe.Code != c.code || fe.Field != c.field {
				t.Errorf("got %v, want %s on %s", err, c.code, c.field)
			}
		})
	}
	if err := ok.Check(m); err != nil {
		t.Errorf("a valid forecast: got %v", err)
	}
	if q := ok.Query(); strings.Join(q.Measures, " ")+" by "+strings.Join(q.By, " ") != "revenue by fiscal_year fiscal_month" {
		t.Errorf("query: got %v, want revenue by fiscal year and month", q)
	}
	if q := (ForecastSpec{Metric: "revenue", Grain: "year"}).Query(); len(q.By) != 1 {
		t.Errorf("a year's query: got %v, want it by fiscal year alone", q.By)
	}
	for name, c := range map[string]struct {
		s      ForecastSpec
		res    *Result
		detail string
	}{
		"no history": {ok, &Result{Rows: []Row{{"2026", "1", nil}}}, "revenue has no history to forecast from"},
		"too few months": {ok, history(t, "2026-8=1", "2026-9=1"),
			"moving_average needs revenue from 2026-07 to 2026-09; its history runs from 2026-08 to 2026-09."},
		"more periods than a query answers": {ok, &Result{Truncated: true}, "The history has more periods than a query answers"},
		"two currencies": {ok, &Result{Rows: []Row{{"2026", "1", cad(1)}, {"2026", "2", Value{Units: 1, Scale: 2, Currency: "USD"}}}},
			"revenue's history is in amounts in CAD and amounts in USD"},
		"no year before": {ForecastSpec{Metric: "revenue", Grain: "month", Ahead: 2, Method: SamePeriodLastYear}, history(t, "2025-11=1", "2026-9=1"),
			"same_period_last_year needs revenue from 2025-10 to 2025-11; its history runs from 2025-11 to 2026-09."},
	} {
		t.Run(name, func(t *testing.T) {
			var fe *ForecastError
			if _, err := NewForecast(m, c.s, c.res); !errors.As(err, &fe) || !strings.HasPrefix(fe.Error(), c.detail) {
				t.Errorf("got %v, want %s", err, c.detail)
			}
		})
	}
}

// TestForecastBounds: a period the semantic layer did not number, and
// totals or averages out of an amount's range, are errors, never a
// forecast.
func TestForecastBounds(t *testing.T) {
	m := model(t)
	s := ForecastSpec{Metric: "revenue", Grain: "month", Ahead: 1, Method: MovingAverage, Window: 1}
	for name, res := range map[string]*Result{
		"a period that is no number": {Rows: []Row{{"2026", "May", cad(1)}}},
		"a history out of range":     {Rows: []Row{{"2026", "1", cad(math.MaxInt64)}, {"2026", "2", cad(1)}}},
		"a window out of range":      {Rows: []Row{{"2026", "1", cad(math.MaxInt64)}, {"2026", "2", cad(1)}}},
		"the most negative int64":    {Rows: []Row{{"2026", "1", cad(math.MinInt64)}}},
		"a forecast out of range":    {Rows: []Row{{"2026", "1", cad(math.MaxInt64)}}},
	} {
		t.Run(name, func(t *testing.T) {
			spec := s
			switch name {
			case "a forecast out of range":
				spec.Ahead = 2
			case "a window out of range":
				spec.Window = 2
			}
			if f, err := NewForecast(m, spec, res); err == nil {
				t.Errorf("got %v, want an error", projection(f))
			}
		})
	}
}
