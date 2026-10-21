package configuration

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
)

// recordedEvidence is the regression evidence recorded for a suite of the
// cases, replayed with the failures (stale ones when stale), as the worker
// records it.
func recordedEvidence(t *testing.T, cases []RegressionCase, stale bool, touched map[string]bool, failures ...RegressionCase) Evidence {
	t.Helper()
	rec := newRegressionRecord(RegressionSuite{ID: uuid.New(), Number: "RS-0001", Cases: cases}, 1)
	rec.StaleSuite = stale
	passed := !stale
	for _, c := range failures {
		r, f := rec.company(c), RegressionFailure{Case: c, Decision: "review", Issues: []string{}}
		switch {
		case stale:
			r.Stale = append(r.Stale, f)
		case touched[c.Company]:
			r.Changed = append(r.Changed, f)
		default:
			r.Regressions, passed = append(r.Regressions, f), false
		}
	}
	sim := rec.result(passed, keys(touched))
	if sim.Summary != rec.recordedSummary() {
		t.Errorf("recorded summary: got %q, want %q", sim.Summary, rec.recordedSummary())
	}
	b, err := json.Marshal(sim.Detail)
	if err != nil {
		t.Fatal(err)
	}
	return Evidence{Kind: "regression", Passed: sim.Passed, Summary: sim.Summary, Detail: b}
}

// shownDetail is a regression detail as a reader is shown it.
type shownDetail struct {
	Cases       *int                `json:"cases"`
	Stale       []RegressionFailure `json:"stale"`
	Changed     []RegressionFailure `json:"changed"`
	Regressions []RegressionFailure `json:"regressions"`
	ByCompany   json.RawMessage     `json:"cases_by_company"`
	StaleSuite  json.RawMessage     `json:"stale_suite"`
}

func caseNumbers(fs []RegressionFailure) []int32 {
	out := []int32{}
	for _, f := range fs {
		out = append(out, f.Case.CaseNo)
	}
	return out
}

// TestRegressionShown: each reader of regression evidence is shown the
// cases of the companies they may access alone, in the suite's order and
// numbered among those (so the gaps tell nothing of other companies), with
// the summary's counts over those; the verdict stays the change's. A case
// of no legal entity is shown only to a reader of every company, and a
// reader of no company is shown no case and counts of zero.
func TestRegressionShown(t *testing.T) {
	us, de := uuid.New(), uuid.New()
	cases := []RegressionCase{
		{CaseNo: 1, LegalEntityID: de, Company: "2000"}, {CaseNo: 2, LegalEntityID: us, Company: "1000"},
		{CaseNo: 3, LegalEntityID: de, Company: "2000"}, {CaseNo: 4, Company: "9000"}, // of no legal entity
	}
	all, onlyDE, onlyUS, none := authz.Access{All: true}, authz.Access{IDs: []uuid.UUID{de}}, authz.Access{IDs: []uuid.UUID{us}}, authz.Access{}
	replayed := recordedEvidence(t, cases, false, map[string]bool{"2000": true}, cases[2], cases[1], cases[0], cases[3])
	stale := recordedEvidence(t, cases, true, map[string]bool{"2000": true}, cases[0], cases[1])
	tests := map[string]struct {
		in                          Evidence
		access                      authz.Access
		summary                     string
		cases                       int
		stale, changed, regressions []int32
	}{
		"replayed, every company": {replayed, all, "Suite RS-0001: 4 of 4 cases decided differently under the proposal; 2 of them outside the companies the change touches",
			4, nil, []int32{1, 3}, []int32{2, 4}},
		// Numbered among the cases shown: 1 and 3 of the suite are 1 and 2 to a reader of company 2000.
		"replayed, company 2000": {replayed, onlyDE, "Suite RS-0001: 2 of 2 cases decided differently under the proposal", 2, nil, []int32{1, 2}, []int32{}},
		"replayed, company 1000": {replayed, onlyUS, "Suite RS-0001: 1 of 1 cases decided differently under the proposal; 1 of them outside the companies the change touches",
			1, nil, []int32{}, []int32{1}},
		"replayed, no company": {replayed, none, "Suite RS-0001: 0 of 0 cases decided differently under the proposal", 0, nil, []int32{}, []int32{}},
		"stale, every company": {stale, all, "Suite RS-0001 no longer matches version 1 (2 of 4 cases decided differently today); capture a new suite.",
			4, []int32{1, 2}, nil, nil},
		"stale, company 1000": {stale, onlyUS, "Suite RS-0001 no longer matches version 1 (1 of 1 cases decided differently today); capture a new suite.",
			1, []int32{1}, nil, nil},
		"stale, no company": {stale, none, "Suite RS-0001 no longer matches version 1 (0 of 0 cases decided differently today); capture a new suite.",
			0, []int32{}, nil, nil},
	}
	for name, c := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := regressionShown(c.in, c.access)
			if err != nil {
				t.Fatal(err)
			}
			if got.Summary != c.summary {
				t.Errorf("summary: got %q, want %q", got.Summary, c.summary)
			}
			if got.Passed != c.in.Passed {
				t.Errorf("passed: got %v, want the change's %v", got.Passed, c.in.Passed)
			}
			var d shownDetail
			if err := json.Unmarshal(got.Detail, &d); err != nil {
				t.Fatal(err)
			}
			if d.Cases == nil || *d.Cases != c.cases {
				t.Errorf("cases: got %v, want %d", d.Cases, c.cases)
			}
			if d.ByCompany != nil || d.StaleSuite != nil {
				t.Errorf("detail: got %s, want the record by company withheld", got.Detail)
			}
			for what, want := range map[string][]int32{"stale": c.stale, "changed": c.changed, "regressions": c.regressions} {
				list := map[string][]RegressionFailure{"stale": d.Stale, "changed": d.Changed, "regressions": d.Regressions}[what]
				if want == nil && list != nil {
					t.Errorf("%s: got %v, want none listed", what, caseNumbers(list))
				}
				if want != nil && !slices.Equal(caseNumbers(list), want) {
					t.Errorf("%s: got cases %v, want %v", what, caseNumbers(list), want)
				}
			}
		})
	}
}

