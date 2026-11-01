package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// TestCurrencyFollowsTheCompany (review:batch32): a new entry's currency
// and dates are its company's, from the stub at 22:00 on 31 December in
// Toronto (CAD) and 07:00 on 1 January in Dubai (AED). Toronto's form
// switched to Dubai with htmx is rendered for Dubai at once; switched
// without htmx and sent, it is not sent in Toronto's currency and day but
// rendered for Dubai and said so. What the person typed is kept where
// nothing was filled in, and a company the person may not see fills
// nothing in. A person who may not post is not offered it.
func TestCurrencyFollowsTheCompany(t *testing.T) {
	toronto, dubai := uuid.New(), uuid.New()
	_, b, _ := stubStack(t, todayStub(toronto, dubai, nil))
	p := b.get(ui.NewEntryPath + "?" + ui.FieldCompany + "=" + toronto.String()).page(t)
	v := formValues(p.form(ui.NewEntryPath))
	for name, want := range map[string]string{"currency": "CAD", "posting_date": "2025-12-31", "filled_for": toronto.String()} {
		if got := v.Get(name); got != want {
			t.Errorf("Toronto's form, %s: got %q, want %q", name, got, want)
		}
	}
	holds(t, "Toronto's form", p.mainText(), "The company's currency is CAD.")
	if byID(p, "entry-post-button") != nil {
		t.Error("a person who may not post: offered the post")
	}
	v = withLines(v, pair("1.00")...)
	v.Set(ui.FieldCompany, dubai.String())
	filled := func(what string, r response, want map[string]string) {
		t.Helper()
		doc := r.page(t).doc
		for name, w := range want {
			if got := field(doc, name); got != w {
				t.Errorf("%s, %s: got %q, want %q", what, name, got, w)
			}
		}
	}
	hx, headers := asHTMX(v, ui.OpCompany)
	r := b.do(http.MethodPost, ui.NewEntryPath, hx, headers...)
	status(t, "Dubai chosen with htmx", r, http.StatusOK)
	filled("Dubai chosen with htmx", r, map[string]string{"currency": "AED", "posting_date": "2026-01-01", "document_date": "2026-01-01",
		"filled_for": dubai.String()})
	r = b.do(http.MethodPost, ui.NewEntryPath, v)
	status(t, "Dubai chosen without htmx and sent", r, http.StatusUnprocessableEntity)
	holds(t, "Dubai chosen without htmx and sent", r.page(t).mainText(), "The company was changed", "The company's currency is AED.")
	filled("Dubai chosen without htmx and sent", r, map[string]string{"currency": "AED", "posting_date": "2026-01-01", "filled_for": dubai.String()})
	typed := withLines(formValues(b.get(ui.NewEntryPath).page(t).form(ui.NewEntryPath)), pair("1.00")...)
	typed.Set("currency", "USD")
	typed.Set(ui.FieldCompany, dubai.String())
	hx, headers = asHTMX(typed, ui.OpCompany)
	filled("Dubai chosen, the currency typed", b.do(http.MethodPost, ui.NewEntryPath, hx, headers...), map[string]string{"currency": "USD",
		"posting_date": "2026-01-01", "filled_for": dubai.String()})
	// Toronto's form whose currency and posting date the person typed:
	// they are kept, and only the document date Toronto's day filled in
	// becomes Dubai's, which the form says.
	mine := formValues(p.form(ui.NewEntryPath))
	mine.Set("currency", "USD")
	mine.Set("posting_date", "2025-09-30")
	mine.Set(ui.FieldCompany, dubai.String())
	hx, headers = asHTMX(mine, ui.OpCompany)
	r = b.do(http.MethodPost, ui.NewEntryPath, hx, headers...)
	filled("Dubai chosen, Toronto's form typed on", r, map[string]string{"currency": "USD", "posting_date": "2025-09-30",
		"document_date": "2026-01-01", "filled_on": "2026-01-01"})
	holds(t, "Dubai chosen, Toronto's form typed on", text(r.page(t).doc), "The company was changed")
	// The same, sent without htmx: nothing is sent, and the notice says
	// what was typed is kept.
	r = b.do(http.MethodPost, ui.NewEntryPath, withLines(mine, pair("1.00")...))
	status(t, "Dubai chosen without htmx, typed on", r, http.StatusUnprocessableEntity)
	holds(t, "Dubai chosen without htmx, typed on", r.page(t).mainText(), "Nothing was sent.", "those you typed are kept")
	filled("Dubai chosen without htmx, typed on", r, map[string]string{"currency": "USD", "posting_date": "2025-09-30", "document_date": "2026-01-01"})
	v.Set(ui.FieldCompany, uuid.NewString())
	hx, headers = asHTMX(v, ui.OpCompany)
	filled("a company the person may not see", b.do(http.MethodPost, ui.NewEntryPath, hx, headers...), map[string]string{"currency": "CAD",
		"filled_for": ""})
}

