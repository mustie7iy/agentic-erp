package analytics

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Query is a semantic query: which metrics, broken down by which
// dimensions, filtered how. The agent writes these; it never writes SQL.
type Query struct {
	Measures []string `json:"measures"`
	By       []string `json:"by,omitempty"`
	Where    []Filter `json:"where,omitempty"`
	OrderBy  string   `json:"order_by,omitempty"`
	Desc     bool     `json:"desc,omitempty"`
	Limit    int      `json:"limit,omitempty"`
}

// Filter restricts a dimension. Op is one of: in, not_in, <=, >=, between.
type Filter struct {
	Dim    string   `json:"dim"`
	Op     string   `json:"op"`
	Values []string `json:"values"`
}

// Principal is who is asking. Queries only see the companies they may
// access; nil means all companies.
type Principal struct {
	ID        string   `json:"id"`
	Companies []string `json:"companies,omitempty"`
}

type Column struct {
	Name     string     `json:"name"`
	Label    string     `json:"label"`
	Kind     string     `json:"kind"` // dimension | measure
	Type     MetricType `json:"type,omitempty"`
	Currency string     `json:"currency,omitempty"`
}

// Row values: dimension values are strings; measures are float64 in major
// units (money), whole numbers (count), or fractions (ratio). A measure is
// nil when it cannot be computed (e.g. a ratio with a zero denominator).
type Row []any

type Result struct {
	ID          string          `json:"query_id"`
	Query       Query           `json:"query"`
	Columns     []Column        `json:"columns"`
	Rows        []Row           `json:"rows"`
	AsOf        time.Time       `json:"as_of"`
	DataThrough string          `json:"data_through"`
	Definitions []MetricLineage `json:"definitions"`
	Truncated   bool            `json:"truncated,omitempty"`
}

// MetricLineage records exactly which definition produced a measure.
type MetricLineage struct {
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Source      string              `json:"source,omitempty"`
	Filter      map[string][]string `json:"filter,omitempty"`
	Derived     *Derived            `json:"derived,omitempty"`
}

const maxRows = 500

// QueryID is stable for the same query and principal scope, so saved reports
// can refer to queries and rerun them later.
func QueryID(q Query, p Principal) string {
	b, _ := json.Marshal(struct {
		Q Query
		C []string
	}{q, p.Companies})
	sum := sha256.Sum256(b)
	return "q_" + hex.EncodeToString(sum[:6])
}

