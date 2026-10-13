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
	if err := validateQuery(m, q); err != nil {
		return nil, err
	}
	agg := aggregateMetrics(m, f, p, q)
	res := &Result{ID: QueryID(q, p), Query: q, AsOf: f.AsOf, DataThrough: f.DataThrough}
	measureCol := describeColumns(m, q, res)
	if err := fillRows(m, q, agg, res, measureCol); err != nil {
		return nil, err
	}
	orderRows(q, res)
	limitRows(q, res)
	return res, nil
}

// validateQuery rejects a query before any facts are read: unknown
// metrics, dimensions or filters the measures do not share, malformed
// filters and an order_by that names no column.
func validateQuery(m *Model, q Query) error {
	if len(q.Measures) == 0 {
		return fmt.Errorf("query needs at least one measure")
	}
	allowed, err := sharedDimensions(m, q.Measures)
	if err != nil {
		return err
	}
	for _, d := range q.By {
		if !allowed[d] {
			return fmt.Errorf("dimension %q is not available for %s", d, strings.Join(q.Measures, ", "))
		}
	}
	for _, fl := range q.Where {
		if !allowed[fl.Dim] {
			return fmt.Errorf("cannot filter %s by %q", strings.Join(q.Measures, ", "), fl.Dim)
		}
		if err := fl.validate(); err != nil {
			return err
		}
	}
	if q.OrderBy != "" && !slices.Contains(q.By, q.OrderBy) && !slices.Contains(q.Measures, q.OrderBy) {
		return fmt.Errorf("order_by %q must be one of the measures or dimensions", q.OrderBy)
	}
	return nil
}

// sharedDimensions is the set of dimensions every measure can be broken
// down by: one breakdown applies to all measures of a query.
func sharedDimensions(m *Model, measures []string) (map[string]bool, error) {
	allowed := map[string]bool{}
	for i, name := range measures {
		if m.Metric(name) == nil {
			return nil, fmt.Errorf("unknown metric %q (use list_metrics)", name)
		}
		dims := m.AllowedDimensions(name)
		if i == 0 {
			for _, d := range dims {
				allowed[d] = true
			}
			continue
		}
		for d := range allowed {
			if !slices.Contains(dims, d) {
				delete(allowed, d)
			}
		}
	}
	return allowed, nil
}

// aggCell accumulates one base metric for one group. Currencies are
// tracked so money from companies with different currencies is never
// added together.
type aggCell struct {
	sum        int64
	ids        map[string]bool
	currencies map[string]bool
}

// aggregation holds every base metric aggregated per group.
type aggregation struct {
	groups map[string][]string // key -> dim values
	base   map[string]map[string]*aggCell
}

// aggregateMetrics computes every base metric the measures need, once each, per
// group of the query's dimensions.
func aggregateMetrics(m *Model, f *Facts, p Principal, q Query) *aggregation {
	agg := &aggregation{groups: map[string][]string{}, base: map[string]map[string]*aggCell{}}
	for _, name := range q.Measures {
		for _, b := range m.baseMetrics(name) {
			if agg.base[b] != nil {
				continue
			}
			agg.base[b] = map[string]*aggCell{}
			agg.addMetric(b, m.Metric(b), f, p, q)
		}
	}
	return agg
}

// addMetric folds the facts the principal may see and the filters select
// into the groups of base metric b.
func (agg *aggregation) addMetric(b string, met *Metric, f *Facts, p Principal, q Query) {
	for _, fact := range f.Tables[met.Source] {
		if !p.canSee(fact.Dims["company"]) || !matchAll(fact.Dims, met.Filter) || !matchQuery(fact.Dims, q.Where) {
			continue
		}
		key := agg.group(fact, q.By)
		c := agg.base[b][key]
		if c == nil {
			c = &aggCell{ids: map[string]bool{}, currencies: map[string]bool{}}
			agg.base[b][key] = c
		}
		c.currencies[fact.Currency] = true
		if met.Aggregate == "count_distinct" {
			c.ids[fact.ID] = true
		} else {
			c.sum += fact.Amount * met.Sign
		}
	}
}

// group registers the fact's group and returns its key.
func (agg *aggregation) group(fact Fact, by []string) string {
	vals := make([]string, len(by))
	for i, d := range by {
		vals[i] = fact.Dims[d]
	}
	key := strings.Join(vals, "\x1f")
	agg.groups[key] = vals
	return key
}

// describeColumns adds the dimension and measure columns and the lineage
// of every metric involved, and returns each measure's column index.
func describeColumns(m *Model, q Query, res *Result) map[string]int {
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
	return measureCol
}

