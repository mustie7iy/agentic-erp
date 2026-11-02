package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
)

// postedJSON is a posted invoice on an order, in EUR.
func postedJSON(id uuid.UUID) string {
	return strings.Replace(invoiceJSON(id), `"kind":"new_kind","status":"in_review"`, `"kind":"invoice","status":"posted"`, 1)
}

// openDownPaymentJSON is a page of one open down payment of entry, line 1,
// with the next page's token when next is not "".
func openDownPaymentJSON(entry uuid.UUID, next string) string {
	more := ""
	if next != "" {
		more = fmt.Sprintf(`,"next_page_token":%q`, next)
	}
	return fmt.Sprintf(`{"items":[{"fiscal_year":2026,"journal_entry_id":%q,"line_no":1,"legal_entity_id":%q,"account_id":%q,"posting_date":"2026-03-05",
		"amount":{"amount":"100.00","currency":"EUR"},"amount_lc":{"amount":"100.00","currency":"EUR"},"document_number":"100009","special_gl":"A"}]%s}`,
		entry, uuid.NewString(), uuid.NewString(), more)
}

// TestClearDownPaymentsStubbed: the API's rarer answers, from a stub
// behind the real sign-in: a next page of down payments kept in the form
// and asked for by its token; a down payment without a paid request shown
// without its box; none open said so; the requests refused in their place; an invoice no
// longer posted when a clearing is refused lists none.
func TestClearDownPaymentsStubbed(t *testing.T) {
	id, entry := uuid.New(), uuid.New()
	me := answer(200, meJSON(permReadInvoices, permPostInvoices, permReadEntries))
	st := &stubbed{routes: map[string]http.HandlerFunc{
		"GET /v1/me":                    me,
		"GET /v1/supplier-invoices/":    answer(200, postedJSON(id)),
		"GET /v1/open-items":            answer(200, openDownPaymentJSON(entry, "t2")),
		"GET /v1/down-payment-requests": answer(200, `{"items":[]}`),
	}}
	_, b, _ := stubStack(t, st)
	r := b.get(downPaymentsPath(id) + "?page=t1")
	status(t, "a page without requests", r, http.StatusOK)
	p := r.page(t)
	holds(t, "a page without requests", p.mainText(), "100009", "No paid down payment request of the supplier names this payment")
	if offered, _ := boxes(p); len(offered) != 0 {
		t.Errorf("a down payment without a request: got %v offered, want none", offered)
	}
	more := ""
	for _, a := range p.all("a", "button") {
		more = attrOf(a, "href")
	}
	if want := downPaymentsPath(id) + "?page=t2"; more != want {
		t.Errorf("next page: got %q, want %q", more, want)
	}
	var asked url.Values
	for _, q := range st.queries {
		if q.Has("special_gl") {
			asked = q
		}
	}
	for name, want := range map[string]string{"special_gl": "A", "page_token": "t1", "page_size": "100"} {
		if got := asked.Get(name); got != want {
			t.Errorf("listOpenItems %s: got %q, want %q", name, got, want)
		}
	}
	_, none, _ := stubStack(t, &stubbed{routes: map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/supplier-invoices/": answer(200, postedJSON(id)),
		"GET /v1/open-items": answer(200, `{"items":[]}`)}})
	holds(t, "none open", none.get(downPaymentsPath(id)).page(t).mainText(), "The supplier has no open down payments in the invoice's company.")

	_, b2, _ := stubStack(t, &stubbed{routes: map[string]http.HandlerFunc{
		"GET /v1/me":                    me,
		"GET /v1/supplier-invoices/":    answer(200, postedJSON(id)),
		"GET /v1/open-items":            answer(200, openDownPaymentJSON(entry, "")),
		"GET /v1/down-payment-requests": answer(403, problemJSON(403, "identity.permission_missing")),
	}})
	refused := b2.get(downPaymentsPath(id))
	status(t, "the requests refused", refused, http.StatusForbidden)
	holds(t, "the requests refused", refused.page(t).mainText(), "The down payments were not cleared", "You do not have the permission this needs.")

	_, b3, token := stubStack(t, &stubbed{routes: map[string]http.HandlerFunc{
		"GET /v1/me":                  me,
		"GET /v1/supplier-invoices/":  answer(200, invoiceJSON(id)),
		"POST /v1/supplier-invoices/": answer(409, problemJSON(409, "payables.invalid_status")),
	}})
	v := url.Values{"csrf_token": {token}, "if_match": {`"3"`}, "idempotency_key": {uuid.NewString()}, "date": {"2026-03-20"},
		"down_payment": {"2026:" + entry.String() + ":1"}}
	closed := b3.do(http.MethodPost, downPaymentsPath(id), v)
	status(t, "no longer posted", closed, http.StatusConflict)
	holds(t, "no longer posted", closed.page(t).mainText(), "The invoice's status does not allow this any more.",
		"Down payments are cleared against a posted invoice only.")
}

