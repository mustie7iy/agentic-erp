package analytics

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/governance"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// --- model -------------------------------------------------------------------

func TestLoadModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.yaml")
	os.WriteFile(path, defaultModelYAML, 0o644)
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
		"undeclared dimension":     src("  - {name: b, type: money, source: t, aggregate: sum, dimensions: [zz]}\n"),
		"needs a source":           src("  - {name: b, type: money}\n"),
		"points to unknown metric": src("defaults: {sales: zz}\n"),
	}
	for want, doc := range cases {
		_, err := ParseModel([]byte(doc))
		if err == nil || (want != "semantic model: yaml" && !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: got %v", want, err)
		}
	}
	m, err := ParseModel([]byte(src("")))
	if err != nil || m.Calendar.FiscalYearStartMonth != 1 || m.Metric("a").Sign != 1 {
		t.Fatalf("defaults applied: %v %+v", err, m)
	}
}

// --- agent ---------------------------------------------------------------------

func TestNewAgentAndToolText(t *testing.T) {
	t.Setenv("ERP_AGENT_MODEL", "claude-haiku-5-5")
	if a := NewAgent(); a.Model != "claude-haiku-5-5" || a.Today == nil {
		t.Fatalf("env model: %+v", a)
	}
	t.Setenv("ERP_AGENT_MODEL", "")
	if a := NewAgent(); a.Model != defaultModel {
		t.Fatal("default model")
	}
	if r, err := toolText("plain"); err != nil || r.OfText.Text != "plain" {
		t.Fatal("string result")
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
	for _, tl := range tools {
		if tl == nil {
			t.Fatal("tool construction failed")
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
	if _, err := llmAgent(refuse).Ask(context.Background(), session(t, us), "q"); err == nil || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("refusal: %v", err)
	}
	// The model answers without building a dashboard; a failing query and a
	// bad filter go back to the model as tool errors.
	none := llmtest.New(t,
		llmtest.Tool("list_metrics", map[string]any{}),
		llmtest.Step{Tool: "run_query", Input: func(string) any { return map[string]any{"measures": []string{"nope"}} }, Expect: "defaults"},
		llmtest.Step{Text: "I could not answer.", Expect: "error: unknown metric"},
	)
	if _, err := llmAgent(none).Ask(context.Background(), session(t, us), "q"); err == nil || !strings.Contains(err.Error(), "did not produce") {
		t.Fatalf("no dashboard: %v", err)
	}
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
	if err != nil || ans.Report.Question != "What was revenue in 2026?" || ans.Report.Spec.Question != ans.Report.Question || ans.Summary != "Done." {
		t.Fatalf("answer: %v %+v", err, ans)
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
	if p := empty.Validate(results); len(p) != 1 || !strings.Contains(p[0], "at least one") {
		t.Fatalf("empty spec: %v", p)
	}
}

func TestSum(t *testing.T) {
	r := fixtureResult()
	if _, _, err := r.sum("nope", nil); err == nil {
		t.Fatal("unknown measure")
	}
	if v, n, err := r.sum("revenue", map[string]string{"fiscal_year": "2026"}); err != nil || n != 2 || v != 1000 {
		t.Fatalf("money sum skips nil values: %v %d %v", v, n, err)
	}
	if v, n, err := r.sum("margin", map[string]string{"fiscal_year": "2025", "fiscal_month": "2"}); err != nil || n != 1 || v != 0.25 {
		t.Fatalf("single-row ratio: %v %d %v", v, n, err)
	}
	if _, n, err := r.sum("margin", map[string]string{"fiscal_year": "2025"}); err == nil || n != 2 {
		t.Fatal("a ratio over several rows must be refused")
	}
	if _, n, err := r.sum("margin", map[string]string{"fiscal_year": "2026", "fiscal_month": "2"}); err != nil || n != 0 {
		t.Fatal("a nil ratio counts as no row")
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
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
	if d, p, up := change(110, 100, money); d != "+$10.00" || p != "+10.0%" || !up {
		t.Errorf("change up: %s %s %v", d, p, up)
	}
	if d, p, up := change(90, 100, money); d != "−$10.00" || p != "−10.0%" || up {
		t.Errorf("change down: %s %s %v", d, p, up)
	}
	if d, p, _ := change(5, 0, money); d != "+$5.00" || p != "" {
		t.Errorf("no percent from zero: %s %q", d, p)
	}
	if d, p, up := change(0.3, 0.25, Column{Type: Ratio}); d != "+5.0 pts" || p != "" || !up {
		t.Errorf("ratio change: %s %q", d, p)
	}
	if d, _, up := changeFull(90_000, 100_000, money); d != "−10,000.00" || up {
		t.Errorf("full change down: %s", d)
	}
	if d, _, _ := changeFull(100_500, 100_000, money); d != "+500.00" {
		t.Errorf("full change up: %s", d)
	}
	if d, _, _ := changeFull(0.2, 0.3, Column{Type: Ratio}); d != "-10.0 pts" {
		t.Errorf("full ratio change: %s", d)
	}
}

// --- facts --------------------------------------------------------------------

func TestFiscalCalendarErrorsAndBuildFactsErrors(t *testing.T) {
	if _, _, err := fiscal("2026-13-01", 1); err == nil {
		t.Fatal("bad date")
	}
	m, _ := DefaultModel()
	base := func() *core.Store {
		s := core.NewStore()
		s.Companies["1000"] = &core.Company{Code: "1000", Currency: "USD"}
		s.PutAccount(&core.GLAccount{CompanyCode: "1000", Number: "400000", Name: "Revenue", Type: core.Revenue})
		return s
	}
	cases := map[string]func(s *core.Store){
		"unknown account": func(s *core.Store) {
			s.Journal["J1"] = &core.JournalEntry{ID: "J1", CompanyCode: "1000", PostingDate: "2026-01-01", Lines: []core.JournalLine{{Account: "999"}}}
		},
		"journal date": func(s *core.Store) {
			s.Journal["J1"] = &core.JournalEntry{ID: "J1", CompanyCode: "1000", PostingDate: "bad", Lines: []core.JournalLine{{Account: "400000"}}}
		},
		"po date": func(s *core.Store) {
			s.PurchaseOrders["P1"] = &core.PurchaseOrder{ID: "P1", CompanyCode: "1000", OrderDate: "bad", Lines: []core.POLine{{GLAccount: "999", Quantity: 1000, UnitPrice: 1}}}
		},
		"invoice date": func(s *core.Store) {
			s.Invoices["I1"] = &core.SupplierInvoice{ID: "I1", CompanyCode: "1000", InvoiceDate: "bad"}
		},
	}
	for name, mutate := range cases {
		s := base()
		mutate(s)
		if _, err := BuildFacts(s, m, time.Now()); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	// A PO line on an account missing from the chart still builds, with no account name.
	s := base()
	s.PurchaseOrders["P1"] = &core.PurchaseOrder{ID: "P1", CompanyCode: "1000", OrderDate: "2026-02-01", Currency: "USD",
		Lines: []core.POLine{{GLAccount: "999", Quantity: money.Qty(1000), UnitPrice: 100, PriceUnit: 1}}}
	f, err := BuildFacts(s, m, time.Now())
	if err != nil || f.Tables["po_lines"][0].Dims["account_name"] != "" {
		t.Fatalf("po fact: %v", err)
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
		if _, err := s.Run(q); err == nil {
			t.Errorf("query %d should be refused", i)
		}
	}
}

func TestComparisonHelpers(t *testing.T) {
	if compareValues("2", "10") != -1 || compareValues("10", "2") != 1 || compareValues("1", "1.0") != 0 || compareValues("a", "b") != -1 {
		t.Fatal("compareValues")
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
		if got := compareCells(c.a, c.b); got != c.want {
			t.Errorf("compareCells(%v,%v)=%d want %d", c.a, c.b, got, c.want)
		}
	}
	m, _ := DefaultModel()
	if m.dimLabel("nope") != "nope" || m.dimLabel("company") != "Company code" {
		t.Fatal("dimLabel")
	}
	for cur, want := range map[string]int{"JPY": 0, "KWD": 3, "USD": 2} {
		if currencyDecimals(cur) != want {
			t.Errorf("currencyDecimals(%s)", cur)
		}
	}
	if differInOne(nil, nil) || differInOne([]string{"a"}, []string{"a", "b"}) || !differInOne([]string{"a", "b"}, []string{"a", "c"}) {
		t.Fatal("differInOne")
	}
}

// A JPY company's amounts are scaled by its minor unit.
func TestZeroDecimalCurrency(t *testing.T) {
	s := core.NewStore()
	s.Companies["3000"] = &core.Company{Code: "3000", Currency: "JPY"}
	s.PutAccount(&core.GLAccount{CompanyCode: "3000", Number: "400000", Name: "Revenue", Type: core.Revenue})
	s.PutAccount(&core.GLAccount{CompanyCode: "3000", Number: "100000", Name: "Bank", Type: core.Asset})
	s.Journal["J1"] = &core.JournalEntry{ID: "J1", CompanyCode: "3000", PostingDate: "2026-01-05",
		Lines: []core.JournalLine{{Account: "100000", Amount: 5000}, {Account: "400000", Amount: -5000}}}
	m, _ := DefaultModel()
	f, _ := BuildFacts(s, m, time.Now())
	r, err := Execute(m, f, Principal{ID: "u"}, Query{Measures: []string{"revenue"}})
	if err != nil || r.Rows[0][0].(float64) != 5000 || r.Columns[0].Currency != "JPY" {
		t.Fatalf("JPY revenue: %v %+v", err, r)
	}
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
	if plain.Header[2] != "Revenue (USD)" || plain.Rows[4][2].Text != "—" || plain.Rows[0][1].Text != "Jan" {
		t.Fatalf("plain table: %+v", plain.Rows[4])
	}
	cmp := tableView(Table{Title: "cmp", Compare: &Compare{Dim: "fiscal_year", Baseline: "2025", Current: "2026", Measure: "revenue"}}, r)
	var neg, missing bool
	for _, row := range cmp.Rows {
		for _, c := range row {
			neg = neg || strings.Contains(c.Class, "neg")
		}
		missing = missing || row[1].Text == "—"
	}
	if !neg || !missing {
		t.Fatalf("compare table must show a decrease and a missing baseline: %+v", cmp.Rows)
	}
	// Totals turn negative when the current year is lower overall; two key columns.
	down := &Result{Columns: []Column{{Name: "company", Kind: "dimension"}, {Name: "region", Kind: "dimension"}, {Name: "fiscal_year", Kind: "dimension"},
		{Name: "v", Kind: "measure", Type: Money}},
		Rows: []Row{{"1000", "a", "2025", 100.0}, {"1000", "a", "2026", 50.0}, {"1000", "b", "2025", 100.0}, {"1000", "b", "2026", 60.0},
			{"1000", "01", "2025", 1.0}, {"1000", "1", "2025", 1.0}}}
	tot := tableView(Table{Compare: &Compare{Dim: "fiscal_year", Baseline: "2025", Current: "2026", Measure: "v"}}, down)
	last := tot.Rows[len(tot.Rows)-1]
	if last[0].Text != "Total" || len(last) != 6 || !strings.Contains(last[4].Class, "neg") {
		t.Fatalf("total row: %+v", last)
	}
	if numCell(0, false, Column{}).Text != "—" || cellText(nil, Column{}) != "—" || cellText(true, Column{}) != "—" {
		t.Fatal("empty cells")
	}
	if dimText("2", "fiscal_quarter") != "Q2" || dimText("13", "fiscal_month") != "13" || dimText("", "vendor") != "(none)" {
		t.Fatal("dimText")
	}

	line := chartSVG(Chart{Title: "t", Type: "line", X: "fiscal_month", Series: "fiscal_year", Measure: "revenue"}, r)
	if !strings.Contains(line, "<polyline") || !strings.Contains(line, `class="zero"`) {
		t.Fatal("line chart with negative values")
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
	if niceStep(0) != 0 || niceStep(3) != 5 || niceStep(7) != 10 || niceStep(0.15) != 0.2 {
		t.Fatalf("niceStep %v %v %v", niceStep(3), niceStep(7), niceStep(0.15))
	}
	for _, c := range []struct {
		v    float64
		col  Column
		want string
	}{{0.25, Column{Type: Ratio}, "25%"}, {2.5e6, Column{}, "2.5M"}, {-1500, Column{}, "−1.5K"}, {40, Column{}, "40"}} {
		if got := axisLabel(c.v, c.col); got != c.want {
			t.Errorf("axisLabel(%v) = %q want %q", c.v, got, c.want)
		}
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
	if lines[0] != "fiscal_year,fiscal_month,revenue,margin,orders" || lines[4] != "2026,2,-500,,1" || len(lines) != 6 {
		t.Fatalf("csv:\n%s", buf.String())
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
	audit, _ := governance.NewAuditLog(filepath.Join(dir, "logs", "a.jsonl"))
	s := session(t, us)
	s.Audit = audit
	os.WriteFile(filepath.Join(dir, "logs"), []byte("not a dir"), 0o644)
	if _, err := s.Run(salesQuery()); err == nil || !strings.Contains(err.Error(), "audit") {
		t.Fatalf("audit failure must fail the query: %v", err)
	}

	if _, err := LoadReport(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("missing report")
	}
	os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{"), 0o644)
	if _, err := LoadReport(filepath.Join(dir, "bad.json")); err == nil {
		t.Fatal("corrupt report")
	}

	ok := session(t, us)
	r, _ := ok.Run(salesQuery())
	rep, _ := ok.Build(salesSpec(r.ID), "x")
	blocked := filepath.Join(dir, "file")
	os.WriteFile(blocked, nil, 0o644)
	if _, err := rep.Save(filepath.Join(blocked, "sub"), ok.Results); err == nil {
		t.Fatal("unwritable directory")
	}
	jsonDir := t.TempDir()
	os.Mkdir(filepath.Join(jsonDir, rep.ID+".json"), 0o755)
	if _, err := rep.Save(jsonDir, ok.Results); err == nil {
		t.Fatal("unwritable report file")
	}

	// Rerun: a query that no longer runs, and a spec that no longer validates.
	broken := &Report{Spec: salesSpec("monthly"), Queries: map[string]Query{"monthly": {Measures: []string{"gone"}}}}
	if _, _, err := ok.Rerun(broken); err == nil {
		t.Fatal("rerun with a failing query")
	}
	stale := &Report{Spec: salesSpec("other"), Queries: map[string]Query{"monthly": salesQuery()}}
	if _, _, err := ok.Rerun(stale); err == nil || !strings.Contains(err.Error(), "no longer valid") {
		t.Fatalf("rerun with an unknown query id: %v", err)
	}
}

// A series without a value for some x leaves a gap in both chart types.
func TestChartGaps(t *testing.T) {
	r := &Result{Columns: []Column{{Name: "m", Kind: "dimension"}, {Name: "y", Kind: "dimension"}, {Name: "v", Kind: "measure", Type: Money}},
		Rows: []Row{{"1", "2025", 10.0}, {"2", "2025", 12.0}, {"1", "2026", 11.0}}}
	for _, typ := range []string{"line", "bar"} {
		svg := chartSVG(Chart{Type: typ, X: "m", Series: "y", Measure: "v"}, r)
		if !strings.Contains(svg, "2026") {
			t.Fatalf("%s chart lacks the second series", typ)
		}
	}
	if n := strings.Count(chartSVG(Chart{Type: "bar", X: "m", Series: "y", Measure: "v"}, r), "<title>"); n != 3 {
		t.Fatalf("three bars expected, got %d", n)
	}
}
