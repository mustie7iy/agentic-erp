package payables_test

import (
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

// TestGRIRExitCriterion walks T33's exit criterion through the API: a
// goods receipt posts Dr expense / Cr GR/IR at the order price; its
// reversal (102) reverses that; the invoice posts Dr GR/IR for its
// quantity with the price difference on its own line; the open GR/IR
// balance per order item is listed and is zero once received and invoiced
// quantities agree.
func TestGRIRExitCriterion(t *testing.T) {
	f := fixture.New(t, append(perms, "procurement.order.read", "procurement.order.create", "procurement.order.release", "procurement.receipt.post",
		"procurement.receipt.read")...)
	e := env{Env: f, expense: f.Account(t, "2000", "520000"), tax: f.Account(t, "2000", "140000"), ap: f.Account(t, "2000", "211000")}
	e.GRIR(t)
	grir, diff := e.Account(t, "2000", "219000").String(), e.Account(t, "2000", "590000").String()
	o, _ := e.Must(201, "POST", "/v1/purchase-orders", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "EUR", "order_date": "2026-03-01",
		"items": [{"description": "Paper", "quantity": {"value": "10", "unit": "EA"}, "unit_price": {"amount": "12.00", "currency": "EUR"},
			"account_id": %q, "tax_code": "V1"}]}`, e.DE, e.Supplier, e.expense), "")
	_, tag := e.Must(200, "GET", "/v1/purchase-orders/"+id(o), "", "")
	released, _ := e.Must(200, "POST", "/v1/purchase-orders/"+id(o)+":release", "", tag)
	if released["valuation"] != "gr_ir" {
		t.Fatalf("order valuation: got %v, want gr_ir", released["valuation"])
	}
	po := id(o)
	receive := func(qty string) map[string]any {
		gr, _ := e.Must(201, "POST", "/v1/goods-receipts", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-05", "posting_date": "2026-03-05",
			"items": [{"purchase_order_id": %q, "po_item_no": 1, "quantity": {"value": %q, "unit": "EA"}}]}`, e.DE, po, qty), "")
		return gr
	}
	balances := func(query string) []any {
		page, _ := e.Must(200, "GET", "/v1/gr-ir-balances?legal_entity_id="+e.DE.String()+"&purchase_order_id="+po+query, "", "")
		return page["items"].([]any)
	}
	check := func(step string, entry any, want ...line) {
		t.Helper()
		if got := e.entryLines(t, entry); fmt.Sprint(sorted(got)) != fmt.Sprint(sorted(want)) {
			t.Errorf("%s: entry lines got %v, want %v", step, got, want)
		}
	}
	first := receive("6")
	check("receipt", first["journal_entry_id"], line{e.expense.String(), "72.00", nil}, line{grir, "-72.00", po})
	_, tag = e.Must(200, "GET", "/v1/goods-receipts/"+id(first), "", "")
	reversal, _ := e.Must(200, "POST", "/v1/goods-receipts/"+id(first)+":reverse", "", tag)
	check("reversal", reversal["journal_entry_id"], line{e.expense.String(), "-72.00", nil}, line{grir, "72.00", po})
	if open := balances(""); len(open) != 0 {
		t.Errorf("after the reversal: got open positions %v, want none", open)
	}
	receive("10")
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-1", "59.50", "9.50", poItemTaxed(po, "4", "50.00")), "")
	e.act(t, 200, id(inv), "match", "")
	posted := e.act(t, 200, id(inv), "post", `{"posting_date": "2026-03-15"}`)
	check("invoice", posted["journal_entry_id"], line{grir, "48.00", po}, line{diff, "2.00", nil}, line{e.tax.String(), "9.50", nil},
		line{e.ap.String(), "-59.50", nil})
	position := func(step, received, invoiced, balance string, query string) {
		t.Helper()
		items := balances(query)
		if len(items) != 1 {
			t.Fatalf("%s: got %d positions, want 1", step, len(items))
		}
		b := items[0].(map[string]any)
		got := fmt.Sprint(b["received_quantity"].(map[string]any)["value"], " ", b["invoiced_quantity"].(map[string]any)["value"], " ",
			b["balance"].(map[string]any)["amount"])
		if want := received + " " + invoiced + " " + balance; got != want {
			t.Errorf("%s: position got %s, want %s", step, got, want)
		}
	}
	position("partly invoiced", "10", "4", "-72.00", "")
	rest, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "INV-2", "85.68", "13.68", poItemTaxed(po, "6", "72.00")), "")
	e.act(t, 200, id(rest), "match", "")
	e.act(t, 200, id(rest), "post", `{"posting_date": "2026-03-16"}`)
	if open := balances(""); len(open) != 0 {
		t.Errorf("fully invoiced: got open positions %v, want none", open)
	}
	position("fully invoiced", "10", "10", "0.00", "&open_only=false")
}

// poItemTaxed is an invoice item of quantity EA on item 1 of the order.
func poItemTaxed(po, qty, amount string) string {
	return fmt.Sprintf(`{"purchase_order_id": %q, "po_item_no": 1, "quantity": {"value": %q, "unit": "EA"}, "amount": {"amount": %q, "currency": "EUR"}}`,
		po, qty, amount)
}
