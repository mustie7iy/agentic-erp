package analytics

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Render writes the dashboard as a self-contained HTML page (inline CSS and
// SVG, no scripts) that can be opened, emailed or archived as-is. It
// validates the spec against the results first and writes nothing for an
// invalid one, so the drawing below can rely on every query, column and
// sum the spec names.
func Render(w io.Writer, s *Spec, results map[string]*Result, p Principal) error {
	if problems := s.Validate(results); len(problems) > 0 {
		return fmt.Errorf("analytics: render: invalid spec: %s", strings.Join(problems, "; "))
	}
	v := view{Spec: s, Principal: p.ID, Generated: time.Now().UTC().Format("2006-01-02 15:04 UTC")}
	v.Tiles = tileViews(s, results)
	for _, c := range s.Charts {
		v.Charts = append(v.Charts, chartView{Title: c.Title, SVG: template.HTML(chartSVG(c, results[c.QueryID]))}) // #nosec G203 -- SVG built by chartSVG from numbers and escaped labels, never from raw input
	}
	for _, t := range s.Tables {
		v.Tables = append(v.Tables, tableView(t, results[t.QueryID]))
	}
	for _, para := range strings.Split(strings.TrimSpace(s.Commentary), "\n\n") {
		if strings.TrimSpace(para) != "" {
			v.Commentary = append(v.Commentary, strings.TrimSpace(para))
		}
	}
	v.addProvenance(results)
	if err := pageTmpl.Execute(w, v); err != nil {
		return fmt.Errorf("analytics: render page: %w", err)
	}
	return nil
}

// tileViews computes each tile's value and, with a baseline, its change.
// Render has validated the spec, so every measure is a column of its
// query and the sums' errors were reported there.
func tileViews(s *Spec, results map[string]*Result) []tileView {
	var tiles []tileView
	for _, t := range s.Tiles {
		r := results[t.QueryID]
		col := r.Columns[r.col(t.Measure)]
		cur, _, _ := r.sum(t.Measure, t.Where)
		tv := tileView{Label: t.Label, Value: formatValue(cur, col)}
		if t.CompareWhere != nil {
			base, _, _ := r.sum(t.Measure, t.CompareWhere)
			delta, _ := diff(cur, base) // validated: one currency, in range
			tv.Delta, tv.Pct, tv.Up = change(base, delta, col)
			tv.CompareLabel = t.CompareLabel
			tv.HasCompare = true
		}
		tiles = append(tiles, tv)
	}
	return tiles
}

// addProvenance lists the queries in id order and the definitions of every
// metric they used, so the page shows how each number was computed.
func (v *view) addProvenance(results map[string]*Result) {
	ids := make([]string, 0, len(results))
	for id := range results {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	defs := map[string]MetricLineage{}
	for _, id := range ids {
		r := results[id]
		q, _ := json.MarshalIndent(r.Query, "", "  ")
		v.Queries = append(v.Queries, queryView{ID: id, JSON: string(q)})
		for _, d := range r.Definitions {
			defs[d.Name] = d
		}
		v.DataThrough, v.AsOf = r.DataThrough, r.AsOf.UTC().Format("2006-01-02 15:04 UTC")
	}
	for _, name := range sortedKeysMap(defs) {
		v.Definitions = append(v.Definitions, defView{Name: name, Text: definitionText(defs[name])})
	}
}

// definitionText is a metric's description followed by its source and
// filter, if it reads a fact table.
func definitionText(d MetricLineage) string {
	line := d.Description
	if d.Source != "" {
		line += fmt.Sprintf(" Source: %s", d.Source)
		if len(d.Filter) > 0 {
			f, _ := json.Marshal(d.Filter)
			line += fmt.Sprintf(", filter %s", f)
		}
		line += "."
	}
	return line
}

type view struct {
	Spec        *Spec
	Principal   string
	Generated   string
	AsOf        string
	DataThrough string
	Tiles       []tileView
	Charts      []chartView
	Tables      []tableData
	Commentary  []string
	Queries     []queryView
	Definitions []defView
}

type tileView struct {
	Label, Value, Delta, Pct, CompareLabel string
	Up, HasCompare                         bool
}
type chartView struct {
	Title string
	SVG   template.HTML
}
type queryView struct{ ID, JSON string }
type defView struct{ Name, Text string }
type tableData struct {
	Title   string
	Header  []string
	Numeric []bool
	Rows    [][]cellView
}
type cellView struct {
	Text  string
	Class string
}

func tableView(t Table, r *Result) tableData {
	td := tableData{Title: t.Title}
	if t.Compare == nil {
		td.plain(r)
		return td
	}
	td.pivot(t.Compare, r)
	return td
}

// plain shows the result as it is, one row per result row.
func (td *tableData) plain(r *Result) {
	for _, c := range r.Columns {
		h := c.Label
		if c.Currency != "" {
			h += " (" + c.Currency + ")"
		}
		td.Header = append(td.Header, h)
		td.Numeric = append(td.Numeric, c.Kind == "measure")
	}
	for _, row := range r.Rows {
		var cells []cellView
		for i, c := range r.Columns {
			cells = append(cells, cellView{Text: cellText(row[i], c)})
		}
		td.Rows = append(td.Rows, cells)
	}
}

// pivotPair is one pivot row: the values of the other dimensions and the
// measure's values at the baseline and at the current value.
type pivotPair struct {
	keys       []string
	base, curr []Value
}

// pivot shows one row per combination of the other dimensions, with a
// total row when there is more than one row of an additive measure whose
// sides each add up: amounts in several currencies have no total.
func (td *tableData) pivot(cmp *Compare, r *Result) {
	ci, mi := r.col(cmp.Dim), r.col(cmp.Measure)
	col := r.Columns[mi]
	keyCols := td.pivotHeader(cmp, r, ci, col)
	order, pairs := pivotPairs(cmp, r, keyCols, ci, mi)
	var all pivotPair
	for _, key := range order {
		p := pairs[key]
		td.Rows = append(td.Rows, pivotRow(p, r, keyCols, col))
		all.base = append(all.base, p.base...)
		all.curr = append(all.curr, p.curr...)
	}
	if len(keyCols) > 0 && col.Type != Ratio && len(order) > 1 {
		if row, ok := pivotTotal(keyCols, &all, col); ok {
			td.Rows = append(td.Rows, row)
		}
	}
}

// sides adds up a pivot row's baseline and current values; ok is false
// when either side does not add up (several currencies, out of range).
func (p *pivotPair) sides() (base, curr Value, hasB, hasC, ok bool) {
	base, hasB, errB := sumValues(p.base)
	curr, hasC, errC := sumValues(p.curr)
	return base, curr, hasB, hasC, errB == nil && errC == nil
}

// changeCells renders the change between the sides, or dashes when one
// is missing or they do not subtract (another currency, out of range).
func changeCells(base, curr Value, both bool, col Column, class string) []cellView {
	delta, err := diff(curr, base)
	if !both || err != nil {
		return []cellView{{Text: "—", Class: class}, {Text: "—", Class: class}}
	}
	d, pct, up := changeFull(base, delta, col)
	cls := "pos"
	if !up {
		cls = "neg"
	}
	return []cellView{{Text: d, Class: strings.TrimSpace(cls + " " + class)}, {Text: pct, Class: strings.TrimSpace(cls + " " + class)}}
}

// pivotHeader adds the other dimensions' headers and the baseline,
// current and change headers, and returns the other dimensions' columns.
func (td *tableData) pivotHeader(cmp *Compare, r *Result, ci int, col Column) []int {
	var keyCols []int
	for i, c := range r.Columns {
		if c.Kind == "dimension" && i != ci {
			keyCols = append(keyCols, i)
			td.Header = append(td.Header, c.Label)
			td.Numeric = append(td.Numeric, false)
		}
	}
	cur := ""
	if col.Currency != "" {
		cur = " (" + col.Currency + ")"
	}
	td.Header = append(td.Header, cmp.Baseline+cur, cmp.Current+cur, "Change", "Change %")
	td.Numeric = append(td.Numeric, true, true, true, true)
	return keyCols
}

// pivotPairs sums the measure per combination of the other dimensions at
// the baseline and current values, in natural order of those dimensions.
func pivotPairs(cmp *Compare, r *Result, keyCols []int, ci, mi int) ([]string, map[string]*pivotPair) {
	var order []string
	pairs := map[string]*pivotPair{}
	for _, row := range r.Rows {
		keys := make([]string, len(keyCols))
		for i, k := range keyCols {
			keys[i], _ = row[k].(string)
		}
		key := strings.Join(keys, "\x1f")
		p := pairs[key]
		if p == nil {
			p = &pivotPair{keys: keys}
			pairs[key] = p
			order = append(order, key)
		}
		val, ok := row[mi].(Value)
		if !ok {
			continue
		}
		switch row[ci] {
		case cmp.Baseline:
			p.base = append(p.base, val)
		case cmp.Current:
			p.curr = append(p.curr, val)
		}
	}
	sort.SliceStable(order, func(a, b int) bool {
		ka, kb := pairs[order[a]].keys, pairs[order[b]].keys
		for i := range ka {
			if c := compareValues(ka[i], kb[i]); c != 0 {
				return c < 0
			}
		}
		return false
	})
	return order, pairs
}

// pivotRow renders one pivot row; the change is shown only when both the
// baseline and the current value exist.
func pivotRow(p *pivotPair, r *Result, keyCols []int, col Column) []cellView {
	var cells []cellView
	for i, k := range p.keys {
		cells = append(cells, cellView{Text: dimText(k, r.Columns[keyCols[i]].Name)})
	}
	// A row has at most one value a side (result rows are unique per
	// group), so its sides always add up.
	base, curr, hasB, hasC, _ := p.sides()
	cells = append(cells, numCell(base, hasB, col, ""), numCell(curr, hasC, col, ""))
	return append(cells, changeCells(base, curr, hasB && hasC, col, "")...)
}

// pivotTotal renders the total row under the pivot; ok is false when the
// rows do not add up, as amounts in several currencies do not.
func pivotTotal(keyCols []int, all *pivotPair, col Column) (row []cellView, ok bool) {
	base, curr, hasB, hasC, ok := all.sides()
	if !ok {
		return nil, false
	}
	row = []cellView{{Text: "Total", Class: "total"}}
	for range keyCols[1:] {
		row = append(row, cellView{Class: "total"})
	}
	row = append(row, numCell(base, hasB, col, "total"), numCell(curr, hasC, col, "total"))
	return append(row, changeCells(base, curr, hasB && hasC, col, "total")...), true
}

func numCell(v Value, ok bool, c Column, class string) cellView {
	if !ok {
		return cellView{Text: "—", Class: class}
	}
	return cellView{Text: formatFull(v, c), Class: class}
}

func cellText(v any, c Column) string {
	switch x := v.(type) {
	case string:
		return dimText(x, c.Name)
	case Value:
		return formatFull(x, c)
	}
	return "—"
}

var monthNames = []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}

// dimText makes dimension values readable (fiscal months as names when the
// fiscal year is the calendar year, quarters as Q1-Q4).
func dimText(v, dim string) string {
	switch dim {
	case "fiscal_month":
		var m int
		if _, err := fmt.Sscanf(v, "%d", &m); err == nil && m >= 1 && m <= 12 {
			return monthNames[m-1]
		}
	case "fiscal_quarter":
		return "Q" + v
	}
	if v == "" {
		return "(none)"
	}
	return v
}

var seriesColors = []string{"var(--s1)", "var(--s2)", "var(--s3)", "var(--s4)", "var(--s5)"}

// Chart geometry in SVG user units: the canvas and the padding around the
// plot area.
const (
	chartW, chartH              = 720.0, 300.0
	chartLeft, chartRight       = 64.0, 12.0
	chartTopPad, chartBottomPad = 28.0, 30.0
	chartPlotW, chartPlotH      = chartW - chartLeft - chartRight, chartH - chartTopPad - chartBottomPad
)

// chartSVG draws a grouped bar or multi-line chart on one linear scale.
func chartSVG(c Chart, r *Result) string {
	col := r.Columns[r.col(c.Measure)]
	xs, ss, vals, _ := chartData(c, r) // Render validated the chart: every point adds up
	if len(xs) == 0 {
		return `<p class="muted">No data for this chart.</p>`
	}
	fr := newChartFrame(vals, len(xs))
	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" role="img" aria-label="%s">`, chartW, chartH, template.HTMLEscapeString(c.Title))
	fr.axis(&b, col)
	if len(ss) > 1 || ss[0] != "" {
		chartLegend(&b, c, ss)
	}
	for i, x := range xs {
		cx := chartLeft + fr.gw*float64(i) + fr.gw/2
		fmt.Fprintf(&b, `<text class="axis" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`, cx, chartH-chartBottomPad+16, template.HTMLEscapeString(dimText(x, c.X)))
	}
	if c.Type == "line" {
		fr.lines(&b, xs, ss, vals)
	} else {
		fr.bars(&b, c, col, xs, ss, vals)
	}
	b.WriteString(`</svg>`)
	return b.String()
}

// chartData sums the measure per x value and series, both in natural
// order; rows without a value are skipped. A point that would add amounts
// in two currencies, or out of range, is an error: the spec validation
// reports it, so the chart is broken down by company instead.
func chartData(c Chart, r *Result) (xs, ss []string, vals map[[2]string]Value, err error) {
	xi, mi := r.col(c.X), r.col(c.Measure)
	si := -1
	if c.Series != "" {
		si = r.col(c.Series)
	}
	points := map[[2]string][]Value{}
	for _, row := range r.Rows {
		x, _ := row[xi].(string)
		s := ""
		if si >= 0 {
			s, _ = row[si].(string)
		}
		v, ok := row[mi].(Value)
		if !ok {
			continue
		}
		if !contains(xs, x) {
			xs = append(xs, x)
		}
		if !contains(ss, s) {
			ss = append(ss, s)
		}
		points[[2]string{x, s}] = append(points[[2]string{x, s}], v)
	}
	sort.SliceStable(xs, func(a, b int) bool { return compareValues(xs[a], xs[b]) < 0 })
	sort.SliceStable(ss, func(a, b int) bool { return compareValues(ss[a], ss[b]) < 0 })
	vals = map[[2]string]Value{}
	for _, x := range xs { // in order, so the error reported does not vary
		for _, s := range ss {
			if p, ok := points[[2]string{x, s}]; ok {
				if vals[[2]string{x, s}], _, err = sumValues(p); err != nil {
					return nil, nil, nil, fmt.Errorf("the point %q: %w; break the chart down by company", strings.TrimSpace(x+" "+s), err)
				}
			}
		}
	}
	return xs, ss, vals, nil
}

// chartFrame is the value scale and the width of one x group.
type chartFrame struct {
	bottom, top, step, gw float64
}

// newChartFrame picks a scale with about four nice steps that includes
// zero and every value.
func newChartFrame(vals map[[2]string]Value, groups int) chartFrame {
	lo, hi := 0.0, 0.0
	for _, v := range vals {
		lo, hi = math.Min(lo, v.float()), math.Max(hi, v.float())
	}
	step := niceStep((hi - lo) / 4)
	if step == 0 {
		step = 1
	}
	top := math.Ceil(hi/step) * step
	bottom := math.Floor(lo/step) * step
	if top == bottom {
		top = bottom + step
	}
	return chartFrame{bottom: bottom, top: top, step: step, gw: chartPlotW / float64(groups)}
}

// y maps a value to its vertical position in the plot area.
func (fr chartFrame) y(v float64) float64 {
	return chartTopPad + (fr.top-v)/(fr.top-fr.bottom)*chartPlotH
}

// axis draws the grid lines with their labels and, below zero, the zero line.
func (fr chartFrame) axis(b *strings.Builder, col Column) {
	for t := fr.bottom; t <= fr.top+fr.step/2; t += fr.step {
		fmt.Fprintf(b, `<line class="grid" x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/>`, chartLeft, chartW-chartRight, fr.y(t), fr.y(t))
		fmt.Fprintf(b, `<text class="axis" x="%.1f" y="%.1f" text-anchor="end">%s</text>`, chartLeft-6, fr.y(t)+3, axisLabel(t, col))
	}
	if fr.bottom < 0 {
		fmt.Fprintf(b, `<line class="zero" x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/>`, chartLeft, chartW-chartRight, fr.y(0), fr.y(0))
	}
}

// chartLegend draws one colour swatch and name per series.
func chartLegend(b *strings.Builder, c Chart, ss []string) {
	lx := chartLeft
	for i, s := range ss {
		fmt.Fprintf(b, `<rect x="%.1f" y="6" width="10" height="10" rx="2" style="fill:%s"/><text class="legend" x="%.1f" y="15">%s</text>`,
			lx, seriesColors[i%len(seriesColors)], lx+14, template.HTMLEscapeString(dimText(s, c.Series)))
		lx += 24 + 7*float64(len(dimText(s, c.Series)))
	}
}

// lines draws one polyline with point markers per series.
func (fr chartFrame) lines(b *strings.Builder, xs, ss []string, vals map[[2]string]Value) {
	for si, s := range ss {
		var pts []string
		for i, x := range xs {
			v, ok := vals[[2]string{x, s}]
			if !ok {
				continue
			}
			pts = append(pts, fmt.Sprintf("%.1f,%.1f", chartLeft+fr.gw*float64(i)+fr.gw/2, fr.y(v.float())))
		}
		color := seriesColors[si%len(seriesColors)]
		fmt.Fprintf(b, `<polyline fill="none" stroke-width="2.5" style="stroke:%s" points="%s"/>`, color, strings.Join(pts, " "))
		for _, p := range pts {
			xy := strings.Split(p, ",")
			fmt.Fprintf(b, `<circle cx="%s" cy="%s" r="3.5" style="fill:%s"/>`, xy[0], xy[1], color)
		}
	}
}

// bars draws one bar per series side by side in each x group, from zero.
func (fr chartFrame) bars(b *strings.Builder, c Chart, col Column, xs, ss []string, vals map[[2]string]Value) {
	n := float64(len(ss))
	bw := math.Min(28, (fr.gw*0.8)/n)
	for i, x := range xs {
		start := chartLeft + fr.gw*float64(i) + fr.gw/2 - bw*n/2
		for si, s := range ss {
			v, ok := vals[[2]string{x, s}]
			if !ok {
				continue
			}
			y0, y1 := fr.y(math.Max(v.float(), 0)), fr.y(math.Min(v.float(), 0))
			fmt.Fprintf(b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" style="fill:%s"><title>%s %s: %s</title></rect>`,
				start+bw*float64(si)+1, y0, bw-2, math.Max(y1-y0, 0.5), seriesColors[si%len(seriesColors)],
				template.HTMLEscapeString(dimText(x, c.X)), template.HTMLEscapeString(dimText(s, c.Series)), formatFull(v, col))
		}
	}
}

func niceStep(raw float64) float64 {
	if raw <= 0 {
		return 0
	}
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 2.5, 5} {
		if raw <= m*mag {
			return m * mag
		}
	}
	return 10 * mag // raw < 10*mag by construction of mag
}

