package web

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/net/html"

	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// journalData is what journalStack's tenant holds.
type journalData struct {
	f          fixture.Env
	centers    fixture.JournalCenters
	accountant pgtest.Actor // parks, posts and reverses; holds every role, as the approvers do
	colleague  pgtest.Actor // another accountant
	viewer     pgtest.Actor // may read entries, accounts and companies alone
	outsider   pgtest.Actor // may read nothing
	proposal   uuid.UUID    // an agent's parked entry no person owns
	centered   uuid.UUID    // a posted entry of company 2000 with CC100 on its line
	payables   uuid.UUID    // a posted entry the payables context owns
}

// journalStack is the workspace in front of a configured tenant whose
// accountant enters manual entries in company 2000 (EUR, a manual entry
// limit of EUR 10,000.00) and whose approver, holding the controller's
// role, approves those above the limit: segment SEG1, profit center PC10
// and cost center CC100 of company 2000, an agent's proposal of EUR 10.00
// that no person owns, a posted entry on CC100 and one the payables
// context owns; a person may only read entries and one may read nothing.
func journalStack(t *testing.T) (*stack, *journalData) {
	t.Helper()
	d := &journalData{}
	s := newStack(t, stackOptions{permissions: fixture.JournalPermissions, seed: func(f fixture.Env) {
		d.f = f
		d.centers = f.Centers(t)
		f.Tenant.AllCompanies(t, f.Tenant.Agent)
		agent := f
		agent.Client = f.As(f.Tenant.Agent)
		d.proposal = agent.Park(t, "2026-03-15", "Accrual the agent proposes", [2]string{"520000", "10.00"}, [2]string{"100000", "-10.00"})
		posted, _ := f.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-10",
			"posting_date": "2026-03-10", "currency": "EUR", "header_text": "Office supplies", "lines": [
			{"account_id": %q, "amount": {"amount": "250.00", "currency": "EUR"}, "cost_center_id": %q},
			{"account_id": %q, "amount": {"amount": "-250.00", "currency": "EUR"}}]}`, f.DE, f.Account(t, "2000", "520000"), d.centers.Cost,
			f.Account(t, "2000", "100000")), "")
		d.centered = uuid.MustParse(posted["id"].(string))
		d.payables = f.PostedBy(t, "payables", "2026-03-11", 5000)
		d.accountant, d.colleague = f.Person(t, "accountant"), f.Person(t, "colleague")
		d.viewer = roleHolder(t, f, "viewer", "ledger.entry.read", "ledger.account.read", "tenancy.org.read")
		d.outsider = roleHolder(t, f, "outsider")
	}})
	return s, d
}

// formValues are a form's fields as a browser sends them: its inputs,
// checked boxes and chosen options, each name's values in their order.
func formValues(form *html.Node) url.Values {
	v := url.Values{}
	walk(form, func(n *html.Node) {
		name := attrOf(n, "name")
		switch {
		case name == "":
		case n.Data == "input" && attrOf(n, "type") == "checkbox":
			if hasAttr(n, "checked") {
				v.Add(name, attrOf(n, "value"))
			}
		case n.Data == "input" && attrOf(n, "type") != "file":
			v.Add(name, attrOf(n, "value"))
		case n.Data == "select":
			chosen := ""
			walk(n, func(o *html.Node) {
				if o.Data == "option" && (chosen == "" || hasAttr(o, "selected")) {
					chosen = attrOf(o, "value")
				}
			})
			v.Add(name, chosen)
		}
	})
	v.Del("op")
	return v
}

// entryLine is one line typed in the entry form, by its fields.
type typedLine struct{ account, amount, cost, profit, segment, partner, tax, text string }

// withLines are the form's fields with the lines given in place of its
// own.
func withLines(v url.Values, lines ...typedLine) url.Values {
	out := url.Values{}
	for k, vs := range v {
		if !strings.HasPrefix(k, "line_") {
			out[k] = vs
		}
	}
	for _, l := range lines {
		for name, value := range map[string]string{"line_account": l.account, "line_amount": l.amount, "line_cost_center": l.cost,
			"line_profit_center": l.profit, "line_segment": l.segment, "line_partner": l.partner, "line_tax_code": l.tax, "line_text": l.text} {
			out.Add(name, value)
		}
	}
	return out
}

// newEntryForm is the new entry's form of company 2000 as the page offers
// it, with the header's fields given besides.
func newEntryForm(b *browser, d *journalData, header url.Values) url.Values {
	b.s.t.Helper()
	v := formValues(b.get("/journal/new?legal_entity_id=" + d.f.DE.String()).page(b.s.t).form("/journal/new"))
	v.Set("posting_date", "2026-03-15")
	v.Set("document_date", "2026-03-15")
	for k, vs := range header {
		v[k] = vs
	}
	return v
}

// parkedIn is the parked entry a park's answer goes to.
func parkedIn(t *testing.T, r response) uuid.UUID {
	t.Helper()
	rest, ok := strings.CutPrefix(r.location(), "/journal/parked/")
	id, err := uuid.Parse(strings.TrimSuffix(rest, "?done=parked"))
	if r.status != http.StatusSeeOther || !ok || err != nil {
		t.Fatalf("park: got %d to %q: %s", r.status, r.location(), r.body)
	}
	return id
}

// parkedStatus is a parked entry's status and owner, from the database.
func parkedStatus(t *testing.T, id uuid.UUID) (string, string) {
	t.Helper()
	var st, owner string
	pgtest.Scan(t, `SELECT status, coalesce(owner_id::text, '') FROM ledger.parked_entry WHERE id = $1`, []any{id}, &st, &owner)
	return st, owner
}

// parkedForm is the verb's form of a parked entry's page, nil for none.
func parkedFormOf(p page, id uuid.UUID, verb string) *html.Node {
	for _, f := range p.all("form", "") {
		if attrOf(f, "action") == parkedPath(id)+"/"+verb {
			return f
		}
	}
	return nil
}

// decideParkedAs posts the verb's form as the parked entry's page offers
// it, with the fields given besides.
func decideParkedAs(b *browser, id uuid.UUID, verb string, fields url.Values) response {
	b.s.t.Helper()
	f := parkedFormOf(b.get(parkedPath(id)).page(b.s.t), id, verb)
	if f == nil {
		b.s.t.Fatalf("parked entry %s: no %s form", id, verb)
	}
	return postForm(b, f, parkedPath(id)+"/"+verb, fields)
}

// parkedDecided checks a decision went back to the entry's page with its
// notice.
func parkedDecided(t *testing.T, what string, r response, id uuid.UUID, notice string) {
	t.Helper()
	if want := parkedPath(id) + "?done=" + notice; r.status != http.StatusSeeOther || r.location() != want {
		t.Fatalf("%s: got %d to %q, want %s: %s", what, r.status, r.location(), want, r.body)
	}
}

// line is a balanced pair of lines: office supplies debited, the bank
// credited.
func pair(amount string) []typedLine {
	return []typedLine{{account: "520000", amount: amount}, {account: "100000", amount: "-" + amount}}
}

// TestParkedList: the parked entries by status and kind, the agent's
// proposal badged and owned by no one; a company that is no id refuses
// the filters, a page token that is none is the API's refusal.
func TestParkedList(t *testing.T) {
	s, d := journalStack(t)
	b := s.as(d.accountant)
	r := b.get("/journal")
	status(t, "the list", r, http.StatusOK)
	p := r.page(t)
	holds(t, "the list", p.mainText(), "Manual journal entries", "Enter a new entry", "Browse the journal of posted entries", "SA entry of 2026-03-15",
		"Status Parked", "Company Acme GmbH", "Header text Accrual the agent proposes", "Debits EUR 10.00", "Lines 2", "Agent",
		"Owner No one yet: an agent's proposal")
	current := ""
	for _, a := range p.all("a", "navigation__link") {
		if attrOf(a, "aria-current") == "page" {
			current = attrOf(a, "href")
		}
	}
	if current != "/journal" {
		t.Errorf("navigation: the current entry is %q, want /journal", current)
	}
	agents := b.get("/journal?created_by_kind=agent").page(t).mainText()
	holds(t, "the agents' proposals", agents, "Accrual the agent proposes")
	for name, tc := range map[string]struct {
		query  string
		status int
		want   string
	}{
		"people's alone":     {"created_by_kind=human", http.StatusOK, "No entry is in this list."},
		"posted":             {"status=posted", http.StatusOK, "No entry is in this list."},
		"a company, no id":   {"legal_entity_id=x", http.StatusBadRequest, "A filter in the address is not valid."},
		"a page token, none": {"page=nonsense", http.StatusBadRequest, "request.invalid_page_token"},
		"another company":    {"legal_entity_id=" + d.f.US.String(), http.StatusOK, "No entry is in this list."},
	} {
		t.Run(name, func(t *testing.T) {
			r := b.get("/journal?" + tc.query)
			status(t, name, r, tc.status)
			holds(t, name, r.page(t).mainText(), tc.want)
		})
	}
	holds(t, "French", b.get("/journal", "Accept-Language", "fr-CA").page(t).mainText(), "Écritures de journal manuelles", "Préenregistrée",
		"10,00 EUR")
}

// TestEntryForm: a new entry's form, its lines added and left out without
// htmx (the page again) and with it (the lines' section alone), what is
// wrong said on its field before the API is called, and the entry parked
// with its centers, partner and tax code, its amounts typed in French.
func TestEntryForm(t *testing.T) {
	s, d := journalStack(t)
	b := s.as(d.accountant)
	p := b.get("/journal/new").page(t)
	form := p.form("/journal/new")
	if got := field(form, "currency"); got != "USD" {
		t.Errorf("the first company's currency: got %q, want USD (Acme Corp US)", got)
	}
	if got := field(form, "posting_date"); got != s.clock.Now().Format("2006-01-02") {
		t.Errorf("the posting date: got %q, want today", got)
	}
	if got := len(formValues(form)["line_account"]); got != 2 {
		t.Errorf("a new entry's lines: got %d, want 2", got)
	}
	holds(t, "the accounts suggested", text(p.all("datalist", "")[0]), "100000 Bank - Operating", "520000 Office Supplies")
	v := newEntryForm(b, d, nil)
	if v.Get("currency") != "EUR" {
		t.Errorf("company 2000's currency: got %q, want EUR", v.Get("currency"))
	}
	v = withLines(v, typedLine{account: "520000", amount: "12.00", text: "Kept"}, typedLine{account: "100000"})
	v.Set("op", "add")
	added := b.do(http.MethodPost, "/journal/new", v)
	status(t, "a line added", added, http.StatusOK)
	ap := added.page(t)
	if got := formValues(ap.form("/journal/new"))["line_account"]; len(got) != 3 || got[0] != "520000" {
		t.Errorf("a line added: got the lines %v, want three, the typed ones kept", got)
	}
	focused := ""
	walk(ap.doc, func(n *html.Node) {
		if hasAttr(n, "autofocus") {
			focused = attrOf(n, "id")
		}
	})
	if focused != "line-3-line_account" {
		t.Errorf("a line added: focus on %q, want the new line's account", focused)
	}
	v.Del("op")
	v.Set("line_remove", "1")
	// As htmx sends it: the CSRF token in its header alone.
	token := v.Get("csrf_token")
	v.Del("csrf_token")
	frag := b.do(http.MethodPost, "/journal/new", v, "HX-Request", "true", "X-CSRF-Token", token)
	status(t, "a line left out with htmx", frag, http.StatusOK)
	if strings.Contains(frag.body, "<html") || !strings.Contains(frag.body, `id="entry-lines"`) {
		t.Errorf("a line left out with htmx: got %q, want the lines' section alone", frag.body)
	}
	if v := frag.header.Values("Vary"); !strings.Contains(strings.Join(v, " "), "HX-Request") {
		t.Errorf("the section's Vary: got %v, want HX-Request in it", v)
	}
	if strings.Contains(frag.body, "Kept") {
		t.Error("a line left out with htmx: line 1 is still there")
	}
	t.Run("what is wrong is said before the API is called", func(t *testing.T) {
		v := withLines(newEntryForm(b, d, url.Values{"document_type": {"S"}, "currency": {"euro"}, "reference": {strings.Repeat("r", 41)},
			"tax_date": {"15.03.2026"}, "document_date": {""}, "posting_date": {"x"}, "header_text": {strings.Repeat("h", 201)}}),
			typedLine{account: "999999", amount: "1.200,00", cost: "NOPE", profit: "nope", segment: "SEG9", partner: "BP0", tax: "VVVVV",
				text: strings.Repeat("t", 201)},
			typedLine{account: "", amount: "0"}, typedLine{account: "100000", amount: "-5", cost: "CODE TOO LONG"}, typedLine{account: "100000"})
		r := b.do(http.MethodPost, "/journal/new", v)
		status(t, "wrong", r, http.StatusUnprocessableEntity)
		holds(t, "wrong", r.page(t).mainText(), "Enter the document type as two letters or digits", "Enter the currency as three capital letters",
			"A reference has at most 40 characters.", "Enter a date as year-month-day", "Enter the amount as digits", "A tax code has 1 to 4",
			"A text has at most 200 characters.", "Enter the account's number.", "A line's amount is never zero.", "A code has at most 10",
			"Enter the amount.")
		r = b.do(http.MethodPost, "/journal/new", withLines(newEntryForm(b, d, nil),
			typedLine{account: "999999", amount: "1", cost: "NOPE", profit: "NOPE", segment: "SEG9", partner: "BP0"}, typedLine{account: "100000", amount: "-1"}))
		status(t, "unknown codes", r, http.StatusUnprocessableEntity)
		holds(t, "unknown codes", r.page(t).mainText(), "No account of the company's chart has this number.",
			"The company has no cost center with this code.", "The company has no profit center with this code.", "There is no segment with this code.",
			"There is no partner with this number.")
		few := b.do(http.MethodPost, "/journal/new", withLines(newEntryForm(b, d, nil), typedLine{account: "520000", amount: "1"}))
		holds(t, "one line", few.page(t).mainText(), "An entry has at least two lines.")
		forged := newEntryForm(b, d, url.Values{"legal_entity_id": {"x"}})
		holds(t, "a company that is no id", b.do(http.MethodPost, "/journal/new", withLines(forged, pair("1.00")...)).page(t).mainText(),
			"Choose the company.")
	})
	t.Run("the API's refusals, on their lines", func(t *testing.T) {
		r := b.do(http.MethodPost, "/journal/new", withLines(newEntryForm(b, d, nil), typedLine{account: "520000", amount: "10.00"},
			typedLine{account: "100000", amount: "-9.00"}))
		status(t, "unbalanced", r, http.StatusUnprocessableEntity)
		holds(t, "unbalanced", r.page(t).mainText(), "The entry was not saved", "The lines do not balance", "ledger.unbalanced")
		r = b.do(http.MethodPost, "/journal/new", withLines(newEntryForm(b, d, nil), typedLine{account: "520000", amount: "10.00"},
			typedLine{account: "100000", amount: "-10.00", cost: "CC100"}))
		status(t, "a cost center on the bank", r, http.StatusUnprocessableEntity)
		holds(t, "a cost center on the bank", r.page(t).mainText(), "ledger.cost_center_invalid", "Cost center: refused by the ERP (not_allowed).")
	})
	parked := b.do(http.MethodPost, "/journal/new", withLines(newEntryForm(b, d, url.Values{"reference": {"ACCR-3"}, "header_text": {"March accrual"}}),
		typedLine{account: "520000", amount: "1200,50", cost: "cc100", profit: "PC10", segment: "seg1", tax: "v0", text: "Paper"},
		typedLine{}, typedLine{account: "211000", amount: "-1200,50", partner: "BP90000001"}), "Accept-Language", "fr-CA")
	id := parkedIn(t, parked)
	pe := b.get(parkedPath(id) + "?done=parked").page(t).mainText()
	holds(t, "the entry parked", pe, "The entry is parked.", "Status Parked", "Reference ACCR-3", "Header text March accrual", "Debits EUR 1,200.50",
		"520000 Office Supplies EUR 1,200.50 CC100 PC10 SEG1 None V0 Paper", d.f.Supplier.String(), "Owner accountant")
	status(t, "a form that is not the page's", b.do(http.MethodPost, "/journal/new", url.Values{"csrf_token": {b.csrf()}}), http.StatusBadRequest)
}

// parkAs parks, as the browser's person, a new entry of company 2000 on
// 15 March 2026 with the lines given, and returns it.
func parkAs(b *browser, d *journalData, lines ...typedLine) uuid.UUID {
	b.s.t.Helper()
	return parkedIn(b.s.t, b.do(http.MethodPost, "/journal/new", withLines(newEntryForm(b, d, nil), lines...)))
}

// TestPostAndApprove is the accountant's and the controller's job: an
// entry within the company's limit is posted at once and links its
// journal entry; one above it waits for another person's approval, which
// the accountant may not give (workflow.own_request, said in the inbox)
// and the controller gives there, which posts it; one withdrawn returns to
// parked, and one cancelled is so for good.
func TestPostAndApprove(t *testing.T) {
	s, d := journalStack(t)
	b := s.as(d.accountant)
	small := parkAs(b, d, pair("1200.00")...)
	page := b.get(parkedPath(small)).page(t)
	holds(t, "a parked entry", page.mainText(), "Change, post or cancel", "Change the entry", "Post the entry", "Cancel the entry",
		"Replace the lines from a CSV file")
	parkedDecided(t, "post within the limit", decideParkedAs(b, small, "post", nil), small, "posted")
	posted := b.get(parkedPath(small) + "?done=posted").page(t)
	holds(t, "posted", posted.mainText(), "The entry is posted.", "Status Posted", "Open the journal entry it was posted as")
	var journal string
	for _, a := range posted.all("a", "") {
		if strings.HasPrefix(attrOf(a, "href"), "/journal/entries/") {
			journal = attrOf(a, "href")
		}
	}
	holds(t, "its journal entry", b.get(journal).page(t).mainText(), "Open the parked entry it was posted for")
	if parkedFormOf(posted, small, "post") != nil {
		t.Error("a posted entry: offered its post")
	}
	big := parkAs(b, d, pair("12000.00")...)
	parkedDecided(t, "post above the limit", decideParkedAs(b, big, "post", nil), big, "submitted")
	waits := b.get(parkedPath(big) + "?done=submitted").page(t)
	holds(t, "waiting", waits.mainText(), "The entry is above the company's limit", "Status Waiting for approval", "Open its approval request",
		"You asked for it, so you may not approve it", "Withdraw the approval request")
	request := requestOf(t, waits)
	own := b.get(requestPath(request)).page(t)
	holds(t, "the accountant's own request", own.mainText(), "Manual journal entry", "You asked for this approval")
	refused := b.do(http.MethodPost, requestPath(request)+"/approve", url.Values{"csrf_token": {b.csrf()}, "if_match": {`"1"`},
		"idempotency_key": {uuid.NewString()}})
	holds(t, "the accountant approves anyway", refused.page(t).mainText(), "workflow.own_request")
	controller := s.as(d.f.Approver)
	approve := controller.get(requestPath(request)).page(t).form(requestPath(request) + "/approve")
	if r := controller.do(http.MethodPost, requestPath(request)+"/approve", values(approve, "Accrual agreed")); r.status != http.StatusSeeOther {
		t.Fatalf("the controller's approval: got %d %s", r.status, r.page(t).mainText())
	}
	if st, _ := parkedStatus(t, big); st != "posted" {
		t.Errorf("approved: got %s, want posted", st)
	}
	holds(t, "approved", b.get(parkedPath(big)).page(t).mainText(), "Status Posted", "Open the journal entry it was posted as")
	again := parkAs(b, d, pair("15000.00")...)
	parkedDecided(t, "post the third", decideParkedAs(b, again, "post", nil), again, "submitted")
	none := decideParkedAs(b, again, "withdraw", url.Values{"comment": {" "}})
	status(t, "a withdrawal without its reason", none, http.StatusUnprocessableEntity)
	holds(t, "a withdrawal without its reason", none.page(t).mainText(), "Enter the reason.")
	parkedDecided(t, "withdraw", decideParkedAs(b, again, "withdraw", url.Values{"comment": {"Wrong period"}}), again, "withdrawn")
	if st, _ := parkedStatus(t, again); st != "parked" {
		t.Errorf("withdrawn: got %s, want parked", st)
	}
	parkedDecided(t, "cancel", decideParkedAs(b, again, "cancel", url.Values{"comment": {"Not needed"}}), again, "cancelled")
	holds(t, "cancelled", b.get(parkedPath(again)+"?done=cancelled").page(t).mainText(), "The entry is cancelled.", "Status Cancelled")
	if st, _ := parkedStatus(t, again); st != "cancelled" {
		t.Errorf("cancelled: got %s, want cancelled", st)
	}
}

// TestParkedRefusals: a stale form is refused with its reason kept; a
// form sent again replays the first answer; a decision the entry's
// status no longer allows is explained by its code; a withdrawal naming
// another entry's request finds nothing; a form that is not the page's is
// refused.
func TestParkedRefusals(t *testing.T) {
	s, d := journalStack(t)
	b := s.as(d.accountant)
	id := parkAs(b, d, pair("100.00")...)
	cancel := parkedFormOf(b.get(parkedPath(id)).page(t), id, "cancel")
	pgtest.Exec(t, `UPDATE ledger.parked_entry SET version = version + 1 WHERE id = $1`, id)
	stale := postForm(b, cancel, parkedPath(id)+"/cancel", url.Values{"comment": {"Kept, though refused"}})
	status(t, "stale", stale, http.StatusPreconditionFailed)
	holds(t, "stale", stale.page(t).mainText(), "The entry changed since you opened it.", "Kept, though refused", "request.version_mismatch")
	post := parkedFormOf(b.get(parkedPath(id)).page(t), id, "post")
	first := postForm(b, post, parkedPath(id)+"/post", nil)
	parkedDecided(t, "post", first, id, "posted")
	if again := postForm(b, post, parkedPath(id)+"/post", nil); again.location() != first.location() {
		t.Errorf("the form sent again: got %d to %q, want the first answer", again.status, again.location())
	}
	late := postForm(b, cancel, parkedPath(id)+"/cancel", url.Values{"comment": {"Too late"}, "idempotency_key": {uuid.NewString()},
		"if_match": {fmt.Sprintf(`"%d"`, 3)}})
	status(t, "cancel once posted", late, http.StatusConflict)
	holds(t, "cancel once posted", late.page(t).mainText(), "The entry is no longer parked", "ledger.invalid_parked_status")
	big := parkAs(b, d, pair("20000.00")...)
	parkedDecided(t, "post", decideParkedAs(b, big, "post", nil), big, "submitted")
	other := parkAs(b, d, pair("30000.00")...)
	parkedDecided(t, "post the other", decideParkedAs(b, other, "post", nil), other, "submitted")
	withdraw := parkedFormOf(b.get(parkedPath(big)).page(t), big, "withdraw")
	status(t, "another entry's request", postForm(b, withdraw, parkedPath(other)+"/withdraw", url.Values{"comment": {"r"}}), http.StatusNotFound)
	status(t, "no request named", postForm(b, withdraw, parkedPath(big)+"/withdraw", url.Values{"comment": {"r"}, "approval_request_id": {"x"}}),
		http.StatusNotFound)
	t.Run("a form that is not the page's", func(t *testing.T) {
		token := b.csrf()
		status(t, "no key", b.do(http.MethodPost, parkedPath(id)+"/post", url.Values{"csrf_token": {token}, "if_match": {`"1"`}}), http.StatusBadRequest)
		status(t, "no entry's id", b.do(http.MethodPost, "/journal/parked/x/post", url.Values{"csrf_token": {token}}), http.StatusNotFound)
		status(t, "no such entry", b.get(parkedPath(uuid.New())), http.StatusNotFound)
		status(t, "no entry's page", b.get("/journal/parked/x"), http.StatusNotFound)
		status(t, "no entry's change", b.get("/journal/parked/x/edit"), http.StatusNotFound)
		status(t, "no entry to change", b.do(http.MethodPost, "/journal/parked/x/edit", url.Values{"csrf_token": {token}}), http.StatusNotFound)
		status(t, "no entry's lines", b.do(http.MethodPost, "/journal/parked/x/lines", url.Values{"csrf_token": {token}}), http.StatusNotFound)
	})
}

// TestTakeOnAgentsProposal: an agent's proposal no person owns is
// offered to the person, who takes it on by changing it and then posts
// it; another person, no longer offered it, is refused by its code.
func TestTakeOnAgentsProposal(t *testing.T) {
	s, d := journalStack(t)
	b := s.as(d.accountant)
	p := b.get(parkedPath(d.proposal)).page(t)
	holds(t, "the agent's proposal", p.mainText(), "An agent's proposal", "If you change, post or cancel it, you take it on", "Owner No one yet")
	edit := b.get(parkedPath(d.proposal) + "/edit").page(t).form(parkedPath(d.proposal) + "/edit")
	v := formValues(edit)
	v.Set("header_text", "Accrual, checked")
	parkedDecided(t, "taken on", b.do(http.MethodPost, parkedPath(d.proposal)+"/edit", v), d.proposal, "changed")
	if _, owner := parkedStatus(t, d.proposal); owner != d.accountant.ID.String() {
		t.Errorf("taken on: owner %q, want the accountant", owner)
	}
	holds(t, "taken on", b.get(parkedPath(d.proposal)+"?done=changed").page(t).mainText(), "The entry is changed.", "Header text Accrual, checked",
		"Owner accountant")
	colleague := s.as(d.colleague)
	cp := colleague.get(parkedPath(d.proposal)).page(t)
	holds(t, "the colleague's view", cp.mainText(), "Only its owner changes, posts or cancels this entry.")
	if parkedFormOf(cp, d.proposal, "post") != nil {
		t.Error("the colleague is offered the post")
	}
	post := parkedFormOf(b.get(parkedPath(d.proposal)).page(t), d.proposal, "post")
	r := postForm(colleague, post, parkedPath(d.proposal)+"/post", url.Values{"csrf_token": {colleague.csrf()}})
	status(t, "the colleague posts anyway", r, http.StatusForbidden)
	holds(t, "the colleague posts anyway", r.page(t).mainText(), "Only the entry's owner changes, posts or cancels it.", "ledger.not_entry_owner")
	parkedDecided(t, "the owner posts", decideParkedAs(b, d.proposal, "post", nil), d.proposal, "posted")
}

// TestChangeEntry: a parked entry's change, its form as the entry is (its
// amounts in the person's locale, its centers by code), its lines
// changed, a stale form refused with what was typed kept, and an entry
// no longer parked not offered for change.
func TestChangeEntry(t *testing.T) {
	s, d := journalStack(t)
	b := s.as(d.accountant)
	id := parkAs(b, d, typedLine{account: "520000", amount: "80.00", cost: "CC100", text: "Toner"}, typedLine{account: "100000", amount: "-80.00"})
	form := b.get(parkedPath(id)+"/edit", "Accept-Language", "fr-CA").page(t).form(parkedPath(id) + "/edit")
	v := formValues(form)
	if got := v["line_amount"]; len(got) != 2 || got[0] != "80,00" || v["line_cost_center"][0] != "CC100" || v["line_text"][0] != "Toner" {
		t.Errorf("the change's form: got %v, want the entry's lines in French", v)
	}
	v = withLines(v, typedLine{account: "530000", amount: "90.00", cost: "CC100"}, typedLine{account: "100000", amount: "-90.00"})
	v.Set("header_text", "")
	v.Set("tax_date", "2026-03-14")
	v.Set("reference", "R-9")
	pgtest.Exec(t, `UPDATE ledger.parked_entry SET version = version + 1 WHERE id = $1`, id)
	stale := b.do(http.MethodPost, parkedPath(id)+"/edit", v)
	status(t, "stale", stale, http.StatusPreconditionFailed)
	sp := stale.page(t)
	holds(t, "stale", sp.mainText(), "The entry changed since you opened it.", "request.version_mismatch")
	v = formValues(sp.form(parkedPath(id) + "/edit"))
	if v.Get("line_account") != "530000" {
		t.Errorf("stale: what was typed is not kept: %v", v)
	}
	parkedDecided(t, "changed", b.do(http.MethodPost, parkedPath(id)+"/edit", v), id, "changed")
	holds(t, "changed", b.get(parkedPath(id)).page(t).mainText(), "530000 IT Services EUR 90.00 CC100", "Tax date 2026-03-14", "Reference R-9")
	if got := field(b.get(parkedPath(id)+"/edit").page(t).form(parkedPath(id)+"/edit"), "tax_date"); got != "2026-03-14" {
		t.Errorf("the change's form: tax date %q, want the entry's", got)
	}
	v.Set("op", "add")
	if r := b.do(http.MethodPost, parkedPath(id)+"/edit", v, "HX-Request", "true"); r.status != http.StatusOK || !strings.Contains(r.body, "line-3-line_account") {
		t.Errorf("a line added to the change with htmx: got %d %s", r.status, r.body)
	}
	v = withLines(formValues(b.get(parkedPath(id)+"/edit").page(t).form(parkedPath(id)+"/edit")), typedLine{account: "530000", amount: "90.00"},
		typedLine{account: "100000", amount: "-80.00"})
	r := b.do(http.MethodPost, parkedPath(id)+"/edit", v)
	status(t, "unbalanced", r, http.StatusUnprocessableEntity)
	holds(t, "unbalanced", r.page(t).mainText(), "ledger.unbalanced")
	wrong := b.do(http.MethodPost, parkedPath(id)+"/edit", withLines(v, typedLine{account: "530000", amount: "x"}, typedLine{account: "100000", amount: "-1"}))
	status(t, "wrong", wrong, http.StatusUnprocessableEntity)
	parkedDecided(t, "post", decideParkedAs(b, id, "post", nil), id, "posted")
	if r := b.get(parkedPath(id) + "/edit"); r.status != http.StatusSeeOther || r.location() != parkedPath(id) {
		t.Errorf("a posted entry's change: got %d to %q, want its page", r.status, r.location())
	}
	status(t, "a form that is not the page's", b.do(http.MethodPost, parkedPath(id)+"/edit", url.Values{"csrf_token": {b.csrf()}}), http.StatusBadRequest)
}

// requestOf is the approval request a parked entry's page links.
func requestOf(t *testing.T, p page) uuid.UUID {
	t.Helper()
	for _, a := range p.all("a", "") {
		if id, ok := strings.CutPrefix(attrOf(a, "href"), "/approvals/"); ok {
			return uuid.MustParse(id)
		}
	}
	t.Fatalf("no approval request linked in %s", p.mainText())
	return uuid.Nil
}

// uploadLinesAs posts a parked entry's upload form as its page offers it,
// with the file when not nil and the fields given besides.
func uploadLinesAs(b *browser, id uuid.UUID, file []byte, fields map[string]string, header ...string) response {
	b.s.t.Helper()
	form := b.get(parkedPath(id)).page(b.s.t).form(parkedPath(id) + "/lines")
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	v := map[string]string{"csrf_token": field(form, "csrf_token"), "if_match": field(form, "if_match"), "idempotency_key": field(form, "idempotency_key")}
	for k, x := range fields {
		v[k] = x
	}
	for k, x := range v {
		_ = w.WriteField(k, x) // into memory
	}
	if file != nil {
		part, _ := w.CreateFormFile("file", "lines.csv") // into memory
		_, _ = part.Write(file)
	}
	_ = w.Close()
	return b.raw(http.MethodPost, parkedPath(id)+"/lines", &body, w.FormDataContentType(), header...)
}

// TestUploadLines: a CSV file's lines replace a parked entry's, comma- or
// semicolon-separated with the amounts as typed in the person's locale;
// a file that is none, its header or rows wrong, or lines the API
// refuses, are said by the row.
func TestUploadLines(t *testing.T) {
	s, d := journalStack(t)
	b := s.as(d.accountant)
	id := parkAs(b, d, pair("1.00")...)
	csv := "\xef\xbb\xbfaccount,amount,cost_center,line_text\n520000,300.00,CC100,Paper\n,,,\n530000,200.00,,\"IT, March\"\n100000,-500.00,,\n"
	parkedDecided(t, "uploaded", uploadLinesAs(b, id, []byte(csv), nil), id, "uploaded")
	holds(t, "uploaded", b.get(parkedPath(id)+"?done=uploaded").page(t).mainText(), "The file's lines replace the entry's lines.",
		"1 520000 Office Supplies EUR 300.00 CC100", "IT, March", "3 100000 Bank - Operating EUR -500.00")
	french := "account;amount\n520000;12,50\n100000;-12,50\n"
	r := uploadLinesAs(b, id, []byte(french), nil, "Accept-Language", "fr-CA")
	status(t, "French, by semicolons", r, http.StatusSeeOther)
	r = b.raw(http.MethodPost, parkedPath(id)+"/lines", strings.NewReader("x"), "multipart/form-data; boundary=b")
	status(t, "a body that is no form", r, http.StatusForbidden)
	for name, tc := range map[string]struct {
		file []byte
		want []string
	}{
		"no file":       {nil, []string{"Choose the CSV file."}},
		"not UTF-8":     {[]byte("account,amount\n\xff\n"), []string{"The file is not in UTF-8."}},
		"no header":     {[]byte(""), []string{"Choose the CSV file."}},
		"a column":      {[]byte("account,amount,colour\n"), []string{"names a column twice, or one the file does not take"}},
		"twice":         {[]byte("account,amount,account\n"), []string{"names a column twice"}},
		"no amount":     {[]byte("account\n520000\n"), []string{"must name the account and amount columns"}},
		"no rows":       {[]byte("account,amount\n,\n"), []string{"The file has no line under its first row."}},
		"quotes":        {[]byte("account,amount\n\"520000,1\n"), []string{"A row of the file could not be read"}},
		"only a header": {[]byte("\n"), []string{"The file has no first row naming its columns."}},
		"wrong rows":    {[]byte("account,amount,tax_code\n999999,1.00,\n520000,x,VVVVV\n"), []string{"Rows of the file to correct", "Row 3, amount: Enter the amount", "Row 3, tax_code: A tax code has"}},
		"unknown codes": {[]byte("account,amount,partner\n999999,1.00,\n100000,-1.00,BP0\n"), []string{"Row 2, account: No account of the company's chart has this number.", "Row 3, partner: There is no partner with this number."}},
		"extra fields":  {[]byte("account,amount\n520000,1.00,x\n100000,-1.00\n"), []string{"has more fields than its first row names columns"}},
		"one line":      {[]byte("account,amount\n520000,1.00\n"), []string{"An entry has at least two lines."}},
		"refused by the API": {[]byte("account,amount,cost_center\n520000,1.00,\n100000,-1.00,CC100\n"), []string{"The lines were not replaced",
			"ledger.cost_center_invalid", "Row 3, cost_center: refused by the ERP (not_allowed)."}},
		"unbalanced": {[]byte("account,amount\n520000,1.00\n100000,-2.00\n"), []string{"The lines do not balance", "ledger.unbalanced"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := uploadLinesAs(b, id, tc.file, nil)
			if r.status < 400 {
				t.Fatalf("got %d %s, want a refusal", r.status, r.location())
			}
			holds(t, name, r.page(t).mainText(), tc.want...)
		})
	}
	var many strings.Builder
	many.WriteString("account,amount\n")
	for i := 0; i < 25; i++ {
		many.WriteString("999999,1.00\n")
	}
	r = uploadLinesAs(b, id, []byte(many.String()), nil)
	holds(t, "many wrong rows", r.page(t).mainText(), "5 more rows are to be corrected.")
	var partners strings.Builder
	partners.WriteString("account,amount,partner\n100000,-21.00,\n")
	for i := 0; i < maxCodeLookups+1; i++ {
		fmt.Fprintf(&partners, "211000,1.00,BP%d\n", i)
	}
	r = uploadLinesAs(b, id, []byte(partners.String()), nil)
	holds(t, "more partners than are looked up", r.page(t).mainText(), "Row 22, partner: There is no partner", "1 more row is to be corrected.")
	status(t, "a form that is not the page's", uploadLinesAs(b, id, []byte(french), map[string]string{"idempotency_key": "x"}), http.StatusBadRequest)
}

// TestJournalBrowse: the journal of company 2000 in the leading ledger,
// filtered by its cost and profit centers and by its other filters; an
// entry's page with its lines and centers; its reversal, linked both
// ways and not reversed again; the reversal of an entry the payables
// context owns refused by its code.
func TestJournalBrowse(t *testing.T) {
	s, d := journalStack(t)
	b := s.as(d.accountant)
	q := url.Values{"legal_entity_id": {d.f.DE.String()}}
	list := b.get("/journal/entries?" + q.Encode())
	status(t, "the journal", list, http.StatusOK)
	holds(t, "the journal", list.page(t).mainText(), "Journal", "Office supplies", "Debits EUR 250.00", "Origin A manual entry", "A supplier invoice",
		"CC100 Cost center hundred", "PC10 Profit center ten")
	q.Set("cost_center_id", d.centers.Cost.String())
	byCost := b.get("/journal/entries?" + q.Encode()).page(t).mainText()
	holds(t, "by cost center", byCost, "Office supplies")
	if strings.Contains(byCost, "A supplier invoice") {
		t.Error("by cost center: the payables entry is listed")
	}
	q.Del("cost_center_id")
	q.Set("profit_center_id", d.centers.Profit.String())
	holds(t, "by profit center", b.get("/journal/entries?"+q.Encode()).page(t).mainText(), "Office supplies")
	for name, tc := range map[string]struct {
		query  string
		status int
		want   string
	}{
		"a date that is none": {"posting_date_from=15.03.2026", http.StatusUnprocessableEntity, "Enter a date as year-month-day"},
		"a center, no id":     {"cost_center_id=x", http.StatusBadRequest, "A filter in the address is not valid."},
		"a page token, none":  {"page=nonsense", http.StatusBadRequest, "request.invalid_page_token"},
		"a number":            {"document_number=0", http.StatusOK, "No posted entry matches the filters."},
		"until":               {"posting_date_to=2026-01-01", http.StatusOK, "No posted entry matches the filters."},
	} {
		t.Run(name, func(t *testing.T) {
			r := b.get("/journal/entries?" + tc.query)
			status(t, name, r, tc.status)
			holds(t, name, r.page(t).mainText(), tc.want)
		})
	}
	e := b.get(entryPath(d.centered)).page(t)
	holds(t, "the entry", e.mainText(), "Journal entry", "Company Acme GmbH", "Ledger 0L Leading ledger", "Period Period 3 of 2026", "Posted by human",
		"1 520000 Office Supplies EUR 250.00 EUR 250.00 CC100 PC10 SEG1", "Reverse the entry")
	reversed := postForm(b, e.form(entryPath(d.centered)+"/reverse"), entryPath(d.centered)+"/reverse", url.Values{"header_text": {"Wrong account"}})
	rest, ok := strings.CutPrefix(reversed.location(), "/journal/entries/")
	if reversed.status != http.StatusSeeOther || !ok || !strings.HasSuffix(rest, "?done=reversed") {
		t.Fatalf("reverse: got %d to %q: %s", reversed.status, reversed.location(), reversed.body)
	}
	rp := b.get(reversed.location()).page(t)
	holds(t, "the reversal", rp.mainText(), "The reversal is posted", "Header text Wrong account", "EUR -250.00", "Open the entry it reverses")
	if len(rp.all("form", "decide")) != 0 {
		t.Error("the reversal: offered its reversal")
	}
	original := b.get(entryPath(d.centered)).page(t)
	holds(t, "the original", original.mainText(), "Open the entry that reversed it")
	again := postForm(b, e.form(entryPath(d.centered)+"/reverse"), entryPath(d.centered)+"/reverse", url.Values{"idempotency_key": {uuid.NewString()}})
	status(t, "reversed again from the page before", again, http.StatusPreconditionFailed)
	holds(t, "reversed again from the page before", again.page(t).mainText(), "The entry was not reversed", "request.version_mismatch")
	pay := b.get(entryPath(d.payables)).page(t)
	owned := postForm(b, pay.form(entryPath(d.payables)+"/reverse"), entryPath(d.payables)+"/reverse", nil)
	status(t, "owned elsewhere", owned, http.StatusConflict)
	holds(t, "owned elsewhere", owned.page(t).mainText(), "Another document owns this entry", "ledger.entry_owned_elsewhere")
	wrong := postForm(b, pay.form(entryPath(d.payables)+"/reverse"), entryPath(d.payables)+"/reverse", url.Values{"posting_date": {"x"},
		"header_text": {strings.Repeat("h", 201)}})
	status(t, "what is wrong", wrong, http.StatusUnprocessableEntity)
	holds(t, "what is wrong", wrong.page(t).mainText(), "Enter a date as year-month-day", "A text has at most 200 characters.")
	t.Run("a form that is not the page's", func(t *testing.T) {
		token := b.csrf()
		status(t, "no key", b.do(http.MethodPost, entryPath(d.payables)+"/reverse", url.Values{"csrf_token": {token}, "if_match": {`"1"`}}), http.StatusBadRequest)
		status(t, "no entry's id", b.do(http.MethodPost, "/journal/entries/x/reverse", url.Values{"csrf_token": {token}}), http.StatusNotFound)
		status(t, "no such entry", b.get(entryPath(uuid.New())), http.StatusNotFound)
		status(t, "no entry's page", b.get("/journal/entries/x"), http.StatusNotFound)
	})
	holds(t, "French", b.get(entryPath(d.centered), "Accept-Language", "fr-CA").page(t).mainText(), "Écriture de journal", "250,00 EUR")
}

// TestJournalAsOthers: a person who may only read entries sees the
// parked ones and the journal and is offered nothing to do; a person who
// may read nothing is told why on every page, and the journal is not in
// their navigation; a segregation-of-duties rule barring the post refuses
// it, said by its code with the barred permissions.
func TestJournalAsOthers(t *testing.T) {
	s, d := journalStack(t)
	viewer := s.as(d.viewer)
	list := viewer.get("/journal")
	status(t, "the viewer's list", list, http.StatusOK)
	if strings.Contains(list.body, "Enter a new entry") {
		t.Error("the viewer is offered a new entry")
	}
	vp := viewer.get(parkedPath(d.proposal)).page(t)
	holds(t, "the viewer's entry", vp.mainText(), "You may read this entry, but not change or post it.")
	if len(vp.all("form", "decide")) != 0 || strings.Contains(vp.mainText(), "An agent's proposal") {
		t.Error("the viewer is offered a decision")
	}
	holds(t, "the viewer's journal entry", viewer.get(entryPath(d.centered)).page(t).mainText(), "You may read this entry, but not reverse it.")
	if j := viewer.get("/journal/entries?legal_entity_id=" + d.f.DE.String()).page(t).mainText(); !strings.Contains(j, "Office supplies") || strings.Contains(j, "Cost center") {
		t.Errorf("the viewer's journal, without the centers: got %q", j)
	}
	outsider := s.as(d.outsider)
	r := outsider.get("/journal")
	status(t, "the outsider's list", r, http.StatusForbidden)
	holds(t, "the outsider's list", r.page(t).mainText(), "The entries could not be shown", "You do not have the permission this needs.")
	for _, a := range r.page(t).all("a", "navigation__link") {
		if attrOf(a, "href") == "/journal" {
			t.Error("the outsider's navigation offers the journal")
		}
	}
	for _, path := range []string{parkedPath(d.proposal), entryPath(d.centered), "/journal/entries", parkedPath(d.proposal) + "/edit"} {
		got := outsider.get(path)
		status(t, path, got, http.StatusForbidden)
		holds(t, path, got.page(t).mainText(), "You do not have the permission this needs.")
	}
	nf := outsider.get("/journal/new")
	holds(t, "the outsider's new entry", nf.page(t).mainText(), "There is no company you may enter an entry in.")
	b := s.as(d.accountant)
	id := parkAs(b, d, pair("5.00")...)
	post := parkedFormOf(b.get(parkedPath(id)).page(t), id, "post")
	pgtest.Exec(t, `INSERT INTO identity.sod_rule (tenant_id, id, permission_a, permission_b, description, valid_from, created_by)
		VALUES ($1, gen_random_uuid(), 'ledger.entry.park', 'ledger.entry.post', 'd', current_date, $2)`, s.ten.ID, s.ten.Human.ID)
	if parkedFormOf(b.get(parkedPath(id)).page(t), id, "post") != nil {
		t.Error("barred: offered the post")
	}
	barred := postForm(b, post, parkedPath(id)+"/post", nil)
	status(t, "posted anyway", barred, http.StatusForbidden)
	holds(t, "posted anyway", barred.page(t).mainText(), "A segregation-of-duties rule bars this", "ledger.entry.park", "ledger.entry.post")
}
