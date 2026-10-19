package analytics

import "strings"

// Grade is how an answer's figures compare with the semantic layer's own
// results for the same question (ADR-0039): the figures it states, on its
// tiles and in its assumptions, commentary and summary (a follow-up
// question's numbers ask, they do not state), and those no reference result
// supports by the rule the commentary already meets (a value, a subtotal,
// or a change between two results that differ in one dimension). The
// wording is not graded.
type Grade struct {
	Figures     []string `json:"figures"`
	Unsupported []string `json:"unsupported"`
}

// Grade grades the answer, whose report was built from results, against
// reference: the results of the question's own queries, run by the
// grader rather than the agent. A figure the agent's own queries support
// but the reference does not is a wrong answer to the question, however
// well its arithmetic holds.
func (a *Answer) Grade(results map[string]*Result, reference []*Result) Grade {
	var stated []string
	for _, t := range tileViews(&a.Report.Spec, results) {
		stated = append(stated, t.Value, t.Delta, t.Pct)
	}
	stated = append(stated, a.Report.Spec.Assumptions...)
	stated = append(stated, a.Report.Spec.Commentary, a.Summary)
	ev := buildEvidence(reference)
	g := Grade{Figures: []string{}, Unsupported: []string{}}
	for _, f := range extractFigures(strings.Join(stated, "\n")) {
		g.Figures = append(g.Figures, f.text)
		if !ev.supports(f) {
			g.Unsupported = append(g.Unsupported, f.text)
		}
	}
	return g
}

// Narrative is every text the agent wrote for the answer: the title, the
// tiles' and charts' labels, the assumptions, the commentary, the
// follow-up questions and the summary, for checks on what it must not say.
func (a *Answer) Narrative() string {
	sp := a.Report.Spec
	parts := append([]string{sp.Title}, sp.Assumptions...)
	for _, t := range sp.Tiles {
		parts = append(parts, t.Label, t.CompareLabel)
	}
	for _, c := range sp.Charts {
		parts = append(parts, c.Title)
	}
	for _, t := range sp.Tables {
		parts = append(parts, t.Title)
	}
	parts = append(parts, sp.Commentary)
	parts = append(parts, sp.FollowUps...)
	return strings.Join(append(parts, a.Summary), "\n")
}