func axisLabel(v float64, c Column) string {
	if c.Type == Ratio {
		return fmt.Sprintf("%.0f%%", v*100)
	}
	av := math.Abs(v)
	sign := ""
	if v < 0 {
		sign = "−"
	}
	switch {
	case av >= 1e6:
		return fmt.Sprintf("%s%gM", sign, round(av/1e6, 2))
	case av >= 1e3:
		return fmt.Sprintf("%s%gK", sign, round(av/1e3, 1))
	}
	return fmt.Sprintf("%s%g", sign, av)
}

// round rounds a scale label to places decimals; the figures themselves
// are Values, never rounded through a float.
func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

// float is a value for drawing and for percentages of change: never for a
// figure shown as an amount, which is rendered from its exact digits.
func (v Value) float() float64 {
	f, _ := strconv.ParseFloat(v.String(), 64) // a plain decimal of an int64 always parses
	return f
}

// formatValue renders a measure compactly, for tiles: an amount below
// 10,000 in full with its currency's digits ("BHD 1,234.567", "JPY
// 5,000"), larger ones in thousands, millions or billions.
func formatValue(v Value, c Column) string {
	switch c.Type {
	case Ratio:
		return fmt.Sprintf("%.1f%%", v.float()*100)
	case Count:
		return v.Units.Format(v.Scale)
	case Money: // with the currency's symbol, below
	}
	prefix := currencySymbol(v.Currency)
	av := math.Abs(v.float())
	sign := ""
	if v.Units < 0 {
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
	return sign + prefix + v.Units.Abs().Format(v.Scale)
}

// formatFull renders a measure with all its digits, for tables.
func formatFull(v Value, c Column) string {
	if c.Type == Ratio {
		return fmt.Sprintf("%.1f%%", v.float()*100)
	}
	s := v.Units.Abs().Format(v.Scale)
	if v.Units < 0 {
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

// change describes the change delta from base, for tiles: compact, with
// the percentage of the baseline.
func change(base, delta Value, c Column) (text string, pct string, up bool) {
	up = delta.Units >= 0
	if c.Type == Ratio {
		return fmt.Sprintf("%+.1f pts", delta.float()*100), "", up
	}
	sign := "+"
	if delta.Units < 0 {
		sign = "−"
	}
	text = sign + formatValue(delta.abs(), c)
	if base.Units != 0 {
		p := delta.float() / math.Abs(base.float()) * 100
		ps := "+"
		if p < 0 {
			ps = "−"
		}
		pct = fmt.Sprintf("%s%.1f%%", ps, math.Abs(p))
	}
	return text, pct, up
}

// changeFull is change with the delta written in full, for tables.
func changeFull(base, delta Value, c Column) (text string, pct string, up bool) {
	text, pct, up = change(base, delta, c)
	if c.Type == Ratio {
		return text, pct, up
	}
	sign := "+"
	if delta.Units < 0 {
		sign = "−"
	}
	return sign + formatFull(delta.abs(), c), pct, up
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func sortedKeysMap[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// WriteCSV exports a result with machine-readable values.
func WriteCSV(w io.Writer, r *Result) error {
	cw := csv.NewWriter(w)
	head := make([]string, len(r.Columns))
	for i, c := range r.Columns {
		head[i] = c.Name
	}
	if err := cw.Write(head); err != nil {
		return fmt.Errorf("analytics: write csv header: %w", err)
	}
	for _, row := range r.Rows {
		rec := make([]string, len(row))
		for i, v := range row {
			switch x := v.(type) {
			case string:
				rec[i] = x
			case Value:
				rec[i] = x.String() // exact, with the value's own digits
			}
		}
		if err := cw.Write(rec); err != nil {
			return fmt.Errorf("analytics: write csv row: %w", err)
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("analytics: flush csv: %w", err)
	}
	return nil
}

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Spec.Title}}</title>
<style>
:root{--bg:#f5f7f6;--surface:#fff;--fg:#1b2420;--muted:#5b6a64;--line:#dce2de;--pos:#1d6b58;--neg:#b23a2b;
--s1:#b9c4bf;--s2:#1d6b58;--s3:#2a5ea6;--s4:#c08a2e;--s5:#7a4fa0}
@media (prefers-color-scheme:dark){:root{--bg:#111614;--surface:#182019;--fg:#e3eae6;--muted:#98a8a1;--line:#2a3430;--pos:#6fd0b0;--neg:#f08b7c;
--s1:#4a5853;--s2:#5fc2a5;--s3:#8db4f0;--s4:#e2b46a;--s5:#c9a3ea;color-scheme:dark}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif;padding:24px 16px 48px}
main{max-width:1040px;margin:0 auto;display:grid;gap:18px}h1{font-size:1.6rem;margin:0;line-height:1.2}h2{font-size:1rem;margin:0 0 8px}
.muted{color:var(--muted)}.q{font-size:1.02rem}.assume{border-left:3px solid var(--s3);padding:4px 12px;font-size:.92rem}
.tiles{display:grid;grid-template-columns:repeat(auto-fit,minmax(190px,1fr));gap:10px}
.tile{background:var(--surface);border:1px solid var(--line);border-radius:8px;padding:12px 14px;display:grid;gap:2px}
.tile .l{font-size:.8rem;color:var(--muted)}.tile .v{font-size:1.6rem;font-weight:700;font-variant-numeric:tabular-nums}
.tile .d{font-size:.82rem;font-weight:600;font-variant-numeric:tabular-nums}.up{color:var(--pos)}.down{color:var(--neg)}
.card{background:var(--surface);border:1px solid var(--line);border-radius:8px;padding:14px 16px;min-width:0}
.charts{display:grid;grid-template-columns:repeat(auto-fit,minmax(min(100%,460px),1fr));gap:12px}
svg{width:100%;height:auto;display:block}svg .grid{stroke:var(--line)}svg .zero{stroke:var(--muted)}
svg .axis,svg .legend{fill:var(--muted);font:11px ui-monospace,Menlo,monospace}
.tw{overflow-x:auto}table{border-collapse:collapse;width:100%;font-size:.88rem}th,td{padding:7px 10px;border-bottom:1px solid var(--line);text-align:left}
th{font-size:.74rem;text-transform:uppercase;letter-spacing:.04em;color:var(--muted)}td.n,th.n{text-align:right;font-variant-numeric:tabular-nums}
td.pos{color:var(--pos)}td.neg{color:var(--neg)}td.total{font-weight:700}
.comm p{margin:0 0 8px;max-width:75ch}.fu{display:flex;flex-wrap:wrap;gap:6px}.fu span{border:1px solid var(--s3);color:var(--s3);border-radius:999px;padding:2px 10px;font-size:.82rem}
details{font-size:.86rem}pre{background:var(--bg);border:1px solid var(--line);border-radius:6px;padding:10px;overflow-x:auto;font-size:.78rem}
footer{font-size:.8rem;color:var(--muted)}
</style></head><body><main>
<header><h1>{{.Spec.Title}}</h1>{{if .Spec.Question}}<p class="q muted">“{{.Spec.Question}}”</p>{{end}}</header>
{{if .Spec.Assumptions}}<div class="assume"><b>How this was read:</b> {{range $i, $a := .Spec.Assumptions}}{{if $i}} · {{end}}{{$a}}{{end}}</div>{{end}}
{{if .Tiles}}<section class="tiles">{{range .Tiles}}<div class="tile"><span class="l">{{.Label}}</span><span class="v">{{.Value}}</span>{{if .HasCompare}}<span class="d {{if .Up}}up{{else}}down{{end}}">{{if .Pct}}{{.Pct}} · {{end}}{{.Delta}}{{if .CompareLabel}} vs {{.CompareLabel}}{{end}}</span>{{end}}</div>{{end}}</section>{{end}}
{{if .Charts}}<section class="charts">{{range .Charts}}<figure class="card" style="margin:0"><h2>{{.Title}}</h2>{{.SVG}}</figure>{{end}}</section>{{end}}
{{range .Tables}}<section class="card"><h2>{{.Title}}</h2><div class="tw"><table><thead><tr>{{$num := .Numeric}}{{range $i, $h := .Header}}<th{{if index $num $i}} class="n"{{end}}>{{$h}}</th>{{end}}</tr></thead><tbody>{{range .Rows}}<tr>{{range $i, $c := .}}<td class="{{if index $num $i}}n {{end}}{{$c.Class}}">{{$c.Text}}</td>{{end}}</tr>{{end}}</tbody></table></div></section>{{end}}
{{if .Commentary}}<section class="card comm"><h2>Commentary</h2>{{range .Commentary}}<p>{{.}}</p>{{end}}{{if .Spec.FollowUps}}<div class="fu">{{range .Spec.FollowUps}}<span>{{.}}</span>{{end}}</div>{{end}}</section>{{end}}
<details class="card"><summary><b>How these numbers were computed</b></summary>
{{range .Definitions}}<p><b>{{.Name}}</b>: {{.Text}}</p>{{end}}
{{range .Queries}}<p class="muted">Query {{.ID}}</p><pre>{{.JSON}}</pre>{{end}}
</details>
<footer>Generated {{.Generated}} for {{.Principal}} · data as of {{.AsOf}}, through {{.DataThrough}} · every figure is computed from the governed semantic layer.</footer>
</main></body></html>`))
