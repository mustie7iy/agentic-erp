package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/net/html"
)

// runJSON is a payment run in the status, its operations and its
// release's approval as given (extra: further fields, a leading comma
// each).
func runJSON(id uuid.UUID, status, extra string) string {
	u := uuid.NewString
	return fmt.Sprintf(`{"id":%q,"version":3,"created_at":"2026-10-22T10:00:00Z","created_by":%q,"updated_at":"2026-10-22T10:00:00Z",
		"document_number":"PR900001","legal_entity_id":%q,"run_date":"2026-10-23","due_by":"2026-10-31","total_amount":{"amount":"1250.00","currency":"AED"},
		"status":%q,"proposed_by":%q,"operation_id":%q%s}`, id, u(), u(), status, u(), u(), extra)
}

// stubPaymentsJSON is a page of a run's payments with a next page: a down
// payment with a cash discount and withholding tax, paid by a method.
func stubPaymentsJSON(run uuid.UUID) string {
	u := uuid.NewString
	return fmt.Sprintf(`{"items":[{"id":%q,"created_at":"2026-10-22T10:00:00Z","created_by":%q,"payment_run_id":%q,"supplier_id":%q,"supplier_name":"Tyrell",
		"bank_account_id":%q,"amount":{"amount":"1250.00","currency":"AED"},"discount_amount":{"amount":"25.00","currency":"AED"},
		"withholding_amount":{"amount":"12.50","currency":"AED"},"end_to_end_id":"E2E-1","kind":"down_payment","down_payment_request_id":%q,"down_payment_request_number":"DP100000",
		"payment_method":"T","journal_entry_id":%q,"items":[]}],"next_page_token":"p2"}`, u(), u(), run, u(), u(), u(), u())
}

// stubExclusionsJSON is a page of what a run left out with a next page: a
// down payment request, for a reason the workspace does not know.
func stubExclusionsJSON(request uuid.UUID) string {
	return fmt.Sprintf(`{"items":[{"supplier_id":%q,"supplier_name":"Tyrell","reason_code":"sanctions_hit","down_payment_request_id":%q,"down_payment_request_number":"DP100000",
		"amount":{"amount":"5.00","currency":"AED"}}],"next_page_token":"e2"}`, uuid.NewString(), request)
}

// stubFilesJSON are a run's bank files: one that failed to go, one in a
// status the workspace does not know.
func stubFilesJSON(run uuid.UUID) string {
	u := uuid.NewString
	return fmt.Sprintf(`{"items":[{"id":%q,"version":4,"created_at":"2026-10-22T10:00:00Z","created_by":%q,"updated_at":"2026-10-22T10:00:00Z",
		"payment_run_id":%q,"payment_method":"T","format":"pain.001.001.09","status":"failed"},
		{"id":%q,"version":1,"created_at":"2026-10-22T10:00:00Z","created_by":%q,"updated_at":"2026-10-22T10:00:00Z",
		"payment_run_id":%q,"format":"pain.001.001.03","status":"on_hold"}]}`, u(), u(), run, u(), u(), run)
}

// operationJSON is an operation of the kind in the status with its
// resource, its total unknown and its error (a problem, or "").
func operationJSON(id uuid.UUID, kind, status, resource, problem string) string {
	u := uuid.NewString
	extra := ""
	if problem != "" {
		extra = `,"error":` + problem
	}
	return fmt.Sprintf(`{"id":%q,"kind":%q,"status":%q,"progress":{"done":41000,"total":null},"resource":%s,"version":7,
		"created_at":"2026-10-22T10:00:00Z","created_by":%q,"updated_at":"2026-10-22T10:00:00Z","updated_by":%q%s}`, id, kind, status, resource, u(), u(), extra)
}

