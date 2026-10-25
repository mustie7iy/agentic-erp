package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// meAs is getCurrentPrincipal's answer for the person id with the
// permissions.
func meAs(id uuid.UUID, permissions ...string) string {
	return fmt.Sprintf(`{"id":%q,"tenant_id":%q,"kind":"human","display_name":"x","email":null,"all_companies":true,"legal_entity_ids":[],
		"permissions":[%s],"barred_permissions":[],"autonomy_level":null}`, id, uuid.NewString(), quoted(permissions))
}

// parkedJSON is a parked entry in the status with two lines of EUR 10.00
// (extra: further fields, a leading comma each).
func parkedJSON(id, company uuid.UUID, status, extra string) string {
	u := uuid.NewString
	return fmt.Sprintf(`{"id":%q,"version":2,"created_at":"2026-10-22T10:00:00Z","created_by":%q,"updated_at":"2026-10-22T10:00:00Z",
		"legal_entity_id":%q,"ledger_id":%q,"document_type":"SA","document_date":"2026-03-15","posting_date":"2026-03-15","currency":"EUR",
		"status":%q,"created_by_kind":"agent","lines":[{"line_no":1,"account_id":%q,"amount":%s},{"line_no":2,"account_id":%q,"amount":%s}]%s}`,
		id, u(), company, u(), status, u(), moneyJSON("10.00"), u(), moneyJSON("-10.00"), extra)
}

// journalEntryJSON is a posted entry of company (extra: further fields,
// a leading comma each), its lines in a currency the workspace sums
// nothing of.
func journalEntryJSON(id, company uuid.UUID, extra string) string {
	u := uuid.NewString
	return fmt.Sprintf(`{"id":%q,"legal_entity_id":%q,"ledger_id":%q,"leading_ledger":true,"fiscal_year":2026,"fiscal_period":3,
		"document_type":"SA","document_number":"100009","document_date":"2026-03-15","posting_date":"2026-03-15","currency":"EUR",
		"local_currency":"EUR","group_currency":"EUR","rate_tc_lc":"1","rate_lc_gc":"1","source_context":"treasury","lines":[
		{"line_no":1,"account_id":%q,"amount":%s,"amount_lc":%s,"amount_gc":%s},
		{"line_no":2,"account_id":%q,"amount":{"amount":"-10","currency":"EUR"},"amount_lc":%s,"amount_gc":%s}],
		"posted_at":"2026-03-15T10:00:00Z","posted_by":%q%s}`, id, company, u(), u(), moneyJSON("10.00"), moneyJSON("10.00"), moneyJSON("10.00"),
		u(), moneyJSON("-10.00"), moneyJSON("-10.00"), u(), extra)
}

