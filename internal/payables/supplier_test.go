package payables_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// vanished is a partner context that no longer finds the supplier.
type vanished struct{}

func (vanished) Supplier(context.Context, uuid.UUID, uuid.UUID) (partner.Supplier, bool, error) {
	return partner.Supplier{}, false, nil
}

func (vanished) PaymentTerm(context.Context, string) (partner.PaymentTerm, bool, error) {
	return partner.PaymentTerm{}, false, nil
}

// TestPostWithoutTheSupplier: an invoice whose supplier the partner context
// no longer finds is refused with a reason that says so, never read as a
// supplier with an empty status ("the business partner is ").
func TestPostWithoutTheSupplier(t *testing.T) {
	e := setup(t)
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Vendor, "V-1", "119.00", "19.00", accountItem(e.expense, "100.00")), "")
	h := endpoints(nil)
	h.Suppliers = vanished{}
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.PostInvoice(ctx, uuid.MustParse(id(inv)), func(int32) error { return nil }, nil)
		return err
	})
	p := problem.From(err)
	if p.Status != 422 || p.Code != "payables.supplier_not_usable" {
		t.Fatalf("got %d %s (%v), want 422 payables.supplier_not_usable", p.Status, p.Code, err)
	}
	if want := "The supplier cannot be posted: no business partner with this id."; p.Detail != want {
		t.Errorf("detail: got %q, want %q", p.Detail, want)
	}
}

// TestExpandWithoutTheSupplier: an invoice whose supplier the partner
// context no longer finds is returned without the expansion, never with a
// supplier summary of zero values (a nil id and an empty name).
func TestExpandWithoutTheSupplier(t *testing.T) {
	e := setup(t)
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Vendor, "V-1", "119.00", "19.00", accountItem(e.expense, "100.00")), "")
	h := endpoints(nil)
	h.Suppliers = vanished{}
	var res gen.GetSupplierInvoiceRes
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		var err error
		res, err = h.GetSupplierInvoice(ctx, gen.GetSupplierInvoiceParams{ID: uuid.MustParse(id(inv)), Expand: []string{"supplier"}})
		return err
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	ok, isOK := res.(*gen.GetSupplierInvoiceOKHeaders)
	if !isOK {
		t.Fatalf("response: got %T, want *gen.GetSupplierInvoiceOKHeaders", res)
	}
	if got := ok.Response; got.Expanded.Set {
		t.Errorf("expanded: got %+v, want no expansion", got.Expanded.Value)
	}
}

// TestExpandNeedsPartnerRead: expand=supplier embeds the supplier only for
// a caller who may read business partners, person or agent; without the
// permission the invoice and the page are answered with supplier_id alone,
// never refused, as an expansion never crosses permissions (04 §4.4).
func TestExpandNeedsPartnerRead(t *testing.T) {
	e := setup(t)
	e.Tenant.AllCompanies(t, e.Tenant.Agent)
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Vendor, "V-1", "119.00", "19.00", accountItem(e.expense, "100.00")), "")
	paths := map[string]string{
		"get":  "/v1/supplier-invoices/" + id(inv) + "?expand=supplier",
		"list": "/v1/supplier-invoices?supplier_id=" + e.Vendor.String() + "&expand=supplier",
	}
	read := func(t *testing.T, as pgtest.Actor, path string) map[string]any {
		t.Helper()
		got, _ := e.As(as).In(t).Must(200, "GET", path, "", "")
		if got["id"] == nil { // a page: its one invoice
			items := got["items"].([]any)
			if len(items) != 1 {
				t.Fatalf("page: got %d invoices, want 1", len(items))
			}
			got = items[0].(map[string]any)
		}
		if got["supplier_id"] != e.Vendor.String() {
			t.Errorf("supplier_id: got %v, want %v", got["supplier_id"], e.Vendor)
		}
		return got
	}
	for _, held := range []bool{true, false} {
		if !held {
			e.Tenant.Revoke(t, partner.ReadPermission, e.Tenant.Human, e.Tenant.Agent)
		}
		for _, as := range []pgtest.Actor{e.Tenant.Human, e.Tenant.Agent} {
			for op, path := range paths {
				t.Run(fmt.Sprintf("%s by %s, permission held %v", op, as.Kind, held), func(t *testing.T) {
					exp, expanded := read(t, as, path)["expanded"].(map[string]any)
					if expanded != held {
						t.Fatalf("expanded: got %v, want %v", expanded, held)
					}
					if held && exp["supplier"].(map[string]any)["number"] != "BP90000002" {
						t.Errorf("expanded supplier: got %v, want BP90000002", exp["supplier"])
					}
				})
			}
		}
	}
}
