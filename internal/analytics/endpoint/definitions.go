package endpoint

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/analytics/store"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Configs reads the tenant's configuration documents in the request's
// transaction (configuration.Settings): the semantic model is the
// document's analytics section (ADR-0044, T55g).
type Configs interface {
	// Document is the active version and its number; a tenant without one
	// is 409 config.not_bootstrapped.
	Document(ctx context.Context) (*config.Config, int32, error)
	// Version is an activated version's; an unknown one is 422.
	Version(ctx context.Context, number int32) (*config.Config, error)
}

// model is the governed model a query is answered under: the active
// configuration version's, or version's when asked (0: the active one),
// with the version's number.
func (e *Endpoints) model(ctx context.Context, version int32) (*analytics.Model, int32, error) {
	var doc *config.Config
	var err error
	if version > 0 {
		doc, err = e.Configs.Version(ctx, version)
	} else {
		doc, version, err = e.Configs.Document(ctx)
	}
	if err != nil {
		return nil, 0, err
	}
	m, err := analytics.FromConfig(doc)
	if err != nil {
		// Validated when its change request was submitted (Section): a
		// version this binary cannot read is a server fault, not the query's.
		return nil, 0, fmt.Errorf("analytics: the semantic model of configuration version %d: %w", version, err)
	}
	return m, version, nil
}

// Section checks a proposed document's semantic model when its change
// request is submitted, so an activated version always reads
// (configuration.Section). It keeps nothing in tables of its own.
type Section struct{}

// Check implements configuration.Section.
func (Section) Check(_ context.Context, _, proposed *config.Config) ([]config.Finding, error) {
	if proposed.Analytics == nil {
		return nil, nil
	}
	_, unreadable := analytics.FromConfig(proposed)
	return finding(unreadable), nil
}

// finding is a model that does not read as the document's error: a
// finding of the proposal, not a failure of the check.
func finding(unreadable error) []config.Finding {
	if unreadable == nil {
		return nil
	}
	return []config.Finding{{Severity: config.SevError, Path: "analytics", Message: unreadable.Error()}}
}

// Apply implements configuration.Section: the model is read from the
// active version itself.
func (Section) Apply(context.Context, *config.Config) error { return nil }

// Simulation shows a change request's approvers what it does to the
// metrics whose meaning it changes: each one's value by company and
// fiscal year, for the current and the previous fiscal year, under the
// base and under the proposed definitions (configuration.Simulator). It
// reads the analytical store as the person who asked for the simulation
// (who must hold analytics.query.run), within what they may read; the
// database path reads the API with a request's credentials, which a
// queued simulation has not, so a store that cannot answer fails the
// simulation, to be run again once the store is current (ADR-0044).
type Simulation struct {
	Endpoints *Endpoints
	Now       func() time.Time
}

// errNoStore is the store failing to answer, which the simulation does
// not fall back from.
var errNoStore = errors.New("analytics: the analytical store cannot answer")

// noDatabase is the simulation's database path: none.
type noDatabase struct{}

func (noDatabase) Facts(context.Context, *analytics.Model, time.Time) (*analytics.Facts, error) {
	return nil, errNoStore
}

// Value is one metric's value for a company and fiscal year before and
// after a change; "" where the metric is not defined or has no data.
type Value struct {
	Company    string `json:"company"`
	FiscalYear string `json:"fiscal_year"`
	Before     string `json:"before"`
	After      string `json:"after"`
}

// MetricChange is an affected metric's values.
type MetricChange struct {
	Metric  string  `json:"metric"`
	Changed int     `json:"changed"` // values that differ
	Values  []Value `json:"values"`
}

