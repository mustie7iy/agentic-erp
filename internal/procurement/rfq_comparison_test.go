package procurement_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/nfr"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// The comparison of an RFQ's quotations (T82c, ADR-0069 §7). The tests'
// clock is 15 June 2026 (setupIn2026), the day a quotation's validity is
// judged on.

// quotationUntil is a supplier's quotation valid until the day.
func quotationUntil(supplier uuid.UUID, reference, until string, prices ...string) string {
	return fmt.Sprintf(`{"supplier_id": %q, "supplier_reference": %q, "valid_until": %q, "items": %s}`, supplier, reference, until, items(prices...))
}

// comparedRows are a comparison's rows, each as "number rank reference
// status until total answered/complete/expired unquoted | prices", a
// price as "item:unit price/price unit=amount" with a * when it is marked
// the lowest; what a supplier without a quotation lacks reads <nil>.
func comparedRows(t *testing.T, c map[string]any) []string {
	t.Helper()
	var out []string
	for _, s := range apitest.List(t, c, "suppliers") {
		m := s.(map[string]any)
		total := "<nil>"
		if m["total"] != nil {
			total = amountOf(t, m, "total")
		}
		var prices []string
		for _, p := range apitest.List(t, m, "prices") {
			pm := p.(map[string]any)
			mark := ""
			if pm["lowest"] == true {
				mark = "*"
			}
			prices = append(prices, fmt.Sprintf("%v:%s/%v=%s%s", pm["rfq_item_no"], amountOf(t, pm, "unit_price"), pm["price_unit"], amountOf(t, pm, "amount"), mark))
		}
		out = append(out, fmt.Sprintf("%v rank %v %v %v %v %s %v/%v/%v %v | %s", m["supplier_number"], m["rank"], m["supplier_reference"], m["quotation_status"],
			m["valid_until"], total, m["answered"], m["complete"], m["expired"], m["unquoted_item_nos"], strings.Join(prices, " ")))
	}
	return out
}

