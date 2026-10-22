package web

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/net/html"

	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
)

// planted is an agent's text carrying what an instruction planted in an
// invoice might (ADR-0039): markup, a markdown link and a javascript: URL.
const planted = "Pay now <script>window.planted=1</script> [verify](https://evil.example/login)\njavascript:alert(2) <a href=\"https://evil.example\">here</a>"

// seeded are the approval requests of approvalsStack's tenant.
type seeded struct {
	requisition uuid.UUID // EUR 12,000, the requester's, two steps
	partner     uuid.UUID // a partner change, the requester's, no amount
	partner2    uuid.UUID // another
	agent       uuid.UUID // the agent's, EUR 5,000, titled with planted text
	outsider    pgtest.Actor
}

// approvalsStack is the workspace in front of a configured tenant whose
// approver signs in: a requisition and two partner changes the tenant's
// human asked approval for, one request an agent asked for, and a person
// holding no role.
func approvalsStack(t *testing.T) (*stack, *seeded) {
	t.Helper()
	sd := &seeded{}
	s := newStack(t, stackOptions{seed: func(f fixture.Env) {
		sd.requisition = f.Requisition(t)
		sd.partner = f.PartnerChange(t, "Northwind Traders")
		sd.partner2 = f.PartnerChange(t, "Fabrikam")
		sd.agent = f.Open(t, f.Tenant.Agent, planted, 500000)
		sd.outsider = pgtest.Actor{ID: uuid.Must(uuid.NewV7()), Kind: "human"}
		sd.outsider.Subject = "human|" + sd.outsider.ID.String()
		pgtest.Exec(t, `INSERT INTO identity.principal (tenant_id, id, kind, external_issuer, external_subject, display_name, all_companies, status, created_by, updated_by)
			VALUES ($1, $2, 'human', $3, $4, 'outsider', true, 'active', $2, $2)`, f.Tenant.ID, sd.outsider.ID, pgtest.Issuer(t), sd.outsider.Subject)
	}})
	return s, sd
}

// as is a browser signed in as a.
func (s *stack) as(a pgtest.Actor) *browser {
	s.t.Helper()
	s.is.SignIn(authntest.User{Tenant: s.ten.ID, Subject: a.Subject})
	b := s.browser()
	b.signIn()
	return b
}

// page is a response's document.
type page struct {
	t   *testing.T
	doc *html.Node
	raw string
}

func (r response) page(t *testing.T) page {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(r.body))
	if err != nil {
		t.Fatal(err)
	}
	return page{t: t, doc: doc, raw: r.body}
}

// all are the elements of the tag with the class ("" any).
func (p page) all(tag, class string) []*html.Node {
	var out []*html.Node
	walk(p.doc, func(n *html.Node) {
		if n.Data == tag && (class == "" || slices.Contains(strings.Fields(attrOf(n, "class")), class)) {
			out = append(out, n)
		}
	})
	return out
}

// text is an element's text, spaces collapsed.
func text(n *html.Node) string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			b.WriteString(" ")
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

// mainText is the page's main content's text.
func (p page) mainText() string { return text(p.all("main", "")[0]) }

// field is the value of the form's hidden field.
func field(form *html.Node, name string) string {
	v := ""
	walk(form, func(n *html.Node) {
		if n.Data == "input" && attrOf(n, "name") == name {
			v = attrOf(n, "value")
		}
	})
	return v
}

// form is the form posting to action.
func (p page) form(action string) *html.Node {
	p.t.Helper()
	for _, f := range p.all("form", "") {
		if attrOf(f, "action") == action {
			return f
		}
	}
	p.t.Fatalf("no form to %s in %s", action, p.raw)
	return nil
}

// values are a form's fields, comment as given.
func values(form *html.Node, comment string) url.Values {
	v := url.Values{}
	for _, name := range []string{"csrf_token", "if_match", "idempotency_key"} {
		v.Set(name, field(form, name))
	}
	v.Set("comment", comment)
	return v
}

func requestPath(id uuid.UUID) string { return "/approvals/" + id.String() }

