package endpoint_test

import (
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

// spendPerms are what a query of the spend metrics reads with.
var spendPerms = []string{"procurement.contract.read", "ledger.cost_center.read"}

// TestSpendMetrics: spend is the net amount of posted supplier invoices
// less posted credit memos by posting date, by supplier, account, cost
// center and month, read through the API as the caller; an item on an
// order made on a contract is under contract, an invoice without an order
// off contract; parked and reversed documents are not spend; and the
// caller's company access and the reads' permissions bound the rows (T91b
// acceptance).
func TestSpendMetrics(t *testing.T) {
	e := seeded(t, append(spendPerms, perms...)...)
	center := e.CostCenter(t, e.DE, "1000")
	contract := e.Contract(t, e.DE, e.Supplier, "EUR", "CT-0001")
	office, paper := e.Account(t, "2000", "520000"), e.Account(t, "2000", "610000")
	order := e.ContractOrder(t, e.DE, e.Supplier, &contract, "EUR", "2026-03-01", paper, &center)
	free := e.ContractOrder(t, e.DE, e.Vendor, nil, "EUR", "2026-03-01", paper, nil)
	e.SupplierDocument(t, e.DE, e.Supplier, "invoice", "posted", "EUR", "2026-03-06",
		fixture.InvoiceItem{Net: 10000, Order: &order}, fixture.InvoiceItem{Net: 4000, Account: &office, CostCenter: &center})
	e.SupplierDocument(t, e.DE, e.Supplier, "credit_memo", "posted", "EUR", "2026-04-07", fixture.InvoiceItem{Net: 1500, Account: &office})
	e.SupplierDocument(t, e.DE, e.Vendor, "invoice", "posted", "EUR", "2026-04-08", fixture.InvoiceItem{Net: 2500, Order: &free})
	e.SupplierDocument(t, e.DE, e.Vendor, "invoice", "parked", "EUR", "2026-04-09", fixture.InvoiceItem{Net: 99900, Account: &office})
	e.SupplierDocument(t, e.DE, e.Vendor, "invoice", "reversed", "EUR", "2026-04-10", fixture.InvoiceItem{Net: 88800, Account: &office})
	usOffice := e.Account(t, "1000", "520000")
	e.SupplierDocument(t, e.US, e.Supplier, "invoice", "posted", "USD", "2026-03-06", fixture.InvoiceItem{Net: 700, Account: &usOffice})
	const measures = `"measures": ["spend", "spend_under_contract", "spend_off_contract", "off_contract_share"]`

	res, _ := e.Must(200, "POST", "/v1/analytics-queries", `{`+measures+`, "by": ["fiscal_month", "vendor", "account", "cost_center", "contract"],
		"companies": ["2000"], "order_by": "fiscal_month"}`, "")
	want := "[[3 <nil> BP90000001 <nil> 520000 <nil> 1000 <nil>  <nil> 40.00 EUR <nil> <nil> 40.00 EUR 1.0000 <nil>]" +
		" [3 <nil> BP90000001 <nil> 610000 <nil> 1000 <nil> CT-0001 <nil> 100.00 EUR 100.00 EUR 0.00 EUR 0.0000 <nil>]" +
		" [4 <nil> BP90000001 <nil> 520000 <nil>  <nil>  <nil> -15.00 EUR <nil> <nil> -15.00 EUR 1.0000 <nil>]" +
		" [4 <nil> BP90000002 <nil> 610000 <nil>  <nil>  <nil> 25.00 EUR <nil> <nil> 25.00 EUR 1.0000 <nil>]]"
	if got := fmt.Sprint(rows(res)); got != want {
		t.Errorf("by month, supplier, account, cost center and contract:\n got %s\nwant %s", got, want)
	}
	if res["source"] != "database" || res["data_through"] != "2026-04-08" {
		t.Errorf("lineage: got %v through %v, want the database's through the last posted document", res["source"], res["data_through"])
	}
	res, _ = e.Must(200, "POST", "/v1/analytics-queries", `{`+measures+`, "by": ["company", "vendor_name"], "order_by": "vendor_name"}`, "")
	want = "[[1000 <nil> BP90000001 <nil> 7.00 USD <nil> <nil> 7.00 USD 1.0000 <nil>]" +
		" [2000 <nil> BP90000001 <nil> 125.00 EUR 100.00 EUR 25.00 EUR 0.2000 <nil>]" +
		" [2000 <nil> BP90000002 <nil> 25.00 EUR <nil> <nil> 25.00 EUR 1.0000 <nil>]]"
	if got := fmt.Sprint(rows(res)); got != want {
		t.Errorf("by company and supplier:\n got %s\nwant %s", got, want)
	}

	t.Run("one company's reader", func(t *testing.T) {
		reader := e.As(e.Reader(t, "Reader", e.US))
		res, _ := reader.Must(200, "POST", "/v1/analytics-queries", `{"measures": ["spend"], "by": ["company"]}`, "")
		if got := fmt.Sprint(rows(res)); got != "[[1000 <nil> 7.00 USD]]" {
			t.Errorf("spend of a reader of 1000: got %s, want 1000's alone", got)
		}
	})
	for _, lacking := range []string{"payables.invoice.read", "procurement.order.read", "procurement.contract.read", "ledger.cost_center.read"} {
		t.Run("without "+lacking, func(t *testing.T) {
			var held []string
			for _, p := range append(spendPerms, perms...) {
				if p != lacking {
					held = append(held, p)
				}
			}
			other := seeded(t, held...)
			code, body, _ := other.Do("POST", "/v1/analytics-queries", `{"measures": ["spend"], "by": ["company"]}`, "")
			if code != 403 || body["code"] != "identity.permission_missing" {
				t.Errorf("spend: got %d %v, want 403 identity.permission_missing", code, body)
			}
			// The journal's metrics need none of them.
			other.Must(200, "POST", "/v1/analytics-queries", `{"measures": ["revenue"], "by": ["company"]}`, "")
		})
	}
}
