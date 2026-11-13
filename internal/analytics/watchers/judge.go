package watchers

import (
	"cmp"
	"maps"
	"math/big"
	"slices"
	"strconv"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// yearsBack is how many fiscal years before the evaluation's year a
// watcher's query reads, by grain: enough for the previous periods a
// condition compares with (ADR-0077 §2), the fiscal year being named
// after the calendar year it ends in.
var yearsBack = map[string]int{Month: 2, Year: 14}

// query is the semantic query an evaluation runs (ADR-0077 §2): the
// metric by company, by currency where the metric has that dimension, by
// fiscal year and, for the month grain, fiscal month, under the watcher's
// filters, from the fiscal year yearsBack before year.
func (w Watcher) query(m *analytics.Model, year int) analytics.Query {
	by := []string{"company"}
	if slices.Contains(m.AllowedDimensions(w.Metric), "currency") {
		by = append(by, "currency")
	}
	by = append(by, "fiscal_year")
	if w.Grain == Month {
		by = append(by, "fiscal_month")
	}
	where := append(slices.Clone(w.Filters), analytics.Filter{Dim: "fiscal_year", Op: ">=", Values: []string{strconv.Itoa(year - yearsBack[w.Grain])}})
	return analytics.Query{Measures: []string{w.Metric}, By: by, Where: where}
}

// period is a fiscal year and, for the month grain, its fiscal month (0
// for the year grain).
type period struct{ year, month int }

func (p period) compare(o period) int {
	return cmp.Or(cmp.Compare(p.year, o.year), cmp.Compare(p.month, o.month))
}

// before is the period n periods before p at its grain.
func (p period) before(n int) period {
	if p.month == 0 {
		return period{year: p.year - n}
	}
	i := p.year*12 + p.month - 1 - n
	return period{year: i / 12, month: i%12 + 1}
}

// key is a series: one company's figures in one currency ("" for a count
// or a ratio).
type key struct{ company, currency string }

// figures are an answer's figures by series and period.
type figures map[key]map[period]analytics.Value

// figuresOf reads an answer's rows (query's columns: company, currency
// where present, fiscal year, fiscal month for the month grain, the
// metric). A row without a value (a ratio of zero) or with a period that
// is not a number is left out.
func (w Watcher) figuresOf(res *analytics.Result) figures {
	out := figures{}
	for _, row := range res.Rows {
		v, ok := row[len(row)-1].(analytics.Value)
		if !ok {
			continue
		}
		dims := make([]string, len(row)-1)
		for i := range dims {
			dims[i], _ = row[i].(string) // a dimension's cell is its value (analytics.Row)
		}
		p, ok := w.periodOf(dims)
		if !ok {
			continue
		}
		k := key{company: dims[0], currency: v.Currency}
		if out[k] == nil {
			out[k] = map[period]analytics.Value{}
		}
		out[k][p] = v
	}
	return out
}

// periodOf reads the period from a row's dimensions, which end with the
// fiscal year and, for the month grain, the fiscal month.
func (w Watcher) periodOf(dims []string) (period, bool) {
	var p period
	var err error
	if w.Grain == Month {
		if p.month, err = strconv.Atoi(dims[len(dims)-1]); err != nil {
			return p, false
		}
		dims = dims[:len(dims)-1]
	}
	p.year, err = strconv.Atoi(dims[len(dims)-1])
	return p, err == nil
}

// latest is the latest period with a figure in any series; ok is false
// for an answer without one.
func (f figures) latest() (period, bool) {
	var at period
	found := false
	for _, s := range f {
		for p := range s {
			if !found || p.compare(at) > 0 {
				at, found = p, true
			}
		}
	}
	return at, found
}

// Judged is one series' figure in the period judged and whether it holds
// the condition: ComparedWith is empty when the series judged nothing,
// Percent empty but for a change judged.
type Judged struct {
	Company, Currency     string
	Figure                analytics.Value
	ComparedWith, Percent string
	Holds                 bool
}

// judge judges every series with a figure in the period at, by company
// and currency.
func (w Watcher) judge(f figures, at period) []Judged {
	out := []Judged{}
	for _, k := range slices.SortedFunc(maps.Keys(f), func(a, b key) int {
		return cmp.Or(cmp.Compare(a.company, b.company), cmp.Compare(a.currency, b.currency))
	}) {
		v, ok := f[k][at]
		if !ok {
			continue
		}
		j := Judged{Company: k.company, Currency: k.currency, Figure: v}
		switch w.Condition.Kind {
		case Above, Below:
			w.compare(&j)
		default:
			w.changed(&j, f[k], at)
		}
		out = append(out, j)
	}
	return out
}

// compare judges above and below: a figure in another currency than the
// value's is not compared.
func (w Watcher) compare(j *Judged) {
	c := w.Condition
	if j.Currency != c.Currency {
		return
	}
	value, _ := new(big.Rat).SetString(c.Value) // the database keeps it a decimal (metric_watcher_threshold_check)
	j.ComparedWith = c.Value
	order := rat(j.Figure).Cmp(value)
	j.Holds = (c.Kind == Above && order > 0) || (c.Kind == Below && order < 0)
}

// changed judges a change against the previous period's figure: none, or
// zero, judges nothing.
func (w Watcher) changed(j *Judged, s map[period]analytics.Value, at period) {
	base, ok := s[at.before(1)]
	if !ok || base.Units == 0 {
		return
	}
	j.ComparedWith = base.String()
	w.percentOf(j, rat(base))
}

// percentOf sets the change of the figure from base in percent of base's
// size, compared with the condition's percentage exactly; the one shown
// is rounded half away from zero to two decimals, and none is shown out of
// an amount's range.
func (w Watcher) percentOf(j *Judged, base *big.Rat) {
	pct := new(big.Rat).Sub(rat(j.Figure), base)
	pct.Quo(pct, new(big.Rat).Abs(base))
	pct.Mul(pct, big.NewRat(100, 1))
	limit, _ := new(big.Rat).SetString(w.Condition.Percent) // the database keeps it a decimal (metric_watcher_percent_check)
	j.Holds = new(big.Rat).Abs(pct).Cmp(limit) > 0
	if units, err := money.Round(pct, 2); err == nil {
		j.Percent = units.Plain(2)
	}
}

// rat is a figure as an exact rational.
func rat(v analytics.Value) *big.Rat { return money.Rat(v.Units, v.Scale) }
