package procurement_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// RFQs from requisitions (T82e, ADR-0069 As built): the requisition's
// items become the RFQ's and the requisition is in sourcing.

// approvedRequisition raises a requisition for paper and pens, needed by
// 20 July 2026, and has it approved.
func (e env) approvedRequisition(t *testing.T) map[string]any {
	t.Helper()
	r := e.requisition(t, items(item(e.office, "10", "EA", "5.00", "EUR", ""), item(e.expense, "2.5", "KG", "4.00", "EUR", "")),
		`, "needed_by": "2026-07-20"`)
	e.act(t, "/v1/requisitions/"+id(r), "submit", "")
	e.Approve(t, id(r))
	return r
}

// sourcingBody is an RFQ of company 2000 from the requisition, inviting
// the suppliers; extra is added to the body.
func (e env) sourcingBody(requisition, extra string, suppliers ...uuid.UUID) string {
	ids := make([]string, len(suppliers))
	for i, s := range suppliers {
		ids[i] = fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf(`{"legal_entity_id": %q, "requisition_id": %q, "due_date": "2026-07-31", "supplier_ids": [%s]%s}`,
		e.DE, requisition, strings.Join(ids, ", "), extra)
}

// rfqItems are an RFQ's items as "no key description quantity unit
// account delivery requisition-item".
func rfqItems(t *testing.T, r map[string]any) []string {
	t.Helper()
	var out []string
	for _, v := range apitest.List(t, r, "items") {
		it := v.(map[string]any)
		q := it["quantity"].(map[string]any)
		out = append(out, fmt.Sprintf("%v %v %v %v %v %v %v %v", it["item_no"], it["material_key"], it["description"], q["value"], q["unit"],
			it["account_id"], it["delivery_date"], it["requisition_item_no"]))
	}
	return out
}

func TestRFQFromRequisition(t *testing.T) {
	e := setupIn2026(t)
	req := e.approvedRequisition(t)
	reqPath := "/v1/requisitions/" + id(req)
	r, _ := e.Must(201, "POST", "/v1/rfqs", e.sourcingBody(id(req), "", e.Supplier, e.Vendor), "")
	for field, want := range map[string]any{"requisition_id": id(req), "requisition_document_number": "PR100000", "currency": "EUR", "status": "draft"} {
		if r[field] != want {
			t.Errorf("RFQ %s: got %v, want %v", field, r[field], want)
		}
	}
	want := []string{
		fmt.Sprintf("1 PR100000/1 Paper 10 EA %s 2026-07-20 1", e.office),
		fmt.Sprintf("2 PR100000/2 Paper 2.5 KG %s 2026-07-20 2", e.expense),
	}
	if got := rfqItems(t, r); strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Errorf("RFQ items: got %v, want %v", got, want)
	}
	got, _ := e.Must(200, "GET", reqPath, "", "")
	for field, want := range map[string]any{"status": "in_sourcing", "rfq_id": id(r), "rfq_document_number": r["document_number"]} {
		if got[field] != want {
			t.Errorf("requisition %s: got %v, want %v", field, got[field], want)
		}
	}
	// In sourcing, the requisition is sourced, ordered and cancelled no more.
	e.Problem(409, "procurement.invalid_status", "POST", "/v1/rfqs", e.sourcingBody(id(req), "", e.Supplier), "")
	_, tag := e.Must(200, "GET", reqPath, "", "")
	e.Problem(409, "procurement.invalid_status", "POST", reqPath+":cancel", "", tag)
	e.Problem(409, "procurement.invalid_status", "POST", "/v1/purchase-orders", fmt.Sprintf(`{"legal_entity_id": %q, "supplier_id": %q, "currency": "EUR",
		"order_date": "2026-06-15", "requisition_id": %q, "items": %s}`, e.DE, e.Supplier, id(req), items(item(e.office, "1", "EA", "1.00", "EUR", ""))), "")
	e.act(t, "/v1/rfqs/"+id(r), "publish", "")
	list, _ := e.Must(200, "GET", "/v1/rfqs?requisition_id="+id(req), "", "")
	if got := apitest.List(t, list, "items"); len(got) != 1 || id(got[0].(map[string]any)) != id(r) {
		t.Errorf("RFQs of the requisition: got %v, want the one RFQ", got)
	}
	if got := apitest.List(t, mustGet(t, e, "/v1/rfqs?requisition_id="+uuid.NewString()), "items"); len(got) != 0 {
		t.Errorf("RFQs of another requisition: got %v, want none", got)
	}
	for _, name := range []string{"rfq_changed", "rfq_published"} {
		for i, ev := range events(t, e, name) {
			if ev["requisition_id"] != id(req) || ev["requisition_document_number"] != "PR100000" {
				t.Errorf("%s %d: got requisition %v %v, want %s PR100000", name, i, ev["requisition_id"], ev["requisition_document_number"], id(req))
			}
		}
	}
	if n := audits(t, e, "procurement.requisition.in_sourcing", id(req)); n != 1 {
		t.Errorf("procurement.requisition.in_sourcing audit records: got %d, want 1", n)
	}
}

// mustGet reads a path that answers 200.
func mustGet(t *testing.T, e env, path string) map[string]any {
	t.Helper()
	got, _ := e.Must(200, "GET", path, "", "")
	return got
}

