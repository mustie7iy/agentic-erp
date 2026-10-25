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

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// bankStatementJSON is a statement reconciled, after a previous one, with
// a line matched by a rule to two candidates (payments), a line posted by
// hand and one of a status the workspace does not know.
func bankStatementJSON(id uuid.UUID) string {
	u := uuid.NewString
	return fmt.Sprintf(`{"id":%q,"version":4,"created_at":"2026-10-22T10:00:00Z","created_by":%q,"updated_at":"2026-10-22T10:00:00Z",
		"house_bank_id":%q,"legal_entity_id":%q,"format":"camt.053.001.08","file_sha256":"%s","file_position":1,"statement_ref":"STMT-9",
		"sequence":"9","account":"AE070331234567890123456","opening_balance":{"amount":"100.00","currency":"AED"},"opening_date":"2026-10-19",
		"closing_balance":{"amount":"80.00","currency":"AED"},"closing_date":"2026-10-20","line_count":3,"previous_statement_id":%q,
		"status":"in_review","reconciled_at":"2026-10-21T08:00:00Z","reconciled_by":%q,"lines":[
		{"line_no":1,"amount":{"amount":"-20.00","currency":"AED"},"is_reversal":false,"booking_date":"2026-10-20","value_date":"2026-10-20",
		 "transaction_code":"NTRF","bank_reference":"R1","owner_reference":"NONREF","text":"","status":"proposed","transactions":[]},
		{"line_no":2,"amount":{"amount":"10.00","currency":"AED"},"is_reversal":true,"booking_date":"2026-10-20","value_date":"2026-10-20",
		 "transaction_code":"RCHG","bank_reference":"R2","owner_reference":"NONREF","text":"","status":"posted","transactions":[]},
		{"line_no":3,"amount":{"amount":"-10.00","currency":"AED"},"is_reversal":false,"booking_date":"2026-10-20","value_date":"2026-10-20",
		 "transaction_code":"NMSC","bank_reference":"R3","owner_reference":"NONREF","text":"","status":"on_hold","transactions":[]}]}`,
		id, u(), u(), u(), strings.Repeat("a", 64), u(), u())
}

// bankMatchesJSON are the stubbed statement's matches: line 1 open with
// two candidates of the amount rule, line 2 posted by hand.
func bankMatchesJSON(id uuid.UUID, pay1, pay2, pay3 uuid.UUID) string {
	u := uuid.NewString
	return fmt.Sprintf(`{"items":[
		{"id":%q,"version":1,"created_at":"2026-10-22T10:00:00Z","created_by":%q,"updated_at":"2026-10-22T10:00:00Z","bank_statement_id":%q,
		 "line_no":1,"kind":"rule","rule":"amount","reason":"Two payments of 20.00.","status":"open","targets":[
		 {"candidate":1,"payment_id":%q},{"candidate":2,"payment_id":%q,"bank_file_id":%q},{"candidate":2,"payment_id":%q},{"candidate":3,"posting_rule":"X"}]},
		{"id":%q,"version":2,"created_at":"2026-10-22T10:00:00Z","created_by":%q,"updated_at":"2026-10-22T10:00:00Z","bank_statement_id":%q,
		 "line_no":2,"kind":"new_kind","reason":"By hand.","status":"matched","targets":[{"candidate":1,"posting_rule":"RCHG"}],
		 "posting_date":"2026-10-21","posting_reason":"Booked late.","approval_request_id":%q,"journal_entry_id":%q,"journal_entry_year":2026}]}`,
		u(), u(), id, pay1, pay2, u(), pay3, u(), u(), id, u(), u())
}

