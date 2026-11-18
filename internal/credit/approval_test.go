package credit_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/credit"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// decide decides the change's pending request as the approver: approve,
// or reject with a reason.
func (e env) decide(t *testing.T, change, decision string) {
	t.Helper()
	r := e.Pending(t, change)
	body := `{"comment": "ok"}`
	if decision == "reject" {
		body = `{"comment": "Not with its payment history."}`
	}
	e.As(e.Approver).In(t).Must(200, "POST", "/v1/approval-requests/"+apitest.String(t, r, "id")+":"+decision, body, fmt.Sprintf(`"%v"`, r["version"]))
}

// shownLimit is a limit as "status amount pending-change", "-" when it
// names none.
func shownLimit(t *testing.T, l map[string]any) string {
	t.Helper()
	pending := "-"
	if _, ok := l["pending_change_id"]; ok {
		pending = "change"
	}
	return fmt.Sprintf("%v %s %s", l["status"], amount(t, l, "limit"), pending)
}

// TestRaiseApproved: a raise waits for its approval, the limit answered
// as it is and the exposure checked against it; its requester cannot
// approve it and a second change waits for the first; approved by the
// controller it holds, audited and published with the change; the
// change reads back for its approver (T77d acceptance).
func TestRaiseApproved(t *testing.T) {
	e := setup(t)
	e.story(t) // Northwind owes 290.00 EUR
	set, tag := e.set(t, limitOf(e.DE, e.north, "500.00", "EUR"))
	id := apitest.String(t, set, "id")
	waiting, next := e.Must(200, "PATCH", "/v1/credit-limits/"+id, `{"limit": {"amount": "900.00", "currency": "EUR"}, "next_review_on": "2027-04-01"}`, tag)
	if got := shownLimit(t, waiting); got != "active 500.00 EUR change" {
		t.Errorf("the raise asked for: got %s, want active 500.00 EUR change", got)
	}
	if next == tag {
		t.Errorf("ETag: got %s again, want it moved: the limit names its change", next)
	}
	changeID := apitest.String(t, waiting, "pending_change_id")
	if c := checked(t, e.exposure(t, e.DE, e.north, "&amount=300.00")); c != "4 290.00 EUR | true 500.00 EUR 210.00 EUR | 300.00 EUR | false" {
		t.Errorf("exposure while the raise waits: got %s, want it checked against 500.00", c)
	}
	t.Run("a second change while the first waits", func(t *testing.T) {
		e.In(t).Problem(409, "credit.change_pending", "PATCH", "/v1/credit-limits/"+id, change("100.00", "EUR"), next)
	})
	t.Run("approved by its requester", func(t *testing.T) {
		r := e.Pending(t, changeID)
		e.In(t).Problem(403, "workflow.own_request", "POST", "/v1/approval-requests/"+apitest.String(t, r, "id")+":approve", `{"comment": "mine"}`,
			fmt.Sprintf(`"%v"`, r["version"]))
	})
	read, _ := e.Must(200, "GET", "/v1/credit-limit-changes/"+changeID, "", "")
	for f, want := range map[string]any{"credit_limit_id": id, "company_code": "2000", "customer_number": "BP70000002", "customer_name": "Northwind Traders",
		"kind": "change", "status": "pending", "next_review_on": "2027-04-01", "requested_by": e.Tenant.Human.ID.String()} {
		if read[f] != want {
			t.Errorf("the change's %s: got %v, want %v", f, read[f], want)
		}
	}
	if got := amount(t, read, "limit") + " / " + amount(t, read, "limit_before"); got != "900.00 EUR / 500.00 EUR" {
		t.Errorf("the change's limit and limit before: got %s, want 900.00 EUR / 500.00 EUR", got)
	}
	e.decide(t, changeID, "approve")
	after, _ := e.Must(200, "GET", "/v1/credit-limits/"+id, "", "")
	if got := shownLimit(t, after) + " " + fmt.Sprint(after["next_review_on"]); got != "active 900.00 EUR - 2027-04-01" {
		t.Errorf("approved: got %s, want active 900.00 EUR - 2027-04-01", got)
	}
	if c := checked(t, e.exposure(t, e.DE, e.north, "&amount=300.00")); c != "4 290.00 EUR | true 900.00 EUR 610.00 EUR | 300.00 EUR | true" {
		t.Errorf("exposure after the approval: got %s, want it checked against 900.00", c)
	}
	applied, _ := e.Must(200, "GET", "/v1/credit-limit-changes/"+changeID, "", "")
	if _, decided := applied["decided_at"]; applied["status"] != "applied" || !decided {
		t.Errorf("the change after its approval: got %v, decided_at %v, want applied with its time", applied["status"], applied["decided_at"])
	}
	events := e.changed(t)
	if last := events[len(events)-1]; fmt.Sprintf("%v %v", apitest.Object(t, last, "limit")["amount"], last["credit_limit_change_id"]) != "900.00 "+changeID {
		t.Errorf("CreditLimitChanged of the raise: got %v, want 900.00 with its change", last)
	}
	var audits []string
	pgtest.Scan(t, `SELECT coalesce(array_agg(action ORDER BY seq), '{}') FROM audit.audit_event WHERE tenant_id = $1 AND object_id = $2`,
		[]any{e.Tenant.ID, changeID}, &audits)
	if got := strings.Join(audits, " "); got != "credit.credit_limit_change.request credit.credit_limit_change.apply" {
		t.Errorf("the change's audit records: got %s, want request and apply", got)
	}
	t.Run("applied again", func(t *testing.T) {
		err := apitest.InRequest(t, e.Tenant, e.Approver, func(ctx context.Context) error {
			return credit.Approvals{Endpoints: endpoints(t)}.Approved(ctx, uuid.MustParse(changeID), uuid.Nil)
		})
		if p := problem.From(err); p.Code != "credit.invalid_status" {
			t.Errorf("got %v, want credit.invalid_status", err)
		}
	})
}

