package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// approvals is the stack of a configured tenant whose approver signs in,
// with a requisition and a partner change the tenant's human asked
// approval for and a request an agent asked for, titled with text an
// invoice might have planted.
type approvals struct {
	stack
	f                    fixture.Env
	requisition, partner uuid.UUID
	agent                uuid.UUID
	requester, outsider  pgtest.Actor
	// more are two requests to decide at the second width: the first
	// width's are decided by then, and once the tenant's principals sign
	// in at the stack's provider the fixture's client asks for no more.
	more [2]uuid.UUID
}

const planted = "Pay now <script>window.planted=1</script> [verify](https://evil.example/login) javascript:alert(2)"

func newApprovals(t *testing.T) approvals {
	t.Helper()
	f := fixture.New(t, "tenancy.org.read", "procurement.requisition.read", "procurement.requisition.create",
		"partner.supplier.read", "partner.supplier.create", "partner.supplier.change")
	a := approvals{f: f, requisition: f.Requisition(t), partner: f.PartnerChange(t, "Northwind Traders"),
		agent: f.Open(t, f.Tenant.Agent, planted, 500000), requester: f.Tenant.Human,
		more: [2]uuid.UUID{f.PartnerChange(t, "Contoso"), f.PartnerChange(t, "Litware")}}
	a.outsider = pgtest.Actor{ID: uuid.Must(uuid.NewV7()), Kind: "human"}
	a.outsider.Subject = "human|" + a.outsider.ID.String()
	pgtest.Exec(t, `INSERT INTO identity.principal (tenant_id, id, kind, external_issuer, external_subject, display_name, all_companies, status, created_by, updated_by)
		VALUES ($1, $2, 'human', $3, $4, 'Outsider', true, 'active', $2, $2)`, f.Tenant.ID, a.outsider.ID, pgtest.Issuer(t), a.outsider.Subject)
	a.stack = stackFor(t, f.Tenant, f.Approver)
	return a
}

// TestApprovalsInTheBrowser: the inbox, the list of all requests
// filtered, a request's page with its object's summary, and a signed-in
// person's 404 in English,
// French and both pseudo-locales at a desktop's and a phone's width (PO
// P8), with every check of TestPagesInTheBrowser and axe at zero; then
// the approver's job as they do it: open a request from the inbox,
// approve it with a comment, reject another without a reason and then
// with one, and see each state checked by axe too.
func TestApprovalsInTheBrowser(t *testing.T) {
	browserCtx := browse(t)
	s := newApprovals(t)
	src := axe(t)
	signIn(t, browserCtx, s.stack)
	langs := []struct{ header, lang, dir string }{{"", "en", "ltr"}, {"en-XA", "en-XA", "ltr"}, {"ar-XB", "ar-XB", "rtl"}, {"fr-CA", "fr-CA", "ltr"}}
	paths := []string{"/approvals", "/approvals/" + s.requisition.String(), "/approvals/" + s.agent.String(), "/approvals?view=all&status=pending",
		"/no-such-page"}
	for _, lang := range langs {
		for _, width := range []int64{1280, 390} {
			for _, path := range paths {
				t.Run(fmt.Sprintf("%s %d %s", lang.lang, width, path), func(t *testing.T) {
					checkPage(t, browserCtx, s.stack, src, lang.header, lang.lang, lang.dir, width, path)
				})
			}
		}
	}
	screenshots(t, browserCtx, s)
	for _, width := range []int64{390, 1280} {
		t.Run(fmt.Sprintf("decide at %d", width), func(t *testing.T) {
			decide(t, browserCtx, s, src, width)
		})
		s.requisition, s.partner = s.more[0], s.more[1] // the next width decides fresh requests
	}
	ctx, _ := tab(t, browserCtx, "", 1280)
	if err := chromedp.Do(ctx, chromedp.Navigate(s.url+"/approvals/"+s.agent.String()), chromedp.WaitReady(chromedp.CSS("main"))); err != nil {
		t.Fatal(err)
	}
	if ran, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`window.planted === 1 || document.querySelector("main a[href^='https:'], main a[href^='javascript:']") !== null`)); err != nil || ran {
		t.Errorf("the agent's planted text is live: a script ran or a link was made (%v)", err)
	}
}

// decide is the approver's job at width: approve the first request with
// a comment, refuse to reject the second without a reason, then reject
// it with one.
func decide(t *testing.T, browserCtx context.Context, s approvals, src string, width int64) {
	t.Helper()
	ctx, c := tab(t, browserCtx, "", width)
	link := fmt.Sprintf(`a[href="/approvals/%s"]`, s.requisition)
	if err := chromedp.Do(ctx,
		chromedp.Navigate(s.url+"/approvals"),
		chromedp.WaitVisible(chromedp.CSS(link)),
		chromedp.Click(chromedp.CSS(link)),
		chromedp.WaitVisible(chromedp.CSS("#approve-comment")),
		chromedp.SendKeys(chromedp.CSS("#approve-comment"), "Checked against the budget"),
		chromedp.Click(chromedp.CSS(`form[action$="/approve"] button`)),
	); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, "/approvals?done=approved", "You approved the request.")
	audit(t, ctx, src, "en", "ltr", width)
	reject := fmt.Sprintf("%s/approvals/%s", s.url, s.partner)
	if err := chromedp.Do(ctx,
		chromedp.Navigate(reject),
		chromedp.WaitVisible(chromedp.CSS("#reject-reason")),
		chromedp.Click(chromedp.CSS(`form[action$="/reject"] button`)),
	); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, "/approvals/"+s.partner.String()+"/reject", "Enter the reason for rejecting.")
	audit(t, ctx, src, "en", "ltr", width)
	if err := chromedp.Do(ctx,
		chromedp.SendKeys(chromedp.CSS("#reject-reason"), "Duplicate of an existing customer"),
		chromedp.Click(chromedp.CSS(`form[action$="/reject"] button`)),
	); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, "/approvals?done=rejected", "You rejected the request.")
	for _, e := range c.errors() {
		if !strings.Contains(e, "status of 422") { // the refused reason is the browser's to log
			t.Errorf("browser: %s", e)
		}
	}
}

