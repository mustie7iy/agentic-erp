package processmining_test

import (
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/nfr"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

// TestEventLogBudget holds a read of the purchase-to-pay log to the
// budget of a report read (p95 3 s; plan-breadth-week3, what every slice
// reuses) at 01 §1.3's volume: the largest tenant's 20 million supplier
// invoices a year over its 500 companies are 40,000 a company a year,
// some 3,400 a month, so a month's log of one company holds 4,000 orders
// here, each with its requisition, receipt, invoice and payment and nine
// audit events (36,000), beside another month's 4,000 and another
// company's. A page reads its cases' events whatever its place, so the
// first page, a page of 500 and a later page are measured.
func TestEventLogBudget(t *testing.T) {
	nfr.Measure(t)
	e := fixture.New(t, "processmining.log.read")
	flows(t, e, e.DE, 1, 4000, "2026-03-01", 28, 10_000_000)
	flows(t, e, e.DE, 4001, 4000, "2026-04-01", 28, 20_000_000)
	flows(t, e, e.US, 8001, 4000, "2026-03-01", 28, 30_000_000)
	q := "legal_entity_id=" + e.DE.String() + "&from=2026-03-01&to=2026-03-31"
	first := read(t, e.Client, q+"&page_size=500")
	later := q + "&page_size=500&page_token=" + url.QueryEscape(first["next_page_token"].(string))
	for _, c := range []struct {
		name, query string
		cases       int
	}{
		{"the first page", q, 50},
		{"a page of 500", q + "&page_size=500", 500},
		{"the second page of 500", later, 500},
	} {
		t.Run(c.name, func(t *testing.T) {
			var times []time.Duration
			for range 20 {
				start := time.Now()
				got := read(t, e.In(t), c.query)
				times = append(times, time.Since(start))
				cases := apitest.List(t, got, "cases")
				if len(cases) != c.cases || got["case_count"] != float64(4000) {
					t.Fatalf("got %d cases of %v, want %d of 4000", len(cases), got["case_count"], c.cases)
				}
				if n := len(apitest.List(t, cases[0].(map[string]any), "events")); n != 8 {
					t.Fatalf("got %d events in a case, want 8", n)
				}
			}
			slices.Sort(times)
			p95 := times[len(times)*95/100]
			t.Logf("a month's log of 4,000 orders and 36,000 audit events: median %v, p95 %v", times[len(times)/2], p95)
			if budget := nfr.Budget(3 * time.Second); p95 > budget {
				t.Fatalf("p95 %v exceeds the %v budget (3 s scaled by ERP_BUDGET_SCALE)", p95, budget)
			}
		})
	}
}

// TestProcessMapBudget holds a read of the purchase-to-pay map to the
// budget of a report read (p95 3 s; T103b) at the volume of
// TestEventLogBudget: a month's map of one company sums every one of its
// 4,000 orders and their 32,000 activities, beside another month's 4,000
// and another company's.
func TestProcessMapBudget(t *testing.T) {
	nfr.Measure(t)
	e := fixture.New(t, "processmining.log.read")
	flows(t, e, e.DE, 1, 4000, "2026-03-01", 28, 10_000_000)
	flows(t, e, e.DE, 4001, 4000, "2026-04-01", 28, 20_000_000)
	flows(t, e, e.US, 8001, 4000, "2026-03-01", 28, 30_000_000)
	var times []time.Duration
	for range 20 {
		start := time.Now()
		got := readMap(t, e.In(t), "legal_entity_id="+e.DE.String()+"&from=2026-03-01&to=2026-03-31")
		times = append(times, time.Since(start))
		if got["case_count"] != float64(4000) || got["variant_count"] != float64(1) {
			t.Fatalf("got %v cases in %v variants, want 4000 in 1", got["case_count"], got["variant_count"])
		}
	}
	slices.Sort(times)
	p95 := times[len(times)*95/100]
	t.Logf("a month's map of 4,000 orders and 36,000 audit events: median %v, p95 %v", times[len(times)/2], p95)
	if budget := nfr.Budget(3 * time.Second); p95 > budget {
		t.Fatalf("p95 %v exceeds the %v budget (3 s scaled by ERP_BUDGET_SCALE)", p95, budget)
	}
}