// TestJournalStubbed: the API's rarer answers, from a stub behind the
// real sign-in: a list with a next page whose companies, names and
// statuses the workspace cannot read or does not know; the journal with
// a next page whose centers, ledgers and sources it cannot read or does
// not know; an entry whose amounts do not sum, with a tax date, in a
// ledger it cannot name; a parked entry posted from the ledger, and one
// waiting for an approval request it cannot read, or one no longer
// pending.
func TestJournalStubbed(t *testing.T) {
	company, entry, parked, waiting, decided, request := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	me := uuid.New()
	broken := answer(500, problemJSON(500, "platform.internal"))
	st := &stubbed{routes: map[string]http.HandlerFunc{
		"GET /v1/me": answer(200, meAs(me, permReadEntries, permParkEntries, permPostEntries, permReadCompanies, permReadAccounts, permReadCenters,
			permCancelRequests)),
		"GET /v1/principal-names": broken,
		"GET /v1/legal-entities":  broken,
		"GET /v1/ledgers":         broken,
		"GET /v1/accounts":        broken,
		"GET /v1/cost-centers":    broken,
		"GET /v1/profit-centers":  broken,
		"GET /v1/parked-journal-entries": answer(200, `{"items":[`+parkedJSON(parked, company, "frozen", `,"owner_id":"`+uuid.NewString()+`"`)+
			`],"next_page_token":"n2"}`),
		"GET /v1/journal-entries": answer(200, `{"items":[`+journalEntryJSON(entry, company, `,"reversal_of_id":"`+uuid.NewString()+`"`)+
			`],"next_page_token":"j2"}`),
		"GET /v1/journal-entries/": answer(200, journalEntryJSON(entry, company, `,"tax_date":"2026-03-16","reversed_by_id":"`+uuid.NewString()+`"`)),
		"GET /v1/parked-journal-entries/": func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, waiting.String()):
				answer(200, parkedJSON(waiting, company, "pending_approval", `,"owner_id":"`+me.String()+`","approval_request_id":"`+request.String()+`"`))(w, r)
			case strings.HasSuffix(r.URL.Path, decided.String()):
				answer(200, parkedJSON(decided, company, "pending_approval", `,"owner_id":"`+me.String()+`","approval_request_id":"`+uuid.NewString()+`"`))(w, r)
			default:
				answer(200, parkedJSON(parked, company, "posted", `,"tax_date":"2026-03-16","journal_entry_id":"`+entry.String()+`"`))(w, r)
			}
		},
		"GET /v1/approval-requests/": func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, request.String()) {
				broken(w, r)
				return
			}
			answer(200, strings.Replace(requestJSON(uuid.New()), `"status":"on_hold"`, `"status":"approved"`, 1))(w, r)
		},
	}}
	_, b, _ := stubStack(t, st)
	l := b.get("/journal?page=n1").page(t).mainText()
	holds(t, "the list", l, "frozen", "Unknown", "More entries")
	if strings.Contains(l, "Company") {
		t.Error("the list names a company it could not read")
	}
	j := b.get("/journal/entries?legal_entity_id=" + company.String()).page(t)
	holds(t, "the journal", j.mainText(), "100009", "treasury", "A reversal", "More entries")
	if strings.Contains(j.mainText(), "Cost center") || strings.Contains(j.mainText(), "Debits") {
		t.Error("the journal offers centers it could not read, or sums what does not sum")
	}
	e := b.get(entryPath(entry)).page(t).mainText()
	holds(t, "the entry", e, "Tax date 2026-03-16", "Origin treasury", "Open the entry that reversed it", "Posted by Unknown")
	p := b.get(parkedPath(parked)).page(t).mainText()
	holds(t, "a posted entry", p, "Status Posted", "Tax date 2026-03-16", "Open the journal entry it was posted as")
	w := b.get(parkedPath(waiting)).page(t)
	holds(t, "a request that cannot be read", w.mainText(), "The entry waits for another person's approval in the approvals inbox.")
	if parkedFormOf(w, waiting, "withdraw") != nil {
		t.Error("a request that cannot be read: offered its withdrawal")
	}
	if parkedFormOf(b.get(parkedPath(decided)).page(t), decided, "withdraw") != nil {
		t.Error("a request no longer pending: offered its withdrawal")
	}
}

