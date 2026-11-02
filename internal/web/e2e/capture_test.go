package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore/blobstoretest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// Capture by upload (PO 2026-11-01): the capture's form and its progress
// in every locale at both widths, and the AP clerk capturing an invoice
// from its document on a phone, the document streamed through erp-web to
// the API's signed path and into the bucket (the S3 double).

// reads answers one extraction.
type reads struct{ e *capture.Extraction }

func (x reads) Extract(context.Context, []byte) (*capture.Extraction, error) { return x.e, nil }

// captures is the stack of a configured tenant whose approver captures
// invoices, PO-1 ordered, on an S3 double, and a capture still running.
type captures struct {
	stack
	f       fixture.Env
	running uuid.UUID
}

func newCaptures(t *testing.T) captures {
	t.Helper()
	f := fixture.New(t, append([]string{"tenancy.org.read", "platform.operation.read"}, fixture.InvoicePermissions...)...)
	f.Order(t, f.Supplier, "PO-1")
	s3 := blobstoretest.New(t)
	e := &capture.Extraction{DocumentKind: "invoice", VendorName: "Supplier", InvoiceNumber: "SCAN-7", InvoiceDate: "2026-03-10", PurchaseOrder: "PO-1",
		Currency: "EUR", Lines: []capture.Line{{Description: "Paper", Quantity: "10", Amount: "120.00", POLine: 1}}, TaxAmount: "22.80",
		GrossAmount: "142.80", Confidence: "high"}
	c := captures{f: f, running: uuid.Must(uuid.NewV7())}
	pgtest.Exec(t, `INSERT INTO platform.operation (tenant_id, id, kind, status, created_by, updated_by) VALUES ($1, $2, 'payables.capture', 'running', $3, $3)`,
		f.Tenant.ID, c.running, f.Approver.ID)
	c.stack = stackWith(t, f.Tenant, f.Approver, func(cfg *app.Config) { cfg.Objects, cfg.Extractor = s3.Store(), reads{e: e} })
	return c
}

// TestCaptureInTheBrowser: the capture's form and a capture's progress in
// English, French and both pseudo-locales at 1280 and 390 px, axe at zero.
func TestCaptureInTheBrowser(t *testing.T) {
	browserCtx := browse(t)
	s := newCaptures(t)
	src := axe(t)
	signIn(t, browserCtx, s.stack)
	for _, l := range []lang{{"", "en", "ltr"}, {"fr-CA", "fr-CA", "ltr"}, {"en-XA", "en-XA", "ltr"}, {"ar-XB", "ar-XB", "rtl"}} {
		for _, width := range []int64{1280, 390} {
			for _, path := range []string{"/invoices/capture", "/invoices/capture?operation=" + s.running.String()} {
				t.Run(fmt.Sprintf("%s %d %s", l.lang, width, path), func(t *testing.T) {
					checkPage(t, browserCtx, s.stack, src, l.header, l.lang, l.dir, width, path)
				})
			}
		}
	}
}

// TestCaptureJobInTheBrowser: on a phone, in French, the AP clerk sends
// the form without a document (refused on its field), then with the
// supplier's scan, and lands on the new invoice; a capture still running
// is polled by htmx until it names its invoice, where the browser goes.
func TestCaptureJobInTheBrowser(t *testing.T) {
	browserCtx := browse(t)
	s := newCaptures(t)
	src := axe(t)
	signIn(t, browserCtx, s.stack)
	phone, c := tab(t, browserCtx, "fr-CA", 390)
	if err := chromedp.Do(phone,
		chromedp.Navigate(s.url+"/invoices"),
		chromedp.WaitVisible(chromedp.CSS(`main a[href="/invoices/capture"]`)),
		chromedp.Click(chromedp.CSS(`main a[href="/invoices/capture"]`)),
		chromedp.WaitVisible(chromedp.CSS("#upload-file")),
		chromedp.Click(chromedp.CSS(`main form[action="/invoices/capture"] button[type=submit]`)),
	); err != nil {
		t.Fatal(err)
	}
	waitForText(t, phone, "/invoices/capture", "Choisissez le document de la facture.")
	waitForText(t, phone, "/invoices/capture", "Choisissez la société à laquelle la facture")
	audit(t, phone, src, "fr-CA", "ltr", 390)
	file := filepath.Join(t.TempDir(), "facture.pdf")
	if err := os.WriteFile(file, []byte("%PDF-1.7\n% la facture numérisée du fournisseur\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Do(phone,
		chromedp.SetValue(chromedp.CSS("#capture-company"), s.f.DE.String()),
		chromedp.SetUploadFiles(chromedp.CSS("#upload-file"), []string{file}),
		chromedp.Click(chromedp.CSS(`main form[action="/invoices/capture"] button[type=submit]`)),
	); err != nil {
		t.Fatal(err)
	}
	waitForText(t, phone, "/invoices/0", "SCAN-7")
	audit(t, phone, src, "fr-CA", "ltr", 390)
	var invoice uuid.UUID
	pgtest.Scan(t, `SELECT id FROM payables.supplier_invoice WHERE tenant_id = $1 AND supplier_reference = 'SCAN-7'`, []any{s.f.Tenant.ID}, &invoice)
	if err := chromedp.Do(phone, chromedp.Navigate(s.url+"/invoices/capture?operation="+s.running.String())); err != nil {
		t.Fatal(err)
	}
	waitForText(t, phone, "/invoices/capture", "Lecture du document de la facture")
	pgtest.Exec(t, `UPDATE platform.operation SET status = 'succeeded', resource = $3, version = version + 1 WHERE tenant_id = $1 AND id = $2`,
		s.f.Tenant.ID, s.running, "/v1/supplier-invoices/"+invoice.String())
	waitForText(t, phone, "/invoices/"+invoice.String(), "SCAN-7")
	for _, e := range c.errors() {
		// The form refused on its field is answered 422, the browser's to log.
		if strings.Contains(e, "status of 422") && strings.HasSuffix(e, s.url+"/invoices/capture") {
			continue
		}
		t.Errorf("browser: %s", e)
	}
}
