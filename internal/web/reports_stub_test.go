package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
)

// balanceJSON is an account's EUR totals of March 2026.
func balanceJSON(company uuid.UUID) string {
	m := moneyJSON("1.00")
	return fmt.Sprintf(`{"ledger_id":%q,"legal_entity_id":%q,"account_id":%q,"fiscal_year":2026,"fiscal_period":3,"currency":"EUR","debit":%s,
		"credit":%s,"debit_lc":%s,"credit_lc":%s,"debit_gc":%s,"credit_gc":%s,"updated_at":"2026-10-22T10:00:00Z"}`,
		uuid.New(), company, uuid.New(), m, m, m, m, m, m)
}

// openItemJSON is a down payment's open item with a discount still open
// on 15 April and a reference that begins as a spreadsheet's formula.
func openItemJSON(company uuid.UUID) string {
	m := moneyJSON("-5.00")
	return fmt.Sprintf(`{"fiscal_year":2026,"journal_entry_id":%q,"line_no":1,"legal_entity_id":%q,"account_id":%q,"posting_date":"2026-04-01",
		"amount":%s,"amount_lc":%s,"document_number":"5100000123","reference":"=HYPERLINK(1)","special_gl":"A",
		"payment_terms":{"code":"ZB01","baseline_date":"2026-04-01","net_due_date":"2026-05-01","discounts":[{"until":"2026-04-10","percent":"3"},
		{"until":"2026-04-20","percent":"2"}]}}`, uuid.New(), company, uuid.New(), m, m)
}

// accountEntryJSON is a posted entry of company in March with its line 2
// on the account (extra: the line's further fields, each followed by a
// comma) and line 1 on another, headed "Freight in".
func accountEntryJSON(company, account uuid.UUID, extra string) string {
	u := uuid.NewString
	return fmt.Sprintf(`{"id":%q,"legal_entity_id":%q,"ledger_id":%q,"leading_ledger":true,"fiscal_year":2026,"fiscal_period":3,
		"document_type":"SA","document_number":"100009","document_date":"2026-03-15","posting_date":"2026-03-15","currency":"EUR",
		"local_currency":"EUR","group_currency":"EUR","rate_tc_lc":"1","rate_lc_gc":"1","source_context":"ledger","header_text":"Freight in","lines":[
		{"line_no":1,"account_id":%q,"amount":%s,"amount_lc":%s,"amount_gc":%s},
		{"line_no":2,"account_id":%q,%s"amount":%s,"amount_lc":%s,"amount_gc":%s}],
		"posted_at":"2026-03-15T10:00:00Z","posted_by":%q}`, u(), company, u(), u(), moneyJSON("-10.00"), moneyJSON("-10.00"), moneyJSON("-10.00"),
		account, extra, moneyJSON("10.00"), moneyJSON("10.00"), moneyJSON("10.00"), u())
}

// partnerJSON is a supplier with its number and name.
func partnerJSON(id uuid.UUID, number, name string) string {
	return fmt.Sprintf(`{"id":%q,"version":1,"created_at":"2026-10-22T10:00:00Z","created_by":%q,"updated_at":"2026-10-22T10:00:00Z","number":%q,
		"kind":"organization","name":%q,"country_code":"CA","status":"active","roles":["supplier"],"supplier_companies":[],"customer_companies":[],"addresses":[],
		"tax_identifiers":[],"extensions":{}}`, id, uuid.New(), number, name)
}