// Simulate implements configuration.Simulator.
func (s Simulation) Simulate(ctx context.Context, base, proposed *config.Config) (configuration.Simulation, error) {
	before, err := analytics.FromConfig(base)
	if err != nil {
		return configuration.Simulation{}, fmt.Errorf("analytics: simulate: the base model: %w", err)
	}
	after, err := analytics.FromConfig(proposed)
	if err != nil {
		return configuration.Simulation{}, fmt.Errorf("analytics: simulate: the proposed model: %w", err)
	}
	affected := analytics.Affected(before, after)
	if len(affected) == 0 {
		return configuration.Simulation{Passed: true}, nil
	}
	if err := authz.Require(ctx, "analytics.query.run"); err != nil {
		return configuration.Simulation{}, err
	}
	p, err := s.Endpoints.principal(ctx, nil)
	if err != nil {
		return configuration.Simulation{}, err
	}
	year := clock.Now(s.Now).Year()
	was, err := s.values(ctx, before, p, affected, year)
	if err == nil {
		var is map[string]map[string]string
		if is, err = s.values(ctx, after, p, affected, year); err == nil {
			return compare(affected, was, is, year), nil
		}
	}
	if !errors.Is(err, errNoStore) {
		return configuration.Simulation{}, err
	}
	return configuration.Simulation{Summary: "The values of metrics " + strings.Join(affected, ", ") +
		" could not be computed: the analytical store is not current. Simulate again once it is.",
		Detail: map[string]any{"metrics": affected}}, nil
}

// values are each metric's values under m, keyed by metric and then
// company and fiscal year, from fiscal year year-1 on.
func (s Simulation) values(ctx context.Context, m *analytics.Model, p analytics.Principal, metrics []string, year int) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	for _, name := range metrics {
		if m.Metric(name) == nil {
			continue
		}
		q := recent(m, name, year)
		datasets, err := s.Endpoints.datasets(ctx, m, q)
		if err != nil {
			return nil, err
		}
		src := store.Source{Reader: s.Endpoints.Reader, Database: noDatabase{}, Datasets: datasets,
			Requires: analytics.Source{Datasets: datasets}.Permissions(), Now: s.Now, Log: s.Endpoints.Log}
		facts, err := src.Facts(ctx, m, p, q, time.Time{})
		if err != nil {
			return nil, err
		}
		res, err := analytics.Execute(m, facts, p, q)
		if err != nil {
			// The same data refuses it again: the request's problem, which
			// fails the operation rather than retry it.
			return nil, problem.Unprocessable("analytics.invalid_query", fmt.Sprintf("Metric %s cannot be simulated: %v.", name, err))
		}
		out[name] = cells(res)
	}
	return out, nil
}

// recent is the query of a metric's recent values: by company and fiscal
// year from year-1, as far as the metric allows those breakdowns.
func recent(m *analytics.Model, name string, year int) analytics.Query {
	q := analytics.Query{Measures: []string{name}}
	allowed := m.AllowedDimensions(name)
	for _, d := range []string{"company", "fiscal_year"} {
		if slices.Contains(allowed, d) {
			q.By = append(q.By, d)
		}
	}
	if slices.Contains(allowed, "fiscal_year") {
		q.Where = []analytics.Filter{{Dim: "fiscal_year", Op: ">=", Values: []string{strconv.Itoa(year - 1)}}}
	}
	return q
}

// cells are a result's measure by its dimensions' values ("company/year").
func cells(res *analytics.Result) map[string]string {
	out := map[string]string{}
	for _, row := range res.Rows {
		var key []string
		value := ""
		for i, c := range res.Columns {
			switch {
			case c.Kind == "dimension":
				key = append(key, fmt.Sprint(row[i]))
			case row[i] != nil:
				value = fmt.Sprint(row[i])
			}
		}
		out[strings.Join(key, "/")] = value
	}
	return out
}

// compare is the simulation's evidence: every affected metric's values
// side by side. It passes: the values are for the approvers to judge.
func compare(affected []string, was, is map[string]map[string]string, year int) configuration.Simulation {
	changes := make([]MetricChange, 0, len(affected))
	total, differ := 0, 0
	for _, name := range affected {
		mc := MetricChange{Metric: name, Values: []Value{}}
		keys := map[string]bool{}
		for k := range was[name] {
			keys[k] = true
		}
		for k := range is[name] {
			keys[k] = true
		}
		for _, k := range slices.Sorted(maps.Keys(keys)) {
			company, fy, _ := strings.Cut(k, "/")
			v := Value{Company: company, FiscalYear: fy, Before: was[name][k], After: is[name][k]}
			if v.Before != v.After {
				mc.Changed++
			}
			mc.Values = append(mc.Values, v)
		}
		total, differ = total+len(mc.Values), differ+mc.Changed
		changes = append(changes, mc)
	}
	summary := fmt.Sprintf("Metric definitions change for %s: %d of %d values by company and fiscal year since %d differ.",
		strings.Join(affected, ", "), differ, total, year-1)
	return configuration.Simulation{Passed: true, Summary: summary, Detail: map[string]any{"metrics": changes}}
}
