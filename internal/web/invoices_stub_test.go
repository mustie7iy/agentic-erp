package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// invoiceJSON is an invoice on an order, of a status and a kind the
// workspace does not know (open enumerations), with a tax date a person
// set.
func invoiceJSON(id uuid.UUID) string {
	u := uuid.NewString
	return fmt.Sprintf(`{"id":%q,"version":3,"created_at":"2026-10-22T10:00:00Z","created_by":%q,"updated_at":"2026-10-22T10:00:00Z",
		"document_number":"SI9","legal_entity_id":%q,"supplier_id":%q,"supplier_reference":"R-9","invoice_date":"2026-03-10",
		"tax_date":"2026-03-11","gross_amount":{"amount":"1.00","currency":"EUR"},"tax_amount":{"amount":"0.00","currency":"EUR"},
		"purchase_order_id":%q,"kind":"new_kind","status":"in_review","source":"edi","items":[{"quantity":{"value":"1","unit":"EA"},
		"amount":{"amount":"1.00","currency":"EUR"}}],"extensions":{},"documentary_warnings":[]}`, id, u(), u(), u(), u())
}

// recommendationJSON is an open recommendation of a kind the workspace
// does not know.
func recommendationJSON(invoice uuid.UUID) string {
	u := uuid.NewString
	return fmt.Sprintf(`{"items":[{"id":%q,"version":1,"session_id":%q,"agent_id":%q,"object_type":"supplier_invoice","object_id":%q,
		"kind":"new_kind","content":"Look","created_at":"2026-10-22T10:00:00Z","updated_at":"2026-10-22T10:00:00Z"}]}`, u(), u(), u(), invoice)
}

// TestInvoicesStubbed: the API's rarer answers, from a stub behind the
// real sign-in: a queue with a next page, the companies unreadable, the
// match and the recommendations refused in their places, values the
// workspace does not know shown as they are, recommendations a person
// may read but not decide.
func TestInvoicesStubbed(t *testing.T) {
	id := uuid.New()
	st := &stubbed{routes: map[string]http.HandlerFunc{
		"GET /v1/me":                answer(200, meJSON(permReadInvoices, permReadRecommendations, permReadCompanies, permReadOrders)),
		"GET /v1/principal-names":   answer(200, `{"items":[]}`),
		"GET /v1/legal-entities":    answer(500, problemJSON(500, "platform.internal")),
		"GET /v1/supplier-invoices": answer(200, `{"items":[`+invoiceJSON(id)+`],"next_page_token":"t2"}`),
		"GET /v1/supplier-invoices/": func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/match-results") {
				answer(403, problemJSON(403, "identity.permission_missing"))(w, r)
				return
			}
			answer(200, invoiceJSON(id))(w, r)
		},
		"GET /v1/recommendations": answer(200, recommendationJSON(id)),
	}}
	s, b, _ := stubStack(t, st)
	q := b.get("/invoices?status=in_review&page=t1")
	status(t, "the queue", q, http.StatusOK)
	p := q.page(t)
	more := ""
	for _, a := range p.all("a", "button") {
		more = attrOf(a, "href")
	}
	if more != "/invoices?page=t2&status=in_review" {
		t.Errorf("next page: got %q", more)
	}
	if len(p.all("select", "")) != 0 {
		t.Error("the company filter offered without the companies")
	}
	holds(t, "the queue", p.mainText(), "in_review", "Kind new_kind")
	if got := st.queries[2].Get("page_token"); got != "t1" {
		t.Errorf("the queue asked with token %q, want t1", got)
	}
	if !strings.Contains(s.logs.String(), "the companies could not be read") {
		t.Error("the companies' failure is not logged")
	}
	inv := b.get(invoicePath(id)).page(t).mainText()
	holds(t, "the invoice", inv, "Status in_review", "Kind new_kind", "Tax date 2026-03-11", "How it was entered Electronic data interchange",
		"Three-way match You do not have the permission this needs.", "You may not decide agents' recommendations. new_kind", "Nothing is left to do on this invoice.")
	_, b2, _ := stubStack(t, &stubbed{routes: map[string]http.HandlerFunc{
		"GET /v1/me":                 answer(200, meJSON(permReadInvoices, permReadRecommendations)),
		"GET /v1/principal-names":    answer(200, `{"items":[]}`),
		"GET /v1/supplier-invoices/": answer(200, invoiceJSON(id)),
		"GET /v1/supplier-invoices/" + id.String() + "/match-results": answer(200, `{"items":[]}`),
		"GET /v1/recommendations":                                     answer(403, problemJSON(403, "identity.permission_missing")),
	}})
	holds(t, "the recommendations refused", b2.get(invoicePath(id)).page(t).mainText(), "The recommendations could not be shown",
		"You do not have the permission this needs.", "Not matched yet.")
}

