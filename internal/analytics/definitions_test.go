package analytics

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
)

// TestFromConfig: a document without an analytics section reads the seed;
// one with a section reads it, defaults applied to a copy; an invalid
// section is refused.
func TestFromConfig(t *testing.T) {
	seed, err := DefaultModel()
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]*config.Config{"no document": nil, "no section": {}} {
		t.Run(name, func(t *testing.T) {
			m, err := FromConfig(c)
			if err != nil || !reflect.DeepEqual(m.Metrics, seed.Metrics) {
				t.Errorf("got %v, %v; want the seed", m, err)
			}
		})
	}
	section := &config.Analytics{Dimensions: []config.Dimension{{Name: "d"}},
		Metrics: []config.Metric{{Name: "a", Type: Money, Source: "gl_lines", Aggregate: "sum", Dimensions: []string{"d"}}}}
	m, err := FromConfig(&config.Config{Analytics: section})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Metric("a").Sign; got != 1 {
		t.Errorf("sign: got %d, want the default 1", got)
	}
	if got := section.Metrics[0].Sign; got != 0 {
		t.Errorf("the document's sign: got %d, want it unchanged (0)", got)
	}
	for want, bad := range map[string]config.Metric{
		"needs a source":          {Name: "b", Type: Money},
		`unknown type "amount"`:   {Name: "b", Type: "amount", Source: "gl_lines", Aggregate: "sum"},
		`unknown dataset "gl_li"`: {Name: "b", Type: Money, Source: "gl_li", Aggregate: "sum"},
	} {
		t.Run(want, func(t *testing.T) {
			s := *section
			s.Metrics = append(slices.Clone(section.Metrics), bad)
			if _, err := FromConfig(&config.Config{Analytics: &s}); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("got %v, want an error containing %s", err, want)
			}
		})
	}
}

// TestAffected: the metrics a change can move are those added, removed or
// computed differently, through derivations and the calendar; words and
// breakdowns move none.
func TestAffected(t *testing.T) {
	seed := func(change func(*config.Analytics)) *Model {
		t.Helper()
		m, err := DefaultModel()
		if err != nil {
			t.Fatal(err)
		}
		a := m.Analytics
		a.Metrics = append([]config.Metric(nil), a.Metrics...)
		change(&a)
		out, err := FromConfig(&config.Config{Analytics: &a})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	metric := func(a *config.Analytics, name string) *config.Metric {
		for i := range a.Metrics {
			if a.Metrics[i].Name == name {
				return &a.Metrics[i]
			}
		}
		t.Fatalf("no metric %s", name)
		return nil
	}
	base := seed(func(*config.Analytics) {})
	cases := map[string]struct {
		change func(*config.Analytics)
		want   string
	}{
		"words": {func(a *config.Analytics) {
			metric(a, "revenue").Label, metric(a, "revenue").Synonyms = "Sales", nil
			metric(a, "revenue").Dimensions = []string{"company"}
		}, ""},
		"a filter": {func(a *config.Analytics) { metric(a, "expenses").Filter = map[string][]string{"account": {"610000"}} },
			"expenses operating_margin operating_profit"},
		"a derivation": {func(a *config.Analytics) {
			metric(a, "operating_profit").Derived = &config.Derived{Op: "subtract", Left: "revenue", Right: "revenue"}
		},
			"operating_margin operating_profit"},
		"added and removed": {func(a *config.Analytics) {
			a.Metrics = append(a.Metrics, config.Metric{Name: "aa_new", Type: Count, Source: "ap_invoices", Aggregate: "count_distinct"})
			a.Metrics = slicesDelete(a.Metrics, "invoice_count")
		}, "aa_new invoice_count"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := strings.Join(Affected(base, seed(c.change)), " "); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
	t.Run("the calendar", func(t *testing.T) {
		if got, want := len(Affected(base, seed(func(a *config.Analytics) { a.Calendar.FiscalYearStartMonth = 7 }))), len(base.Metrics); got != want {
			t.Errorf("got %d metrics, want every one (%d)", got, want)
		}
	})
}

// slicesDelete is metrics without the one named.
func slicesDelete(ms []config.Metric, name string) []config.Metric {
	out := []config.Metric{}
	for _, m := range ms {
		if m.Name != name {
			out = append(out, m)
		}
	}
	return out
}
