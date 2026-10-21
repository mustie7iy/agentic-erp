package payables

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
)

// byCompanyKey is the simulation detail's key for what replaying each
// company's invoices found (CompanyReplay).
const byCompanyKey = "invoices_by_company"

// recordedSummary is the simulation's summary as recorded: no count, since
// counts over every company would tell a reader of narrower access that
// other companies' invoices change; each reader is shown the counts over
// their own (Disclosure).
func recordedSummary(days int) string {
	return fmt.Sprintf("Invoices on purchase orders of the last %d days replayed under the proposal.", days)
}

// legacySummary is shown in place of a simulation recorded with counts over
// every company, before they were recorded by company, to a reader who may
// not access every company.
const legacySummary = "Invoices replayed under the proposal; recorded before the results were kept by company, so shown only to readers of every company."

// legacyKeys are the detail's keys of such a simulation.
var legacyKeys = []string{"window_days", "invoices", "decisions", "changes", "breaks", "postings"}

// Disclosure shows the reader of a change request the simulated invoices
// of the companies they may access alone (ADR-0010, 04-api-design §4.9),
// with the summary's counts over those: the simulation replays them as the
// person who asked for it, over every company of the tenant, so that the
// verdict covers the whole change. It fails closed: a reader of no company
// is shown no invoice and counts of zero, and a replay of no legal entity
// is shown only to a reader of every company. The verdict (passed) is the
// change request's and is shown whole, as its status shows it anyway.
type Disclosure struct{}

// Shown is the simulation's detail as a reader is shown it: what replaying
// the invoices of the companies they may access found, with the first
// examples of changes and breaks in the order of the companies' codes.
type Shown struct {
	WindowDays int        `json:"window_days"`
	Invoices   int        `json:"invoices"`
	Decisions  Decisions  `json:"decisions"`
	Changes    []Decision `json:"changes"`
	Breaks     []Broken   `json:"breaks"`
	Postings   []Posting  `json:"postings"`
}

// Decisions counts the shown invoices by how they are decided.
type Decisions struct {
	Changed   int            `json:"changed"`
	Unchanged int            `json:"unchanged"`
	Base      map[string]int `json:"base"`
	Proposed  map[string]int `json:"proposed"`
}

// recorded is the simulation's detail as recorded.
type recorded struct {
	WindowDays int             `json:"window_days"`
	Companies  []CompanyReplay `json:"invoices_by_company"`
}

// Disclose implements configuration.Disclosure.
func (Disclosure) Disclose(_ context.Context, e configuration.Evidence, access authz.Access) (configuration.Evidence, error) {
	if e.Kind != "simulation" {
		return e, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(e.Detail, &fields); err != nil {
		return configuration.Evidence{}, fmt.Errorf("payables: simulated invoices: %w", err)
	}
	legacy := fields[byCompanyKey] == nil && fields["decisions"] != nil
	if fields[byCompanyKey] == nil && !legacy {
		return e, nil // another context's simulation
	}
	if legacy {
		return legacyShown(e, fields, access), nil
	}
	var rec recorded
	if err := json.Unmarshal(e.Detail, &rec); err != nil {
		return configuration.Evidence{}, fmt.Errorf("payables: simulated invoices: %w", err)
	}
	shown := shownTo(rec, access)
	// Structs of strings, numbers and maps of them marshal without fail,
	// and so does a map of the raw values just read.
	b, _ := json.Marshal(shown)
	var shownFields map[string]json.RawMessage
	_ = json.Unmarshal(b, &shownFields) // b is the object just marshalled
	delete(fields, byCompanyKey)
	maps.Copy(fields, shownFields)
	e.Detail, _ = json.Marshal(fields)
	// A summary not recorded as this simulation records it carries no
	// count of ours, so it is shown as it is.
	e.Summary = strings.Replace(e.Summary, recordedSummary(rec.WindowDays), shown.summary(), 1)
	return e, nil
}

// shownTo is what the reader with the access is shown of the recorded
// replays: those of the companies they may access, counted together.
func shownTo(rec recorded, access authz.Access) Shown {
	s := Shown{WindowDays: rec.WindowDays, Decisions: Decisions{Base: map[string]int{}, Proposed: map[string]int{}},
		Changes: []Decision{}, Breaks: []Broken{}, Postings: []Posting{}}
	for _, c := range rec.Companies {
		if !access.Allows(c.LegalEntityID) {
			continue
		}
		s.Invoices += c.Invoices
		s.Decisions.Changed += c.Changed
		s.Decisions.Unchanged += c.Invoices - c.Changed - c.Broken
		for outcome, n := range c.Base {
			s.Decisions.Base[outcome] += n
		}
		for outcome, n := range c.Proposed {
			s.Decisions.Proposed[outcome] += n
		}
		s.Changes = first(append(s.Changes, c.Changes...), examples)
		s.Breaks = first(append(s.Breaks, c.Breaks...), examples)
		s.Postings = append(s.Postings, c.Postings...)
	}
	return s
}

// first is at most the first n of list.
func first[T any](list []T, n int) []T {
	return list[:min(n, len(list))]
}

// broken counts the shown invoices the proposal cannot evaluate.
func (s Shown) broken() int {
	return s.Invoices - s.Decisions.Changed - s.Decisions.Unchanged
}

// summary is the line for approvers, counted over the shown invoices.
func (s Shown) summary() string {
	out := fmt.Sprintf("%d invoices of the last %d days replayed: %d decided differently, %d posting rules move",
		s.Invoices, s.WindowDays, s.Decisions.Changed, len(s.Postings))
	if n := s.broken(); n > 0 {
		out += fmt.Sprintf("; %d cannot be evaluated under the proposal", n)
	}
	return out
}

// legacyShown is a simulation recorded with counts over every company: a
// reader of every company is shown it whole, any other reader none of it.
func legacyShown(e configuration.Evidence, fields map[string]json.RawMessage, access authz.Access) configuration.Evidence {
	if access.All {
		return e
	}
	for _, k := range legacyKeys {
		delete(fields, k)
	}
	// A map of the raw values just read marshals without fail.
	e.Detail, _ = json.Marshal(fields)
	e.Summary = legacySummary
	return e
}
