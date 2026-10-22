package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// invoices is the stack of a configured tenant whose approver works
// supplier invoices: a clean invoice on a received order, one held for a
// price variance with the AP exceptions agent's recommendation on it
// (its text planted), one whose tax does not agree with its lines' codes,
// and one an agent captured from a scanned document.
type invoices struct {
	stack
	f                                   fixture.Env
	clean, variance, taxWrong, captured uuid.UUID
	rec                                 uuid.UUID
	viewer                              pgtest.Actor
}

func newInvoices(t *testing.T) invoices {
	t.Helper()
	f := fixture.New(t, append([]string{"tenancy.org.read"}, fixture.InvoicePermissions...)...)
	objects := blobstore.Dir{Root: t.TempDir()}
	po, poVariance := f.Order(t, f.Supplier, "PO-1"), f.Order(t, f.Supplier, "PO-2")
	f.Receive(t, po, 1, "10", "GR-1")
	f.Receive(t, po, 2, "5", "GR-2")
	f.Receive(t, poVariance, 1, "10", "GR-3")
	a := invoices{f: f}
	a.clean = f.SupplierInvoice(t, f.Supplier, "INV-1", "157.80", "22.80", fixture.POItem(po, 1, "10", "EA", "120.00"), fixture.POItem(po, 2, "5", "KG", "15.00"))
	a.variance = f.SupplierInvoice(t, f.Supplier, "INV-X", "154.70", "24.70", fixture.POItem(poVariance, 1, "10", "EA", "130.00"))
	_, tag := f.Must(200, "GET", "/v1/supplier-invoices/"+a.variance.String(), "", "")
	f.Must(200, "POST", "/v1/supplier-invoices/"+a.variance.String()+":match", "", tag)
	a.taxWrong = f.SupplierInvoice(t, f.Vendor, "W-2", "120.00", "20.00", f.AccountItem(t, "Fees", "100.00"))
	a.captured = f.SupplierInvoice(t, f.Vendor, "C-1", "119.00", "19.00", f.AccountItem(t, "Consulting, March", "100.00"))
	doc := scan(t)
	sum := sha256.Sum256(doc)
	key := blobstore.Key(f.Tenant.ID, blobstore.KindDocuments, hex.EncodeToString(sum[:]))
	if err := objects.Put(context.Background(), key, doc, blobstore.PutOptions{ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	f.Captured(t, a.captured, sum[:], key)
	a.rec = f.Recommend(t, a.variance, "approve_with_variance", "The supplier's new price list of March gives 13.00 per ream; approve the variance.\n"+planted)
	a.viewer = viewer(t, f)
	a.stack = stackWith(t, f.Tenant, f.Approver, func(cfg *app.Config) { cfg.Objects = objects })
	return a
}

// viewer is a person who may read invoices and nothing else.
func viewer(t *testing.T, f fixture.Env) pgtest.Actor {
	t.Helper()
	a := pgtest.Actor{ID: uuid.Must(uuid.NewV7()), Kind: "human"}
	a.Subject = "human|" + a.ID.String()
	role := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO identity.principal (tenant_id, id, kind, external_issuer, external_subject, display_name, all_companies, status, created_by, updated_by)
		VALUES ($1, $2, 'human', $3, $4, 'Viewer', true, 'active', $2, $2)`, f.Tenant.ID, a.ID, pgtest.Issuer(t), a.Subject)
	pgtest.Exec(t, `INSERT INTO identity.role (tenant_id, id, code, name, status, created_by, updated_by) VALUES ($1, $2, 'viewer', 'Viewer', 'active', $3, $3)`,
		f.Tenant.ID, role, f.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, created_by) VALUES ($1, gen_random_uuid(), $2, 'payables.invoice.read', current_date - 1, $3)`,
		f.Tenant.ID, role, f.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by) VALUES ($1, gen_random_uuid(), $2, $3, current_date - 1, $2)`,
		f.Tenant.ID, a.ID, role)
	return a
}

// scan is a scanned invoice's image: a page with a letterhead and lines.
func scan(t *testing.T) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, 420, 560))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	bar := func(x0, y0, x1, y1 int, c uint8) {
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				img.SetGray(x, y, color.Gray{Y: c})
			}
		}
	}
	bar(30, 30, 200, 60, 60)
	for y := 120; y < 420; y += 30 {
		bar(30, y, 390, y+8, 180)
	}
	bar(260, 470, 390, 490, 60)
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func invoicePath(id uuid.UUID) string { return "/invoices/" + id.String() }

// lang is a language a page is checked in: the Accept-Language asked,
// and the page's language and direction.
type lang struct{ header, lang, dir string }

// checkInvoicePages checks the queue, an invoice held as an exception, an
// invoice beside its scanned document and an order's page in the
// languages at a desktop's and a phone's width, with every check of
// TestPagesInTheBrowser and axe at zero.
func checkInvoicePages(t *testing.T, browserCtx context.Context, s invoices, src string, langs []lang) {
	t.Helper()
	paths := []string{"/invoices", invoicePath(s.variance), invoicePath(s.captured), invoicePath(s.clean) + "/order"}
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

// TestInvoicesInPseudoLocalesInTheBrowser: the invoice pages in both
// pseudo-locales (a browser of their own: the four languages take longer
// than one browser's three minutes).
func TestInvoicesInPseudoLocalesInTheBrowser(t *testing.T) {
	browserCtx := browse(t)
	s := newInvoices(t)
	src := axe(t)
	signIn(t, browserCtx, s.stack)
	checkInvoicePages(t, browserCtx, s, src, []lang{{"en-XA", "en-XA", "ltr"}, {"ar-XB", "ar-XB", "rtl"}})
}

// TestInvoicesInTheBrowser: the invoice pages in English and French;
// htmx loads the order into the invoice's page; the agent's planted text
// stays inert.
func TestInvoicesInTheBrowser(t *testing.T) {
	browserCtx := browse(t)
	s := newInvoices(t)
	src := axe(t)
	signIn(t, browserCtx, s.stack)
	checkInvoicePages(t, browserCtx, s, src, []lang{{"", "en", "ltr"}, {"fr-CA", "fr-CA", "ltr"}})
	ctx, _ := tab(t, browserCtx, "", 1280)
	if err := chromedp.Do(ctx, chromedp.Navigate(s.url+invoicePath(s.variance)), chromedp.WaitVisible(chromedp.CSS("section.order table"))); err != nil {
		t.Fatalf("the order's section: %v", err)
	}
	if ran, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`window.planted === 1 || document.querySelector("main a[href^='https:'], main a[href^='javascript:']") !== null`)); err != nil || ran {
		t.Errorf("the agent's planted text is live: a script ran or a link was made (%v)", err)
	}
	audit(t, ctx, src, "en", "ltr", 1280)
}

// TestInvoiceScreenshots saves the queue, the exceptions, an invoice held
// as one and an invoice beside its document at both widths for the
// product owner, when ERP_E2E_SCREENSHOTS names a directory.
func TestInvoiceScreenshots(t *testing.T) {
	if os.Getenv("ERP_E2E_SCREENSHOTS") == "" {
		t.Skip("ERP_E2E_SCREENSHOTS names no directory")
	}
	browserCtx := browse(t)
	s := newInvoices(t)
	signIn(t, browserCtx, s.stack)
	for _, width := range []int64{1280, 390} {
		for name, path := range map[string]string{"queue": "/invoices", "exceptions": "/invoices?status=on_hold", "exception": invoicePath(s.variance),
			"captured": invoicePath(s.captured)} {
			shoot(t, browserCtx, s.stack, path, fmt.Sprintf("%s-%d", name, width), width)
		}
	}
}

// TestInvoiceJobsInTheBrowser: the AP clerk's job as they do it, each
// state checked by axe: record what they did with the agent's
// recommendation (refused without a choice, then recorded), match and
// post a clean invoice, and post one whose tax disagrees, refused with
// the reason in their language.
func TestInvoiceJobsInTheBrowser(t *testing.T) {
	browserCtx := browse(t)
	s := newInvoices(t)
	src := axe(t)
	signIn(t, browserCtx, s.stack)
	for _, width := range []int64{390, 1280} {
		t.Run(fmt.Sprintf("decide at %d", width), func(t *testing.T) {
			decideRecommendation(t, browserCtx, s, src, width)
		})
		if width == 390 { // the next width decides a fresh recommendation
			s.rec = s.f.Recommend(t, s.variance, "request_credit_note", "Ask the supplier for a credit note of 10.00.")
		}
	}
	ctx, c := tab(t, browserCtx, "", 1280)
	if err := chromedp.Do(ctx,
		chromedp.Navigate(s.url+invoicePath(s.clean)),
		chromedp.WaitVisible(chromedp.CSS(`form[action$="/match"] button`)),
		settled,
		chromedp.Click(chromedp.CSS(`form[action$="/match"] button`)),
	); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, invoicePath(s.clean)+"?done=matched", "The latest match is clean")
	if err := chromedp.Do(ctx, settled, chromedp.Click(chromedp.CSS(`form[action$="/post"] button`))); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, invoicePath(s.clean)+"?done=posted", "The invoice is posted.")
	audit(t, ctx, src, "en", "ltr", 1280)
	if err := chromedp.Do(ctx,
		chromedp.Navigate(s.url+invoicePath(s.taxWrong)),
		chromedp.WaitVisible(chromedp.CSS(`form[action$="/post"] button`)),
		chromedp.Click(chromedp.CSS(`form[action$="/post"] button`)),
	); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, invoicePath(s.taxWrong)+"/post", "The tax amount does not agree")
	audit(t, ctx, src, "en", "ltr", 1280)
	shot(t, ctx, "refused-1280")
	for _, e := range c.errors() {
		if !strings.Contains(e, "status of 422") { // the refused post is the browser's to log
			t.Errorf("browser: %s", e)
		}
	}
}

// decideRecommendation is the clerk's decision on the recommendation at
// width: refused without a choice, then recorded with one and a comment.
func decideRecommendation(t *testing.T, browserCtx context.Context, s invoices, src string, width int64) {
	t.Helper()
	ctx, c := tab(t, browserCtx, "", width)
	form := fmt.Sprintf(`form[action="%s/recommendations/%s/decide"]`, invoicePath(s.variance), s.rec)
	if err := chromedp.Do(ctx,
		chromedp.Navigate(s.url+"/invoices?status=on_hold"),
		chromedp.WaitVisible(chromedp.CSS(fmt.Sprintf(`a[href="%s"]`, invoicePath(s.variance)))),
		chromedp.Click(chromedp.CSS(fmt.Sprintf(`a[href="%s"]`, invoicePath(s.variance)))),
		chromedp.WaitVisible(chromedp.CSS(form+" button")),
		settled,
		chromedp.Click(chromedp.CSS(form+" button")),
	); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, invoicePath(s.variance)+"/recommendations/"+s.rec.String()+"/decide", "Choose what you did with the recommendation.")
	audit(t, ctx, src, "en", "ltr", width)
	shot(t, ctx, fmt.Sprintf("decision-refused-%d", width))
	if err := chromedp.Do(ctx,
		chromedp.EmulateViewport(width, 900), // as it was before the screenshot
		settled,
		// As a keyboard user chooses: focus the choice, press space.
		chromedp.Focus(chromedp.CSS(fmt.Sprintf(`#rec-%s-accepted`, s.rec))),
		chromedp.KeyEvent(" "),
		chromedp.SendKeys(chromedp.CSS(form+" textarea"), "Approved the exception's request as the agent advised."),
	); err != nil {
		t.Fatal(err)
	}
	shot(t, ctx, fmt.Sprintf("decision-form-%d", width))
	if err := chromedp.Do(ctx, chromedp.Click(chromedp.CSS(form+" button"))); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, invoicePath(s.variance)+"?done=decided", "Your decision on the recommendation is recorded.")
	audit(t, ctx, src, "en", "ltr", width)
	shot(t, ctx, fmt.Sprintf("decided-%d", width))
	for _, e := range c.errors() {
		if !strings.Contains(e, "status of 422") { // the refused decision is the browser's to log
			t.Errorf("browser: %s", e)
		}
	}
}