// TestRunsStubbed: the API's rarer answers, from a stub behind the real
// sign-in: a list with a next page, a status and a house bank the
// workspace does not know and names that cannot be read; a run being
// released with a page more of payments and of exclusions, values the
// workspace does not know, a file that failed to go, offered again; its
// release stopped, its total unknown, its error explained and its
// resumption offered; a run whose release's request was withdrawn, so
// the second approval is offered again; a proposal whose operation names
// no run.
func TestRunsStubbed(t *testing.T) {
	releasing, proposed, request, op := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	nothing := `{"type":"about:blank","title":"Stubbed","status":409,"code":"payments.nothing_to_pay"}`
	withdrawn := fmt.Sprintf(`,"release_approval":{"request_id":%q,"status":"withdrawn","requested_total":{"amount":"1250.00","currency":"AED"}}`, request)
	st := &stubbed{routes: map[string]http.HandlerFunc{
		"GET /v1/me":              answer(200, meJSON(permReadRuns, permProposeRuns, permReleaseRuns, permTransmitFiles, permReadOperations, permReadCompanies)),
		"GET /v1/principal-names": answer(500, problemJSON(500, "platform.internal")),
		"GET /v1/legal-entities":  answer(200, fmt.Sprintf(`{"items":[{"id":%q,"code":"3000","name":"Acme FZE","country_code":"AE","local_currency":"AED","status":"active"}]}`, uuid.New())),
		"GET /v1/house-banks":     answer(500, problemJSON(500, "platform.internal")),
		"GET /v1/payment-runs":    answer(200, `{"items":[`+runJSON(releasing, "on_hold", fmt.Sprintf(`,"house_bank_id":%q`, uuid.New()))+`],"next_page_token":"r2"}`),
		"GET /v1/payment-runs/" + releasing.String(): func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("ETag", `"3"`)
			answer(200, runJSON(releasing, "releasing", fmt.Sprintf(`,"release_operation_id":%q`, op)))(w, r)
		},
		"GET /v1/payment-runs/" + releasing.String() + "/payments":   answer(200, stubPaymentsJSON(releasing)),
		"GET /v1/payment-runs/" + releasing.String() + "/exclusions": answer(200, stubExclusionsJSON(request)),
		"GET /v1/payment-runs/" + proposed.String():                  answer(200, runJSON(proposed, "proposed", withdrawn)),
		"GET /v1/payment-runs/" + proposed.String() + "/":            answer(200, `{"items":[]}`),
		"GET /v1/bank-files":            answer(200, stubFilesJSON(releasing)),
		"GET /v1/operations/":           answer(200, operationJSON(op, "payments.release", "failed", `"/v1/payment-runs/`+releasing.String()+`"`, nothing)),
		"POST /v1/payment-runs:propose": answer(202, operationJSON(op, "payments.propose", "queued", "null", "")),
	}}
	_, b, token := stubStack(t, st)
	l := b.get("/payments?status=on_hold&page=r1").page(t)
	holds(t, "the list", l.mainText(), "Status on_hold", "Proposed by Unknown", "Total AED 1,250.00")
	more := ""
	for _, a := range l.all("a", "button") {
		more = attrOf(a, "href")
	}
	if more != "/payments?page=r2&status=on_hold" {
		t.Errorf("next page: got %q", more)
	}
	p := b.get(runPath(releasing)).page(t)
	holds(t, "the run being released", p.mainText(), "Status Being released", "AED 1,250.00 AED 25.00 AED 12.50 T 0 E2E-1 Yes", "Down payment",
		"sanctions_hit", "Status Not sent", "Format pain.001.001.09", "Payment method T", "Status on_hold", "More payments", "More left out")
	links := map[string]bool{}
	for _, a := range p.all("a", "button") {
		links[attrOf(a, "href")] = true
	}
	for _, want := range []string{runPath(releasing) + "?payments_page=p2", runPath(releasing) + "?exclusions_page=e2"} {
		if !links[want] {
			t.Errorf("next page link %q: not in %v", want, links)
		}
	}
	b.get(runPath(releasing) + "?payments_page=p2&exclusions_page=e2")
	asked := map[string]bool{}
	for _, q := range st.queries {
		asked[q.Get("page_token")] = true
	}
	if !asked["p2"] || !asked["e2"] {
		t.Errorf("the next pages asked: got %v, want p2 and e2", asked)
	}
	if f := runForm(p, releasing, "transmit"); f == nil || field(f, "if_match") != `"4"` {
		t.Error("the file that failed to go: not offered again at its version")
	}
	pr := b.get(progressPath(releasing), "HX-Request", "true")
	holds(t, "the stopped release", text(pr.page(t).doc), "Releasing the run", "Failed", "41,000 done", "Why it stopped",
		"nothing is left to pay", "payments.nothing_to_pay", "The release stopped before it was done")
	if f := pr.page(t).form(runPath(releasing) + "/release"); field(f, "if_match") != `"3"` {
		t.Errorf("the resumption: If-Match %q, want the run's", field(f, "if_match"))
	}
	w := b.get(runPath(proposed)).page(t)
	holds(t, "the request withdrawn", w.mainText(), "State of the request Withdrawn", "Ask for the second approval", "Release the run")
	r := b.do(http.MethodPost, "/payments/propose", url.Values{"csrf_token": {token}, "idempotency_key": {uuid.NewString()},
		"legal_entity_id": {uuid.NewString()}, "run_date": {"2026-10-23"}, "due_by": {"2026-10-31"}, "currency": {"AED"}})
	if r.status != http.StatusSeeOther || r.location() != "/payments" {
		t.Errorf("a proposal whose operation names no run: got %d to %q, want the list", r.status, r.location())
	}
}

