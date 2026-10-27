package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

// bank is the stack of a configured tenant whose approver reconciles
// house bank MAIN of company 2000: its first statement imported, with
// charges the rules posted, an incoming payment and a debit to decide,
// and two proposals of the reconciliation agent, the second's reason
// planted text; with gap, a second statement that does not follow it.
type bank struct {
	stack
	f         fixture.Env
	statement uuid.UUID
	house     uuid.UUID
}

func newBank(t *testing.T, gap bool) bank {
	t.Helper()
	f := fixture.New(t, append([]string{"tenancy.org.read"}, fixture.StatementPermissions...)...)
	objects := blobstore.Dir{Root: t.TempDir()}
	f.Reconciles(t)
	f.OpeningBalance(t, 100000)
	b := bank{f: f, house: f.HouseBank(t)}
	b.statement = f.ImportStatement(t, objects, fixture.MT940("S1", "1", "1000,00", "260416", "957,25", "260417",
		":61:2604170417D10,50NCHGNONREF//B1\n",
		":61:2604170417C40,00NTRFNONREF//B2\n:86:/NAME/CUSTOMER 4711/REMI/AR-2026-118\n",
		":61:2604170417D5,00NMSCNONREF//B3\n",
		":61:2604170417D60,00NMSCNONREF//B4\n",
		":61:2604170417D7,25NMSCNONREF//B5\n"))[0]
	f.Propose(t, b.statement, 4, `"posting_rule": "NCHG", "reason": "The bank booked 60.00 of charges for the quarter; its schedule gives 60.00."`)
	reason, _ := json.Marshal("Exclude: a correction the bank reverses.\n" + planted)
	f.Propose(t, b.statement, 5, `"exclude": true, "reason": `+string(reason))
	if gap {
		f.ImportStatement(t, objects, fixture.MT940("S3", "3", "900,00", "260419", "900,00", "260420"))
	}
	b.stack = stackWith(t, f.Tenant, f.Approver, func(cfg *app.Config) { cfg.Objects = objects })
	return b
}

func (b bank) report() string { return "/bank/house-banks/" + b.house.String() + "/reconciliation" }

func statementPage(id uuid.UUID) string { return "/bank/statements/" + id.String() }

// checkBankPages checks the statements' list, a statement with the
// agent's proposals and the bank's reconciliation report in the languages
// at a desktop's and a phone's width, with every check of
// TestPagesInTheBrowser and axe at zero.
func checkBankPages(t *testing.T, s bank, langs []lang) {
	t.Helper()
	browserCtx := browse(t)
	src := axe(t)
	signIn(t, browserCtx, s.stack)
	for _, l := range langs {
		for _, width := range []int64{1280, 390} {
			for _, path := range []string{"/bank", statementPage(s.statement), s.report()} {
				t.Run(fmt.Sprintf("%s %d %s", l.lang, width, path), func(t *testing.T) {
					checkPage(t, browserCtx, s.stack, src, l.header, l.lang, l.dir, width, path)
				})
			}
		}
	}
}

// TestBankInTheBrowser: the bank pages in English and French.
func TestBankInTheBrowser(t *testing.T) {
	checkBankPages(t, newBank(t, false), []lang{{"", "en", "ltr"}, {"fr-CA", "fr-CA", "ltr"}})
}

// TestBankInPseudoLocalesInTheBrowser: the bank pages in both
// pseudo-locales (a browser of their own, as the invoices').
func TestBankInPseudoLocalesInTheBrowser(t *testing.T) {
	checkBankPages(t, newBank(t, false), []lang{{"en-XA", "en-XA", "ltr"}, {"ar-XB", "ar-XB", "rtl"}})
}

