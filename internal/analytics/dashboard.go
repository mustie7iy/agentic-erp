package analytics

import (
	"fmt"
	"math"
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
	var problems []string
	bad := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	if strings.TrimSpace(s.Title) == "" {
		bad("title is required")
	}
	if len(s.Tiles)+len(s.Charts)+len(s.Tables) == 0 {
		bad("add at least one tile, chart or table")
	}
	get := func(where, id string) *Result {
		r := results[id]
		if r == nil {
			bad("%s: unknown query_id %q (run the query first)", where, id)
		}
		return r
	}
	for i, t := range s.Tiles {
		where := fmt.Sprintf("tile %d (%s)", i+1, t.Label)
		r := get(where, t.QueryID)
		if r == nil {
			continue
		}
		if r.col(t.Measure) < 0 || r.Columns[r.col(t.Measure)].Kind != "measure" {
			bad("%s: measure %q is not in query %s", where, t.Measure, t.QueryID)
			continue
		}
		if _, n, err := r.sum(t.Measure, t.Where); err != nil {
			bad("%s: %v", where, err)
		} else if n == 0 {
			bad("%s: no rows match %v", where, t.Where)
		}
		if t.CompareWhere != nil {
			if _, n, err := r.sum(t.Measure, t.CompareWhere); err != nil {
				bad("%s compare: %v", where, err)
			} else if n == 0 {
				bad("%s: no rows match compare_where %v", where, t.CompareWhere)
			}
		}
	}
	for i, c := range s.Charts {
		where := fmt.Sprintf("chart %d (%s)", i+1, c.Title)
		r := get(where, c.QueryID)
		if r == nil {
			continue
		}
		if c.Type != "bar" && c.Type != "line" {
			bad("%s: type must be bar or line", where)
		}
		if r.col(c.X) < 0 || r.Columns[r.col(c.X)].Kind != "dimension" {
			bad("%s: x %q is not a dimension of query %s", where, c.X, c.QueryID)
		}
		if c.Series != "" && (r.col(c.Series) < 0 || r.Columns[r.col(c.Series)].Kind != "dimension") {
			bad("%s: series %q is not a dimension of query %s", where, c.Series, c.QueryID)
		}
		if r.col(c.Measure) < 0 || r.Columns[r.col(c.Measure)].Kind != "measure" {
			bad("%s: measure %q is not in query %s", where, c.Measure, c.QueryID)
		}
	}
	for i, t := range s.Tables {
		where := fmt.Sprintf("table %d (%s)", i+1, t.Title)
		r := get(where, t.QueryID)
		if r == nil || t.Compare == nil {
			continue
		}
		c := t.Compare
		if r.col(c.Dim) < 0 || r.Columns[r.col(c.Dim)].Kind != "dimension" {
			bad("%s: compare dim %q is not a dimension of query %s", where, c.Dim, t.QueryID)
		}
		if r.col(c.Measure) < 0 || r.Columns[r.col(c.Measure)].Kind != "measure" {
			bad("%s: compare measure %q is not in query %s", where, c.Measure, t.QueryID)
		}
	}
	if s.Commentary != "" {
		var used []*Result
		for _, r := range results {
			used = append(used, r)
		}
		if missing := CheckNarrative(s.Commentary, used); len(missing) > 0 {
			problems = append(problems, narrativeError(missing).Error())
		}
	}
	return problems
}

func (r *Result) col(name string) int {
	return slices.IndexFunc(r.Columns, func(c Column) bool { return c.Name == name })
}

