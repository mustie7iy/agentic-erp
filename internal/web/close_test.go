package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/html"

	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// closeData is what closeStack's tenant holds.
type closeData struct {
	f              fixture.Env
	leading, local uuid.UUID    // ledgers 0L and 2L, both kept by company 2000
	order          uuid.UUID    // the purchase order of company 2000's open GR/IR position
	statement      uuid.UUID    // company 2000's statement S1, imported and not reconciled
	accountant     pgtest.Actor // closes periods and revalues; holds every role, as the approvers do
	viewer         pgtest.Actor // may read periods, ledgers and companies alone
	outsider       pgtest.Actor // may read nothing
}

// closeStack is the workspace in front of a configured tenant whose
// accountant closes company 2000's periods and whose approver, holding
// the controller's role, decides their unlocks: 2L kept beside 0L with a
// posting in 2L alone in March, a USD supplier item a revaluation on 31
// March values at a loss of EUR 10.00, an open GR/IR position and a bank
// statement not reconciled; a person may only read periods and one may
// read nothing.
func closeStack(t *testing.T) (*stack, *closeData) {
	t.Helper()
	d := &closeData{}
	objects := blobstore.Dir{Root: t.TempDir()}
	perms := append([]string{"payments.statement.read", "payments.statement.import"}, fixture.ClosePermissions...)
	s := newStack(t, stackOptions{permissions: perms, extend: fixture.Parallel, objects: objects, seed: func(f fixture.Env) {
		d.f = f
		f.Revalues(t, fixture.Parallel)
		d.leading, d.local = f.Ledger(t, "0L"), f.Ledger(t, "2L")
		f.LedgerOnly(t, d.local, "2026-03-16", "25.00")
		d.order = f.GRIROpen(t)
		d.statement = f.ImportStatement(t, objects, fixture.MT940("S1", "1", "1000,00", "260416", "990,00", "260417", ":61:2604170417D10,00NMSCNONREF//B1\n"))[0]
		d.accountant = f.Person(t, "accountant")
		d.viewer = roleHolder(t, f, "viewer", "ledger.period.read", "ledger.account.read", "tenancy.org.read")
		d.outsider = roleHolder(t, f, "outsider")
	}})
	return s, d
}

// periodOf is the id of a period of company 2000 in 2026 in the ledger.
func periodOf(t *testing.T, d *closeData, ledger uuid.UUID, period int) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	pgtest.Scan(t, `SELECT id FROM ledger.period_status WHERE tenant_id = $1 AND legal_entity_id = $2 AND ledger_id = $3 AND fiscal_year = 2026 AND period = $4`,
		[]any{d.f.Tenant.ID, d.f.DE, ledger, period}, &id)
	return id
}

// periodStatus is a period's status, from the database.
func periodStatus(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var st string
	pgtest.Scan(t, `SELECT status FROM ledger.period_status WHERE id = $1`, []any{id}, &st)
	return st
}

// periodForm is the verb's form of the period's page, nil for none.
func periodForm(p page, id uuid.UUID, verb string) *html.Node {
	for _, f := range p.all("form", "") {
		if attrOf(f, "action") == periodPath(id)+"/"+verb {
			return f
		}
	}
	return nil
}

// decidePeriodAs posts the verb's form as the period's page offers it,
// with the fields given besides its own.
func decidePeriodAs(b *browser, id uuid.UUID, verb string, fields url.Values) response {
	b.s.t.Helper()
	f := periodForm(b.get(periodPath(id)).page(b.s.t), id, verb)
	if f == nil {
		b.s.t.Fatalf("period %s: no %s form", id, verb)
	}
	return postForm(b, f, periodPath(id)+"/"+verb, fields)
}

// decided checks a decision went back to the period's page with its notice.
func decided(t *testing.T, what string, r response, id uuid.UUID, notice string) {
	t.Helper()
	if want := periodPath(id) + "?done=" + notice; r.status != http.StatusSeeOther || r.location() != want {
		t.Fatalf("%s: got %d to %q, want %s: %s", what, r.status, r.location(), want, r.body)
	}
}

