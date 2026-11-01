package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// rulesForm is the statement's page's form running the rules again, nil
// for none.
func rulesForm(p page, statement uuid.UUID) map[string]string {
	for _, f := range p.all("form", "decide") {
		if attrOf(f, "action") == statementPath(statement)+"/"+rulesVerb {
			return map[string]string{"csrf_token": field(f, "csrf_token"), "if_match": field(f, "if_match"), "idempotency_key": field(f, "idempotency_key")}
		}
	}
	return nil
}

// sendForm posts the fields to path.
func sendForm(b *browser, path string, fields map[string]string) response {
	b.s.t.Helper()
	v := url.Values{}
	for k, val := range fields {
		v.Set(k, val)
	}
	return b.do(http.MethodPost, path, v)
}

// TestRulesAgain: the reconciler runs the company's rules on the statement
// again from its page; a replayed form gives the first answer, a stale one
// the API's refusal on the page; whoever may not match is not offered it
// (T73g2).
func TestRulesAgain(t *testing.T) {
	s, d := bankStack(t)
	b := s.browser()
	b.signIn()
	path := statementPath(d.statement) + "/" + rulesVerb
	p := b.get(statementPath(d.statement)).page(t)
	holds(t, "the statement", p.mainText(), "Run the matching rules again", "Download the bank's file of this statement")
	first := rulesForm(p, d.statement)
	if first == nil {
		t.Fatal("no form to run the rules again")
	}
	r := sendForm(b, path, first)
	status(t, "the rules run", r, http.StatusSeeOther)
	if got, want := r.location(), statementPath(d.statement)+"?done=rules"; got != want {
		t.Fatalf("the rules run's redirect: got %q, want %q", got, want)
	}
	holds(t, "after the run", b.get(r.location()).page(t).mainText(), "The matching rules ran again; the lines show what they found.")
	if got := linesOfStatement(t, d.statement); got != "posted unmatched unmatched proposed proposed" {
		t.Errorf("lines after the run: got %q, want them as they were", got)
	}
	if again := sendForm(b, path, first); again.status != http.StatusSeeOther {
		t.Errorf("the replayed form: got %d, want the first answer", again.status)
	}
	stale := first
	stale["idempotency_key"], stale["if_match"] = uuid.NewString(), `"99"`
	refused := sendForm(b, path, stale)
	status(t, "a stale form", refused, http.StatusPreconditionFailed)
	holds(t, "a stale form", refused.page(t).mainText(), "Bank statement S1", "changed since you opened it")
	if f := rulesForm(s.as(d.viewer).get(statementPath(d.statement)).page(t), d.statement); f != nil {
		t.Error("the viewer is offered the rules' run")
	}
	status(t, "no statement", sendForm(b, statementPath(uuid.New())+"/"+rulesVerb, first), http.StatusNotFound)
	status(t, "no id", sendForm(b, "/bank/statements/x/"+rulesVerb, first), http.StatusNotFound)
	missing := map[string]string{"csrf_token": first["csrf_token"], "idempotency_key": uuid.NewString()}
	status(t, "no If-Match", sendForm(b, path, missing), http.StatusBadRequest)
}

// TestStatementFile: the bank's file is downloaded byte for byte, never
// inline, never cached, under a sandbox; a person who may not read
// statements is told so, and a statement not found is not found (T73g2).
func TestStatementFile(t *testing.T) {
	s, d := bankStack(t)
	b := s.browser()
	b.signIn()
	r := b.get(statementPath(d.statement) + "/file")
	status(t, "the file", r, http.StatusOK)
	if r.body != statementS1 {
		t.Errorf("the file: got %q, want the bank's bytes", r.body)
	}
	for k, want := range map[string]string{"Content-Type": "application/octet-stream", "Cache-Control": "no-store",
		"Content-Disposition": `attachment; filename="statement-` + d.statement.String() + `.txt"`, "Content-Security-Policy": documentCSP,
		"X-Content-Type-Options": "nosniff"} {
		if got := r.header.Get(k); got != want {
			t.Errorf("%s: got %q, want %q", k, got, want)
		}
	}
	status(t, "the viewer's file", s.as(d.viewer).get(statementPath(d.statement)+"/file"), http.StatusOK)
	outsider := s.as(d.outsider).get(statementPath(d.statement) + "/file")
	status(t, "the outsider's file", outsider, http.StatusForbidden)
	holds(t, "the outsider's file", outsider.page(t).mainText(), "You do not have the permission this needs.")
	status(t, "no statement", b.get(statementPath(uuid.New())+"/file"), http.StatusNotFound)
	status(t, "no id", b.get("/bank/statements/x/file"), http.StatusNotFound)
}

