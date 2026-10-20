package analytics

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
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
	// Ledger is the ledger journal metrics read: the leading ledger when
	// empty, another (its id) only when the question names it, so a sum
	// never adds a parallel ledger's lines to the leading one's (ADR-0044).
	Ledger string `json:"ledger,omitempty"`
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

// Row values: dimension values are strings; measures are Values, exact
// decimals: amounts at their currency's exponent, whole counts, ratios at
// ratioScale digits. A measure is nil when it cannot be computed (e.g. a
// ratio with a zero denominator).
type Row []any

type Result struct {
	ID      string   `json:"query_id"`
	Query   Query    `json:"query"`
	Columns []Column `json:"columns"`
	Rows    []Row    `json:"rows"`
	// Source and AsOf are the answer's lineage: the analytical store
	// (SourceStore) as of its manifest, or the database (SourceDatabase)
	// as of the read.
	Source      string          `json:"source,omitempty"`
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
	// A Query and company ids are plain data: they always encode.
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
	agg, err := aggregateMetrics(m, f, p, q)
	if err != nil {
		return nil, err
	}
	res := &Result{ID: QueryID(q, p), Query: q, Source: f.Source, AsOf: f.AsOf, DataThrough: f.DataThrough}
	measureCol := describeColumns(m, q, res)
	if err := fillRows(m, q, agg, res, measureCol); err != nil {
		return nil, err
	}
	orderRows(q, res)
	limitRows(q, res)
	return res, nil
}

// Validate rejects a query the model cannot answer before any data is
// read: erp-server answers it 422 rather than reading facts for it.
func Validate(m *Model, q Query) error { return validateQuery(m, q) }