// offersPeriod checks which of the verbs' forms the period's page offers.
func offersPeriod(t *testing.T, what string, p page, id uuid.UUID, want map[string]bool) {
	t.Helper()
	for verb, offered := range want {
		if got := periodForm(p, id, verb) != nil; got != offered {
			t.Errorf("%s, %s: offered %v, want %v", what, verb, got, offered)
		}
	}
}

// TestPeriodList: the periods of the company, ledger and year chosen, the
// first company, the leading ledger and this year by default, each linked
// to its page; a parallel ledger's own periods; a year that is none is
// said before the API is called, a company that is no id refuses the
// filters.
func TestPeriodList(t *testing.T) {
	s, d := closeStack(t)
	b := s.as(d.accountant)
	r := b.get("/close")
	status(t, "the default list", r, http.StatusOK)
	p := r.page(t)
	selected := map[string]bool{}
	walk(p.doc, func(n *html.Node) {
		if n.Data == "option" && hasAttr(n, "selected") {
			selected[attrOf(n, "value")] = true
		}
	})
	if !selected[d.f.US.String()] || !selected[d.leading.String()] {
		t.Errorf("the default choice: got %v, want the first company by name (Acme Corp US) and the leading ledger", selected)
	}
	if got := field(p.form("/close"), "fiscal_year"); got != fmt.Sprint(time.Now().Year()) {
		t.Errorf("the default year: got %q, want this year", got)
	}
	current := ""
	for _, a := range p.all("a", "navigation__link") {
		if attrOf(a, "aria-current") == "page" {
			current = attrOf(a, "href")
		}
	}
	if current != "/close" {
		t.Errorf("navigation: the current entry is %q, want /close", current)
	}
	q := url.Values{"legal_entity_id": {d.f.DE.String()}, "ledger_id": {d.leading.String()}, "fiscal_year": {"2026"}}
	list := b.get("/close?" + q.Encode()).page(t)
	march := periodOf(t, d, d.leading, 3)
	holds(t, "company 2000's 2026", list.mainText(), "Period 3 of 2026 2026-03-01 2026-03-31 Open", "Foreign-currency revaluation of this company and ledger")
	linked := false
	for _, a := range list.all("a", "") {
		linked = linked || attrOf(a, "href") == periodPath(march)
	}
	if !linked {
		t.Error("period 3: not linked to its page")
	}
	q.Set("ledger_id", d.local.String())
	if got := b.get("/close?" + q.Encode()).page(t).mainText(); !strings.Contains(got, "Period 12 of 2026") {
		t.Errorf("2L's periods: got %q", got)
	}
	for name, tc := range map[string]struct {
		query  string
		status int
		want   string
	}{
		"a year that is none": {"fiscal_year=26", http.StatusUnprocessableEntity, "Enter the fiscal year as four digits"},
		"a company, no id":    {"legal_entity_id=x", http.StatusBadRequest, "A filter in the address is not valid."},
		"a page token, none":  {"fiscal_year=2026&page=nonsense", http.StatusBadRequest, "request.invalid_page_token"},
		"a year without any":  {"fiscal_year=2990", http.StatusOK, "This company and ledger have no period in this fiscal year."},
	} {
		t.Run(name, func(t *testing.T) {
			r := b.get("/close?" + tc.query)
			status(t, name, r, tc.status)
			holds(t, name, r.page(t).mainText(), tc.want)
		})
	}
	holds(t, "French", b.get("/close?"+q.Encode(), "Accept-Language", "fr-CA").page(t).mainText(), "Clôture de période", "Période 3, exercice 2026",
		"Ouverte")
}