// TestClearDownPaymentsUnavailable: an API that cannot answer, limits the
// rate or fails makes the screen unavailable, never a half screen; its 401
// signs the person in again.
func TestClearDownPaymentsUnavailable(t *testing.T) {
	id, entry := uuid.New(), uuid.New()
	me := answer(200, meJSON(permReadInvoices, permPostInvoices, permReadEntries))
	challenge := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_user_authentication", acr_values="urn:erp:mfa"`)
		answer(401, problemJSON(401, "identity.mfa_required"))(w, r)
	}
	unavailable := answer(503, problemJSON(503, "platform.unavailable"))
	posted, items := answer(200, postedJSON(id)), answer(200, openDownPaymentJSON(entry, ""))
	path := downPaymentsPath(id)
	for name, tc := range map[string]struct {
		routes   map[string]http.HandlerFunc
		method   string
		form     url.Values
		status   int
		location string
	}{
		"screen: who am I, 401": {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodGet, nil, http.StatusSeeOther,
			"return=%2Finvoices%2F" + id.String() + "%2Fdown-payments"},
		"screen: invoice unavailable": {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/supplier-invoices/": unavailable}, http.MethodGet, nil, http.StatusServiceUnavailable, ""},
		"screen: items unavailable": {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/supplier-invoices/": posted, "GET /v1/open-items": unavailable},
			http.MethodGet, nil, http.StatusServiceUnavailable, ""},
		"screen: requests unavailable": {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/supplier-invoices/": posted, "GET /v1/open-items": items,
			"GET /v1/down-payment-requests": unavailable}, http.MethodGet, nil, http.StatusServiceUnavailable, ""},
		"sent: who am I, 401": {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodPost, url.Values{"down_payment": {"2026:" + entry.String() + ":1"}},
			http.StatusSeeOther, "return=%2Finvoices%2F" + id.String()},
		"sent: unavailable": {map[string]http.HandlerFunc{"GET /v1/me": me, "POST /v1/supplier-invoices/": unavailable}, http.MethodPost,
			url.Values{"down_payment": {"2026:" + entry.String() + ":1"}}, http.StatusServiceUnavailable, ""},
		"sent and refused: invoice unavailable": {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/supplier-invoices/": unavailable,
			"POST /v1/supplier-invoices/": answer(409, problemJSON(409, "payables.invalid_status"))}, http.MethodPost,
			url.Values{"down_payment": {"2026:" + entry.String() + ":1"}}, http.StatusServiceUnavailable, ""},
		"checked: invoice unavailable": {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/supplier-invoices/": unavailable}, http.MethodPost, url.Values{},
			http.StatusServiceUnavailable, ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, b, token := stubStack(t, &stubbed{routes: tc.routes})
			var r response
			if tc.method == http.MethodGet {
				r = b.get(path)
			} else {
				v := url.Values{"csrf_token": {token}, "if_match": {`"3"`}, "idempotency_key": {uuid.NewString()}, "date": {"2026-03-20"}}
				for k, vs := range tc.form {
					v[k] = vs
				}
				r = b.do(tc.method, path, v)
			}
			if r.status != tc.status || !strings.HasSuffix(r.location(), tc.location) {
				t.Errorf("got %d to %q, want %d to %q", r.status, r.location(), tc.status, tc.location)
			}
		})
	}
}