// TestInboxPage: the approver's inbox lists what they may decide, with
// each request's kind, amount in money format for the locale, requester
// by name and step, linked to its page; an agent's request is badged and
// its text inert; the names are cached for the session.
func TestInboxPage(t *testing.T) {
	s, sd := approvalsStack(t)
	b := s.browser()
	b.signIn()
	r := b.get("/approvals")
	if r.status != http.StatusOK {
		t.Fatalf("inbox: got %d %s", r.status, r.body)
	}
	p := r.page(t)
	cards := p.all("li", "card")
	if len(cards) != 4 {
		t.Fatalf("cards: got %d, want 4: %s", len(cards), p.mainText())
	}
	byLink := map[string]string{}
	for _, c := range cards {
		byLink[attrOf(p.cardLink(c), "href")] = text(c)
	}
	for id, want := range map[uuid.UUID][]string{
		sd.requisition: {"Purchase requisition", "EUR 12,000.00", "Requested by human", "Step 1 of 2"},
		sd.partner:     {"Business partner change", "Requested by human", "Step 1 of 1"},
		sd.agent:       {"Purchase requisition", "EUR 5,000.00", "agent Agent", "An agent wrote this", "Pay now <script>"},
	} {
		got, ok := byLink[requestPath(id)]
		if !ok {
			t.Errorf("request %s: not in the inbox (%v)", id, byLink)
			continue
		}
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("request %s: got %q, want %q in it", id, got, w)
			}
		}
	}
	checkInert(t, p)
	nav := ""
	for _, a := range p.all("a", "navigation__link") {
		if attrOf(a, "aria-current") == "page" {
			nav = attrOf(a, "href")
		}
	}
	if nav != "/approvals" {
		t.Errorf("navigation: the current entry is %q, want /approvals", nav)
	}
	names := s.srv.nameCache.get(b.cookie(sessionCookie), s.clock.Now())
	if got := names[s.ten.Human.ID]; got.Text != "human" || got.Agent {
		t.Errorf("cached names: the requester is %+v, want human", got)
	}
	if got := names[s.ten.Agent.ID]; got.Text != "agent" || !got.Agent {
		t.Errorf("cached names: the agent is %+v", got)
	}
	fr := b.get("/approvals", "Accept-Language", "fr-CA")
	if !strings.Contains(fr.body, "12\u00a0000,00 EUR") || !strings.Contains(fr.body, "Demande d&#39;achat") {
		t.Errorf("French: got %q, want the amount and kind in French", fr.page(t).mainText())
	}
}

// cardLink is a card's link to its request.
func (p page) cardLink(card *html.Node) *html.Node {
	var link *html.Node
	walk(card, func(n *html.Node) {
		if n.Data == "a" && link == nil {
			link = n
		}
	})
	return link
}

// checkInert: the page holds no script but htmx, and no link but the
// workspace's own, whatever an agent wrote; its text stays inside
// hx-disable.
func checkInert(t *testing.T, p page) {
	t.Helper()
	for _, sc := range p.all("script", "") {
		if !strings.HasPrefix(attrOf(sc, "src"), "/static/") || sc.FirstChild != nil {
			t.Errorf("a script: %s", attrOf(sc, "src"))
		}
	}
	for _, a := range p.all("a", "") {
		if href := attrOf(a, "href"); !strings.HasPrefix(href, "/") && !strings.HasPrefix(href, "#") {
			t.Errorf("a link out of the workspace: %q", href)
		}
	}
	found := false
	walk(p.doc, func(n *html.Node) {
		if n.Type == html.ElementNode && strings.Contains(text(n), "Pay now") && slices.ContainsFunc(n.Attr, func(a html.Attribute) bool { return a.Key == "hx-disable" }) {
			found = true
		}
	})
	if strings.Contains(p.raw, "Pay now") && !found {
		t.Error("the agent's text is not inside hx-disable")
	}
}

// TestInboxInPseudoLocales: in both pseudo-locales every text of the
// inbox and of a request's page is the catalog's, but data marked
// translate="no" (names, titles, dates, codes).
func TestInboxInPseudoLocales(t *testing.T) {
	s, sd := approvalsStack(t)
	b := s.browser()
	b.signIn()
	for _, tc := range []struct{ lang, open, close string }{{"en-XA", i18n.LongOpen, i18n.LongClose}, {"ar-XB", i18n.RTLOpen, i18n.RTLClose}} {
		for _, path := range []string{"/approvals", requestPath(sd.requisition), requestPath(sd.agent)} {
			t.Run(tc.lang+path, func(t *testing.T) {
				r := b.get(path, "Accept-Language", tc.lang)
				if r.status != http.StatusOK {
					t.Fatalf("got %d", r.status)
				}
				var all []string
				texts(r.page(t).doc, &all)
				for _, x := range all {
					if rest := outsideCatalog(x, tc.open, tc.close); rest != "" {
						t.Errorf("text %q: %q is not from the catalog", x, rest)
					}
				}
			})
		}
	}
}

