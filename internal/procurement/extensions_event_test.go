package procurement_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
)

// TestEventsMaskCustomFields: PurchaseOrderReleased and
// PurchaseOrderChanged carry the order's custom fields and their schema
// version, the personal field masked; its values are in no procurement
// event.
func TestEventsMaskCustomFields(t *testing.T) {
	e := setup(t)
	secret, later := uuid.NewString(), uuid.NewString()
	o := e.order(t, items(item(e.office, "10", "EA", "2.50", "EUR", "")), `, "extensions": {"region": "north", "project": "`+secret+`"}`)
	path := "/v1/purchase-orders/" + id(o)
	e.act(t, path, "release", "")
	ch, tag := e.Must(201, "POST", path+"/changes", `{"reason": "r", "extensions": {"region": "south", "project": "`+later+`"}}`, "")
	if ch, _ = e.Must(200, "POST", path+"/changes/"+id(ch)+":submit", "", tag); ch["status"] != "applied" {
		t.Fatalf("change: got %v, want applied", ch["status"])
	}
	for name, center := range map[string]string{"purchase_order_released": "north", "purchase_order_changed": "south"} {
		t.Run(name, func(t *testing.T) {
			got := events(t, e, name)
			if len(got) != 1 {
				t.Fatalf("got %d events, want 1", len(got))
			}
			ext, _ := got[0]["extensions"].(map[string]any)
			if ext["region"] != center {
				t.Errorf("region: got %v, want %s", ext["region"], center)
			}
			if ext["project"] != extensions.Masked {
				t.Errorf("project: got %v, want %s", ext["project"], extensions.Masked)
			}
			if got[0]["extensions_schema"] != float64(1) {
				t.Errorf("extensions_schema: got %v, want 1", got[0]["extensions_schema"])
			}
		})
	}
	apitest.NeverPublished(t, e.Tenant, "procurement", secret)
	apitest.NeverPublished(t, e.Tenant, "procurement", later)
}
