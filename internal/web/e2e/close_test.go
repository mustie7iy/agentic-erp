package e2e

import (
	"fmt"
	"net/url"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// closing is the stack of a configured tenant whose accountant closes
// company 2000's periods and whose approver, holding the controller's
// role, decides their unlocks: 2L kept beside 0L with a posting in 2L
// alone in March, a USD supplier item a revaluation on 31 March values at
// a loss, an open GR/IR position and a bank statement not reconciled.
// With posted, January is locked with its unlock asked for and March's
// revaluation is posted.
type closing struct {
	stack
	f          fixture.Env
	accountant pgtest.Actor
	leading    uuid.UUID
	run        uuid.UUID // March's revaluation, with posted
}

func newClosing(t *testing.T, posted bool) closing {
	t.Helper()
	perms := append([]string{"tenancy.org.read", "payments.statement.read", "payments.statement.import"}, fixture.ClosePermissions...)
	f := fixture.NewWith(t, nil, fixture.Parallel, perms...)
	objects := blobstore.Dir{Root: t.TempDir()}
	f.Revalues(t, fixture.Parallel)
	s := closing{f: f, accountant: f.Person(t, "accountant"), leading: f.Ledger(t, "0L")}
	f.LedgerOnly(t, f.Ledger(t, "2L"), "2026-03-16", "25.00")
	f.GRIROpen(t)
	f.ImportStatement(t, objects, fixture.MT940("S1", "1", "1000,00", "260416", "990,00", "260417", ":61:2604170417D10,00NMSCNONREF//B1\n"))
	if posted {
		january := "/v1/fiscal-periods/" + s.period(t, 1).String()
		_, tag := f.Must(200, "GET", january, "", "")
		_, tag = f.Must(200, "POST", january+":close", "", tag)
		_, tag = f.Must(200, "POST", january+":lock", "", tag)
		f.Must(200, "POST", january+":unlock", `{"reason": "Audit adjustment"}`, tag)
		run, _ := f.Must(201, "POST", "/v1/revaluation-runs", fmt.Sprintf(`{"legal_entity_id": %q, "key_date": "2026-03-31"}`, f.DE), "")
		s.run = uuid.MustParse(run["id"].(string))
	}
	s.stack = stackWith(t, f.Tenant, s.accountant, func(cfg *app.Config) { cfg.Objects = objects })
	return s
}

// period is the id of company 2000's period of 2026 in the leading ledger.
func (s closing) period(t *testing.T, n int) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	pgtest.Scan(t, `SELECT id FROM ledger.period_status WHERE tenant_id = $1 AND legal_entity_id = $2 AND ledger_id = $3 AND fiscal_year = 2026 AND period = $4`,
		[]any{s.f.Tenant.ID, s.f.DE, s.leading, n}, &id)
	return id
}

func periodPage(id uuid.UUID) string { return "/close/periods/" + id.String() }

