package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/net/html"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// runData is what paymentsStack's tenant holds.
type runData struct {
	f        fixture.Env
	bank     uuid.UUID    // house bank MAIN of company 2000
	proposer pgtest.Actor // proposes the runs; holds every role, as the approvers do
	viewer   pgtest.Actor // may read runs and companies alone
	outsider pgtest.Actor // may read nothing
}

// paymentsStack is the workspace in front of a configured tenant whose
// approver releases payment runs: the supplier has a bank account and
// an invoice of EUR 119.00 due on 9 April, the vendor an invoice of EUR
// 59.50 and no bank account, so a run of 15 April pays the one and
// leaves the other out; a person proposes runs, one may only read them
// and one may not. With limit, company 2000 releases a run above it only
// after a second person's approval (ADR-0038, T72m).
func paymentsStack(t *testing.T, limit string) (*stack, *runData) {
	t.Helper()
	d := &runData{}
	opts := stackOptions{permissions: append([]string{"payments.statement.read"}, fixture.PaymentPermissions...), seed: func(f fixture.Env) {
		d.f, d.bank = f, f.HouseBank(t)
		f.BankAccount(t, f.Supplier, "DE89370400440532013000", "Supplier GmbH")
		f.Payable(t, f.Supplier, "INV-1", "100.00", "19.00", "119.00")
		f.Payable(t, f.Vendor, "INV-2", "50.00", "9.50", "59.50")
		d.proposer = f.Person(t, "proposer")
		d.viewer = roleHolder(t, f, "viewer", "payments.run.read", "tenancy.org.read")
		d.outsider = roleHolder(t, f, "outsider")
	}}
	if limit != "" {
		opts.extend = func(c *config.Config) { c.Company("2000").ReleaseDualLimit = limit }
	}
	s := newStack(t, opts)
	// The workers take every tenant's work in the package database: none of
	// this test's is left for a later test's to count as its own.
	t.Cleanup(func() {
		fixture.RunProposals(t)
		fixture.RunReleases(t, s.objects)
		fixture.RunTransmissions(t, s.objects)
	})
	return s, d
}

// proposeAs posts the list's propose form as the browser's person, for
// company 2000 and house bank MAIN on 15 April with the fields given
// besides; it returns the answer.
func proposeAs(b *browser, d *runData, fields url.Values) response {
	b.s.t.Helper()
	form := b.get("/payments").page(b.s.t).form("/payments/propose")
	v := url.Values{"csrf_token": {field(form, "csrf_token")}, "idempotency_key": {field(form, "idempotency_key")},
		"legal_entity_id": {d.f.DE.String()}, "house_bank_id": {d.bank.String()}, "run_date": {"2026-04-15"}, "due_by": {"2026-04-15"}}
	for k, vs := range fields {
		v[k] = vs
	}
	return b.do(http.MethodPost, "/payments/propose", v)
}

// proposed proposes a run as the browser's person and has the proposal
// worker build it; it returns the run.
func proposed(b *browser, d *runData) uuid.UUID {
	b.s.t.Helper()
	run := runIn(b.s.t, proposeAs(b, d, nil))
	fixture.RunProposals(b.s.t)
	return run
}

// runForm is the verb's form of the run's page, nil for none.
func runForm(p page, run uuid.UUID, verb string) *html.Node {
	for _, f := range p.all("form", "") {
		if attrOf(f, "action") == runPath(run)+"/"+verb {
			return f
		}
	}
	return nil
}

// decideRunAs posts the verb's form as the run's page offers it, with the
// fields given besides its own.
func decideRunAs(b *browser, run uuid.UUID, verb string, fields url.Values) response {
	b.s.t.Helper()
	f := runForm(b.get(runPath(run)).page(b.s.t), run, verb)
	if f == nil {
		b.s.t.Fatalf("run %s: no %s form", run, verb)
	}
	return postForm(b, f, runPath(run)+"/"+verb, fields)
}

