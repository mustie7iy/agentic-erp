package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// fiscalPeriodJSON is a posting period of 2026 in the status (extra:
// further fields, a leading comma each).
func fiscalPeriodJSON(id, company, ledger uuid.UUID, period int, status, extra string) string {
	u := uuid.NewString
	return fmt.Sprintf(`{"id":%q,"legal_entity_id":%q,"ledger_id":%q,"calendar_id":%q,"fiscal_year":2026,"period":%d,"start_date":"2026-03-01",
		"end_date":"2026-03-31","is_special":false,"status":%q,"version":4,"updated_at":"2026-10-22T10:00:00Z","updated_by":%q%s}`,
		id, company, ledger, u(), period, status, u(), extra)
}

// ledgerJSON is a ledger the companies keep.
func ledgerJSON(id uuid.UUID, code string, leading bool, companies ...uuid.UUID) string {
	ids := make([]string, len(companies))
	for i, c := range companies {
		ids[i] = c.String()
	}
	return fmt.Sprintf(`{"id":%q,"code":%q,"name":"Ledger %s","accounting_principle":"IFRS","is_leading":%v,"legal_entity_ids":[%s],"version":1,
		"created_at":"2026-10-22T10:00:00Z","created_by":%q}`, id, code, code, leading, quoted(ids), uuid.NewString())
}

// moneyJSON is an amount in EUR.
func moneyJSON(amount string) string { return fmt.Sprintf(`{"amount":%q,"currency":"EUR"}`, amount) }

// revaluationJSON is a revaluation run of the key date, without entries
// (no item had a difference), with its items when items is given.
func revaluationJSON(id uuid.UUID, items string) string {
	return fmt.Sprintf(`{"id":%q,"legal_entity_id":%q,"ledger_id":%q,"key_date":"2026-03-31","rate_type":"spot","status":"reversed","item_count":1,
		"created_at":"2026-10-22T10:00:00Z","created_by":%q%s}`, id, uuid.NewString(), uuid.NewString(), uuid.NewString(), items)
}

// balanceItemJSON is a flagged account's balance valued at zero without
// a rate, in a direction the workspace does not know.
func balanceItemJSON() string {
	m := moneyJSON("0.00")
	return fmt.Sprintf(`{"account_id":%q,"amount":%s,"amount_lc":%s,"amount_gc":%s,"valued_lc":%s,"difference_lc":%s,"valued_gc":%s,"difference_gc":%s,
		"direction":"sideways"}`, uuid.NewString(), m, m, m, m, m, m, m)
}