// TestRegressionShownAsIs: evidence of another kind, of no suite replayed,
// or of a summary not recorded as regression evidence records it, keeps
// what it is; evidence recorded with counts over every company, before
// cases were kept by company (or of any other shape holding cases), is
// shown whole to a reader of every company and without its cases to any
// other; a detail that does not read fails
// rather than show it whole.
func TestRegressionShownAsIs(t *testing.T) {
	narrow := authz.Access{IDs: []uuid.UUID{uuid.New()}}
	legacy := Evidence{Kind: "regression", Summary: "Suite RS-0001: 1 of 3 cases decided differently under the proposal",
		Detail: json.RawMessage(`{"suite_id": "x", "number": "RS-0001", "cases": 3, "changed": [{}], "regressions": [], "touched_companies": ["2000"]}`)}
	custom := recordedEvidence(t, []RegressionCase{{CaseNo: 1}}, false, nil)
	custom.Summary = "Checked by hand."
	tests := map[string]struct {
		in      Evidence
		access  authz.Access
		summary string
		detail  string
	}{
		"simulation":            {Evidence{Kind: "simulation", Summary: "S.", Detail: json.RawMessage(`[1]`)}, narrow, "S.", `[1]`},
		"no suite":              {Evidence{Kind: "regression", Summary: "No suite.", Detail: json.RawMessage(`{"config_version": 1}`)}, narrow, "No suite.", `{"config_version": 1}`},
		"legacy, every company": {legacy, authz.Access{All: true}, legacy.Summary, string(legacy.Detail)},
		"legacy, narrower":      {legacy, narrow, legacyRegressionSummary, `{"number":"RS-0001","suite_id":"x","touched_companies":["2000"]}`},
		"unknown shape, narrower": {Evidence{Kind: "regression", Summary: "Suite RS-0001: 2 stale.", Detail: json.RawMessage(`{"stale": [{}, {}]}`)}, narrow,
			legacyRegressionSummary, `{}`},
		"summary not recorded": {custom, narrow, "Checked by hand.", ""},
	}
	for name, c := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := regressionShown(c.in, c.access)
			if err != nil {
				t.Fatal(err)
			}
			if got.Summary != c.summary {
				t.Errorf("summary: got %q, want %q", got.Summary, c.summary)
			}
			if c.detail != "" && string(got.Detail) != c.detail {
				t.Errorf("detail: got %s, want %s", got.Detail, c.detail)
			}
		})
	}
	for name, detail := range map[string]string{"not an object": `[1]`, "replays not a list": `{"cases_by_company": 1}`} {
		t.Run(name, func(t *testing.T) {
			_, err := regressionShown(Evidence{Kind: "regression", Detail: json.RawMessage(detail)}, narrow)
			if err == nil {
				t.Error("unreadable detail: got no error, want one")
			}
		})
	}
}

// TestSuiteShown: a reader of every company is shown a suite whole; any
// other reader the cases of their companies alone, numbered among those,
// counted, with the strata that selected those (none for no case, never
// null).
func TestSuiteShown(t *testing.T) {
	us, de := uuid.New(), uuid.New()
	s := RegressionSuite{CaseCount: 3, Strata: []string{"decision review", "issue price_variance", "sample"}, Cases: []RegressionCase{
		{CaseNo: 1, LegalEntityID: de, Why: "decision review; issue price_variance"}, {CaseNo: 2, LegalEntityID: us, Why: "sample"}, {CaseNo: 3, Why: "sample"}}}
	tests := map[string]struct {
		access authz.Access
		cases  []int32
		strata []string
	}{
		"every company": {authz.Access{All: true}, []int32{1, 2, 3}, s.Strata},
		"company 2000":  {authz.Access{IDs: []uuid.UUID{de}}, []int32{1}, []string{"decision review", "issue price_variance"}},
		"company 1000":  {authz.Access{IDs: []uuid.UUID{us}}, []int32{1}, []string{"sample"}}, // case 2, numbered among those shown
		"no company":    {authz.Access{}, []int32{}, []string{}},
	}
	for name, c := range tests {
		t.Run(name, func(t *testing.T) {
			got := suiteShown(s, c.access)
			numbers := []int32{}
			for _, cs := range got.Cases {
				numbers = append(numbers, cs.CaseNo)
			}
			if !slices.Equal(numbers, c.cases) {
				t.Errorf("cases: got %v, want %v", numbers, c.cases)
			}
			if int(got.CaseCount) != len(c.cases) {
				t.Errorf("case count: got %d, want %d", got.CaseCount, len(c.cases))
			}
			if got.Strata == nil || !slices.Equal(got.Strata, c.strata) {
				t.Errorf("strata: got %#v, want %#v", got.Strata, c.strata)
			}
		})
	}
}
