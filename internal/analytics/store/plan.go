package store

import (
	"fmt"
	"maps"
	"slices"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/analytics/wire"
)

// Plans is the reader plan of each base metric q's measures need: the
// metric's dataset and aggregate, grouped by the query's breakdown, the
// metric's filter and the query's as filters, in one ledger (the dataset
// keyed by ledger is read in it alone) and with the model's calendar. The
// semantic dimensions are the contract's column names (datasets_test), so
// they pass as they are.
func Plans(m *analytics.Model, q analytics.Query, ledger string) (map[string]wire.Plan, error) {
	plans := map[string]wire.Plan{}
	for _, name := range q.Measures {
		bases := m.BaseMetrics(name)
		if bases == nil {
			return nil, fmt.Errorf("unknown metric %q", name)
		}
		for _, b := range bases {
			met := m.Metric(b)
			p := wire.Plan{Dataset: met.Source, Aggregate: met.Aggregate, By: q.By, Ledger: ledger,
				FiscalYearStartMonth: m.Calendar.FiscalYearStartMonth}
			for _, d := range slices.Sorted(maps.Keys(met.Filter)) {
				p.Where = append(p.Where, wire.Filter{Column: d, Op: "in", Values: met.Filter[d]})
			}
			for _, f := range q.Where {
				p.Where = append(p.Where, wire.Filter{Column: f.Dim, Op: f.Op, Values: f.Values})
			}
			plans[b] = p
		}
	}
	return plans, nil
}