// TestCloseStubbed: the API's rarer answers, from a stub behind the real
// sign-in: a list with a next page whose ledgers, names and periods'
// status the workspace cannot read or does not know; a period in that
// status, offered no decision; a preview that values a balance without a
// rate and without a difference, its accounts unreadable; the company's
// runs with a next page; a run that posted no entries, in values the
// workspace does not know.
func TestCloseStubbed(t *testing.T) {
	company, ledger, period, run := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	broken := answer(500, problemJSON(500, "platform.internal"))
	st := &stubbed{routes: map[string]http.HandlerFunc{
		"GET /v1/me": answer(200, meJSON(permReadPeriods, permManagePeriods, permLockPeriods, permReadAccounts, permReadEntries, permPostEntries,
			permReadCompanies)),
		"GET /v1/principal-names":   broken,
		"GET /v1/ledgers":           broken,
		"GET /v1/accounts":          broken,
		"GET /v1/legal-entities":    answer(200, fmt.Sprintf(`{"items":[{"id":%q,"code":"3000","name":"Acme FZE","country_code":"AE","local_currency":"AED","status":"active"}]}`, company)),
		"GET /v1/fiscal-periods":    answer(200, `{"items":[`+fiscalPeriodJSON(period, company, ledger, 3, "frozen", "")+`],"next_page_token":"f2"}`),
		"GET /v1/fiscal-periods/":   answer(200, fiscalPeriodJSON(period, company, ledger, 3, "frozen", "")),
		"GET /v1/revaluation-runs":  answer(200, `{"items":[`+revaluationJSON(run, "")+`],"next_page_token":"v2"}`),
		"GET /v1/revaluation-runs/": answer(200, revaluationJSON(run, `,"items":[`+balanceItemJSON()+`],"totals":[]`)),
		"POST /v1/revaluation-runs/preview": answer(200, fmt.Sprintf(`{"legal_entity_id":%q,"ledger_id":%q,"key_date":"2026-03-31","fiscal_year":2026,
			"fiscal_period":3,"rate_type":"spot","items":[%s],"totals":[]}`, company, ledger, balanceItemJSON())),
	}}
	_, b, token := stubStack(t, st)
	l := b.get("/close?fiscal_year=2026").page(t)
	holds(t, "the list", l.mainText(), "Period 3 of 2026", "frozen", "Unknown")
	if strings.Contains(l.mainText(), "Ledger 0L") {
		t.Error("the list offers ledgers it could not read")
	}
	more := ""
	for _, a := range l.all("a", "button") {
		more = attrOf(a, "href")
	}
	want := "/close?" + (url.Values{"fiscal_year": {"2026"}, "page": {"f2"}}).Encode()
	if more != want {
		t.Errorf("next page: got %q, want %q", more, want)
	}
	p := b.get(periodPath(period)).page(t)
	holds(t, "a period in a status the workspace does not know", p.mainText(), "Status frozen", "Ledger "+ledger.String(), "Last changed by Unknown")
	if len(p.all("form", "decide")) != 0 || strings.Contains(p.mainText(), "You may read this period") {
		t.Error("a period in a status the workspace does not know: offered a decision, or told why not")
	}
	rv := b.get("/close/revaluation?page=v1").page(t)
	holds(t, "the runs", rv.mainText(), "reversed", "More revaluations")
	r := b.do(http.MethodPost, "/close/revaluation/preview", url.Values{"csrf_token": {token}, "idempotency_key": {uuid.NewString()},
		"legal_entity_id": {company.String()}, "key_date": {"2026-03-31"}})
	status(t, "the preview", r, http.StatusOK)
	holds(t, "the preview", r.page(t).mainText(), "Rate type spot", "No item has a difference: the run posts no entry.", "Account balance", "None", "sideways")
	runPage := b.get(revaluationRunPath(run)).page(t)
	holds(t, "a run without entries", runPage.mainText(), "Status reversed", "Rate type spot", "No item had a difference, so the run posted no entry.")
}

// TestChecksStubbed: the checks' rarer answers: the period of a parallel
// ledger, reconciled with the leading one; more GR/IR positions,
// differences and statements than a check shows; a ledger whose trial
// balance does not net to zero; a statement after a gap.
func TestChecksStubbed(t *testing.T) {
	company, leading, local, other := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	m := moneyJSON("5.00")
	st := &stubbed{routes: map[string]http.HandlerFunc{
		"GET /v1/me": answer(200, meJSON(permReadAccounts, permReadEntries, permReadStatements)),
		"GET /v1/ledgers": answer(200, `{"items":[`+ledgerJSON(local, "2L", false, company)+","+ledgerJSON(other, "3L", false)+","+
			ledgerJSON(leading, "0L", true, company)+`]}`),
		"GET /v1/gr-ir-balances": answer(200, fmt.Sprintf(`{"items":[{"legal_entity_id":%q,"purchase_order_id":%q,"po_item_no":2,"account_id":%q,"account_number":"219000","account_name":"GR/IR clearing",
			"received_quantity":{"value":"3","unit":"KG"},"invoiced_quantity":{"value":"0","unit":"KG"},"cleared_quantity":{"value":"0","unit":"KG"},"migrated_quantity":{"value":"0","unit":"KG"},"balance":%s,"balance_lc":%s,"balance_gc":%s,
			"last_posting_date":"2026-03-05"}],"next_page_token":"g2"}`, company, uuid.New(), uuid.New(), m, m, m)),
		"GET /v1/ledger-reconciliation": answer(200, fmt.Sprintf(`{"ledger_id":%q,"compare_ledger_id":%q,"trial_balances":[
			{"ledger_id":%q,"debit_lc":%s,"credit_lc":%s,"debit_gc":%s,"credit_gc":%s,"balanced":false}],
			"items":[{"account_id":%q,"currency":"EUR","balance":%s,"compare_balance":%s,"difference":%s,"difference_lc":%s,"difference_gc":%s}],
			"next_page_token":"l2"}`, local, leading, local, m, m, m, m, uuid.New(), m, m, m, m, m)),
		"GET /v1/bank-statements": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("status") == "gap" {
				answer(200, `{"items":[`+statementJSON(uuid.New(), "gap")+`]}`)(w, r)
				return
			}
			answer(200, `{"items":[],"next_page_token":"s2"}`)(w, r)
		},
	}}
	_, b, _ := stubStack(t, st)
	q := url.Values{"legal_entity_id": {company.String()}, "ledger_id": {local.String()}, "fiscal_year": {"2026"}, "period": {"3"}}
	got := b.get("/close/checks?"+q.Encode(), "HX-Request", "true")
	status(t, "the checks", got, http.StatusOK)
	holds(t, "the checks", text(got.page(t).doc), "3 KG 0 KG", "Ledger 2L compared with ledger 0L in this period.", "Debits and credits do not net to zero",
		"There are more than are shown here.", "Gap before it")
	if strings.Contains(text(got.page(t).doc), "Back to the period") {
		t.Error("a section without its period: links back to one")
	}
	asked := map[string]string{}
	for _, v := range st.queries {
		if v.Has("compare_ledger_id") {
			asked["ledger"], asked["compare"] = v.Get("ledger_id"), v.Get("compare_ledger_id")
		}
	}
	if asked["ledger"] != local.String() || asked["compare"] != leading.String() {
		t.Errorf("reconciled: got %v, want 2L with 0L", asked)
	}
}