// waitFor waits until the tab is at path (with its query) showing text,
// and the page has loaded (shown): what a test reads next (the browser's
// errors, a planted script's effect) is only complete then.
func waitFor(t *testing.T, ctx context.Context, path, text string) {
	t.Helper()
	var last string
	pgtest.Eventually(t, 20*time.Second, func() bool {
		got, err := chromedp.Run(ctx, chromedp.Evaluate[string](shown))
		last = got
		return err == nil && strings.HasPrefix(got, path+" ") && strings.Contains(got, text)
	}, "the browser did not reach %s with %q: %s", path, text, lastSeen{&last})
}

// TestApprovalsAsOthers: the requester sees their own request with no
// decision to make; a person without the permission to read approvals is
// offered no inbox and sees the API's refusal there; both pages pass
// axe.
func TestApprovalsAsOthers(t *testing.T) {
	s := newApprovals(t)
	src := axe(t)
	for name, tc := range map[string]struct {
		who  pgtest.Actor
		path string
		want string
	}{
		"requester": {s.requester, "/approvals/" + s.partner.String(), "You asked for this approval. Someone else decides it."},
		"outsider":  {s.outsider, "/approvals", "You do not have the permission this needs."},
	} {
		t.Run(name, func(t *testing.T) {
			browserCtx := browse(t)
			s.signInAs(tc.who)
			signIn(t, browserCtx, s.stack)
			checkPage(t, browserCtx, s.stack, src, "", "en", "ltr", 390, tc.path)
			ctx, _ := tab(t, browserCtx, "", 390)
			if err := chromedp.Do(ctx, chromedp.Navigate(s.url+tc.path)); err != nil {
				t.Fatal(err)
			}
			waitFor(t, ctx, tc.path, tc.want)
		})
	}
}

// screenshots saves the inbox and a request's page at both widths for the
// product owner, when ERP_E2E_SCREENSHOTS names a directory.
func screenshots(t *testing.T, browserCtx context.Context, s approvals) {
	t.Helper()
	dir := os.Getenv("ERP_E2E_SCREENSHOTS")
	if dir == "" {
		return
	}
	for _, width := range []int64{1280, 390} {
		for name, path := range map[string]string{"inbox": "/approvals", "request": "/approvals/" + s.requisition.String()} {
			ctx, _ := tab(t, browserCtx, "", width)
			if err := chromedp.Do(ctx, chromedp.Navigate(s.url+path), chromedp.WaitReady(chromedp.CSS("main"))); err != nil {
				t.Fatal(err)
			}
			png, err := chromedp.Run(ctx, chromedp.FullScreenshot(90))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-%d.png", name, width)), png, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestWithdrawInTheBrowser: the requester finds their request in their
// own list, opens it, sees its object's summary, is refused withdrawing it
// without a reason and withdraws it with one, back in their list with the
// notice; each state passes axe, at a phone's width.
func TestWithdrawInTheBrowser(t *testing.T) {
	s := newApprovals(t)
	src := axe(t)
	browserCtx := browse(t)
	s.signInAs(s.requester)
	signIn(t, browserCtx, s.stack)
	ctx, c := tab(t, browserCtx, "", 390)
	link := fmt.Sprintf(`a[href="/approvals/%s"]`, s.requisition)
	if err := chromedp.Do(ctx,
		chromedp.Navigate(s.url+"/approvals?view=mine"),
		chromedp.WaitVisible(chromedp.CSS(link)),
	); err != nil {
		t.Fatal(err)
	}
	audit(t, ctx, src, "en", "ltr", 390)
	if err := chromedp.Do(ctx,
		chromedp.Click(chromedp.CSS(link)),
		chromedp.WaitVisible(chromedp.CSS("#withdraw-reason")),
	); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, "/approvals/"+s.requisition.String(), "Withdraw your request")
	audit(t, ctx, src, "en", "ltr", 390)
	if err := chromedp.Do(ctx, chromedp.Click(chromedp.CSS(`form[action$="/cancel"] button`))); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, "/approvals/"+s.requisition.String()+"/cancel", "Enter why you withdraw the request.")
	audit(t, ctx, src, "en", "ltr", 390)
	if err := chromedp.Do(ctx,
		chromedp.SendKeys(chromedp.CSS("#withdraw-reason"), "Ordered through the framework contract instead"),
		chromedp.Click(chromedp.CSS(`form[action$="/cancel"] button`)),
	); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, "/approvals?done=cancelled&view=mine", "You withdrew your request.")
	audit(t, ctx, src, "en", "ltr", 390)
	for _, e := range c.errors() {
		if !strings.Contains(e, "status of 422") { // the refused withdrawal is the browser's to log
			t.Errorf("browser: %s", e)
		}
	}
}