// postForm posts the form's hidden fields to path, with the fields given
// besides.
func postForm(b *browser, f *html.Node, path string, fields url.Values) response {
	b.s.t.Helper()
	v := url.Values{}
	walk(f, func(n *html.Node) {
		if n.Data == "input" && attrOf(n, "type") == "hidden" {
			v.Set(attrOf(n, "name"), attrOf(n, "value"))
		}
	})
	for k, vs := range fields {
		v[k] = vs
	}
	return b.do(http.MethodPost, path, v)
}

// runTag is a run's ETag now, from the database: a form sent as though
// the page were read again.
func runTag(t *testing.T, run uuid.UUID) string {
	t.Helper()
	var version int
	pgtest.Scan(t, `SELECT version FROM payments.payment_run WHERE id = $1`, []any{run}, &version)
	return fmt.Sprintf(`"%d"`, version)
}

// offers checks which of the verbs' forms the run's page offers.
func offers(t *testing.T, what string, p page, run uuid.UUID, want map[string]bool) {
	t.Helper()
	for verb, offered := range want {
		if got := runForm(p, run, verb) != nil; got != offered {
			t.Errorf("%s, %s: offered %v, want %v", what, verb, got, offered)
		}
	}
}

// runStatus is a run's status, from the database.
func runStatus(t *testing.T, run uuid.UUID) string {
	t.Helper()
	var st string
	pgtest.Scan(t, `SELECT status FROM payments.payment_run WHERE id = $1`, []any{run}, &st)
	return st
}

// runIn is the run a proposal's answer goes on to.
func runIn(t *testing.T, r response) uuid.UUID {
	t.Helper()
	run, ok := strings.CutSuffix(strings.TrimPrefix(r.location(), "/payments/"), "?done=proposing")
	if r.status != http.StatusSeeOther || !ok {
		t.Fatalf("propose: got %d to %q", r.status, r.location())
	}
	return uuid.MustParse(run)
}

// TestPropose: the proposer proposes a run from the list and goes to its
// page, where its progress is shown: a section htmx loads, whose status
// it asks again while the proposal runs and which reloads the page once
// it has stopped, or a page of its own; the built run shows its payments
// and what it left out, and offers its proposer no decision: one sent
// anyway is refused by segregation of duties, said by its code.
func TestPropose(t *testing.T) {
	s, d := paymentsStack(t, "")
	b := s.as(d.proposer)
	form := b.get("/payments").page(t).form("/payments/propose")
	holds(t, "the propose form", text(form), "Propose a payment run", "Company", "House bank", "MAIN •••• 3000 EUR", "Payment date")
	sent := url.Values{"csrf_token": {field(form, "csrf_token")}, "idempotency_key": {field(form, "idempotency_key")},
		"legal_entity_id": {d.f.DE.String()}, "house_bank_id": {d.bank.String()}, "run_date": {"2026-04-15"}, "due_by": {"2026-04-15"}}
	r := b.do(http.MethodPost, "/payments/propose", sent)
	run := runIn(t, r)
	p := b.get(r.location()).page(t)
	holds(t, "the run being proposed", p.mainText(), "The run is being proposed.", "Status Being proposed", "Show the progress",
		"The payments are listed once the proposal is built.")
	section := b.get(progressPath(run), "HX-Request", "true")
	holds(t, "the progress section", text(section.page(t).doc), "Progress", "Operation Building the proposal", "State Waiting to start", "0 done",
		"This updates on its own", "Cancel the operation")
	if v := section.header.Values("Vary"); !strings.Contains(strings.Join(v, " "), "HX-Request") {
		t.Errorf("the section's Vary: got %v, want HX-Request in it", v)
	}
	if got := b.get(progressPath(run)+"?part=status", "HX-Request", "true").header.Get("HX-Redirect"); got != "" {
		t.Errorf("the status while it runs: HX-Redirect %q, want none", got)
	}
	holds(t, "the progress page", b.get(progressPath(run)).page(t).mainText(), "Back to the run", "Progress of the payment run",
		"Waiting to start", "Refresh the progress")
	fixture.RunProposals(t)
	if got := b.get(progressPath(run)+"?part=status", "HX-Request", "true").header.Get("HX-Redirect"); got != runPath(run) {
		t.Errorf("the status once it stopped: HX-Redirect %q, want the run's page", got)
	}
	built := b.get(runPath(run)).page(t)
	holds(t, "the run proposed", built.mainText(), "Status Proposed", "Total EUR 119.00", "Proposed by proposer",
		"You proposed this run, so another person releases or rejects it.", d.f.Supplier.String()+" EUR 119.00 None None None 1",
		"Not yet", d.f.Vendor.String()+" The supplier has no bank account valid on the payment date EUR 59.50 None")
	if strings.Contains(built.mainText(), "Show the progress") {
		t.Error("the proposer's built run: offers its progress")
	}
	offers(t, "the proposer's built run", built, run, map[string]bool{"release": false, "reject": false, "submit": false})
	refused := b.do(http.MethodPost, runPath(run)+"/release", url.Values{"csrf_token": {b.csrf()}, "if_match": {runTag(t, run)},
		"idempotency_key": {uuid.NewString()}})
	status(t, "the proposer releases anyway", refused, http.StatusForbidden)
	holds(t, "the proposer releases anyway", refused.page(t).mainText(), "The run was not changed",
		"Segregation of duties: whoever proposed a run may neither release nor reject it", "identity.sod_conflict")
	if got := runStatus(t, run); got != "proposed" {
		t.Errorf("run: got %s, want proposed", got)
	}
	again := b.do(http.MethodPost, "/payments/propose", sent)
	if got := again.location(); got != r.location() {
		t.Errorf("the form sent again: got %d to %q, want the first answer %q", again.status, got, r.location())
	}
	fr := b.get(runPath(run), "Accept-Language", "fr-CA").page(t).mainText()
	holds(t, "French", fr, "Cycle de paiement", "119,00 EUR", "Proposé", "Écartés du cycle")
}