// TestCloseLockAndUnlock is the accountant's and the controller's job:
// the accountant closes March and locks it, then asks for its unlock
// with a reason, which waits in the inbox; asked to approve it themself,
// they are refused by segregation of duties, said by its code; the
// controller approves it, which leaves the period closed, and the
// accountant opens it again.
func TestCloseLockAndUnlock(t *testing.T) {
	s, d := closeStack(t)
	b := s.as(d.accountant)
	march := periodOf(t, d, d.leading, 3)
	p := b.get(periodPath(march)).page(t)
	holds(t, "the open period", p.mainText(), "Posting period", "Period 3 of 2026", "Status Open", "Company Acme GmbH", "Ledger 0L Leading ledger",
		"First day 2026-03-01", "Last day 2026-03-31", "Close the period", "Preview the revaluation at the period's end", "Show the pre-close checks")
	offersPeriod(t, "the open period", p, march, map[string]bool{"close": true, "open": false, "lock": false, "unlock": false})
	decided(t, "close", decidePeriodAs(b, march, "close", nil), march, "closed")
	closed := b.get(periodPath(march) + "?done=closed").page(t)
	holds(t, "the closed period", closed.mainText(), "The period is closed.", "Status Closed", "Last changed by accountant")
	offersPeriod(t, "the closed period", closed, march, map[string]bool{"close": false, "open": true, "lock": true, "unlock": false})
	if strings.Contains(closed.mainText(), "Preview the revaluation") {
		t.Error("a closed period offers its revaluation")
	}
	decided(t, "lock", decidePeriodAs(b, march, "lock", nil), march, "locked")
	locked := b.get(periodPath(march)).page(t)
	holds(t, "the locked period", locked.mainText(), "Status Locked", "Ask for the unlock")
	offersPeriod(t, "the locked period", locked, march, map[string]bool{"open": false, "lock": false, "unlock": true})
	if strings.Contains(locked.mainText(), "Show the pre-close checks") {
		t.Error("a locked period offers its pre-close checks")
	}
	none := decidePeriodAs(b, march, "unlock", url.Values{"comment": {" "}})
	status(t, "an unlock without its reason", none, http.StatusUnprocessableEntity)
	holds(t, "an unlock without its reason", none.page(t).mainText(), "Enter the reason.")
	decided(t, "unlock", decidePeriodAs(b, march, "unlock", url.Values{"comment": {"Audit adjustment"}}), march, "unlock")
	waits := b.get(periodPath(march) + "?done=unlock").page(t)
	holds(t, "the unlock asked for", waits.mainText(), "The unlock is asked for. It waits for the controller's approval.", "Unlock requested",
		"Whoever asked for it may not approve it.", "Open the approval request")
	offersPeriod(t, "the unlock asked for", waits, march, map[string]bool{"unlock": false})
	request := approvalRequestOf(t, waits)
	own := b.get(requestPath(request)).page(t)
	holds(t, "the accountant's own request", own.mainText(), "Period unlock", "Unlock period 2026/3: Audit adjustment")
	refused := b.do(http.MethodPost, requestPath(request)+"/approve", url.Values{"csrf_token": {b.csrf()}, "if_match": {`"1"`},
		"idempotency_key": {uuid.NewString()}})
	holds(t, "the accountant approves anyway", refused.page(t).mainText(), "workflow.own_request")
	controller := s.as(d.f.Approver)
	approve := controller.get(requestPath(request)).page(t).form(requestPath(request) + "/approve")
	if r := controller.do(http.MethodPost, requestPath(request)+"/approve", values(approve, "Adjustment agreed")); r.status != http.StatusSeeOther {
		t.Fatalf("the controller's approval: got %d %s", r.status, r.page(t).mainText())
	}
	if got := periodStatus(t, march); got != "closed" {
		t.Fatalf("approved: got %s, want closed", got)
	}
	reopened := b.get(periodPath(march)).page(t)
	holds(t, "unlocked", reopened.mainText(), "Status Closed", "Open the period")
	decided(t, "open", decidePeriodAs(b, march, "open", nil), march, "opened")
	if got := periodStatus(t, march); got != "open" {
		t.Errorf("opened: got %s, want open", got)
	}
}

