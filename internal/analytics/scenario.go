package analytics

import (
	"fmt"
	"math/big"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// A scenario on a forecast (ADR-0077 §8, T97b): percentage changes of the
// rows of named dimension values. The history is read by period and by
// every dimension an adjustment names; the rows of each adjustment's
// value are one part, the rows of none another, and each part is forecast
// by the method on its own. The baseline is the parts' forecasts summed,
// the scenario the same with each adjusted part changed by its percentage,
// rounded half away from zero to the metric's digits, so both add up.
// Nothing is stored.

// Adjustment changes the forecast of the rows whose dimension Dim is
// Value by Percent, a decimal ("10" is ten percent more, "-5" five
// percent less).
type Adjustment struct {
	Dim, Value, Percent string
}

// ForecastOverlap is a scenario two of whose adjustments would change one
// row.
const ForecastOverlap = "analytics.scenario_overlap"

// periodDims are the dimensions a forecast's periods are made of, which a
// scenario does not adjust.
var periodDims = map[string]bool{"fiscal_year": true, "fiscal_quarter": true, "fiscal_month": true, "posting_date": true}

// checkScenario refuses an adjustment the forecast cannot make: on a
// dimension of its periods or one the metric lacks, by a percentage that
// is no decimal or takes off more than all, or naming a value another
// adjustment names.
func (s ForecastSpec) checkScenario(m *Model) error {
	allowed := m.AllowedDimensions(s.Metric)
	seen := map[[2]string]int{}
	for i, a := range s.Scenario {
		field := fmt.Sprintf("scenario.adjustments[%d]", i)
		invalid := func(f, detail string) error {
			return &ForecastError{Code: ForecastInvalid, Field: field + f, Detail: detail}
		}
		switch {
		case periodDims[a.Dim]:
			return invalid(".dimension", a.Dim+" makes the forecast's periods; a scenario adjusts another dimension's values")
		case m.Metric(s.Metric) != nil && !slices.Contains(allowed, a.Dim):
			return invalid(".dimension", fmt.Sprintf("%s has no dimension %s; it has %s", s.Metric, a.Dim, strings.Join(allowed, ", ")))
		}
		pct, err := money.ParsePlain(strings.TrimPrefix(a.Percent, "-"))
		if err != nil || (strings.HasPrefix(a.Percent, "-") && pct.Cmp(big.NewRat(100, 1)) > 0) {
			return invalid(".percent", "a decimal percentage, -100 or more: "+a.Percent)
		}
		if j, ok := seen[[2]string{a.Dim, a.Value}]; ok {
			return overlap(s.Scenario, j, i)
		}
		seen[[2]string{a.Dim, a.Value}] = i
	}
	return nil
}

// overlap refuses adjustments i and j, which would both change a row.
func overlap(adj []Adjustment, i, j int) error {
	return &ForecastError{Code: ForecastOverlap, Field: fmt.Sprintf("scenario.adjustments[%d]", j), Detail: fmt.Sprintf(
		"Adjustments %d (%s %s) and %d (%s %s) would both change the same rows; a row takes one adjustment.",
		i, adj[i].Dim, adj[i].Value, j, adj[j].Dim, adj[j].Value)}
}

// scenarioDims are the distinct dimensions the adjustments name, in the
// order they are first named.
func (s ForecastSpec) scenarioDims() []string {
	var out []string
	for _, a := range s.Scenario {
		if !slices.Contains(out, a.Dim) {
			out = append(out, a.Dim)
		}
	}
	return out
}

// split reads a scenario's result (by period and by the scenario's
// dimensions) as the history's result by period alone, and each row's
// figure by part and period: part i the rows of adjustment i, the last
// part the rows of none. A row two adjustments would change is refused.
func (s ForecastSpec) split(res *Result) (*Result, []map[Period]Value, error) {
	dims := s.scenarioDims()
	n := len(s.Query().By) - len(dims) // the period's dimensions lead each row
	parts := make([]map[Period]Value, len(s.Scenario)+1)
	for i := range parts {
		parts[i] = map[Period]Value{}
	}
	total := &Result{Truncated: res.Truncated}
	byPeriod := map[Period]int{}
	for _, row := range res.Rows {
		v, ok := row[len(row)-1].(Value)
		if !ok { // a measure that cannot be computed: no data
			continue
		}
		cells := append(slices.Clone(row[:n]), v)
		p, err := periodOf(cells)
		if err != nil {
			return nil, nil, err
		}
		part, err := s.partOf(row[n:len(row)-1], dims)
		if err != nil {
			return nil, nil, err
		}
		cur, had := parts[part][p]
		if parts[part][p], err = s.add(cur, had, v); err != nil {
			return nil, nil, err
		}
		if i, seen := byPeriod[p]; seen {
			sum, _ := total.Rows[i][n].(Value) // written as a Value below
			if total.Rows[i][n], err = s.add(sum, true, v); err != nil {
				return nil, nil, err
			}
			continue
		}
		byPeriod[p] = len(total.Rows)
		total.Rows = append(total.Rows, cells)
	}
	return total, parts, nil
}

// partOf is the part a row's values of the scenario's dimensions put it
// in.
func (s ForecastSpec) partOf(cells Row, dims []string) (int, error) {
	part := len(s.Scenario)
	for i, a := range s.Scenario {
		if cells[slices.Index(dims, a.Dim)] != a.Value {
			continue
		}
		if part != len(s.Scenario) {
			return 0, overlap(s.Scenario, part, i)
		}
		part = i
	}
	return part, nil
}

// add is cur, when there is one (ok), with v added, refused as a history
// in two currencies is.
func (s ForecastSpec) add(cur Value, ok bool, v Value) (Value, error) {
	if !ok {
		return v, nil
	}
	if err := cur.sameKind(v); err != nil {
		return Value{}, &ForecastError{Code: ForecastCurrencies, Detail: fmt.Sprintf(
			"%s's history is in %s and %s, which a forecast never adds together; filter it to one company or currency.", s.Metric, cur.kind(), v.kind())}
	}
	sum, _, err := sumValues([]Value{cur, v})
	if err != nil {
		return Value{}, fmt.Errorf("analytics: forecast: %w", err)
	}
	return sum, nil
}

// ScenarioPart is an adjustment's part of a scenario: its rows' forecast
// summed over the periods ahead, before and after its change.
type ScenarioPart struct {
	Adjustment              Adjustment
	BaselineTotal, Adjusted Value
}

// ScenarioPoint is a period ahead's baseline, scenario and their
// difference.
type ScenarioPoint struct {
	Period                         Period
	Baseline, Scenario, Difference Value
}

// Scenario is a forecast's scenario: per period ahead and in total.
type Scenario struct {
	Parts                                         []ScenarioPart
	Points                                        []ScenarioPoint
	BaselineTotal, ScenarioTotal, DifferenceTotal Value
}

// scenario forecasts each part over the history's periods and sums them:
// the baseline, the scenario and their difference per period ahead.
func (f *Forecast) scenario(history []Point, zero Value, parts []map[Period]Value) error {
	s := f.Spec
	sc := &Scenario{}
	base, adjusted := make([][]Value, s.Ahead), make([][]Value, s.Ahead)
	for i, part := range parts {
		points := make([]Point, len(history))
		for j, h := range history {
			v, ok := part[h.Period]
			if !ok {
				v = zero
			}
			points[j] = Point{Period: h.Period, Value: v}
		}
		projected, used, err := project(s, points, zero)
		if err != nil {
			return err
		}
		f.Used = used
		changed, err := adjust(s, i, projected)
		if err != nil {
			return err
		}
		for j := range projected {
			base[j], adjusted[j] = append(base[j], projected[j].Value), append(adjusted[j], changed[j].Value)
		}
		if i < len(s.Scenario) {
			p := ScenarioPart{Adjustment: s.Scenario[i]}
			if p.BaselineTotal, err = total(projected, zero); err == nil {
				p.Adjusted, err = total(changed, zero)
			}
			if err != nil {
				return err
			}
			sc.Parts = append(sc.Parts, p)
		}
	}
	return f.sumScenario(sc, base, adjusted, zero)
}

// adjust is the part's forecast changed by its adjustment's percentage,
// unchanged for the rows of none.
func adjust(s ForecastSpec, part int, points []Point) ([]Point, error) {
	if part == len(s.Scenario) {
		return points, nil
	}
	out := make([]Point, len(points))
	for i, pt := range points {
		// The percentage is checked; a result beyond an amount fails.
		by, err := money.Scale(pt.Value.Units, s.Scenario[part].Percent, 100)
		if err == nil {
			pt.Value.Units, err = money.Sum(pt.Value.Units, by)
		}
		if err != nil {
			return nil, fmt.Errorf("analytics: scenario: %w", err)
		}
		out[i] = pt
	}
	return out, nil
}

// sumScenario adds the parts up per period ahead: the forecast is the
// baseline.
func (f *Forecast) sumScenario(sc *Scenario, base, adjusted [][]Value, zero Value) error {
	f.Projection = make([]Point, len(base))
	var bases, scenarios, diffs []Point
	for j := range base {
		b, _, err := sumValues(append([]Value{zero}, base[j]...))
		if err != nil {
			return fmt.Errorf("analytics: scenario: %w", err)
		}
		a, _, err := sumValues(append([]Value{zero}, adjusted[j]...))
		if err != nil {
			return fmt.Errorf("analytics: scenario: %w", err)
		}
		d, err := diff(a, b)
		if err != nil {
			return fmt.Errorf("analytics: scenario: %w", err)
		}
		p := periodAt(f.Spec.Grain, f.History[len(f.History)-1].Period.index(f.Spec.Grain)+j+1)
		f.Projection[j] = Point{Period: p, Value: b}
		sc.Points = append(sc.Points, ScenarioPoint{Period: p, Baseline: b, Scenario: a, Difference: d})
		bases, scenarios, diffs = append(bases, Point{Value: b}), append(scenarios, Point{Value: a}), append(diffs, Point{Value: d})
	}
	var err error
	if sc.BaselineTotal, err = total(bases, zero); err == nil {
		if sc.ScenarioTotal, err = total(scenarios, zero); err == nil {
			sc.DifferenceTotal, err = total(diffs, zero)
		}
	}
	f.Scenario = sc
	return err
}