// TestOpenItemsChosen: the open items' suppliers are named by their ids
// (one read of a page's suppliers, none when the filter names the
// supplier, by id when the person may not read partners or the read
// fails, logged); the aging's buckets chosen are the API's aging_days,
// and buckets that are not what it takes are said on their field before
// it is called.
func TestOpenItemsChosen(t *testing.T) {
	company, supplier := uuid.New(), uuid.New()
	item := strings.Replace(openItemJSON(company), `"line_no":1,`, `"line_no":1,"partner_id":"`+supplier.String()+`",`, 1)
	var asked url.Values
	partners := 0
	named := func(w http.ResponseWriter, r *http.Request) {
		partners++
		asked = r.URL.Query()
		answer(200, `{"items":[`+partnerJSON(supplier, "BP42", "Tyrell")+`]}`)(w, r)
	}
	var aging string
	routes := func(me string, partnersRoute http.HandlerFunc) *stubbed {
		return &stubbed{routes: map[string]http.HandlerFunc{
			"GET /v1/me": answer(200, me), "GET /v1/accounts": answer(200, `{"items":[]}`), "GET /v1/business-partners": partnersRoute,
			"GET /v1/open-items": func(w http.ResponseWriter, r *http.Request) {
				aging = r.URL.Query().Get("aging_days")
				answer(200, `{"items":[`+item+`]}`)(w, r)
			},
		}}
	}
	path := func(kv ...string) string {
		q := url.Values{"legal_entity_id": {company.String()}, "key_date": {"2026-04-15"}}
		for i := 0; i+1 < len(kv); i += 2 {
			q.Set(kv[i], kv[i+1])
		}
		return "/reports/open-items?" + q.Encode()
	}
	_, b, _ := stubStack(t, routes(meJSON(permReadEntries, permReadSuppliers), named))
	holds(t, "the suppliers named", b.get(path()).page(t).mainText(), "5100000123 Down payment BP42 Tyrell")
	if got := asked.Get("ids"); got != supplier.String() {
		t.Errorf("the suppliers read: got ids %q, want %s", got, supplier)
	}
	partners = 0
	holds(t, "the supplier the filter names", b.get(path("supplier", "BP42")).page(t).mainText(), "Supplier: BP42 Tyrell", "Down payment BP42 Tyrell")
	if partners != 1 {
		t.Errorf("the supplier the filter names: got %d reads of partners, want 1 (the number's)", partners)
	}
	for name, tc := range map[string]struct {
		typed, sent string
		status      int
	}{
		"the API's":            {"", "", http.StatusOK},
		"chosen":               {" 7, 14,21 ", "7,14,21", http.StatusOK},
		"six":                  {"1,2,3,4,5,6", "1,2,3,4,5,6", http.StatusOK},
		"seven":                {"1,2,3,4,5,6,7", "", http.StatusUnprocessableEntity},
		"not ascending":        {"30,30", "", http.StatusUnprocessableEntity},
		"no number":            {"30,x", "", http.StatusUnprocessableEntity},
		"a day past the limit": {"3651", "", http.StatusUnprocessableEntity},
		"no day":               {"0", "", http.StatusUnprocessableEntity},
	} {
		t.Run(name, func(t *testing.T) {
			aging = "unasked"
			r := b.get(path("aging_days", tc.typed))
			status(t, name, r, tc.status)
			want := tc.sent
			if tc.status != http.StatusOK {
				want = "unasked"
				holds(t, name, r.page(t).mainText(), "Enter up to six numbers of days")
			}
			if aging != want {
				t.Errorf("aging_days sent: got %q, want %q", aging, want)
			}
		})
	}
	s, unread, _ := stubStack(t, routes(meJSON(permReadEntries, permReadSuppliers), answer(500, problemJSON(500, "platform.internal"))))
	holds(t, "the suppliers unread", unread.get(path()).page(t).mainText(), "Down payment "+supplier.String())
	holds(t, "the suppliers unread", s.logs.String(), "the open items' suppliers could not be read")
	partners = 0
	_, unnamed, _ := stubStack(t, routes(meJSON(permReadEntries), named))
	holds(t, "a person who may not read partners", unnamed.get(path()).page(t).mainText(), "Down payment "+supplier.String())
	if partners != 0 {
		t.Errorf("a person who may not read partners: got %d reads of partners, want none", partners)
	}
}

