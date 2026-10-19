package analytics

import (
	"slices"
	"strings"
	"testing"
)

// TestGrade: an answer's tiles, commentary and summary are graded against
// the reference results of the question, not the agent's own: a figure
// they support passes, an invented one or one from another question does
// not, and every figure stated is listed.
func TestGrade(t *testing.T) {
	s := session(t, us)
	r, err := s.Run(salesQuery())
	if err != nil {
		t.Fatal(err)
	}
	spec := salesSpec(r.ID)
	ytd, _, _ := r.sum("revenue", map[string]string{"fiscal_year": "2026"})
	spec.Commentary = "Revenue for January to September 2026 was " + formatValue(ytd, r.Columns[2]) + "."
	rep, problems := s.Build(spec, "agent:analytics")
	if len(problems) > 0 {
		t.Fatalf("valid spec rejected: %v", problems)
	}
	ans := &Answer{Report: rep, Summary: "Revenue grew on last year."}
	g := ans.Grade(s.Results, []*Result{r})
	if len(g.Unsupported) != 0 {
		t.Errorf("unsupported: got %v, want none", g.Unsupported)
	}
	// Two tiles with a value, a change and its percentage, and the
	// commentary's figure.
	if len(g.Figures) != 7 {
		t.Errorf("figures: got %v, want 7", g.Figures)
	}
	for _, want := range []string{spec.Title, "Revenue, year to date", "Sep 2025", "Revenue by month", "Monthly comparison", spec.Assumptions[1], "Revenue grew"} {
		if !strings.Contains(ans.Narrative(), want) {
			t.Errorf("narrative: got %q, want %q in it", ans.Narrative(), want)
		}
	}
	ans.Summary = "Revenue reached $9.99M."
	if g := ans.Grade(s.Results, []*Result{r}); !slices.Contains(g.Unsupported, "$9.99M") {
		t.Errorf("an invented figure: got %v, want $9.99M unsupported", g.Unsupported)
	}
	other, err := s.Run(Query{Measures: []string{"revenue"}, By: []string{"fiscal_year"}, Where: []Filter{{Dim: "fiscal_year", Op: "in", Values: []string{"2025"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if g := ans.Grade(s.Results, []*Result{other}); len(g.Unsupported) < 2 {
		t.Errorf("another question's reference: got %v unsupported, want the 2026 figures", g.Unsupported)
	}
}