// fillRows builds one row per group in key order, then labels each money
// column with its currency when exactly one currency contributed to it.
func fillRows(m *Model, q Query, agg *aggregation, res *Result, measureCol map[string]int) error {
	keys := make([]string, 0, len(agg.groups))
	for k := range agg.groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	colCurrency := map[string]map[string]bool{}
	for _, key := range keys {
		row := make(Row, len(res.Columns))
		for i, v := range agg.groups[key] {
			row[i] = v
		}
		for _, name := range q.Measures {
			if err := fillMeasure(m, agg, key, name, row, measureCol[name], colCurrency); err != nil {
				return err
			}
		}
		res.Rows = append(res.Rows, row)
	}
	for name, i := range measureCol {
		if c := sortedSet(colCurrency[name]); len(c) == 1 && m.Metric(name).Type == Money {
			res.Columns[i].Currency = c[0]
		}
	}
	return nil
}

// fillMeasure evaluates one measure for one group into row[col], refusing
// to add amounts in different currencies, and records the currencies seen
// for the column.
func fillMeasure(m *Model, agg *aggregation, key, name string, row Row, col int, colCurrency map[string]map[string]bool) error {
	ev := &cellEval{m: m, base: agg.base, key: key, curs: map[string]bool{}, summed: map[string]bool{}}
	v, ok := ev.eval(name)
	met := m.Metric(name)
	if len(ev.summed) > 1 {
		return fmt.Errorf("%s would add amounts in %s together; break down by company or filter to one company",
			name, strings.Join(sortedSet(ev.summed), " and "))
	}
	if colCurrency[name] == nil {
		colCurrency[name] = map[string]bool{}
	}
	for c := range ev.curs {
		colCurrency[name][c] = true
	}
	if ok {
		if met.Type == Ratio {
			row[col] = round(v, 4)
		} else {
			row[col] = round(v, 2)
		}
	}
	return nil
}

// cellEval evaluates a metric for one group, collecting the currencies of
// every base metric it reads so the caller can refuse mixed currencies.
// summed holds only the currencies of sum aggregates: fact amounts are
// minor units of the fact's currency (facts.go), so a sum is an amount
// whatever the metric's type, while a distinct count is not. A cell is
// refused when its sums read several currencies, whether one sum spans
// them or each operand of a derived metric is in its own; distinct
// counts, and ratios of them, may span currencies.
type cellEval struct {
	m      *Model
	base   map[string]map[string]*aggCell
	key    string
	curs   map[string]bool
	summed map[string]bool
}

func (ev *cellEval) eval(n string) (float64, bool) {
	met := ev.m.Metric(n)
	if met.Derived != nil {
		return ev.derived(met.Derived)
	}
	c := ev.base[n][ev.key]
	if c == nil {
		return 0, false
	}
	for cur := range c.currencies {
		ev.curs[cur] = true
	}
	if met.Aggregate == "count_distinct" {
		return float64(len(c.ids)), true
	}
	for cur := range c.currencies {
		ev.summed[cur] = true
	}
	// With several currencies the caller refuses the cell, so the scale of
	// the first one only keeps the value deterministic.
	scale := math.Pow(10, float64(currencyDecimals(sortedSet(c.currencies)[0])))
	return float64(c.sum) / scale, true
}

// derived evaluates both inputs first, so both contribute currencies even
// when the result is undefined: a ratio needs both inputs and a non-zero
// denominator, a difference needs only one side.
func (ev *cellEval) derived(d *Derived) (float64, bool) {
	l, okL := ev.eval(d.Left)
	r, okR := ev.eval(d.Right)
	if d.Op == "divide" {
		if !okL || !okR || r == 0 {
			return 0, false
		}
		return l / r, true
	}
	return l - r, okL || okR
}

// orderRows sorts stably by the order_by column, so ties keep group order.
func orderRows(q Query, res *Result) {
	if q.OrderBy == "" {
		return
	}
	idx := slices.IndexFunc(res.Columns, func(c Column) bool { return c.Name == q.OrderBy })
	sort.SliceStable(res.Rows, func(a, b int) bool {
		less := compareCells(res.Rows[a][idx], res.Rows[b][idx]) < 0
		if q.Desc {
			return compareCells(res.Rows[b][idx], res.Rows[a][idx]) < 0
		}
		return less
	})
}

// limitRows caps the rows at the query's limit (at most maxRows); the
// result is marked truncated only when the cap, not the caller, cut it.
func limitRows(q Query, res *Result) {
	limit := q.Limit
	if limit <= 0 || limit > maxRows {
		limit = maxRows
	}
	if len(res.Rows) > limit {
		res.Rows = res.Rows[:limit]
		res.Truncated = q.Limit == 0 || q.Limit > maxRows
	}
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
