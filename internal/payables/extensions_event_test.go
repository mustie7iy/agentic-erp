package payables_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
)

// TestEventsMaskCustomFields: SupplierInvoiceCaptured and
// SupplierInvoicePosted carry the invoice's custom fields and their schema
// version, the personal field masked; its value is in no payables event.
func TestEventsMaskCustomFields(t *testing.T) {
	e := setup(t)
	po := e.order(t, e.Supplier, "PO-1")
	e.receive(t, po, 1, "10")
	e.receive(t, po, 2, "5")
	secret := uuid.NewString()
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", strings.Replace(e.invoice(e.Supplier, "INV-X", "157.80", "22.80",
		poItem(po, 1, "10", "EA", "120.00"), poItem(po, 2, "5", "KG", "15.00")),
		`"items"`, `"extensions": {"region": "CC9", "channel": "`+secret+`"}, "items"`, 1), "")
	e.posted(t, inv)
	for _, name := range []string{"supplier_invoice_captured", "supplier_invoice_posted"} {
		t.Run(name, func(t *testing.T) {
			got := events(t, e.Tenant, name)
			if len(got) != 1 {
				t.Fatalf("got %d events, want 1", len(got))
			}
			ext, _ := got[0]["extensions"].(map[string]any)
			if ext["region"] != "CC9" {
				t.Errorf("region: got %v, want CC9", ext["region"])
			}
			if ext["channel"] != extensions.Masked {
				t.Errorf("channel: got %v, want %s", ext["channel"], extensions.Masked)
			}
			if got[0]["extensions_schema"] != float64(1) {
				t.Errorf("extensions_schema: got %v, want 1", got[0]["extensions_schema"])
			}
		})
	}
	apitest.NeverPublished(t, e.Tenant, "payables", secret)
}
