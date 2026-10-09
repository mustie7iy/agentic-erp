package analytics

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/governance"
	"github.com/mustie7iy/agentic-erp/internal/migration"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

type memSource struct{ e *sap.Extract }

func (m memSource) Name() string { return "memory" }
func (m memSource) Extract(_ context.Context, s sap.Scope) (*sap.Extract, error) {
	return sap.FilterScope(m.e, s), nil
}

var (
	fixtureOnce  sync.Once
	fixtureStore *core.Store
)

// twoYears migrates mock SAP data for 2025 (full year) and 2026 (through
// September) for both company codes into one store.
func twoYears(t *testing.T) *core.Store {
	t.Helper()
	fixtureOnce.Do(func() {
		s := core.NewStore()
		for _, y := range []struct {
			year    string
			seed    uint64
			through int
		}{{"2025", 41, 12}, {"2026", 42, 9}} {
			e := sap.GenerateMockThrough(y.seed, y.year, y.through)
			scope := sap.Scope{CompanyCodes: []string{"1000", "2000"}, FiscalYear: y.year}
			res, err := migration.Run(context.Background(), memSource{e}, scope, migration.Options{}, s)
			if err != nil || !res.Report.Passed {
				panic("fixture migration failed")
			}
		}
		fixtureStore = s
	})
	return fixtureStore
}

