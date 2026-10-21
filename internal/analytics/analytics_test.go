package analytics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

var (
	fixtureOnce   sync.Once
	fixtureFacts  *Facts
	ledgerRevenue map[string]int64 // company 1000 revenue per year, from the journal
)

// twoYears builds the facts of mock SAP data for 2025 (full year) and 2026
// (through September) for both company codes, the way loading them onto
// the platform and reading them back through the API does.
func twoYears(t *testing.T) *Facts {
	t.Helper()
	fixtureOnce.Do(func() {
		m, _ := DefaultModel()
		f := &Facts{Tables: map[string][]Fact{}, Companies: map[string]string{}, Exponents: map[string]int{"USD": 2, "EUR": 2}, AsOf: time.Date(2026, 10, 8, 6, 0, 0, 0, time.UTC)}
		ledgerRevenue = map[string]int64{}
		types := func(number string) string {
			switch number[0] {
			case '1':
				return "asset"
			case '2':
				return "liability"
			case '3':
				return "equity"
			case '4':
				return "revenue"
			}
			return "expense"
		}
		add := func(table, id, company, date, currency string, amount money.Minor, dims map[string]string) {
			iso := date[:4] + "-" + date[4:6] + "-" + date[6:]
			fy, fm, _ := fiscal(iso, m.Calendar.FiscalYearStartMonth)
			dims["company"], dims["posting_date"] = company, iso
			dims["fiscal_year"], dims["fiscal_month"], dims["fiscal_quarter"] = strconv.Itoa(fy), strconv.Itoa(fm), strconv.Itoa((fm-1)/3+1)
			f.DataThrough = max(f.DataThrough, iso)
			f.Tables[table] = append(f.Tables[table], Fact{ID: id, Dims: dims, Amount: amount, Currency: currency})
		}
		for _, y := range []struct {
			year    string
			seed    uint64
			through int
		}{{"2025", 41, 12}, {"2026", 42, 9}} {
			e := sap.GenerateMockThrough(y.seed, y.year, y.through)
			names, accounts, recon := map[string]string{}, map[string]string{}, map[string]bool{}
			for _, v := range e.LFA1 {
				names[sap.Alpha(v.LIFNR)] = v.NAME1
			}
			for _, a := range e.SKB1 {
				accounts[sap.Alpha(a.SAKNR)], recon[sap.Alpha(a.SAKNR)] = a.TXT50, a.MITKZ == "K"
			}
			currency := map[string]string{}
			for _, c := range e.T001 {
				currency[c.BUKRS], f.Companies[c.BUKRS] = c.WAERS, c.WAERS
			}
			for _, l := range e.ACDOCA {
				amt, _ := money.ParseDecimal(l.HSL, 2)
				acct := sap.Alpha(l.RACCT)
				add("gl_lines", l.RBUKRS+"/"+l.GJAHR+"/"+l.BELNR, l.RBUKRS, l.BUDAT, currency[l.RBUKRS], amt, map[string]string{
					"account": acct, "account_name": accounts[acct], "account_type": types(acct), "document_type": l.BLART,
					"vendor": sap.Alpha(l.LIFNR), "vendor_name": names[sap.Alpha(l.LIFNR)], "vendor_recon": strconv.FormatBool(recon[acct]),
					"cleared": strconv.FormatBool(recon[acct] && l.AUGBL != "")})
				if l.RBUKRS == "1000" && acct == "400000" {
					ledgerRevenue[l.GJAHR] -= int64(amt)
				}
			}
			heads := map[string]sap.EKKO{}
			for _, h := range e.EKKO {
				heads[h.EBELN] = h
			}
			for _, it := range e.EKPO {
				h := heads[it.EBELN]
				price, _ := money.ParseDecimal(it.NETPR, 2)
				unit, _ := strconv.ParseInt(it.PEINH, 10, 64)
				amount, _ := money.Scale(price, it.MENGE, max(unit, 1)) // mock amounts are far from the range
				add("po_lines", it.EBELN, h.BUKRS, h.BEDAT, h.WAERS, amount, map[string]string{
					"vendor": sap.Alpha(h.LIFNR), "vendor_name": names[sap.Alpha(h.LIFNR)], "account": sap.Alpha(it.SAKTO), "account_name": accounts[sap.Alpha(it.SAKTO)]})
			}
			for _, h := range e.RBKP {
				status := map[string]string{"A": "parked", "5": "posted"}[h.RBSTAT]
				gross, _ := money.ParseDecimal(h.RMWWR, 2)
				add("ap_invoices", h.BELNR+"/"+h.GJAHR, h.BUKRS, h.BLDAT, h.WAERS, gross, map[string]string{
					"vendor": sap.Alpha(h.LIFNR), "vendor_name": names[sap.Alpha(h.LIFNR)], "invoice_status": status})
			}
		}
		fixtureFacts = f
	})
	return fixtureFacts
}

