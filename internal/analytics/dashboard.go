package analytics

import (
	"fmt"
	"slices"
	"strings"
)

// Spec is a dashboard the agent composes. It contains no numbers of its own:
// tiles, charts and tables point at query results by query_id, so every
// figure on the page is computed by the engine.
type Spec struct {
	Title       string   `json:"title"`
	Question    string   `json:"question,omitempty"`
	Assumptions []string `json:"assumptions,omitempty"`
	Tiles       []Tile   `json:"tiles,omitempty"`
	Charts      []Chart  `json:"charts,omitempty"`
	Tables      []Table  `json:"tables,omitempty"`
	Commentary  string   `json:"commentary,omitempty"`
	FollowUps   []string `json:"follow_ups,omitempty"`
}

// Tile shows one measure summed over the rows matching Where, optionally
// compared with the rows matching CompareWhere (the baseline).
type Tile struct {
	Label        string            `json:"label"`
	QueryID      string            `json:"query_id"`
	Measure      string            `json:"measure"`
	Where        map[string]string `json:"where,omitempty"`
	CompareWhere map[string]string `json:"compare_where,omitempty"`
	CompareLabel string            `json:"compare_label,omitempty"`
}

// Chart plots a measure along X, one series per value of Series (optional).
type Chart struct {
	Title   string `json:"title"`
	Type    string `json:"type"` // bar | line
	QueryID string `json:"query_id"`
	X       string `json:"x"`
	Series  string `json:"series,omitempty"`
	Measure string `json:"measure"`
}

// Table shows a query result. With Compare it pivots one dimension into
// baseline, current, change and change % columns.
type Table struct {
	Title   string   `json:"title"`
	QueryID string   `json:"query_id"`
	Compare *Compare `json:"compare,omitempty"`
}

type Compare struct {
	Dim      string `json:"dim"`
	Baseline string `json:"baseline"`
	Current  string `json:"current"`
	Measure  string `json:"measure"`
}

// Validate checks that every reference resolves and that the commentary
// only cites numbers the results support.
func (s *Spec) Validate(results map[string]*Result) []string {
	v := &specValidator{results: results}
	if strings.TrimSpace(s.Title) == "" {
		v.bad("title is required")
	}
	if len(s.Tiles)+len(s.Charts)+len(s.Tables) == 0 {
		v.bad("add at least one tile, chart or table")
	}
	for i, t := range s.Tiles {
		v.tile(i, t)
	}
	for i, c := range s.Charts {
		v.chart(i, c)
	}
	for i, t := range s.Tables {
		v.table(i, t)
	}
	if s.Commentary != "" {
		v.commentary(s.Commentary)
	}
	return v.problems
}

// specValidator collects the problems of a spec in the order found.
type specValidator struct {
	results  map[string]*Result
	problems []string
}

func (v *specValidator) bad(format string, args ...any) {
	v.problems = append(v.problems, fmt.Sprintf(format, args...))
}

// get resolves a query_id, reporting an unknown one.
func (v *specValidator) get(where, id string) *Result {
	r := v.results[id]
	if r == nil {
		v.bad("%s: unknown query_id %q (run the query first)", where, id)
	}
	return r
}

func (v *specValidator) tile(i int, t Tile) {
	where := fmt.Sprintf("tile %d (%s)", i+1, t.Label)
	r := v.get(where, t.QueryID)
	if r == nil {
		return
	}
	if !r.hasColumn(t.Measure, "measure") {
		v.bad("%s: measure %q is not in query %s", where, t.Measure, t.QueryID)
		return
	}
	cur, okC := v.tileSum(where, "", "where", r, t.Measure, t.Where)
	if t.CompareWhere == nil {
		return
	}
	base, okB := v.tileSum(where, " compare", "compare_where", r, t.Measure, t.CompareWhere)
	if _, err := diff(cur, base); okC && okB && err != nil {
		v.bad("%s compare: %v", where, err) // e.g. the baseline is in another currency
	}
}