// TestWhatWaits: a lowering, a new review date and both together hold at
// once; a raise, a change of the validity and a lowering with one wait
// for their approval; a rejection or a withdrawal leaves the limit as it
// is, with its version moved (T77d acceptance).
func TestWhatWaits(t *testing.T) {
	e := setup(t)
	set, tag := e.set(t, limitOf(e.DE, e.north, "500.00", "EUR"))
	id := apitest.String(t, set, "id")
	for _, c := range []struct{ name, body, want string }{
		{"a lowering", change("400.00", "EUR"), "active 400.00 EUR -"},
		{"a review date", `{"next_review_on": "2027-01-15"}`, "active 400.00 EUR -"},
		{"a lowering with a review date", `{"limit": {"amount": "300.00", "currency": "EUR"}, "next_review_on": null}`, "active 300.00 EUR -"},
		{"a raise", change("300.01", "EUR"), "active 300.00 EUR change"},
		{"an end of validity", `{"valid_to": "2027-12-31"}`, "active 300.00 EUR change"},
		{"a start of validity", `{"valid_from": "2026-01-01"}`, "active 300.00 EUR change"},
		{"a lowering with a validity", `{"limit": {"amount": "1.00", "currency": "EUR"}, "valid_to": "2027-12-31"}`, "active 300.00 EUR change"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, next := e.In(t).Must(200, "PATCH", "/v1/credit-limits/"+id, c.body, tag)
			tag = next
			if s := shownLimit(t, got); s != c.want {
				t.Errorf("got %s, want %s", s, c.want)
			}
			if change, ok := got["pending_change_id"].(string); ok {
				e.decide(t, change, "reject")
				after, afterTag := e.In(t).Must(200, "GET", "/v1/credit-limits/"+id, "", "")
				if s := shownLimit(t, after); s != "active 300.00 EUR -" || afterTag == tag {
					t.Errorf("rejected: got %s at %s, want active 300.00 EUR - at a version after %s", s, afterTag, tag)
				}
				tag = afterTag
			}
		})
	}
	t.Run("withdrawn by its requester", func(t *testing.T) {
		got, _ := e.In(t).Must(200, "PATCH", "/v1/credit-limits/"+id, change("800.00", "EUR"), tag)
		r := e.Pending(t, apitest.String(t, got, "pending_change_id"))
		e.In(t).Must(200, "POST", "/v1/approval-requests/"+apitest.String(t, r, "id")+":cancel", `{"comment": "Asked too soon."}`, fmt.Sprintf(`"%v"`, r["version"]))
		after, _ := e.In(t).Must(200, "GET", "/v1/credit-limits/"+id, "", "")
		if s := shownLimit(t, after); s != "active 300.00 EUR -" {
			t.Errorf("got %s, want active 300.00 EUR -", s)
		}
		cancelled, _ := e.In(t).Must(200, "GET", "/v1/credit-limit-changes?status=cancelled&credit_limit_id="+id, "", "")
		if n := len(objects(t, cancelled, "items")); n != 1 {
			t.Errorf("cancelled changes: got %d, want 1", n)
		}
	})
	t.Run("paged", func(t *testing.T) {
		first, _ := e.In(t).Must(200, "GET", "/v1/credit-limit-changes?page_size=1&credit_limit_id="+id, "", "")
		second, _ := e.In(t).Must(200, "GET", "/v1/credit-limit-changes?page_size=1&credit_limit_id="+id+"&page_token="+apitest.String(t, first, "next_page_token"), "", "")
		a, b := objects(t, first, "items"), objects(t, second, "items")
		if len(a) != 1 || len(b) != 1 || a[0]["id"] == b[0]["id"] {
			t.Errorf("pages: got %v and %v, want one change each, the second older", a, b)
		}
	})
	all, _ := e.Must(200, "GET", "/v1/credit-limit-changes?credit_limit_id="+id, "", "")
	var statuses []string
	for _, c := range objects(t, all, "items") {
		statuses = append(statuses, fmt.Sprintf("%v %v", c["kind"], c["status"]))
	}
	if got := strings.Join(statuses, ", "); got != "change cancelled, change rejected, change rejected, change rejected, change rejected, create applied" {
		t.Errorf("the limit's changes, newest first: got %s", got)
	}
}