// TestChoiceOf: a down payment is offered when a paid request names its
// entry, in the invoice's currency and on its order or on none; else it is
// shown with why not.
func TestChoiceOf(t *testing.T) {
	entry, order := uuid.New(), uuid.New()
	item := gen.OpenItem{FiscalYear: 2026, JournalEntryID: entry, LineNo: 2, DocumentNumber: "100009", Reference: gen.NewOptString("SAP 4711")}
	request := func(currency string, po gen.OptUUID, ref gen.OptString) map[uuid.UUID]gen.DownPaymentRequest {
		return map[uuid.UUID]gen.DownPaymentRequest{entry: {DocumentNumber: "DP1", Amount: gen.Money{Amount: "119.00", Currency: currency},
			TaxAmount: gen.Money{Amount: "19.00", Currency: currency}, PurchaseOrderID: po, Reference: ref}}
	}
	onOrder := gen.SupplierInvoice{GrossAmount: gen.Money{Currency: "EUR"}, PurchaseOrderID: gen.NewOptUUID(order)}
	noOrder := gen.SupplierInvoice{GrossAmount: gen.Money{Currency: "EUR"}}
	for name, c := range map[string]struct {
		inv       gen.SupplierInvoice
		requests  map[uuid.UUID]gen.DownPaymentRequest
		value     string
		why       i18n.Key
		reference string
	}{
		"no request":          {noOrder, nil, "", i18n.DownPaymentsNoRequest, "SAP 4711"},
		"another currency":    {noOrder, request("USD", gen.OptUUID{}, gen.OptString{}), "", i18n.DownPaymentsOtherCurrency, "SAP 4711"},
		"an order, none":      {noOrder, request("EUR", gen.NewOptUUID(order), gen.OptString{}), "", i18n.DownPaymentsOtherOrder, "SAP 4711"},
		"another order":       {onOrder, request("EUR", gen.NewOptUUID(uuid.New()), gen.OptString{}), "", i18n.DownPaymentsOtherOrder, "SAP 4711"},
		"the invoice's order": {onOrder, request("EUR", gen.NewOptUUID(order), gen.NewOptString("AB-1")), "2026:" + entry.String() + ":2", "", "AB-1"},
		"no order":            {onOrder, request("EUR", gen.OptUUID{}, gen.OptString{}), "2026:" + entry.String() + ":2", "", "SAP 4711"},
	} {
		t.Run(name, func(t *testing.T) {
			got := choiceOf(item, c.inv, c.requests)
			if got.Value != c.value {
				t.Errorf("value: got %q, want %q", got.Value, c.value)
			}
			if got.Why != c.why {
				t.Errorf("why: got %q, want %q", got.Why, c.why)
			}
			if got.Reference != c.reference {
				t.Errorf("reference: got %q, want %q", got.Reference, c.reference)
			}
		})
	}
}

// TestDownPaymentRef: a down payment's value is read back only as the
// form writes it.
func TestDownPaymentRef(t *testing.T) {
	entry := uuid.New()
	for name, c := range map[string]struct {
		value string
		ok    bool
	}{
		"as written":     {"2026:" + entry.String() + ":3", true},
		"two parts":      {"2026:" + entry.String(), false},
		"no year":        {"x:" + entry.String() + ":3", false},
		"no entry":       {"2026:x:3", false},
		"no line":        {"2026:" + entry.String() + ":x", false},
		"a year too big": {"9999999999:" + entry.String() + ":3", false},
	} {
		t.Run(name, func(t *testing.T) {
			ref, ok := downPaymentRef(c.value)
			if ok != c.ok {
				t.Fatalf("ok: got %t, want %t", ok, c.ok)
			}
			if ok && (ref.FiscalYear != 2026 || ref.JournalEntryID != entry || ref.LineNo != 3) {
				t.Errorf("ref: got %+v", ref)
			}
		})
	}
}