// TestRFQFromRequisitionRefusals: what an RFQ from a requisition is refused
// for, each by the request's own field, and what an RFQ without one must
// name. The requisition stays approved.
func TestRFQFromRequisitionRefusals(t *testing.T) {
	e := setupIn2026(t)
	req := e.approvedRequisition(t)
	draft := e.requisition(t, items(item(e.office, "1", "EA", "1.00", "EUR", "")), "")
	own := func(body string) string { return strings.Replace(body, `"currency": "EUR", `, "", 1) }
	for name, c := range map[string]struct {
		body   string
		status int
		code   string
		fields string
	}{
		"items beside it": {e.sourcingBody(id(req), `, "items": `+items(rfqItem("A", "1", "EA", e.office, "")), e.Supplier), 422,
			"procurement.rfq_items_from_requisition", "items"},
		"another currency": {e.sourcingBody(id(req), `, "currency": "USD"`, e.Supplier), 422, "procurement.requisition_mismatch", "currency"},
		"another company":  {strings.Replace(e.sourcingBody(id(req), "", e.Supplier), e.DE.String(), e.US.String(), 1), 422, "procurement.requisition_mismatch", "requisition_id"},
		"no such one":      {e.sourcingBody(uuid.NewString(), "", e.Supplier), 422, "request.reference_not_found", "requisition_id"},
		"a draft":          {e.sourcingBody(id(draft), "", e.Supplier), 409, "procurement.invalid_status", ""},
		"neither":          {own(e.rfqBody("2026-07-31", "[]", e.Supplier)), 400, "request.invalid_body", ""},
		"no currency":      {own(e.rfqBody("2026-07-31", e.chairsAndDesks(), e.Supplier)), 422, "request.validation_failed", "currency"},
		"no items": {fmt.Sprintf(`{"legal_entity_id": %q, "currency": "EUR", "due_date": "2026-07-31", "supplier_ids": [%q]}`, e.DE, e.Supplier), 422,
			"request.validation_failed", "items"},
		"nothing": {fmt.Sprintf(`{"legal_entity_id": %q, "due_date": "2026-07-31", "supplier_ids": [%q]}`, e.DE, e.Supplier), 422,
			"request.validation_failed", "currency items"},
	} {
		t.Run(name, func(t *testing.T) {
			p := e.in(t).Problem(c.status, c.code, "POST", "/v1/rfqs", c.body, "")
			if c.fields == "" {
				return
			}
			var fields []string
			for _, fe := range apitest.List(t, p, "errors") {
				fields = append(fields, fe.(map[string]any)["field"].(string))
			}
			if got := strings.Join(fields, " "); got != c.fields {
				t.Errorf("field errors: got %s, want %s", got, c.fields)
			}
		})
	}
	if got := mustGet(t, e, "/v1/requisitions/"+id(req)); got["status"] != "approved" || got["rfq_id"] != nil {
		t.Errorf("requisition after the refusals: got %v naming RFQ %v, want approved naming none", got["status"], got["rfq_id"])
	}
}

// TestRFQFromRequisitionAccountBlocked: a requisition item whose account
// the company can no longer post to is refused by the RFQ item it would
// become, and the requisition stays approved.
func TestRFQFromRequisitionAccountBlocked(t *testing.T) {
	e := setupIn2026(t)
	req := e.approvedRequisition(t)
	pgtest.Exec(t, `UPDATE ledger.gl_account SET status = 'inactive', version = version + 1 WHERE id = $1`, e.expense)
	p := e.Problem(422, "procurement.account_not_postable", "POST", "/v1/rfqs", e.sourcingBody(id(req), "", e.Supplier), "")
	if errs := apitest.List(t, p, "errors"); len(errs) != 1 || errs[0].(map[string]any)["field"] != "items[1].account_id" {
		t.Errorf("field errors: got %v, want one on items[1].account_id", errs)
	}
	if got := mustGet(t, e, "/v1/requisitions/"+id(req)); got["status"] != "approved" {
		t.Errorf("requisition: got %v, want approved", got["status"])
	}
}

// TestRFQFromRequisitionDatabaseFailures: creating an RFQ from a
// requisition with each of its database statements failing in turn.
func TestRFQFromRequisitionDatabaseFailures(t *testing.T) {
	e := setupIn2026(t)
	req := uuidOf(e.approvedRequisition(t))
	h := endpoints()
	h.Now = func() time.Time { return time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) }
	call := func(ctx context.Context) error {
		_, err := h.CreateRfq(ctx, &gen.RfqCreate{LegalEntityID: e.DE, RequisitionID: gen.NewOptUUID(req), DueDate: dateOf("2026-07-31"),
			SupplierIds: []uuid.UUID{e.Supplier}}, gen.CreateRfqParams{})
		return err
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "create RFQ from a requisition", call)
}

// TestRFQItemDeliveryDate: a buyer's own RFQ item may carry the date it is
// wanted by, and names no requisition item.
func TestRFQItemDeliveryDate(t *testing.T) {
	e := setupIn2026(t)
	r, _ := e.Must(201, "POST", "/v1/rfqs", e.rfqBody("2026-07-31", items(rfqItem("A", "1", "EA", e.office, `, "delivery_date": "2026-08-15"`),
		rfqItem("B", "2", "EA", e.office, "")), e.Supplier), "")
	want := []string{fmt.Sprintf("1 A Item A 1 EA %s 2026-08-15 <nil>", e.office), fmt.Sprintf("2 B Item B 2 EA %s <nil> <nil>", e.office)}
	if got := rfqItems(t, r); strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Errorf("RFQ items: got %v, want %v", got, want)
	}
	if r["requisition_id"] != nil {
		t.Errorf("requisition_id: got %v, want none", r["requisition_id"])
	}
}
