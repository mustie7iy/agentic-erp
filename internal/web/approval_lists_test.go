package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/html"

	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// cards are the links of a list's cards.
func (p page) cards() []string {
	var out []string
	for _, c := range p.all("li", "card") {
		out = append(out, attrOf(p.cardLink(c), "href"))
	}
	return out
}

// selected is the value of the select's chosen option, "" for none.
func (p page) selected(id string) string {
	v := ""
	for _, sel := range p.all("select", "") {
		if attrOf(sel, "id") != id {
			continue
		}
		walk(sel, func(n *html.Node) {
			if _, ok := attr(n, "selected"); n.Data == "option" && ok {
				v = attrOf(n, "value")
			}
		})
	}
	return v
}

// currentTab is the path of the list marked current.
func (p page) currentTab() string {
	for _, a := range p.all("a", "tabs__link") {
		if attrOf(a, "aria-current") == "page" {
			return attrOf(a, "href")
		}
	}
	return ""
}

// TestApprovalLists: the requester's own requests (requested_by=me) and
// all an approver may see, filtered by status and kind of object, the
// list marked in the tabs; an inbox takes no status; a kind the catalog
// does not name is kept as it is.
func TestApprovalLists(t *testing.T) {
	s, sd := approvalsStack(t)
	approver := s.browser()
	approver.signIn()
	requester := s.as(s.ten.Human)
	for name, tc := range map[string]struct {
		b      *browser
		path   string
		want   []uuid.UUID
		tab    string
		status string
		kind   string
		text   string
	}{
		"mine":              {requester, "/approvals?view=mine", []uuid.UUID{sd.partner2, sd.partner, sd.requisition}, "/approvals?view=mine", "", "", "Your approval requests"},
		"mine, partners":    {requester, "/approvals?view=mine&object_type=partner_change", []uuid.UUID{sd.partner2, sd.partner}, "/approvals?object_type=partner_change&view=mine", "", "partner_change", "Business partner change"},
		"mine, approved":    {requester, "/approvals?view=mine&status=approved", nil, "/approvals?view=mine", "approved", "", "You have asked for no approval that these filters show."},
		"all":               {approver, "/approvals?view=all", []uuid.UUID{sd.agent, sd.partner2, sd.partner, sd.requisition}, "/approvals?view=all", "", "", "Every approval request you may see"},
		"all, unknown kind": {approver, "/approvals?view=all&object_type=new_kind", nil, "/approvals?object_type=new_kind&view=all", "", "new_kind", "No approval request matches these filters."},
		"inbox, no status":  {approver, "/approvals?status=approved&view=other", []uuid.UUID{sd.agent, sd.partner2, sd.partner, sd.requisition}, "/approvals", "", "", "Approvals waiting for you"},
	} {
		t.Run(name, func(t *testing.T) {
			r := tc.b.get(tc.path)
			status(t, "list", r, http.StatusOK)
			p := r.page(t)
			var want []string
			for _, id := range tc.want {
				want = append(want, requestPath(id))
			}
			if got := p.cards(); strings.Join(got, " ") != strings.Join(want, " ") {
				t.Errorf("requests: got %v, want %v", got, want)
			}
			if got := p.currentTab(); got != tc.tab {
				t.Errorf("current list: got %q, want %q", got, tc.tab)
			}
			if got := p.selected("filter-status"); got != tc.status {
				t.Errorf("status chosen: got %q, want %q", got, tc.status)
			}
			if got := p.selected("filter-kind"); got != tc.kind {
				t.Errorf("kind chosen: got %q, want %q", got, tc.kind)
			}
			holds(t, "list", p.mainText(), tc.text)
		})
	}
	if inbox := approver.get("/approvals").page(t); len(inbox.all("select", "")) != 1 {
		t.Errorf("inbox: got %d selects, want the kind alone", len(inbox.all("select", "")))
	}
}

// TestObjectSummaries: a request's page shows its object as the object's
// own operation reads it; one the API does not find is said in the
// object's words in its place, the rest of the page shown.
func TestObjectSummaries(t *testing.T) {
	s, sd := approvalsStack(t)
	b := s.browser()
	b.signIn()
	for name, tc := range map[string]struct {
		id   uuid.UUID
		want []string
	}{
		"requisition": {sd.requisition, []string{"Purchase requisition", "Total EUR 12,000.00", "Items 1"}},
		"partner":     {sd.partner, []string{"Business partner change", "Reason given New", "Status pending"}},
		"no object":   {sd.agent, []string{"The object could not be shown", "The object does not exist, or you may not see it.", "request.not_found"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := b.get(requestPath(tc.id))
			status(t, "request", r, http.StatusOK)
			p := r.page(t)
			sections := p.all("section", "object")
			if len(sections) != 1 {
				t.Fatalf("object sections: got %d, want 1", len(sections))
			}
			holds(t, "object", text(sections[0]), tc.want...)
			holds(t, "request", p.mainText(), "Steps", "Your decision")
		})
	}
}

