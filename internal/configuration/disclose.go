package configuration

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
)

// Disclosure limits what the reader of a change request is shown of its
// evidence: a context whose evidence records values of several companies
// (the payables context's simulated invoices, the analytics context's
// metric values) withholds those of companies
// the reader may not access, and any count over them in the summary. It is
// given the reader's company access, read once per request; it returns the
// evidence to show, and an error fails the read rather than show the
// evidence whole.
type Disclosure interface {
	Disclose(ctx context.Context, e Evidence, access authz.Access) (Evidence, error)
}

// disclosed is the change request as its reader is shown it.
func (d Dependencies) disclosed(ctx context.Context, c ChangeRequest) (ChangeRequest, error) {
	crs, err := d.disclosedAll(ctx, []ChangeRequest{c})
	if err != nil {
		return ChangeRequest{}, err
	}
	return crs[0], nil
}

// disclosedAll is the change requests as their reader is shown them. The
// reader's company access is read once for them all, and only when one
// has evidence, not once per piece of evidence; a context's disclosure
// may read more of its own (the analytics context names the companies of
// each simulation of metric values).
func (d Dependencies) disclosedAll(ctx context.Context, crs []ChangeRequest) ([]ChangeRequest, error) {
	if !slices.ContainsFunc(crs, func(c ChangeRequest) bool { return len(c.Evidence) > 0 }) {
		return crs, nil
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("configuration: evidence as its reader is shown it: %w", err)
	}
	out := make([]ChangeRequest, len(crs))
	for i, c := range crs {
		evidence := make([]Evidence, len(c.Evidence))
		for j, ev := range c.Evidence {
			if evidence[j], err = d.disclose(ctx, ev, access); err != nil {
				return nil, fmt.Errorf("configuration: the %s evidence of change request %s: %w", ev.Kind, c.ID, err)
			}
		}
		c.Evidence = evidence
		out[i] = c
	}
	return out, nil
}

// disclose is one piece of evidence as the reader with the access is
// shown it: the regression cases of their companies (this context's), then
// what each other context's disclosure leaves.
func (d Dependencies) disclose(ctx context.Context, e Evidence, access authz.Access) (Evidence, error) {
	e, err := regressionShown(e, access)
	if err != nil {
		return Evidence{}, err
	}
	for _, dis := range d.Disclosures {
		if e, err = dis.Disclose(ctx, e, access); err != nil {
			return Evidence{}, err
		}
	}
	return e, nil
}

// casesByCompanyKey is the regression evidence detail's key for what
// replaying each company's cases found (caseReplay).
const casesByCompanyKey = "cases_by_company"

// caseReplay is what replaying one company's cases of a suite found. The
// regression evidence records it per company, with no count over
// companies, so that each reader of the change request is shown the cases
// of the companies they may access alone and counts over those.
type caseReplay struct {
	Company       string              `json:"company"`
	LegalEntityID uuid.UUID           `json:"legal_entity_id"`
	Cases         int                 `json:"cases"`
	CaseNos       []int32             `json:"case_nos"`    // the cases' numbers in the suite, which each reader's numbering follows
	Stale         []RegressionFailure `json:"stale"`       // decided differently under the base version
	Changed       []RegressionFailure `json:"changed"`     // under the proposal, in a company the change touches
	Regressions   []RegressionFailure `json:"regressions"` // under the proposal, outside them
}

// regressionRecord is the regression evidence's detail as recorded, less
// the companies the change touches, which every reader of the request is
// shown in its items anyway.
type regressionRecord struct {
	SuiteID       uuid.UUID    `json:"suite_id"`
	Number        string       `json:"number"`
	ConfigVersion int32        `json:"config_version"`
	StaleSuite    bool         `json:"stale_suite"` // the suite no longer matches its version
	Companies     []caseReplay `json:"cases_by_company"`
}

