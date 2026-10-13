package nfr

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/config"
)

// Budgets are about 10-25x the time measured on a laptop: tight enough to
// catch an accidental O(n^2), loose enough to ignore machine noise. Run
// with -short to skip them.
func budget(t *testing.T, name string, limit time.Duration, f func()) {
	t.Helper()
	limit = Budget(limit)
	start := time.Now()
	f()
	took := time.Since(start)
	t.Logf("%s took %v (budget %v)", name, took, limit)
	if took > limit {
		t.Fatalf("%s took %v, over the %v budget", name, took, limit)
	}
}

func skipShort(t *testing.T) {
	t.Helper()
	if testing.Short() || raceDetector {
		t.Skip("performance budget: runs without -short and without the race detector (make perf, CI)")
	}
}

// Ten semantic queries over 60,000 journal lines plus orders and invoices.
func TestPerfAnalyticsQuery(t *testing.T) {
	skipShort(t)
	facts := LargeFacts(1_000, 20_000)
	m, _ := analytics.DefaultModel()
	budget(t, "10 semantic queries", 3*time.Second, func() {
		for i := 0; i < 10; i++ {
			if _, err := analytics.Execute(m, facts, analytics.Principal{ID: "u"}, analytics.Query{
				Measures: []string{"invoice_value", "purchase_order_value"}, By: []string{"fiscal_month", "vendor"},
			}); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// Validating and diffing a configuration with 2,000 number ranges.
func TestPerfConfiguration(t *testing.T) {
	skipShort(t)
	b, err := os.ReadFile("../config/testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := config.Parse(b)
	other, _ := config.Parse(b)
	for i := 0; i < 2_000; i++ {
		c.NumberRanges = append(c.NumberRanges, config.NumberRange{Company: "1000", DocumentType: fmt.Sprintf("type_%c", 'a'+i%26),
			FiscalYear: 2000 + i/26, First: 1, Last: 999999})
	}
	budget(t, "validate + diff (2k number ranges)", 3*time.Second, func() {
		if config.HasErrors(config.Validate(c)) {
			t.Fatal("unexpected errors")
		}
		changes, err := config.Diff(other, c)
		if err != nil || len(changes) != 2_000 {
			t.Fatalf("diff: %v %d", err, len(changes))
		}
	})
}

func BenchmarkSemanticQuery(b *testing.B) {
	facts := LargeFacts(200, 5_000)
	m, _ := analytics.DefaultModel()
	q := analytics.Query{Measures: []string{"expenses"}, By: []string{"fiscal_month"}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := analytics.Execute(m, facts, analytics.Principal{ID: "u"}, q); err != nil {
			b.Fatal(err)
		}
	}
}
