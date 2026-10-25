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
	company, ledger, compare := uuid.New(), uuid.New(), uuid.New()
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
		"GET /v1/gr-ir-balances": answer(200, fmt.Sprintf(`{"items":[{"legal_entity_id":%q,"purchase_order_id":%q,"po_item_no":2,"account_id":%q,
			"received_quantity":{"value":"3","unit":"KG"},"invoiced_quantity":{"value":"0","unit":"KG"},"balance":%s,"balance_lc":%s,"balance_gc":%s,
			"last_posting_date":"2026-03-05"}],"next_page_token":"g2"}`, company, uuid.New(), uuid.New(), m, m, m)),
		"GET /v1/ledger-reconciliation/entries": answer(200, fmt.Sprintf(`{"items":[{"ledger_id":%q,"journal_entry_id":%q,"fiscal_year":2026,"line_no":1,
			"document_number":"100042","posting_date":"2026-03-16","fiscal_period":3,"amount":%s,"amount_lc":%s,"amount_gc":%s}],"next_page_token":"e2"}`,
			ledger, uuid.New(), m, m, m)),
		"GET /v1/ledger-reconciliation": answer(200, fmt.Sprintf(`{"ledger_id":%q,"compare_ledger_id":%q,"trial_balances":[],
			"items":[{"account_id":%q,"currency":"EUR","balance":%s,"compare_balance":%s,"difference":%s,"difference_lc":%s,"difference_gc":%s}],
			"next_page_token":"l2"}`, ledger, compare, uuid.New(), m, m, m, m, m)),
		"GET /v1/audit-events": answer(200, `{"items":[`+auditJSON()+`],"next_page_token":"a2"}`),
	}}
	_, b, _ := stubStack(t, st)
	co := url.Values{"legal_entity_id": {company.String()}, "fiscal_year": {"2026"}, "page": {"p2"}}
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
		"the reconciliation":     {"GET /v1/ledger-reconciliation", "/reports/ledger-reconciliation?" + url.Values{"legal_entity_id": {company}, "ledger_id": {company}, "compare_ledger_id": {company}}.Encode()},
		"the entries":            {"GET /v1/ledger-reconciliation/entries", "/reports/ledger-reconciliation/entries?" + url.Values{"legal_entity_id": {company}, "compare_ledger_id": {company}, "account_id": {company}, "currency": {"EUR"}}.Encode()},
		"the open items":         {"GET /v1/open-items", "/reports/open-items?legal_entity_id=" + company},
		"a download, refused":    {"GET /v1/gr-ir-balances", "/reports/gr-ir.csv?legal_entity_id=" + company},
		"a download of balances": {"GET /v1/balances", "/reports/trial-balance.csv?legal_entity_id=" + company},
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
	for name, tc := range map[string]struct {
		page   string
		pages  int // pages with a next one; 0 every page
		status int
		want   string
	}{
		"two pages":        {full, 1, http.StatusOK, ""},
		"over 10,000 rows": {full, 0, http.StatusUnprocessableEntity, "The filters select more than 10,000 rows."},
		"too many pages":   {short, 0, http.StatusUnprocessableEntity, "The report is too long to download"},
		"the pages' end":   {short, 3, http.StatusOK, ""},
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
	co := url.Values{"legal_entity_id": {id}, "ledger_id": {id}, "compare_ledger_id": {id}, "account_id": {id}, "currency": {"EUR"}}.Encode()
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
		"audit: 503":             {"GET /v1/audit-events", "/reports/audit?object_id=" + id, http.StatusServiceUnavailable, ""},
	}
	for _, path := range []string{"/reports/trial-balance", "/reports/trial-balance.csv", "/reports/open-items", "/reports/open-items.csv",
		"/reports/gr-ir", "/reports/gr-ir.csv", "/reports/ledger-reconciliation", "/reports/ledger-reconciliation/entries", "/reports/audit"} {
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

// TestSafeCell: a text that a spreadsheet would run as a formula is kept
// as text; numbers, dates and other texts are as they are.
func TestSafeCell(t *testing.T) {
	for in, want := range map[string]string{"=SUM(A1)": "'=SUM(A1)", "+1": "'+1", "-x": "'-x", "@x": "'@x", "\tx": "'\tx", "\rx": "'\rx",
		"-119.00": "-119.00", "2026-04-09": "2026-04-09", "INV-1": "INV-1", "": ""} {
		if got := safeCell(in); got != want {
			t.Errorf("safeCell(%q): got %q, want %q", in, got, want)
		}
	}
}