// TestPeriodRefusals: a stale form is refused with its reason kept; a
// form sent again replays the first answer; a decision the period's
// status no longer allows is explained by its code; a form that is not
// the page's is refused.
func TestPeriodRefusals(t *testing.T) {
	s, d := closeStack(t)
	b := s.as(d.accountant)
	april := periodOf(t, d, d.leading, 4)
	form := periodForm(b.get(periodPath(april)).page(t), april, "close")
	first := postForm(b, form, periodPath(april)+"/close", nil)
	decided(t, "close", first, april, "closed")
	if again := postForm(b, form, periodPath(april)+"/close", nil); again.location() != first.location() {
		t.Errorf("the form sent again: got %d to %q, want the first answer", again.status, again.location())
	}
	lock := periodForm(b.get(periodPath(april)).page(t), april, "lock")
	decided(t, "lock", postForm(b, lock, periodPath(april)+"/lock", nil), april, "locked")
	unlock := periodForm(b.get(periodPath(april)).page(t), april, "unlock")
	pgtest.Exec(t, `UPDATE ledger.period_status SET version = version + 1 WHERE id = $1`, april)
	stale := postForm(b, unlock, periodPath(april)+"/unlock", url.Values{"comment": {"Kept, though refused"}})
	status(t, "stale", stale, http.StatusPreconditionFailed)
	holds(t, "stale", stale.page(t).mainText(), "The period changed since you opened it.", "Kept, though refused", "request.version_mismatch")
	var version int
	pgtest.Scan(t, `SELECT version FROM ledger.period_status WHERE id = $1`, []any{april}, &version)
	opened := postForm(b, lock, periodPath(april)+"/open", url.Values{"idempotency_key": {uuid.NewString()}, "if_match": {fmt.Sprintf(`"%d"`, version)}})
	status(t, "opened once locked", opened, http.StatusConflict)
	holds(t, "opened once locked", opened.page(t).mainText(), "The period was not changed", "The period's status does not allow this",
		"ledger.invalid_period_status")
	t.Run("a form that is not the page's", func(t *testing.T) {
		token := b.csrf()
		status(t, "no key", b.do(http.MethodPost, periodPath(april)+"/close", url.Values{"csrf_token": {token}, "if_match": {`"1"`}}), http.StatusBadRequest)
		status(t, "no period id", b.do(http.MethodPost, "/close/periods/x/close", url.Values{"csrf_token": {token}}), http.StatusNotFound)
		status(t, "no such period", b.get(periodPath(uuid.New())), http.StatusNotFound)
		status(t, "no period's page", b.get("/close/periods/x"), http.StatusNotFound)
	})
}

// TestPreCloseChecks: a period's checks, a section htmx loads into its
// page or a page of their own: company 2000's open GR/IR position, its
// leading ledger's difference with 2L in March from the posting in 2L
// alone, and its statement not reconciled linked to its page; company
// 1000, keeping one ledger, has nothing to look at; a query that names
// no period is refused.
func TestPreCloseChecks(t *testing.T) {
	s, d := closeStack(t)
	b := s.as(d.accountant)
	march := periodOf(t, d, d.leading, 3)
	p := b.get(periodPath(march)).page(t)
	path := ""
	walk(p.doc, func(n *html.Node) {
		if v := attrOf(n, "hx-get"); strings.HasPrefix(v, "/close/checks?") {
			path = v
		}
	})
	if path == "" {
		t.Fatal("the period's page loads no checks")
	}
	section := b.get(path, "HX-Request", "true")
	if v := section.header.Values("Vary"); !strings.Contains(strings.Join(v, " "), "HX-Request") {
		t.Errorf("the section's Vary: got %v, want HX-Request in it", v)
	}
	got := text(section.page(t).doc)
	holds(t, "company 2000's checks", got, "Pre-close checks", "GR/IR positions still open", "To look at", d.order.String()+" 1 10 EA 4 EA EUR -72.00 2026-03-05",
		"Ledger 0L compared with ledger 2L in this period.", "Ledger 0L Debits", "Debits and credits net to zero",
		d.f.Account(t, "2000", "100000").String()+" EUR EUR 0.00 EUR 25.00 EUR -25.00",
		"Bank statements not reconciled", "S1", "Every statement imported, not reconciled")
	links := map[string]bool{}
	walk(section.page(t).doc, func(n *html.Node) { links[attrOf(n, "href")] = true })
	for _, want := range []string{statementPath(d.statement), "/bank?status=imported", "/bank?status=gap"} {
		if !links[want] {
			t.Errorf("the checks link %s: not in %v", want, links)
		}
	}
	whole := b.get(path).page(t)
	holds(t, "the checks' page", whole.mainText(), "Back to the period", "Pre-close checks")
	local := b.get(marchChecks(d, d.local)).page(t).mainText()
	holds(t, "2L's checks", local, "Ledger 2L compared with ledger 0L in this period.")
	us := b.get(checksQuery("legal_entity_id", d.f.US.String(), "ledger_id", d.leading.String())).page(t).mainText()
	holds(t, "company 1000's checks", us, "Every order item's receipts and invoices agree.", "The company keeps one ledger: there is nothing to reconcile.",
		"Every bank statement of the company is reconciled.")
	status(t, "a query that names no period", b.get("/close/checks?legal_entity_id=x"), http.StatusBadRequest)
}