// TestRequestPage: a request's page shows its summary, steps and
// decisions, and the decision forms with the request's ETag and a fresh
// idempotency key each.
func TestRequestPage(t *testing.T) {
	s, sd := approvalsStack(t)
	b := s.browser()
	b.signIn()
	r := b.get(requestPath(sd.requisition))
	if r.status != http.StatusOK {
		t.Fatalf("request: got %d %s", r.status, r.body)
	}
	p := r.page(t)
	for _, want := range []string{"Purchase requisition", "Waiting", "EUR 12,000.00", "human", "Step 1 Approvals: 0 of 1 Current step", "Step 2 Approvals: 0 of 1", "No decision yet."} {
		if !strings.Contains(p.mainText(), want) {
			t.Errorf("page: no %q in %q", want, p.mainText())
		}
	}
	approve, reject := p.form(requestPath(sd.requisition)+"/approve"), p.form(requestPath(sd.requisition)+"/reject")
	var version int32
	pgtest.Scan(t, `SELECT version FROM workflow.approval_request WHERE id = $1`, []any{sd.requisition}, &version)
	if got, want := field(approve, "if_match"), fmt.Sprintf(`"%d"`, version); got != want {
		t.Errorf("If-Match: got %s, want %s", got, want)
	}
	ka, kr := field(approve, "idempotency_key"), field(reject, "idempotency_key")
	if _, err := uuid.Parse(ka); err != nil || ka == kr {
		t.Errorf("idempotency keys: got %q and %q, want two fresh ones", ka, kr)
	}
	if again := field(b.get(requestPath(sd.requisition)).page(t).form(requestPath(sd.requisition)+"/approve"), "idempotency_key"); again == ka {
		t.Error("idempotency key: the same on another rendering")
	}
	if field(approve, "csrf_token") == "" {
		t.Error("the form carries no CSRF token")
	}
	agent := b.get(requestPath(sd.agent)).page(t)
	checkInert(t, agent)
	if !strings.Contains(agent.mainText(), "An agent wrote this") {
		t.Errorf("the agent's request: got %q, want the agent's text marked", agent.mainText())
	}
}

