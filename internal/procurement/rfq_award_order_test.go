package procurement_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// The award of an RFQ to a purchase order (T82f, ADR-0069 As built), and
// the reason an award to a quotation not ranked first needs.

// sourcedQuoted is an RFQ from an approved requisition of ten sheets of
// paper on the expense account charged to the Admin cost center and ten on
// the office account charged to Sales, needed by 20 July 2026, published and
// quoted by Tyrell (4.80 and 3.90, 87.00 in all, ranked second) and the
// vendor (4.50 and 3.50, 80.00, first). The clock is 15 June 2026.
func sourcedQuoted(t *testing.T) (e assignedEnv, req, path, tyrells, cheapest string) {
	t.Helper()
	e = setupAssignment(t)
	e.API = apitest.NewWith(t, func(cfg *app.Config) {
		cfg.Now = func() time.Time { return time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) }
	})
	r := e.requisition(t, items(centered(e.expense, "5.00", e.c.Admin, ""), centered(e.office, "4.00", e.c.Sales, "")), `, "needed_by": "2026-07-20"`)
	e.act(t, "/v1/requisitions/"+id(r), "submit", "")
	e.Approve(t, id(r))
	q, _ := e.Must(201, "POST", "/v1/rfqs", e.sourcingBody(id(r), "", e.Supplier, e.Vendor), "")
	path = "/v1/rfqs/" + id(q)
	e.act(t, path, "publish", "")
	a, _ := e.Must(201, "POST", path+"/quotations", quotationBody(e.Supplier, "Q-1", price(1, "4.80", "EUR", ""), price(2, "3.90", "EUR", "")), "")
	b, _ := e.Must(201, "POST", path+"/quotations", quotationBody(e.Vendor, "Q-2", price(1, "4.50", "EUR", ""), price(2, "3.50", "EUR", "")), "")
	return e, id(r), path, id(a), id(b)
}

// orderAward awards to the quotation as a purchase order; extra is added
// to the body.
func orderAward(quotation, extra string) string {
	return fmt.Sprintf(`{"quotation_id": %q, "award_to": "purchase_order"%s}`, quotation, extra)
}

func TestAwardRFQToOrder(t *testing.T) {
	e, req, path, tyrells, _ := sourcedQuoted(t)
	t.Run("refused", func(t *testing.T) {
		for name, c := range map[string]struct {
			body, code, fields string
		}{
			"to a contract":         {awardBody(tyrells), "procurement.rfq_from_requisition", "award_to"},
			"to nothing named":      {fmt.Sprintf(`{"quotation_id": %q}`, tyrells), "procurement.rfq_from_requisition", "award_to"},
			"without a reason":      {orderAward(tyrells, ""), "procurement.award_reason_required", "reason"},
			"a blank reason":        {orderAward(tyrells, `, "reason": " "`), "request.validation_failed", "reason"},
			"a contract validity":   {orderAward(tyrells, `, "valid_from": "2026-07-01", "valid_to": "2027-06-30", "reason": "R"`), "request.validation_failed", "valid_from, valid_to"},
			"an unknown document":   {fmt.Sprintf(`{"quotation_id": %q, "award_to": "framework"}`, tyrells), "request.validation_failed", "award_to"},
			"a validity of its own": {orderAward(tyrells, `, "valid_to": "2027-06-30", "reason": "R"`), "request.validation_failed", "valid_to"},
		} {
			t.Run(name, func(t *testing.T) {
				p := e.in(t).Problem(422, c.code, "POST", path+":award", c.body, "*")
				if got := errorFields(p); got != c.fields {
					t.Errorf("field errors: got %s, want %s", got, c.fields)
				}
			})
		}
	})
	_, tag := e.Must(200, "GET", path, "", "")
	r, _ := e.Must(200, "POST", path+":award", orderAward(tyrells, `, "reason": "Delivers by the need date"`), tag)
	for field, want := range map[string]any{"status": "awarded", "award_reason": "Delivers by the need date", "purchase_order_document_number": "PO100000",
		"contract_id": nil} {
		if r[field] != want {
			t.Errorf("RFQ %s: got %v, want %v", field, r[field], want)
		}
	}
	o, _ := e.Must(200, "GET", "/v1/purchase-orders/"+fmt.Sprint(r["purchase_order_id"]), "", "")
	for field, want := range map[string]any{"status": "draft", "supplier_id": e.Supplier.String(), "currency": "EUR", "order_date": "2026-06-15",
		"requisition_id": req, "document_number": "PO100000"} {
		if o[field] != want {
			t.Errorf("order %s: got %v, want %v", field, o[field], want)
		}
	}
	if got := amountOf(t, o, "total_amount"); got != "87.00 EUR" {
		t.Errorf("order total: got %s, want 87.00 EUR", got)
	}
	var lines []string
	for _, v := range apitest.List(t, o, "items") {
		it := v.(map[string]any)
		lines = append(lines, fmt.Sprintf("%v %v %v %v %v", apitest.Object(t, it, "unit_price")["amount"], it["account_id"], it["delivery_date"],
			it["account_assignment"], it["cost_center_id"]))
	}
	want := []string{fmt.Sprintf("4.80 %s 2026-07-20 cost_center %s", e.expense, e.c.Admin), fmt.Sprintf("3.90 %s 2026-07-20 cost_center %s", e.office, e.c.Sales)}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("order items: got %v, want %v", lines, want)
	}
	if got := mustGet(t, e.env, "/v1/requisitions/"+req); got["status"] != "ordered" {
		t.Errorf("requisition: got %v, want ordered", got["status"])
	}
	if got := e.outcomes(t, path); !strings.Contains(got, "Q-1 awarded") || !strings.Contains(got, "Q-2 not_awarded") {
		t.Errorf("quotations: got %s, want Q-1 awarded and Q-2 not_awarded", got)
	}
	e.Problem(409, "procurement.invalid_status", "POST", path+":award", orderAward(tyrells, `, "reason": "R"`), "*")
	ordered := events(t, e.env, "requisition_ordered")
	if len(ordered) != 1 || ordered[0]["previous_status"] != "in_sourcing" || ordered[0]["purchase_order_id"] != r["purchase_order_id"] {
		t.Errorf("requisition_ordered events: got %v, want one from in_sourcing naming the order", ordered)
	}
	awarded := apitest.CompanyNamed(t, events(t, e.env, "rfq_awarded_to_order"))
	wantEvent := map[string]any{"rfq_id": id(r), "document_number": r["document_number"], "legal_entity_id": e.DE.String(), "legal_entity_code": "2000",
		"supplier_quotation_id": tyrells, "supplier_id": e.Supplier.String(), "supplier_number": "BP90000001", "supplier_reference": "Q-1",
		"purchase_order_id": r["purchase_order_id"], "purchase_order_document_number": "PO100000", "order_date": "2026-06-15",
		"requisition_id": req, "requisition_document_number": "PR100000", "total": map[string]any{"amount": "87.00", "currency": "EUR"},
		"reason": "Delivers by the need date"}
	if !reflect.DeepEqual(awarded, []map[string]any{wantEvent}) {
		t.Errorf("rfq_awarded_to_order events: got %v, want %v", awarded, wantEvent)
	}
	if n := len(events(t, e.env, "rfq_awarded")); n != 0 {
		t.Errorf("rfq_awarded events: got %d, want none", n)
	}
	if n := audits(t, e.env, "procurement.purchase_order.create", fmt.Sprint(r["purchase_order_id"])); n != 1 {
		t.Errorf("the order's audit records: got %d, want 1", n)
	}
	// The order is released as any: within its requisition's total, at once.
	released := e.act(t, "/v1/purchase-orders/"+fmt.Sprint(r["purchase_order_id"]), "release", "")
	if released["status"] != "released" {
		t.Errorf("released order: got %v, want released", released["status"])
	}
}

