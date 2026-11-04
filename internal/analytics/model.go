// Package analytics answers questions on demand (the analytics context of
// 02-architecture §2.2, run as an API client). A governed semantic layer
// defines every metric once; the engine computes results deterministically
// from the core store; an agent turns a plain-language question into
// semantic queries and a dashboard spec; the renderer checks that every
// number in the commentary comes from a query result.
package analytics

import (
	_ "embed"
	"fmt"
	"maps"
	"os"
	"slices"

	"gopkg.in/yaml.v3"

	"github.com/mustie7iy/agentic-erp/internal/config"
)

//go:embed semantic.yaml
var defaultModelYAML []byte

// The semantic model's types are the configuration document's: the model
// lives in its analytics section (ADR-0044, T55g).
type (
	MetricType = config.MetricType
	Dimension  = config.Dimension
	Derived    = config.Derived
	Metric     = config.Metric
)

const (
	Money MetricType = "money"
	Count MetricType = "count"
	Ratio MetricType = "ratio"
)

// Model is the governed semantic model, indexed for queries.
type Model struct {
	Version          int `yaml:"version"`
	config.Analytics `yaml:",inline"`

	byName map[string]*Metric
}

// DefaultModel returns the embedded semantic model.
func DefaultModel() (*Model, error) { return ParseModel(defaultModelYAML) }

// LoadModel reads a semantic model from a YAML file.
func LoadModel(path string) (*Model, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("analytics: load model: %w", err)
	}
	return ParseModel(b)
}

// FromConfig is the governed model of a configuration document: its
// analytics section, or the seed (semantic.yaml) for a document without
// one, as every tenant's is until a change request adds it (ADR-0044,
// T55g). The document is not changed.
func FromConfig(c *config.Config) (*Model, error) {
	if c == nil || c.Analytics == nil {
		return DefaultModel()
	}
	m := &Model{Version: 1, Analytics: *c.Analytics}
	m.Metrics = slices.Clone(m.Metrics) // index defaults signs in place
	if err := m.index(); err != nil {
		return nil, err
	}
	if err := m.governed(); err != nil {
		return nil, err
	}
	return m, nil
}

// governed refuses what a model from a change request may not hold beyond
// what index checks: a metric over a table that is no fact dataset (which
// no permission would gate and nothing would answer), or of a type the
// engine does not know.
func (m *Model) governed() error {
	for _, met := range m.Metrics {
		if met.Type != Money && met.Type != Count && met.Type != Ratio {
			return fmt.Errorf("semantic model: %s has unknown type %q", met.Name, met.Type)
		}
		if met.Source != "" && !slices.ContainsFunc(FactDatasets, func(d FactDataset) bool { return d.Dataset == met.Source }) {
			return fmt.Errorf("semantic model: %s reads unknown dataset %q", met.Name, met.Source)
		}
	}
	return nil
}

// ParseModel reads a semantic model from YAML.
func ParseModel(b []byte) (*Model, error) {
	var m Model
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("semantic model: %w", err)
	}
	if err := m.index(); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *Model) index() error {
	if m.Calendar.FiscalYearStartMonth == 0 {
		m.Calendar.FiscalYearStartMonth = 1
	}
	if s := m.Calendar.FiscalYearStartMonth; s < 1 || s > 12 {
		return fmt.Errorf("semantic model: fiscal_year_start_month %d out of range", s)
	}
	dims := map[string]bool{}
	for _, d := range m.Dimensions {
		dims[d.Name] = true
	}
	m.byName = map[string]*Metric{}
	for i := range m.Metrics {
		met := &m.Metrics[i]
		if _, dup := m.byName[met.Name]; dup {
			return fmt.Errorf("semantic model: duplicate metric %s", met.Name)
		}
		m.byName[met.Name] = met
	}
	for i := range m.Metrics {
		if err := m.checkMetric(&m.Metrics[i], dims); err != nil {
			return err
		}
	}
	walked := map[string]int{}
	for _, met := range m.Metrics {
		if err := m.acyclic(met.Name, walked); err != nil {
			return err
		}
	}
	// Sorted, so a model with several bad defaults reports the same one
	// every time.
	for _, word := range slices.Sorted(maps.Keys(m.Defaults)) {
		if metric := m.Defaults[word]; m.byName[metric] == nil {
			return fmt.Errorf("semantic model: default %q points to unknown metric %s", word, metric)
		}
	}
	return nil
}

