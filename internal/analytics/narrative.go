package analytics

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// The narrative check enforces "numbers come from queries": every figure in
// agent-written commentary must be a value in a query result, a subtotal of
// one, or a change between two results that differ in a single dimension
// (e.g. the same month in two years).

type figure struct {
	value    float64 // in major units; percentages as percent (7.0 = 7%)
	percent  bool    // a share or a change in percent
	points   bool    // percentage points
	decimals int     // precision the figure was written with
	scale    float64 // 1, 1e3, 1e6, 1e9 from K/M/B suffixes
	text     string
}

var figureRe = regexp.MustCompile(`(?i)([+\-−]?)(\$|€|£|USD\s?|EUR\s?)?(\d{1,3}(?:,\d{3})+|\d+)(?:\.(\d+))?(\s?(?:%|percentage points?|pts?\b|points?\b|bn\b|billion\b|million\b|thousand\b|[kmb]\b))?`)

// extractFigures finds the numbers in text that claim a quantity. Years,
// small integers (days, counts like "8 of 9", quarter numbers) and numbers
// inside words are ignored.
func extractFigures(text string) []figure {
	var out []figure
	for _, m := range figureRe.FindAllStringSubmatchIndex(text, -1) {
		if followsLetter(text, m[0]) {
			continue // e.g. Q4, FY2026
		}
		if f, ok := parseFigure(text, m); ok {
			out = append(out, f)
		}
	}
	return out
}

// followsLetter reports whether the match at start continues a word.
func followsLetter(text string, start int) bool {
	if start == 0 {
		return false
	}
	prev := text[start-1]
	return (prev >= 'a' && prev <= 'z') || (prev >= 'A' && prev <= 'Z') || prev == '_'
}

// parseFigure reads one figureRe match; ok is false for numbers that do not
// claim a quantity (plain years and small integers).
func parseFigure(text string, m []int) (figure, bool) {
	start, end := m[0], m[1]
	get := func(i int) string {
		if m[2*i] < 0 {
			return ""
		}
		return text[m[2*i]:m[2*i+1]]
	}
	intPart := strings.ReplaceAll(get(3), ",", "")
	frac := get(4)
	suffix := strings.ToLower(strings.TrimSpace(get(5)))
	currency := get(2) != ""
	v, err := strconv.ParseFloat(intPart+"."+frac+"0", 64)
	if err != nil {
		return figure{}, false
	}
	f := figure{value: v, decimals: len(frac), scale: 1, text: text[start:end]}
	f.applySuffix(suffix)
	plain := !currency && suffix == "" && frac == ""
	if plain && (v <= 31 || (v >= 1900 && v <= 2100)) {
		return figure{}, false
	}
	return f, true
}

// applySuffix marks a figure as a percentage or points, or scales it by a
// K/M/B suffix.
func (f *figure) applySuffix(suffix string) {
	switch {
	case suffix == "%":
		f.percent = true
	case strings.HasPrefix(suffix, "percentage"), strings.HasPrefix(suffix, "pt"), strings.HasPrefix(suffix, "point"):
		f.points = true
	case suffix == "k" || suffix == "thousand":
		f.scale = 1e3
	case suffix == "m" || suffix == "million":
		f.scale = 1e6
	case suffix == "b" || suffix == "bn" || suffix == "billion":
		f.scale = 1e9
	}
}

// evidence is the set of numbers the commentary may cite.
type evidence struct {
	amounts []float64 // money and counts, major units
	percent []float64 // ratio values and % changes, as percent
	points  []float64 // differences between ratio values, as percentage points
}

func buildEvidence(results []*Result) *evidence {
	ev := &evidence{}
	for _, r := range results {
		dimIdx, measIdx := splitColumns(r)
		for _, mi := range measIdx {
			ev.addMeasure(r, dimIdx, mi)
		}
	}
	return ev
}

// splitColumns returns the indexes of the dimension and measure columns.
func splitColumns(r *Result) (dimIdx, measIdx []int) {
	for i, c := range r.Columns {
		if c.Kind == "dimension" {
			dimIdx = append(dimIdx, i)
		} else {
			measIdx = append(measIdx, i)
		}
	}
	return dimIdx, measIdx
}