// TestPendingLimit: a limit set waits for its approval and is not in
// effect meanwhile: the exposure and the check treat the customer as one
// without a limit, naming the limit; rejected, it stays pending and is
// asked for again, its values as they are or changed (T77d acceptance).
func TestPendingLimit(t *testing.T) {
	e := setup(t)
	e.story(t)
	set, _ := e.Must(201, "POST", "/v1/credit-limits", limitOf(e.DE, e.north, "100.00", "EUR"), "")
	id := apitest.String(t, set, "id")
	x := e.exposure(t, e.DE, e.north, "&amount=1000.00")
	if c := checked(t, x); c != "4 290.00 EUR | false - - | 1000.00 EUR | true" || x["credit_limit_id"] != id {
		t.Errorf("exposure of a pending limit: got %s naming %v, want no limit in effect, naming %s", c, x["credit_limit_id"], id)
	}
	var checkedLimit *credit.Limit
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		x, err := endpoints(t).Check(ctx, e.DE, e.north, 100000)
		checkedLimit = x.Limit
		return err
	}); err != nil || checkedLimit != nil {
		t.Errorf("the check of a pending limit: got %v, %v, want no limit used", checkedLimit, err)
	}
	e.decide(t, apitest.String(t, set, "pending_change_id"), "reject")
	rejected, tag := e.Must(200, "GET", "/v1/credit-limits/"+id, "", "")
	if s := shownLimit(t, rejected); s != "pending 100.00 EUR -" {
		t.Errorf("rejected: got %s, want pending 100.00 EUR -", s)
	}
	again, _ := e.Must(200, "PATCH", "/v1/credit-limits/"+id, change("100.00", "EUR"), tag)
	again, _ = e.approved(t, again)
	if s := shownLimit(t, again); s != "active 100.00 EUR -" {
		t.Errorf("asked again and approved: got %s, want active 100.00 EUR -", s)
	}
	creates, _ := e.Must(200, "GET", "/v1/credit-limit-changes?status=applied&credit_limit_id="+id, "", "")
	if items := objects(t, creates, "items"); len(items) != 1 || items[0]["kind"] != "create" {
		t.Errorf("the change applied: got %v, want one create", items)
	}
	t.Run("a change of another company's limit read", func(t *testing.T) {
		other := e.As(e.Reader(t, "A person of company 1000", e.US))
		other.In(t).Problem(404, "request.not_found", "GET", "/v1/credit-limit-changes/"+apitest.String(t, set, "pending_change_id"), "", "")
		page, _ := other.In(t).Must(200, "GET", "/v1/credit-limit-changes", "", "")
		if n := len(objects(t, page, "items")); n != 0 {
			t.Errorf("another company's changes listed: got %d, want 0", n)
		}
	})
	t.Run("a change that does not exist", func(t *testing.T) {
		e.In(t).Problem(404, "request.not_found", "GET", "/v1/credit-limit-changes/"+uuid.NewString(), "", "")
	})
}

// TestApprovalWaitsForACheck: applying a change takes the customer's key
// and the limit's row as a check of its orders does, in that order, so an
// approval waits for a confirmation under way, and the next check sees
// the limit approved (ADR-0008; T77d).
func TestApprovalWaitsForACheck(t *testing.T) {
	e := setup(t)
	set, tag := e.set(t, limitOf(e.DE, e.north, "100.00", "EUR"))
	waiting, _ := e.Must(200, "PATCH", "/v1/credit-limits/"+apitest.String(t, set, "id"), change("900.00", "EUR"), tag)
	change := uuid.MustParse(apitest.String(t, waiting, "pending_change_id"))
	h := endpoints(t)
	check := func(ctx context.Context) error {
		_, err := h.Check(ctx, e.DE, e.north, 100)
		return err
	}
	apply := func(ctx context.Context) error { return credit.Approvals{Endpoints: h}.Approved(ctx, change, uuid.Nil) }
	checked, applied := apitest.Serialized(t, e.Tenant, e.Approver, check, apply)
	if checked != nil {
		t.Errorf("the check: got %v, want it answered", checked)
	}
	if applied != nil {
		t.Errorf("the approval: got %v, want it applied once the check ended", applied)
	}
}

// noApproval is an approval policy that asks none: a credit limit change
// always needs one, so it is refused rather than applied unapproved.
type noApproval struct{}

func (noApproval) Open(context.Context, approvals.NewRequest) (uuid.UUID, bool, error) {
	return uuid.Nil, false, nil
}

// TestNoApprovalAsked: a policy that asks no approval of a credit limit
// change refuses it, so nothing applies unapproved.
func TestNoApprovalAsked(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	h.Approvals = noApproval{}
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.SetLimit(ctx, credit.LimitInput{LegalEntityID: e.DE, CustomerID: e.north, Amount: "100.00", Currency: "EUR"})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "asks no approval") {
		t.Errorf("got %v, want the change refused", err)
	}
}
