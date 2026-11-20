package sales_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// jurisdictions are the items' tax jurisdictions, "-" for none, in item
// order.
func jurisdictions(t *testing.T, doc map[string]any) string {
	t.Helper()
	var out []string
	for _, it := range items(t, doc) {
		j, ok := it["tax_jurisdiction"].(string)
		if !ok {
			j = "-"
		}
		out = append(out, j)
	}
	return strings.Join(out, " ")
}

// lineJurisdictions are the journal entry's lines naming a jurisdiction:
// "tax code jurisdiction amount", by line number.
func (e env) lineJurisdictions(t *testing.T, entry any) string {
	t.Helper()
	var out string
	pgtest.Scan(t, `SELECT coalesce(string_agg(format('%s %s %s', l.tax_code, l.tax_jurisdiction, l.amount_tc), '; ' ORDER BY l.line_no), '')
		FROM ledger.journal_line l WHERE l.tenant_id = $1 AND l.journal_entry_id = $2 AND l.tax_jurisdiction IS NOT NULL`, []any{e.Tenant.ID, entry}, &out)
	return out
}

// TestSalesItemJurisdiction (ADR-0067 As built, the emirate on a sales
// item): an item of a code levying its lines' subdivision names its
// jurisdiction, which a quotation's order keeps, its billing document
// shows and the customer invoice it posts carries to the item and to the
// entry's revenue and tax lines; an item of another code names none.
func TestSalesItemJurisdiction(t *testing.T) {
	e := setup(t)
	levied := e.item(line{price: "100.00", code: "AJ", jurisdiction: "DE-BY"})
	plain := e.item(line{price: "50.00", code: "A1"})
	quoted, _ := e.Must(201, "POST", "/v1/quotations", e.body(e.customer, far, levied, plain), "")
	if j := jurisdictions(t, quoted); j != "DE-BY -" {
		t.Errorf("the quotation's items: got %s, want DE-BY -", j)
	}
	t.Run("an order from the quotation keeps it", func(t *testing.T) {
		id := e.accepted(t, e.customer, levied)
		order, _ := e.In(t).Must(201, "POST", "/v1/sales-orders", fromQuotation(id), "")
		if j := jurisdictions(t, order); j != "DE-BY" {
			t.Errorf("the order's items: got %s, want DE-BY", j)
		}
	})
	_, doc, _ := e.billedOrder(t, []string{levied, plain}, "1 1 EA", "2 1 EA")
	billing, _ := e.Must(200, "GET", billings+"/"+doc, "", "")
	if j := jurisdictions(t, billing); j != "DE-BY -" {
		t.Errorf("the billing document's items: got %s, want DE-BY -", j)
	}
	posted, _ := e.Must(200, "POST", posting(doc), "", `"1"`)
	invoice, _ := e.Must(200, "GET", "/v1/customer-invoices/"+posted["customer_invoice_id"].(string), "", "")
	if j := jurisdictions(t, invoice); j != "DE-BY -" {
		t.Errorf("the customer invoice's items: got %s, want DE-BY -", j)
	}
	if l := e.lineJurisdictions(t, invoice["journal_entry_id"]); l != "AJ DE-BY -10000; AJ DE-BY -1900" {
		t.Errorf("the entry's lines in DE-BY: got %s, want the revenue line and its tax, AJ DE-BY -10000; AJ DE-BY -1900", l)
	}
	t.Run("a credit memo request's credit memo names it", func(t *testing.T) {
		approved := e.approvedRequest(t, byAmount(doc, "Goodwill", "1 10.00", "2 5.00"))
		if j := jurisdictions(t, approved); j != "DE-BY -" {
			t.Errorf("the request's items: got %s, want DE-BY -", j)
		}
		credited, _ := e.In(t).Must(200, "POST", creditPosting(approved["id"].(string)), "", `"2"`)
		if j := jurisdictions(t, e.creditMemoOf(t, credited)); j != "DE-BY -" {
			t.Errorf("the credit memo's items: got %s, want DE-BY -", j)
		}
	})
	t.Run("a cancellation's credit memo names it", func(t *testing.T) {
		_, other, _ := e.postedOrder(t, []string{levied}, "1 1 EA")
		cancelled, _ := e.In(t).Must(200, "POST", cancelBilling(other), `{"reason": "Billed twice"}`, `"2"`)
		c, _ := cancelled["cancellation"].(map[string]any)
		memo, _ := e.In(t).Must(200, "GET", "/v1/customer-invoices/"+c["customer_invoice_id"].(string), "", "")
		if j := jurisdictions(t, memo); j != "DE-BY" {
			t.Errorf("the cancellation's credit memo's items: got %s, want DE-BY", j)
		}
	})
}