// TestProposeRefusals: what is wrong with a proposal is said before the
// API is called, what was sent kept; the API's refusal is explained by
// its code; a form that is not the page's is refused.
func TestProposeRefusals(t *testing.T) {
	s, d := paymentsStack(t, "")
	b := s.as(d.proposer)
	for name, tc := range map[string]struct {
		fields url.Values
		status int
		want   []string
	}{
		"no company":        {url.Values{"legal_entity_id": {""}}, http.StatusUnprocessableEntity, []string{"Choose the company."}},
		"no payment date":   {url.Values{"run_date": {""}}, http.StatusUnprocessableEntity, []string{"Enter a date as year-month-day"}},
		"a due date, none":  {url.Values{"due_by": {"15.04.2026"}}, http.StatusUnprocessableEntity, []string{"Enter a date as year-month-day"}},
		"a currency, none":  {url.Values{"currency": {"euro"}}, http.StatusUnprocessableEntity, []string{"Enter the currency as three capital letters"}},
		"no bank, currency": {url.Values{"house_bank_id": {""}}, http.StatusUnprocessableEntity, []string{"The run was not proposed", "Without a house bank, enter the run's currency.", "payments.currency_required"}},
		"another currency":  {url.Values{"currency": {"USD"}}, http.StatusUnprocessableEntity, []string{"The currency is not the house bank's."}},
	} {
		t.Run(name, func(t *testing.T) {
			r := proposeAs(b, d, tc.fields)
			status(t, "the proposal", r, tc.status)
			holds(t, "the proposal", r.page(t).mainText(), tc.want...)
		})
	}
	t.Run("what was sent is kept", func(t *testing.T) {
		form := proposeAs(b, d, url.Values{"currency": {"euro"}, "run_date": {"2026-04-16"}}).page(t).form("/payments/propose")
		kept := map[string]string{}
		walk(form, func(n *html.Node) {
			switch {
			case n.Data == "option" && hasAttr(n, "selected"):
				kept[attrOf(n, "value")] = "selected"
			case n.Data == "input":
				kept[attrOf(n, "name")] = attrOf(n, "value")
			}
		})
		for k, want := range map[string]string{d.f.DE.String(): "selected", d.bank.String(): "selected", "currency": "euro", "run_date": "2026-04-16"} {
			if kept[k] != want {
				t.Errorf("%s: got %q, want %q", k, kept[k], want)
			}
		}
	})
	if n := fixture.RunProposals(t); n != 0 {
		t.Errorf("proposals built after the refusals: got %d, want none", n)
	}
	t.Run("a form that is not the page's", func(t *testing.T) {
		token := b.csrf()
		for name, v := range map[string]url.Values{
			"no key":        {"csrf_token": {token}, "legal_entity_id": {d.f.DE.String()}},
			"a bank, no id": {"csrf_token": {token}, "idempotency_key": {uuid.NewString()}, "house_bank_id": {"MAIN"}},
		} {
			status(t, name, b.do(http.MethodPost, "/payments/propose", v), http.StatusBadRequest)
		}
		status(t, "no CSRF token", b.do(http.MethodPost, "/payments/propose", url.Values{"idempotency_key": {uuid.NewString()}}), http.StatusForbidden)
	})
}