// groupJSON is a ledger group of the ledgers, as the API lists it.
func groupJSON(code string, ledgers ...uuid.UUID) string {
	return fmt.Sprintf(`{"id":%q,"code":%q,"name":"Group %s","representative_ledger_id":%q,"ledger_ids":[%s],"version":1,
		"created_at":"2026-10-22T10:00:00Z","created_by":%q,"updated_at":"2026-10-22T10:00:00Z","updated_by":%q}`,
		uuid.New(), code, code, ledgers[0], quoted(uuidStrings(ledgers)), uuid.New(), uuid.New())
}

func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

// TestPostToStubbed: the ledgers a new entry may post to are those its
// company keeps and the groups with one of them; a company of one ledger
// is offered no choice; ledgers that cannot be read offer none and groups
// that cannot be read none of theirs, logged.
func TestPostToStubbed(t *testing.T) {
	toronto, dubai := uuid.New(), uuid.New()
	leading, parallel, other := uuid.New(), uuid.New(), uuid.New()
	st := todayStub(toronto, dubai, nil)
	st.routes["GET /v1/ledgers"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("legal_entity_id") == dubai.String() {
			answer(200, `{"items":[`+ledgerJSON(leading, "0L", true, dubai)+`]}`)(w, r)
			return
		}
		answer(200, `{"items":[`+ledgerJSON(leading, "0L", true, toronto)+`,`+ledgerJSON(parallel, "2L", false, toronto)+`]}`)(w, r)
	}
	st.routes["GET /v1/ledger-groups"] = answer(200, `{"items":[`+groupJSON("ALL", leading, parallel)+`,`+groupJSON("XX", other)+`]}`)
	s, b, _ := stubStack(t, st)
	in := func(company uuid.UUID) string {
		return ui.NewEntryPath + "?" + ui.FieldCompany + "=" + company.String()
	}
	p := b.get(in(toronto)).page(t)
	choice := byID(p, "entry-post-to")
	if choice == nil {
		t.Fatal("Toronto's form: no ledgers offered")
	}
	holds(t, "Toronto's ledgers", text(choice), "Every ledger the company keeps", "Ledger 0L Ledger 0L", "Ledger 2L Ledger 2L", "Ledger group ALL Group ALL")
	if strings.Contains(text(choice), "XX") {
		t.Error("Toronto's ledgers: offered a group of none of its ledgers")
	}
	if byID(b.get(in(dubai)).page(t), "entry-post-to") != nil {
		t.Error("Dubai's form: offered a choice of its one ledger")
	}
	broken := answer(500, problemJSON(500, "platform.internal"))
	st.routes["GET /v1/ledger-groups"] = broken
	got := text(byID(b.get(in(toronto)).page(t), "entry-post-to"))
	if !strings.Contains(got, "Ledger 2L Ledger 2L") || strings.Contains(got, "group") {
		t.Errorf("groups that cannot be read: got %q, want the ledgers alone", got)
	}
	st.routes["GET /v1/ledgers"] = broken
	if byID(b.get(in(toronto)).page(t), "entry-post-to") != nil {
		t.Error("ledgers that cannot be read: offered a choice")
	}
	// A choice the form holds, rendered again while the ledgers cannot be
	// read, stays chosen: never every ledger in its place.
	v := withLines(formValues(b.get(in(toronto)).page(t).form(ui.NewEntryPath)), pair("1.00")...)
	v.Set(ui.FieldPostTo, ui.PostToLedger+parallel.String())
	v.Set("op", "add")
	if got := chosen(b.do(http.MethodPost, ui.NewEntryPath, v).page(t).doc, ui.FieldPostTo); got != ui.PostToLedger+parallel.String() {
		t.Errorf("a chosen ledger rendered again unread: got %q, want it kept", got)
	}
	for _, logged := range []string{"the ledger groups could not be read", "the company's ledgers could not be read"} {
		if !strings.Contains(s.logs.String(), logged) {
			t.Errorf("not logged: %s", logged)
		}
	}
}

