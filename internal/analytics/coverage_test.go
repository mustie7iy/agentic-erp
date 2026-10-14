package analytics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// --- model -------------------------------------------------------------------

func TestLoadModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.yaml")
	if err := os.WriteFile(path, defaultModelYAML, 0o644); err != nil {
		t.Fatalf("write the model: %v", err)
	}
	m, err := LoadModel(path)
	if err != nil || m.Metric("revenue") == nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := LoadModel(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("missing file must error")
	}
	if m.AllowedDimensions("nope") != nil {
		t.Fatal("unknown metric has no dimensions")
	}
}

func TestModelValidationMessages(t *testing.T) {
	src := func(extra string) string {
		return "dimensions: [{name: d}]\nmetrics:\n  - {name: a, type: money, source: t, aggregate: sum, dimensions: [d]}\n" + extra
	}
	cases := map[string]string{
		"semantic model: yaml":     "metrics: [",
		"out of range":             "calendar: {fiscal_year_start_month: 13}\n" + src(""),
		"duplicate metric":         src("  - {name: a, type: money, source: t, aggregate: sum}\n"),
		"derives from unknown":     src("  - {name: b, type: money, derived: {op: subtract, left: a, right: zz}}\n"),
		"unknown op":               src("  - {name: b, type: money, derived: {op: multiply, left: a, right: a}}\n"),
		"unknown aggregate":        src("  - {name: b, type: money, source: t, aggregate: avg}\n"),
		"want 1 or -1":             src("  - {name: b, type: money, source: t, aggregate: sum, sign: 2}\n"),
		"undeclared dimension":     src("  - {name: b, type: money, source: t, aggregate: sum, dimensions: [zz]}\n"),
		"needs a source":           src("  - {name: b, type: money}\n"),
		"points to unknown metric": src("defaults: {sales: zz}\n"),
	}
	for want, doc := range cases {
		t.Run(want, func(t *testing.T) {
			_, err := ParseModel([]byte(doc))
			if err == nil || (want != "semantic model: yaml" && !strings.Contains(err.Error(), want)) {
				t.Errorf("got %v, want an error containing %q", err, want)
			}
		})
	}
	m, err := ParseModel([]byte(src("")))
	if err != nil {
		t.Fatalf("parse a valid model: %v", err)
	}
	if m.Calendar.FiscalYearStartMonth != 1 {
		t.Errorf("default fiscal year start month: got %d, want 1", m.Calendar.FiscalYearStartMonth)
	}
	if m.Metric("a").Sign != 1 {
		t.Errorf("default sign: got %d, want 1", m.Metric("a").Sign)
	}
}

// With several invalid defaults the error names the first word in sorted
// order, every time; a map range would report a different one per run.
func TestModelDefaultErrorIsDeterministic(t *testing.T) {
	doc := "dimensions: [{name: d}]\nmetrics:\n  - {name: a, type: money, source: t, aggregate: sum, dimensions: [d]}\n" +
		"defaults: {zeta: z1, alpha: z2, mid: z3, beta: z4, omega: z5, kappa: z6, delta: z7, gamma: z8}\n"
	want := `semantic model: default "alpha" points to unknown metric z2`
	for i := range 50 {
		_, err := ParseModel([]byte(doc))
		if err == nil || err.Error() != want {
			t.Fatalf("run %d: got %v, want %s", i, err, want)
		}
	}
}

// --- agent ---------------------------------------------------------------------

func TestNewAgentAndToolText(t *testing.T) {
	t.Setenv("ERP_AGENT_MODEL", "claude-haiku-5-5")
	a := NewAgent()
	if a.Model != "claude-haiku-5-5" {
		t.Errorf("env model: got %s, want claude-haiku-5-5", a.Model)
	}
	if a.Today == nil {
		t.Error("clock: got nil, want one")
	}
	t.Setenv("ERP_AGENT_MODEL", "")
	if a := NewAgent(); a.Model != defaultModel {
		t.Fatalf("default model: got %s, want %s", a.Model, defaultModel)
	}
	r, err := toolText("plain")
	if err != nil {
		t.Fatalf("string result: %v", err)
	}
	if r.OfText.Text != "plain" {
		t.Errorf("string result: got %q, want plain", r.OfText.Text)
	}
	if _, err := toolText(math.Inf(1)); err == nil {
		t.Fatal("unmarshalable value must error")
	}
}

func llmAgent(srv *llmtest.Server) *Agent {
	r := srv.Runner()
	return &Agent{Client: r.Client, Model: r.Model}
}