// TestRunList: the list shows each run with its status, company, house
// bank, dates, total and proposer, linked to its page; the status tabs
// and the filters narrow it; a company that is no id refuses the
// filters.
func TestRunList(t *testing.T) {
	s, d := paymentsStack(t, "")
	run := proposed(s.as(d.proposer), d)
	b := s.as(s.f.Approver)
	r := b.get("/payments")
	status(t, "the list", r, http.StatusOK)
	p := r.page(t)
	cards := p.all("li", "card")
	if len(cards) != 1 || attrOf(p.cardLink(cards[0]), "href") != runPath(run) {
		t.Fatalf("cards: got %d, want the run's", len(cards))
	}
	holds(t, "the run's card", text(cards[0]), "Status Proposed", "Company Acme GmbH", "House bank MAIN", "Payment date 2026-04-15",
		"Pays items due by 2026-04-15", "Total EUR 119.00", "Proposed by proposer")
	current := ""
	for _, a := range p.all("a", "navigation__link") {
		if attrOf(a, "aria-current") == "page" {
			current = attrOf(a, "href")
		}
	}
	if current != "/payments" {
		t.Errorf("navigation: the current entry is %q, want /payments", current)
	}
	for name, tc := range map[string]struct {
		query string
		want  int
	}{
		"proposed":        {"status=proposed", 1},
		"released":        {"status=released", 0},
		"the company":     {"legal_entity_id=" + d.f.DE.String(), 1},
		"another company": {"legal_entity_id=" + d.f.US.String(), 0},
		"its number":      {"document_number=" + text(p.cardLink(cards[0])), 1},
		"another number":  {"document_number=PR999999", 0},
	} {
		t.Run(name, func(t *testing.T) {
			if got := len(b.get("/payments?"+tc.query).page(t).all("li", "card")); got != tc.want {
				t.Errorf("got %d runs, want %d", got, tc.want)
			}
		})
	}
	status(t, "a company that is no id", b.get("/payments?legal_entity_id=x"), http.StatusBadRequest)
	refused := b.get("/payments?page=nonsense")
	status(t, "a page token that is none", refused, http.StatusBadRequest)
	holds(t, "a page token that is none", refused.page(t).mainText(), "The payment runs could not be shown", "request.invalid_page_token")
	holds(t, "French", b.get("/payments", "Accept-Language", "fr-CA").page(t).mainText(), "Cycles de paiement", "Proposer un cycle de paiement", "119,00 EUR")
}