// TestEntryLookups: a chart longer than a page looks a line's account up
// by its number; a lookup that fails says so on its line, and nothing is
// sent; a partner named by its id is sent as it is; without the
// companies, a new entry has no company to choose.
func TestEntryLookups(t *testing.T) {
	company, account, partner := uuid.New(), uuid.New(), uuid.New()
	var parked string
	st := &stubbed{routes: map[string]http.HandlerFunc{
		"GET /v1/me": answer(200, meJSON(permReadEntries, permParkEntries, permReadCompanies, permReadAccounts)),
		"GET /v1/legal-entities": answer(200, fmt.Sprintf(`{"items":[{"id":%q,"version":1,"created_at":"2026-10-22T10:00:00Z","created_by":%q,
			"updated_at":"2026-10-22T10:00:00Z","code":"3000","name":"Acme FZE","country_code":"AE","local_currency":"AED","fiscal_calendar_id":%q,
			"status":"active","extensions":{}}]}`, company, uuid.New(), uuid.New())),
		"GET /v1/accounts": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("number") == "" {
				answer(200, `{"items":[],"next_page_token":"a2"}`)(w, r)
				return
			}
			answer(200, fmt.Sprintf(`{"items":[{"id":%q,"version":1,"created_at":"2026-10-22T10:00:00Z","created_by":%q,"updated_at":"2026-10-22T10:00:00Z",
				"chart_id":%q,"number":"700000","name":"Far","account_type":"expense","is_balance_sheet":false,"reconciliation_kind":"none","status":"active",
				"company_settings":[]}]}`, account, uuid.New(), uuid.New()))(w, r)
		},
		"GET /v1/cost-centers":   answer(503, problemJSON(503, "platform.unavailable")),
		"GET /v1/segments":       answer(200, `{"items":[]}`),
		"GET /v1/profit-centers": answer(200, `{"items":[]}`),
		"POST /v1/parked-journal-entries": func(w http.ResponseWriter, r *http.Request) {
			parked = r.URL.Path
			answer(201, parkedJSON(uuid.New(), company, "parked", ""))(w, r)
		},
	}}
	_, b, token := stubStack(t, st)
	v := url.Values{"csrf_token": {token}, "idempotency_key": {uuid.NewString()}, "legal_entity_id": {company.String()}, "document_type": {"SA"},
		"document_date": {"2026-03-15"}, "posting_date": {"2026-03-15"}, "currency": {"AED"}}
	failed := b.do(http.MethodPost, "/journal/new", withLines(v, typedLine{account: "700000", amount: "1.00", cost: "CC1"},
		typedLine{account: "700000", amount: "-1.00"}))
	status(t, "a lookup that fails", failed, http.StatusUnprocessableEntity)
	holds(t, "a lookup that fails", failed.page(t).mainText(), "This could not be looked up just now.")
	if parked != "" {
		t.Error("a lookup that fails: the entry was sent")
	}
	ok := b.do(http.MethodPost, "/journal/new", withLines(v, typedLine{account: "700000", amount: "1.00", partner: partner.String()},
		typedLine{account: "700000", amount: "-1.00"}))
	if ok.status != http.StatusSeeOther || parked == "" {
		t.Errorf("an account by its number, a partner by its id: got %d %s", ok.status, ok.body)
	}
	if got := len(st.queriesWith("number", "700000")); got != 2 {
		t.Errorf("the account looked up by its number: %d calls, want one a send", got)
	}
	none := &stubbed{routes: map[string]http.HandlerFunc{"GET /v1/me": answer(200, meJSON(permParkEntries))}}
	_, nb, _ := stubStack(t, none)
	holds(t, "no companies", nb.get("/journal/new").page(t).mainText(), "There is no company you may enter an entry in.")
}

// queriesWith are the recorded queries whose key is value.
func (st *stubbed) queriesWith(key, value string) []url.Values {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []url.Values
	for _, q := range st.queries {
		if q.Get(key) == value {
			out = append(out, q)
		}
	}
	return out
}