// statementJSON is a statement of a house bank in the status.
func statementJSON(id uuid.UUID, status string) string {
	u := uuid.NewString
	return fmt.Sprintf(`{"id":%q,"version":1,"created_at":"2026-10-22T10:00:00Z","created_by":%q,"updated_at":"2026-10-22T10:00:00Z",
		"house_bank_id":%q,"house_bank_code":"MAIN","legal_entity_id":%q,"format":"mt940","file_sha256":"%s","file_position":1,"statement_ref":"S9","sequence":"9",
		"account":"DE89370400440532013000","opening_balance":%s,"opening_date":"2026-04-16","closing_balance":%s,"closing_date":"2026-04-17",
		"line_count":0,"status":%q}`, id, u(), u(), u(), strings.Repeat("a", 64), moneyJSON("1.00"), moneyJSON("1.00"), status)
}

// TestCloseUnavailable: an API that cannot answer, limits the rate or
// fails makes a close page unavailable, never a half page; its 401 signs
// the person in again, back to the page a form was sent from.
func TestCloseUnavailable(t *testing.T) {
	id, leading := uuid.New(), uuid.New()
	me := answer(200, meJSON(permReadPeriods, permManagePeriods, permLockPeriods, permReadAccounts, permReadEntries, permPostEntries, permReadCompanies,
		permReadStatements))
	challenge := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_user_authentication", acr_values="urn:erp:mfa"`)
		answer(401, problemJSON(401, "identity.mfa_required"))(w, r)
	}
	unavailable := answer(503, problemJSON(503, "platform.unavailable"))
	limited := answer(429, problemJSON(429, "request.rate_limited"))
	empty := answer(200, `{"items":[]}`)
	ledgers := answer(200, `{"items":[`+ledgerJSON(leading, "0L", true, id)+","+ledgerJSON(uuid.New(), "2L", false, id)+`]}`)
	checks := "/close/checks?" + url.Values{"legal_entity_id": {id.String()}, "ledger_id": {leading.String()}, "fiscal_year": {"2026"}, "period": {"3"}}.Encode()
	path := periodPath(id)
	for name, tc := range map[string]struct {
		routes   map[string]http.HandlerFunc
		method   string
		path     string
		status   int
		location string
	}{
		"list: who am I, 401":        {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodGet, "/close", http.StatusSeeOther, "return=%2Fclose"},
		"list: rate limited":         {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/fiscal-periods": limited}, http.MethodGet, "/close", http.StatusServiceUnavailable, ""},
		"period: who am I, 401":      {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodGet, path, http.StatusSeeOther, "return=%2Fclose%2Fperiods%2F" + id.String()},
		"period: unavailable":        {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/fiscal-periods/": unavailable}, http.MethodGet, path, http.StatusServiceUnavailable, ""},
		"decision: who am I, 401":    {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodPost, path + "/close", http.StatusSeeOther, "return=%2Fclose%2Fperiods%2F" + id.String()},
		"decision: rate limited":     {map[string]http.HandlerFunc{"GET /v1/me": me, "POST /v1/fiscal-periods/": limited}, http.MethodPost, path + "/lock", http.StatusServiceUnavailable, ""},
		"checks: who am I, 401":      {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodGet, checks, http.StatusSeeOther, "return=%2Fclose%2Fchecks"},
		"checks: GR/IR 503":          {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/gr-ir-balances": unavailable}, http.MethodGet, checks, http.StatusServiceUnavailable, ""},
		"checks: ledgers 503":        {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/gr-ir-balances": empty, "GET /v1/ledgers": ledgers, "GET /v1/ledger-reconciliation": unavailable}, http.MethodGet, checks, http.StatusServiceUnavailable, ""},
		"checks: statements 503":     {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/gr-ir-balances": empty, "GET /v1/ledgers": empty, "GET /v1/bank-statements": unavailable}, http.MethodGet, checks, http.StatusServiceUnavailable, ""},
		"revaluation: who am I, 401": {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodGet, "/close/revaluation", http.StatusSeeOther, "return=%2Fclose%2Frevaluation"},
		"revaluation: runs 503":      {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/revaluation-runs": unavailable}, http.MethodGet, "/close/revaluation", http.StatusServiceUnavailable, ""},
		"preview: who am I, 401":     {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodPost, "/close/revaluation/preview", http.StatusSeeOther, "return=%2Fclose%2Frevaluation"},
		"preview: unavailable":       {map[string]http.HandlerFunc{"GET /v1/me": me, "POST /v1/revaluation-runs/preview": unavailable}, http.MethodPost, "/close/revaluation/preview", http.StatusServiceUnavailable, ""},
		"post: who am I, 401":        {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodPost, "/close/revaluation/post", http.StatusSeeOther, "return=%2Fclose%2Frevaluation"},
		"post: unavailable":          {map[string]http.HandlerFunc{"GET /v1/me": me, "POST /v1/revaluation-runs": unavailable}, http.MethodPost, "/close/revaluation/post", http.StatusServiceUnavailable, ""},
		"run: who am I, 401":         {map[string]http.HandlerFunc{"GET /v1/me": challenge}, http.MethodGet, revaluationRunPath(id), http.StatusSeeOther, "return=%2Fclose%2Frevaluation%2F" + id.String()},
		"run: unavailable":           {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/revaluation-runs/": unavailable}, http.MethodGet, revaluationRunPath(id), http.StatusServiceUnavailable, ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, b, token := stubStack(t, &stubbed{routes: tc.routes})
			var r response
			if tc.method == http.MethodGet {
				r = b.get(tc.path)
			} else {
				r = b.do(tc.method, tc.path, url.Values{"csrf_token": {token}, "if_match": {`"1"`}, "idempotency_key": {uuid.NewString()},
					"legal_entity_id": {id.String()}, "key_date": {"2026-03-31"}})
			}
			if r.status != tc.status || !strings.HasSuffix(r.location(), tc.location) && !strings.Contains(r.location(), tc.location) {
				t.Errorf("got %d to %q, want %d to %q", r.status, r.location(), tc.status, tc.location)
			}
		})
	}
}

// TestPostRefusedBeforeTheAPI: a post whose company or key date is not
// valid is said on the page, as the preview's is, and nothing is posted;
// a preview without the permission to read accounts names them by id.
func TestPostRefusedBeforeTheAPI(t *testing.T) {
	st := &stubbed{routes: map[string]http.HandlerFunc{
		"GET /v1/me":               answer(200, meJSON(permReadEntries, permPostEntries)),
		"GET /v1/revaluation-runs": answer(200, `{"items":[]}`),
		"POST /v1/revaluation-runs": func(w http.ResponseWriter, r *http.Request) {
			t.Error("the post reached the API")
			answer(500, problemJSON(500, "platform.internal"))(w, r)
		},
		"POST /v1/revaluation-runs/preview": answer(200, fmt.Sprintf(`{"legal_entity_id":%q,"ledger_id":%q,"key_date":"2026-03-31","fiscal_year":2026,
			"fiscal_period":3,"rate_type":"average","items":[%s],"totals":[]}`, uuid.New(), uuid.New(), balanceItemJSON())),
	}}
	_, b, token := stubStack(t, st)
	r := b.do(http.MethodPost, "/close/revaluation/post", url.Values{"csrf_token": {token}, "idempotency_key": {uuid.NewString()}, "key_date": {"x"}})
	status(t, "a post without its company", r, http.StatusUnprocessableEntity)
	// Without the accounts' permission a preview names its accounts by id.
	p := b.do(http.MethodPost, "/close/revaluation/preview", url.Values{"csrf_token": {token}, "idempotency_key": {uuid.NewString()},
		"legal_entity_id": {uuid.NewString()}, "key_date": {"2026-03-31"}})
	status(t, "a preview without the accounts", p, http.StatusOK)
	holds(t, "a preview without the accounts", p.page(t).mainText(), "Average rate", "Account balance")
}
