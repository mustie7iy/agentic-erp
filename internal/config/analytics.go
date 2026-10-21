package config

// Analytics is the semantic layer's governed model in the configuration
// document (ADR-0044, T55g): the metrics every report computes, the
// dimensions they break down by, the fiscal calendar and what everyday
// words mean. It changes only through a change request, so a metric's
// meaning changes with a simulation of its values before and after, an
// approval and a configuration version; the analytics context validates
// and reads it (analytics.FromConfig).
type Analytics struct {
	Calendar   AnalyticsCalendar `yaml:"calendar" json:"calendar"`
	Defaults   map[string]string `yaml:"defaults,omitempty" json:"defaults,omitempty"`
	Dimensions []Dimension       `yaml:"dimensions" json:"dimensions"`
	Metrics    []Metric          `yaml:"metrics" json:"metrics"`
}

// AnalyticsCalendar is the fiscal calendar metrics are grouped by.
type AnalyticsCalendar struct {
	FiscalYearStartMonth int `yaml:"fiscal_year_start_month" json:"fiscal_year_start_month"`
}

// MetricType is what a metric's values are.
type MetricType string

// Dimension is a column metrics break down and filter by.
type Dimension struct {
	Name        string `yaml:"name" json:"name"`
	Label       string `yaml:"label" json:"label"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

// Derived is a metric computed from two others.
type Derived struct {
	Op    string `yaml:"op" json:"op"` // subtract | divide
	Left  string `yaml:"left" json:"left"`
	Right string `yaml:"right" json:"right"`
}

// Metric is one governed metric: read from a fact table or derived.
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
