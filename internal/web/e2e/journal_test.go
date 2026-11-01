package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// journaling is the stack of a configured tenant whose accountant enters
// manual entries in company 2000 (EUR, a limit of EUR 10,000.00) and
// whose approver, holding the controller's role, approves those above it:
// cost center CC100, profit center PC10 and segment SEG1, an agent's
// proposal no person owns, a posted entry on CC100, an entry of the
// accountant's parked, and two above the limit waiting for approval.
type journaling struct {
	stack
	f                   fixture.Env
	accountant          pgtest.Actor
	proposal, posted    uuid.UUID
	parked              uuid.UUID
	withdrawn, approved uuid.UUID // waiting for approval
}

func newJournaling(t *testing.T) journaling {
	t.Helper()
	f := fixture.NewWith(t, nil, nil, append([]string{"tenancy.org.read", "partner.supplier.read"}, fixture.JournalPermissions...)...)
	f.Centers(t)
	s := journaling{f: f, accountant: f.Person(t, "accountant")}
	f.Tenant.AllCompanies(t, f.Tenant.Agent)
	agent := f
	agent.Client = f.As(f.Tenant.Agent)
	s.proposal = agent.Park(t, "2026-03-15", "Accrual the agent proposes", [2]string{"520000", "10.00"}, [2]string{"100000", "-10.00"})
	posted, _ := f.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-03-10",
		"posting_date": "2026-03-10", "currency": "EUR", "header_text": "Office supplies", "lines": [
		{"account_id": %q, "amount": {"amount": "250.00", "currency": "EUR"}},
		{"account_id": %q, "amount": {"amount": "-250.00", "currency": "EUR"}}]}`, f.DE, f.Account(t, "2000", "520000"), f.Account(t, "2000", "100000")), "")
	s.posted = uuid.MustParse(posted["id"].(string))
	accountant := f
	accountant.Client = f.As(s.accountant)
	s.parked = accountant.Park(t, "2026-03-15", "Paper", [2]string{"520000", "40.00"}, [2]string{"100000", "-40.00"})
	for _, id := range []*uuid.UUID{&s.withdrawn, &s.approved} {
		*id = accountant.Park(t, "2026-03-15", "Year-end accrual", [2]string{"520000", "12000.00"}, [2]string{"100000", "-12000.00"})
		path := "/v1/parked-journal-entries/" + id.String()
		_, tag := accountant.Must(200, "GET", path, "", "")
		accountant.Must(200, "POST", path+":post", "", tag)
	}
	s.stack = stackWith(t, f.Tenant, s.accountant, nil)
	return s
}

func parkedPage(id uuid.UUID) string { return "/journal/parked/" + id.String() }

func journalPage(id uuid.UUID) string { return "/journal/entries/" + id.String() }

// checkJournalPages checks the parked entries and the agents' proposals,
// a new entry's form, an agent's proposal to take on, an entry waiting
// for approval with its withdrawal, a parked entry's change, the journal
// and a posted entry with its reversal, in the languages at a desktop's
// and a phone's width, with every check of TestPagesInTheBrowser and axe
// at zero.
func checkJournalPages(t *testing.T, s journaling, langs []lang) {
	t.Helper()
	browserCtx := browse(t)
	src := axe(t)
	signIn(t, browserCtx, s.stack)
	paths := []string{"/journal", "/journal?created_by_kind=agent", "/journal/new?legal_entity_id=" + s.f.DE.String(), parkedPage(s.proposal),
		parkedPage(s.withdrawn), parkedPage(s.parked) + "/edit", "/journal/entries?legal_entity_id=" + s.f.DE.String(), journalPage(s.posted)}
	for _, l := range langs {
		for _, width := range []int64{1280, 390} {
			for _, path := range paths {
				t.Run(fmt.Sprintf("%s %d %s", l.lang, width, path), func(t *testing.T) {
					checkPage(t, browserCtx, s.stack, src, l.header, l.lang, l.dir, width, path)
				})
			}
		}
	}
}

// TestJournalInTheBrowser: the manual journal entry pages in English and
// French.
func TestJournalInTheBrowser(t *testing.T) {
	checkJournalPages(t, newJournaling(t), []lang{{"", "en", "ltr"}, {"fr-CA", "fr-CA", "ltr"}})
}

// TestJournalInPseudoLocalesInTheBrowser: the manual journal entry pages
// in both pseudo-locales.
func TestJournalInPseudoLocalesInTheBrowser(t *testing.T) {
	checkJournalPages(t, newJournaling(t), []lang{{"en-XA", "en-XA", "ltr"}, {"ar-XB", "ar-XB", "rtl"}})
}

// keys presses the keys as a person types them, from the focused field.
func keys(t *testing.T, ctx context.Context, typed ...string) {
	t.Helper()
	for _, k := range typed {
		if err := chromedp.Do(ctx, chromedp.KeyEvent(k)); err != nil {
			t.Fatal(err)
		}
	}
}

// focusedIs waits until the focused element has the id.
func focusedIs(t *testing.T, ctx context.Context, id string) {
	t.Helper()
	var last string
	pgtest.Eventually(t, 10*time.Second, func() bool {
		got, err := chromedp.Run(ctx, chromedp.Evaluate[string](`document.activeElement ? document.activeElement.id : ""`))
		last = got
		return err == nil && got == id
	}, "the focus did not reach %s: it is on %q", id, lastSeen{&last})

}

// TestJournalJobsInTheBrowser: the accountant's and the controller's
// jobs as they do them, each state checked by axe. At a desktop's width
// the accountant enters a ten-line entry from the keyboard alone: each
// line typed field by field with Tab, a line added by its button with
// Enter (htmx swaps the lines in, the page is not loaded again, and the
// new line's account takes the focus), the entry parked with Enter. At a
// phone's width, with the mouse, they post a parked entry, which is
// within the limit, and withdraw the approval request of one above it.
// The controller approves the other in the approvals inbox, which posts
// it; back at a desktop's width the accountant reverses a posted entry.
func TestJournalJobsInTheBrowser(t *testing.T) {
	s := newJournaling(t)
	src := axe(t)
	browserCtx := browse(t)
	signIn(t, browserCtx, s.stack)
	desk, dc := tab(t, browserCtx, "", 1280)
	if err := chromedp.Do(desk, chromedp.Navigate(s.url+"/journal/new?legal_entity_id="+s.f.DE.String()), chromedp.WaitVisible(chromedp.CSS("#entry-header"))); err != nil {
		t.Fatal(err)
	}
	audit(t, desk, src, "en", "ltr", 1280)
	if err := chromedp.Do(desk, chromedp.Evaluate[chromedp.Void](`window.loadedOnce = true`), chromedp.Focus(chromedp.CSS("#entry-header"))); err != nil {
		t.Fatal(err)
	}
	keys(t, desk, "Ten-line accrual", "\t")
	tabs := func(n int) []string { return strings.Split(strings.Repeat("\t", n), "") }
	for i := 1; i <= 10; i++ {
		focusedIs(t, desk, fmt.Sprintf("line-%d-line_account", i))
		if i < 10 {
			keys(t, desk, "520000", "\t", "100.00", "\t", "CC100")
		} else {
			keys(t, desk, "100000", "\t", "-900.00", "\t", "")
		}
		// Past the profit center, segment, partner, tax code, tax
		// jurisdiction, tax base, quantity, unit, text and the box that
		// leaves the line out: the next line, or the form's buttons.
		keys(t, desk, tabs(11)...)
		switch {
		case i == 1:
		case i < 10:
			focusedIs(t, desk, "entry-send-button")
			keys(t, desk, "\t")
			focusedIs(t, desk, "entry-post-button")
			keys(t, desk, "\t")
			focusedIs(t, desk, "entry-add-button")
			keys(t, desk, "\r")
		default:
			focusedIs(t, desk, "entry-send-button")
		}
	}
	once, err := chromedp.Run(desk, chromedp.Evaluate[bool](`window.loadedOnce === true`))
	if err != nil || !once {
		t.Errorf("the lines were added by loading the page again, not by htmx (%v)", err)
	}
	keys(t, desk, "\r")
	waitForText(t, desk, "?done=parked", "The entry is parked.")
	audit(t, desk, src, "en", "ltr", 1280)
	var lines int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.parked_entry_line l JOIN ledger.parked_entry e ON e.tenant_id = l.tenant_id AND e.id = l.parked_entry_id
		WHERE e.tenant_id = $1 AND e.header_text = 'Ten-line accrual'`, []any{s.f.Tenant.ID}, &lines)
	if lines != 10 {
		t.Errorf("the entry typed from the keyboard: got %d lines, want 10", lines)
	}
	phone, pc := tab(t, browserCtx, "", 390)
	if err := chromedp.Do(phone, chromedp.Navigate(s.url+parkedPage(s.parked)), chromedp.WaitVisible(chromedp.CSS("#parked-post-button"))); err != nil {
		t.Fatal(err)
	}
	audit(t, phone, src, "en", "ltr", 390)
	if err := chromedp.Do(phone, chromedp.Click(chromedp.CSS("#parked-post-button"))); err != nil {
		t.Fatal(err)
	}
	waitForText(t, phone, "?done=posted", "The entry is posted.")
	audit(t, phone, src, "en", "ltr", 390)
	if err := chromedp.Do(phone, chromedp.Navigate(s.url+parkedPage(s.withdrawn)), chromedp.WaitVisible(chromedp.CSS("#parked-withdraw-text"))); err != nil {
		t.Fatal(err)
	}
	audit(t, phone, src, "en", "ltr", 390)
	if err := chromedp.Do(phone,
		chromedp.SendKeys(chromedp.CSS("#parked-withdraw-text"), "Wrong period"),
		chromedp.Click(chromedp.CSS("#parked-withdraw-button")),
	); err != nil {
		t.Fatal(err)
	}
	waitForText(t, phone, "?done=withdrawn", "The approval request is withdrawn")
	audit(t, phone, src, "en", "ltr", 390)
	var request uuid.UUID
	pgtest.Scan(t, `SELECT approval_request_id FROM ledger.parked_entry WHERE id = $1`, []any{s.approved}, &request)
	controller := browse(t)
	s.signInAs(s.f.Approver)
	signIn(t, controller, s.stack)
	ctx, cc := tab(t, controller, "", 1280)
	if err := chromedp.Do(ctx, chromedp.Navigate(s.url+"/approvals/"+request.String()), chromedp.WaitVisible(chromedp.CSS("#approve-comment"))); err != nil {
		t.Fatal(err)
	}
	audit(t, ctx, src, "en", "ltr", 1280)
	if err := chromedp.Do(ctx,
		chromedp.SendKeys(chromedp.CSS("#approve-comment"), "Accrual agreed"),
		chromedp.Click(chromedp.CSS(`form[action$="/approve"] button`)),
	); err != nil {
		t.Fatal(err)
	}
	waitForText(t, ctx, "/approvals?done=approved", "You approved the request.")
	var st string
	pgtest.Scan(t, `SELECT status FROM ledger.parked_entry WHERE id = $1`, []any{s.approved}, &st)
	if st != "posted" {
		t.Errorf("the entry after the approval: got %s, want posted", st)
	}
	if err := chromedp.Do(desk, chromedp.Navigate(s.url+journalPage(s.posted)), chromedp.WaitVisible(chromedp.CSS("#entry-reverse-button"))); err != nil {
		t.Fatal(err)
	}
	audit(t, desk, src, "en", "ltr", 1280)
	if err := chromedp.Do(desk, chromedp.Click(chromedp.CSS("#entry-reverse-button"))); err != nil {
		t.Fatal(err)
	}
	waitForText(t, desk, "?done=reversed", "The reversal is posted")
	audit(t, desk, src, "en", "ltr", 1280)
	for _, e := range append(append(dc.errors(), pc.errors()...), cc.errors()...) {
		t.Errorf("browser: %s", e)
	}
}