// Execute runs a semantic query against the facts with the principal's
// row-level security applied.
func Execute(m *Model, f *Facts, p Principal, q Query) (*Result, error) {
	if len(q.Measures) == 0 {
		return nil, fmt.Errorf("query needs at least one measure")
	}
	allowed := map[string]bool{}
	for i, name := range q.Measures {
		met := m.Metric(name)
		if met == nil {
			return nil, fmt.Errorf("unknown metric %q (use list_metrics)", name)
		}
		dims := m.AllowedDimensions(name)
		if i == 0 {
			for _, d := range dims {
				allowed[d] = true
			}
		} else {
			for d := range allowed {
				if !slices.Contains(dims, d) {
					delete(allowed, d)
				}
			}
		}
	}
	for _, d := range q.By {
		if !allowed[d] {
			return nil, fmt.Errorf("dimension %q is not available for %s", d, strings.Join(q.Measures, ", "))
		}
	}
	for _, fl := range q.Where {
		if !allowed[fl.Dim] {
			return nil, fmt.Errorf("cannot filter %s by %q", strings.Join(q.Measures, ", "), fl.Dim)
		}
		if err := fl.validate(); err != nil {
			return nil, err
		}
	}
	if q.OrderBy != "" && !slices.Contains(q.By, q.OrderBy) && !slices.Contains(q.Measures, q.OrderBy) {
		return nil, fmt.Errorf("order_by %q must be one of the measures or dimensions", q.OrderBy)
	}

	// Aggregate every base metric per group, tracking currencies so money
	// from companies with different currencies is never added together.
	type cell struct {
		sum        int64
		ids        map[string]bool
		currencies map[string]bool
		seen       bool
	}
	groups := map[string][]string{} // key -> dim values
	base := map[string]map[string]*cell{}
	for _, name := range q.Measures {
		for _, b := range m.baseMetrics(name) {
			if base[b] != nil {
				continue
			}
			base[b] = map[string]*cell{}
			met := m.Metric(b)
			for _, fact := range f.Tables[met.Source] {
				if !p.canSee(fact.Dims["company"]) || !matchAll(fact.Dims, met.Filter) || !matchQuery(fact.Dims, q.Where) {
					continue
				}
				vals := make([]string, len(q.By))
				for i, d := range q.By {
					vals[i] = fact.Dims[d]
				}
				key := strings.Join(vals, "\x1f")
				groups[key] = vals
				c := base[b][key]
				if c == nil {
					c = &cell{ids: map[string]bool{}, currencies: map[string]bool{}}
					base[b][key] = c
				}
				c.seen = true
				c.currencies[fact.Currency] = true
				if met.Aggregate == "count_distinct" {
					c.ids[fact.ID] = true
				} else {
					c.sum += fact.Amount * met.Sign
				}
			}
		}
	}

	res := &Result{ID: QueryID(q, p), Query: q, AsOf: f.AsOf, DataThrough: f.DataThrough}
	for _, d := range q.By {
		res.Columns = append(res.Columns, Column{Name: d, Label: m.dimLabel(d), Kind: "dimension"})
	}
	measureCol := map[string]int{}
	for _, name := range q.Measures {
		met := m.Metric(name)
		measureCol[name] = len(res.Columns)
		res.Columns = append(res.Columns, Column{Name: name, Label: met.Label, Kind: "measure", Type: met.Type})
		for _, b := range m.baseMetrics(name) {
			bm := m.Metric(b)
			res.Definitions = appendLineage(res.Definitions, MetricLineage{Name: bm.Name, Description: bm.Description, Source: bm.Source, Filter: bm.Filter})
		}
		if met.Derived != nil {
			res.Definitions = appendLineage(res.Definitions, MetricLineage{Name: met.Name, Description: met.Description, Derived: met.Derived})
		}
	}

	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	colCurrency := map[string]map[string]bool{}
	for _, key := range keys {
		row := make(Row, len(res.Columns))
		for i, v := range groups[key] {
			row[i] = v
		}
		for _, name := range q.Measures {
			curs := map[string]bool{}
			var eval func(n string) (float64, bool)
			eval = func(n string) (float64, bool) {
				met := m.Metric(n)
				if met.Derived != nil {
					l, okL := eval(met.Derived.Left)
					r, okR := eval(met.Derived.Right)
					if met.Derived.Op == "divide" {
						if !okL || !okR || r == 0 {
							return 0, false
						}
						return l / r, true
					}
					return l - r, okL || okR
				}
				c := base[n][key]
				if c == nil {
					return 0, false
				}
				for cur := range c.currencies {
					curs[cur] = true
				}
				if met.Aggregate == "count_distinct" {
					return float64(len(c.ids)), true
				}
				scale := 100.0
				for cur := range c.currencies {
					scale = math.Pow(10, float64(currencyDecimals(cur)))
				}
				return float64(c.sum) / scale, true
			}
			v, ok := eval(name)
			met := m.Metric(name)
			if len(curs) > 1 && met.Type != Count {
				return nil, fmt.Errorf("%s would add amounts in %s together; break down by company or filter to one company",
					name, strings.Join(sortedSet(curs), " and "))
			}
			if colCurrency[name] == nil {
				colCurrency[name] = map[string]bool{}
			}
			for c := range curs {
				colCurrency[name][c] = true
			}
			if ok {
				if met.Type == Ratio {
					row[measureCol[name]] = round(v, 4)
				} else {
					row[measureCol[name]] = round(v, 2)
				}
			}
		}
		res.Rows = append(res.Rows, row)
	}
	for name, i := range measureCol {
		if c := sortedSet(colCurrency[name]); len(c) == 1 && m.Metric(name).Type == Money {
			res.Columns[i].Currency = c[0]
		}
	}

	if q.OrderBy != "" {
		idx := slices.IndexFunc(res.Columns, func(c Column) bool { return c.Name == q.OrderBy })
		sort.SliceStable(res.Rows, func(a, b int) bool {
			less := compareCells(res.Rows[a][idx], res.Rows[b][idx]) < 0
			if q.Desc {
				return compareCells(res.Rows[b][idx], res.Rows[a][idx]) < 0
			}
			return less
		})
	}
	limit := q.Limit
	if limit <= 0 || limit > maxRows {
		limit = maxRows
	}
	if len(res.Rows) > limit {
		res.Rows = res.Rows[:limit]
		res.Truncated = q.Limit == 0 || q.Limit > maxRows
	}
	return res, nil
}