// TestRFQComparison: seven suppliers are invited to quote 40 chairs and
// 12.5 desks. Two complete quotations tie at 5,000.00 and share rank 1 in
// the order invited, a third and a fourth follow; then the quotations not
// ranked, one expired the day before and one that leaves the desks out;
// last the supplier that did not answer. The lowest amount of each item
// is marked wherever it is quoted, never in the expired quotation, though
// its desks are at that amount.
func TestRFQComparison(t *testing.T) {
	e := setupIn2026(t)
	e.nameSuppliers(t)
	c3, d4, e5, f6, g7 := e.NewSupplier(t, "BP90000003"), e.NewSupplier(t, "BP90000004"), e.NewSupplier(t, "BP90000005"), e.NewSupplier(t, "BP90000006"),
		e.NewSupplier(t, "BP90000007")
	r, _ := e.Must(201, "POST", "/v1/rfqs", e.rfqBody("2026-07-31", e.chairsAndDesks(), f6, e5, d4, g7, c3, e.Vendor, e.Supplier), "")
	path := "/v1/rfqs/" + id(r)
	t.Run("a draft RFQ has no quotations", func(t *testing.T) {
		got, _ := e.in(t).Must(200, "GET", path+"/comparison", "", "")
		for field, want := range map[string]any{"status": "draft", "invited_count": float64(7), "answered_count": float64(0), "ranked_count": float64(0)} {
			if got[field] != want {
				t.Errorf("%s: got %v, want %v", field, got[field], want)
			}
		}
		rows := comparedRows(t, got)
		if len(rows) != 7 {
			t.Fatalf("rows: got %d, want 7: %v", len(rows), rows)
		}
		if want := "BP90000006 rank <nil> <nil> <nil> <nil> <nil> false/false/false [1 2] | "; rows[0] != want {
			t.Errorf("first row: got %q, want %q", rows[0], want)
		}
		if low := apitest.List(t, got, "items")[0].(map[string]any)["lowest_amount"]; low != nil {
			t.Errorf("item 1 lowest_amount: got %v, want none", low)
		}
	})
	e.act(t, path, "publish", "")
	for _, body := range []string{
		quotationUntil(e.Supplier, "A", "2026-12-31", price(1, "100.00", "EUR", ""), price(2, "80.00", "EUR", "")),            // 4,000.00 + 1,000.00
		quotationUntil(c3, "C", "2026-12-31", price(1, "110.00", "EUR", ""), price(2, "96.00", "EUR", "")),                    // 4,400.00 + 1,200.00
		quotationUntil(e.Vendor, "B", "2026-12-31", price(1, "75.00", "EUR", ""), price(2, "160.00", "EUR", "")),              // 3,000.00 + 2,000.00
		quotationUntil(d4, "D", "2026-12-31", price(1, "70.00", "EUR", "")),                                                   // 2,800.00, no desks
		quotationUntil(e5, "E", "2026-06-14", price(1, "1.00", "EUR", ""), price(2, "80.00", "EUR", "")),                      // expired yesterday; its desks at the lowest amount, 1,000.00
		quotationUntil(g7, "G", "2026-06-15", price(1, "200.00", "EUR", ""), price(2, "800.00", "EUR", `, "price_unit": 10`)), // 8,000.00 + 1,000.00, valid today
	} {
		e.Must(201, "POST", path+"/quotations", body, "")
	}
	got, _ := e.Must(200, "GET", path+"/comparison", "", "")
	t.Run("the RFQ compared", func(t *testing.T) {
		for field, want := range map[string]any{"rfq_id": id(r), "rfq_document_number": "RQ100000", "legal_entity_id": e.DE.String(), "company_code": "2000",
			"currency": "EUR", "status": "published", "due_date": "2026-07-31", "as_of": "2026-06-15", "invited_count": float64(7),
			"answered_count": float64(6), "ranked_count": float64(4)} {
			if got[field] != want {
				t.Errorf("%s: got %v, want %v", field, got[field], want)
			}
		}
		apitest.WantNamed(t, "company", got, "legal_entity_id", "company_name", `SELECT name FROM tenancy.legal_entity WHERE id = $1`)
	})
	t.Run("the rows", func(t *testing.T) {
		want := []string{
			"BP90000002 rank 1 B recorded 2026-12-31 5000.00 EUR true/true/false [] | 1:75.00 EUR/1=3000.00 EUR 2:160.00 EUR/1=2000.00 EUR",
			"BP90000001 rank 1 A recorded 2026-12-31 5000.00 EUR true/true/false [] | 1:100.00 EUR/1=4000.00 EUR 2:80.00 EUR/1=1000.00 EUR*",
			"BP90000003 rank 3 C recorded 2026-12-31 5600.00 EUR true/true/false [] | 1:110.00 EUR/1=4400.00 EUR 2:96.00 EUR/1=1200.00 EUR",
			"BP90000007 rank 4 G recorded 2026-06-15 9000.00 EUR true/true/false [] | 1:200.00 EUR/1=8000.00 EUR 2:800.00 EUR/10=1000.00 EUR*",
			"BP90000005 rank <nil> E recorded 2026-06-14 1040.00 EUR true/true/true [] | 1:1.00 EUR/1=40.00 EUR 2:80.00 EUR/1=1000.00 EUR",
			"BP90000004 rank <nil> D recorded 2026-12-31 2800.00 EUR true/false/false [2] | 1:70.00 EUR/1=2800.00 EUR*",
			"BP90000006 rank <nil> <nil> <nil> <nil> <nil> false/false/false [1 2] | ",
		}
		rows := comparedRows(t, got)
		if len(rows) != len(want) {
			t.Fatalf("rows: got %d, want %d: %v", len(rows), len(want), rows)
		}
		for i := range want {
			if rows[i] != want[i] {
				t.Errorf("row %d: got %q, want %q", i+1, rows[i], want[i])
			}
		}
	})
	t.Run("the suppliers are named", func(t *testing.T) {
		rows := apitest.List(t, got, "suppliers")
		for i, want := range []string{cabot, tyrell} {
			if name := rows[i].(map[string]any)["supplier_name"]; name != want {
				t.Errorf("row %d supplier_name: got %v, want %s", i+1, name, want)
			}
		}
		if q := rows[1].(map[string]any)["supplier_quotation_id"]; q == nil {
			t.Error("row 2 supplier_quotation_id: got none, want the quotation's id")
		}
	})
	t.Run("the lowest amount of each item", func(t *testing.T) {
		var lines []string
		for _, it := range apitest.List(t, got, "items") {
			m := it.(map[string]any)
			q := apitest.Object(t, m, "quantity")
			lines = append(lines, fmt.Sprintf("%v %v %v %v %v: %s", m["rfq_item_no"], m["material_key"], m["description"], q["value"], q["unit"], amountOf(t, m, "lowest_amount")))
		}
		want := "1 CHAIR Item CHAIR 40 EA: 2800.00 EUR; 2 DESK Item DESK 12.5 EA: 1000.00 EUR"
		if got := strings.Join(lines, "; "); got != want {
			t.Errorf("items: got %s, want %s", got, want)
		}
	})
	t.Run("the same on every read", func(t *testing.T) {
		again, _ := e.in(t).Must(200, "GET", path+"/comparison", "", "")
		if got, want := strings.Join(comparedRows(t, again), "\n"), strings.Join(comparedRows(t, got), "\n"); got != want {
			t.Errorf("second read: got %s, want %s", got, want)
		}
	})
	t.Run("a caller of another company", func(t *testing.T) {
		e.As(e.Reader(t, "Reader of 1000", e.US)).In(t).Problem(404, "request.not_found", "GET", path+"/comparison", "", "")
	})
	t.Run("no RFQ with the id", func(t *testing.T) {
		e.in(t).Problem(404, "request.not_found", "GET", "/v1/rfqs/"+uuid.NewString()+"/comparison", "", "")
	})
	t.Run("without the permission", func(t *testing.T) {
		pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'procurement.rfq.read'`, e.Tenant.ID)
		e.in(t).Problem(403, "identity.permission_missing", "GET", path+"/comparison", "", "")
	})
}

// TestRFQComparisonTimeZone: a quotation's validity is judged on the
// company's date in its time zone, not UTC's. At 23:00 on 30 June in
// Toronto (03:00 UTC on 1 July) a quotation valid until 30 June is still
// ranked; at 01:00 on 1 July in Dubai (21:00 UTC on 30 June) it is
// expired and unranked.
func TestRFQComparisonTimeZone(t *testing.T) {
	for _, c := range []struct {
		zone, asOf string
		at         time.Time
		expired    bool
		ranked     float64
	}{
		{"America/Toronto", "2026-06-30", time.Date(2026, 7, 1, 3, 0, 0, 0, time.UTC), false, 1},
		{"Asia/Dubai", "2026-07-01", time.Date(2026, 6, 30, 21, 0, 0, 0, time.UTC), true, 0},
	} {
		t.Run(c.zone, func(t *testing.T) {
			e := setupWith(t, func(doc *config.Config) { doc.Company("2000").TimeZone = c.zone })
			e.API = apitest.NewWith(t, func(cfg *app.Config) { cfg.Now = func() time.Time { return c.at } })
			path := "/v1/rfqs/" + e.publishedRFQ(t, "2026-07-31")
			e.Must(201, "POST", path+"/quotations", quotationUntil(e.Supplier, "Q", "2026-06-30", price(1, "1.00", "EUR", ""), price(2, "1.00", "EUR", "")), "")
			got, _ := e.Must(200, "GET", path+"/comparison", "", "")
			if got["as_of"] != c.asOf {
				t.Errorf("as_of: got %v, want %s", got["as_of"], c.asOf)
			}
			if got["ranked_count"] != c.ranked {
				t.Errorf("ranked_count: got %v, want %v", got["ranked_count"], c.ranked)
			}
			row := apitest.List(t, got, "suppliers")[0].(map[string]any)
			if row["supplier_reference"] != "Q" {
				t.Fatalf("first row: got %v, want quotation Q", row)
			}
			if row["expired"] != c.expired {
				t.Errorf("expired: got %v, want %v", row["expired"], c.expired)
			}
			if ranked := row["rank"] != nil; ranked == c.expired {
				t.Errorf("rank: got %v, want ranked %v", row["rank"], !c.expired)
			}
		})
	}
}

// TestRFQComparisonOutOfRange: a quotation whose amounts are out of an
// amount's range, which recording refuses and only a row written past the
// API can hold, fails the comparison rather than being ranked at a wrong
// total: an item's amount, and the sum of two amounts each in range.
func TestRFQComparisonOutOfRange(t *testing.T) {
	for name, prices := range map[string][]int64{
		"an item's amount": {9_000_000_000_000_000_00},
		"the total":        {200_000_000_000_000_000, 700_000_000_000_000_000},
	} {
		t.Run(name, func(t *testing.T) {
			e := setupIn2026(t)
			rfq := e.publishedRFQ(t, "2026-07-31")
			quotation := uuid.Must(uuid.NewV7())
			pgtest.Exec(t, `INSERT INTO procurement.supplier_quotation (tenant_id, id, rfq_id, supplier_id, supplier_reference, valid_until, created_by, updated_by)
				VALUES ($1, $2, $3, $4, 'Q', '2026-12-31', $5, $5)`, e.Tenant.ID, quotation, rfq, e.Supplier, e.Tenant.Human.ID)
			for i, p := range prices {
				pgtest.Exec(t, `INSERT INTO procurement.supplier_quotation_item (tenant_id, supplier_quotation_id, rfq_id, rfq_item_no, unit_price)
					VALUES ($1, $2, $3, $4, $5)`, e.Tenant.ID, quotation, rfq, i+1, p)
			}
			e.Problem(500, "platform.internal", "GET", "/v1/rfqs/"+rfq+"/comparison", "", "")
		})
	}
}

// TestRFQComparisonDatabaseFailures calls the comparison outside a unit of
// work and with each of its database statements failing in turn.
func TestRFQComparisonDatabaseFailures(t *testing.T) {
	e := setupIn2026(t)
	h := endpoints()
	h.Now = func() time.Time { return time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) }
	rfq := uuid.MustParse(e.publishedRFQ(t, "2026-07-31"))
	e.Must(201, "POST", "/v1/rfqs/"+rfq.String()+"/quotations", quotationBody(e.Vendor, "ANG-9", price(1, "400.00", "EUR", "")), "")
	call := func(ctx context.Context) error {
		_, err := h.GetRfqComparison(ctx, gen.GetRfqComparisonParams{ID: rfq})
		return err
	}
	if err := call(context.Background()); err == nil {
		t.Error("outside a unit of work: got nil error, want an error")
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "compare quotations", call)
}

// TestRFQComparisonBudget holds the comparison to the budget of a list
// page (01-requirements: p95 150 ms), being the list of an RFQ's suppliers
// with their quotations: an RFQ of 50 items with 20 suppliers invited,
// each quoting every item, so 1,000 prices are read, valued and ranked on
// each read. No requirement names an RFQ's size; this is far above an RFQ
// of the first version, whose quotations a buyer types in.
func TestRFQComparisonBudget(t *testing.T) {
	nfr.Measure(t)
	e := setupIn2026(t)
	const nItems, nSuppliers = 50, 20
	its, suppliers := make([]string, nItems), make([]uuid.UUID, nSuppliers)
	for i := range its {
		its[i] = rfqItem(fmt.Sprintf("M-%03d", i+1), "12.5", "EA", e.office, "")
	}
	for i := range suppliers {
		suppliers[i] = e.NewSupplier(t, fmt.Sprintf("BP91%06d", i+1))
	}
	r, _ := e.Must(201, "POST", "/v1/rfqs", e.rfqBody("2026-07-31", items(its...), suppliers...), "")
	path := "/v1/rfqs/" + id(r)
	e.act(t, path, "publish", "")
	for s, supplier := range suppliers {
		prices := make([]string, nItems)
		for i := range prices {
			prices[i] = price(i+1, fmt.Sprintf("%d.%02d", 100+(s*7+i*3)%40, (s+i)%100), "EUR", "")
		}
		e.Must(201, "POST", path+"/quotations", quotationBody(supplier, fmt.Sprintf("Q-%d", s+1), prices...), "")
	}
	pgtest.Exec(t, `ANALYZE procurement.rfq, procurement.rfq_item, procurement.rfq_supplier, procurement.supplier_quotation, procurement.supplier_quotation_item`)
	var times []time.Duration
	for range 20 {
		start := time.Now()
		got, _ := e.Must(200, "GET", path+"/comparison", "", "")
		times = append(times, time.Since(start))
		if got["ranked_count"] != float64(nSuppliers) {
			t.Fatalf("ranked_count: got %v, want %d", got["ranked_count"], nSuppliers)
		}
	}
	slices.Sort(times)
	p95 := times[len(times)*95/100]
	t.Logf("comparison of %d quotations of %d items: median %v, p95 %v", nSuppliers, nItems, times[len(times)/2], p95)
	if budget := nfr.Budget(150 * time.Millisecond); p95 > budget {
		t.Fatalf("comparison p95 %v exceeds the %v budget (150 ms scaled by ERP_BUDGET_SCALE)", p95, budget)
	}
}
