package analytics

import (
	"encoding/json"
	"maps"
	"slices"
)

// Affected lists, sorted, the metrics whose values a change of the model
// from base to proposed can move: those added or removed, and those
// computed differently — their own definition, one they derive from, or
// the fiscal calendar. Words (labels, descriptions, synonyms) and allowed
// breakdowns move no value. A change request's simulation shows these
// metrics' values before and after (ADR-0044, T55g).
func Affected(base, proposed *Model) []string {
	names := map[string]bool{}
	for _, m := range []*Model{base, proposed} {
		for _, met := range m.Metrics {
			names[met.Name] = true
		}
	}
	calendar := base.Calendar != proposed.Calendar
	was, is := map[string]string{}, map[string]string{}
	var out []string
	for _, n := range slices.Sorted(maps.Keys(names)) {
		if calendar || base.meaning(n, was) != proposed.meaning(n, is) {
			out = append(out, n)
		}
	}
	return out
}

// meaning is what a metric computes, through its derivation: "" for a
// metric the model does not define. memo holds the meanings found, so a
// metric shared by many derivations is described once.
func (m *Model) meaning(name string, memo map[string]string) string {
	met := m.byName[name]
	if met == nil {
		return ""
	}
	if s, ok := memo[name]; ok {
		return s
	}
	type def struct {
		Type        MetricType          `json:"type"`
		Source      string              `json:"source,omitempty"`
		Aggregate   string              `json:"aggregate,omitempty"`
		Sign        int64               `json:"sign,omitempty"`
		Filter      map[string][]string `json:"filter,omitempty"`
		Op          string              `json:"op,omitempty"`
		Left, Right string              `json:",omitempty"`
	}
	d := def{Type: met.Type, Source: met.Source, Aggregate: met.Aggregate, Sign: met.Sign, Filter: met.Filter}
	if met.Derived != nil {
		d.Op, d.Left, d.Right = met.Derived.Op, m.meaning(met.Derived.Left, memo), m.meaning(met.Derived.Right, memo)
	}
	b, _ := json.Marshal(d) // strings, numbers and string maps: it cannot fail
	memo[name] = string(b)
	return memo[name]
}