// TestReleaseAndTransmit: the releaser releases a run another proposed,
// its progress shown while the release runs; the released run shows its
// payments posted and its bank file, which is sent to the bank.
func TestReleaseAndTransmit(t *testing.T) {
	s, d := paymentsStack(t, "")
	run := proposed(s.as(d.proposer), d)
	b := s.as(s.f.Approver)
	p := b.get(runPath(run)).page(t)
	offers(t, "the proposed run", p, run, map[string]bool{"release": true, "reject": true, "submit": false})
	r := decideRunAs(b, run, "release", url.Values{"comment": {"Within the cash plan."}})
	if r.status != http.StatusSeeOther || r.location() != runPath(run)+"?done=releasing" {
		t.Fatalf("release: got %d to %q: %s", r.status, r.location(), r.body)
	}
	holds(t, "the run being released", b.get(r.location()).page(t).mainText(), "The release has started.", "Status Being released",
		"Show the progress", "Bank files", "The run has no bank file yet")
	holds(t, "the release's progress", text(b.get(progressPath(run), "HX-Request", "true").page(t).doc), "Releasing the run", "Waiting to start")
	if n := fixture.RunReleases(t, s.objects); n != 1 {
		t.Fatalf("releases done: got %d, want 1", n)
	}
	released := b.get(runPath(run)).page(t)
	holds(t, "the run released", released.mainText(), "Status Released", "Released or rejected by approver1", "Released or rejected on",
		"2000-PR", "Yes", "Status Written, not sent", "Format pain.001", "Send to the bank")
	if strings.Contains(released.mainText(), "Show the progress") {
		t.Error("the released run: offers its progress")
	}
	offers(t, "the released run", released, run, map[string]bool{"release": false, "reject": false})
	sent := decideRunAs(b, run, "transmit", nil)
	if sent.status != http.StatusSeeOther || sent.location() != runPath(run)+"?done=transmitting" {
		t.Fatalf("transmit: got %d to %q: %s", sent.status, sent.location(), sent.body)
	}
	holds(t, "the file queued", b.get(sent.location()).page(t).mainText(), "The bank file is queued for the bank.", "Status Queued for the bank")
	if n := fixture.RunTransmissions(t, s.objects); n != 1 {
		t.Fatalf("files sent: got %d, want 1", n)
	}
	after := b.get(runPath(run)).page(t)
	holds(t, "the file sent", after.mainText(), "Status Sent to the bank")
	if runForm(after, run, "transmit") != nil {
		t.Error("a file sent is offered its transmission again")
	}
	t.Run("a form that is not the page's", func(t *testing.T) {
		token := b.csrf()
		status(t, "no bank file", b.do(http.MethodPost, runPath(run)+"/transmit", url.Values{"csrf_token": {token}, "if_match": {`"1"`},
			"idempotency_key": {uuid.NewString()}}), http.StatusBadRequest)
		status(t, "no run id", b.do(http.MethodPost, "/payments/x/release", url.Values{"csrf_token": {token}}), http.StatusNotFound)
		status(t, "no key", b.do(http.MethodPost, runPath(run)+"/release", url.Values{"csrf_token": {token}, "if_match": {`"1"`}}), http.StatusBadRequest)
	})
}

// TestRejectRun: a rejection needs its reason, said before the API is
// called with the form kept; a stale form is refused with its text kept;
// the rejected run frees its items and offers nothing more; a resubmitted
// form replays the first answer.
func TestRejectRun(t *testing.T) {
	s, d := paymentsStack(t, "")
	run := proposed(s.as(d.proposer), d)
	b := s.as(s.f.Approver)
	none := decideRunAs(b, run, "reject", url.Values{"comment": {" "}})
	status(t, "no reason", none, http.StatusUnprocessableEntity)
	holds(t, "no reason", none.page(t).mainText(), "Enter the reason.")
	long := decideRunAs(b, run, "release", url.Values{"comment": {strings.Repeat("x", 2001)}})
	status(t, "a comment too long", long, http.StatusUnprocessableEntity)
	holds(t, "a comment too long", long.page(t).mainText(), "A comment has at most 2,000 characters.")
	stale := runForm(b.get(runPath(run)).page(t), run, "reject")
	pgtest.Exec(t, `UPDATE payments.payment_run SET version = version + 1 WHERE id = $1`, run)
	r := postForm(b, stale, runPath(run)+"/reject", url.Values{"comment": {"Kept, though refused"}})
	status(t, "stale", r, http.StatusPreconditionFailed)
	holds(t, "stale", r.page(t).mainText(), "The run changed since you opened it.", "Kept, though refused", "request.version_mismatch")
	form := runForm(b.get(runPath(run)).page(t), run, "reject")
	first := postForm(b, form, runPath(run)+"/reject", url.Values{"comment": {"Pay next week instead."}})
	if first.status != http.StatusSeeOther || first.location() != runPath(run)+"?done=rejected" {
		t.Fatalf("reject: got %d to %q", first.status, first.location())
	}
	if again := postForm(b, form, runPath(run)+"/reject", url.Values{"comment": {"Pay next week instead."}}); again.location() != first.location() {
		t.Errorf("the form sent again: got %d to %q, want the first answer", again.status, again.location())
	}
	p := b.get(first.location()).page(t)
	holds(t, "rejected", p.mainText(), "The run is rejected.", "Status Rejected", "Released or rejected by approver1")
	if len(p.all("form", "decide")) != 0 {
		t.Error("a rejected run offers a decision")
	}
	refused := postForm(b, form, runPath(run)+"/release", url.Values{"idempotency_key": {uuid.NewString()}, "if_match": {runTag(t, run)}})
	status(t, "released once rejected", refused, http.StatusConflict)
	holds(t, "released once rejected", refused.page(t).mainText(), "The run's status does not allow this any more.")
}

