package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// runs is the stack of a configured tenant whose approver releases the
// payment runs of company 2000: one released with its bank file, one
// proposed, paying a supplier and leaving another out, and one being
// proposed; a person proposes them. With limit, company 2000 releases a
// run above it only after a second person's approval (ADR-0038, T72m),
// and no run is proposed yet.
type runs struct {
	stack
	f                          fixture.Env
	proposer                   pgtest.Actor
	released, proposed, queued uuid.UUID
}

func newRuns(t *testing.T, limit string) runs {
	t.Helper()
	extend := func(*config.Config) {}
	if limit != "" {
		extend = func(c *config.Config) { c.Company("2000").ReleaseDualLimit = limit }
	}
	f := fixture.NewWith(t, nil, extend, append([]string{"tenancy.org.read", "payments.statement.read"}, fixture.PaymentPermissions...)...)
	objects := blobstore.Dir{Root: t.TempDir()}
	configure := func(cfg *app.Config) { cfg.Objects, cfg.Connector = objects, fixture.Accepting{} }
	// The workers take every tenant's work in the package database: none
	// of this test's is left for a later test's.
	t.Cleanup(func() {
		fixture.RunProposals(t)
		fixture.RunReleases(t, objects)
	})
	f.BankAccount(t, f.Supplier, "DE89370400440532013000", "Supplier GmbH")
	f.Payable(t, f.Supplier, "INV-1", "100.00", "19.00", "119.00")
	f.Payable(t, f.Vendor, "INV-2", "50.00", "9.50", "59.50")
	s := runs{f: f, proposer: f.Person(t, "proposer")}
	if limit != "" {
		// The proposer proposes in the browser: nothing is proposed before.
		s.stack = stackWith(t, f.Tenant, f.Approver, configure)
		return s
	}
	{
		s.released = proposeRun(t, f, s.proposer)
		run := f.As(f.Approver)
		_, tag := run.Must(200, "GET", "/v1/payment-runs/"+s.released.String(), "", "")
		run.Must(202, "POST", "/v1/payment-runs/"+s.released.String()+":release", "", tag)
		fixture.RunReleases(t, objects)
		f.Payable(t, f.Supplier, "INV-3", "200.00", "38.00", "238.00")
	}
	s.proposed = proposeRun(t, f, s.proposer)
	s.queued = uuid.MustParse(strings.TrimPrefix(propose(t, f, s.proposer)["resource"].(string), "/v1/payment-runs/"))
	s.stack = stackWith(t, f.Tenant, f.Approver, configure)
	return s
}

// propose asks, as a, for a run of company 2000 through house bank MAIN
// on 15 April, and returns its operation.
func propose(t *testing.T, f fixture.Env, a pgtest.Actor) map[string]any {
	t.Helper()
	op, _ := f.As(a).Must(202, "POST", "/v1/payment-runs:propose", fmt.Sprintf(`{"legal_entity_id": %q, "house_bank_id": %q,
		"run_date": "2026-04-15", "due_by": "2026-04-15"}`, f.DE, f.HouseBank(t)), "")
	return op
}

// proposeRun proposes a run as a and has the worker build it.
func proposeRun(t *testing.T, f fixture.Env, a pgtest.Actor) uuid.UUID {
	t.Helper()
	op := propose(t, f, a)
	fixture.RunProposals(t)
	return uuid.MustParse(strings.TrimPrefix(op["resource"].(string), "/v1/payment-runs/"))
}

func runPage(id uuid.UUID) string { return "/payments/" + id.String() }

// checkRunPages checks the runs' list, a released run with its bank
// file, a proposed run and one being proposed (its progress polled), and
// a run's progress page, in the languages at a desktop's and a phone's
// width, with every check of TestPagesInTheBrowser and axe at zero.
func checkRunPages(t *testing.T, s runs, langs []lang) {
	t.Helper()
	browserCtx := browse(t)
	src := axe(t)
	signIn(t, browserCtx, s.stack)
	for _, l := range langs {
		for _, width := range []int64{1280, 390} {
			for _, path := range []string{"/payments", runPage(s.released), runPage(s.proposed), runPage(s.queued), runPage(s.queued) + "/progress"} {
				t.Run(fmt.Sprintf("%s %d %s", l.lang, width, path), func(t *testing.T) {
					checkPage(t, browserCtx, s.stack, src, l.header, l.lang, l.dir, width, path)
				})
			}
		}
	}
}

// TestPaymentsInTheBrowser: the payment run pages in English and French.
func TestPaymentsInTheBrowser(t *testing.T) {
	checkRunPages(t, newRuns(t, ""), []lang{{"", "en", "ltr"}, {"fr-CA", "fr-CA", "ltr"}})
}