// TestLedgerComparedWithItself: an address naming one ledger as both
// ledgers of the reconciliation is said on the compared ledger's field,
// or in the report's place when the ledgers are unread and no field
// offers them, and the API is not asked.
func TestLedgerComparedWithItself(t *testing.T) {
	company, ledger, other := uuid.New(), uuid.New(), uuid.New()
	for name, ledgers := range map[string]http.HandlerFunc{
		"the ledgers read":   answer(200, `{"items":[`+ledgerJSON(ledger, "0L", true, company)+","+ledgerJSON(other, "2L", false, company)+`]}`),
		"the ledgers unread": answer(500, problemJSON(500, "platform.internal")),
	} {
		t.Run(name, func(t *testing.T) {
			_, b, _ := stubStack(t, &stubbed{routes: map[string]http.HandlerFunc{"GET /v1/me": answer(200, meJSON(permReadEntries, permReadAccounts)), "GET /v1/ledgers": ledgers,
				"GET /v1/ledger-reconciliation": func(w http.ResponseWriter, r *http.Request) {
					t.Error("the API was asked to compare a ledger with itself")
					answer(200, "{}")(w, r)
				}}})
			r := b.get("/reports/ledger-reconciliation?" + url.Values{"legal_entity_id": {company.String()}, "fiscal_year": {"2026"},
				"ledger_id": {other.String()}, "compare_ledger_id": {other.String()}}.Encode())
			status(t, name, r, http.StatusUnprocessableEntity)
			holds(t, name, r.page(t).mainText(), "Choose another ledger to compare with")
		})
	}
}

// auditJSON is an agent's event with a detail that is no string and a
// client address, of a kind the workspace does not know.
func auditJSON() string {
	return fmt.Sprintf(`{"seq":7,"occurred_at":"2026-10-22T10:00:00Z","actor_id":%q,"actor_kind":"robot","action":"ledger.journal_entry.post",
		"object_type":"ledger.journal_entry","object_id":%q,"outcome":"succeeded","detail":{"lines":2,"text":"<script>x</script>"},
		"client_address":"203.0.113.24"}`, uuid.New(), uuid.New())
}