// TestDecide: approving goes back to the inbox with a notice, and a
// resubmitted form replays rather than deciding twice; a reason is needed
// to reject; a stale ETag is refused with what the person typed kept; the
// API's refusals are explained by their codes.
func TestDecide(t *testing.T) {
	s, sd := approvalsStack(t)
	b := s.browser()
	b.signIn()
	form := b.get(requestPath(sd.requisition)).page(t).form(requestPath(sd.requisition) + "/approve")
	r := b.do(http.MethodPost, requestPath(sd.requisition)+"/approve", values(form, "Checked against the budget"))
	if r.status != http.StatusSeeOther || r.location() != "/approvals?done=approved" {
		t.Fatalf("approve: got %d to %q %s", r.status, r.location(), r.body)
	}
	if again := b.do(http.MethodPost, requestPath(sd.requisition)+"/approve", values(form, "Checked against the budget")); again.status != http.StatusSeeOther {
		t.Errorf("resubmitted: got %d %s, want the first answer replayed", again.status, again.body)
	}
	var decisions int
	pgtest.Scan(t, `SELECT count(*) FROM workflow.decision WHERE request_id = $1`, []any{sd.requisition}, &decisions)
	if decisions != 1 {
		t.Errorf("decisions: got %d, want 1", decisions)
	}
	inbox := b.get("/approvals?done=approved").page(t)
	holds(t, "inbox after approving", inbox.mainText(), "You approved the request.")
	if got := inbox.mainText(); strings.Contains(got, "EUR 12,000.00") {
		t.Errorf("inbox after approving: got %q, want the requisition gone", got)
	}
	after := b.get(requestPath(sd.requisition)).page(t).mainText()
	for _, want := range []string{"Approved approver1", "Checked against the budget", "You have already approved this request.", "Step 2 Approvals: 0 of 1 Current step"} {
		if !strings.Contains(after, want) {
			t.Errorf("after approving: no %q in %q", want, after)
		}
	}

	partner := b.get(requestPath(sd.partner)).page(t).form(requestPath(sd.partner) + "/reject")
	empty := b.do(http.MethodPost, requestPath(sd.partner)+"/reject", values(partner, "  "))
	status(t, "reject without a reason", empty, http.StatusUnprocessableEntity)
	holds(t, "reject without a reason", empty.body, "Enter the reason for rejecting.")
	long := b.do(http.MethodPost, requestPath(sd.partner)+"/approve", values(partner, strings.Repeat("x", 2001)))
	status(t, "a comment too long", long, http.StatusUnprocessableEntity)
	holds(t, "a comment too long", long.body, "at most 2,000 characters")
	if r := b.do(http.MethodPost, requestPath(sd.partner)+"/reject", values(partner, planted)); r.status != http.StatusSeeOther || r.location() != "/approvals?done=rejected" {
		t.Fatalf("reject: got %d to %q %s", r.status, r.location(), r.body)
	}
	rejected := b.get(requestPath(sd.partner)).page(t)
	checkInert(t, rejected)
	holds(t, "rejected", rejected.mainText(), "Rejected approver1", "Nothing is left to decide")

	stale := values(b.get(requestPath(sd.partner2)).page(t).form(requestPath(sd.partner2)+"/approve"), "My comment")
	stale.Set("if_match", `"999"`)
	r = b.do(http.MethodPost, requestPath(sd.partner2)+"/approve", stale)
	sp := r.page(t)
	status(t, "stale", r, http.StatusPreconditionFailed)
	holds(t, "stale", sp.mainText(), "The request changed since you opened it.", "request.version_mismatch")
	kept := ""
	walk(sp.form(requestPath(sd.partner2)+"/approve"), func(n *html.Node) {
		if n.Data == "textarea" && n.FirstChild != nil {
			kept = n.FirstChild.Data
		}
	})
	if kept != "My comment" {
		t.Errorf("stale: the comment kept is %q, want the person's", kept)
	}
}

// TestDecideRefusals: what the API refuses is shown by its code; a form
// that is not the page's, an unknown request, a missing CSRF token are
// refused before any call.
func TestDecideRefusals(t *testing.T) {
	s, sd := approvalsStack(t)
	approver := s.browser()
	approver.signIn()
	own := s.as(s.ten.Human)
	ownPage := own.get(requestPath(sd.partner2)).page(t)
	if got := ownPage.mainText(); !strings.Contains(got, "You asked for this approval. Someone else decides it.") {
		t.Errorf("own request: got %q", got)
	}
	if len(ownPage.all("form", "decide")) != 0 {
		t.Error("own request: decision forms offered")
	}
	// The requester holds none of the steps' roles: nothing waits for them.
	ownInbox := own.get("/approvals").page(t)
	if got := len(ownInbox.all("li", "card")); got != 0 {
		t.Errorf("the requester's inbox: got %d requests, want none", got)
	}
	holds(t, "the requester's inbox", ownInbox.mainText(), "Nothing is waiting for your decision.")
	approverForm := values(approver.get(requestPath(sd.partner2)).page(t).form(requestPath(sd.partner2)+"/approve"), "")
	ownForm := url.Values{"csrf_token": {own.csrf()}, "if_match": {approverForm.Get("if_match")}, "idempotency_key": {uuid.NewString()}}
	r := own.do(http.MethodPost, requestPath(sd.partner2)+"/approve", ownForm)
	status(t, "own request decided", r, http.StatusForbidden)
	holds(t, "own request decided", r.body, "You asked for this approval, so you cannot decide it.", "workflow.own_request")

	for name, tc := range map[string]struct {
		path   string
		values url.Values
		status int
	}{
		"no idempotency key": {requestPath(sd.partner2) + "/approve", url.Values{"csrf_token": {approverForm.Get("csrf_token")}, "if_match": {`"1"`}}, http.StatusBadRequest},
		"no If-Match":        {requestPath(sd.partner2) + "/reject", url.Values{"csrf_token": {approverForm.Get("csrf_token")}, "idempotency_key": {uuid.NewString()}}, http.StatusBadRequest},
		"no request":         {"/approvals/nothing/approve", approverForm, http.StatusNotFound},
		"no CSRF token":      {requestPath(sd.partner2) + "/approve", url.Values{"if_match": {`"1"`}, "idempotency_key": {uuid.NewString()}}, http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			if r := approver.do(http.MethodPost, tc.path, tc.values); r.status != tc.status {
				t.Errorf("got %d, want %d", r.status, tc.status)
			}
		})
	}
	for _, path := range []string{requestPath(uuid.New()), "/approvals/x"} {
		t.Run("GET "+path, func(t *testing.T) { status(t, "no such request", approver.get(path), http.StatusNotFound) })
	}
	// A page token the API does not take: its code has no message in the
	// catalog, so the API's own title says it.
	r = approver.get("/approvals?page=garbage")
	status(t, "a bad page token", r, http.StatusBadRequest)
	holds(t, "a bad page token", r.body, "The ERP refused it:", "request.invalid_page_token")
	// htmx sends the CSRF token as a header, and no form token.
	htmx := url.Values{"if_match": {approverForm.Get("if_match")}, "idempotency_key": {uuid.NewString()}}
	if r := approver.do(http.MethodPost, requestPath(sd.partner2)+"/approve", htmx, "X-CSRF-Token", approverForm.Get("csrf_token")); r.status != http.StatusSeeOther {
		t.Errorf("by htmx: got %d %s", r.status, r.body)
	}
}