// validateQuery rejects a query before any facts are read: unknown
// metrics, dimensions or filters the measures do not share, malformed
// filters and an order_by that names no column.
func validateQuery(m *Model, q Query) error {
	if len(q.Measures) == 0 {
		return fmt.Errorf("query needs at least one measure")
	}
	// The canonical form alone, so both paths compare the same string.
	if id, err := uuid.Parse(q.Ledger); q.Ledger != "" && (err != nil || id.String() != q.Ledger) {
		return fmt.Errorf("ledger %q is not a ledger id", q.Ledger)
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
// added together. A sum keeps its terms until every fact is read, so it
// is exact whatever their order (money.Sum).
type aggCell struct {
	terms      []money.Minor
	sum        money.Minor
	ids        map[string]bool
	count      int64 // documents counted by the store (Facts.Aggregated)
	currencies map[string]bool
}

// aggregation holds every base metric aggregated per group.
type aggregation struct {
	groups    map[string][]string // key -> dim values
	base      map[string]map[string]*aggCell
	exponents map[string]int // currency -> minor-unit digits (Facts.Exponents)
}

// aggregateMetrics computes every base metric the measures need, once each, per
// group of the query's dimensions. A sum out of the range of an amount
// refuses the query (money.ErrOverflow) rather than reporting it wrapped.
func aggregateMetrics(m *Model, f *Facts, p Principal, q Query) (*aggregation, error) {
	agg := &aggregation{groups: map[string][]string{}, base: map[string]map[string]*aggCell{}, exponents: f.Exponents}
	for _, name := range q.Measures {
		for _, b := range m.baseMetrics(name) {
			if agg.base[b] != nil {
				continue
			}
			agg.base[b] = map[string]*aggCell{}
			agg.addMetric(b, m.Metric(b), f, p, q)
			if err := agg.sum(b); err != nil {
				return nil, err
			}
		}
	}
	return agg, nil
}

// sum totals each group's terms of base metric b, in key order so the
// group named when several are out of range does not vary.
func (agg *aggregation) sum(b string) error {
	for _, key := range slices.Sorted(maps.Keys(agg.base[b])) {
		c := agg.base[b][key]
		total, err := money.Sum(c.terms...)
		if err != nil {
			group := "in total" // a query without a breakdown has one group, of no values
			if vals := agg.groups[key]; len(vals) > 0 {
				named := make([]string, len(vals))
				for i, v := range vals {
					named[i] = cmp.Or(v, "(blank)") // a fact without the dimension
				}
				group = "for " + strings.Join(named, ", ")
			}
			return fmt.Errorf("%s %s adds up to more than an amount can hold; filter or break it down further: %w", b, group, err)
		}
		c.sum, c.terms = total, nil
	}
	return nil
}

// addMetric folds the facts the principal may see and the filters select
// into the groups of base metric b.
// Groups the store aggregated (Facts.Aggregated) were selected by the
// store with the same filters and scope, and are folded in as they are.
func (agg *aggregation) addMetric(b string, met *Metric, f *Facts, p Principal, q Query) {
	if groups, ok := f.Aggregated[b]; ok {
		for _, g := range groups {
			agg.add(b, met, g, q.By, true)
		}
		return
	}
	for _, fact := range f.Tables[met.Source] {
		if !p.canSee(fact.Dims["company"]) || !inLedger(fact, q.Ledger) || !matchAll(fact.Dims, met.Filter) || !matchQuery(fact.Dims, q.Where) {
			continue
		}
		agg.add(b, met, fact, q.By, false)
	}
}

// add folds one fact, or one group the store aggregated, into its group.
func (agg *aggregation) add(b string, met *Metric, fact Fact, by []string, aggregated bool) {
	key := agg.group(fact, by)
	c := agg.base[b][key]
	if c == nil {
		c = &aggCell{ids: map[string]bool{}, currencies: map[string]bool{}}
		agg.base[b][key] = c
	}
	c.currencies[fact.Currency] = true
	if met.Aggregate == "count_distinct" {
		if aggregated {
			c.count += fact.Count
		} else {
			c.ids[fact.ID] = true
		}
		return
	}
	// The sign is ±1 (checkSourceMetric) and the range symmetric, so the
	// product is an amount.
	c.terms = append(c.terms, fact.Amount*money.Minor(met.Sign))
}

// inLedger reports whether a fact is in the query's ledger: the leading
// one unless the query names another. A fact without a ledger (an order,
// an invoice) is in every one.
func inLedger(fact Fact, ledger string) bool {
	switch {
	case fact.Ledger == "":
		return true
	case ledger == "":
		return fact.Leading
	}
	return fact.Ledger == ledger
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
// to add amounts in different currencies, and records the currencies of
// the column's amounts (not of documents a distinct count spans) to label
// it.
func fillMeasure(m *Model, agg *aggregation, key, name string, row Row, col int, colCurrency map[string]map[string]bool) error {
	ev := &cellEval{m: m, agg: agg, key: key, summed: map[string]bool{}}
	v, ok := ev.eval(name)
	if len(ev.summed) > 1 {
		return fmt.Errorf("%s would add amounts in %s together; break down by company or filter to one company",
			name, strings.Join(sortedSet(ev.summed), " and "))
	}
	if colCurrency[name] == nil {
		colCurrency[name] = map[string]bool{}
	}
	for c := range ev.summed {
		colCurrency[name][c] = true
	}
	if !ok {
		return nil
	}
	val, err := agg.value(m.Metric(name), v, ev.summed)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	row[col] = val
	return nil
}

// value keeps an exact result at its digits: a ratio at ratioScale, a value
// read from sums in one currency at that currency's exponent, a count
// whole. A quotient of amounts (a money metric derived by divide) is
// rounded half away from zero to the currency's digits.
func (agg *aggregation) value(met *Metric, v *big.Rat, summed map[string]bool) (Value, error) {
	scale, cur := 0, ""
	for c := range summed { // at most one: fillMeasure refuses more
		exp, known := agg.exponents[c]
		if !known {
			return Value{}, fmt.Errorf("the minor-unit digits of %s are unknown", c)
		}
		scale, cur = exp, c
	}
	if met.Type == Ratio {
		scale, cur = ratioScale, ""
	}
	units, err := money.Round(v, scale)
	if err != nil {
		return Value{}, fmt.Errorf("the value is out of range: %w", err)
	}
	return Value{Units: units, Scale: scale, Currency: cur}, nil
}

// cellEval evaluates a metric for one group, exactly and in major units,
// collecting in summed the currencies of the sum aggregates it reads, so
// the caller can refuse mixed currencies and label the column. Fact
// amounts are minor units of the fact's currency (facts.go), so a sum is
// an amount whatever the metric's type, while a distinct count is not. A
// cell is refused when its sums read several currencies, whether one sum
// spans them or each operand of a derived metric is in its own; distinct
// counts, and ratios of them, may span currencies.
type cellEval struct {
	m      *Model
	agg    *aggregation
	key    string
	summed map[string]bool
}

func (ev *cellEval) eval(n string) (*big.Rat, bool) {
	met := ev.m.Metric(n)
	if met.Derived != nil {
		return ev.derived(met.Derived)
	}
	c := ev.agg.base[n][ev.key]
	if c == nil {
		return nil, false
	}
	if met.Aggregate == "count_distinct" {
		return new(big.Rat).SetInt64(int64(len(c.ids)) + c.count), true
	}
	for cur := range c.currencies {
		ev.summed[cur] = true
	}
	// With several currencies the caller refuses the cell, and with an
	// unknown exponent it reports that, so the first currency's exponent
	// only keeps the value deterministic.
	cur := sortedSet(c.currencies)[0]
	return Value{Units: c.sum, Scale: ev.agg.exponents[cur]}.rat(), true
}

// derived evaluates both inputs first, so both contribute currencies even
// when the result is undefined: a ratio needs both inputs and a non-zero
// denominator, a difference needs only one side.
func (ev *cellEval) derived(d *Derived) (*big.Rat, bool) {
	l, okL := ev.eval(d.Left)
	r, okR := ev.eval(d.Right)
	if d.Op == "divide" {
		if !okL || !okR || r.Sign() == 0 {
			return nil, false
		}
		return new(big.Rat).Quo(l, r), true
	}
	if !okL && !okR {
		return nil, false
	}
	if !okL {
		l = new(big.Rat)
	}
	if !okR {
		r = new(big.Rat)
	}
	return new(big.Rat).Sub(l, r), true
}

// orderRows sorts stably by the order_by column, so ties keep group order.
func orderRows(q Query, res *Result) {
	if q.OrderBy == "" {
		return
	}
	idx := slices.IndexFunc(res.Columns, func(c Column) bool { return c.Name == q.OrderBy })
	sort.SliceStable(res.Rows, func(a, b int) bool {
		if q.Desc {
			return compareCells(res.Rows[b][idx], res.Rows[a][idx]) < 0
		}
		return compareCells(res.Rows[a][idx], res.Rows[b][idx]) < 0
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

// compareValues compares numerically when both sides are plain decimals
// (months, years, numeric codes), otherwise as strings (dates, codes). The
// numbers compare exactly: through a float, two codes of more than 15
// digits that differ in the last one would compare equal.
func compareValues(a, b string) int {
	ra, okA := plainDecimal(a)
	rb, okB := plainDecimal(b)
	if okA && okB {
		return ra.Cmp(rb)
	}
	return strings.Compare(a, b)
}

// plainDecimal reads an optional sign, digits and an optional "." with
// digits, exactly; anything else ("1e3", "0x10", "Inf", a date) is not a
// number here.
//
// It is deliberately stricter than money's decimal grammar (ParseDecimal,
// ParseDecimalRat), which reads input from SAP extracts: that grammar trims
// space and takes a trailing minus ("12-") and a bare "." side (".5",
// "5."). Here the operands are dimension values, mostly codes, so a code
// such as "12-" or " 12" must stay a string: read as -12 or 12 it would
// sort among the numbers and compare equal to a different value.
func plainDecimal(s string) (*big.Rat, bool) {
	body := s
	if body != "" && (body[0] == '-' || body[0] == '+') {
		body = body[1:]
	}
	intPart, frac, hasFrac := strings.Cut(body, ".")
	if !isDigits(intPart) || (hasFrac && !isDigits(frac)) {
		return nil, false
	}
	r, _ := new(big.Rat).SetString(s) // a sign, digits and at most one dot always parse
	return r, true
}

// isDigits reports whether s is one or more ASCII digits.
func isDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// compareCells orders cells: measure values by size, before the cells
// without one; dimension values as compareValues does.
func compareCells(a, b any) int {
	switch x := a.(type) {
	case Value:
		y, ok := b.(Value)
		if !ok {
			return 1
		}
		return x.compare(y)
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