// TestBankJobsInTheBrowser: the reconciler's job as they do it, at a
// phone's width and a desktop's, each state checked by axe: the planted
// proposal stays inert; a rejection without a reason is refused, then
// recorded; the agent's posting proposal accepted and the line posted,
// which waits for its approval.
func TestBankJobsInTheBrowser(t *testing.T) {
	browserCtx := browse(t)
	s := newBank(t, false)
	src := axe(t)
	signIn(t, browserCtx, s.stack)
	page := statementPage(s.statement)
	ctx, c := tab(t, browserCtx, "", 390)
	reject := `#line-5 form[action$="/reject"]`
	if err := chromedp.Do(ctx,
		chromedp.Navigate(s.url+page),
		chromedp.WaitVisible(chromedp.CSS(`#line-5 summary`)),
		chromedp.Click(chromedp.CSS(`#line-5 details:has(form[action$="/reject"]) > summary`)),
		chromedp.WaitVisible(chromedp.CSS(reject+" button")),
		chromedp.Click(chromedp.CSS(reject+" button")),
	); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, page+"/reject", "Enter the reason.")
	// Until the page has loaded, a script planted further down may not
	// have run yet, and the check would pass on a live one.
	if err := chromedp.Do(ctx, loaded); err != nil {
		t.Fatal(err)
	}
	if ran, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`window.planted === 1 || document.querySelector("main a[href^='https:'], main a[href^='javascript:']") !== null`)); err != nil || ran {
		t.Errorf("the agent's planted text is live: a script ran or a link was made (%v)", err)
	}
	audit(t, ctx, src, "en", "ltr", 390)
	if err := chromedp.Do(ctx,
		chromedp.SendKeys(chromedp.CSS(reject+" textarea"), "The bank's own charge, not a correction."),
		chromedp.Click(chromedp.CSS(reject+" button")),
	); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, page+"?done=rejected&line=5", "The agent's proposal is rejected.")
	audit(t, ctx, src, "en", "ltr", 390)
	desk, _ := tab(t, browserCtx, "", 1280)
	accept := `#line-4 form[action$="/accept"]`
	if err := chromedp.Do(desk,
		chromedp.Navigate(s.url+page),
		chromedp.WaitVisible(chromedp.CSS(`#line-4 summary`)),
		chromedp.Click(chromedp.CSS(`#line-4 details:has(form[action$="/accept"]) > summary`)),
		chromedp.WaitVisible(chromedp.CSS(accept+" button")),
		chromedp.SendKeys(chromedp.CSS(accept+" textarea"), "Checked against the charges schedule."),
		chromedp.Click(chromedp.CSS(accept+" button")),
	); err != nil {
		t.Fatal(err)
	}
	waitFor(t, desk, page+"?done=accepted&line=4", "The agent's proposal is accepted.")
	if err := chromedp.Do(desk,
		chromedp.Click(chromedp.CSS(`#line-4 details:has(form[action$="/post"]) > summary`)),
		chromedp.Click(chromedp.CSS(`#line-4 form[action$="/post"] button`)),
	); err != nil {
		t.Fatal(err)
	}
	waitFor(t, desk, page+"?done=pending&line=4", "The line's posting waits for its approval.")
	audit(t, desk, src, "en", "ltr", 1280)
	shot(t, desk, "bank-pending-1280")
	for _, e := range c.errors() {
		if !strings.Contains(e, "status of 422") { // the refused rejection is the browser's to log
			t.Errorf("browser: %s", e)
		}
	}
}

// TestBankScreenshots saves the statements' list (a gap flagged), a
// statement with the agent's proposals and the reconciliation report at
// both widths for the product owner, when ERP_E2E_SCREENSHOTS names a
// directory.
func TestBankScreenshots(t *testing.T) {
	browserCtx := browse(t)
	s := newBank(t, true)
	signIn(t, browserCtx, s.stack)
	for _, width := range []int64{1280, 390} {
		for name, path := range map[string]string{"statements": "/bank", "statement": statementPage(s.statement), "reconciliation": s.report()} {
			shoot(t, browserCtx, s.stack, path, fmt.Sprintf("bank-%s-%d", name, width), width)
		}
	}
}