func session(t *testing.T, p Principal) *Session {
	t.Helper()
	m, err := DefaultModel()
	if err != nil {
		t.Fatal(err)
	}
	f, err := BuildFacts(twoYears(t), m, time.Date(2026, 10, 8, 6, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	audit, _ := governance.NewAuditLog("")
	return NewSession(m, f, p, audit)
}

var us = Principal{ID: "user:test", Companies: []string{"1000"}}

func TestModelLoadsAndValidates(t *testing.T) {
	m, err := DefaultModel()
	if err != nil {
		t.Fatal(err)
	}
	if m.Metric(m.Defaults["sales"]).Name != "revenue" {
		t.Fatal(`"sales" should default to revenue`)
	}
	if dims := m.AllowedDimensions("operating_margin"); !containsStr(dims, "fiscal_month") || containsStr(dims, "vendor") {
		t.Fatalf("derived metric dimensions wrong: %v", dims)
	}
	if _, err := ParseModel([]byte("metrics:\n  - {name: x, type: money}\n")); err == nil {
		t.Fatal("metric without source or derivation must be rejected")
	}
}

func TestFiscalCalendar(t *testing.T) {
	for _, c := range []struct {
		date        string
		start, y, m int
	}{{"2026-01-15", 1, 2026, 1}, {"2026-12-01", 1, 2026, 12}, {"2026-04-01", 4, 2027, 1}, {"2026-03-31", 4, 2026, 12}} {
		y, m, err := fiscal(c.date, c.start)
		if err != nil || y != c.y || m != c.m {
			t.Errorf("fiscal(%s, %d) = %d/%d, want %d/%d", c.date, c.start, y, m, c.y, c.m)
		}
	}
}

// Revenue from the semantic layer must equal the ledger's revenue account.
func TestRevenueMatchesLedger(t *testing.T) {
	s := session(t, us)
	r, err := s.Run(Query{Measures: []string{"revenue"}, By: []string{"fiscal_year"}})
	if err != nil {
		t.Fatal(err)
	}
	ledger := map[string]money.Minor{}
	for _, je := range twoYears(t).Journal {
		if je.CompanyCode != "1000" {
			continue
		}
		for _, l := range je.Lines {
			if l.Account == "400000" {
				ledger[je.PostingDate[:4]] -= l.Amount
			}
		}
	}
	if len(r.Rows) != 2 {
		t.Fatalf("want 2 years, got %v", r.Rows)
	}
	for _, row := range r.Rows {
		year, got := row[0].(string), row[1].(float64)
		if want := float64(ledger[year]) / 100; got != want {
			t.Errorf("%s revenue %v, ledger says %v", year, got, want)
		}
	}
	if r.Columns[1].Currency != "USD" || r.DataThrough != "2026-09-28" {
		t.Errorf("currency %q, data through %q", r.Columns[1].Currency, r.DataThrough)
	}
}

func TestFiltersOrderingAndLimit(t *testing.T) {
	s := session(t, us)
	r, err := s.Run(Query{
		Measures: []string{"revenue"}, By: []string{"fiscal_year", "fiscal_month"},
		Where:   []Filter{{Dim: "fiscal_month", Op: "between", Values: []string{"7", "9"}}},
		OrderBy: "revenue", Desc: true, Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 2 || r.Rows[0][2].(float64) < r.Rows[1][2].(float64) {
		t.Fatalf("expected top 2 by revenue descending: %v", r.Rows)
	}
	for _, row := range r.Rows {
		if m := row[1].(string); m < "7" || m > "9" {
			t.Fatalf("filter leaked month %s", m)
		}
	}
}

func TestQueryGuardrails(t *testing.T) {
	all := session(t, Principal{ID: "user:cfo"})
	cases := map[string]Query{
		"unknown metric":     {Measures: []string{"ebitda"}},
		"bad dimension":      {Measures: []string{"revenue"}, By: []string{"vendor"}},
		"mixed currencies":   {Measures: []string{"revenue"}},
		"bad operator":       {Measures: []string{"revenue"}, By: []string{"company"}, Where: []Filter{{Dim: "company", Op: "like", Values: []string{"1"}}}},
		"order by not in it": {Measures: []string{"revenue"}, By: []string{"company"}, OrderBy: "fiscal_year"},
	}
	for name, q := range cases {
		if _, err := all.Run(q); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := all.Run(Query{Measures: []string{"revenue"}, By: []string{"company"}}); err != nil {
		t.Errorf("breaking down by company should resolve the currency mix: %v", err)
	}
}

func TestRowLevelSecurity(t *testing.T) {
	de := session(t, Principal{ID: "user:de", Companies: []string{"2000"}})
	r, err := de.Run(Query{Measures: []string{"revenue"}, By: []string{"company"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != "2000" || r.Columns[1].Currency != "EUR" {
		t.Fatalf("user restricted to 2000 saw %v", r.Rows)
	}
	if QueryID(r.Query, de.Principal) == QueryID(r.Query, us) {
		t.Fatal("query ids must differ by access scope")
	}
}

func TestDerivedMetrics(t *testing.T) {
	s := session(t, us)
	r, err := s.Run(Query{Measures: []string{"revenue", "expenses", "operating_profit", "operating_margin"}, By: []string{"fiscal_year"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range r.Rows {
		rev, exp, op, margin := row[1].(float64), row[2].(float64), row[3].(float64), row[4].(float64)
		if round(rev-exp, 2) != op || round(op/rev, 4) != margin {
			t.Errorf("derived values inconsistent: %v", row)
		}
	}
}

func TestNarrativeCheck(t *testing.T) {
	r := &Result{
		Columns: []Column{{Name: "fiscal_year", Kind: "dimension"}, {Name: "fiscal_month", Kind: "dimension"},
			{Name: "revenue", Kind: "measure", Type: Money}, {Name: "margin", Kind: "measure", Type: Ratio}},
		Rows: []Row{
			{"2025", "1", 2410000.0, 0.371}, {"2025", "2", 2280000.0, 0.36},
			{"2026", "1", 2580000.0, 0.384}, {"2026", "2", 2390000.0, 0.39},
		},
	}
	ok := []string{
		"Revenue reached $4.97M in 2026, up from $4.69M.",      // subtotals by year
		"January grew 7.1% ($170K) on last year.",              // same month, two years
		"February revenue was 2,390,000.",                      // a row value
		"Margin was 38.4% in January, up 1.3 pts.",             // ratio and points
		"Over the two months revenue rose 6.0% to $4,970,000.", // total change
		"8 of 9 months grew in Q3 of FY2026.",                  // small ints, words, years
	}
	for _, text := range ok {
		if bad := CheckNarrative(text, []*Result{r}); len(bad) > 0 {
			t.Errorf("%q: unexpectedly rejected %v", text, bad)
		}
	}
	wrong := []string{
		"Revenue reached $5.2M.",
		"January grew 9.5%.",
		"Margin improved by 4 pts.",
		"Revenue was 1,234,567.",
	}
	for _, text := range wrong {
		if bad := CheckNarrative(text, []*Result{r}); len(bad) == 0 {
			t.Errorf("%q: invented figure not caught", text)
		}
	}
}

// salesSpec is what the agent should produce for "monthly sales vs last year".
func salesSpec(qid string) Spec {
	return Spec{
		Title:       "Revenue: Jan–Sep 2026 vs Jan–Sep 2025",
		Question:    "monthly sales vs last year this time",
		Assumptions: []string{"Sales means revenue (governed default)", "January to September, the last closed month"},
		Tiles: []Tile{
			{Label: "Revenue, year to date", QueryID: qid, Measure: "revenue", Where: map[string]string{"fiscal_year": "2026"},
				CompareWhere: map[string]string{"fiscal_year": "2025"}, CompareLabel: "2025"},
			{Label: "September", QueryID: qid, Measure: "revenue", Where: map[string]string{"fiscal_year": "2026", "fiscal_month": "9"},
				CompareWhere: map[string]string{"fiscal_year": "2025", "fiscal_month": "9"}, CompareLabel: "Sep 2025"},
		},
		Charts: []Chart{{Title: "Revenue by month", Type: "bar", QueryID: qid, X: "fiscal_month", Series: "fiscal_year", Measure: "revenue"}},
		Tables: []Table{{Title: "Monthly comparison", QueryID: qid, Compare: &Compare{Dim: "fiscal_year", Baseline: "2025", Current: "2026", Measure: "revenue"}}},
	}
}

func salesQuery() Query {
	return Query{Measures: []string{"revenue"}, By: []string{"fiscal_year", "fiscal_month"},
		Where: []Filter{{Dim: "fiscal_month", Op: "<=", Values: []string{"9"}}}}
}

func TestBuildRenderAndRerun(t *testing.T) {
	s := session(t, us)
	r, err := s.Run(salesQuery())
	if err != nil {
		t.Fatal(err)
	}
	spec := salesSpec(r.ID)
	ytd26, _, _ := r.sum("revenue", map[string]string{"fiscal_year": "2026"})
	spec.Commentary = "Revenue for January to September 2026 was " + formatValue(ytd26, r.Columns[2]) + "."
	rep, problems := s.Build(spec, "agent:analytics")
	if len(problems) > 0 {
		t.Fatalf("valid spec rejected: %v", problems)
	}

	// Invented numbers are rejected.
	bad := spec
	bad.Commentary = "Revenue grew 42.0% to $9.99M."
	if _, problems := s.Build(bad, "agent:analytics"); len(problems) == 0 || !strings.Contains(problems[0], "42.0%") {
		t.Fatalf("invented figures accepted: %v", problems)
	}
	// Unknown query references are rejected.
	ghost := salesSpec("q_missing")
	if _, problems := s.Build(ghost, "agent:analytics"); len(problems) == 0 {
		t.Fatal("unknown query_id accepted")
	}

	dir := t.TempDir()
	path, err := rep.Save(dir, s.Results)
	if err != nil {
		t.Fatal(err)
	}
	html, _ := os.ReadFile(path)
	for _, want := range []string{"Revenue: Jan–Sep 2026", "<svg", "Monthly comparison", "How these numbers were computed", formatValue(ytd26, r.Columns[2])} {
		if !bytes.Contains(html, []byte(want)) {
			t.Errorf("rendered page missing %q", want)
		}
	}

	// Rerun from the saved file: same figures, commentary kept.
	loaded, err := LoadReport(filepath.Join(dir, rep.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	fresh, results, err := s.Rerun(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Spec.Commentary == "" || len(results) != 1 {
		t.Fatalf("rerun lost commentary or results: %+v", fresh.Spec)
	}

	// A stale commentary (figures no longer in the data) is dropped on rerun.
	stale := *loaded
	stale.Spec.Commentary = "Revenue was $1.11M."
	fresh, _, err = s.Rerun(&stale)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Spec.Commentary != "" || !strings.Contains(strings.Join(fresh.Spec.Assumptions, " "), "commentary was removed") {
		t.Fatal("stale commentary should be removed with a note")
	}
}

func TestHandWrittenReportRemapsQueryIDs(t *testing.T) {
	s := session(t, us)
	rep := &Report{Spec: salesSpec("monthly"), Queries: map[string]Query{"monthly": salesQuery()}}
	fresh, results, err := s.Rerun(rep)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Spec.Charts[0].QueryID == "monthly" || results[fresh.Spec.Charts[0].QueryID] == nil {
		t.Fatal("readable query ids should be remapped to real ones")
	}
}

func TestQueriesAreAudited(t *testing.T) {
	s := session(t, us)
	s.Run(salesQuery())
	s.Run(Query{Measures: []string{"nope"}})
	ev := s.Audit.Events()
	if len(ev) != 2 || ev[0].Action != "analytics.query" || ev[0].Actor != "user:test" || ev[1].Outcome != "error" {
		t.Fatalf("unexpected audit events: %+v", ev)
	}
}

// fakeClaude scripts the agent loop: list metrics, run a query, submit a
// dashboard with an invented number, get rejected, fix it, finish.
type fakeClaude struct {
	mu       sync.Mutex
	calls    int
	bodies   []string
	tools    []string
	rejected bool
}

var qidRe = regexp.MustCompile(`q_[0-9a-f]{12}`)

func (f *fakeClaude) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	body, _ := io.ReadAll(r.Body)
	f.bodies = append(f.bodies, string(body))
	if f.calls == 1 {
		var req struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		json.Unmarshal(body, &req)
		for _, t := range req.Tools {
			f.tools = append(f.tools, t.Name)
		}
	}
	toolUse := func(name string, input any) []map[string]any {
		return []map[string]any{{"type": "tool_use", "id": "toolu_" + name, "name": name, "input": input}}
	}
	var content []map[string]any
	stop := "tool_use"
	switch f.calls {
	case 1:
		content = toolUse("list_metrics", map[string]any{})
	case 2:
		content = toolUse("run_query", map[string]any{
			"measures": []string{"revenue"}, "by": []string{"fiscal_year", "fiscal_month"},
			"where": []map[string]any{{"dim": "fiscal_month", "op": "<=", "values": []string{"9"}}},
		})
	case 3, 4:
		qid := qidRe.FindString(string(body))
		spec := salesSpec(qid)
		spec.Commentary = "Revenue grew 42.0% on last year." // invented
		if f.calls == 4 {
			if !strings.Contains(string(body), "Not accepted") {
				http.Error(w, "rejection did not reach the model", http.StatusBadRequest)
				return
			}
			f.rejected = true
			spec.Commentary = "Revenue is compared month by month below."
		}
		content = toolUse("build_dashboard", map[string]any{"spec": spec})
	default:
		content = []map[string]any{{"type": "text", "text": "Revenue is ahead of last year; details in the dashboard."}}
		stop = "end_turn"
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id": "msg_test", "type": "message", "role": "assistant", "model": defaultModel,
		"content": content, "stop_reason": stop, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 5},
	})
}

func TestAgentAskBuildsValidatedDashboard(t *testing.T) {
	s := session(t, us)
	fake := &fakeClaude{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	a := &Agent{
		Client: anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0)),
		Model:  defaultModel,
		Today:  func() time.Time { return time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC) },
	}
	ans, err := a.Ask(context.Background(), s, "monthly sales vs last year this time")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(fake.tools, ",") != "list_metrics,run_query,build_dashboard" {
		t.Errorf("agent tool surface changed: %v", fake.tools)
	}
	if !fake.rejected {
		t.Error("the invented figure should have been rejected before acceptance")
	}
	if ans.Report == nil || strings.Contains(ans.Report.Spec.Commentary, "42.0%") || ans.Summary == "" {
		t.Fatalf("unexpected answer: %+v", ans)
	}
	if !strings.Contains(fake.bodies[0], "Today is 8 October 2026") || !strings.Contains(fake.bodies[0], `"fallbacks":"default"`) {
		t.Error("request missing date context or fallback config")
	}
	if _, err := ans.Report.Save(t.TempDir(), s.Results); err != nil {
		t.Fatal(err)
	}
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