func session(t *testing.T, p Principal) *Session {
	t.Helper()
	m, err := DefaultModel()
	if err != nil {
		t.Fatal(err)
	}
	return NewSession(m, twoYears(t), p)
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
	dims := m.AllowedDimensions("operating_margin")
	if !containsStr(dims, "fiscal_month") {
		t.Errorf("derived metric dimensions: got %v, want fiscal_month among them", dims)
	}
	if containsStr(dims, "vendor") {
		t.Errorf("derived metric dimensions: got %v, want no vendor", dims)
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
		t.Run(fmt.Sprintf("%s from month %d", c.date, c.start), func(t *testing.T) {
			y, m, err := fiscal(c.date, c.start)
			if err != nil {
				t.Fatalf("fiscal(%s, %d): %v", c.date, c.start, err)
			}
			if y != c.y || m != c.m {
				t.Errorf("fiscal(%s, %d): got %d/%d, want %d/%d", c.date, c.start, y, m, c.y, c.m)
			}
		})
	}
}

// Revenue from the semantic layer must equal the ledger's revenue account.
func TestRevenueMatchesLedger(t *testing.T) {
	s := session(t, us)
	r, err := s.Run(context.Background(), Query{Measures: []string{"revenue"}, By: []string{"fiscal_year"}})
	if err != nil {
		t.Fatal(err)
	}
	twoYears(t)
	ledger := ledgerRevenue
	if len(r.Rows) != 2 {
		t.Fatalf("want 2 years, got %v", r.Rows)
	}
	for _, row := range r.Rows {
		year, got := row[0].(string), row[1].(Value)
		if want := (Value{Units: money.Minor(ledger[year]), Scale: 2, Currency: "USD"}); got != want {
			t.Errorf("%s revenue %v, ledger says %v", year, got, want)
		}
	}
	if r.Columns[1].Currency != "USD" {
		t.Errorf("currency: got %q, want USD", r.Columns[1].Currency)
	}
	if r.DataThrough != "2026-09-28" {
		t.Errorf("data through: got %q, want 2026-09-28", r.DataThrough)
	}
}

