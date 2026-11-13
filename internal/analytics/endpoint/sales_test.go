package endpoint_test

import (
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

// TestSalesMetrics: order intake is the confirmed orders' items by order
// date and billed value the posted billing documents' by billing date,
// net, by customer, item key and month, read through the API as the
// caller; a draft order and a billing document not posted are in neither;
// amounts of two currencies are not added; and the sales lines need both
// sales reads, whichever metric is asked for (T91a acceptance).
func TestSalesMetrics(t *testing.T) {
	e := seeded(t, append([]string{"sales.order.read", "sales.billing.read"}, perms...)...)
	customer := e.Customer(t, "BP80000001", "Northwind Traders")
	order := e.SalesOrder(t, e.DE, customer, "EUR", "2026-03-12", "confirmed", fixture.SalesItem{Key: "BRACKET-M8", Net: 150000}, fixture.SalesItem{Net: 23753})
	e.BillingDocument(t, order, "2026-04-01", true)
	e.SalesOrder(t, e.DE, customer, "EUR", "2026-03-13", "draft", fixture.SalesItem{Key: "DRAFT", Net: 99999})
	later := e.SalesOrder(t, e.DE, customer, "EUR", "2026-04-15", "confirmed", fixture.SalesItem{Key: "BRACKET-M8", Net: 5000})
	e.BillingDocument(t, later, "2026-04-16", false)
	const measures = `"measures": ["order_intake", "billed_value", "billing_document_count"]`

	res, _ := e.Must(200, "POST", "/v1/analytics-queries", `{`+measures+`, "by": ["fiscal_month", "customer", "customer_name", "item_key"],
		"companies": ["2000"], "order_by": "fiscal_month"}`, "")
	want := "[[3 <nil> BP80000001 <nil> Northwind Traders <nil>  <nil> 237.53 EUR <nil> <nil> <nil> <nil>]" +
		" [3 <nil> BP80000001 <nil> Northwind Traders <nil> BRACKET-M8 <nil> 1500.00 EUR <nil> <nil> <nil> <nil>]" +
		" [4 <nil> BP80000001 <nil> Northwind Traders <nil>  <nil> <nil> <nil> 237.53 EUR 1 <nil>]" +
		" [4 <nil> BP80000001 <nil> Northwind Traders <nil> BRACKET-M8 <nil> 50.00 EUR 1500.00 EUR 1 <nil>]]"
	if got := fmt.Sprint(rows(res)); got != want {
		t.Errorf("by month, customer and item key:\n got %s\nwant %s", got, want)
	}
	if res["source"] != "database" || res["data_through"] != "2026-04-15" {
		t.Errorf("lineage: got %v through %v, want the database's through the last confirmed order", res["source"], res["data_through"])
	}
	res, _ = e.Must(200, "POST", "/v1/analytics-queries", `{`+measures+`, "companies": ["2000"]}`, "")
	if got := fmt.Sprint(rows(res)); got != "[[1787.53 EUR 1737.53 EUR 1 <nil>]]" {
		t.Errorf("totals of 2000: got %s, want 1787.53 ordered and 1737.53 billed in one document", got)
	}

	t.Run("two currencies", func(t *testing.T) {
		e.SalesOrder(t, e.DE, customer, "USD", "2026-05-02", "confirmed", fixture.SalesItem{Key: "BRACKET-M8", Net: 700})
		code, body, _ := e.Do("POST", "/v1/analytics-queries", `{"measures": ["order_intake"], "companies": ["2000"]}`, "")
		if code != 422 || body["code"] != "analytics.invalid_query" {
			t.Errorf("EUR and USD orders in one sum: got %d %v, want 422 analytics.invalid_query", code, body)
		}
		res, _ := e.Must(200, "POST", "/v1/analytics-queries", `{"measures": ["order_intake"], "by": ["currency"], "companies": ["2000"], "order_by": "currency"}`, "")
		if got := fmt.Sprint(rows(res)); got != "[[EUR <nil> 1787.53 EUR] [USD <nil> 7.00 USD]]" {
			t.Errorf("by currency: got %s, want each currency's orders", got)
		}
	})
	for lacking, held := range map[string]string{"sales.order.read": "sales.billing.read", "sales.billing.read": "sales.order.read"} {
		t.Run("without "+lacking, func(t *testing.T) {
			other := seeded(t, append([]string{held}, perms...)...)
			for _, metric := range []string{"order_intake", "billed_value"} {
				code, body, _ := other.Do("POST", "/v1/analytics-queries", `{"measures": ["`+metric+`"]}`, "")
				if code != 403 || body["code"] != "identity.permission_missing" {
					t.Errorf("%s: got %d %v, want 403 identity.permission_missing", metric, code, body)
				}
			}
			// The journal's metrics need neither.
			other.Must(200, "POST", "/v1/analytics-queries", `{"measures": ["revenue"], "by": ["company"]}`, "")
		})
	}
}