func (p Principal) canSee(company string) bool {
	return p.Companies == nil || slices.Contains(p.Companies, company)
}

func (fl Filter) validate() error {
	switch fl.Op {
	case "in", "not_in":
		if len(fl.Values) == 0 {
			return fmt.Errorf("filter on %s needs at least one value", fl.Dim)
		}
	case "<=", ">=":
		if len(fl.Values) != 1 {
			return fmt.Errorf("filter %s %s needs exactly one value", fl.Dim, fl.Op)
		}
	case "between":
		if len(fl.Values) != 2 {
			return fmt.Errorf("filter %s between needs two values", fl.Dim)
		}
	default:
		return fmt.Errorf("unknown filter operator %q (use in, not_in, <=, >=, between)", fl.Op)
	}
	return nil
}

func matchAll(dims map[string]string, filter map[string][]string) bool {
	for d, vals := range filter {
		if !slices.Contains(vals, dims[d]) {
			return false
		}
	}
	return true
}

func matchQuery(dims map[string]string, where []Filter) bool {
	for _, fl := range where {
		v := dims[fl.Dim]
		switch fl.Op {
		case "in":
			if !slices.Contains(fl.Values, v) {
				return false
			}
		case "not_in":
			if slices.Contains(fl.Values, v) {
				return false
			}
		case "<=":
			if compareValues(v, fl.Values[0]) > 0 {
				return false
			}
		case ">=":
			if compareValues(v, fl.Values[0]) < 0 {
				return false
			}
		case "between":
			if compareValues(v, fl.Values[0]) < 0 || compareValues(v, fl.Values[1]) > 0 {
				return false
			}
		}
	}
	return true
}

// compareValues compares numerically when both sides are numbers (months,
// years), otherwise as strings (dates, codes).
func compareValues(a, b string) int {
	fa, errA := strconv.ParseFloat(a, 64)
	fb, errB := strconv.ParseFloat(b, 64)
	if errA == nil && errB == nil {
		switch {
		case fa < fb:
			return -1
		case fa > fb:
			return 1
		}
		return 0
	}
	return strings.Compare(a, b)
}

func compareCells(a, b any) int {
	switch x := a.(type) {
	case float64:
		y, ok := b.(float64)
		if !ok {
			return 1
		}
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	case string:
		y, _ := b.(string)
		return compareValues(x, y)
	}
	if b == nil {
		return 0
	}
	return -1
}

func appendLineage(list []MetricLineage, l MetricLineage) []MetricLineage {
	if slices.ContainsFunc(list, func(x MetricLineage) bool { return x.Name == l.Name }) {
		return list
	}
	return append(list, l)
}

func (m *Model) dimLabel(name string) string {
	for _, d := range m.Dimensions {
		if d.Name == name {
			return d.Label
		}
	}
	return name
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

// currencyDecimals gives the minor-unit exponent (ISO 4217) for the
// currencies that differ from the default of 2.
func currencyDecimals(cur string) int {
	switch cur {
	case "JPY", "KRW", "CLP", "ISK", "VND":
		return 0
	case "BHD", "KWD", "OMR", "JOD", "TND":
		return 3
	}
	return 2
}