// newRegressionRecord starts the record of replaying the suite on the base
// version: its cases by company, in the order of the companies' codes.
func newRegressionRecord(s RegressionSuite, version int32) *regressionRecord {
	r := &regressionRecord{SuiteID: s.ID, Number: s.Number, ConfigVersion: version, Companies: []caseReplay{}}
	for _, c := range s.Cases {
		cr := r.company(c)
		cr.Cases++
		cr.CaseNos = append(cr.CaseNos, c.CaseNo)
	}
	slices.SortFunc(r.Companies, func(a, b caseReplay) int {
		return cmp.Or(strings.Compare(a.Company, b.Company), strings.Compare(a.LegalEntityID.String(), b.LegalEntityID.String()))
	})
	return r
}

// company is the replay of the case's company, started on its first case.
func (r *regressionRecord) company(c RegressionCase) *caseReplay {
	for i := range r.Companies {
		if r.Companies[i].LegalEntityID == c.LegalEntityID {
			return &r.Companies[i]
		}
	}
	r.Companies = append(r.Companies, caseReplay{Company: c.Company, LegalEntityID: c.LegalEntityID, CaseNos: []int32{},
		Stale: []RegressionFailure{}, Changed: []RegressionFailure{}, Regressions: []RegressionFailure{}})
	return &r.Companies[len(r.Companies)-1]
}

// result is the regression evidence as recorded: a summary with no count,
// since it also reaches the operation's result and the audit trail, and
// the detail by company.
func (r *regressionRecord) result(passed bool, touched []string) Simulation {
	return Simulation{Passed: passed, Summary: r.recordedSummary(), Detail: map[string]any{"suite_id": r.SuiteID, "number": r.Number,
		"config_version": r.ConfigVersion, "stale_suite": r.StaleSuite, "touched_companies": touched, casesByCompanyKey: r.Companies}}
}

// recordedSummary is the summary as recorded.
func (r *regressionRecord) recordedSummary() string {
	if r.StaleSuite {
		return fmt.Sprintf("Suite %s no longer matches version %d; capture a new suite.", r.Number, r.ConfigVersion)
	}
	return fmt.Sprintf("Suite %s replayed under the proposal.", r.Number)
}

