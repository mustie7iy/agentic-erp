package web

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/html"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// pendingJSON is a pending partner change's request the requester asked
// for, one step, no decision.
func pendingJSON(id, requester uuid.UUID) string {
	u := uuid.NewString
	return fmt.Sprintf(`{"id":%q,"version":3,"created_at":"2026-10-22T10:00:00Z","created_by":%q,"updated_at":"2026-10-22T10:00:00Z",
		"object_type":"partner_change","object_id":%q,"title":"Stubbed change","requester_id":%q,"status":"pending","current_step":1,
		"steps":[{"step_no":1,"role_id":%q,"required":1,"approvals":0}],"decisions":[]}`, id, u(), u(), requester, u())
}

// textarea is the text of the form's text area, "" for none.
func textarea(form *html.Node) string {
	kept := ""
	walk(form, func(n *html.Node) {
		if n.Data == "textarea" && n.FirstChild != nil {
			kept = n.FirstChild.Data
		}
	})
	return kept
}

// TestDraftAcrossStepUp: a decision the API answers with a step-up
// challenge sends the person to sign in again with what they wrote kept
// in their browser; back on the request's page after the sign-in it is in
// its form, not sent, and said so, once. Each decision keeps its own
// field; a long text in several scripts survives whole; another person
// signing in on the browser, another request's page, a draft past its ten
// minutes or one tampered with show nothing.
func TestDraftAcrossStepUp(t *testing.T) {
	id, own, meID := uuid.New(), uuid.New(), uuid.New()
	challenge := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_user_authentication", acr_values="urn:erp:mfa"`)
		answer(401, problemJSON(401, "identity.mfa_required"))(w, r)
	}
	closing, closed := uuid.New(), &atomic.Bool{} // a request someone else decides while the person signs in again
	closingAnswer := func(w http.ResponseWriter, r *http.Request) {
		if closed.Load() {
			answer(200, requestJSON(closing))(w, r)
			return
		}
		answer(200, pendingJSON(closing, uuid.New()))(w, r)
	}
	st := &stubbed{routes: map[string]http.HandlerFunc{
		"GET /v1/me":                                    answer(200, meAs(meID, permReadApprovals, permDecide, permCancelRequests, "partner.change.approve")),
		"GET /v1/principal-names":                       answer(200, `{"items":[]}`),
		"GET /v1/approval-requests/":                    answer(200, pendingJSON(id, uuid.New())),
		"GET /v1/approval-requests/" + own.String():     answer(200, pendingJSON(own, meID)),
		"GET /v1/approval-requests/" + closing.String(): closingAnswer,
		"POST /v1/approval-requests/":                   challenge,
	}}
	s, b, token := stubStack(t, st)
	long := strings.Repeat("𝄞", 2000) // 2,000 characters of four bytes each: the most three cookies hold
	for _, tc := range []struct {
		verb, action, text string
	}{{ui.VerbApprove, "/approve", "Checked <b>twice</b>\nagainst the budget"}, {ui.VerbReject, "/reject", long}, {ui.VerbWithdraw, "/cancel", "Raised twice"}} {
		t.Run(tc.verb, func(t *testing.T) {
			id := id
			if tc.verb == ui.VerbWithdraw {
				id = own // the person's own request
			}
			form := url.Values{"csrf_token": {token}, "if_match": {`"3"`}, "idempotency_key": {uuid.NewString()}, "comment": {tc.text}}
			r := b.do(http.MethodPost, requestPath(id)+tc.action, form)
			if r.status != http.StatusSeeOther || !strings.Contains(r.location(), "acr=urn%3Aerp%3Amfa") {
				t.Fatalf("step-up: got %d to %q", r.status, r.location())
			}
			b.signIn() // a new session, the same person
			p := b.get(requestPath(id)).page(t)
			holds(t, "back", p.mainText(), "Your decision was not sent")
			if got := textarea(p.form(requestPath(id) + tc.action)); got != tc.text {
				t.Errorf("kept: got %d characters %.40q, want the %d written", len([]rune(got)), got, len([]rune(tc.text)))
			}
			if again := b.get(requestPath(id)).page(t); strings.Contains(again.mainText(), "Your decision was not sent") {
				t.Error("kept twice")
			}
			token = field(b.get(requestPath(id)).page(t).form("/logout"), "csrf_token")
		})
	}
	// The request decided by someone else while the person signed in
	// again: the text is shown as it was written, not lost, with no form.
	form := url.Values{"csrf_token": {token}, "if_match": {`"3"`}, "idempotency_key": {uuid.NewString()}, "comment": {"Duplicate <i>order</i>"}}
	if r := b.do(http.MethodPost, requestPath(closing)+"/reject", form); r.status != http.StatusSeeOther {
		t.Fatalf("step-up: got %d", r.status)
	}
	closed.Store(true)
	b.signIn()
	gone := b.get(requestPath(closing)).page(t)
	holds(t, "closed meanwhile", gone.mainText(), "it no longer takes this decision", "Duplicate <i>order</i>")
	if strings.Contains(gone.mainText(), "What you wrote is kept below") || len(gone.all("form", "decide")) != 0 {
		t.Errorf("closed meanwhile: got %q, want no form and no promise of one", gone.mainText())
	}
	token = field(b.get(requestPath(id)).page(t).form("/logout"), "csrf_token")
	keep := func() {
		t.Helper()
		form := url.Values{"csrf_token": {token}, "if_match": {`"3"`}, "idempotency_key": {uuid.NewString()}, "comment": {"Mine"}}
		if r := b.do(http.MethodPost, requestPath(id)+"/approve", form); r.status != http.StatusSeeOther {
			t.Fatalf("step-up: got %d", r.status)
		}
	}
	notKept := func(what string, path string) {
		t.Helper()
		if p := b.get(path).page(t); strings.Contains(p.mainText(), "Your decision was not sent") {
			t.Errorf("%s: the draft shown", what)
		}
	}
	keep()
	notKept("another request's page", requestPath(uuid.New()))
	if b.cookie(draftCookie+"0") == "" {
		t.Error("another request's page deleted the draft")
	}
	s.clock.Advance(signInTTL)
	notKept("past ten minutes", requestPath(id))
	if b.cookie(draftCookie+"0") != "" {
		t.Error("a draft past its time is kept")
	}
	s.clock.Advance(-signInTTL)
	keep()
	b.setCookie(draftCookie+"0", "x"+b.cookie(draftCookie+"0"))
	notKept("tampered with", requestPath(id))
	keep()
	b.setCookie(draftCookie+"0", "!")
	notKept("not base64", requestPath(id))
	keep()
	other := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO identity.principal (tenant_id, id, kind, external_issuer, external_subject, display_name, all_companies, status, created_by, updated_by)
		VALUES ($1, $2, 'human', $3, $4, 'other', true, 'active', $2, $2)`, s.ten.ID, other, s.is.URL, "human|"+other.String())
	s.is.SignIn(authntest.User{Tenant: s.ten.ID, Subject: "human|" + other.String()})
	b.signIn()
	notKept("another person", requestPath(id))
}

// TestObjectHiddenOrUnknown: a request's object the person may not read
// is not read, and the page says so; an object type the workspace does
// not summarise says that; a read the API cannot answer makes the page
// unavailable.
func TestObjectHiddenOrUnknown(t *testing.T) {
	id := uuid.New()
	for name, tc := range map[string]struct {
		me, object string
		read       http.HandlerFunc
		status     int
		want       string
	}{
		"hidden":      {meJSON(permReadApprovals), pendingJSON(id, uuid.New()), nil, http.StatusOK, "You may not read this object itself"},
		"unknown":     {meJSON(permReadApprovals), requestJSON(id), nil, http.StatusOK, "does not show this kind of object yet"},
		"unavailable": {meJSON(permReadApprovals, permReadSuppliers), pendingJSON(id, uuid.New()), answer(503, problemJSON(503, "platform.unavailable")), http.StatusServiceUnavailable, ""},
	} {
		t.Run(name, func(t *testing.T) {
			st := &stubbed{routes: map[string]http.HandlerFunc{"GET /v1/me": answer(200, tc.me), "GET /v1/principal-names": answer(200, `{"items":[]}`),
				"GET /v1/approval-requests/": answer(200, tc.object), "GET /v1/partner-changes/": tc.read}}
			_, b, _ := stubStack(t, st)
			r := b.get(requestPath(id))
			status(t, name, r, tc.status)
			if tc.want != "" {
				holds(t, name, r.page(t).mainText(), tc.want)
			}
		})
	}
}

// TestSummaries: every object's summary says what its type is about, its
// data as data; any answer that is no object has none.
func TestSummaries(t *testing.T) {
	srv, _ := testServer(t)
	ctx := i18n.With(context.Background(), srv.Catalog.Match("en"))
	day := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	eur := gen.Money{Amount: "12000.00", Currency: "EUR"}
	id := uuid.New()
	for name, tc := range map[string]struct {
		res  any
		want []string
		link string
	}{
		"requisition": {&gen.GetRequisitionOKHeaders{Response: gen.Requisition{DocumentNumber: "PR-1", RequisitionDate: day, NeededBy: gen.NewOptDate(day),
			TotalAmount: eur, Items: make([]gen.RequisitionItem, 2), Status: "submitted"}}, []string{"Number PR-1", "Needed by 2026-10-22", "Total EUR 12,000.00", "Items 2"}, ""},
		"partner change": {&gen.GetPartnerChangeOKHeaders{Response: gen.PartnerChange{Number: "PC-1", Kind: "update", Status: "pending", Reason: "<b>New</b>"}},
			[]string{"Kind of change update", "Reason given <b>New</b>"}, ""},
		"configuration": {&gen.GetChangeRequestOKHeaders{Response: gen.ChangeRequest{Number: "CR-1", Title: "Tiers", Status: "pending_approval", Tier: 2}},
			[]string{"Title Tiers", "Approval tier 2"}, ""},
		"invoice": {&gen.GetSupplierInvoiceOKHeaders{Response: gen.SupplierInvoice{ID: id, DocumentNumber: "5100", SupplierReference: "INV-9", InvoiceDate: day,
			GrossAmount: eur, Status: "on_hold", HoldReason: gen.NewOptString("Price")}}, []string{"Supplier's reference INV-9", "Held because Price"}, invoicePath(id)},
		"period": {&gen.FiscalPeriodHeaders{Response: gen.FiscalPeriod{ID: id, FiscalYear: 2026, Period: 9, StartDate: day, EndDate: day, Status: "locked"}},
			[]string{"Period 9 of 2026", "Locked"}, ui.PeriodsBase + "/" + id.String()},
		"entry": {&gen.GetParkedJournalEntryOKHeaders{Response: gen.ParkedJournalEntry{ID: id, DocumentType: "SA", PostingDate: day, Currency: "EUR",
			Reference: gen.NewOptString("REF-1"), HeaderText: gen.NewOptString("Accrual"), Lines: make([]gen.ParkedJournalLine, 2), Status: "pending_approval"}},
			[]string{"Document type SA", "Header text Accrual", "Lines 2"}, parkedPath(id)},
		"order": {&gen.GetPurchaseOrderOKHeaders{Response: gen.PurchaseOrder{DocumentNumber: "4500", OrderDate: day, TotalAmount: eur, Status: "pending_approval"}},
			[]string{"Number 4500", "Total EUR 12,000.00"}, ""},
		"center change": {&gen.CenterChangeHeaders{Response: gen.CenterChange{Kind: "lock", Status: "pending", Reason: "Closing"}}, []string{"Kind of change lock", "Reason given Closing"}, ""},
		"GR/IR clearing": {&gen.GrIrClearingHeaders{Response: gen.GrIrClearing{PurchaseOrderNumber: "4501", PostingDate: day, Total: eur, Status: "pending", Reason: "Closed"}},
			[]string{"Purchase order 4501", "Reason given Closed"}, ""},
		"payment run": {&gen.PaymentRunHeaders{Response: gen.PaymentRun{ID: id, DocumentNumber: "PR-9", RunDate: day, DueBy: day, TotalAmount: eur, Status: "proposed"}},
			[]string{"Number PR-9", "Paying what is due by 2026-10-22"}, runPath(id)},
	} {
		t.Run(name, func(t *testing.T) {
			sum, ok := summaryOf(tc.res)
			if !ok {
				t.Fatal("no summary")
			}
			var buf bytes.Buffer
			if err := ui.SummaryList(sum.Rows).Render(ctx, &buf); err != nil {
				t.Fatal(err)
			}
			doc, err := html.Parse(&buf)
			if err != nil {
				t.Fatal(err)
			}
			holds(t, name, text(doc), tc.want...)
			if sum.Link != tc.link {
				t.Errorf("link: got %q, want %q", sum.Link, tc.link)
			}
		})
	}
	if _, ok := summaryOf(&gen.GetRequisitionNotFound{}); ok {
		t.Error("a refusal summarised")
	}
}