func TestAgentToolSurface(t *testing.T) {
	s := session(t, us)
	var built *Report
	tools := (&Agent{}).tools(s, &built, "x")
	if len(tools) != 3 {
		t.Fatal("three tools")
	}
	for i, tl := range tools {
		if tl == nil {
			t.Fatalf("tool %d: got nil, want a tool", i)
		}
	}
}

func TestAskPaths(t *testing.T) {
	// The API fails.
	down := llmtest.New(t)
	if _, err := llmAgent(down).Ask(context.Background(), session(t, us), "q"); err == nil {
		t.Fatal("API error must propagate")
	}
	// The model declines.
	refuse := llmtest.New(t, llmtest.Step{StopReason: "refusal"})
	_, err := llmAgent(refuse).Ask(context.Background(), session(t, us), "q")
	pgtest.WantErr(t, err, "declined")
	// The model answers without building a dashboard; a failing query and a
	// bad filter go back to the model as tool errors.
	none := llmtest.New(t,
		llmtest.Tool("list_metrics", map[string]any{}),
		llmtest.Step{Tool: "run_query", Input: func(string) any { return map[string]any{"measures": []string{"nope"}} }, Expect: "defaults"},
		llmtest.Step{Text: "I could not answer.", Expect: "error: unknown metric"},
	)
	_, err = llmAgent(none).Ask(context.Background(), session(t, us), "q")
	pgtest.WantErr(t, err, "did not produce")
	// A spec without a question takes the user's question; a filtered query is accepted.
	s := session(t, us)
	ok := llmtest.New(t,
		llmtest.Tool("run_query", map[string]any{"measures": []string{"revenue"}, "by": []string{"fiscal_year"},
			"where": []map[string]any{{"dim": "fiscal_year", "op": "in", "values": []string{"2026"}}}}),
		llmtest.ToolFn("build_dashboard", func(body string) any {
			qid := qidRe.FindString(body)
			return map[string]any{"spec": Spec{Title: "Revenue 2026", Tables: []Table{{Title: "t", QueryID: qid}}}}
		}),
		llmtest.Final("Done."),
	)
	a := llmAgent(ok)
	a.Today = func() time.Time { return time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC) }
	ans, err := a.Ask(context.Background(), s, "What was revenue in 2026?")
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	if ans.Report.Question != "What was revenue in 2026?" {
		t.Errorf("report question: got %q, want the user's question", ans.Report.Question)
	}
	if ans.Report.Spec.Question != ans.Report.Question {
		t.Errorf("spec question: got %q, want %q", ans.Report.Spec.Question, ans.Report.Question)
	}
	if ans.Summary != "Done." {
		t.Errorf("summary: got %q, want Done.", ans.Summary)
	}
}

// --- dashboard validation and formatting -------------------------------------------

func fixtureResult() *Result {
	return &Result{
		ID: "q1",
		Columns: []Column{
			{Name: "fiscal_year", Label: "Fiscal year", Kind: "dimension"},
			{Name: "fiscal_month", Label: "Fiscal month", Kind: "dimension"},
			{Name: "revenue", Label: "Revenue", Kind: "measure", Type: Money, Currency: "USD"},
			{Name: "margin", Label: "Margin", Kind: "measure", Type: Ratio},
			{Name: "orders", Label: "Orders", Kind: "measure", Type: Count},
		},
		Rows: []Row{
			{"2025", "1", 1000.0, 0.20, 3.0},
			{"2025", "2", 2000.0, 0.25, 4.0},
			{"2026", "1", 1500.0, 0.30, 5.0},
			{"2026", "2", -500.0, nil, 1.0},
			{"2026", "3", nil, 0.10, 2.0},
		},
	}
}