// TestInvoicesUnavailable: an API that cannot answer, limits the rate or
// fails makes the page unavailable, never a half page; its 401 signs the
// person in again, back to the invoice an action was sent from.
func TestInvoicesUnavailable(t *testing.T) {
	id := uuid.New()
	me := answer(200, meJSON(permReadInvoices, permReadRecommendations, permReadOrders))
	challenge := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_user_authentication", acr_values="urn:erp:mfa"`)
		answer(401, problemJSON(401, "identity.mfa_required"))(w, r)
	}
	unavailable := answer(503, problemJSON(503, "platform.unavailable"))
	invoice := answer(200, invoiceJSON(id))
	form := func(token string) url.Values {
		return url.Values{"csrf_token": {token}, "if_match": {`"1"`}, "idempotency_key": {uuid.NewString()}, "comment": {"ok"}, "outcome": {"accepted"}}
	}
	path := invoicePath(id)
	for name, tc := range map[string]struct {
		routes   map[string]http.HandlerFunc
		method   string
		path     string
		status   int
		location string
	}{
		"queue: who am I, 401":   {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodGet, "/invoices", http.StatusSeeOther, "return=%2Finvoices"},
		"queue: rate limited":    {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/supplier-invoices": answer(429, problemJSON(429, "request.rate_limited"))}, http.MethodGet, "/invoices", http.StatusServiceUnavailable, ""},
		"invoice: who am I, 401": {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodGet, path, http.StatusSeeOther, "return=%2Finvoices%2F" + id.String()},
		"invoice: unavailable":   {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/supplier-invoices/": unavailable}, http.MethodGet, path, http.StatusServiceUnavailable, ""},
		"invoice: refused":       {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/supplier-invoices/": answer(403, problemJSON(403, "identity.permission_missing"))}, http.MethodGet, path, http.StatusForbidden, ""},
		"invoice: the match unavailable": {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/supplier-invoices/" + id.String() + "/match-results": unavailable,
			"GET /v1/supplier-invoices/": invoice}, http.MethodGet, path, http.StatusServiceUnavailable, ""},
		"invoice: the recommendations unavailable": {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/supplier-invoices/" + id.String() + "/match-results": answer(200, `{"items":[]}`),
			"GET /v1/supplier-invoices/": invoice, "GET /v1/recommendations": unavailable}, http.MethodGet, path, http.StatusServiceUnavailable, ""},
		"order: who am I, 401": {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodGet, path + "/order", http.StatusSeeOther, "return=%2Finvoices%2F" + id.String() + "%2Forder"},
		"order: the receipts refused": {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/supplier-invoices/": invoice,
			"GET /v1/purchase-orders/": answer(200, `{"id":"`+uuid.NewString()+`","version":1,"created_at":"2026-10-22T10:00:00Z","created_by":"`+uuid.NewString()+
				`","updated_at":"2026-10-22T10:00:00Z","document_number":"PO9","legal_entity_id":"`+uuid.NewString()+`","supplier_id":"`+uuid.NewString()+
				`","currency":"EUR","order_date":"2026-03-01","payment_term_code":"NT30","status":"released","items":[{"description":"Paper","quantity":{"value":"1","unit":"EA"},"unit_price":{"amount":"1.00","currency":"EUR"},"account_id":"`+uuid.NewString()+`"}],"total_amount":{"amount":"0.00","currency":"EUR"},"extensions":{}}`),
			"GET /v1/goods-receipts": answer(403, problemJSON(403, "identity.permission_missing"))}, http.MethodGet, path + "/order", http.StatusOK, ""},
		"document: unavailable": {map[string]http.HandlerFunc{"GET /v1/supplier-invoices/": unavailable}, http.MethodGet, path + "/document", http.StatusServiceUnavailable, ""},
		"action: who am I, 401": {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodPost, path + "/post", http.StatusSeeOther, "return=%2Finvoices%2F" + id.String()},
		"action: rate limited":  {map[string]http.HandlerFunc{"GET /v1/me": me, "POST /v1/supplier-invoices/": answer(429, problemJSON(429, "request.rate_limited"))}, http.MethodPost, path + "/hold", http.StatusServiceUnavailable, ""},
		"change: who am I, 401": {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodGet, path + "/edit", http.StatusSeeOther,
			"return=%2Finvoices%2F" + id.String() + "%2Fedit"},
		"change: unavailable": {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/supplier-invoices/": unavailable}, http.MethodGet, path + "/edit",
			http.StatusServiceUnavailable, ""},
		"change sent: who am I, 401": {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodPost, path + "/edit", http.StatusSeeOther,
			"return=%2Finvoices%2F" + id.String()},
		"change sent: unavailable": {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/supplier-invoices/": unavailable}, http.MethodPost, path + "/edit",
			http.StatusServiceUnavailable, ""},
		"decision: who am I, 401": {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodPost, path + "/recommendations/" + id.String() + "/decide", http.StatusSeeOther,
			"return=%2Finvoices%2F" + id.String() + "%2Frecommendations%2F" + id.String()},
	} {
		t.Run(name, func(t *testing.T) {
			_, b, token := stubStack(t, &stubbed{routes: tc.routes})
			var r response
			if tc.method == http.MethodGet {
				r = b.get(tc.path)
			} else {
				r = b.do(tc.method, tc.path, form(token))
			}
			if r.status != tc.status || !strings.HasSuffix(r.location(), tc.location) {
				t.Errorf("got %d to %q, want %d to %q", r.status, r.location(), tc.status, tc.location)
			}
		})
	}
}