// TestReportsStubbed: the API's rarer answers, from a stub behind the real
// sign-in: reports with a next page, whose companies, ledgers, accounts
// and names the workspace cannot read; aging buckets it does not know; a
// down payment with a discount still open; the reconciliation and its
// entries with the ledgers unread, by the address's ids; an audit event
// of an actor kind it does not know, its details as data; the house banks
// unread.
func TestReportsStubbed(t *testing.T) {
	company, ledger, compare, account := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	broken := answer(500, problemJSON(500, "platform.internal"))
	m := moneyJSON("5.00")
	st := &stubbed{routes: map[string]http.HandlerFunc{
		"GET /v1/me":              answer(200, meJSON(permReadEntries, permReadAccounts, permReadCompanies, permReadStatements, permReadAudit, permReadSuppliers)),
		"GET /v1/principal-names": broken,
		"GET /v1/ledgers":         broken,
		"GET /v1/accounts":        answer(200, `{"items":[]}`),
		"GET /v1/legal-entities":  broken,
		"GET /v1/house-banks":     broken,
		"GET /v1/balances":        answer(200, `{"items":[`+balanceJSON(company)+`],"next_page_token":"b2"}`),
		"GET /v1/open-items": answer(200, `{"items":[`+openItemJSON(company)+`],"next_page_token":"o2","aging":[
			{"bucket":"disputed","currency":"EUR","item_count":2,"amount_lc":`+m+`}]}`),
		"GET /v1/gr-ir-balances": answer(200, fmt.Sprintf(`{"items":[{"legal_entity_id":%q,"purchase_order_id":%q,"po_item_no":2,"account_id":%q,"account_number":"219000","account_name":"GR/IR clearing",
			"received_quantity":{"value":"3","unit":"KG"},"invoiced_quantity":{"value":"0","unit":"KG"},"cleared_quantity":{"value":"0","unit":"KG"},"migrated_quantity":{"value":"0","unit":"KG"},"balance":%s,"balance_lc":%s,"balance_gc":%s,
			"last_posting_date":"2026-03-05"}],"next_page_token":"g2"}`, company, uuid.New(), uuid.New(), m, m, m)),
		"GET /v1/ledger-reconciliation/entries": answer(200, fmt.Sprintf(`{"items":[{"ledger_id":%q,"journal_entry_id":%q,"fiscal_year":2026,"line_no":1,
			"document_number":"100042","posting_date":"2026-03-16","fiscal_period":3,"amount":%s,"amount_lc":%s,"amount_gc":%s}],"next_page_token":"e2"}`,
			ledger, uuid.New(), m, m, m)),
		"GET /v1/ledger-reconciliation": answer(200, fmt.Sprintf(`{"ledger_id":%q,"compare_ledger_id":%q,"trial_balances":[],
			"items":[{"account_id":%q,"account_number":"400000","account_name":"Sales Revenue","currency":"EUR","balance":%s,"compare_balance":%s,"difference":%s,"difference_lc":%s,"difference_gc":%s}],
			"next_page_token":"l2"}`, ledger, compare, uuid.New(), m, m, m, m, m)),
		"GET /v1/audit-events": answer(200, `{"items":[`+auditJSON()+`],"next_page_token":"a2"}`),
		"GET /v1/journal-entries": answer(200, `{"items":[`+accountEntryJSON(company, account, `"line_text":"<b>freight</b>",`)+","+
			accountEntryJSON(company, account, "")+`],"next_page_token":"j2"}`),
	}}
	_, b, _ := stubStack(t, st)
	co := url.Values{"legal_entity_id": {company.String()}, "fiscal_year": {"2026"}, "page": {"p2"}}
	ae := b.get("/reports/trial-balance/entries?" + url.Values{"legal_entity_id": {company.String()}, "ledger_id": {ledger.String()}, "fiscal_year": {"2026"},
		"period": {"3"}, "account_id": {account.String()}, "page": {"p2"}}.Encode()).page(t).mainText()
	holds(t, "the account's entries, its ledger and account unread", ae, account.String(), ledger.String(), "100009 2 2026-03-15 3 <b>freight</b> EUR 10.00",
		"100009 2 2026-03-15 3 Freight in EUR 10.00", "More rows")
	if strings.Contains(ae, "-10.00") {
		t.Errorf("the account's entries: got %q, want no line on another account", ae)
	}
	tb := b.get("/reports/trial-balance?" + co.Encode()).page(t)
	holds(t, "the trial balance", tb.mainText(), "EUR 1.00", "More rows")
	if strings.Contains(tb.mainText(), "Ledger 0L") {
		t.Error("the trial balance offers ledgers it could not read")
	}
	items := b.get("/reports/open-items?" + url.Values{"legal_entity_id": {company.String()}, "key_date": {"2026-04-15"}, "page": {"p2"}}.Encode())
	status(t, "the open items", items, http.StatusOK)
	holds(t, "the open items", items.page(t).mainText(), "disputed None EUR 2 EUR 5.00", "5100000123 Down payment", "2026-05-01 2026-04-20 None",
		"=HYPERLINK(1)", "More rows")
	if strings.Contains(items.page(t).mainText(), "Reconciliation account") {
		t.Error("the open items offer accounts the chart does not hold")
	}
	status(t, "an account, no id", b.get("/reports/open-items?"+url.Values{"legal_entity_id": {company.String()}, "account_id": {"211000"}}.Encode()),
		http.StatusBadRequest)
	holds(t, "the GR/IR positions", b.get("/reports/gr-ir?"+co.Encode()).page(t).mainText(), "3 KG 0 KG", "More rows")
	unknown := b.get("/reports/ledger-reconciliation?" + co.Encode()).page(t).mainText()
	holds(t, "the reconciliation, its ledgers unread", unknown, "The company's ledgers could not be read")
	co.Set("ledger_id", ledger.String())
	co.Set("compare_ledger_id", compare.String())
	recon := b.get("/reports/ledger-reconciliation?" + co.Encode()).page(t)
	holds(t, "the reconciliation by the address's ledgers", recon.mainText(), "Ledger "+ledger.String()+" compared with ledger "+compare.String(), "More rows")
	status(t, "a ledger, no id, the ledgers unread", b.get("/reports/ledger-reconciliation?"+url.Values{"legal_entity_id": {company.String()},
		"ledger_id": {"0L"}, "compare_ledger_id": {compare.String()}}.Encode()), http.StatusBadRequest)
	co.Set("account_id", uuid.NewString())
	co.Set("currency", "EUR")
	entries := b.get("/reports/ledger-reconciliation/entries?" + co.Encode()).page(t).mainText()
	holds(t, "the entries", entries, ledger.String()+" 100042 1 2026-03-16 3 EUR 5.00", "More rows")
	audit := b.get("/reports/audit?page=p2&object_id=" + uuid.NewString()).page(t).mainText()
	holds(t, "the history", audit, "Who Unknown", "Kind robot", "From the address 203.0.113.24", "lines 2", "text <script>x</script>", "More rows")
	holds(t, "the index, its banks unread", b.get("/reports").page(t).mainText(), "The house banks could not be listed.")
}