// marchChecks is the checks of company 2000's March 2026 in the ledger.
func marchChecks(d *closeData, ledger uuid.UUID) string {
	return checksQuery("legal_entity_id", d.f.DE.String(), "ledger_id", ledger.String())
}

// checksQuery is the checks of March 2026 with the fields given, as
// name, value pairs.
func checksQuery(kv ...string) string {
	q := url.Values{"fiscal_year": {"2026"}, "period": {"3"}}
	for i := 0; i+1 < len(kv); i += 2 {
		q.Set(kv[i], kv[i+1])
	}
	return "/close/checks?" + q.Encode()
}

// previewAs posts the revaluation's preview form as the browser's person
// for company 2000's leading ledger, with the fields given besides; it
// returns the answer.
func previewAs(b *browser, d *closeData, fields url.Values) response {
	b.s.t.Helper()
	form := b.get("/close/revaluation").page(b.s.t).form("/close/revaluation/preview")
	v := url.Values{"csrf_token": {field(form, "csrf_token")}, "idempotency_key": {field(form, "idempotency_key")},
		"legal_entity_id": {d.f.DE.String()}, "ledger_id": {d.leading.String()}, "key_date": {"2026-03-31"}}
	for k, vs := range fields {
		v[k] = vs
	}
	return b.do(http.MethodPost, "/close/revaluation/preview", v)
}

// TestRevaluation: from March's page the accountant previews the
// revaluation at its end, the loss on the USD supplier item valued, and
// posts it; the run's page shows its entries, totals and items, and the
// company's runs list it; a second run of the key date is refused, said
// by its code; a key date that is no period's end is refused; what is
// wrong is said before the API is called.
func TestRevaluation(t *testing.T) {
	s, d := closeStack(t)
	b := s.as(d.accountant)
	march := periodOf(t, d, d.leading, 3)
	link := ""
	for _, a := range b.get(periodPath(march)).page(t).all("a", "button") {
		if strings.HasPrefix(attrOf(a, "href"), "/close/revaluation?") {
			link = attrOf(a, "href")
		}
	}
	form := b.get(link).page(t).form("/close/revaluation/preview")
	if got := field(form, "key_date"); got != "2026-03-31" {
		t.Errorf("the period's revaluation: key date %q, want its end", got)
	}
	r := previewAs(b, d, nil)
	status(t, "the preview", r, http.StatusOK)
	pv := r.page(t)
	holds(t, "the preview", pv.mainText(), "What the revaluation would value", "Key date 2026-03-31", "Period Period 3 of 2026", "Rate type Bank selling rate",
		"211000 Accounts Payable - Domestic USD Loss 1 USD -100.00 EUR -80.00 EUR -90.00 EUR -10.00", "line 2", "0.900000000000", "Post the revaluation")
	post := pv.form("/close/revaluation/post")
	posted := postForm(b, post, "/close/revaluation/post", nil)
	run, ok := strings.CutPrefix(strings.TrimSuffix(posted.location(), "?done=posted"), "/close/revaluation/")
	if posted.status != http.StatusSeeOther || !ok {
		t.Fatalf("post: got %d to %q: %s", posted.status, posted.location(), posted.body)
	}
	if again := postForm(b, post, "/close/revaluation/post", nil); again.location() != posted.location() {
		t.Errorf("the form sent again: got %d to %q, want the first answer", again.status, again.location())
	}
	rp := b.get(posted.location()).page(t)
	holds(t, "the run", rp.mainText(), "The revaluation is posted.", "Status Posted", "Company Acme GmbH", "Ledger 0L Leading ledger", "Key date 2026-03-31",
		"Posted by accountant", "Valuation entry, on the key date", "Reset entry, on the next day", "EUR -10.00")
	list := b.get("/close/revaluation?legal_entity_id=" + d.f.DE.String()).page(t)
	found := false
	for _, a := range list.all("a", "") {
		found = found || attrOf(a, "href") == "/close/revaluation/"+run
	}
	if !found {
		t.Error("the company's runs: the run is not listed")
	}
	second := postForm(b, previewAs(b, d, nil).page(t).form("/close/revaluation/post"), "/close/revaluation/post", nil)
	status(t, "a second run", second, http.StatusConflict)
	holds(t, "a second run", second.page(t).mainText(), "The revaluation was not posted", "is posted already", "ledger.revaluation_exists")
	for name, tc := range map[string]struct {
		fields url.Values
		status int
		want   []string
	}{
		"no period's end": {url.Values{"key_date": {"2026-03-15"}}, http.StatusUnprocessableEntity, []string{"The revaluation could not be previewed",
			"not the last day of a posting period", "ledger.key_date_not_period_end"}},
		"no company":  {url.Values{"legal_entity_id": {""}}, http.StatusUnprocessableEntity, []string{"Choose the company."}},
		"no key date": {url.Values{"key_date": {"31.03.2026"}}, http.StatusUnprocessableEntity, []string{"Enter a date as year-month-day"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := previewAs(b, d, tc.fields)
			status(t, name, r, tc.status)
			holds(t, name, r.page(t).mainText(), tc.want...)
		})
	}
	t.Run("a form that is not the page's", func(t *testing.T) {
		token := b.csrf()
		status(t, "no key", b.do(http.MethodPost, "/close/revaluation/preview", url.Values{"csrf_token": {token}}), http.StatusBadRequest)
		status(t, "a ledger, no id", b.do(http.MethodPost, "/close/revaluation/post", url.Values{"csrf_token": {token},
			"idempotency_key": {uuid.NewString()}, "ledger_id": {"0L"}}), http.StatusBadRequest)
		status(t, "no run id", b.get("/close/revaluation/x"), http.StatusNotFound)
		status(t, "no such run", b.get("/close/revaluation/"+uuid.NewString()), http.StatusNotFound)
	})
	holds(t, "French", b.get(posted.location(), "Accept-Language", "fr-CA").page(t).mainText(), "Réévaluation", "Comptabilisée", "-10,00 EUR")
}