func TestValidateEveryProblem(t *testing.T) {
	r := fixtureResult()
	results := map[string]*Result{"q1": r}
	spec := Spec{
		Tiles: []Tile{
			{Label: "missing query", QueryID: "q9", Measure: "revenue"},
			{Label: "not a measure", QueryID: "q1", Measure: "fiscal_year"},
			{Label: "bad where", QueryID: "q1", Measure: "revenue", Where: map[string]string{"revenue": "1"}},
			{Label: "no rows", QueryID: "q1", Measure: "revenue", Where: map[string]string{"fiscal_year": "1999"}},
			{Label: "bad compare", QueryID: "q1", Measure: "revenue", Where: map[string]string{"fiscal_year": "2025"}, CompareWhere: map[string]string{"orders": "1"}},
			{Label: "empty compare", QueryID: "q1", Measure: "revenue", Where: map[string]string{"fiscal_year": "2025"}, CompareWhere: map[string]string{"fiscal_year": "1999"}},
		},
		Charts: []Chart{
			{Title: "missing", QueryID: "q9"},
			{Title: "bad", Type: "pie", QueryID: "q1", X: "revenue", Series: "orders", Measure: "fiscal_year"},
		},
		Tables: []Table{
			{Title: "missing", QueryID: "q9", Compare: &Compare{}},
			{Title: "bad compare", QueryID: "q1", Compare: &Compare{Dim: "revenue", Measure: "fiscal_year"}},
		},
	}
	got := strings.Join(spec.Validate(results), "\n")
	for _, want := range []string{
		"title is required", "unknown query_id", "measure \"fiscal_year\" is not in query", "not a dimension of this query",
		"no rows match", "compare:", "no rows match compare_where", "type must be bar or line", "x \"revenue\" is not a dimension",
		"series \"orders\" is not a dimension", "compare dim \"revenue\"", "compare measure \"fiscal_year\"",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing problem %q in:\n%s", want, got)
		}
	}
	empty := Spec{Title: "x"}
	p := empty.Validate(results)
	if len(p) != 1 {
		t.Fatalf("empty spec: got %d problems %v, want 1", len(p), p)
	}
	if !strings.Contains(p[0], "at least one") {
		t.Errorf("empty spec: got %q, want it to ask for at least one element", p[0])
	}
}

// The commentary is checked against every result in a map; the problems
// it reports must not depend on the order the map is ranged in.
func TestValidateCommentaryIsDeterministic(t *testing.T) {
	results := map[string]*Result{}
	for i, id := range []string{"q1", "q2", "q3", "q4", "q5", "q6", "q7", "q8"} {
		r := fixtureResult()
		r.ID = id
		for _, row := range r.Rows {
			if v, ok := row[2].(float64); ok {
				row[2] = v + float64(i)*10000
			}
		}
		results[id] = r
	}
	spec := Spec{Title: "t", Tables: []Table{{QueryID: "q1"}},
		Commentary: "Revenue was 41,000.00 and 1,000.00, not 777.77, 888.88 or 999.99."}
	want := "commentary cites figures not found in any query result: 777.77, 888.88, 999.99."
	for i := range 50 {
		got := spec.Validate(results)
		if len(got) != 1 || !strings.HasPrefix(got[0], want) {
			t.Fatalf("run %d: got %q, want one problem starting %q", i, got, want)
		}
	}
}

func TestSum(t *testing.T) {
	r := fixtureResult()
	if _, _, err := r.sum("nope", nil); err == nil {
		t.Fatal("unknown measure")
	}
	v, n, err := r.sum("revenue", map[string]string{"fiscal_year": "2026"})
	if err != nil {
		t.Fatalf("money sum: %v", err)
	}
	if n != 2 {
		t.Errorf("money sum skips nil values: got %d rows, want 2", n)
	}
	if v != 1000 {
		t.Errorf("money sum: got %v, want 1000", v)
	}
	v, n, err = r.sum("margin", map[string]string{"fiscal_year": "2025", "fiscal_month": "2"})
	if err != nil {
		t.Fatalf("single-row ratio: %v", err)
	}
	if n != 1 {
		t.Errorf("single-row ratio: got %d rows, want 1", n)
	}
	if v != 0.25 {
		t.Errorf("single-row ratio: got %v, want 0.25", v)
	}
	_, n, err = r.sum("margin", map[string]string{"fiscal_year": "2025"})
	if err == nil {
		t.Error("a ratio over several rows: got no error, want it refused")
	}
	if n != 2 {
		t.Errorf("a ratio over several rows: got %d rows, want 2", n)
	}
	_, n, err = r.sum("margin", map[string]string{"fiscal_year": "2026", "fiscal_month": "2"})
	if err != nil {
		t.Errorf("a nil ratio: %v", err)
	}
	if n != 0 {
		t.Errorf("a nil ratio counts as no row: got %d rows, want 0", n)
	}
}