// TestReportRefusals: a report the API refuses says so in its place; a
// supplier whose lookup is refused or fails is said on its field.
func TestReportRefusals(t *testing.T) {
	refused := answer(403, problemJSON(403, "identity.permission_missing"))
	company := uuid.NewString()
	for name, tc := range map[string]struct {
		route, path string
	}{
		"the GR/IR positions":    {"GET /v1/gr-ir-balances", "/reports/gr-ir?legal_entity_id=" + company},
		"the reconciliation":     {"GET /v1/ledger-reconciliation", "/reports/ledger-reconciliation?" + url.Values{"legal_entity_id": {company}, "ledger_id": {company}, "compare_ledger_id": {uuid.NewString()}}.Encode()},
		"the entries":            {"GET /v1/ledger-reconciliation/entries", "/reports/ledger-reconciliation/entries?" + url.Values{"legal_entity_id": {company}, "compare_ledger_id": {company}, "account_id": {company}, "currency": {"EUR"}}.Encode()},
		"the open items":         {"GET /v1/open-items", "/reports/open-items?legal_entity_id=" + company},
		"an account's entries":   {"GET /v1/journal-entries", "/reports/trial-balance/entries?" + url.Values{"legal_entity_id": {company}, "account_id": {company}, "fiscal_year": {"2026"}}.Encode()},
		"a download, refused":    {"GET /v1/gr-ir-balances", "/reports/gr-ir.csv?legal_entity_id=" + company},
		"a download of balances": {"GET /v1/balances", "/reports/trial-balance.csv?legal_entity_id=" + company},
		"the reconciliation's download": {"GET /v1/ledger-reconciliation", "/reports/ledger-reconciliation.csv?" + url.Values{"legal_entity_id": {company},
			"ledger_id": {company}, "compare_ledger_id": {uuid.NewString()}, "fiscal_year": {"2026"}}.Encode()},
		"the history's download": {"GET /v1/audit-events", "/reports/audit.csv?object_id=" + company},
	} {
		t.Run(name, func(t *testing.T) {
			_, b, _ := stubStack(t, &stubbed{routes: map[string]http.HandlerFunc{"GET /v1/me": answer(200, meJSON(permReadEntries)), tc.route: refused}})
			r := b.get(tc.path)
			status(t, name, r, http.StatusForbidden)
			holds(t, name, r.page(t).mainText(), "The report could not be shown", "identity.permission_missing")
		})
	}
	for name, tc := range map[string]struct {
		answer http.HandlerFunc
		want   string
	}{
		"refused": {refused, "You may not look this up"},
		"failed":  {answer(500, problemJSON(500, "platform.internal")), "The supplier could not be looked up."},
	} {
		t.Run("a supplier's lookup "+name, func(t *testing.T) {
			_, b, _ := stubStack(t, &stubbed{routes: map[string]http.HandlerFunc{"GET /v1/me": answer(200, meJSON(permReadEntries, permReadSuppliers)),
				"GET /v1/business-partners": tc.answer}})
			r := b.get("/reports/open-items?" + url.Values{"legal_entity_id": {company}, "supplier": {"BP1"}}.Encode())
			status(t, name, r, http.StatusUnprocessableEntity)
			holds(t, name, r.page(t).mainText(), tc.want)
		})
	}
}

