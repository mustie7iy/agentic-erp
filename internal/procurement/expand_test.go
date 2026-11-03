package procurement_test

import (
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestExpandNeedsPartnerRead: expand=supplier embeds a requisition's or an
// order's supplier only for a caller who may read business partners,
// person or agent; without the permission the document and the page are
// answered with supplier_id alone, never refused, as an expansion never
// crosses permissions (04 §4.4).
func TestExpandNeedsPartnerRead(t *testing.T) {
	e := setup(t)
	e.Tenant.AllCompanies(t, e.Tenant.Agent)
	r := e.requisition(t, items(item(e.office, "1", "EA", "1.00", "EUR", "")), fmt.Sprintf(`, "supplier_id": %q`, e.Supplier))
	o := e.order(t, items(item(e.office, "1", "EA", "1.00", "EUR", "")), "")
	paths := map[string]string{
		"requisition":       "/v1/requisitions/" + id(r) + "?expand=supplier",
		"requisitions page": "/v1/requisitions?document_number=" + r["document_number"].(string) + "&expand=supplier",
		"order":             "/v1/purchase-orders/" + id(o) + "?expand=supplier",
		"orders page":       "/v1/purchase-orders?document_number=" + o["document_number"].(string) + "&expand=supplier",
	}
	read := func(t *testing.T, as pgtest.Actor, path string) map[string]any {
		t.Helper()
		got, _ := e.As(as).In(t).Must(200, "GET", path, "", "")
		if got["id"] == nil { // a page: its one document
			items := got["items"].([]any)
			if len(items) != 1 {
				t.Fatalf("page: got %d documents, want 1", len(items))
			}
			got = items[0].(map[string]any)
		}
		if got["supplier_id"] != e.Supplier.String() {
			t.Errorf("supplier_id: got %v, want %v", got["supplier_id"], e.Supplier)
		}
		return got
	}
	for _, held := range []bool{true, false} {
		if !held {
			e.Tenant.Revoke(t, partner.ReadPermission, e.Tenant.Human, e.Tenant.Agent)
		}
		for _, as := range []pgtest.Actor{e.Tenant.Human, e.Tenant.Agent} {
			for doc, path := range paths {
				t.Run(fmt.Sprintf("%s by %s, permission held %v", doc, as.Kind, held), func(t *testing.T) {
					exp, expanded := read(t, as, path)["expanded"].(map[string]any)
					if expanded != held {
						t.Fatalf("expanded: got %v, want %v", expanded, held)
					}
					if held && exp["supplier"].(map[string]any)["number"] != "BP90000001" {
						t.Errorf("expanded supplier: got %v, want BP90000001", exp["supplier"])
					}
				})
			}
		}
	}
}