// TestBankStubbed: the API's rarer answers, from a stub behind the real
// sign-in: a list with a next page and its house banks unreadable, a
// statement whose rules and names are unreadable with a rule's open
// candidates matched by hand to the payments of one, values the
// workspace does not know shown as they are.
func TestBankStubbed(t *testing.T) {
	id, pay1, pay2, pay3 := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	var sent map[string]any
	st := &stubbed{routes: map[string]http.HandlerFunc{
		"GET /v1/me":              answer(200, meJSON(permReadStatements, permMatchLines, permPostLines, permImportStatements)),
		"GET /v1/principal-names": answer(500, problemJSON(500, "platform.internal")),
		"GET /v1/house-banks":     answer(500, problemJSON(500, "platform.internal")),
		"GET /v1/bank-statements": answer(200, `{"items":[`+bankStatementJSON(id)+`],"next_page_token":"t2"}`),
		"GET /v1/bank-statements/": func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/matches"):
				answer(200, bankMatchesJSON(id, pay1, pay2, pay3))(w, r)
			case strings.HasSuffix(r.URL.Path, "/rules"):
				answer(409, problemJSON(409, "config.not_bootstrapped"))(w, r)
			default:
				w.Header().Set("ETag", `"4"`)
				answer(200, bankStatementJSON(id))(w, r)
			}
		},
		"POST /v1/bank-statements/": func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body) // the test's own request
			_ = json.Unmarshal(body, &sent)
			answer(422, problemJSON(422, "payments.statement_match_target"))(w, r)
		},
	}}
	s, b, _ := stubStack(t, st)
	l := b.get("/bank?status=in_review&page=t1")
	status(t, "the list", l, http.StatusOK)
	lp := l.page(t)
	holds(t, "the list", lp.mainText(), "in_review", "House bank •••• 3456 Statement number 9")
	more := ""
	for _, a := range lp.all("a", "button") {
		more = attrOf(a, "href")
	}
	if more != "/bank?page=t2&status=in_review" {
		t.Errorf("next page: got %q", more)
	}
	if n := len(lp.all("select", "")); n != 0 { // no bank to filter by or to import for
		t.Errorf("selects: got %d, want none", n)
	}
	if !strings.Contains(s.logs.String(), "the house banks could not be read") {
		t.Error("the house banks' failure is not logged")
	}
	p := b.get(statementPath(id)).page(t)
	holds(t, "the statement", p.mainText(), "Status in_review", "File format camt.053.001.08", "Previous statement Open the previous statement",
		"Reconciled on 2026-10-21", "Reconciled by Unknown")
	holds(t, "line 1", lineCard(p, 1), "Matched by a rule", "Matching rule Amount", "Candidate 1: Payment "+pay1.String(),
		"Candidate 2: Payment "+pay2.String()+" in bank file", "Candidate 3: Posting rule X")
	holds(t, "line 2", lineCard(p, 2), "new_kind", "Reverses an earlier booking", "Why on that date Booked late.", "/ 2026")
	holds(t, "line 3", lineCard(p, 3), "on_hold")
	if !strings.Contains(s.logs.String(), "a statement's rules could not be read") {
		t.Error("the rules' failure is not logged")
	}
	var values []string
	for _, in := range p.all("input", "radio__input") {
		values = append(values, attrOf(in, "value"))
	}
	if got, want := strings.Join(values, " "), "payments:"+pay1.String()+" payments:"+pay2.String()+","+pay3.String(); got != want {
		t.Errorf("line 1's choices: got %q, want %q", got, want)
	}
	r := decideOn(b, id, 1, "match", url.Values{"target": {"payments:" + pay2.String() + "," + pay3.String()}, "comment": {"Both."}})
	status(t, "the match", r, http.StatusUnprocessableEntity)
	holds(t, "the match", r.page(t).mainText(), "The line may not clear what you chose")
	if got := fmt.Sprint(sent["payment_ids"]); got != "["+pay2.String()+" "+pay3.String()+"]" {
		t.Errorf("sent: got %v, want the second candidate's payments", sent)
	}
}

// reconJSON is a report of a house bank without a main bank account (no
// ledger balance), of a statement with a difference nothing explains, a
// next page, and lines waiting for an approval and of a proposal decided.
func reconJSON(bank uuid.UUID) string {
	u := uuid.NewString
	return fmt.Sprintf(`{"house_bank_id":%q,"legal_entity_id":%q,"currency":"EUR","summary":{"statements":2,
		"lines":{"unmatched":0,"proposed":0,"matched":0,"excluded":0,"pending_approval":1,"posted":0}},
		"items":[{"bank_statement_id":%q,"statement_ref":"R1","sequence":"1","status":"gap","opening_date":"2026-04-16",
		"opening_balance":{"amount":"0.00","currency":"EUR"},"figures":{"as_of":"2026-04-17","closing_balance":{"amount":"5.00","currency":"EUR"},
		"excluded":{"amount":"0.00","currency":"EUR"},"in_transit":{"amount":"0.00","currency":"EUR"},"posted_later":{"amount":"0.00","currency":"EUR"},
		"unexplained":{"amount":"-5.00","currency":"EUR"}},"line_counts":{"unmatched":0,"proposed":0,"matched":0,"excluded":0,"pending_approval":1,"posted":0},
		"lines":[{"line_no":1,"booking_date":"2026-04-17","amount":{"amount":"5.00","currency":"EUR"},"is_reversal":true,"transaction_code":"NTRF",
		"status":"pending_approval","match_kind":"manual","approval_request_id":%q,"proposal_outcome":"overridden"},
		{"line_no":2,"booking_date":"2026-04-17","amount":{"amount":"1.00","currency":"EUR"},"is_reversal":false,"transaction_code":"NTRF",
		"status":"unmatched","posting_date":"2026-04-18"}]}],"next_page_token":"r2"}`, bank, u(), u(), u())
}