// TestChangeFormPartners: a parked line's partner is shown by number to be
// changed (listBusinessPartners by ids); partners that cannot be read
// leave it by id, logged, and a person who may not read partners reads
// none.
func TestChangeFormPartners(t *testing.T) {
	me, company, parked, partner := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	line := fmt.Sprintf(`{"line_no":3,"account_id":%q,"amount":%s,"partner_id":%q,"tax_jurisdiction":"CA-ON","tax_base":%s,
		"quantity":{"value":"1.5","unit":"KG"}}`, uuid.New(), moneyJSON("0.00"), partner, moneyJSON("100.00"))
	entry := strings.Replace(parkedJSON(parked, company, "parked", `,"owner_id":"`+me.String()+`"`), "}}]", "}},"+line+"]", 1)
	empty := answer(200, `{"items":[]}`)
	st := &stubbed{routes: map[string]http.HandlerFunc{
		"GET /v1/me":                      answer(200, meAs(me, permReadEntries, permParkEntries, permReadCompanies, permReadAccounts, permReadSuppliers)),
		"GET /v1/parked-journal-entries/": answer(200, entry),
		"GET /v1/legal-entities":          empty, "GET /v1/accounts": empty,
		"GET /v1/business-partners": answer(200, `{"items":[{"id":"`+partner.String()+`","version":1,"created_at":"2026-10-22T10:00:00Z",
			"created_by":"`+uuid.NewString()+`","updated_at":"2026-10-22T10:00:00Z","number":"BP42","kind":"organization","name":"Tyrell",
			"country_code":"CA","status":"active","roles":["supplier"],"supplier_companies":[],"addresses":[],"tax_identifiers":[],"extensions":{}}]}`),
	}}
	s, b, _ := stubStack(t, st)
	edit := func() string {
		v := formValues(b.get(parkedPath(parked)+"/edit", "Accept-Language", "fr-CA").page(t).form(parkedPath(parked) + "/edit"))
		if got := v["line_quantity"]; len(got) < 3 || got[2] != "1,5" || v["line_tax_base"][2] != "100,00" || v["line_tax_jurisdiction"][2] != "CA-ON" {
			t.Errorf("the third line: got %v %v %v, want 1,5, 100,00 and CA-ON", got, v["line_tax_base"], v["line_tax_jurisdiction"])
		}
		return v["line_partner"][2]
	}
	if got := edit(); got != "BP42" {
		t.Errorf("the line's partner: got %q, want its number BP42", got)
	}
	if q := st.queriesWith("ids", partner.String()); len(q) != 1 {
		t.Errorf("the partners read by ids: got %v, want once", q)
	}
	st.routes["GET /v1/business-partners"] = answer(500, problemJSON(500, "platform.internal"))
	if got := edit(); got != partner.String() {
		t.Errorf("partners that cannot be read: got %q, want the partner's id", got)
	}
	if !strings.Contains(s.logs.String(), "the lines' partners could not be read") {
		t.Error("partners that cannot be read: not logged")
	}
	st.routes["GET /v1/me"] = answer(200, meAs(me, permReadEntries, permParkEntries, permReadCompanies, permReadAccounts))
	before := len(st.queriesWith("ids", partner.String()))
	if got := edit(); got != partner.String() || len(st.queriesWith("ids", partner.String())) != before {
		t.Errorf("a person who may not read partners: got %q and %d reads, want the id and none", got, len(st.queriesWith("ids", partner.String()))-before)
	}
}

// TestParkFileUnavailable: a file sent while the API cannot say who the
// person is parks nothing, and the page says the workspace is
// unavailable.
func TestParkFileUnavailable(t *testing.T) {
	st := todayStub(uuid.New(), uuid.New(), nil)
	_, b, _ := stubStack(t, st)
	token := field(b.get(ui.NewEntryPath).page(t).form(ui.NewEntryPath), "csrf_token")
	st.routes["GET /v1/me"] = answer(500, problemJSON(500, "platform.internal"))
	r := parkFileAs(b, url.Values{"csrf_token": {token}, "idempotency_key": {uuid.NewString()}}, []byte("account,amount\n"))
	if r.status < 500 {
		t.Errorf("got %d, want the workspace unavailable", r.status)
	}
}
