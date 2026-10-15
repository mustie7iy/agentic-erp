package payables_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// vanished is a partner context that no longer finds the supplier.
type vanished struct{}

func (vanished) Supplier(context.Context, uuid.UUID, uuid.UUID) (partner.Supplier, bool, error) {
	return partner.Supplier{}, false, nil
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