// TestReconciliationStubbed: a report of a bank without a main bank
// account and a difference nothing explains, flagged; a line waiting
// for its posting's approval links to the request; a next page.
func TestReconciliationStubbed(t *testing.T) {
	bank := uuid.New()
	st := &stubbed{routes: map[string]http.HandlerFunc{
		"GET /v1/me":          answer(200, meJSON(permReadStatements)),
		"GET /v1/house-banks": answer(200, `{"items":[]}`),
		"GET /v1/house-banks/": func(w http.ResponseWriter, r *http.Request) {
			answer(200, reconJSON(bank))(w, r)
		},
	}}
	_, b, _ := stubStack(t, st)
	p := b.get(reportPath(bank) + "?closing_date_from=2026-04-01").page(t)
	holds(t, "the report", p.mainText(), bank.String(), "2 statements close in the period.",
		"Ledger: balance of the bank's account None: the house bank has no main bank account", "Unexplained EUR -5.00 To investigate",
		"Gap before it", "Reverses an earlier booking", "Agent's last proposal: Overridden by hand", "Open the approval request")
	if got := st.queries[len(st.queries)-1].Get("closing_date_from"); got != "2026-04-01" {
		t.Errorf("the period asked: got %q", got)
	}
	more := ""
	for _, a := range p.all("a", "button") {
		more = attrOf(a, "href")
	}
	if more != reportPath(bank)+"?closing_date_from=2026-04-01&page=r2" {
		t.Errorf("next page: got %q", more)
	}
	status(t, "the next page", b.get(more), http.StatusOK)
	if got := st.queries[len(st.queries)-1].Get("page_token"); got != "r2" {
		t.Errorf("the next page asked with token %q, want r2", got)
	}
}

