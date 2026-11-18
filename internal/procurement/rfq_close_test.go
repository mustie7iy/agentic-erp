package procurement_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
)

// RFQs ended without an award (T82g, ADR-0069 As built): cancelled or
// closed with a reason, their quotations not awarded, and a requisition
// they were created from approved again.

// closing is the body of :cancel or :close.
func closing(reason string) string { return fmt.Sprintf(`{"reason": %q}`, reason) }

// reopened are the tenant's requisition_reopened events, each with the
// company named.
func reopened(t *testing.T, e env) []map[string]any {
	t.Helper()
	return apitest.CompanyNamed(t, events(t, e, "requisition_reopened"))
}

// lastChange is the last rfq_changed event of the RFQ.
func lastChange(t *testing.T, e env, rfq string) map[string]any {
	t.Helper()
	var last map[string]any
	for _, ev := range events(t, e, "rfq_changed") {
		if ev["rfq_id"] == rfq {
			last = ev
		}
	}
	return last
}

// TestCloseRFQFromRequisition: a published RFQ from a requisition, quoted
// by two suppliers, is closed without an award: both quotations are not
// awarded, the requisition is approved again and published as reopened,
// and a new RFQ sources it.
func TestCloseRFQFromRequisition(t *testing.T) {
	e, req, path, _, cheapest := sourcedQuoted(t)
	rfq := strings.TrimPrefix(path, "/v1/rfqs/")
	t.Run("refused", func(t *testing.T) {
		p := e.in(t).Problem(422, "request.validation_failed", "POST", path+":close", closing(" "), "*")
		if got := errorFields(p); got != "reason" {
			t.Errorf("field errors: got %s, want reason", got)
		}
		e.in(t).Problem(412, "request.version_mismatch", "POST", path+":close", closing("R"), `"1"`)
	})
	_, tag := e.Must(200, "GET", path, "", "")
	r, _ := e.Must(200, "POST", path+":close", closing("All quotations are above the budget"), tag)
	for field, want := range map[string]any{"status": "closed", "closing_reason": "All quotations are above the budget", "purchase_order_id": nil,
		"contract_id": nil, "award_reason": nil, "version": float64(3)} {
		if r[field] != want {
			t.Errorf("RFQ %s: got %v, want %v", field, r[field], want)
		}
	}
	if got, want := e.outcomes(t, path), "Q-1 not_awarded 2, Q-2 not_awarded 2"; got != want {
		t.Errorf("quotations: got %s, want %s", got, want)
	}
	got := mustGet(t, e.env, "/v1/requisitions/"+req)
	for field, want := range map[string]any{"status": "approved", "rfq_id": rfq} {
		if got[field] != want {
			t.Errorf("requisition %s: got %v, want %v", field, got[field], want)
		}
	}
	wantEvent := map[string]any{"requisition_id": req, "document_number": "PR100000", "legal_entity_id": e.DE.String(), "legal_entity_code": "2000",
		"previous_status": "in_sourcing", "status": "approved", "cause": "rfq_closed", "rfq_id": rfq, "rfq_document_number": r["document_number"]}
	if ev := reopened(t, e.env); !reflect.DeepEqual(ev, []map[string]any{wantEvent}) {
		t.Errorf("requisition_reopened events: got %v, want %v", ev, wantEvent)
	}
	if ev := lastChange(t, e.env, rfq); ev["change"] != "closed" || ev["status"] != "closed" || ev["reason"] != "All quotations are above the budget" {
		t.Errorf("rfq_changed event: got %v, want closed with the reason", ev)
	}
	for action, object := range map[string]string{"procurement.rfq.close": rfq, "procurement.requisition.reopened": req} {
		if n := audits(t, e.env, action, object); n != 1 {
			t.Errorf("%s audit records: got %d, want 1", action, n)
		}
	}
	t.Run("ended", func(t *testing.T) {
		for name, c := range map[string]struct{ method, path, body string }{
			"closed again": {"POST", path + ":close", closing("Again")},
			"cancelled":    {"POST", path + ":cancel", closing("Again")},
			"awarded":      {"POST", path + ":award", orderAward(cheapest, "")},
			"quoted":       {"POST", path + "/quotations", quotationBody(e.Supplier, "Q-3", price(1, "4.00", "EUR", ""), price(2, "3.00", "EUR", ""))},
		} {
			t.Run(name, func(t *testing.T) {
				e.in(t).Problem(409, "procurement.invalid_status", c.method, c.path, c.body, "*")
			})
		}
	})
	// The requisition is sourced again: the closed RFQ is not its open one.
	again, _ := e.Must(201, "POST", "/v1/rfqs", e.sourcingBody(req, "", e.Supplier), "")
	if got := mustGet(t, e.env, "/v1/requisitions/"+req); got["status"] != "in_sourcing" || got["rfq_id"] != id(again) {
		t.Errorf("requisition: got %v naming %v, want in_sourcing naming %v", got["status"], got["rfq_id"], id(again))
	}
}