// TestCancelAndResume: a proposal cancelled fails its run; a release
// cancelled leaves the run being released, its progress saying so and
// offering its resumption, which releases it; an operation that has
// finished is not cancelled, and the page says why.
func TestCancelAndResume(t *testing.T) {
	s, d := paymentsStack(t, "")
	proposer := s.as(d.proposer)
	first := runIn(t, proposeAs(proposer, d, nil))
	b := s.as(s.f.Approver)
	cancel := text(b.get(progressPath(first)).page(t).doc)
	holds(t, "the proposal's progress", cancel, "Cancel the operation")
	form := b.get(progressPath(first)).page(t).form(runPath(first) + "/cancel")
	r := postForm(b, form, runPath(first)+"/cancel", nil)
	if r.status != http.StatusSeeOther || r.location() != runPath(first)+"?done=cancelled" {
		t.Fatalf("cancel: got %d to %q: %s", r.status, r.location(), r.body)
	}
	holds(t, "cancelled", b.get(r.location()).page(t).mainText(), "The operation is cancelled.", "Status Being proposed")
	waits := b.get(progressPath(first)+"?part=status", "HX-Request", "true")
	if got := waits.header.Get("HX-Redirect"); got != "" {
		t.Errorf("a cancelled proposal's run still being proposed: HX-Redirect %q, want none until the worker fails it", got)
	}
	holds(t, "a cancelled proposal waits", text(waits.page(t).doc), "Cancelled")
	fixture.RunProposals(t)
	if got := runStatus(t, first); got != "failed" {
		t.Errorf("the run of a proposal cancelled: got %s, want failed", got)
	}
	holds(t, "the cancelled proposal", b.get(runPath(first)).page(t).mainText(), "Status Failed", "Show the progress")
	if got := b.get(progressPath(first)+"?part=status", "HX-Request", "true").header.Get("HX-Redirect"); got != runPath(first) {
		t.Errorf("the failed run's status: HX-Redirect %q, want the run's page", got)
	}
	late := postForm(b, form, runPath(first)+"/cancel", url.Values{"idempotency_key": {uuid.NewString()}})
	status(t, "cancelled once finished", late, http.StatusConflict)
	holds(t, "cancelled once finished", late.page(t).mainText(), "The operation was not cancelled", "The operation has already finished",
		"Building the proposal", "Cancelled")
	run := proposed(proposer, d)
	decideRunAs(b, run, "release", nil)
	release := b.get(progressPath(run)).page(t).form(runPath(run) + "/cancel")
	if r := postForm(b, release, runPath(run)+"/cancel", nil); r.status != http.StatusSeeOther {
		t.Fatalf("cancel the release: got %d %s", r.status, r.body)
	}
	fixture.RunReleases(t, s.objects)
	if got := runStatus(t, run); got != "releasing" {
		t.Fatalf("the run of a release cancelled: got %s, want releasing", got)
	}
	section := b.get(progressPath(run), "HX-Request", "true").page(t)
	holds(t, "the cancelled release", text(section.doc), "Releasing the run", "Cancelled", "The release stopped before it was done", "Release the run")
	resume := section.form(runPath(run) + "/release")
	if r := postForm(b, resume, runPath(run)+"/release", nil); r.location() != runPath(run)+"?done=releasing" {
		t.Fatalf("resume: got %d to %q: %s", r.status, r.location(), r.body)
	}
	fixture.RunReleases(t, s.objects)
	if got := runStatus(t, run); got != "released" {
		t.Errorf("the run resumed: got %s, want released", got)
	}
	t.Run("a form that is not the page's", func(t *testing.T) {
		token := b.csrf()
		status(t, "no operation", b.do(http.MethodPost, runPath(run)+"/cancel", url.Values{"csrf_token": {token}, "idempotency_key": {uuid.NewString()}}),
			http.StatusBadRequest)
		status(t, "another run's operation", postForm(b, form, runPath(run)+"/cancel", url.Values{"idempotency_key": {uuid.NewString()}}), http.StatusNotFound)
		status(t, "no such operation", postForm(b, form, runPath(run)+"/cancel", url.Values{"idempotency_key": {uuid.NewString()},
			"operation": {uuid.NewString()}}), http.StatusNotFound)
		status(t, "no run id", b.do(http.MethodPost, "/payments/x/cancel", url.Values{"csrf_token": {token}}), http.StatusNotFound)
		status(t, "no run's progress", b.get("/payments/x/progress"), http.StatusNotFound)
		status(t, "no such run", b.get(progressPath(uuid.New())), http.StatusNotFound)
	})
}