// regressionShown is regression evidence as the reader with the access is
// shown it: the cases of the companies they may access alone (ADR-0010,
// 04-api-design §4.9), numbered among those, with the summary's counts
// over those. It fails closed: a reader of no company is shown no case and
// counts of zero, and a case of no legal entity is shown only to a reader
// of every company. The verdict (passed) is the change request's and is
// shown whole, as its status shows it anyway. Evidence of no suite has no
// case and no count, and is shown as it is; so is evidence of a suite of
// no cases (noCases), whose summary counts none.
func regressionShown(e Evidence, access authz.Access) (Evidence, error) {
	if e.Kind != "regression" {
		return e, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(e.Detail, &fields); err != nil {
		return Evidence{}, fmt.Errorf("configuration: regression cases: %w", err)
	}
	if fields[casesByCompanyKey] == nil {
		// Evidence of no suite holds none of the keys of cases or counts;
		// any other shape is taken for evidence recorded before, which
		// fails closed.
		if !slices.ContainsFunc(legacyRegressionKeys, func(k string) bool { return fields[k] != nil }) {
			return e, nil
		}
		return legacyRegressionShown(e, fields, access), nil
	}
	var rec regressionRecord
	if err := json.Unmarshal(e.Detail, &rec); err != nil {
		return Evidence{}, fmt.Errorf("configuration: regression cases: %w", err)
	}
	shown := rec.shownTo(access)
	delete(fields, casesByCompanyKey)
	delete(fields, "stale_suite")
	// Numbers and lists of cases marshal without fail, and so does a map
	// of the raw values just read.
	fields["cases"], _ = json.Marshal(shown.cases)
	if rec.StaleSuite {
		fields["stale"], _ = json.Marshal(shown.stale)
	} else {
		fields["changed"], _ = json.Marshal(shown.changed)
		fields["regressions"], _ = json.Marshal(shown.regressions)
	}
	e.Detail, _ = json.Marshal(fields)
	// A summary not recorded as this evidence records it carries no count
	// of ours, so it is shown as it is.
	e.Summary = strings.Replace(e.Summary, rec.recordedSummary(), shown.summary(rec), 1)
	return e, nil
}

// shownCases is what a reader is shown of the replayed cases.
type shownCases struct {
	cases                       int
	stale, changed, regressions []RegressionFailure
}

// shownTo is what the reader with the access is shown of the recorded
// replays: those of the companies they may access, in the suite's order and
// numbered among the cases they are shown, as the suite numbers them for
// that reader (suiteShown).
func (r regressionRecord) shownTo(access authz.Access) shownCases {
	s := shownCases{stale: []RegressionFailure{}, changed: []RegressionFailure{}, regressions: []RegressionFailure{}}
	var numbers []int32
	for _, c := range r.Companies {
		if !access.Allows(c.LegalEntityID) {
			continue
		}
		numbers = append(numbers, c.CaseNos...)
		s.cases += c.Cases
		s.stale = append(s.stale, c.Stale...)
		s.changed = append(s.changed, c.Changed...)
		s.regressions = append(s.regressions, c.Regressions...)
	}
	byCase := func(a, b RegressionFailure) int { return cmp.Compare(a.Case.CaseNo, b.Case.CaseNo) }
	slices.SortFunc(s.stale, byCase)
	slices.SortFunc(s.changed, byCase)
	slices.SortFunc(s.regressions, byCase)
	if !access.All {
		renumber := positions(numbers)
		for _, list := range [][]RegressionFailure{s.stale, s.changed, s.regressions} {
			for i := range list {
				list[i].Case.CaseNo = renumber[list[i].Case.CaseNo]
			}
		}
	}
	return s
}

// positions maps the numbers of the cases a reader is shown, in the suite,
// to their positions among them: the suite's own numbers would tell a
// reader of fewer companies how many cases of others lie between. A reader
// of every company is shown the suite's numbers, which are the same.
func positions(numbers []int32) map[int32]int32 {
	slices.Sort(numbers)
	out := make(map[int32]int32, len(numbers))
	for i, n := range numbers {
		out[n] = int32(i + 1)
	}
	return out
}

// summary is the line for approvers, counted over the shown cases.
func (s shownCases) summary(r regressionRecord) string {
	if r.StaleSuite {
		return fmt.Sprintf("Suite %s no longer matches version %d (%d of %d cases decided differently today); capture a new suite.",
			r.Number, r.ConfigVersion, len(s.stale), s.cases)
	}
	out := fmt.Sprintf("Suite %s: %d of %d cases decided differently under the proposal", r.Number, len(s.changed)+len(s.regressions), s.cases)
	if len(s.regressions) > 0 {
		out += fmt.Sprintf("; %d of them outside the companies the change touches", len(s.regressions))
	}
	return out
}

// legacyRegressionSummary is shown in place of regression evidence
// recorded with counts over every company, before its cases were recorded
// by company, to a reader who may not access every company.
const legacyRegressionSummary = "Regression suite replayed under the proposal; recorded before the results were kept by company, so shown only to readers of every company."

// legacyRegressionKeys are the detail's keys of such evidence that hold
// cases or counts over them.
var legacyRegressionKeys = []string{"cases", "stale", "changed", "regressions"}

// legacyRegressionShown is regression evidence recorded with counts over
// every company: a reader of every company is shown it whole, any other
// reader none of its cases.
func legacyRegressionShown(e Evidence, fields map[string]json.RawMessage, access authz.Access) Evidence {
	if access.All {
		return e
	}
	for _, k := range legacyRegressionKeys {
		delete(fields, k)
	}
	// A map of the raw values just read marshals without fail.
	e.Detail, _ = json.Marshal(fields)
	e.Summary = legacyRegressionSummary
	return e
}

// suiteShown is the regression suite as its reader is shown it: the cases
// of the companies they may access alone, numbered among those (the suite
// is read in the order of its numbers), with their count and the strata
// that selected them (a case of no legal entity only to a reader of every
// company, who is shown the suite whole).
func suiteShown(s RegressionSuite, access authz.Access) RegressionSuite {
	if access.All {
		return s
	}
	cases := []RegressionCase{}
	for _, c := range s.Cases {
		if access.Allows(c.LegalEntityID) {
			c.CaseNo = int32(len(cases) + 1) // numbered among the cases shown (positions)
			cases = append(cases, c)
		}
	}
	s.Cases, s.CaseCount, s.Strata = cases, int32(len(cases)), append([]string{}, strataOf(cases)...)
	return s
}
