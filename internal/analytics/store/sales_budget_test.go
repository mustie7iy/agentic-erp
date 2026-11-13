package store_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/nfr"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

// TestSalesMetricsBudget measures POST /v1/analytics-queries for the sales
// metrics (T91a, ADR-0044 As built T91a) against the week 3 budget of a
// report read, p95 3 s: order intake, billed value and the billing
// documents of one company by month, and by customer and item key for one
// month, over a year of 100,000 confirmed order items and 100,000 posted
// billing document items of 2,000 customers. 01 §1.3 names no sales
// volume; this is the volume its other reports of one company are
// measured at (100,000 open items). The answer is the analytical store's,
// which is what answers a tenant of that size: the database path reads
// the API's lists page by page and is the pilot's (ADR-0044, Context).
// The pilot's own volumes are not known yet (ADR-0052, facts).
func TestSalesMetricsBudget(t *testing.T) {
	nfr.Measure(t)
	p := parity{Env: fixture.NewWith(t, nil, nil, parityPerms...)}
	p.SalesVolume(t, p.DE, 2000, 50000)
	p.extract(t)
	api := apitest.NewWith(t, func(c *app.Config) { c.Analytics = direct{p.r} })
	c := apitest.Client{T: t, API: api, Tenant: p.Tenant, As: p.Tenant.Human}
	measures := `"measures": ["order_intake", "billed_value", "billing_document_count"], "companies": ["2000"]`
	byMonth := `{` + measures + `, "by": ["fiscal_year", "fiscal_month"], "order_by": "fiscal_month"}`
	byCustomer := `{` + measures + `, "by": ["customer", "customer_name", "item_key"], "where": [{"dim": "fiscal_year", "op": "in", "values": ["2026"]},
		{"dim": "fiscal_month", "op": "in", "values": ["6"]}], "order_by": "order_intake", "desc": true, "limit": 100}`
	var times []time.Duration
	for i := range 20 {
		q, rows := byMonth, 12
		if i%2 == 1 {
			q, rows = byCustomer, 100
		}
		start := time.Now()
		got, _ := c.Must(200, "POST", "/v1/analytics-queries", q, "")
		times = append(times, time.Since(start))
		if got["source"] != "store" || len(got["rows"].([]any)) != rows {
			t.Fatalf("query %d: got %d rows from %v, want %d from the store", i, len(got["rows"].([]any)), got["source"], rows)
		}
	}
	// Every item is in the answer: 50,000 orders of 3.00 each.
	total, _ := c.Must(200, "POST", "/v1/analytics-queries", `{`+measures+`}`, "")
	if got := fmt.Sprint(total["rows"]); got != "[[map[currency:EUR value:150000.00] map[currency:EUR value:150000.00] map[value:50000]]]" {
		t.Fatalf("totals: got %s, want 150000.00 EUR ordered and billed in 50000 billing documents", got)
	}
	slices.Sort(times)
	p95 := times[len(times)*95/100]
	t.Logf("sales metrics over 100,000 order items and 100,000 billing items of 2,000 customers, by month and by customer and item key: median %v, p95 %v",
		times[len(times)/2], p95)
	if budget := nfr.Budget(3 * time.Second); p95 > budget {
		t.Fatalf("sales metrics p95 %v exceeds the %v budget (3 s scaled by ERP_BUDGET_SCALE)", p95, budget)
	}
}