// aggregationLevels lists the series at every level of aggregation: the
// rows themselves, subtotals with any one dimension removed, and the grand
// total.
func aggregationLevels(dimIdx []int) [][]int {
	levels := [][]int{dimIdx}
	for drop := range dimIdx {
		keep := append(append([]int{}, dimIdx[:drop]...), dimIdx[drop+1:]...)
		levels = append(levels, keep)
	}
	if len(dimIdx) > 1 {
		levels = append(levels, nil)
	}
	return levels
}

// addMeasure adds the values of measure column mi at every level of
// aggregation, and the changes between them.
func (ev *evidence) addMeasure(r *Result, dimIdx []int, mi int) {
	ratio := r.Columns[mi].Type == Ratio
	for _, keep := range aggregationLevels(dimIdx) {
		if ratio && len(keep) != len(dimIdx) {
			continue // ratios don't add up
		}
		points := aggregate(r, keep, mi)
		for _, p := range points {
			if ratio {
				ev.percent = append(ev.percent, p.value*100)
			} else {
				ev.amounts = append(ev.amounts, p.value)
			}
		}
		ev.addChanges(points, ratio)
	}
}

// addChanges adds the changes between points that differ in exactly one
// dimension: percentage points for ratios, amounts and percent otherwise.
func (ev *evidence) addChanges(points []point, ratio bool) {
	for a := range points {
		for b := range points {
			if a == b || !differInOne(points[a].dims, points[b].dims) {
				continue
			}
			x, y := points[a].value, points[b].value
			if ratio {
				ev.points = append(ev.points, (y-x)*100)
				continue
			}
			ev.amounts = append(ev.amounts, y-x)
			if x != 0 {
				ev.percent = append(ev.percent, (y-x)/math.Abs(x)*100)
			}
		}
	}
}

type point struct {
	dims  []string
	value float64
}

func aggregate(r *Result, keep []int, mi int) []point {
	idx := map[string]int{}
	var out []point
	for _, row := range r.Rows {
		v, ok := row[mi].(float64)
		if !ok {
			continue
		}
		dims := make([]string, len(keep))
		for i, k := range keep {
			dims[i], _ = row[k].(string)
		}
		key := strings.Join(dims, "\x1f")
		if i, seen := idx[key]; seen {
			out[i].value += v
			continue
		}
		idx[key] = len(out)
		out = append(out, point{dims: dims, value: v})
	}
	return out
}

func differInOne(a, b []string) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}
	diff := 0
	for i := range a {
		if a[i] != b[i] {
			diff++
		}
	}
	return diff == 1
}

// supports reports whether a written figure matches any evidence at the
// precision it was written with. Signs are ignored ("down 2.6%").
func (ev *evidence) supports(f figure) bool {
	tol := 0.5*math.Pow(10, -float64(f.decimals)) + 1e-9
	match := func(candidates []float64, scale float64) bool {
		for _, c := range candidates {
			if math.Abs(math.Abs(c)/scale-f.value) <= tol {
				return true
			}
		}
		return false
	}
	switch {
	case f.percent:
		return match(ev.percent, 1)
	case f.points:
		return match(ev.points, 1)
	case f.scale != 1:
		return match(ev.amounts, f.scale)
	default:
		// A bare number may be written in units, thousands or millions.
		return match(ev.amounts, 1) || match(ev.amounts, 1e3) || match(ev.amounts, 1e6)
	}
}

// CheckNarrative returns the figures in text that no result supports.
func CheckNarrative(text string, results []*Result) []string {
	ev := buildEvidence(results)
	var bad []string
	for _, f := range extractFigures(text) {
		if !ev.supports(f) {
			bad = append(bad, f.text)
		}
	}
	return bad
}

func narrativeError(bad []string) error {
	return fmt.Errorf("commentary cites figures not found in any query result: %s. Use only numbers from the results, their subtotals, or changes between them",
		strings.Join(bad, ", "))
}
