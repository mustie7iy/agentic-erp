package procurement_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/nfr"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestCatalogSearchBudget holds one page of the catalog search to
// 01-requirements' budget for a list with filters (p95 150 ms): a
// company's catalog of 50,000 active items of two suppliers among the
// 200,000 of its tenant. No requirement names a catalog's size; items are
// loaded one at a time through the API in the first version (ADR-0071),
// so 50,000 is far above a catalog of it. The search reads the company's
// items in key order (catalog_item_by_key, 000287) and matches the text
// in each, so its worst case is a text nothing contains, which reads the
// company's whole catalog; a text many items contain stops at the page.
// By creation time no index gives the order, so a text most items contain
// reads and sorts the company's matches for each page: measured too.
func TestCatalogSearchBudget(t *testing.T) {
	nfr.Measure(t)
	e := setup(t)
	pgtest.Exec(t, `INSERT INTO procurement.catalog_item (tenant_id, id, legal_entity_id, supplier_id, item_key, description, uom_code, currency,
			unit_price, created_by, updated_by)
		SELECT $1, gen_random_uuid(), CASE WHEN n % 4 = 0 THEN $2::uuid ELSE $3::uuid END, CASE WHEN n % 8 = 0 THEN $4::uuid ELSE $5::uuid END,
			'ITEM-' || lpad(n::text, 6, '0'), CASE WHEN n % 20000 = 0 THEN 'Standing desk, oak, ' ELSE 'Office chair, model ' END || n, 'EA', 'EUR',
			100 + n, $6, $6
		FROM generate_series(1, 200000) n`, e.Tenant.ID, e.DE, e.US, e.Supplier, e.Vendor, e.Tenant.Human.ID)
	pgtest.Exec(t, `ANALYZE procurement.catalog_item`)
	for _, c := range []struct {
		name, query string
		want        int
	}{
		{"a text a few items contain", "&text=oak", 10},
		{"a text nothing contains", "&text=sofa", 0},
		{"a text every item contains, the first page", "&text=item-", 50},
		{"one supplier's items, the first page", "&supplier_id=" + e.Supplier.String(), 50},
		{"a text and a supplier", "&text=chair&supplier_id=" + e.Supplier.String() + "&page_size=100", 100},
		{"a text most items contain, by when they were loaded", "&text=chair&order_by=created_at", 50},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := e.in(t)
			var times []time.Duration
			for range 20 {
				start := time.Now()
				page, _ := e.Must(200, "GET", fmt.Sprintf("/v1/catalog-items?legal_entity_id=%s%s", e.DE, c.query), "", "")
				times = append(times, time.Since(start))
				if n := len(apitest.List(t, page, "items")); n != c.want {
					t.Fatalf("got %d items, want %d", n, c.want)
				}
			}
			slices.Sort(times)
			p95 := times[len(times)*95/100]
			t.Logf("one page of a company's 50,000 catalog items among 200,000: median %v, p95 %v", times[len(times)/2], p95)
			if budget := nfr.Budget(150 * time.Millisecond); p95 > budget {
				t.Fatalf("p95 %v exceeds the %v budget (150 ms scaled by ERP_BUDGET_SCALE)", p95, budget)
			}
		})
	}
}