// acyclic refuses a metric derived, through any chain, from itself: a
// model from a change request is not trusted to be well formed, and the
// derivations are walked recursively. state is each metric's: 1 while its
// derivation is walked, 2 once it is known acyclic (each walked once).
func (m *Model) acyclic(name string, state map[string]int) error {
	switch state[name] {
	case 1:
		return fmt.Errorf("semantic model: %s derives from itself", name)
	case 2:
		return nil
	}
	state[name] = 1
	if d := m.byName[name].Derived; d != nil {
		if err := m.acyclic(d.Left, state); err != nil {
			return err
		}
		if err := m.acyclic(d.Right, state); err != nil {
			return err
		}
	}
	state[name] = 2
	return nil
}

// checkMetric validates one metric once all metrics are indexed, so a
// derivation may refer to metrics declared after it.
func (m *Model) checkMetric(met *Metric, dims map[string]bool) error {
	switch {
	case met.Derived != nil:
		l, r := m.byName[met.Derived.Left], m.byName[met.Derived.Right]
		if l == nil || r == nil {
			return fmt.Errorf("semantic model: %s derives from unknown metric", met.Name)
		}
		switch met.Derived.Op {
		case "subtract", "divide":
		case "complement":
			// One less a quotient is a share of something: a ratio.
			if met.Type != Ratio {
				return fmt.Errorf("semantic model: %s is a complement of type %q, want ratio", met.Name, met.Type)
			}
		default:
			return fmt.Errorf("semantic model: %s has unknown op %q", met.Name, met.Derived.Op)
		}
		return nil
	case met.Source != "":
		return checkSourceMetric(met, dims)
	default:
		return fmt.Errorf("semantic model: %s needs a source or a derivation", met.Name)
	}
}

// checkSourceMetric validates a metric read from a fact table and
// defaults its sign to +1. A sign is +1 or -1: any other factor could
// scale an amount out of range.
func checkSourceMetric(met *Metric, dims map[string]bool) error {
	if met.Aggregate != "sum" && met.Aggregate != "count_distinct" {
		return fmt.Errorf("semantic model: %s has unknown aggregate %q", met.Name, met.Aggregate)
	}
	switch met.Sign {
	case 0:
		met.Sign = 1
	case 1, -1:
	default:
		return fmt.Errorf("semantic model: %s has sign %d, want 1 or -1", met.Name, met.Sign)
	}
	for _, d := range met.Dimensions {
		if !dims[d] {
			return fmt.Errorf("semantic model: %s uses undeclared dimension %s", met.Name, d)
		}
	}
	return nil
}

func (m *Model) Metric(name string) *Metric { return m.byName[name] }

// AllowedDimensions returns the dimensions a metric can be broken down by;
// for derived metrics, the dimensions both inputs share.
func (m *Model) AllowedDimensions(name string) []string {
	met := m.byName[name]
	if met == nil {
		return nil
	}
	if met.Derived == nil {
		return met.Dimensions
	}
	left := m.AllowedDimensions(met.Derived.Left)
	right := m.AllowedDimensions(met.Derived.Right)
	var out []string
	for _, d := range left {
		if slices.Contains(right, d) {
			out = append(out, d)
		}
	}
	return out
}

// baseMetrics lists the source metrics a metric is computed from.
func (m *Model) baseMetrics(name string) []string {
	met := m.byName[name]
	if met.Derived == nil {
		return []string{name}
	}
	return append(m.baseMetrics(met.Derived.Left), m.baseMetrics(met.Derived.Right)...)
}

// BaseMetrics lists the source metrics a metric is computed from, each
// once, in the order a derivation names them; nil for an unknown metric.
func (m *Model) BaseMetrics(name string) []string {
	if m.byName[name] == nil {
		return nil
	}
	var out []string
	for _, b := range m.baseMetrics(name) {
		if !slices.Contains(out, b) {
			out = append(out, b)
		}
	}
	return out
}
