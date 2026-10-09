package analytics

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

// Render writes the dashboard as a self-contained HTML page (inline CSS and
// SVG, no scripts) that can be opened, emailed or archived as-is.
func Render(w io.Writer, s *Spec, results map[string]*Result, p Principal) error {
	v := view{Spec: s, Principal: p.ID, Generated: time.Now().UTC().Format("2006-01-02 15:04 UTC")}
	for _, t := range s.Tiles {
		r := results[t.QueryID]
		col := r.Columns[r.col(t.Measure)]
		cur, _, _ := r.sum(t.Measure, t.Where)
		tv := tileView{Label: t.Label, Value: formatValue(cur, col)}
		if t.CompareWhere != nil {
			base, _, _ := r.sum(t.Measure, t.CompareWhere)
			tv.Delta, tv.Pct, tv.Up = change(cur, base, col)
			tv.CompareLabel = t.CompareLabel
			tv.HasCompare = true
		}
		v.Tiles = append(v.Tiles, tv)
	}
	for _, c := range s.Charts {
		v.Charts = append(v.Charts, chartView{Title: c.Title, SVG: template.HTML(chartSVG(c, results[c.QueryID]))})
	}
	for _, t := range s.Tables {
		v.Tables = append(v.Tables, tableView(t, results[t.QueryID]))
	}
	for _, para := range strings.Split(strings.TrimSpace(s.Commentary), "\n\n") {
		if strings.TrimSpace(para) != "" {
			v.Commentary = append(v.Commentary, strings.TrimSpace(para))
		}
	}
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
		d := defs[name]
		line := d.Description
		if d.Source != "" {
			line += fmt.Sprintf(" Source: %s", d.Source)
			if len(d.Filter) > 0 {
				f, _ := json.Marshal(d.Filter)
				line += fmt.Sprintf(", filter %s", f)
			}
			line += "."
		}
		v.Definitions = append(v.Definitions, defView{Name: name, Text: line})
	}
	return pageTmpl.Execute(w, v)
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
		return td
	}

	// Pivot: one row per combination of the other dimensions.
	cmp := t.Compare
	ci, mi := r.col(cmp.Dim), r.col(cmp.Measure)
	col := r.Columns[mi]
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
	type pair struct {
		keys       []string
		base, curr float64
		hasB, hasC bool
	}
	var order []string
	pairs := map[string]*pair{}
	for _, row := range r.Rows {
		keys := make([]string, len(keyCols))
		for i, k := range keyCols {
			keys[i], _ = row[k].(string)
		}
		key := strings.Join(keys, "\x1f")
		p := pairs[key]
		if p == nil {
			p = &pair{keys: keys}
			pairs[key] = p
			order = append(order, key)
		}
		val, ok := row[mi].(float64)
		if !ok {
			continue
		}
		switch row[ci] {
		case cmp.Baseline:
			p.base, p.hasB = p.base+val, true
		case cmp.Current:
			p.curr, p.hasC = p.curr+val, true
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
	var totB, totC float64
	for _, key := range order {
		p := pairs[key]
		var cells []cellView
		for i, k := range p.keys {
			cells = append(cells, cellView{Text: dimText(k, r.Columns[keyCols[i]].Name)})
		}
		cells = append(cells, numCell(p.base, p.hasB, col), numCell(p.curr, p.hasC, col))
		if p.hasB && p.hasC {
			d, pct, up := changeFull(p.curr, p.base, col)
			cls := "pos"
			if !up {
				cls = "neg"
			}
			cells = append(cells, cellView{Text: d, Class: cls}, cellView{Text: pct, Class: cls})
		} else {
			cells = append(cells, cellView{Text: "—"}, cellView{Text: "—"})
		}
		td.Rows = append(td.Rows, cells)
		totB += p.base
		totC += p.curr
	}
	if len(keyCols) > 0 && col.Type != Ratio && len(order) > 1 {
		cells := []cellView{{Text: "Total", Class: "total"}}
		for range keyCols[1:] {
			cells = append(cells, cellView{Class: "total"})
		}
		d, pct, up := changeFull(totC, totB, col)
		cls := "pos total"
		if !up {
			cls = "neg total"
		}
		cells = append(cells, cellView{Text: formatFull(totB, col), Class: "total"}, cellView{Text: formatFull(totC, col), Class: "total"},
			cellView{Text: d, Class: cls}, cellView{Text: pct, Class: cls})
		td.Rows = append(td.Rows, cells)
	}
	return td
}

func numCell(v float64, ok bool, c Column) cellView {
	if !ok {
		return cellView{Text: "—"}
	}
	return cellView{Text: formatFull(v, c)}
}

func cellText(v any, c Column) string {
	switch x := v.(type) {
	case string:
		return dimText(x, c.Name)
	case float64:
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

// chartSVG draws a grouped bar or multi-line chart on one linear scale.
func chartSVG(c Chart, r *Result) string {
	xi, mi := r.col(c.X), r.col(c.Measure)
	si := -1
	if c.Series != "" {
		si = r.col(c.Series)
	}
	col := r.Columns[mi]
	var xs, ss []string
	vals := map[[2]string]float64{}
	for _, row := range r.Rows {
		x, _ := row[xi].(string)
		s := ""
		if si >= 0 {
			s, _ = row[si].(string)
		}
		v, ok := row[mi].(float64)
		if !ok {
			continue
		}
		if !contains(xs, x) {
			xs = append(xs, x)
		}
		if !contains(ss, s) {
			ss = append(ss, s)
		}
		vals[[2]string{x, s}] += v
	}
	sort.SliceStable(xs, func(a, b int) bool { return compareValues(xs[a], xs[b]) < 0 })
	sort.SliceStable(ss, func(a, b int) bool { return compareValues(ss[a], ss[b]) < 0 })
	if len(xs) == 0 {
		return `<p class="muted">No data for this chart.</p>`
	}
	lo, hi := 0.0, 0.0
	for _, v := range vals {
		lo, hi = math.Min(lo, v), math.Max(hi, v)
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

	const W, H, left, right, topPad, bottomPad = 720.0, 300.0, 64.0, 12.0, 28.0, 30.0
	plotW, plotH := W-left-right, H-topPad-bottomPad
	y := func(v float64) float64 { return topPad + (top-v)/(top-bottom)*plotH }
	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" role="img" aria-label="%s">`, W, H, template.HTMLEscapeString(c.Title))
	for t := bottom; t <= top+step/2; t += step {
		fmt.Fprintf(&b, `<line class="grid" x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/>`, left, W-right, y(t), y(t))
		fmt.Fprintf(&b, `<text class="axis" x="%.1f" y="%.1f" text-anchor="end">%s</text>`, left-6, y(t)+3, axisLabel(t, col))
	}
	if bottom < 0 {
		fmt.Fprintf(&b, `<line class="zero" x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/>`, left, W-right, y(0), y(0))
	}
	gw := plotW / float64(len(xs))
	if len(ss) > 1 || ss[0] != "" {
		lx := left
		for i, s := range ss {
			fmt.Fprintf(&b, `<rect x="%.1f" y="6" width="10" height="10" rx="2" style="fill:%s"/><text class="legend" x="%.1f" y="15">%s</text>`,
				lx, seriesColors[i%len(seriesColors)], lx+14, template.HTMLEscapeString(dimText(s, c.Series)))
			lx += 24 + 7*float64(len(dimText(s, c.Series)))
		}
	}
	for i, x := range xs {
		cx := left + gw*float64(i) + gw/2
		fmt.Fprintf(&b, `<text class="axis" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`, cx, H-bottomPad+16, template.HTMLEscapeString(dimText(x, c.X)))
	}
	if c.Type == "line" {
		for si, s := range ss {
			var pts []string
			for i, x := range xs {
				v, ok := vals[[2]string{x, s}]
				if !ok {
					continue
				}
				pts = append(pts, fmt.Sprintf("%.1f,%.1f", left+gw*float64(i)+gw/2, y(v)))
			}
			color := seriesColors[si%len(seriesColors)]
			fmt.Fprintf(&b, `<polyline fill="none" stroke-width="2.5" style="stroke:%s" points="%s"/>`, color, strings.Join(pts, " "))
			for _, p := range pts {
				xy := strings.Split(p, ",")
				fmt.Fprintf(&b, `<circle cx="%s" cy="%s" r="3.5" style="fill:%s"/>`, xy[0], xy[1], color)
			}
		}
	} else {
		n := float64(len(ss))
		bw := math.Min(28, (gw*0.8)/n)
		for i, x := range xs {
			start := left + gw*float64(i) + gw/2 - bw*n/2
			for si, s := range ss {
				v, ok := vals[[2]string{x, s}]
				if !ok {
					continue
				}
				y0, y1 := y(math.Max(v, 0)), y(math.Min(v, 0))
				fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" style="fill:%s"><title>%s %s: %s</title></rect>`,
					start+bw*float64(si)+1, y0, bw-2, math.Max(y1-y0, 0.5), seriesColors[si%len(seriesColors)],
					template.HTMLEscapeString(dimText(x, c.X)), template.HTMLEscapeString(dimText(s, c.Series)), formatFull(v, col))
			}
		}
	}
	b.WriteString(`</svg>`)
	return b.String()
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
		return err
	}
	for _, row := range r.Rows {
		rec := make([]string, len(row))
		for i, v := range row {
			switch x := v.(type) {
			case string:
				rec[i] = x
			case float64:
				rec[i] = fmt.Sprintf("%g", x)
			}
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
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
