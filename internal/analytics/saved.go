package analytics

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// SpecError is a saved report whose specification its results do not
// support (Rerun): a tile, chart or table naming a query the report lacks
// or a column a result does not have. The API refuses it naming each
// problem (ADR-0076 §2).
type SpecError struct {
	Problems []string
}

func (e *SpecError) Error() string {
	return fmt.Sprintf("saved report no longer valid: %s", strings.Join(e.Problems, "; "))
}

// RemovedMetric is the first metric a report's queries name that m does
// not have, with the id of the query naming it, in the order of the
// queries' ids; "" when m has every one (ADR-0076 §2: a metric removed
// since the report was saved).
func RemovedMetric(m *Model, queries map[string]Query) (id, metric string) {
	for _, qid := range slices.Sorted(maps.Keys(queries)) {
		for _, name := range queries[qid].Measures {
			if m.Metric(name) == nil {
				return qid, name
			}
		}
	}
	return "", ""
}
