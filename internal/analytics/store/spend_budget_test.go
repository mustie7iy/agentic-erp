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

// TestSpendCubeBudget measures POST /v1/analytics-queries for the spend
// metrics (T91b, ADR-0044 As built T91b) against the week 3 budget of a
// report read, p95 3 s: spend, under and off contract, of one company by
// month, by supplier, and by account, cost center and contract, over a
// year of 100,000 items of 50,000 posted supplier invoices of 500
// suppliers, half of them on purchase orders, every other order made on a
// contract. 01 §1.3 gives a tenant's supplier invoices a year (20
// million, over 500 companies: 40,000 a company); this is the volume its
// other reports of one company are measured at (100,000 items). The
// answer is the analytical store's, which is what answers a tenant of
// that size: the database path reads the API's lists page by page and is
// the pilot's (ADR-0044, Context). The pilot's own volumes are not known
// yet (ADR-0052, facts).
func TestSpendCubeBudget(t *testing.T) {
	nfr.Measure(t)
	p := parity{Env: fixture.NewWith(t, nil, nil, parityPerms...)}
	p.SpendVolume(t, p.DE, 500, 50000)
	p.extract(t)
	api := apitest.NewWith(t, func(c *app.Config) { c.Analytics = direct{p.r} })
	c := apitest.Client{T: t, API: api, Tenant: p.Tenant, As: p.Tenant.Human}
	measures := `"measures": ["spend", "spend_under_contract", "spend_off_contract", "off_contract_share"], "companies": ["2000"]`
	queries := []struct {
		body string
		rows int
	}{
		{`{` + measures + `, "by": ["fiscal_year", "fiscal_month"], "order_by": "fiscal_month"}`, 12},
		{`{` + measures + `, "by": ["vendor", "vendor_name"], "order_by": "spend", "desc": true, "limit": 100}`, 100},
		// Eighty groups: the orders' items on account 610000, fifty of a
		// contract and cost center and ten of a cost center alone, and the
		// others on 520000 in each of the twenty cost centers.
		{`{` + measures + `, "by": ["account", "account_name", "cost_center", "contract"], "where": [{"dim": "fiscal_month", "op": "in", "values": ["6"]}],
			"order_by": "spend", "desc": true}`, 80},
	}
	var times []time.Duration
	for i := range 21 {
		q := queries[i%len(queries)]
		start := time.Now()
		got, _ := c.Must(200, "POST", "/v1/analytics-queries", q.body, "")
		times = append(times, time.Since(start))
		if got["source"] != "store" || len(got["rows"].([]any)) != q.rows {
			t.Fatalf("query %d: got %d rows from %v, want %d from the store", i, len(got["rows"].([]any)), got["source"], q.rows)
		}
	}
	// Every item is in the answer: 50,000 invoices of 3.00 each, the 1.00
	// of every other order's invoices under contract.
	total, _ := c.Must(200, "POST", "/v1/analytics-queries", `{`+measures+`}`, "")
	if got := fmt.Sprint(total["rows"]); got != "[[map[currency:EUR value:150000.00] map[currency:EUR value:25000.00] map[currency:EUR value:125000.00] map[value:0.8333]]]" {
		t.Fatalf("totals: got %s, want 150000.00 EUR spent, 25000.00 under contract, 125000.00 off, a share of 0.8333", got)
	}
	slices.Sort(times)
	p95 := times[len(times)*95/100]
	t.Logf("spend cube over 100,000 items of 50,000 posted invoices of 500 suppliers, by month, by supplier and by account, cost center and contract: median %v, p95 %v",
		times[len(times)/2], p95)
	if budget := nfr.Budget(3 * time.Second); p95 > budget {
		t.Fatalf("spend cube p95 %v exceeds the %v budget (3 s scaled by ERP_BUDGET_SCALE)", p95, budget)
	}
}