// TestTwoPersonRelease is the release of a run above its company's limit
// (ADR-0038 as amended by T72m): the release answers that it needs a
// second approval, which the page then offers to ask for; the request
// waits in the inbox, where a second person approves it; the page shows
// the request's state, and the approver may not release, the requester
// does. A request rejected in the inbox rejects its run.
func TestTwoPersonRelease(t *testing.T) {
	s, d := paymentsStack(t, "100.00")
	proposer := s.as(d.proposer)
	run := proposed(proposer, d)
	b := s.as(s.f.Approver)
	refused := decideRunAs(b, run, "release", nil)
	status(t, "released above the limit", refused, http.StatusConflict)
	rp := refused.page(t)
	holds(t, "released above the limit", rp.mainText(), "The run is above its company's limit", "Ask for the second approval below.",
		"payments.release_approval_required", "Ask for the second approval")
	ask := runForm(rp, run, "submit")
	if ask == nil {
		t.Fatal("the refusal offers no request for the second approval")
	}
	r := postForm(b, ask, runPath(run)+"/submit", nil)
	if r.status != http.StatusSeeOther || r.location() != runPath(run)+"?done=submitted" {
		t.Fatalf("submit: got %d to %q: %s", r.status, r.location(), r.body)
	}
	waiting := b.get(r.location()).page(t)
	holds(t, "waiting", waiting.mainText(), "The second approval is asked for.", "Second approval of the release", "State of the request Waiting for approval",
		"Asked for a total of EUR 119.00", "The release waits for the second approval", "Open the approval request")
	offers(t, "waiting", waiting, run, map[string]bool{"release": false, "reject": true})
	request := approvalRequestOf(t, waiting)
	second := s.as(d.f.Approver2)
	inbox := second.get("/approvals").page(t).mainText()
	holds(t, "the second person's inbox", inbox, "Payment run release", "EUR 119.00", "approver1")
	approve := second.get(requestPath(request)).page(t).form(requestPath(request) + "/approve")
	if a := second.do(http.MethodPost, requestPath(request)+"/approve", values(approve, "Within the cash plan")); a.status != http.StatusSeeOther {
		t.Fatalf("the approval: got %d %s", a.status, a.page(t).mainText())
	}
	holds(t, "approved", b.get(runPath(run)).page(t).mainText(), "State of the request Approved", "Approved by approver2", "Approved on", "Release the run")
	approvers := second.get(runPath(run)).page(t)
	holds(t, "the approver's page", approvers.mainText(), "You approved this run's release, so another person releases it. You may still reject it.")
	offers(t, "the approver's page", approvers, run, map[string]bool{"release": false, "reject": true, "submit": false})
	if r := decideRunAs(b, run, "release", nil); r.status != http.StatusSeeOther {
		t.Fatalf("released once approved: got %d %s", r.status, r.page(t).mainText())
	}
	fixture.RunReleases(t, s.objects)
	if got := runStatus(t, run); got != "released" {
		t.Fatalf("run: got %s, want released", got)
	}
}