// TestDownloadLimits: a download reads its report's pages until the last;
// filters selecting more than 10,000 rows, or more pages than a download
// reads, are said on a page of their own, with the way back to the
// report.
func TestDownloadLimits(t *testing.T) {
	company := uuid.New()
	full := strings.Repeat(balanceJSON(company)+",", exportPageSize-1) + balanceJSON(company)
	short := balanceJSON(company)
	almost := strings.Repeat(balanceJSON(company)+",", exportPageSize-2) + balanceJSON(company)
	for name, tc := range map[string]struct {
		page   string
		pages  int // pages with a next one; 0 every page
		status int
		want   string
	}{
		"two pages":          {full, 1, http.StatusOK, ""},
		"over 10,000 rows":   {full, 0, http.StatusUnprocessableEntity, "The filters select more than 10,000 rows."},
		"too many pages":     {short, 0, http.StatusUnprocessableEntity, "The report is too long to download"},
		"the pages' end":     {short, 3, http.StatusOK, ""},
		"the last page over": {almost, 20, http.StatusUnprocessableEntity, "The filters select more than 10,000 rows."},
	} {
		t.Run(name, func(t *testing.T) {
			read := 0
			st := &stubbed{routes: map[string]http.HandlerFunc{
				"GET /v1/me": answer(200, meJSON(permReadEntries)),
				"GET /v1/balances": func(w http.ResponseWriter, r *http.Request) {
					read++
					next := `,"next_page_token":"n"`
					if tc.pages > 0 && read > tc.pages {
						next = ""
					}
					answer(200, `{"items":[`+tc.page+`]`+next+`}`)(w, r)
				},
			}}
			_, b, _ := stubStack(t, st)
			r := b.get("/reports/trial-balance.csv?" + url.Values{"legal_entity_id": {company.String()}, "fiscal_year": {"2026"}}.Encode())
			status(t, name, r, tc.status)
			if tc.want != "" {
				holds(t, name, r.page(t).mainText(), tc.want, "Back to the report")
			}
			if read > maxExportPages {
				t.Errorf("%s: %d pages read, want at most %d", name, read, maxExportPages)
			}
		})
	}
}

// TestReportsUnavailable: an API that cannot answer, limits the rate or
// fails makes a report unavailable, never a half page; its 401 signs the
// person in again, back to the report.
func TestReportsUnavailable(t *testing.T) {
	me := answer(200, meJSON(permReadEntries, permReadAudit, permReadStatements))
	challenge := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_user_authentication", acr_values="urn:erp:mfa"`)
		answer(401, problemJSON(401, "identity.mfa_required"))(w, r)
	}
	unavailable := answer(503, problemJSON(503, "platform.unavailable"))
	id := uuid.NewString()
	co := url.Values{"legal_entity_id": {id}, "ledger_id": {id}, "compare_ledger_id": {uuid.NewString()}, "account_id": {id}, "currency": {"EUR"}}.Encode()
	cases := map[string]struct {
		route, path string
		status      int
		location    string
	}{
		"index: who am I, 401":   {"GET /v1/me", "/reports", http.StatusSeeOther, "return=%2Freports"},
		"balances: 503":          {"GET /v1/balances", "/reports/trial-balance?" + co, http.StatusServiceUnavailable, ""},
		"balances: download 503": {"GET /v1/balances", "/reports/trial-balance.csv?" + co, http.StatusServiceUnavailable, ""},
		"items: 503":             {"GET /v1/open-items", "/reports/open-items?" + co, http.StatusServiceUnavailable, ""},
		"items: download 503":    {"GET /v1/open-items", "/reports/open-items.csv?" + co, http.StatusServiceUnavailable, ""},
		"GR/IR: 503":             {"GET /v1/gr-ir-balances", "/reports/gr-ir?" + co, http.StatusServiceUnavailable, ""},
		"GR/IR: download 503":    {"GET /v1/gr-ir-balances", "/reports/gr-ir.csv?" + co, http.StatusServiceUnavailable, ""},
		"reconciliation: 503":    {"GET /v1/ledger-reconciliation", "/reports/ledger-reconciliation?" + co, http.StatusServiceUnavailable, ""},
		"entries: 503":           {"GET /v1/ledger-reconciliation/entries", "/reports/ledger-reconciliation/entries?" + co, http.StatusServiceUnavailable, ""},
		"account entries: 503":   {"GET /v1/journal-entries", "/reports/trial-balance/entries?" + co, http.StatusServiceUnavailable, ""},
		"audit: 503":             {"GET /v1/audit-events", "/reports/audit?object_id=" + id, http.StatusServiceUnavailable, ""},
		"audit: download 503":    {"GET /v1/audit-events", "/reports/audit.csv?object_id=" + id, http.StatusServiceUnavailable, ""},
		"reconciliation: download 503": {"GET /v1/ledger-reconciliation", "/reports/ledger-reconciliation.csv?" + co + "&fiscal_year=2026",
			http.StatusServiceUnavailable, ""},
	}
	for _, path := range []string{"/reports/trial-balance", "/reports/trial-balance.csv", "/reports/open-items", "/reports/open-items.csv",
		"/reports/gr-ir", "/reports/gr-ir.csv", "/reports/ledger-reconciliation", "/reports/ledger-reconciliation/entries", "/reports/trial-balance/entries",
		"/reports/audit", "/reports/ledger-reconciliation.csv", "/reports/audit.csv"} {
		cases[path+": who am I, 401"] = struct {
			route, path string
			status      int
			location    string
		}{"GET /v1/me", path + "?" + co, http.StatusSeeOther, "return=" + url.QueryEscape(path)}
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			routes := map[string]http.HandlerFunc{"GET /v1/me": me, "GET /v1/accounts": answer(200, `{"items":[]}`), tc.route: unavailable}
			if tc.route == "GET /v1/me" {
				routes[tc.route] = challenge
			}
			_, b, _ := stubStack(t, &stubbed{routes: routes})
			r := b.get(tc.path)
			if r.status != tc.status || !strings.Contains(r.location(), tc.location) {
				t.Errorf("got %d to %q, want %d to %q", r.status, r.location(), tc.status, tc.location)
			}
		})
	}
}

