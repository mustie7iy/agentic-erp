package telemetry

import (
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil/promlint"
	dto "github.com/prometheus/client_model/go"
)

// Labels are the label names a metric may carry, each with the most values
// it may take. Only bounded enumerations are labels (ADR-0043): a tenant,
// company, principal or document id would make the series grow with the
// business, so per-tenant figures stay in the database.
var Labels = map[string]int{
	"operation":  512, // operationIds of the contracts, and "unmatched"
	"code":       32,  // HTTP status codes the API returns
	"class":      8,   // api.Classes
	"context":    16,  // outbox.Contexts
	"event":      256, // event names in api/asyncapi.yaml
	"gauge":      64,  // names of the gauges a process registers
	"store":      8,   // stores whose encryption at rest is stated
	"encryption": 8,   // how a store encrypts at rest
	// Business and agent metrics (T54b, T54c).
	"status":     16,  // a document's or run's state
	"outcome":    8,   // how a decision ended: approved, rejected, modified …
	"agent_kind": 32,  // agent types
	"alias":      32,  // model aliases (ADR-0036)
	"currency":   200, // ISO 4217 codes
}

// MaxSeries is the most series one process may expose when every label
// takes all its values: the ceiling that keeps a cell's Prometheus small
// whatever its tenants do (ADR-0043).
const MaxSeries = 50_000

// family is what the catalog knows of a registered metric.
type family struct {
	name, help string
	kind       dto.MetricType
	labels     []string
	buckets    int  // a histogram's bucket boundaries
	info       bool // an info metric: exactly rows series
	rows       int
}

// register adds a collector and its catalog entry.
func (m *Metrics) register(c prometheus.Collector, f family) {
	m.registry.MustRegister(c)
	m.families = append(m.families, f)
}

func (m *Metrics) counterVec(name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
	m.register(c, family{name: name, help: help, kind: dto.MetricType_COUNTER, labels: labels})
	return c
}

func (m *Metrics) histogramVec(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help, Buckets: buckets}, labels)
	m.register(h, family{name: name, help: help, kind: dto.MetricType_HISTOGRAM, labels: labels, buckets: len(buckets)})
	return h
}

// series is the most series the family exposes: every label at its bound,
// and for a histogram each bucket, +Inf, _sum and _count.
func (f family) series() int {
	if f.info {
		return f.rows
	}
	n := 1
	for _, l := range f.labels {
		n *= Labels[l]
	}
	if f.kind == dto.MetricType_HISTOGRAM {
		n *= f.buckets + 3
	}
	return n
}

// Check holds the registered metrics to the catalog (ADR-0043): their names
// and help pass promlint, every label is in Labels, and together they stay
// under MaxSeries.
func (m *Metrics) Check() error {
	var errs []error
	mfs := make([]*dto.MetricFamily, 0, len(m.families))
	total := 0
	for _, f := range m.families {
		mf := &dto.MetricFamily{Name: &f.name, Help: &f.help, Type: f.kind.Enum(), Metric: []*dto.Metric{{}}}
		for _, l := range f.labels {
			if _, ok := Labels[l]; !ok {
				errs = append(errs, fmt.Errorf("telemetry: %s: label %q is not in the catalog's allow-list", f.name, l))
			}
			mf.Metric[0].Label = append(mf.Metric[0].Label, &dto.LabelPair{Name: &l, Value: new(string)})
		}
		mfs = append(mfs, mf)
		total += f.series()
	}
	// A linter over metric families reads no input, so Lint cannot fail.
	problems, _ := promlint.NewWithMetricFamilies(mfs).Lint()
	for _, p := range problems {
		errs = append(errs, fmt.Errorf("telemetry: %s: %s", p.Metric, p.Text))
	}
	if total > MaxSeries {
		errs = append(errs, fmt.Errorf("telemetry: at most %d series, over the ceiling of %d", total, MaxSeries))
	}
	return errors.Join(errs...)
}