// TestBankChoices: an inactive house bank is no bank to import for; a
// file of several statements goes on to the bank's statements; a person
// who may decide nothing is told so only while a line is open.
func TestBankChoices(t *testing.T) {
	active, inactive := uuid.New(), uuid.New()
	banks := map[uuid.UUID]gen.HouseBank{active: {ID: active, Code: "MAIN", AccountLast4: "3000", Currency: "EUR", Status: "active"},
		inactive: {ID: inactive, Code: "OLD", AccountLast4: "1111", Currency: "EUR", Status: "inactive"}}
	if got := bankOptions(banks, "", true); len(got) != 1 || got[0].Value != active.String() {
		t.Errorf("banks to import for: got %v, want MAIN alone", got)
	}
	if got := bankOptions(banks, inactive.String(), false); len(got) != 2 || !got[1].Selected {
		t.Errorf("banks to filter by: got %v, want both, OLD chosen", got)
	}
	two := gen.BankStatementImport{Statements: []gen.BankStatement{{ID: uuid.New()}, {ID: uuid.New()}}}
	if got, want := importedPath(active, two, "imported"), "/bank?done=imported&house_bank_id="+active.String(); got != want {
		t.Errorf("two statements: got %q, want %q", got, want)
	}
	reader := me{permissions: map[string]bool{permReadStatements: true}}
	for name, tc := range map[string]struct {
		status string
		lines  []string
		want   i18n.Key
	}{
		"open lines":    {"imported", []string{"posted", "unmatched"}, i18n.LinesNotYours},
		"decided lines": {"imported", []string{"posted", "pending_approval"}, ""},
		"reconciled":    {"reconciled", nil, i18n.StatementReconciledWhy},
	} {
		t.Run(name, func(t *testing.T) {
			var views []ui.LineView
			for _, l := range tc.lines {
				views = append(views, ui.LineView{Status: ui.Label{Raw: l}})
			}
			if got := linesWhy(gen.BankStatement{Status: tc.status}, views, reader); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBankUnavailable: an API that cannot answer, limits the rate or
// fails makes a bank page unavailable, never a half page; its 401 signs
// the person in again, back to the page a form was sent from.
func TestBankUnavailable(t *testing.T) {
	id := uuid.New()
	me := answer(200, meJSON(permReadStatements, permMatchLines, permImportStatements))
	challenge := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_user_authentication", acr_values="urn:erp:mfa"`)
		answer(401, problemJSON(401, "identity.mfa_required"))(w, r)
	}
	unavailable := answer(503, problemJSON(503, "platform.unavailable"))
	statement := answer(200, bankStatementJSON(id))
	path := statementPath(id)
	for name, tc := range map[string]struct {
		routes   map[string]http.HandlerFunc
		method   string
		path     string
		status   int
		location string
	}{
		"list: who am I, 401":        {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodGet, "/bank", http.StatusSeeOther, "return=%2Fbank"},
		"list: rate limited":         {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/house-banks": answer(200, `{"items":[]}`), "GET /v1/bank-statements": answer(429, problemJSON(429, "request.rate_limited"))}, http.MethodGet, "/bank", http.StatusServiceUnavailable, ""},
		"statement: who am I, 401":   {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodGet, path, http.StatusSeeOther, "return=%2Fbank%2Fstatements%2F" + id.String()},
		"statement: unavailable":     {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/bank-statements/": unavailable}, http.MethodGet, path, http.StatusServiceUnavailable, ""},
		"statement: the matches 503": {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/bank-statements/" + id.String() + "/matches": unavailable, "GET /v1/bank-statements/": statement}, http.MethodGet, path, http.StatusServiceUnavailable, ""},
		"statement: the matches refused": {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/bank-statements/" + id.String() + "/matches": answer(403, problemJSON(403, "identity.permission_missing")),
			"GET /v1/bank-statements/": statement}, http.MethodGet, path, http.StatusForbidden, ""},
		"statement: gone":           {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/bank-statements/" + id.String() + "/matches": answer(404, problemJSON(404, "request.not_found")), "GET /v1/bank-statements/": statement}, http.MethodGet, path, http.StatusNotFound, ""},
		"decision: who am I, 401":   {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodPost, path + "/exclude", http.StatusSeeOther, "return=%2Fbank%2Fstatements%2F" + id.String()},
		"decision: rate limited":    {map[string]http.HandlerFunc{"GET /v1/me": me, "POST /v1/bank-statements/": answer(429, problemJSON(429, "request.rate_limited"))}, http.MethodPost, path + "/exclude", http.StatusServiceUnavailable, ""},
		"report: who am I, 401":     {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodGet, reportPath(id), http.StatusSeeOther, "return=%2Fbank%2Fhouse-banks%2F" + id.String() + "%2Freconciliation"},
		"report: unavailable":       {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/house-banks": answer(200, `{"items":[]}`), "GET /v1/house-banks/": unavailable}, http.MethodGet, reportPath(id), http.StatusServiceUnavailable, ""},
		"upload: who am I, 401":     {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodPost, "/bank/statements", http.StatusSeeOther, "return=%2Fbank"},
		"upload: unavailable":       {map[string]http.HandlerFunc{"GET /v1/me": me, "POST /v1/bank-statements": unavailable}, http.MethodPost, "/bank/statements", http.StatusServiceUnavailable, ""},
		"upload too large: no call": {map[string]http.HandlerFunc{}, http.MethodPost, "/bank/statements?large", http.StatusRequestEntityTooLarge, ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, b, token := stubStack(t, &stubbed{routes: tc.routes})
			var r response
			switch {
			case tc.method == http.MethodGet:
				r = b.get(tc.path)
			case strings.HasPrefix(tc.path, "/bank/statements?"):
				large := "--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"s.sta\"\r\n\r\n" + strings.Repeat("x", maxUploadBody)
				r = b.raw(http.MethodPost, "/bank/statements", strings.NewReader(large), "multipart/form-data; boundary=b")
			case tc.path == "/bank/statements":
				r = uploadTo(b, token, id)
			default:
				r = b.do(tc.method, tc.path, url.Values{"csrf_token": {token}, "if_match": {`"1"`}, "idempotency_key": {uuid.NewString()},
					"comment": {"ok"}, "line": {"1"}})
			}
			if r.status != tc.status || !strings.HasSuffix(r.location(), tc.location) {
				t.Errorf("got %d to %q, want %d to %q", r.status, r.location(), tc.status, tc.location)
			}
		})
	}
}

// uploadTo posts an upload form with the token, for the bank.
func uploadTo(b *browser, token string, bank uuid.UUID) response {
	b.s.t.Helper()
	var body strings.Builder
	for k, v := range map[string]string{"csrf_token": token, "idempotency_key": uuid.NewString(), "house_bank_id": bank.String(), "format": "mt940"} {
		body.WriteString("--b\r\nContent-Disposition: form-data; name=\"" + k + "\"\r\n\r\n" + v + "\r\n")
	}
	body.WriteString("--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"s.sta\"\r\n\r\n:20:X\r\n--b--\r\n")
	return b.raw(http.MethodPost, "/bank/statements", strings.NewReader(body.String()), "multipart/form-data; boundary=b")
}
