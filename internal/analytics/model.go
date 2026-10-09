// Package analytics answers questions on demand. A governed semantic layer
// defines every metric once; the engine computes results deterministically
// from the core store; an agent turns a plain-language question into
// semantic queries and a dashboard spec; the renderer checks that every
// number in the commentary comes from a query result.
package analytics

import (
	_ "embed"
	"fmt"
	"os"
	"slices"

	"gopkg.in/yaml.v3"
)

//go:embed semantic.yaml
var defaultModelYAML []byte

type MetricType string

const (
	Money MetricType = "money"
	Count MetricType = "count"
	Ratio MetricType = "ratio"
)

type Dimension struct {
	Name        string `yaml:"name" json:"name"`
	Label       string `yaml:"label" json:"label"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

type Derived struct {
	Op    string `yaml:"op" json:"op"` // subtract | divide
	Left  string `yaml:"left" json:"left"`
	Right string `yaml:"right" json:"right"`
}

type Metric struct {
	Name        string              `yaml:"name" json:"name"`
	Label       string              `yaml:"label" json:"label"`
	Description string              `yaml:"description" json:"description"`
	Synonyms    []string            `yaml:"synonyms,omitempty" json:"synonyms,omitempty"`
	Type        MetricType          `yaml:"type" json:"type"`
	Source      string              `yaml:"source,omitempty" json:"source,omitempty"`
	Aggregate   string              `yaml:"aggregate,omitempty" json:"aggregate,omitempty"` // sum | count_distinct
	Sign        int64               `yaml:"sign,omitempty" json:"sign,omitempty"`
	Filter      map[string][]string `yaml:"filter,omitempty" json:"filter,omitempty"`
	Dimensions  []string            `yaml:"dimensions,omitempty" json:"dimensions,omitempty"`
	Derived     *Derived            `yaml:"derived,omitempty" json:"derived,omitempty"`
}

type Model struct {
	Version  int `yaml:"version"`
	Calendar struct {
		FiscalYearStartMonth int `yaml:"fiscal_year_start_month"`
	} `yaml:"calendar"`
	Defaults   map[string]string `yaml:"defaults"`
	Dimensions []Dimension       `yaml:"dimensions"`
	Metrics    []Metric          `yaml:"metrics"`

	byName map[string]*Metric
}

// DefaultModel returns the embedded semantic model.
func DefaultModel() (*Model, error) { return ParseModel(defaultModelYAML) }

// LoadModel reads a semantic model from a YAML file.
func LoadModel(path string) (*Model, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseModel(b)
}

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
		met := &m.Metrics[i]
		switch {
		case met.Derived != nil:
			l, r := m.byName[met.Derived.Left], m.byName[met.Derived.Right]
			if l == nil || r == nil {
				return fmt.Errorf("semantic model: %s derives from unknown metric", met.Name)
			}
			if met.Derived.Op != "subtract" && met.Derived.Op != "divide" {
				return fmt.Errorf("semantic model: %s has unknown op %q", met.Name, met.Derived.Op)
			}
		case met.Source != "":
			if met.Aggregate != "sum" && met.Aggregate != "count_distinct" {
				return fmt.Errorf("semantic model: %s has unknown aggregate %q", met.Name, met.Aggregate)
			}
			if met.Sign == 0 {
				met.Sign = 1
			}
			for _, d := range met.Dimensions {
				if !dims[d] {
					return fmt.Errorf("semantic model: %s uses undeclared dimension %s", met.Name, d)
				}
			}
		default:
			return fmt.Errorf("semantic model: %s needs a source or a derivation", met.Name)
		}
	}
	for word, metric := range m.Defaults {
		if m.byName[metric] == nil {
			return fmt.Errorf("semantic model: default %q points to unknown metric %s", word, metric)
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