// TestInvoicesAsViewer: a person who may only read invoices sees the
// queue and an invoice with no action offered; both pass axe.
func TestInvoicesAsViewer(t *testing.T) {
	browserCtx := browse(t)
	s := newInvoices(t)
	src := axe(t)
	s.signInAs(s.viewer)
	signIn(t, browserCtx, s.stack)
	for _, path := range []string{"/invoices", invoicePath(s.variance)} {
		checkPage(t, browserCtx, s.stack, src, "", "en", "ltr", 390, path)
	}
	ctx, _ := tab(t, browserCtx, "", 390)
	if err := chromedp.Do(ctx, chromedp.Navigate(s.url+invoicePath(s.variance))); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, invoicePath(s.variance), "You may not change this invoice in its status.")
}

// settled waits until htmx has loaded an invoice's order section, where
// the page has one: until then the page's lower half moves, and a click
// would land on what was there before.
var settled = chromedp.Poll[chromedp.Void](`document.querySelector("div.order[hx-get]") === null`, chromedp.WithPollingTimeout(10*time.Second))

// shoot saves path at width for the product owner, when
// ERP_E2E_SCREENSHOTS names a directory.
func shoot(t *testing.T, browserCtx context.Context, s stack, path, name string, width int64) {
	t.Helper()
	if os.Getenv("ERP_E2E_SCREENSHOTS") == "" {
		return
	}
	ctx, _ := tab(t, browserCtx, "", width)
	if err := chromedp.Do(ctx, chromedp.Navigate(s.url+path), chromedp.WaitReady(chromedp.CSS("main"))); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Do(ctx, settled); err != nil {
		t.Fatal(err)
	}
	shot(t, ctx, name)
}

// shot saves what the tab shows, as name.png in ERP_E2E_SCREENSHOTS.
func shot(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	dir := os.Getenv("ERP_E2E_SCREENSHOTS")
	if dir == "" {
		return
	}
	img, err := chromedp.Run(ctx, chromedp.FullScreenshot(90))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".png"), img, 0o600); err != nil {
		t.Fatal(err)
	}
}