// TestChangeInvoiceUnavailable: an API that cannot take a change makes
// the page unavailable, never the form as if it were refused.
func TestChangeInvoiceUnavailable(t *testing.T) {
	id := uuid.New()
	_, b, token := stubStack(t, &stubbed{routes: map[string]http.HandlerFunc{
		"GET /v1/me":                   answer(200, meJSON(permReadInvoices, permCaptureInvoices)),
		"GET /v1/supplier-invoices/":   answer(200, invoiceJSON(id)),
		"PATCH /v1/supplier-invoices/": answer(503, problemJSON(503, "platform.unavailable")),
	}})
	v := url.Values{"csrf_token": {token}, "if_match": {`"3"`}, "idempotency_key": {uuid.NewString()}, "supplier_reference": {"R-10"},
		"invoice_date": {"2026-03-10"}, "tax_date": {"2026-03-11"}, "gross_amount": {"1.00"}, "tax_amount": {"0.00"}, "item_quantity": {"1"},
		"item_amount": {"1.00"}}
	status(t, "the change unavailable", b.do(http.MethodPost, editPath(id), v), http.StatusServiceUnavailable)
}

// TestReasonError: a reason is required where an action needs one, and
// any text has at most 2,000 characters (not bytes).
func TestReasonError(t *testing.T) {
	for name, tc := range map[string]struct {
		text     string
		required bool
		want     string
	}{
		"no comment":       {"", false, ""},
		"no reason":        {" \n", true, "action.reason.required"},
		"a reason":         {"Duplicate", true, ""},
		"2,000 characters": {strings.Repeat("é", 2000), true, ""},
		"2,001 characters": {strings.Repeat("x", 2001), false, "decide.comment.long"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := reasonError(tc.text, tc.required); string(got) != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
