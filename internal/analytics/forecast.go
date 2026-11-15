package analytics

import (
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// The methods a governed metric is forecast by (ADR-0076 §8, T97a).
const (
	MovingAverage      = "moving_average"
	SamePeriodLastYear = "same_period_last_year"
)

// grains are the fiscal periods a forecast is made by: the calendar
// dimension that numbers a period within its fiscal year (none for a
// year) and how many a year has.
var grains = map[string]struct {
	dim     string
	perYear int
}{"month": {"fiscal_month", 12}, "quarter": {"fiscal_quarter", 4}, "year": {"", 1}}

// ForecastSpec is a forecast as an analyst asks for it.
type ForecastSpec struct {
	Metric    string
	Where     []Filter
	Grain     string // month, quarter or year
	Ahead     int    // the periods after the history to forecast
	Method    string // MovingAverage or SamePeriodLastYear
	Window    int    // MovingAverage: the last periods it averages
	Companies []string
}

// ForecastError is a forecast refused: Code names the problem and Field
// the request's field, when one is at fault.
type ForecastError struct {
	Code, Field, Detail string
}

func (e *ForecastError) Error() string { return e.Detail }

// The codes of a refused forecast.
const (
	ForecastInvalid      = "request.validation_failed"
	ForecastRatio        = "analytics.forecast_ratio"
	ForecastHistoryShort = "analytics.forecast_history_short"
	// ForecastCurrencies is a history in several currencies, refused as
	// the semantic layer refuses amounts of two currencies summed, or one
	// of more periods than a query answers.
	ForecastCurrencies = "analytics.invalid_query"
)

// Check refuses a forecast the method or the metric cannot make: an
// unknown grain or method, a window given to a method without one or
// missing from moving_average, more periods ahead than a year has for
// the same period a year before, and a ratio, whose average is not the
// ratio of averages. An unknown metric is left to the query, which
// refuses it.
func (s ForecastSpec) Check(m *Model) error {
	g, ok := grains[s.Grain]
	invalid := func(field, detail string) error {
		return &ForecastError{Code: ForecastInvalid, Field: field, Detail: detail}
	}
	switch {
	case !ok:
		return invalid("grain", "grain is month, quarter or year")
	case s.Method != MovingAverage && s.Method != SamePeriodLastYear:
		return invalid("method", "method is moving_average or same_period_last_year")
	case (s.Window > 0) != (s.Method == MovingAverage):
		return invalid("window", "window is given for moving_average alone")
	case s.Method == SamePeriodLastYear && s.Ahead > g.perYear:
		return invalid("periods_ahead", fmt.Sprintf("same_period_last_year forecasts at most a year ahead: %d %ss", g.perYear, s.Grain))
	}
	if met := m.Metric(s.Metric); met != nil && met.Type == Ratio {
		return &ForecastError{Code: ForecastRatio, Field: "metric", Detail: s.Metric + " is a ratio: the average of a ratio is not the ratio of its averages, so it is not forecast."}
	}
	return nil
}

// Query is the history's query: the metric by fiscal year and period.
func (s ForecastSpec) Query() Query {
	by := []string{"fiscal_year"}
	if d := grains[s.Grain].dim; d != "" {
		by = append(by, d)
	}
	return Query{Measures: []string{s.Metric}, By: by, Where: s.Where}
}

// Period is a fiscal period: a year and its month or quarter (0 for a
// year).
type Period struct{ Year, Number int }

// Point is a period's figure; Data says whether a history's period had
// data (one without is zero).
type Point struct {
	Period Period
	Value  Value
	Data   bool
}

// Forecast is a metric's history and its forecast (ADR-0076 §8): an
// arithmetic projection, stored nowhere.
type Forecast struct {
	Spec            ForecastSpec
	Label           string
	Type            MetricType
	Currency        string
	History         []Point // the first period with data through the last
	Projection      []Point // the periods after the history
	Used            []Period
	HistoryTotal    Value
	ProjectionTotal Value
}

// NewForecast forecasts the metric from its history, res (s.Query()'s
// result): every period from the first with data through the last, one
// without data counting as zero.
func NewForecast(m *Model, s ForecastSpec, res *Result) (*Forecast, error) {
	met := m.Metric(s.Metric)
	f := &Forecast{Spec: s, Label: met.Label, Type: met.Type}
	history, zero, err := series(s, res)
	if err != nil {
		return nil, err
	}
	f.History, f.Currency = history, zero.Currency
	if f.Projection, f.Used, err = project(s, history, zero); err != nil {
		return nil, err
	}
	if f.HistoryTotal, err = total(history, zero); err != nil {
		return nil, err
	}
	if f.ProjectionTotal, err = total(f.Projection, zero); err != nil {
		return nil, err
	}
	return f, nil
}

// series reads the history's periods from the result's rows, in order,
// with the periods between them that had no data as zero; zero is the
// metric's zero, of its currency and digits. No period with data is too
// little history for any method.
func series(s ForecastSpec, res *Result) ([]Point, Value, error) {
	if res.Truncated { // the earliest periods alone: a forecast from them would project the past
		return nil, Value{}, &ForecastError{Code: ForecastCurrencies, Detail: "The history has more periods than a query answers (500); filter it to fewer."}
	}
	byIndex := map[int]Point{}
	var zero Value
	for _, row := range res.Rows {
		p, err := periodOf(row)
		if err != nil {
			return nil, Value{}, err
		}
		v, ok := row[len(row)-1].(Value)
		if !ok { // a measure that cannot be computed: no data
			continue
		}
		if len(byIndex) > 0 && v.sameKind(zero) != nil {
			return nil, Value{}, &ForecastError{Code: ForecastCurrencies, Detail: fmt.Sprintf(
				"%s's history is in %s and %s, which a forecast never adds together; filter it to one company or currency.", s.Metric, zero.kind(), v.kind())}
		}
		zero = Value{Scale: v.Scale, Currency: v.Currency}
		byIndex[p.index(s.Grain)] = Point{Period: p, Value: v, Data: true}
	}
	if len(byIndex) == 0 {
		return nil, Value{}, &ForecastError{Code: ForecastHistoryShort, Detail: s.Metric + " has no history to forecast from: no period has data."}
	}
	indexes := slices.Sorted(maps.Keys(byIndex))
	var out []Point
	for i := indexes[0]; i <= indexes[len(indexes)-1]; i++ {
		pt, ok := byIndex[i]
		if !ok {
			pt = Point{Period: periodAt(s.Grain, i), Value: zero}
		}
		out = append(out, pt)
	}
	return out, zero, nil
}

// periodOf reads a row's fiscal year and its month or quarter, which the
// semantic layer writes as decimal numbers.
func periodOf(row Row) (Period, error) {
	var p Period
	for i, cell := range row[:len(row)-1] {
		s, _ := cell.(string) // a dimension's value is text
		n, err := strconv.Atoi(s)
		if err != nil {
			return Period{}, fmt.Errorf("analytics: forecast: the fiscal period %q is no number", s)
		}
		if i == 0 {
			p.Year = n
		} else {
			p.Number = n
		}
	}
	return p, nil
}

// index numbers a period consecutively across years.
func (p Period) index(grain string) int {
	g := grains[grain]
	if g.perYear == 1 {
		return p.Year
	}
	return p.Year*g.perYear + p.Number - 1
}

// periodAt is the period index numbers.
func periodAt(grain string, i int) Period {
	g := grains[grain]
	if g.perYear == 1 {
		return Period{Year: i}
	}
	return Period{Year: i / g.perYear, Number: i%g.perYear + 1}
}

// Label names the period: 2026-09 a month, 2026-Q3 a quarter, 2026 a
// year.
func (p Period) Label(grain string) string {
	switch grain {
	case "month":
		return fmt.Sprintf("%d-%02d", p.Year, p.Number)
	case "quarter":
		return fmt.Sprintf("%d-Q%d", p.Year, p.Number)
	}
	return strconv.Itoa(p.Year)
}

// project forecasts the periods after the history by the method and
// names the history's periods it read.
func project(s ForecastSpec, history []Point, zero Value) ([]Point, []Period, error) {
	last := history[len(history)-1].Period.index(s.Grain)
	first := history[0].Period.index(s.Grain)
	if s.Method == MovingAverage {
		return movingAverage(s, history, zero, last)
	}
	// The same period a year before: the earliest the forecast reads is a
	// year before its first period.
	perYear := grains[s.Grain].perYear
	if from := last + 1 - perYear; from < first {
		return nil, nil, short(s, history, periodAt(s.Grain, from), periodAt(s.Grain, last+s.Ahead-perYear))
	}
	var out []Point
	var used []Period
	for i := last + 1; i <= last+s.Ahead; i++ {
		before := history[i-perYear-first]
		out = append(out, Point{Period: periodAt(s.Grain, i), Value: before.Value})
		used = append(used, before.Period)
	}
	return out, used, nil
}

// movingAverage forecasts every period after the history as the average
// of its last Window periods, rounded half away from zero to the metric's
// digits.
func movingAverage(s ForecastSpec, history []Point, zero Value, last int) ([]Point, []Period, error) {
	if len(history) < s.Window {
		return nil, nil, short(s, history, periodAt(s.Grain, last+1-s.Window), periodAt(s.Grain, last))
	}
	window := history[len(history)-s.Window:]
	sum, err := total(window, zero)
	if err != nil {
		return nil, nil, err
	}
	// Divide fails only for a divisor below one, which Check refuses, or
	// for the most negative int64 divided by one, which money.Sum never
	// returns: it refuses it as out of range.
	units, _ := money.Divide(sum.Units, int64(s.Window))
	var out []Point
	for i := last + 1; i <= last+s.Ahead; i++ {
		out = append(out, Point{Period: periodAt(s.Grain, i), Value: Value{Units: units, Scale: zero.Scale, Currency: zero.Currency}})
	}
	used := make([]Period, len(window))
	for i, pt := range window {
		used[i] = pt.Period
	}
	return out, used, nil
}

// short refuses a forecast whose method needs the periods from to through
// and whose history does not reach back to from.
func short(s ForecastSpec, history []Point, from, through Period) error {
	return &ForecastError{Code: ForecastHistoryShort, Field: "method", Detail: fmt.Sprintf(
		"%s needs %s from %s to %s; its history runs from %s to %s.", s.Method, s.Metric, from.Label(s.Grain), through.Label(s.Grain),
		history[0].Period.Label(s.Grain), history[len(history)-1].Period.Label(s.Grain))}
}

// total adds the points' figures exactly.
func total(points []Point, zero Value) (Value, error) {
	vals := []Value{zero}
	for _, pt := range points {
		vals = append(vals, pt.Value)
	}
	sum, _, err := sumValues(vals)
	if err != nil {
		return Value{}, fmt.Errorf("analytics: forecast: %w", err)
	}
	return sum, nil
}

// Basis says what the forecast is: an arithmetic projection by the
// method named over the periods it used.
func (f *Forecast) Basis() string {
	g := f.Spec.Grain
	from, through := f.Used[0].Label(g), f.Used[len(f.Used)-1].Label(g)
	if f.Spec.Method == MovingAverage {
		return fmt.Sprintf("An arithmetic projection, not a statistical forecast: each period is the average of the last %d %ss of history (%s to %s), rounded half away from zero.",
			f.Spec.Window, g, from, through)
	}
	return fmt.Sprintf("An arithmetic projection, not a statistical forecast: each period is the same %s a fiscal year before (%s to %s), unchanged.",
		g, from, through)
}

// UsedLabels are the labels of the periods the method read.
func (f *Forecast) UsedLabels() []string {
	out := make([]string, len(f.Used))
	for i, p := range f.Used {
		out[i] = p.Label(f.Spec.Grain)
	}
	return out
}