// TestTwoPersonReleaseRejected: the second person rejects the release's
// request in the inbox, which rejects its run (product owner
// 2026-10-25); the run's page shows both.
func TestTwoPersonReleaseRejected(t *testing.T) {
	s, d := paymentsStack(t, "100.00")
	run := proposed(s.as(d.proposer), d)
	b := s.as(s.f.Approver)
	refused := decideRunAs(b, run, "release", nil)
	postForm(b, runForm(refused.page(t), run, "submit"), runPath(run)+"/submit", nil)
	request := approvalRequestOf(t, b.get(runPath(run)).page(t))
	second := s.as(d.f.Approver2)
	reject := second.get(requestPath(request)).page(t).form(requestPath(request) + "/reject")
	if r := second.do(http.MethodPost, requestPath(request)+"/reject", values(reject, "Not this week")); r.status != http.StatusSeeOther {
		t.Fatalf("the rejection: got %d %s", r.status, r.page(t).mainText())
	}
	p := b.get(runPath(run)).page(t)
	holds(t, "rejected", p.mainText(), "Status Rejected", "State of the request Rejected", "Released or rejected by approver2")
	if len(p.all("form", "decide")) != 0 {
		t.Error("a run rejected with its request offers a decision")
	}
}

// approvalRequestOf is the release's approval request a run's page links.
func approvalRequestOf(t *testing.T, p page) uuid.UUID {
	t.Helper()
	for _, a := range p.all("a", "button") {
		if id, ok := strings.CutPrefix(attrOf(a, "href"), "/approvals/"); ok {
			return uuid.MustParse(id)
		}
	}
	t.Fatalf("no approval request linked in %s", p.mainText())
	return uuid.Nil
}

// TestRunsAsOthers: a person who may only read runs sees the list and a
// run with nothing to propose or decide; a person who may read none is
// told why, and the runs are not in their navigation.
func TestRunsAsOthers(t *testing.T) {
	s, d := paymentsStack(t, "")
	run := proposed(s.as(d.proposer), d)
	viewer := s.as(d.viewer)
	list := viewer.get("/payments")
	status(t, "the viewer's list", list, http.StatusOK)
	if strings.Contains(list.body, "Propose a payment run") {
		t.Error("the viewer is offered the proposal")
	}
	page := viewer.get(runPath(run)).page(t)
	holds(t, "the viewer's run", page.mainText(), "You may read this run, but not release or reject it.")
	if len(page.all("form", "decide")) != 0 {
		t.Error("the viewer is offered a decision")
	}
	outsider := s.as(d.outsider)
	r := outsider.get("/payments")
	status(t, "the outsider's list", r, http.StatusForbidden)
	holds(t, "the outsider's list", r.page(t).mainText(), "The payment runs could not be shown", "You do not have the permission this needs.")
	for _, a := range r.page(t).all("a", "navigation__link") {
		if attrOf(a, "href") == "/payments" {
			t.Error("the outsider's navigation offers the payment runs")
		}
	}
	for _, path := range []string{runPath(run), progressPath(run)} {
		got := outsider.get(path)
		status(t, path, got, http.StatusForbidden)
		holds(t, path, got.page(t).mainText(), "You do not have the permission this needs.")
	}
}
