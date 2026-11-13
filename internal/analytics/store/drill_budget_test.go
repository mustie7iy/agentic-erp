package store_test

import (
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/nfr"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

// TestDrillThroughBudget measures POST /v1/analytics-documents
// (T94b, ADR-0044 As built T94b) against the week 3 budget of a report
// read, p95 3 s: the documents behind spend of one supplier over the year,
// and behind spend of one month a page of 50 at a time, the first page and
// the next, each with its supplier invoices' numbers read from their own
// reads, over the spend cube's volume (TestSpendCubeBudget): a year of
// 100,000 items of 50,000 posted supplier invoices of 500 suppliers of one
// company, answered from the store.
func TestDrillThroughBudget(t *testing.T) {
	nfr.Measure(t)
	p := parity{Env: fixture.NewWith(t, nil, nil, parityPerms...)}
	p.SpendVolume(t, p.DE, 500, 50000)
	p.extract(t)
	api := apitest.NewWith(t, func(c *app.Config) { c.Analytics = direct{p.r} })
	c := apitest.Client{T: t, API: api, Tenant: p.Tenant, As: p.Tenant.Human}
	supplier := `{"query": {"measures": ["spend"], "by": ["vendor"], "companies": ["2000"]}, "row": {"vendor": "BP50000001"}}`
	month := `{"query": {"measures": ["spend"], "by": ["fiscal_year", "fiscal_month"], "companies": ["2000"]}, "row": {"fiscal_year": "2026", "fiscal_month": "6"}}`
	first, _ := c.Must(200, "POST", "/v1/analytics-documents", month, "")
	next := "/v1/analytics-documents?page_token=" + url.QueryEscape(first["next_page_token"].(string))
	var times []time.Duration
	for i := range 21 {
		path, body := "/v1/analytics-documents", supplier
		switch i % 3 {
		case 1:
			body = month
		case 2:
			path, body = next, month
		}
		start := time.Now()
		got, _ := c.Must(200, "POST", path, body, "")
		times = append(times, time.Since(start))
		if docs := got["documents"].([]any); got["source"] != "store" || len(docs) == 0 || docs[0].(map[string]any)["number"] == "" {
			t.Fatalf("drill %d: got %d documents from %v, want numbered documents from the store", i, len(docs), got["source"])
		}
	}
	slices.Sort(times)
	p95 := times[len(times)*95/100]
	t.Logf("drill-through of spend over 100,000 items of 50,000 invoices of 500 suppliers, by supplier and a month's pages: median %v, p95 %v",
		times[len(times)/2], p95)
	if budget := nfr.Budget(3 * time.Second); p95 > budget {
		t.Fatalf("drill-through p95 %v exceeds the %v budget (3 s scaled by ERP_BUDGET_SCALE)", p95, budget)
	}
}