// TestRunsRefusedInPlace: a run's payments, exclusions and bank files the
// API refuses are said in their place, the rest of the page shown; an
// operation it refuses is said in the progress section, whose status,
// asked again, reloads the page.
func TestRunsRefusedInPlace(t *testing.T) {
	run, op := uuid.New(), uuid.New()
	forbidden := answer(403, problemJSON(403, "identity.permission_missing"))
	st := &stubbed{routes: map[string]http.HandlerFunc{
		"GET /v1/me":              answer(200, meJSON(permReadRuns, permReadOperations, permCancelOperations)),
		"GET /v1/principal-names": answer(200, `{"items":[]}`),
		"GET /v1/payment-runs/":   answer(200, runJSON(run, "released", fmt.Sprintf(`,"release_operation_id":%q`, op))),
		"GET /v1/payment-runs/" + run.String() + "/payments":   forbidden,
		"GET /v1/payment-runs/" + run.String() + "/exclusions": forbidden,
		"GET /v1/bank-files":  forbidden,
		"GET /v1/operations/": forbidden,
	}}
	_, b, _ := stubStack(t, st)
	r := b.get(runPath(run))
	status(t, "the run", r, http.StatusOK)
	p := r.page(t)
	for _, id := range []string{"payments-refusal-title", "exclusions-refusal-title", "files-refusal-title"} {
		found := false
		walk(p.doc, func(n *html.Node) { found = found || attrOf(n, "id") == id })
		if !found {
			t.Errorf("no refusal %s on the page", id)
		}
	}
	holds(t, "the run", p.mainText(), "Status Released", "You do not have the permission this needs.")
	holds(t, "the progress refused", text(b.get(progressPath(run), "HX-Request", "true").page(t).doc), "The progress could not be shown")
	poll := b.get(progressPath(run)+"?part=status", "HX-Request", "true")
	if got := poll.header.Get("HX-Redirect"); got != runPath(run) {
		t.Errorf("the status refused: HX-Redirect %q, want the run's page", got)
	}
}