// TestEntryChangeAfterAnotherInTheBrowser: the accountant opens a parked
// entry's change and types a reference while the entry's header text is
// changed; saved, the change is refused (412) and the
// form shows the entry as it is now with the reference typed on it, axe
// at zero; saved again, both changes are the entry's.
func TestEntryChangeAfterAnotherInTheBrowser(t *testing.T) {
	s := newJournaling(t)
	src := axe(t)
	browserCtx := browse(t)
	signIn(t, browserCtx, s.stack)
	ctx, c := tab(t, browserCtx, "", 1280)
	edit := parkedPage(s.parked) + "/edit"
	if err := chromedp.Do(ctx, chromedp.Navigate(s.url+edit), chromedp.WaitVisible(chromedp.CSS("#entry-reference"))); err != nil {
		t.Fatal(err)
	}
	// Another change meanwhile, as the API makes it: the header text and
	// the version.
	pgtest.Exec(t, `UPDATE ledger.parked_entry SET header_text = 'Checked by Sam', version = version + 1 WHERE id = $1`, s.parked)
	if err := chromedp.Do(ctx, chromedp.SendKeys(chromedp.CSS("#entry-reference"), "R-7"), chromedp.Click(chromedp.CSS("#entry-send-button"))); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, edit, "The entry changed since you opened it.")
	valueIs(t, ctx, "entry-header", "Checked by Sam")
	valueIs(t, ctx, "entry-reference", "R-7")
	audit(t, ctx, src, "en", "ltr", 1280)
	if err := chromedp.Do(ctx, chromedp.Click(chromedp.CSS("#entry-send-button"))); err != nil {
		t.Fatal(err)
	}
	waitForText(t, ctx, "?done=changed", "The entry is changed.")
	var header, reference string
	pgtest.Scan(t, `SELECT header_text, reference FROM ledger.parked_entry WHERE id = $1`, []any{s.parked}, &header, &reference)
	if header != "Checked by Sam" {
		t.Errorf("the entry sent again: header text %q, want the other change's", header)
	}
	if reference != "R-7" {
		t.Errorf("the entry sent again: reference %q, want the one typed", reference)
	}
	for _, e := range c.errors() {
		if !strings.Contains(e, "status of 412") { // the refused change is the browser's to log
			t.Errorf("browser: %s", e)
		}
	}
}
