package procurement_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// TestOrderValuation: an order is valued when it is released, through
// GR/IR when its legal entity has a GR/IR account then and directly
// otherwise, and keeps that valuation; an order a migration run loads is
// valued directly whatever the account determination says, as its
// accounting comes from the loaded journal (T33).
func TestOrderValuation(t *testing.T) {
	e := setup(t)
	before := e.order(t, items(item(e.office, "10", "EA", "5.00", "EUR", "")), "")
	if before["valuation"] != "direct" {
		t.Errorf("draft valuation: got %v, want direct", before["valuation"])
	}
	if got := e.act(t, "/v1/purchase-orders/"+id(before), "release", ""); got["valuation"] != "direct" {
		t.Errorf("released before GR/IR: got %v, want direct", got["valuation"])
	}
	e.GRIR(t)
	after := e.act(t, "/v1/purchase-orders/"+id(e.order(t, items(item(e.office, "10", "EA", "5.00", "EUR", "")), "")), "release", "")
	if after["valuation"] != "gr_ir" {
		t.Errorf("released with GR/IR: got %v, want gr_ir", after["valuation"])
	}
	if got, _ := e.Must(200, "GET", "/v1/purchase-orders/"+id(before), "", ""); got["valuation"] != "direct" {
		t.Errorf("released before GR/IR, read again: got %v, want direct", got["valuation"])
	}
	released := events(t, e, "purchase_order_released")
	if got := released[len(released)-1]["valuation"]; got != "gr_ir" {
		t.Errorf("PurchaseOrderReleased valuation: got %v, want gr_ir", got)
	}
	loaded := e.order(t, items(item(e.office, "10", "EA", "5.00", "EUR", "")), "")
	var o procurement.PurchaseOrder
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
		o, err = endpoints().ReleaseLoadedOrder(ctx, uuid.MustParse(id(loaded)), func(int32) error { return nil })
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if o.Valuation != procurement.ValuationDirect {
		t.Errorf("loaded order: got %v, want direct", o.Valuation)
	}
}