// tileSum sums a tile's measure over the rows matching filter, reporting a
// failed sum or one of no rows.
func (v *specValidator) tileSum(where, side, field string, r *Result, measure string, filter map[string]string) (Value, bool) {
	total, n, err := r.sum(measure, filter)
	switch {
	case err != nil:
		v.bad("%s%s: %v", where, side, err)
	case n == 0 && field == "where":
		v.bad("%s: no rows match %v", where, filter)
	case n == 0:
		v.bad("%s: no rows match %s %v", where, field, filter)
	}
	return total, err == nil && n > 0
}

func (v *specValidator) chart(i int, c Chart) {
	where := fmt.Sprintf("chart %d (%s)", i+1, c.Title)
	r := v.get(where, c.QueryID)
	if r == nil {
		return
	}
	if c.Type != "bar" && c.Type != "line" {
		v.bad("%s: type must be bar or line", where)
	}
	n := len(v.problems)
	if !r.hasColumn(c.X, "dimension") {
		v.bad("%s: x %q is not a dimension of query %s", where, c.X, c.QueryID)
	}
	if c.Series != "" && !r.hasColumn(c.Series, "dimension") {
		v.bad("%s: series %q is not a dimension of query %s", where, c.Series, c.QueryID)
	}
	if !r.hasColumn(c.Measure, "measure") {
		v.bad("%s: measure %q is not in query %s", where, c.Measure, c.QueryID)
	}
	if len(v.problems) > n {
		return
	}
	if _, _, _, err := chartData(c, r); err != nil {
		v.bad("%s: %v", where, err) // a point adds amounts in two currencies
	}
}

func (v *specValidator) table(i int, t Table) {
	where := fmt.Sprintf("table %d (%s)", i+1, t.Title)
	r := v.get(where, t.QueryID)
	if r == nil || t.Compare == nil {
		return
	}
	c := t.Compare
	if !r.hasColumn(c.Dim, "dimension") {
		v.bad("%s: compare dim %q is not a dimension of query %s", where, c.Dim, t.QueryID)
	}
	if !r.hasColumn(c.Measure, "measure") {
		v.bad("%s: compare measure %q is not in query %s", where, c.Measure, t.QueryID)
	}
}

// commentary checks the commentary against every result, not only those
// the spec references: the agent may cite any query it ran.
func (v *specValidator) commentary(text string) {
	var used []*Result
	for _, r := range v.results {
		used = append(used, r)
	}
	if missing := CheckNarrative(text, used); len(missing) > 0 {
		v.problems = append(v.problems, narrativeError(missing).Error())
	}
}

func (r *Result) col(name string) int {
	return slices.IndexFunc(r.Columns, func(c Column) bool { return c.Name == name })
}

// hasColumn reports whether the result has a column name of the given kind.
func (r *Result) hasColumn(name, kind string) bool {
	i := r.col(name)
	return i >= 0 && r.Columns[i].Kind == kind
}

// sum adds a measure over rows whose dimensions match every where entry.
func (r *Result) sum(measure string, where map[string]string) (Value, int, error) {
	mi := r.col(measure)
	if mi < 0 {
		return Value{}, 0, fmt.Errorf("measure %q not in result", measure)
	}
	for d := range where {
		if i := r.col(d); i < 0 || r.Columns[i].Kind != "dimension" {
			return Value{}, 0, fmt.Errorf("where uses %q, which is not a dimension of this query", d)
		}
	}
	var vals []Value
	for _, row := range r.Rows {
		if v, ok := row[mi].(Value); ok && rowMatches(r, row, where) {
			vals = append(vals, v)
		}
	}
	if len(vals) > 1 && r.Columns[mi].Type == Ratio {
		return Value{}, len(vals), fmt.Errorf("ratio %s matches %d rows; narrow where to one row", measure, len(vals))
	}
	// Exact, in the amounts' own digits: a BHD total keeps its third
	// decimal, and amounts in two currencies are refused, not added.
	total, _, err := sumValues(vals)
	if err != nil {
		return Value{}, len(vals), fmt.Errorf("%s: %w; narrow where to one company", measure, err)
	}
	return total, len(vals), nil
}

func rowMatches(r *Result, row Row, where map[string]string) bool {
	for d, want := range where {
		if v, _ := row[r.col(d)].(string); v != want {
			return false
		}
	}
	return true
}