// TestJournalUnavailable: an API that cannot answer, limits the rate or
// fails makes a journal page unavailable, never a half page; its 401
// signs the person in again, back to the page a form was sent from.
func TestJournalUnavailable(t *testing.T) {
	id, company := uuid.New(), uuid.New()
	me := answer(200, meJSON(permReadEntries, permParkEntries, permPostEntries, permReadCompanies, permReadAccounts, permCancelRequests))
	challenge := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_user_authentication", acr_values="urn:erp:mfa"`)
		answer(401, problemJSON(401, "identity.mfa_required"))(w, r)
	}
	unavailable := answer(503, problemJSON(503, "platform.unavailable"))
	empty := answer(200, `{"items":[]}`)
	parkedOK := answer(200, parkedJSON(id, company, "parked", ""))
	pending := answer(200, parkedJSON(id, company, "pending_approval", `,"approval_request_id":"`+id.String()+`"`))
	who := map[string]http.HandlerFunc{"GET /v1/me": challenge}
	for name, tc := range map[string]struct {
		routes   map[string]http.HandlerFunc
		method   string
		path     string
		status   int
		location string
	}{
		"list: who am I, 401":     {who, http.MethodGet, "/journal", http.StatusSeeOther, "return=%2Fjournal"},
		"list: unavailable":       {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/parked-journal-entries": unavailable}, http.MethodGet, "/journal", http.StatusServiceUnavailable, ""},
		"journal: who am I, 401":  {who, http.MethodGet, "/journal/entries", http.StatusSeeOther, "return=%2Fjournal%2Fentries"},
		"journal: unavailable":    {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/journal-entries": unavailable}, http.MethodGet, "/journal/entries", http.StatusServiceUnavailable, ""},
		"entry: who am I, 401":    {who, http.MethodGet, entryPath(id), http.StatusSeeOther, "return=%2Fjournal%2Fentries%2F" + id.String()},
		"entry: unavailable":      {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/journal-entries/": unavailable}, http.MethodGet, entryPath(id), http.StatusServiceUnavailable, ""},
		"reverse: who am I, 401":  {who, http.MethodPost, entryPath(id) + "/reverse", http.StatusSeeOther, "return=%2Fjournal%2Fentries%2F" + id.String()},
		"reverse: unavailable":    {map[string]http.HandlerFunc{"GET /v1/me": me, "POST /v1/journal-entries/": unavailable}, http.MethodPost, entryPath(id) + "/reverse", http.StatusServiceUnavailable, ""},
		"new: who am I, 401":      {who, http.MethodGet, "/journal/new", http.StatusSeeOther, "return=%2Fjournal%2Fnew"},
		"park: who am I, 401":     {who, http.MethodPost, "/journal/new", http.StatusSeeOther, "return=%2Fjournal"},
		"park: unavailable":       {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/accounts": empty, "POST /v1/parked-journal-entries": unavailable}, http.MethodPost, "/journal/new", http.StatusServiceUnavailable, ""},
		"parked: who am I, 401":   {who, http.MethodGet, parkedPath(id), http.StatusSeeOther, "return=%2Fjournal%2Fparked%2F" + id.String()},
		"parked: unavailable":     {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/parked-journal-entries/": unavailable}, http.MethodGet, parkedPath(id), http.StatusServiceUnavailable, ""},
		"decision: who am I, 401": {who, http.MethodPost, parkedPath(id) + "/post", http.StatusSeeOther, "return=%2Fjournal%2Fparked%2F" + id.String()},
		"decision: unavailable":   {map[string]http.HandlerFunc{"GET /v1/me": me, "POST /v1/parked-journal-entries/": unavailable}, http.MethodPost, parkedPath(id) + "/cancel", http.StatusServiceUnavailable, ""},
		"withdraw: unavailable":   {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/approval-requests/": unavailable}, http.MethodPost, parkedPath(id) + "/withdraw", http.StatusServiceUnavailable, ""},
		"withdraw: refused":       {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/approval-requests/": answer(403, problemJSON(403, "identity.permission_missing")), "GET /v1/parked-journal-entries/": pending}, http.MethodPost, parkedPath(id) + "/withdraw", http.StatusForbidden, ""},
		"change: who am I, 401":   {who, http.MethodGet, parkedPath(id) + "/edit", http.StatusSeeOther, "return=%2Fjournal%2Fparked%2F" + id.String()},
		"change: unavailable":     {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/parked-journal-entries/": unavailable}, http.MethodGet, parkedPath(id) + "/edit", http.StatusServiceUnavailable, ""},
		"save: who am I, 401":     {who, http.MethodPost, parkedPath(id) + "/edit", http.StatusSeeOther, "return=%2Fjournal%2Fparked%2F" + id.String()},
		"save: entry 503":         {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/parked-journal-entries/": unavailable}, http.MethodPost, parkedPath(id) + "/edit", http.StatusServiceUnavailable, ""},
		"lines: who am I, 401":    {who, http.MethodPost, parkedPath(id) + "/lines", http.StatusSeeOther, "return=%2Fjournal%2Fparked%2F" + id.String()},
		"lines: entry 503":        {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/parked-journal-entries/": unavailable}, http.MethodPost, parkedPath(id) + "/lines", http.StatusServiceUnavailable, ""},
		"save: unavailable":       {map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/parked-journal-entries/": parkedOK, "GET /v1/accounts": empty, "PATCH /v1/parked-journal-entries/": unavailable}, http.MethodPost, parkedPath(id) + "/edit", http.StatusServiceUnavailable, ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, b, token := stubStack(t, &stubbed{routes: tc.routes})
			var r response
			if tc.method == http.MethodGet {
				r = b.get(tc.path)
			} else {
				v := withLines(url.Values{"csrf_token": {token}, "if_match": {`"1"`}, "idempotency_key": {uuid.NewString()}, "comment": {"r"},
					"legal_entity_id": {company.String()}, "document_type": {"SA"}, "document_date": {"2026-03-15"}, "posting_date": {"2026-03-15"},
					"currency": {"EUR"}, "approval_request_id": {id.String()}}, typedLine{account: id.String(), amount: "1.00"},
					typedLine{account: id.String(), amount: "-1.00"})
				r = b.do(tc.method, tc.path, v)
			}
			if r.status != tc.status || !strings.HasSuffix(r.location(), tc.location) && !strings.Contains(r.location(), tc.location) {
				t.Errorf("got %d to %q, want %d to %q", r.status, r.location(), tc.status, tc.location)
			}
		})
	}
}