// TestLineSearch: a debit's match by hand links to its search, a credit's
// does not; the search shows the line and what it found (nothing: the
// web suite releases no payment runs), its filter checks amounts and dates
// before the API is called, and a person who may not match lines sees the
// API's refusal in the results' place (T73g2).
func TestLineSearch(t *testing.T) {
	s, d := bankStack(t)
	b := s.browser()
	b.signIn()
	st := b.get(statementPath(d.statement)).page(t)
	search := linePaymentsPath(statementPath(d.statement), 3)
	holds(t, "line 3, a debit", lineCard(st, 3), "Find a payment the rules did not find")
	if strings.Contains(lineCard(st, 2), "Find a payment") {
		t.Error("line 2, a credit, offers the search")
	}
	r := b.get(search)
	status(t, "the search", r, http.StatusOK)
	p := r.page(t)
	checkInert(t, p)
	holds(t, "the search", p.mainText(), "S1", "Find a payment for line 3", "EUR -5.00", "Payments to choose from",
		"No payment of this bank is left to match with these filters.")
	for name, c := range map[string]struct {
		query string
		code  int
		want  string
	}{
		"amounts":             {"?amount_min=5&amount_max=5.5&run_date_from=2026-04-01&run_date_to=2026-04-30", http.StatusOK, "No payment"},
		"no amount":           {"?amount_min=abc", http.StatusBadRequest, "Enter an amount with at most the currency's decimals, without a sign."},
		"too many decimals":   {"?amount_max=5.001", http.StatusBadRequest, "Enter an amount"},
		"a sign":              {"?amount_min=-5", http.StatusBadRequest, "Enter an amount"},
		"no date":             {"?run_date_to=yesterday", http.StatusBadRequest, "Enter a date as year-month-day"},
		"a page of the token": {"?page=nonsense", http.StatusBadRequest, "request.invalid_page_token"},
	} {
		t.Run(name, func(t *testing.T) {
			got := b.get(search + c.query)
			status(t, name, got, c.code)
			holds(t, name, got.page(t).mainText(), c.want)
		})
	}
	viewer := s.as(d.viewer).get(search)
	status(t, "the viewer's search", viewer, http.StatusForbidden)
	holds(t, "the viewer's search", viewer.page(t).mainText(), "Find a payment for line 3", "You do not have the permission this needs.")
	outsider := s.as(d.outsider).get(search)
	status(t, "the outsider's search", outsider, http.StatusForbidden)
	for name, path := range map[string]string{"no line": linePaymentsPath(statementPath(d.statement), 99),
		"no statement": linePaymentsPath(statementPath(uuid.New()), 1), "no id": "/bank/statements/x/lines/1/payments",
		"line 0": linePaymentsPath(statementPath(d.statement), 0)} {
		status(t, name, b.get(path), http.StatusNotFound)
	}
	fr := b.get(search, "Accept-Language", "fr-CA").page(t).mainText()
	holds(t, "French", fr, "Trouver un paiement pour la ligne 3", "Paiements à choisir")
}

// foundPaymentsJSON is a page of two payments found, the first's payee's
// name planted, and a next page.
func foundPaymentsJSON(pay1, pay2 uuid.UUID) string {
	u := uuid.NewString
	one := func(id uuid.UUID, e2e, name, amount string) string {
		return fmt.Sprintf(`{"id":%q,"created_at":"2026-10-22T10:00:00Z","created_by":%q,"payment_run_id":%q,"supplier_id":%q,"supplier_name":%q,
			"bank_account_id":%q,"bank_account_last4":"3000","bank_account_scheme":"iban","bank_account_holder_name":"Holder","amount":{"amount":%q,"currency":"AED"},
			"end_to_end_id":%q,"journal_entry_id":%q,"journal_entry_number":"100042","items":[]}`, id, u(), u(), u(), name, u(), amount, e2e, u())
	}
	return `{"items":[` + one(pay1, "2000-PY1-000002", planted, "20.00") + "," + one(pay2, "2000-PY1-000001", "Tyrell", "19.50") +
		`],"next_page_token":"p2"}`
}