// TestAwardRFQToOrderOwnItems: an RFQ with its own items is awarded to a
// purchase order naming no requisition; the quotation ranked first needs
// no reason; a contract needs its validity.
func TestAwardRFQToOrderOwnItems(t *testing.T) {
	e := setupIn2026(t)
	path, _, cabots := e.quoted(t)
	p := e.Problem(422, "request.validation_failed", "POST", path+":award", fmt.Sprintf(`{"quotation_id": %q}`, cabots), "*")
	if got := errorFields(p); got != "valid_from, valid_to" {
		t.Errorf("field errors: got %s, want valid_from, valid_to", got)
	}
	r, _ := e.Must(200, "POST", path+":award", orderAward(cabots, ""), "*")
	if r["award_reason"] != nil || r["purchase_order_id"] == nil {
		t.Errorf("RFQ: got reason %v and order %v, want no reason and an order", r["award_reason"], r["purchase_order_id"])
	}
	o := mustGet(t, e, "/v1/purchase-orders/"+fmt.Sprint(r["purchase_order_id"]))
	if o["requisition_id"] != nil || o["supplier_id"] != e.Vendor.String() || amountOf(t, o, "total_amount") != "19125.00 EUR" {
		t.Errorf("order: got requisition %v, supplier %v, total %s, want none, the vendor, 19125.00 EUR", o["requisition_id"], o["supplier_id"],
			amountOf(t, o, "total_amount"))
	}
	if ev := events(t, e, "rfq_awarded_to_order"); len(ev) != 1 || ev[0]["requisition_id"] != nil || ev[0]["reason"] != nil {
		t.Errorf("rfq_awarded_to_order events: got %v, want one without requisition or reason", ev)
	}
}

// TestAwardRFQToOrderDatabaseFailures: the award to a purchase order with
// each of its database statements failing in turn.
func TestAwardRFQToOrderDatabaseFailures(t *testing.T) {
	e, _, path, tyrells, _ := sourcedQuoted(t)
	h := endpoints()
	h.Now = func() time.Time { return time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) }
	rfq := uuid.MustParse(strings.TrimPrefix(path, "/v1/rfqs/"))
	call := func(ctx context.Context) error {
		_, err := h.AwardRfq(ctx, &gen.RfqAward{QuotationID: uuid.MustParse(tyrells), AwardTo: gen.NewOptString("purchase_order"),
			Reason: gen.NewOptString("Delivers by the need date")}, gen.AwardRfqParams{ID: rfq, IfMatch: "*"})
		return err
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "award RFQ to an order", call)
}

// TestAwardRFQToOrderNeedsOrderCreate: an award to a purchase order
// creates one, which procurement.order.create grants, as it does through
// POST /v1/purchase-orders; the RFQ stays published.
func TestAwardRFQToOrderNeedsOrderCreate(t *testing.T) {
	e, req, path, tyrells, _ := sourcedQuoted(t)
	pgtest.Exec(t, `DELETE FROM identity.role_permission WHERE tenant_id = $1 AND permission_code = 'procurement.order.create'`, e.Tenant.ID)
	e.Problem(403, "identity.permission_missing", "POST", path+":award", orderAward(tyrells, `, "reason": "R"`), "*")
	if got := mustGet(t, e.env, path); got["status"] != "published" {
		t.Errorf("RFQ: got %v, want published", got["status"])
	}
	if got := mustGet(t, e.env, "/v1/requisitions/"+req); got["status"] != "in_sourcing" {
		t.Errorf("requisition: got %v, want in_sourcing", got["status"])
	}
}