// TestNoPermission: a person without the permission to read approval
// requests is offered no inbox, and sees the API's own refusal there.
func TestNoPermission(t *testing.T) {
	s, sd := approvalsStack(t)
	b := s.as(sd.outsider)
	home := b.get("/").page(t)
	for _, a := range home.all("a", "navigation__link") {
		if attrOf(a, "href") == "/approvals" {
			t.Error("navigation: the inbox offered to a person who may not read approvals")
		}
	}
	for _, path := range []string{"/approvals", requestPath(sd.partner)} {
		r := b.get(path)
		status(t, path, r, http.StatusForbidden)
		holds(t, path, r.body, "You do not have the permission this needs.")
	}
}

// TestSegregationOfDuties: holding both permissions of a rule bars
// deciding: the inbox says so and lists them, the forms are not offered,
// and a decision sent anyway is refused with them listed.
func TestSegregationOfDuties(t *testing.T) {
	s, sd := approvalsStack(t)
	b := s.browser()
	b.signIn()
	form := values(b.get(requestPath(sd.partner)).page(t).form(requestPath(sd.partner)+"/approve"), "")
	pgtest.Exec(t, `INSERT INTO identity.sod_rule (tenant_id, id, permission_a, permission_b, description, valid_from, created_by)
		VALUES ($1, gen_random_uuid(), 'workflow.request.cancel', 'workflow.request.decide', 'd', current_date, $2)`, s.ten.ID, s.ten.Human.ID)
	inbox := b.get("/approvals").page(t).mainText()
	for _, want := range []string{"Your approvals are on hold", "workflow.request.cancel", "workflow.request.decide", "Nothing is waiting"} {
		if !strings.Contains(inbox, want) {
			t.Errorf("inbox: no %q in %q", want, inbox)
		}
	}
	req := b.get(requestPath(sd.partner)).page(t)
	holds(t, "request", req.mainText(), "You may not decide this request")
	if got := len(req.all("form", "decide")); got != 0 {
		t.Errorf("request: got %d decision forms, want none", got)
	}
	r := b.do(http.MethodPost, requestPath(sd.partner)+"/approve", form)
	status(t, "decided anyway", r, http.StatusForbidden)
	holds(t, "decided anyway", r.page(t).mainText(), "A segregation-of-duties rule bars this",
		"Your barred permissions: workflow.request.cancel workflow.request.decide")
}

// holds checks that got holds each of wants.
func holds(t *testing.T, what, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("%s: got %q, want %q in it", what, got, w)
		}
	}
}

// status checks a response's status.
func status(t *testing.T, what string, r response, want int) {
	t.Helper()
	if r.status != want {
		t.Errorf("%s: got status %d, want %d", what, r.status, want)
	}
}