// failingWriter is a response whose body cannot be written.
type failingWriter struct{ *httptest.ResponseRecorder }

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("the client went away") }

// TestDownloadCutShort: a download whose writes fail is logged.
func TestDownloadCutShort(t *testing.T) {
	s, logs := testServer(t)
	r := httptest.NewRequest(http.MethodGet, "/reports/gr-ir.csv", nil)
	s.writeCSV(failingWriter{httptest.NewRecorder()}, r.WithContext(i18n.With(r.Context(), s.Catalog.Default())),
		export{file: "gr-ir", title: i18n.GRIRReportTitle}, [][]string{{"a"}})
	holds(t, "the log", logs.String(), "a report's download was cut short", "the client went away")
}

// TestSafeCell: numbers and dates are as they are; every other text is
// quoted, its quotes doubled, so neither a comma nor a semicolon nor a
// line break starts a new cell; a text beginning with a character OWASP
// names (= + - @, tab, carriage return, line feed and their full-width
// forms) is kept as text by a leading apostrophe; OWASP's own example.
func TestSafeCell(t *testing.T) {
	for in, want := range map[string]string{"=SUM(A1)": `"'=SUM(A1)"`, "+1": `"'+1"`, "-x": `"'-x"`, "@x": `"'@x"`, "\tx": "\"'\tx\"",
		"\rx": "\"'\rx\"", "\nx": "\"'\nx\"", "\uff1dSUM(A1)": "\"'\uff1dSUM(A1)\"", "\uff0b1": "\"'\uff0b1\"", "\uff0dx": "\"'\uff0dx\"",
		"\uff20x": "\"'\uff20x\"", "-2-3": `"'-2-3"`, "-119.00": "-119.00", "42": "42", "2026-04-09": "2026-04-09", "INV-1": `"INV-1"`, "": "",
		"a,=1+2": `"a,=1+2"`, "a;=1+2": `"a;=1+2"`, "a\n=1+2": "\"a\n=1+2\"", `say "hi"`: `"say ""hi"""`,
		`=1+2";=1+2`: `"'=1+2"";=1+2"`, "\xff=": "\"\xff=\""} {
		t.Run(in, func(t *testing.T) {
			if got := safeCell(in); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

// TestDownloadsStubbed: the reconciliation's and the history's downloads
// from a stub: every page read until the last; the reconciled ledgers
// named from the API's answer, by id when they cannot be read; the history's actors named
// in one read, by id alone when the names cannot be read, a kind the
// catalog does not name as the API gives it, the details as one JSON
// object and its texts safe for a spreadsheet; the downloads refused,
// unavailable and over the limit as the others are.
func TestDownloadsStubbed(t *testing.T) {
	company, ledger, compare, object := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	m := moneyJSON("5.00")
	difference := fmt.Sprintf(`{"account_id":%q,"account_number":"400000","account_name":"=Sales","currency":"EUR","balance":%s,
		"compare_balance":%s,"difference":%s,"difference_lc":%s,"difference_gc":%s}`, uuid.New(), m, m, m, m, m)
	paged := func(item string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			next := `,"next_page_token":"n"`
			if r.URL.Query().Get("page_token") != "" {
				next = ""
			}
			if got := r.URL.Query().Get("page_size"); got != "500" {
				t.Errorf("%s: page_size %q, want 500", r.URL.Path, got)
			}
			answer(200, fmt.Sprintf(`{"ledger_id":%q,"compare_ledger_id":%q,"trial_balances":[],"items":[%s]%s}`, ledger, compare, item, next))(w, r)
		}
	}
	names := 0
	st := &stubbed{routes: map[string]http.HandlerFunc{
		"GET /v1/me":                    answer(200, meJSON(permReadEntries, permReadAudit)),
		"GET /v1/ledger-reconciliation": paged(difference),
		"GET /v1/audit-events":          paged(auditJSON()),
		"GET /v1/principal-names": func(w http.ResponseWriter, r *http.Request) {
			names++
			answer(500, problemJSON(500, "platform.internal"))(w, r)
		},
	}}
	_, b, _ := stubStack(t, st)
	recon := sheet(t, "the reconciliation", b.get("/reports/ledger-reconciliation.csv?"+url.Values{"legal_entity_id": {company.String()},
		"ledger_id": {ledger.String()}, "compare_ledger_id": {compare.String()}, "fiscal_year": {"2026"}, "period": {"3"}}.Encode()))
	if got := len(recon); got != 4 {
		t.Errorf("the reconciliation: got %d lines, want the filters, the columns and a row of each of two pages", got)
	}
	if got, want := strings.Join(recon[2], "|"), "400000 =Sales|EUR|5.00|5.00|5.00|5.00|EUR|5.00|EUR"; got != want {
		t.Errorf("the reconciliation's row: got %q, want %q", got, want)
	}
	// Without ledger_id the API reconciles its leading ledger, which the
	// first line names from the answer; the ledgers unread, by id.
	leading := sheet(t, "the reconciliation from the leading ledger", b.get("/reports/ledger-reconciliation.csv?"+url.Values{
		"legal_entity_id": {company.String()}, "compare_ledger_id": {compare.String()}, "fiscal_year": {"2026"}}.Encode()))
	holds(t, "the reconciliation's ledgers", strings.Join(leading[0], "|"), "|compare_ledger="+compare.String()+"|", "|ledger="+ledger.String()+"|")
	audit := b.get("/reports/audit.csv?object_id=" + object.String())
	history := sheet(t, "the history", audit)
	if got := len(history); got != 4 {
		t.Errorf("the history: got %d lines, want the filters, the columns and an event of each of two pages", got)
	}
	if got, want := strings.Join(history[2][4:], "|"), `robot|ledger.journal_entry.post|succeeded|203.0.113.24|{"lines":2,"text":"<script>x</script>"}`; got != want {
		t.Errorf("the history's event: got %q, want %q", got, want)
	}
	if history[2][2] != "" || history[2][3] == "" {
		t.Errorf("the history's actor, the names unread: got %q and %q, want no name and the id", history[2][2], history[2][3])
	}
	if names != 1 {
		t.Errorf("the names: read %d times, want once for the whole history", names)
	}
	holds(t, "the history's first line", strings.SplitN(audit.body, "\n", 2)[0], `"object_id=`+object.String()+`"`)
}

// TestFirstActors: a history's actors are named in one read, each once,
// the first hundred of them.
func TestFirstActors(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	if got := firstActors([]uuid.UUID{a, b, a}); len(got) != 2 || got[0] != a || got[1] != b {
		t.Errorf("two actors, one twice: got %v, want %v and %v", got, a, b)
	}
	many := make([]uuid.UUID, namesPerCall+5)
	for i := range many {
		many[i] = uuid.New()
	}
	if got := firstActors(many); len(got) != namesPerCall || got[namesPerCall-1] != many[namesPerCall-1] {
		t.Errorf("%d actors: got %d, want the first %d", len(many), len(got), namesPerCall)
	}
}