func TestFormatting(t *testing.T) {
	money := Column{Type: Money, Currency: "USD"}
	cases := []struct {
		got, want string
	}{
		{formatValue(0.256, Column{Type: Ratio}), "25.6%"},
		{formatValue(12345, Column{Type: Count}), "12,345"},
		{formatValue(2.5e9, money), "$2.50B"},
		{formatValue(-3.25e6, money), "−$3.25M"},
		{formatValue(45_600, money), "$45.6K"},
		{formatValue(-1234.5, money), "−$1,234.50"},
		{formatValue(10, Column{Type: Money, Currency: "EUR"}), "€10.00"},
		{formatValue(10, Column{Type: Money, Currency: "GBP"}), "£10.00"},
		{formatValue(10, Column{Type: Money}), "10.00"},
		{formatValue(10, Column{Type: Money, Currency: "CHF"}), "CHF 10.00"},
		{formatFull(0.5, Column{Type: Ratio}), "50.0%"},
		{formatFull(1234, Column{Type: Count}), "1,234"},
		{formatFull(-1234567.891, money), "−1,234,567.89"},
		{formatFull(12, money), "12.00"},
		{groupThousands("-1234567"), "-1,234,567"},
		{groupThousands("12"), "12"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got %q, want %q", c.got, c.want)
			}
		})
	}
	for _, c := range []struct {
		name       string
		cur, prev  float64
		col        Column
		delta, pct string
		up         bool
	}{
		{"change up", 110, 100, money, "+$10.00", "+10.0%", true},
		{"change down", 90, 100, money, "−$10.00", "−10.0%", false},
		{"ratio change", 0.3, 0.25, Column{Type: Ratio}, "+5.0 pts", "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, p, up := change(c.cur, c.prev, c.col)
			if d != c.delta {
				t.Errorf("delta: got %s, want %s", d, c.delta)
			}
			if p != c.pct {
				t.Errorf("percent: got %q, want %q", p, c.pct)
			}
			if up != c.up {
				t.Errorf("up: got %v, want %v", up, c.up)
			}
		})
	}
	if d, p, _ := change(5, 0, money); d != "+$5.00" {
		t.Errorf("change from zero: got %s, want +$5.00", d)
	} else if p != "" {
		t.Errorf("no percent from zero: got %q, want none", p)
	}
	d, _, up := changeFull(90_000, 100_000, money)
	if d != "−10,000.00" {
		t.Errorf("full change down: got %s, want −10,000.00", d)
	}
	if up {
		t.Error("full change down: got up, want down")
	}
	if d, _, _ := changeFull(100_500, 100_000, money); d != "+500.00" {
		t.Errorf("full change up: %s", d)
	}
	if d, _, _ := changeFull(0.2, 0.3, Column{Type: Ratio}); d != "-10.0 pts" {
		t.Errorf("full ratio change: %s", d)
	}
}

// --- facts --------------------------------------------------------------------

func TestFiscalCalendarErrors(t *testing.T) {
	if _, _, err := fiscal("2026-13-01", 1); err == nil {
		t.Fatal("bad date")
	}
}

// --- query engine ----------------------------------------------------------------