// TestLineSearchStubbed: what the search found, from a stub: the payments
// as the line's choices, their payees inert, the next page keeping the
// filter, htmx answered with the results alone; the payment chosen sent as
// the line's match by hand, and the API's refusal of it shown on the
// statement with the choice kept. A reconciled statement's line lists
// what was found without a form.
func TestLineSearchStubbed(t *testing.T) {
	id, pay1, pay2 := uuid.New(), uuid.New(), uuid.New()
	var sent map[string]any
	var query url.Values
	st := &stubbed{routes: map[string]http.HandlerFunc{
		"GET /v1/me":              answer(200, meJSON(permReadStatements, permMatchLines)),
		"GET /v1/principal-names": answer(200, `{"items":[]}`),
		"GET /v1/house-banks":     answer(200, `{"items":[]}`),
		"GET /v1/bank-statements/": func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/payments"):
				query = r.URL.Query()
				answer(200, foundPaymentsJSON(pay1, pay2))(w, r)
			case strings.HasSuffix(r.URL.Path, "/matches"):
				answer(200, `{"items":[]}`)(w, r)
			case strings.HasSuffix(r.URL.Path, "/rules"):
				answer(200, `{"posting_rules":[],"matching_rules":[]}`)(w, r)
			default:
				w.Header().Set("ETag", `"4"`)
				answer(200, strings.Replace(bankStatementJSON(id), `"status":"in_review"`, `"status":"imported"`, 1))(w, r)
			}
		},
		"POST /v1/bank-statements/": func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body) // the test's own request
			_ = json.Unmarshal(body, &sent)
			answer(422, problemJSON(422, "payments.statement_match_target"))(w, r)
		},
	}}
	_, b, _ := stubStack(t, st)
	search := linePaymentsPath(statementPath(id), 1)
	r := b.get(search + "?amount_min=19.5&run_date_from=2026-10-01")
	status(t, "the search", r, http.StatusOK)
	if got := query.Get("amount_min") + " " + query.Get("run_date_from") + " " + query.Get("page_size"); got != "19.50 2026-10-01 25" {
		t.Errorf("the API's filter: got %q", got)
	}
	p := r.page(t)
	checkInert(t, p)
	holds(t, "the search", p.mainText(), "2000-PY1-000002", "Pay now <script>", "AED 20.00", "paid to the account ending 3000",
		"journal entry 100042", "2000-PY1-000001 Tyrell AED 19.50")
	var values []string
	for _, in := range p.all("input", "radio__input") {
		values = append(values, attrOf(in, "value"))
	}
	if got, want := strings.Join(values, " "), "payments:"+pay1.String()+" payments:"+pay2.String(); got != want {
		t.Errorf("the choices: got %q, want %q", got, want)
	}
	more := ""
	for _, a := range p.all("a", "button") {
		more = attrOf(a, "href")
	}
	if want := search + "?amount_min=19.5&page=p2&run_date_from=2026-10-01"; more != want {
		t.Errorf("next page: got %q, want %q", more, want)
	}
	refusedFilter := b.get(search+"?amount_min=x", "HX-Request", "true")
	status(t, "htmx's refused filter", refusedFilter, http.StatusOK)
	holds(t, "htmx's refused filter", refusedFilter.body, "The search could not be run")
	section := b.get(search, "HX-Request", "true")
	if !strings.HasPrefix(section.body, `<div id="payment-search">`) {
		t.Errorf("htmx's answer: got %q, want the filter and results alone", section.body)
	}
	if got := section.header.Values("Vary"); !strings.Contains(strings.Join(got, ","), "HX-Request") {
		t.Errorf("Vary: got %v", got)
	}
	f := p.form(statementPath(id) + "/match")
	v := url.Values{"target": {"payments:" + pay2.String()}, "comment": {"The remittance names the invoice."}}
	for _, name := range []string{"csrf_token", "if_match", "idempotency_key", "line"} {
		v.Set(name, field(f, name))
	}
	refused := b.do(http.MethodPost, statementPath(id)+"/match", v)
	status(t, "the match", refused, http.StatusUnprocessableEntity)
	if got := fmt.Sprint(sent["payment_ids"]); got != "["+pay2.String()+"]" {
		t.Errorf("payment_ids sent: got %s", got)
	}
	rp := refused.page(t)
	holds(t, "the refusal", lineCard(rp, 1), "The payment you found "+pay2.String())
	for _, in := range rp.all("input", "radio__input") {
		if attrOf(in, "value") == "payments:"+pay2.String() && !hasAttr(in, "checked") {
			t.Error("the payment chosen is not kept")
		}
	}
}

// TestLineSearchClosed: a reconciled statement's line shows what the
// search found, without a form, saying why.
func TestLineSearchClosed(t *testing.T) {
	id, pay1, pay2 := uuid.New(), uuid.New(), uuid.New()
	st := &stubbed{routes: map[string]http.HandlerFunc{
		"GET /v1/me": answer(200, meJSON(permReadStatements, permMatchLines)),
		"GET /v1/bank-statements/": func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/payments") {
				answer(200, foundPaymentsJSON(pay1, pay2))(w, r)
				return
			}
			answer(200, strings.Replace(bankStatementJSON(id), `"status":"in_review"`, `"status":"reconciled"`, 1))(w, r)
		},
	}}
	_, b, _ := stubStack(t, st)
	p := b.get(linePaymentsPath(statementPath(id), 1)).page(t)
	holds(t, "the search", p.mainText(), "This line takes no match now", "2000-PY1-000001 Tyrell")
	for _, f := range p.all("form", "") {
		if attrOf(f, "action") == statementPath(id)+"/match" {
			t.Error("a reconciled statement's line is offered a match")
		}
	}
}