// TestCancelRFQ: a draft is cancelled, not closed; a published RFQ with
// its own items and quotations is cancelled, its quotations not awarded;
// neither has a requisition to reopen.
func TestCancelRFQ(t *testing.T) {
	e := setupIn2026(t)
	draft := "/v1/rfqs/" + id(e.rfq(t, "2026-07-31"))
	e.Problem(409, "procurement.invalid_status", "POST", draft+":close", closing("Not needed"), "*")
	r, _ := e.Must(200, "POST", draft+":cancel", closing("Raised in error"), "*")
	if r["status"] != "cancelled" || r["closing_reason"] != "Raised in error" {
		t.Errorf("draft: got %v with %v, want cancelled with the reason", r["status"], r["closing_reason"])
	}
	e.Problem(409, "procurement.invalid_status", "POST", draft+":publish", "", "*")
	path, _, _ := e.quoted(t)
	r, _ = e.Must(200, "POST", path+":cancel", closing("The project is stopped"), "*")
	if r["status"] != "cancelled" {
		t.Errorf("published: got %v, want cancelled", r["status"])
	}
	if got, want := e.outcomes(t, path), "Q-457 not_awarded 2, ANG-9 not_awarded 2"; got != want {
		t.Errorf("quotations: got %s, want %s", got, want)
	}
	if ev := lastChange(t, e, id(r)); ev["change"] != "cancelled" || ev["reason"] != "The project is stopped" || ev["requisition_id"] != nil {
		t.Errorf("rfq_changed event: got %v, want cancelled with the reason and no requisition", ev)
	}
	if n := len(events(t, e, "requisition_reopened")); n != 0 {
		t.Errorf("requisition_reopened events: got %d, want none", n)
	}
	list, _ := e.Must(200, "GET", "/v1/rfqs?status=cancelled", "", "")
	if got := apitest.List(t, list, "items"); len(got) != 2 {
		t.Errorf("cancelled RFQs: got %d, want 2", len(got))
	}
}

// TestCancelRFQFromRequisition: a draft RFQ from a requisition is
// cancelled; the requisition is approved again and can be cancelled.
func TestCancelRFQFromRequisition(t *testing.T) {
	e := setupIn2026(t)
	req := id(e.approvedRequisition(t))
	r, _ := e.Must(201, "POST", "/v1/rfqs", e.sourcingBody(req, "", e.Supplier), "")
	e.Must(200, "POST", "/v1/rfqs/"+id(r)+":cancel", closing("The requisition is ordered from the catalog instead"), "*")
	if ev := reopened(t, e); len(ev) != 1 || ev[0]["cause"] != "rfq_cancelled" || ev[0]["rfq_id"] != id(r) {
		t.Errorf("requisition_reopened events: got %v, want one by rfq_cancelled naming the RFQ", ev)
	}
	got := e.act(t, "/v1/requisitions/"+req, "cancel", "")
	if got["status"] != "cancelled" {
		t.Errorf("requisition: got %v, want cancelled", got["status"])
	}
}

// TestEndRFQRace: an award and a close of one RFQ at once, each at the
// version read: the RFQ's row lock lets one through, and the other meets
// another version, so the RFQ is either awarded or closed, never both.
func TestEndRFQRace(t *testing.T) {
	e, _, path, _, cheapest := sourcedQuoted(t)
	_, tag := e.Must(200, "GET", path, "", "")
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, c := range []struct{ action, body string }{{"award", orderAward(cheapest, "")}, {"close", closing("Above budget")}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _, _ = e.in(t).Do("POST", path+":"+c.action, c.body, tag)
		}()
	}
	wg.Wait()
	ok := 0
	for _, code := range codes {
		if code == 200 {
			ok++
		} else if code != 412 {
			t.Errorf("the other request: got %d, want 412", code)
		}
	}
	if ok != 1 {
		t.Errorf("requests through: got %d (%v), want 1", ok, codes)
	}
}

// TestEndRFQDatabaseFailures calls the close outside a unit of work and
// with each of its database statements failing in turn; the RFQ is from a
// requisition, so the reopening's statements fail too.
func TestEndRFQDatabaseFailures(t *testing.T) {
	e, _, path, _, _ := sourcedQuoted(t)
	h := endpoints()
	rfq := uuid.MustParse(strings.TrimPrefix(path, "/v1/rfqs/"))
	for name, call := range map[string]func(ctx context.Context) error{
		"close": func(ctx context.Context) error {
			_, err := h.CloseRfq(ctx, &gen.RfqClosing{Reason: "Above budget"}, gen.CloseRfqParams{ID: rfq, IfMatch: "*"})
			return err
		},
		"cancel": func(ctx context.Context) error {
			_, err := h.CancelRfq(ctx, &gen.RfqClosing{Reason: "Not needed"}, gen.CancelRfqParams{ID: rfq, IfMatch: "*"})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name+" RFQ", call)
		})
	}
}