func TestQueryOperatorsAndBranches(t *testing.T) {
	s := session(t, us)
	run := func(q Query) *Result {
		t.Helper()
		r, err := s.Run(q)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	months := func(q Query) int { return len(run(q).Rows) }
	base := Query{Measures: []string{"revenue"}, By: []string{"fiscal_year", "fiscal_month"}}
	withWhere := func(f ...Filter) Query { q := base; q.Where = f; return q }
	if n := months(withWhere(Filter{"fiscal_year", "in", []string{"2025"}})); n != 12 {
		t.Errorf("in: %d", n)
	}
	if n := months(withWhere(Filter{"fiscal_year", "not_in", []string{"2025"}})); n != 9 {
		t.Errorf("not_in: %d", n)
	}
	if n := months(withWhere(Filter{"fiscal_month", "<=", []string{"3"}})); n != 6 {
		t.Errorf("<=: %d", n)
	}
	if n := months(withWhere(Filter{"fiscal_month", ">=", []string{"10"}})); n != 3 {
		t.Errorf(">=: %d", n)
	}
	if n := months(withWhere(Filter{"fiscal_month", "between", []string{"2", "3"}})); n != 4 {
		t.Errorf("between: %d", n)
	}
	// Ascending order.
	asc := run(Query{Measures: []string{"revenue"}, By: []string{"fiscal_month"}, Where: []Filter{{"fiscal_year", "in", []string{"2025"}}}, OrderBy: "revenue"})
	if asc.Rows[0][1].(float64) > asc.Rows[1][1].(float64) {
		t.Fatal("ascending order")
	}
	// Distinct counts and derived metrics with missing inputs.
	cnt := run(Query{Measures: []string{"invoice_count", "purchase_order_count"}, By: []string{"fiscal_year"}})
	if cnt.Rows[0][1].(float64) == 0 {
		t.Fatal("count_distinct")
	}
	margin := run(Query{Measures: []string{"operating_margin", "operating_profit"}, By: []string{"fiscal_year", "fiscal_month", "document_type"}})
	nils := 0
	for _, row := range margin.Rows {
		if row[3] == nil {
			nils++
		}
	}
	if nils == 0 {
		t.Fatal("a margin without revenue in the group must be empty (nil)")
	}
	// Measures with different dimensions restrict the allowed breakdowns.
	if _, err := s.Run(Query{Measures: []string{"revenue", "invoice_value"}, By: []string{"vendor"}}); err == nil {
		t.Fatal("vendor is not shared by revenue and invoice_value")
	}
	errs := []Query{
		{},
		{Measures: []string{"revenue"}, Where: []Filter{{"vendor", "in", []string{"x"}}}},
		{Measures: []string{"revenue"}, By: []string{"company"}, Where: []Filter{{"company", "in", nil}}},
		{Measures: []string{"revenue"}, By: []string{"company"}, Where: []Filter{{"fiscal_month", "<=", []string{"1", "2"}}}},
		{Measures: []string{"revenue"}, By: []string{"company"}, Where: []Filter{{"fiscal_month", "between", []string{"1"}}}},
	}
	for i, q := range errs {
		t.Run(fmt.Sprintf("query %d", i), func(t *testing.T) {
			if _, err := s.Run(q); err == nil {
				t.Errorf("got no error for %+v, want it refused", q)
			}
		})
	}
}

func TestComparisonHelpers(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{{"2", "10", -1}, {"10", "2", 1}, {"1", "1.0", 0}, {"a", "b", -1}} {
		t.Run(fmt.Sprintf("compareValues(%s,%s)", c.a, c.b), func(t *testing.T) {
			if got := compareValues(c.a, c.b); got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
	cases := []struct {
		a, b any
		want int
	}{
		{1.0, 2.0, -1}, {2.0, 1.0, 1}, {1.0, 1.0, 0}, {1.0, nil, 1}, {1.0, "x", 1},
		{"2", "10", -1}, {"b", nil, 1},
		{nil, nil, 0}, {nil, 1.0, -1},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("compareCells(%v,%v)", c.a, c.b), func(t *testing.T) {
			if got := compareCells(c.a, c.b); got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
	m, _ := DefaultModel()
	for dim, want := range map[string]string{"nope": "nope", "company": "Company code"} {
		if got := m.dimLabel(dim); got != want {
			t.Errorf("dimLabel(%s): got %q, want %q", dim, got, want)
		}
	}
	for cur, want := range map[string]int{"JPY": 0, "KWD": 3, "USD": 2} {
		t.Run("currencyDecimals("+cur+")", func(t *testing.T) {
			if got := currencyDecimals(cur); got != want {
				t.Errorf("got %d, want %d", got, want)
			}
		})
	}
	for _, c := range []struct {
		a, b []string
		want bool
	}{{nil, nil, false}, {[]string{"a"}, []string{"a", "b"}, false}, {[]string{"a", "b"}, []string{"a", "c"}, true}} {
		if got := differInOne(c.a, c.b); got != c.want {
			t.Errorf("differInOne(%v, %v): got %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// A JPY company's amounts are scaled by its minor unit.
func TestZeroDecimalCurrency(t *testing.T) {
	dims := func(typ string) map[string]string {
		return map[string]string{"company": "3000", "account_type": typ, "fiscal_year": "2026", "fiscal_month": "1", "posting_date": "2026-01-05"}
	}
	f := &Facts{Companies: map[string]string{"3000": "JPY"}, Tables: map[string][]Fact{"gl_lines": {
		{ID: "J1", Dims: dims("asset"), Amount: 5000, Currency: "JPY"}, {ID: "J1", Dims: dims("revenue"), Amount: -5000, Currency: "JPY"}}}}
	m, _ := DefaultModel()
	r, err := Execute(m, f, Principal{ID: "u"}, Query{Measures: []string{"revenue"}})
	if err != nil {
		t.Fatalf("JPY revenue: %v", err)
	}
	if got := r.Rows[0][0].(float64); got != 5000 {
		t.Errorf("JPY revenue: got %v, want 5000", got)
	}
	if r.Columns[0].Currency != "JPY" {
		t.Errorf("JPY revenue currency: got %s, want JPY", r.Columns[0].Currency)
	}
}

// A count metric that sums fact amounts sums minor units of each fact's
// currency, so facts in JPY (exponent 0) and USD (exponent 2) cannot be
// added; the refusal must not depend on map order, so it is repeated.
func TestCountSumRefusesMixedCurrencies(t *testing.T) {
	m, err := ParseModel([]byte("dimensions: [{name: company}]\nmetrics:\n" +
		"  - {name: units, type: count, source: t, aggregate: sum, dimensions: [company]}\n" +
		"  - {name: docs, type: count, source: t, aggregate: count_distinct, dimensions: [company]}\n" +
		"  - {name: docs_share, type: ratio, derived: {op: divide, left: docs, right: docs}}\n" +
		"  - {name: units_share, type: ratio, derived: {op: divide, left: units, right: units}}\n" +
		"  - {name: usd_units, type: money, source: t, aggregate: sum, filter: {company: [\"1000\"]}, dimensions: [company]}\n" +
		"  - {name: jpy_units, type: money, source: t, aggregate: sum, filter: {company: [\"3000\"]}, dimensions: [company]}\n" +
		"  - {name: units_gap, type: money, derived: {op: subtract, left: usd_units, right: jpy_units}}\n" +
		"  - {name: units_rate, type: ratio, derived: {op: divide, left: usd_units, right: jpy_units}}\n" +
		"  - {name: usd_per_doc, type: money, derived: {op: divide, left: usd_units, right: docs}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	fact := func(id, company, cur string) Fact {
		return Fact{ID: id, Dims: map[string]string{"company": company}, Amount: 500, Currency: cur}
	}
	f := &Facts{Tables: map[string][]Fact{"t": {fact("A", "3000", "JPY"), fact("B", "1000", "USD")}}}
	for i := range 50 {
		r, err := Execute(m, f, Principal{ID: "u"}, Query{Measures: []string{"units"}})
		if err == nil {
			t.Fatalf("run %d: got rows %v, want a mixed-currency refusal", i, r.Rows)
		}
		if want := "units would add amounts in JPY and USD together"; !strings.Contains(err.Error(), want) {
			t.Fatalf("run %d: got %v, want %q", i, err, want)
		}
	}
	t.Run("a distinct count spans currencies", func(t *testing.T) {
		r, err := Execute(m, f, Principal{ID: "u"}, Query{Measures: []string{"docs"}})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := r.Rows[0][0], 2.0; got != want {
			t.Errorf("got %v, want %v", got, want)
		}
	})
	// A ratio of distinct counts adds no amounts, so its cell may span
	// currencies.
	t.Run("a count ratio spans currencies", func(t *testing.T) {
		r, err := Execute(m, f, Principal{ID: "u"}, Query{Measures: []string{"docs_share"}})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := r.Rows[0][0], 1.0; got != want {
			t.Errorf("got %v, want %v", got, want)
		}
	})
	// A derived metric is refused when its sums together read several
	// currencies: one sum spanning them, or each operand in its own.
	for _, name := range []string{"units_share", "units_gap", "units_rate"} {
		t.Run(name+" over currencies is refused", func(t *testing.T) {
			_, err := Execute(m, f, Principal{ID: "u"}, Query{Measures: []string{name}})
			pgtest.CheckErr(t, err, name+" would add amounts in JPY and USD together")
		})
	}
	// The column's currency is that of the amounts it adds, not of the
	// documents a distinct count spans.
	t.Run("a money column keeps the currency of its sums", func(t *testing.T) {
		r, err := Execute(m, f, Principal{ID: "u"}, Query{Measures: []string{"usd_per_doc"}})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := r.Columns[0].Currency, "USD"; got != want {
			t.Errorf("currency: got %q, want %q", got, want)
		}
		if got, want := r.Rows[0][0], 2.5; got != want { // USD 5.00 over 2 documents
			t.Errorf("value: got %v, want %v", got, want)
		}
	})
	t.Run("one currency per group", func(t *testing.T) {
		r, err := Execute(m, f, Principal{ID: "u"}, Query{Measures: []string{"units"}, By: []string{"company"}})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := r.Rows[0][1], 5.0; got != want { // USD 500 minor units
			t.Errorf("company 1000: got %v, want %v", got, want)
		}
		if got, want := r.Rows[1][1], 500.0; got != want { // JPY has no minor unit
			t.Errorf("company 3000: got %v, want %v", got, want)
		}
	})
}

// --- narrative ------------------------------------------------------------------

func TestNarrativeEdgeCases(t *testing.T) {
	huge := strings.Repeat("9", 400)
	if figs := extractFigures("value " + huge + " units"); len(figs) != 0 {
		t.Fatal("a number too large to parse is ignored")
	}
	r := fixtureResult()
	if bad := CheckNarrative("Margin was 30.0% and revenue 1,500.00.", []*Result{r}); len(bad) != 0 {
		t.Fatalf("figures from rows with nil cells: %v", bad)
	}
}

// --- rendering ------------------------------------------------------------------

func TestRenderingBranches(t *testing.T) {
	r := fixtureResult()
	plain := tableView(Table{Title: "all"}, r)
	if plain.Header[2] != "Revenue (USD)" {
		t.Errorf("plain table header: got %q, want Revenue (USD)", plain.Header[2])
	}
	if got := plain.Rows[4][2].Text; got != "—" {
		t.Errorf("plain table missing value: got %q, want —", got)
	}
	if got := plain.Rows[0][1].Text; got != "Jan" {
		t.Errorf("plain table month: got %q, want Jan", got)
	}
	cmp := tableView(Table{Title: "cmp", Compare: &Compare{Dim: "fiscal_year", Baseline: "2025", Current: "2026", Measure: "revenue"}}, r)
	var neg, missing bool
	for _, row := range cmp.Rows {
		for _, c := range row {
			neg = neg || strings.Contains(c.Class, "neg")
		}
		missing = missing || row[1].Text == "—"
	}
	if !neg {
		t.Errorf("compare table: got no decrease in %+v, want one", cmp.Rows)
	}
	if !missing {
		t.Errorf("compare table: got no missing baseline in %+v, want one", cmp.Rows)
	}
	// Totals turn negative when the current year is lower overall; two key columns.
	down := &Result{Columns: []Column{{Name: "company", Kind: "dimension"}, {Name: "region", Kind: "dimension"}, {Name: "fiscal_year", Kind: "dimension"},
		{Name: "v", Kind: "measure", Type: Money}},
		Rows: []Row{{"1000", "a", "2025", 100.0}, {"1000", "a", "2026", 50.0}, {"1000", "b", "2025", 100.0}, {"1000", "b", "2026", 60.0},
			{"1000", "01", "2025", 1.0}, {"1000", "1", "2025", 1.0}}}
	tot := tableView(Table{Compare: &Compare{Dim: "fiscal_year", Baseline: "2025", Current: "2026", Measure: "v"}}, down)
	last := tot.Rows[len(tot.Rows)-1]
	if len(last) != 6 {
		t.Fatalf("total row: got %d cells %+v, want 6", len(last), last)
	}
	if last[0].Text != "Total" {
		t.Errorf("total row label: got %q, want Total", last[0].Text)
	}
	if !strings.Contains(last[4].Class, "neg") {
		t.Errorf("total row change class: got %q, want neg", last[4].Class)
	}
	for name, got := range map[string]string{"numCell(0, false)": numCell(0, false, Column{}).Text, "cellText(nil)": cellText(nil, Column{}),
		"cellText(true)": cellText(true, Column{})} {
		if got != "—" {
			t.Errorf("%s: got %q, want —", name, got)
		}
	}
	for _, c := range []struct{ v, dim, want string }{{"2", "fiscal_quarter", "Q2"}, {"13", "fiscal_month", "13"}, {"", "vendor", "(none)"}} {
		if got := dimText(c.v, c.dim); got != c.want {
			t.Errorf("dimText(%q, %s): got %q, want %q", c.v, c.dim, got, c.want)
		}
	}

	line := chartSVG(Chart{Title: "t", Type: "line", X: "fiscal_month", Series: "fiscal_year", Measure: "revenue"}, r)
	for _, want := range []string{"<polyline", `class="zero"`} {
		if !strings.Contains(line, want) {
			t.Errorf("line chart with negative values: got %s, want it to contain %s", line, want)
		}
	}
	bar := chartSVG(Chart{Title: "t", Type: "bar", X: "fiscal_month", Series: "fiscal_year", Measure: "revenue"}, r)
	if !strings.Contains(bar, "<rect") {
		t.Fatal("bar chart with gaps")
	}
	if !strings.Contains(chartSVG(Chart{X: "fiscal_month", Measure: "revenue"}, &Result{Columns: r.Columns}), "No data") {
		t.Fatal("empty chart")
	}
	zero := &Result{Columns: []Column{{Name: "x", Kind: "dimension"}, {Name: "v", Kind: "measure", Type: Ratio}}, Rows: []Row{{"a", 0.0}}}
	if !strings.Contains(chartSVG(Chart{Type: "bar", X: "x", Measure: "v"}, zero), "<svg") {
		t.Fatal("all-zero chart")
	}
	for _, c := range []struct{ in, want float64 }{{0, 0}, {3, 5}, {7, 10}, {0.15, 0.2}} {
		t.Run(fmt.Sprintf("niceStep(%v)", c.in), func(t *testing.T) {
			if got := niceStep(c.in); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
	for _, c := range []struct {
		v    float64
		col  Column
		want string
	}{{0.25, Column{Type: Ratio}, "25%"}, {2.5e6, Column{}, "2.5M"}, {-1500, Column{}, "−1.5K"}, {40, Column{}, "40"}} {
		t.Run(fmt.Sprintf("axisLabel(%v)", c.v), func(t *testing.T) {
			if got := axisLabel(c.v, c.col); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestWriteCSV(t *testing.T) {
	r := fixtureResult()
	var buf bytes.Buffer
	if err := WriteCSV(&buf, r); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 6 {
		t.Fatalf("csv: got %d lines, want 6:\n%s", len(lines), buf.String())
	}
	if lines[0] != "fiscal_year,fiscal_month,revenue,margin,orders" {
		t.Errorf("csv header: got %q, want fiscal_year,fiscal_month,revenue,margin,orders", lines[0])
	}
	if lines[4] != "2026,2,-500,,1" {
		t.Errorf("csv line 5: got %q, want 2026,2,-500,,1", lines[4])
	}
	if err := WriteCSV(failWriter{}, r); err == nil {
		t.Fatal("flush error must be returned")
	}
	wide := &Result{Columns: []Column{{Name: strings.Repeat("h", 5000)}}}
	if err := WriteCSV(failWriter{}, wide); err == nil {
		t.Fatal("header write error must be returned")
	}
	long := &Result{Columns: []Column{{Name: "a"}}, Rows: []Row{{strings.Repeat("x", 5000)}}}
	if err := WriteCSV(failWriter{}, long); err == nil {
		t.Fatal("row write error must be returned")
	}
}

// --- sessions and reports ----------------------------------------------------------

func TestSessionErrorPaths(t *testing.T) {
	dir := t.TempDir()

	if _, err := LoadReport(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("missing report")
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{"), 0o644); err != nil {
		t.Fatalf("write a corrupt report: %v", err)
	}
	if _, err := LoadReport(filepath.Join(dir, "bad.json")); err == nil {
		t.Fatal("corrupt report")
	}

	ok := session(t, us)
	r, _ := ok.Run(salesQuery())
	rep, _ := ok.Build(salesSpec(r.ID), "x")
	blocked := filepath.Join(dir, "file")
	if err := os.WriteFile(blocked, nil, 0o644); err != nil {
		t.Fatalf("write the blocking file: %v", err)
	}
	if _, err := rep.Save(filepath.Join(blocked, "sub"), ok.Results); err == nil {
		t.Fatal("unwritable directory")
	}
	jsonDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(jsonDir, rep.ID+".json"), 0o755); err != nil {
		t.Fatalf("block the report file: %v", err)
	}
	_, err := rep.Save(jsonDir, ok.Results)
	pgtest.WantErr(t, err, "analytics: save report "+rep.ID+" definition")
	htmlDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(htmlDir, rep.ID+".html"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = rep.Save(htmlDir, ok.Results)
	pgtest.WantErr(t, err, "analytics: save report "+rep.ID+" page")
	pgtest.WantErr(t, Render(failWriter{}, &rep.Spec, ok.Results, rep.Principal), "analytics: render page")

	// Rerun: a query that no longer runs, and a spec that no longer validates.
	broken := &Report{Spec: salesSpec("monthly"), Queries: map[string]Query{"monthly": {Measures: []string{"gone"}}}}
	if _, _, err := ok.Rerun(broken); err == nil {
		t.Fatal("rerun with a failing query")
	}
	stale := &Report{Spec: salesSpec("other"), Queries: map[string]Query{"monthly": salesQuery()}}
	_, _, err = ok.Rerun(stale)
	pgtest.WantErr(t, err, "no longer valid")
}

// A series without a value for some x leaves a gap in both chart types.
func TestChartGaps(t *testing.T) {
	r := &Result{Columns: []Column{{Name: "m", Kind: "dimension"}, {Name: "y", Kind: "dimension"}, {Name: "v", Kind: "measure", Type: Money}},
		Rows: []Row{{"1", "2025", 10.0}, {"2", "2025", 12.0}, {"1", "2026", 11.0}}}
	for _, typ := range []string{"line", "bar"} {
		t.Run(typ, func(t *testing.T) {
			svg := chartSVG(Chart{Type: typ, X: "m", Series: "y", Measure: "v"}, r)
			if !strings.Contains(svg, "2026") {
				t.Errorf("got %s, want the second series 2026", svg)
			}
		})
	}
	if n := strings.Count(chartSVG(Chart{Type: "bar", X: "m", Series: "y", Measure: "v"}, r), "<title>"); n != 3 {
		t.Fatalf("three bars expected, got %d", n)
	}
}