// TestCloseAsOthers: a person who may only read periods sees them and a
// period with nothing to decide, and no revaluation; a person who may
// read nothing is told why on every page, and the period close is not in
// their navigation.
func TestCloseAsOthers(t *testing.T) {
	s, d := closeStack(t)
	march := periodOf(t, d, d.leading, 3)
	viewer := s.as(d.viewer)
	list := viewer.get("/close")
	status(t, "the viewer's list", list, http.StatusOK)
	if strings.Contains(list.body, "Foreign-currency revaluation of this company") {
		t.Error("the viewer is offered the revaluation")
	}
	p := viewer.get(periodPath(march)).page(t)
	holds(t, "the viewer's period", p.mainText(), "You may read this period, but not change its status.")
	if len(p.all("form", "decide")) != 0 || strings.Contains(p.mainText(), "Preview the revaluation") {
		t.Error("the viewer is offered a decision")
	}
	rv := viewer.get("/close/revaluation")
	holds(t, "the viewer's revaluation", rv.page(t).mainText(), "The revaluations could not be shown", "You do not have the permission this needs.")
	if strings.Contains(rv.body, "/close/revaluation/preview") {
		t.Error("the viewer is offered the preview")
	}
	outsider := s.as(d.outsider)
	r := outsider.get("/close")
	status(t, "the outsider's list", r, http.StatusForbidden)
	holds(t, "the outsider's list", r.page(t).mainText(), "The periods could not be shown", "You do not have the permission this needs.")
	for _, a := range r.page(t).all("a", "navigation__link") {
		if attrOf(a, "href") == "/close" {
			t.Error("the outsider's navigation offers the period close")
		}
	}
	for _, path := range []string{periodPath(march), "/close/revaluation/" + uuid.NewString()} {
		got := outsider.get(path)
		status(t, path, got, http.StatusForbidden)
		holds(t, path, got.page(t).mainText(), "You do not have the permission this needs.")
	}
	checks := outsider.get(marchChecks(d, d.leading)).page(t).mainText()
	holds(t, "the outsider's checks", checks, "The check could not be run", "The company's ledgers could not be read")
}
