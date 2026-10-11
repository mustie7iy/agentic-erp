package telemetry

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// readTimeout bounds one gauge read on a scrape.
const readTimeout = 2 * time.Second

// gauge reads its value when scraped: backlogs live in the database, so
// they are measured where they are rather than sampled by a goroutine.
type gauge struct {
	desc   *prometheus.Desc
	read   func(context.Context) (float64, error)
	errors prometheus.Counter
}

func (g *gauge) Describe(ch chan<- *prometheus.Desc) { ch <- g.desc }

func (g *gauge) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	v, err := g.read(ctx)
	if err != nil {
		g.errors.Inc()
		return
	}
	ch <- prometheus.MustNewConstMetric(g.desc, prometheus.GaugeValue, v)
}

// Gauge registers a gauge read on every scrape. A read that fails adds to
// erp_gauge_read_errors_total and leaves the sample out.
func (m *Metrics) Gauge(name, help string, read func(context.Context) (float64, error)) {
	if m.readErrors == nil {
		m.readErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "erp_gauge_read_errors_total", Help: "Gauge reads that failed, by gauge.",
		}, []string{"gauge"})
		m.Registry.MustRegister(m.readErrors)
	}
	m.Registry.MustRegister(&gauge{desc: prometheus.NewDesc(name, help, nil, nil), read: read, errors: m.readErrors.WithLabelValues(name)})
}
