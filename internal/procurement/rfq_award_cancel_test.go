package procurement_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
)

// An award's purchase order cancelled (T82h, ADR-0069 As built): the RFQ
// shows the order cancelled and its requisition is approved again.

// awardedToOrder is sourcedQuoted awarded to Tyrell's quotation as a
// purchase order, with a reason; it returns the RFQ and the order.
func awardedToOrder(t *testing.T) (e assignedEnv, req, path, tyrells string, rfq map[string]any) {
	t.Helper()
	e, req, path, tyrells, _ = sourcedQuoted(t)
	rfq, _ = e.Must(200, "POST", path+":award", orderAward(tyrells, `, "reason": "Delivers by the need date"`), "*")
	return e, req, path, tyrells, rfq
}

func TestCancelAwardOrder(t *testing.T) {
	e, req, path, tyrells, awarded := awardedToOrder(t)
	order := fmt.Sprint(awarded["purchase_order_id"])
	e.act(t, "/v1/purchase-orders/"+order, "cancel", "")
	r := mustGet(t, e.env, path)
	for field, want := range map[string]any{"status": "order_cancelled", "purchase_order_id": order,
		"purchase_order_document_number": awarded["purchase_order_document_number"], "award_reason": "Delivers by the need date",
		"version": awarded["version"].(float64) + 1} {
		if r[field] != want {
			t.Errorf("RFQ %s: got %v, want %v", field, r[field], want)
		}
	}
	if got, want := e.outcomes(t, path), "Q-1 awarded 2, Q-2 not_awarded 2"; got != want {
		t.Errorf("quotations: got %s, want %s", got, want)
	}
	if got := mustGet(t, e.env, "/v1/requisitions/"+req); got["status"] != "approved" {
		t.Errorf("requisition: got %v, want approved", got["status"])
	}
	ev := reopened(t, e.env)
	if len(ev) != 1 {
		t.Fatalf("requisition_reopened events: got %d, want 1", len(ev))
	}
	for field, want := range map[string]any{"cause": "purchase_order_cancelled", "previous_status": "ordered", "purchase_order_id": order,
		"purchase_order_document_number": awarded["purchase_order_document_number"], "legal_entity_code": "2000"} {
		if ev[0][field] != want {
			t.Errorf("requisition_reopened %s: got %v, want %v", field, ev[0][field], want)
		}
	}
	changed := lastChange(t, e.env, id(r))
	for field, want := range map[string]any{"change": "order_cancelled", "status": "order_cancelled", "purchase_order_id": order,
		"purchase_order_document_number": awarded["purchase_order_document_number"]} {
		if changed[field] != want {
			t.Errorf("rfq_changed %s: got %v, want %v", field, changed[field], want)
		}
	}
	for action, object := range map[string]string{"procurement.rfq.order_cancelled": id(r), "procurement.requisition.reopened": req} {
		if n := audits(t, e.env, action, object); n != 1 {
			t.Errorf("%s audit records: got %d, want 1", action, n)
		}
	}
	// The RFQ is not awarded again; the requisition is sourced by a new one.
	e.Problem(409, "procurement.invalid_status", "POST", path+":award", orderAward(tyrells, `, "reason": "R"`), "*")
	e.Problem(409, "procurement.invalid_status", "POST", path+":close", closing("R"), "*")
	e.Must(201, "POST", "/v1/rfqs", e.sourcingBody(req, "", e.Supplier), "")
}

// TestCancelAwardOrderReleased: a released award order is cancelled as
// any; the RFQ follows it.
func TestCancelAwardOrderReleased(t *testing.T) {
	e, _, path, _, awarded := awardedToOrder(t)
	order := "/v1/purchase-orders/" + fmt.Sprint(awarded["purchase_order_id"])
	e.act(t, order, "release", "")
	e.act(t, order, "cancel", "")
	if got := mustGet(t, e.env, path); got["status"] != "order_cancelled" {
		t.Errorf("RFQ: got %v, want order_cancelled", got["status"])
	}
}

// TestCancelAwardOrderDatabaseFailures cancels the award's order with each
// of its database statements failing in turn, the RFQ's and the
// requisition's among them.
func TestCancelAwardOrderDatabaseFailures(t *testing.T) {
	e, _, _, _, awarded := awardedToOrder(t)
	h := endpoints()
	order := uuid.MustParse(fmt.Sprint(awarded["purchase_order_id"]))
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "cancel an award's order", func(ctx context.Context) error {
		_, err := h.CancelPurchaseOrder(ctx, gen.CancelPurchaseOrderParams{ID: order, IfMatch: "*"})
		return err
	})
}