// checkClosePages checks the periods of company 2000, an open period with
// its checks, a locked one whose unlock waits, the checks' page, the
// revaluation and a posted run, in the languages at a desktop's and a
// phone's width, with every check of TestPagesInTheBrowser and axe at
// zero.
func checkClosePages(t *testing.T, s closing, langs []lang) {
	t.Helper()
	browserCtx := browse(t)
	src := axe(t)
	signIn(t, browserCtx, s.stack)
	march := s.period(t, 3)
	checks := "/close/checks?" + url.Values{"legal_entity_id": {s.f.DE.String()}, "ledger_id": {s.leading.String()}, "fiscal_year": {"2026"},
		"period": {"3"}, "period_id": {march.String()}}.Encode()
	paths := []string{"/close?legal_entity_id=" + s.f.DE.String(), periodPage(march), periodPage(s.period(t, 1)), checks,
		"/close/revaluation?legal_entity_id=" + s.f.DE.String(), "/close/revaluation/" + s.run.String()}
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

// TestCloseInTheBrowser: the period close pages in English and French.
func TestCloseInTheBrowser(t *testing.T) {
	checkClosePages(t, newClosing(t, true), []lang{{"", "en", "ltr"}, {"fr-CA", "fr-CA", "ltr"}})
}

// TestCloseInPseudoLocalesInTheBrowser: the period close pages in both
// pseudo-locales.
func TestCloseInPseudoLocalesInTheBrowser(t *testing.T) {
	checkClosePages(t, newClosing(t, true), []lang{{"en-XA", "en-XA", "ltr"}, {"ar-XB", "ar-XB", "rtl"}})
}

// TestCloseJobsInTheBrowser: the accountant's and the controller's jobs
// as they do them, each state checked by axe. At a desktop's width the
// accountant previews March's revaluation from its page and posts it. At
// a phone's, they look at March's pre-close checks, which htmx loads into
// its page, close it, lock it and ask for its unlock, which waits for
// the controller. The controller approves it in the approvals inbox,
// which leaves March closed.
func TestCloseJobsInTheBrowser(t *testing.T) {
	s := newClosing(t, false)
	src := axe(t)
	browserCtx := browse(t)
	signIn(t, browserCtx, s.stack)
	march := periodPage(s.period(t, 3))
	desk, dc := tab(t, browserCtx, "", 1280)
	if err := chromedp.Do(desk,
		chromedp.Navigate(s.url+march),
		chromedp.WaitVisible(chromedp.CSS(`a.button[href^="/close/revaluation?"]`)),
		chromedp.Click(chromedp.CSS(`a.button[href^="/close/revaluation?"]`)),
		chromedp.WaitVisible(chromedp.CSS("#preview-button")),
		chromedp.Click(chromedp.CSS("#preview-button")),
	); err != nil {
		t.Fatal(err)
	}
	waitForText(t, desk, "/close/revaluation/preview", "What the revaluation would value")
	audit(t, desk, src, "en", "ltr", 1280)
	if err := chromedp.Do(desk, chromedp.Click(chromedp.CSS("#revaluation-post-button"))); err != nil {
		t.Fatal(err)
	}
	waitForText(t, desk, "?done=posted", "The revaluation is posted.")
	audit(t, desk, src, "en", "ltr", 1280)
	phone, pc := tab(t, browserCtx, "", 390)
	if err := chromedp.Do(phone, chromedp.Navigate(s.url+march)); err != nil {
		t.Fatal(err)
	}
	waitForText(t, phone, march, "GR/IR positions still open")
	audit(t, phone, src, "en", "ltr", 390)
	if err := chromedp.Do(phone, chromedp.Click(chromedp.CSS("#period-close-button"))); err != nil {
		t.Fatal(err)
	}
	waitForText(t, phone, "?done=closed", "The period is closed.")
	audit(t, phone, src, "en", "ltr", 390)
	if err := chromedp.Do(phone, chromedp.Click(chromedp.CSS("#period-lock-button"))); err != nil {
		t.Fatal(err)
	}
	waitForText(t, phone, "?done=locked", "Ask for the unlock")
	if err := chromedp.Do(phone,
		chromedp.SendKeys(chromedp.CSS("#period-unlock-text"), "Audit adjustment"),
		chromedp.Click(chromedp.CSS("#period-unlock-button")),
	); err != nil {
		t.Fatal(err)
	}
	waitForText(t, phone, "?done=unlock", "Whoever asked for it may not approve it.")
	audit(t, phone, src, "en", "ltr", 390)
	var request uuid.UUID
	pgtest.Scan(t, `SELECT unlock_request_id FROM ledger.period_status WHERE id = $1`, []any{s.period(t, 3)}, &request)
	controller := browse(t)
	s.signInAs(s.f.Approver)
	signIn(t, controller, s.stack)
	ctx, cc := tab(t, controller, "", 1280)
	if err := chromedp.Do(ctx,
		chromedp.Navigate(s.url+"/approvals/"+request.String()),
		chromedp.WaitVisible(chromedp.CSS("#approve-comment")),
	); err != nil {
		t.Fatal(err)
	}
	audit(t, ctx, src, "en", "ltr", 1280)
	if err := chromedp.Do(ctx,
		chromedp.SendKeys(chromedp.CSS("#approve-comment"), "Adjustment agreed"),
		chromedp.Click(chromedp.CSS(`form[action$="/approve"] button`)),
	); err != nil {
		t.Fatal(err)
	}
	waitForText(t, ctx, "/approvals?done=approved", "You approved the request.")
	var status string
	pgtest.Scan(t, `SELECT status FROM ledger.period_status WHERE id = $1`, []any{s.period(t, 3)}, &status)
	if status != "closed" {
		t.Errorf("March after the approval: got %s, want closed", status)
	}
	for _, e := range append(append(dc.errors(), pc.errors()...), cc.errors()...) {
		t.Errorf("browser: %s", e)
	}
}