// TestPaymentsInPseudoLocalesInTheBrowser: the payment run pages in both
// pseudo-locales.
func TestPaymentsInPseudoLocalesInTheBrowser(t *testing.T) {
	checkRunPages(t, newRuns(t, ""), []lang{{"en-XA", "en-XA", "ltr"}, {"ar-XB", "ar-XB", "rtl"}})
}

// waitForText waits until the tab's address holds part and it shows text.
func waitForText(t *testing.T, ctx context.Context, part, text string) {
	t.Helper()
	var last string
	pgtest.Eventually(t, 20*time.Second, func() bool {
		got, err := chromedp.Run(ctx, chromedp.Evaluate[string](`location.pathname + location.search + " " + document.body.innerText`))
		last = got
		return err == nil && strings.Contains(strings.SplitN(got, " ", 2)[0], part) && strings.Contains(got, text)
	}, "the browser did not reach %s with %q: %s", part, text, lastSeen{&last})
}

// TestPaymentJobsInTheBrowser: the proposer's and the releaser's jobs as
// they do them, each state checked by axe. At a phone's width the
// proposer proposes a run and watches its progress, which htmx asks for
// again until the proposal is built and the page shows the run, with no
// decision for its proposer. At a desktop's the releaser releases it,
// above its company's limit: the release is refused, said by its code,
// and the second approval asked for, which then waits in the inbox.
func TestPaymentJobsInTheBrowser(t *testing.T) {
	s := newRuns(t, "100.00")
	src := axe(t)
	browserCtx := browse(t)
	s.signInAs(s.proposer)
	signIn(t, browserCtx, s.stack)
	ctx, c := tab(t, browserCtx, "", 390)
	if err := chromedp.Do(ctx,
		chromedp.Navigate(s.url+"/payments"),
		chromedp.WaitVisible(chromedp.CSS("#propose-company")),
		chromedp.SetValue(chromedp.CSS("#propose-company"), s.f.DE.String()),
		chromedp.SetValue(chromedp.CSS("#propose-bank"), s.f.HouseBank(t).String()),
		chromedp.SetValue(chromedp.CSS("#propose-rundate"), "2026-04-16"),
		chromedp.SetValue(chromedp.CSS("#propose-dueby"), "2026-04-16"),
		chromedp.Click(chromedp.CSS(`form[action="/payments/propose"] button`)),
	); err != nil {
		t.Fatal(err)
	}
	waitForText(t, ctx, "?done=proposing", "Waiting to start")
	audit(t, ctx, src, "en", "ltr", 390)
	fixture.RunProposals(t)
	waitForText(t, ctx, "/payments/", "You proposed this run, so another person releases or rejects it.")
	audit(t, ctx, src, "en", "ltr", 390)
	run, err := chromedp.Run(ctx, chromedp.Evaluate[string](`location.pathname`))
	if err != nil {
		t.Fatal(err)
	}
	releaser := browse(t)
	s.signInAs(s.f.Approver)
	signIn(t, releaser, s.stack)
	desk, dc := tab(t, releaser, "", 1280)
	if err := chromedp.Do(desk,
		chromedp.Navigate(s.url+run),
		chromedp.WaitVisible(chromedp.CSS("#run-release-button")),
		chromedp.Click(chromedp.CSS("#run-release-button")),
	); err != nil {
		t.Fatal(err)
	}
	waitForText(t, desk, run+"/release", "Ask for the second approval below.")
	audit(t, desk, src, "en", "ltr", 1280)
	// From the keyboard, as the audit left the focus on the skip link.
	if err := chromedp.Do(desk, chromedp.Focus(chromedp.CSS("#run-submit-button")), chromedp.KeyEvent("\r")); err != nil {
		t.Fatal(err)
	}
	waitForText(t, desk, "?done=submitted", "Waiting for approval")
	audit(t, desk, src, "en", "ltr", 1280)
	shot(t, desk, "payments-approval-1280")
	for _, e := range append(c.errors(), dc.errors()...) {
		if !strings.Contains(e, "status of 409") { // the refused release is the browser's to log
			t.Errorf("browser: %s", e)
		}
	}
}

// TestPaymentScreenshots saves the runs' list, a released run and a run
// being proposed at both widths for the product owner, when
// ERP_E2E_SCREENSHOTS names a directory.
func TestPaymentScreenshots(t *testing.T) {
	browserCtx := browse(t)
	s := newRuns(t, "")
	signIn(t, browserCtx, s.stack)
	for _, width := range []int64{1280, 390} {
		for name, path := range map[string]string{"runs": "/payments", "released": runPage(s.released), "proposing": runPage(s.queued)} {
			shoot(t, browserCtx, s.stack, path, fmt.Sprintf("payments-%s-%d", name, width), width)
		}
	}
}