// TestOrderChangeKeepsJurisdiction: an item an order change keeps keeps
// its jurisdiction; an item it adds is held to its code as a new order's
// is, the refusal naming new_items[i].
func TestOrderChangeKeepsJurisdiction(t *testing.T) {
	e := setup(t)
	id, _ := e.confirmed(t, e.item(line{code: "AJ", jurisdiction: "DE-BY"}))
	changed, _ := e.Must(200, "POST", changing(id), changeBody("", itemChange("1 2 EA", "")), e.version(t, id))
	if j := jurisdictions(t, changed); j != "DE-BY" {
		t.Errorf("the changed item: got %s, want DE-BY", j)
	}
	p := e.Problem(422, "sales.tax_jurisdiction_required", "POST", changing(id), fmt.Sprintf(`{"new_items": [%s]}`, e.item(line{code: "AJ"})), e.version(t, id))
	if f := field(p); f != "new_items[0].tax_jurisdiction" {
		t.Errorf("the field: got %v, want new_items[0].tax_jurisdiction", f)
	}
	added, _ := e.Must(200, "POST", changing(id), fmt.Sprintf(`{"new_items": [%s]}`, e.item(line{code: "AJ", jurisdiction: "DE-BE"})), e.version(t, id))
	if j := jurisdictions(t, added); j != "DE-BY DE-BE" {
		t.Errorf("the order's items: got %s, want DE-BY DE-BE", j)
	}
}

// TestCaptureOrderJurisdiction: a captured order's items take the
// request's tax jurisdiction with its tax code.
func TestCaptureOrderJurisdiction(t *testing.T) {
	e := capturing(t, reader{ext: purchaseOrder()})
	upload := e.uploaded(t, "an order", "text/plain")
	op, _ := e.Must(202, "POST", "/v1/sales-orders:capture", e.captureBody(upload, `, "tax_code": "AJ", "tax_jurisdiction": "DE-BY"`), "")
	order, _ := e.Must(200, "GET", op["resource"].(string), "", "")
	if j := jurisdictions(t, order); j != "DE-BY DE-BY" {
		t.Errorf("the captured order's items: got %s, want DE-BY DE-BY", j)
	}
}

// TestBillingJurisdictionRechecked: an item's jurisdiction is held to its
// code again on the billing date, the configuration having changed since
// the order's: a code that no longer lists the item's subdivision, or that
// levies the subdivision of items that name none, refuses the billing,
// naming the order and its item; nothing is billed.
func TestBillingJurisdictionRechecked(t *testing.T) {
	e := setup(t)
	listed, _ := e.deliveredOrder(t, []string{e.item(line{code: "AL", jurisdiction: "DE-BE"})}, "1 1 EA")
	unnamed, _ := e.deliveredOrder(t, []string{e.item(line{}), e.item(line{code: "A1"})}, "1 1 EA", "2 1 EA")
	doc := fixture.Document(t)
	outputCodes(doc)
	codes := doc.Company("2000").TaxCodes
	code := func(name string) config.TaxCode {
		return codes[slices.IndexFunc(codes, func(c config.TaxCode) bool { return c.Code == name })]
	}
	al, a1 := code("AL"), code("A1")
	al.Components[0].Subdivisions = []string{"DE-BY"}
	a1.Components[0].Jurisdiction, a1.Components[0].JurisdictionOnLine = "DE", true
	fixture.TaxCodes(t, e.Tenant, doc, al, a1)
	cases := []struct{ name, order, detail string }{
		{"a subdivision no longer listed", listed, "Item 1 of sales order SO"},
		{"a code levying its lines' subdivision since", unnamed, "Item 2 of sales order SO"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := e.In(t).Problem(422, "sales.order_item_not_billable", "POST", billings, bill(c.order, ""), "")
			if f := field(p); f != "sales_order_id" {
				t.Errorf("field: got %v, want sales_order_id", f)
			}
			if d, _ := p["detail"].(string); !strings.Contains(d, c.detail) {
				t.Errorf("detail: got %q, want it to contain %q", d, c.detail)
			}
		})
	}
	var n int
	pgtest.Scan(t, `SELECT count(*) FROM sales.billing_document WHERE tenant_id = $1`, []any{e.Tenant.ID}, &n)
	if n != 0 {
		t.Errorf("billing documents: got %d, want none", n)
	}
}