// sum adds a measure over rows whose dimensions match every where entry.
func (r *Result) sum(measure string, where map[string]string) (float64, int, error) {
	mi := r.col(measure)
	if mi < 0 {
		return 0, 0, fmt.Errorf("measure %q not in result", measure)
	}
	for d := range where {
		if i := r.col(d); i < 0 || r.Columns[i].Kind != "dimension" {
			return 0, 0, fmt.Errorf("where uses %q, which is not a dimension of this query", d)
		}
	}
	if r.Columns[mi].Type == Ratio {
		// A ratio can only be read from exactly one row.
		var val float64
		n := 0
		for _, row := range r.Rows {
			if rowMatches(r, row, where) {
				if v, ok := row[mi].(float64); ok {
					val, n = v, n+1
				}
			}
		}
		if n > 1 {
			return 0, n, fmt.Errorf("ratio %s matches %d rows; narrow where to one row", measure, n)
		}
		return val, n, nil
	}
	var total float64
	n := 0
	for _, row := range r.Rows {
		if !rowMatches(r, row, where) {
			continue
		}
		if v, ok := row[mi].(float64); ok {
			total += v
			n++
		}
	}
	return round(total, 2), n, nil
}

func rowMatches(r *Result, row Row, where map[string]string) bool {
	for d, want := range where {
		if v, _ := row[r.col(d)].(string); v != want {
			return false
		}
	}
	return true
}

// formatValue renders a measure for display.
func formatValue(v float64, c Column) string {
	switch c.Type {
	case Ratio:
		return fmt.Sprintf("%.1f%%", v*100)
	case Count:
		return groupThousands(fmt.Sprintf("%.0f", v))
	}
	prefix := currencySymbol(c.Currency)
	av := math.Abs(v)
	sign := ""
	if v < 0 {
		sign = "−"
	}
	switch {
	case av >= 1e9:
		return fmt.Sprintf("%s%s%.2fB", sign, prefix, av/1e9)
	case av >= 1e6:
		return fmt.Sprintf("%s%s%.2fM", sign, prefix, av/1e6)
	case av >= 1e4:
		return fmt.Sprintf("%s%s%sK", sign, prefix, groupThousands(fmt.Sprintf("%.1f", av/1e3)))
	}
	return sign + prefix + groupThousands(fmt.Sprintf("%.2f", av))
}

// formatFull renders an amount with all digits, for tables.
func formatFull(v float64, c Column) string {
	switch c.Type {
	case Ratio:
		return fmt.Sprintf("%.1f%%", v*100)
	case Count:
		return groupThousands(fmt.Sprintf("%.0f", v))
	}
	s := groupThousands(fmt.Sprintf("%.2f", math.Abs(v)))
	if v < 0 {
		return "−" + s
	}
	return s
}

func currencySymbol(cur string) string {
	switch cur {
	case "USD":
		return "$"
	case "EUR":
		return "€"
	case "GBP":
		return "£"
	case "":
		return ""
	}
	return cur + " "
}

func groupThousands(s string) string {
	intPart, frac, hasFrac := strings.Cut(s, ".")
	neg := strings.HasPrefix(intPart, "-")
	intPart = strings.TrimPrefix(intPart, "-")
	var b strings.Builder
	for i, r := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	out := b.String()
	if hasFrac {
		out += "." + frac
	}
	if neg {
		out = "-" + out
	}
	return out
}

// change describes current vs baseline for tiles and tables.
func change(cur, base float64, c Column) (delta string, pct string, up bool) {
	d := cur - base
	up = d >= 0
	if c.Type == Ratio {
		return fmt.Sprintf("%+.1f pts", d*100), "", up
	}
	sign := "+"
	if d < 0 {
		sign = "−"
	}
	delta = sign + strings.TrimPrefix(formatValue(math.Abs(d), c), "−")
	if base != 0 {
		p := d / math.Abs(base) * 100
		ps := "+"
		if p < 0 {
			ps = "−"
		}
		pct = fmt.Sprintf("%s%.1f%%", ps, math.Abs(p))
	}
	return delta, pct, up
}

// changeFull is change with the delta written in full, for tables.
func changeFull(cur, base float64, c Column) (delta string, pct string, up bool) {
	_, pct, up = change(cur, base, c)
	if c.Type == Ratio {
		delta, _, _ = change(cur, base, c)
		return delta, pct, up
	}
	sign := "+"
	if cur-base < 0 {
		sign = "−"
	}
	return sign + formatFull(math.Abs(cur-base), c), pct, up
}