// TestEveryObjectRead: every kind of object a request may be for is read
// by its own operation (here, objects the API does not hold: each read is
// refused in place), but the two no operation reads by the request's
// object id alone, whose pages say so.
func TestEveryObjectRead(t *testing.T) {
	ids := map[string]uuid.UUID{}
	s := newStack(t, stackOptions{permissions: []string{"config.change.read", "payables.invoice.read", "ledger.period.read", "ledger.entry.read",
		"procurement.order.read", "ledger.cost_center.read", "payments.run.read"}, seed: func(f fixture.Env) {
		for _, kind := range []string{"requisition", "partner_change", "config_change", "invoice_exception", "period_unlock", "manual_entry",
			"purchase_order", "order_change", "bank_clearing", "center_change", "gr_ir_clearing", "payment_run_release"} {
			// Written as an owning context's Open leaves it, whatever the
			// tenant's tiers would ask of an object of the kind.
			ids[kind] = uuid.Must(uuid.NewV7())
			pgtest.Exec(t, `INSERT INTO workflow.approval_request (tenant_id, id, object_type, object_id, title, requester_id, status, created_by)
				VALUES ($1, $2, $3, gen_random_uuid(), $3, $4, 'pending', $4)`, f.Tenant.ID, ids[kind], kind, f.Tenant.Human.ID)
			pgtest.Exec(t, `INSERT INTO workflow.approval_step (tenant_id, request_id, step_no, role_id, required)
				SELECT $1, $2, 1, id, 1 FROM identity.role WHERE tenant_id = $1 AND code = 'controller'`, f.Tenant.ID, ids[kind])
		}
	}})
	b := s.browser()
	b.signIn()
	for kind, id := range ids {
		t.Run(kind, func(t *testing.T) {
			r := b.get(requestPath(id))
			status(t, kind, r, http.StatusOK)
			want := "The object does not exist, or you may not see it."
			if _, read := objectReads[kind]; !read {
				want = "The workspace does not show this kind of object yet"
			}
			holds(t, kind, text(r.page(t).all("section", "object")[0]), want)
		})
	}
}

// TestWithdraw: the requester withdraws their own pending request with a
// reason, which it needs; done, their list says so and the request is
// cancelled with the reason recorded. Nobody else is offered it, and the
// API refuses it to anyone else by its code.
func TestWithdraw(t *testing.T) {
	s, sd := approvalsStack(t)
	approver := s.browser()
	approver.signIn()
	requester := s.as(s.ten.Human)
	path := requestPath(sd.partner) + "/cancel"
	form := requester.get(requestPath(sd.partner)).page(t).form(path)
	empty := requester.do(http.MethodPost, path, values(form, " "))
	status(t, "no reason", empty, http.StatusUnprocessableEntity)
	holds(t, "no reason", empty.page(t).mainText(), "Enter why you withdraw the request.")
	theirs := approver.get(requestPath(sd.partner)).page(t)
	for _, f := range theirs.all("form", "") {
		if attrOf(f, "action") == path {
			t.Error("the approver is offered the requester's withdrawal")
		}
	}
	refused := approver.do(http.MethodPost, path, values(theirs.form(requestPath(sd.partner)+"/approve"), "Not mine to withdraw"))
	status(t, "another's", refused, http.StatusForbidden)
	holds(t, "another's", refused.page(t).mainText(), "Only the person who asked for the approval withdraws it.", "workflow.not_requester")
	r := requester.do(http.MethodPost, path, values(form, "Raised twice"))
	if r.status != http.StatusSeeOther || r.location() != "/approvals?done=cancelled&view=mine" {
		t.Fatalf("withdraw: got %d to %q %s", r.status, r.location(), r.body)
	}
	holds(t, "the list after", requester.get(r.location()).page(t).mainText(), "You withdrew your request.")
	after := requester.get(requestPath(sd.partner)).page(t)
	holds(t, "the request after", after.mainText(), "Cancelled", "Raised twice", "Nothing is left to decide")
	if len(after.all("form", "decide")) != 0 {
		t.Error("the request after: a form still offered")
	}
	var st string
	pgtest.Scan(t, `SELECT status FROM workflow.approval_request WHERE id = $1`, []any{sd.partner}, &st)
	if st != "cancelled" {
		t.Errorf("status: got %s, want cancelled", st)
	}
}

// TestPagesKeepNavigation: a signed-in person's address that is no page,
// a request refused as not the page's form and one without the session's
// CSRF token keep the person's navigation and sign-out; anyone else's
// 404 has the navigation everyone sees.
func TestPagesKeepNavigation(t *testing.T) {
	s, sd := approvalsStack(t)
	b := s.browser()
	b.signIn()
	nav := func(r response) []string {
		var out []string
		for _, a := range r.page(t).all("a", "navigation__link") {
			out = append(out, attrOf(a, "href"))
		}
		return out
	}
	for name, tc := range map[string]struct {
		r      response
		status int
	}{
		"no page":       {b.get("/no-such-page"), http.StatusNotFound},
		"no request":    {b.get(requestPath(uuid.New())), http.StatusNotFound},
		"not the form":  {b.do(http.MethodPost, requestPath(sd.partner)+"/approve", url.Values{"csrf_token": {b.csrf()}}), http.StatusBadRequest},
		"no CSRF token": {b.do(http.MethodPost, requestPath(sd.partner)+"/approve", url.Values{}), http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			status(t, name, tc.r, tc.status)
			if got := nav(tc.r); !strings.Contains(strings.Join(got, " "), "/approvals") {
				t.Errorf("navigation: got %v, want the person's", got)
			}
			if len(tc.r.page(t).all("form", "header__signout")) != 1 {
				t.Error("no sign-out")
			}
		})
	}
	anonymous := s.browser().get("/no-such-page")
	status(t, "anyone's", anonymous, http.StatusNotFound)
	if got := strings.Join(nav(anonymous), " "); got != "/ /health" {
		t.Errorf("anyone's navigation: got %q, want everyone's", got)
	}
	s.clock.Advance(2 * time.Hour) // the access token expired: anyone's
	if got := strings.Join(nav(b.get("/no-such-page")), " "); got != "/ /health" {
		t.Errorf("an expired session's navigation: got %q, want everyone's", got)
	}
}