func TestFiltersOrderingAndLimit(t *testing.T) {
	s := session(t, us)
	r, err := s.Run(context.Background(), Query{
		Measures: []string{"revenue"}, By: []string{"fiscal_year", "fiscal_month"},
		Where:   []Filter{{Dim: "fiscal_month", Op: "between", Values: []string{"7", "9"}}},
		OrderBy: "revenue", Desc: true, Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 2 {
		t.Fatalf("rows: got %d %v, want the top 2", len(r.Rows), r.Rows)
	}
	if first, second := r.Rows[0][2].(Value), r.Rows[1][2].(Value); first.compare(second) < 0 {
		t.Errorf("order: got %v before %v, want revenue descending", first, second)
	}
	for _, row := range r.Rows {
		if m := row[1].(string); m < "7" || m > "9" {
			t.Errorf("row %v: got month %s, want 7 to 9", row, m)
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
		t.Run(name, func(t *testing.T) {
			if _, err := all.Run(context.Background(), q); err == nil {
				t.Error("got no error, want the query refused")
			}
		})
	}
	if _, err := all.Run(context.Background(), Query{Measures: []string{"revenue"}, By: []string{"company"}}); err != nil {
		t.Errorf("breaking down by company should resolve the currency mix: %v", err)
	}
}

func TestRowLevelSecurity(t *testing.T) {
	de := session(t, Principal{ID: "user:de", Companies: []string{"2000"}})
	r, err := de.Run(context.Background(), Query{Measures: []string{"revenue"}, By: []string{"company"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 1 {
		t.Fatalf("user restricted to 2000: got %d rows %v, want 1", len(r.Rows), r.Rows)
	}
	if r.Rows[0][0] != "2000" {
		t.Errorf("user restricted to 2000: got company %v, want 2000", r.Rows[0][0])
	}
	if r.Columns[1].Currency != "EUR" {
		t.Errorf("user restricted to 2000: got currency %s, want EUR", r.Columns[1].Currency)
	}
	if QueryID(r.Query, de.Principal) == QueryID(r.Query, us) {
		t.Fatal("query ids must differ by access scope")
	}
}

// TestAggregateOutOfRange: invoices the API accepts (5·10^18 minor units
// each) can add up to more than an amount holds. The query is refused with
// money.ErrOverflow rather than reporting a wrapped, negative total; terms
// that net back into range still sum exactly, whatever their order.
func TestAggregateOutOfRange(t *testing.T) {
	m, err := DefaultModel()
	if err != nil {
		t.Fatal(err)
	}
	const big = money.Minor(5_000_000_000_000_000_000)
	facts := func(amounts ...money.Minor) *Facts {
		f := &Facts{Tables: map[string][]Fact{}, Companies: map[string]string{"1000": "EUR"}, Exponents: map[string]int{"EUR": 2}}
		for i, a := range amounts {
			f.Tables["ap_invoices"] = append(f.Tables["ap_invoices"], Fact{ID: strconv.Itoa(i), Amount: a, Currency: "EUR",
				Dims: map[string]string{"company": "1000", "fiscal_year": "2026"}})
		}
		return f
	}
	q := Query{Measures: []string{"invoice_value"}, By: []string{"company"}}
	t.Run("out of range", func(t *testing.T) {
		res, err := Execute(m, facts(big, big), Principal{ID: "user:cfo"}, q)
		if !errors.Is(err, money.ErrOverflow) {
			t.Fatalf("got %v (rows %v), want money.ErrOverflow", err, res)
		}
		if !strings.Contains(err.Error(), "invoice_value for 1000 adds up") {
			t.Errorf("got %v, want the group named", err)
		}
	})
	t.Run("out of range in total", func(t *testing.T) {
		_, err := Execute(m, facts(big, big), Principal{ID: "user:cfo"}, Query{Measures: []string{"invoice_value"}})
		if err == nil || !strings.Contains(err.Error(), "invoice_value in total adds up") {
			t.Errorf("got %v, want invoice_value in total out of range", err)
		}
	})
	t.Run("out of range for a blank value", func(t *testing.T) {
		f := facts(big, big)
		for i := range f.Tables["ap_invoices"] {
			delete(f.Tables["ap_invoices"][i].Dims, "fiscal_year")
		}
		_, err := Execute(m, f, Principal{ID: "user:cfo"}, Query{Measures: []string{"invoice_value"}, By: []string{"fiscal_year"}})
		if err == nil || !strings.Contains(err.Error(), "invoice_value for (blank) adds up") {
			t.Errorf("got %v, want the blank group named, not the total", err)
		}
	})
	t.Run("nets into range", func(t *testing.T) {
		res, err := Execute(m, facts(big, big, -big), Principal{ID: "user:cfo"}, q)
		if err != nil {
			t.Fatalf("got %v, want the total", err)
		}
		if got, want := res.Rows[0][1], (Value{Units: big, Scale: 2, Currency: "EUR"}); got != want {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}

func TestDerivedMetrics(t *testing.T) {
	s := session(t, us)
	r, err := s.Run(context.Background(), Query{Measures: []string{"revenue", "expenses", "operating_profit", "operating_margin"}, By: []string{"fiscal_year"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range r.Rows {
		rev, exp, op, margin := row[1].(Value), row[2].(Value), row[3].(Value), row[4].(Value)
		if want, err := diff(rev, exp); err != nil || op != want {
			t.Errorf("row %v: got operating profit %v, want %v (%v)", row, op, want, err)
		}
		units, err := money.Round(big.NewRat(int64(op.Units), int64(rev.Units)), ratioScale)
		if want := (Value{Units: units, Scale: ratioScale}); err != nil || margin != want {
			t.Errorf("row %v: got operating margin %v, want %v (%v)", row, margin, want, err)
		}
	}
}

func TestNarrativeCheck(t *testing.T) {
	r := &Result{
		Columns: []Column{{Name: "fiscal_year", Kind: "dimension"}, {Name: "fiscal_month", Kind: "dimension"},
			{Name: "revenue", Kind: "measure", Type: Money}, {Name: "margin", Kind: "measure", Type: Ratio}},
		Rows: []Row{
			{"2025", "1", usd("2410000.00"), ratio("0.371")}, {"2025", "2", usd("2280000.00"), ratio("0.36")},
			{"2026", "1", usd("2580000.00"), ratio("0.384")}, {"2026", "2", usd("2390000.00"), ratio("0.39")},
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
		t.Run(text, func(t *testing.T) {
			if bad := CheckNarrative(text, []*Result{r}); len(bad) > 0 {
				t.Errorf("got %v rejected, want the text accepted", bad)
			}
		})
	}
	wrong := []string{
		"Revenue reached $5.2M.",
		"January grew 9.5%.",
		"Margin improved by 4 pts.",
		"Revenue was 1,234,567.",
	}
	for _, text := range wrong {
		t.Run(text, func(t *testing.T) {
			if bad := CheckNarrative(text, []*Result{r}); len(bad) == 0 {
				t.Error("got nothing rejected, want the invented figure caught")
			}
		})
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
	r, err := s.Run(context.Background(), salesQuery())
	if err != nil {
		t.Fatal(err)
	}
	spec := salesSpec(r.ID)
	ytd26, _, _ := r.sum("revenue", map[string]string{"fiscal_year": "2026"})
	spec.Commentary = "Revenue for January to September 2026 was " + formatValue(ytd26, r.Columns[2]) + "."
	built := time.Date(2026, 10, 2, 8, 30, 0, 0, time.UTC)
	s.Now = func() time.Time { return built }
	rep, problems := s.Build(spec, "agent:analytics")
	if len(problems) > 0 {
		t.Fatalf("valid spec rejected: %v", problems)
	}
	if !rep.CreatedAt.Equal(built) {
		t.Errorf("created at: got %v, want %v", rep.CreatedAt, built)
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
	for _, want := range []string{"Revenue: Jan–Sep 2026", "<svg", "Monthly comparison", "Generated 2026-10-02 08:30 UTC", "How these numbers were computed", formatValue(ytd26, r.Columns[2])} {
		if !bytes.Contains(html, []byte(want)) {
			t.Errorf("rendered page missing %q", want)
		}
	}

	// Rerun from the saved file: same figures, commentary kept.
	loaded, err := LoadReport(filepath.Join(dir, rep.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	fresh, results, err := s.Rerun(context.Background(), loaded)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Spec.Commentary == "" {
		t.Errorf("rerun commentary: got none, want it kept: %+v", fresh.Spec)
	}
	if len(results) != 1 {
		t.Errorf("rerun results: got %d, want 1", len(results))
	}

	// A stale commentary (figures no longer in the data) is dropped on rerun.
	stale := *loaded
	stale.Spec.Commentary = "Revenue was $1.11M."
	fresh, _, err = s.Rerun(context.Background(), &stale)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Spec.Commentary != "" {
		t.Errorf("stale commentary: got %q, want it removed", fresh.Spec.Commentary)
	}
	if notes := strings.Join(fresh.Spec.Assumptions, " "); !strings.Contains(notes, "commentary was removed") {
		t.Errorf("assumptions: got %q, want a note that the commentary was removed", notes)
	}
}

func TestHandWrittenReportRemapsQueryIDs(t *testing.T) {
	s := session(t, us)
	rep := &Report{Spec: salesSpec("monthly"), Queries: map[string]Query{"monthly": salesQuery()}}
	fresh, results, err := s.Rerun(context.Background(), rep)
	if err != nil {
		t.Fatal(err)
	}
	qid := fresh.Spec.Charts[0].QueryID
	if qid == "monthly" {
		t.Error("chart query id: got monthly, want it remapped to a real one")
	}
	if results[qid] == nil {
		t.Errorf("chart query id: got %s, want one with a result", qid)
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
		if err := json.Unmarshal(body, &req); err != nil { // not a Messages request: refuse it, and the test sees the error
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
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
	// A failed write reaches the client as a broken answer, which the test sees there.
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "msg_test", "type": "message", "role": "assistant", "model": Alias,
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
		Model:  Alias,
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
	if ans.Report == nil {
		t.Fatalf("answer: got no report, want one: %+v", ans)
	}
	if strings.Contains(ans.Report.Spec.Commentary, "42.0%") {
		t.Errorf("commentary: got %q, want the invented figure gone", ans.Report.Spec.Commentary)
	}
	if ans.Summary == "" {
		t.Error("summary: got none, want one")
	}
	if !strings.Contains(fake.bodies[0], "Today is 8 October 2026") {
		t.Errorf("first request: got %s, want today's date in it", fake.bodies[0])
	}
	// No provider fallback: the gateway routes every call to the one model
	// it prices (ADR-0036).
	if strings.Contains(fake.bodies[0], "fallbacks") {
		t.Errorf("first request: got %s, want no fallback", fake.bodies[0])
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