// TestRunsUnavailable: an API that cannot answer, limits the rate or
// fails makes a run's page unavailable, never a half page; its 401 signs
// the person in again, back to the page a form was sent from; a run it
// refuses is said on a page of its own, one it does not know is not
// found.
func TestRunsUnavailable(t *testing.T) {
	id, op := uuid.New(), uuid.New()
	me := answer(200, meJSON(permReadRuns, permProposeRuns, permReleaseRuns, permReadOperations, permCancelOperations))
	challenge := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_user_authentication", acr_values="urn:erp:mfa"`)
		answer(401, problemJSON(401, "identity.mfa_required"))(w, r)
	}
	unavailable := answer(503, problemJSON(503, "platform.unavailable"))
	run := answer(200, runJSON(id, "releasing", fmt.Sprintf(`,"release_operation_id":%q`, op)))
	operation := answer(200, operationJSON(op, "payments.release", "running", `"/v1/payment-runs/`+id.String()+`"`, ""))
	path := runPath(id)
	for name, tc := range map[string]struct {
		routes   map[string]http.HandlerFunc
		method   string
		path     string
		status   int
		location string
	}{
		"list: who am I, 401":     {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodGet, "/payments", http.StatusSeeOther, "return=%2Fpayments"},
		"list: rate limited":      {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/payment-runs": answer(429, problemJSON(429, "request.rate_limited"))}, http.MethodGet, "/payments", http.StatusServiceUnavailable, ""},
		"run: who am I, 401":      {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodGet, path, http.StatusSeeOther, "return=%2Fpayments%2F" + id.String()},
		"run: unavailable":        {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/payment-runs/": unavailable}, http.MethodGet, path, http.StatusServiceUnavailable, ""},
		"run: refused":            {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/payment-runs/": answer(403, problemJSON(403, "identity.permission_missing"))}, http.MethodGet, path, http.StatusForbidden, ""},
		"run: payments 503":       {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/payment-runs/" + id.String() + "/payments": unavailable, "GET /v1/payment-runs/": run}, http.MethodGet, path, http.StatusServiceUnavailable, ""},
		"run: exclusions 503":     {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/payment-runs/" + id.String() + "/payments": answer(200, `{"items":[]}`), "GET /v1/payment-runs/" + id.String() + "/exclusions": unavailable, "GET /v1/payment-runs/": run}, http.MethodGet, path, http.StatusServiceUnavailable, ""},
		"run: files 503":          {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/payment-runs/" + id.String() + "/": answer(200, `{"items":[]}`), "GET /v1/payment-runs/": run, "GET /v1/bank-files": unavailable}, http.MethodGet, path, http.StatusServiceUnavailable, ""},
		"run: no id":              {map[string]http.HandlerFunc{}, http.MethodGet, "/payments/x", http.StatusNotFound, ""},
		"progress: who am I, 401": {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodGet, path + "/progress", http.StatusSeeOther, "return=%2Fpayments%2F" + id.String() + "%2Fprogress"},
		"progress: unavailable":   {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/payment-runs/": run, "GET /v1/operations/": unavailable}, http.MethodGet, path + "/progress", http.StatusServiceUnavailable, ""},
		"decision: who am I, 401": {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodPost, path + "/release", http.StatusSeeOther, "return=%2Fpayments%2F" + id.String()},
		"decision: rate limited":  {map[string]http.HandlerFunc{"GET /v1/me": me, "POST /v1/payment-runs/": answer(429, problemJSON(429, "request.rate_limited"))}, http.MethodPost, path + "/release", http.StatusServiceUnavailable, ""},
		"propose: who am I, 401":  {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodPost, "/payments/propose", http.StatusSeeOther, "return=%2Fpayments"},
		"propose: unavailable":    {map[string]http.HandlerFunc{"GET /v1/me": me, "POST /v1/payment-runs:propose": unavailable}, http.MethodPost, "/payments/propose", http.StatusServiceUnavailable, ""},
		"cancel: who am I, 401":   {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodPost, path + "/cancel", http.StatusSeeOther, "return=%2Fpayments%2F" + id.String()},
		"cancel: no operation":    {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/operations/": unavailable}, http.MethodPost, path + "/cancel", http.StatusServiceUnavailable, ""},
		"cancel: unavailable":     {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/operations/": operation, "POST /v1/operations/": unavailable}, http.MethodPost, path + "/cancel", http.StatusServiceUnavailable, ""},
		"cancel: refused": {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/operations/": operation, "GET /v1/payment-runs/": run,
			"POST /v1/operations/": answer(409, problemJSON(409, "platform.operation_not_cancellable"))}, http.MethodPost, path + "/cancel", http.StatusConflict, ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, b, token := stubStack(t, &stubbed{routes: tc.routes})
			var r response
			if tc.method == http.MethodGet {
				r = b.get(tc.path)
			} else {
				r = b.do(tc.method, tc.path, url.Values{"csrf_token": {token}, "if_match": {`"1"`}, "idempotency_key": {uuid.NewString()},
					"operation": {op.String()}, "legal_entity_id": {uuid.NewString()}, "house_bank_id": {uuid.NewString()}, "run_date": {"2026-10-23"},
					"due_by": {"2026-10-31"}})
			}
			if r.status != tc.status || !strings.HasSuffix(r.location(), tc.location) {
				t.Errorf("got %d to %q, want %d to %q", r.status, r.location(), tc.status, tc.location)
			}
		})
	}
}
