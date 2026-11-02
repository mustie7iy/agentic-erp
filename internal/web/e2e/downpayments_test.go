package e2e

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

// downPayments is the stack of a configured tenant whose approver clears
// down payments (T73e4): a supplier's two paid down payments, EUR 119.00
// and 238.00, and its posted final invoice of EUR 238.00.
type downPayments struct {
	stack
	small, large ledger.Entry
	invoice      uuid.UUID
}

func newDownPayments(t *testing.T) downPayments {
	t.Helper()
	f := fixture.New(t, append([]string{"tenancy.org.read", "payables.down_payment.request"}, fixture.InvoicePermissions...)...)
	f.DownPayments(t)
	d := downPayments{small: f.PaidDownPayment(t, f.Vendor, "119.00", "AB-1"), large: f.PaidDownPayment(t, f.Vendor, "238.00", "AB-2")}
	d.invoice = f.SupplierInvoice(t, f.Vendor, "FIN-1", "238.00", "38.00", f.AccountItem(t, "Fees", "200.00"))
	f.Must(200, "POST", "/v1/supplier-invoices/"+d.invoice.String()+":post", `{"posting_date": "2026-03-10"}`, `"1"`)
	d.stack = stackFor(t, f.Tenant, f.Approver)
	return d
}

// downPaymentBox is the selector of a down payment's box.
func downPaymentBox(e ledger.Entry) string {
	return fmt.Sprintf(`input[name="down_payment"][value="%s:%s:1"]`, strconv.Itoa(int(e.FiscalYear)), e.ID)
}

// TestDownPaymentsInTheBrowser: the clearing's screen in English, French
// and both pseudo-locales at a desktop's and a phone's width, axe at zero;
// then, in French on a phone, the AP clerk opens it from the invoice,
// chooses both down payments, is told they exceed the invoice with the
// choice kept, leaves one out and clears the other, landing on the
// invoice, which says so.
func TestDownPaymentsInTheBrowser(t *testing.T) {
	browserCtx := browse(t)
	s := newDownPayments(t)
	src := axe(t)
	signIn(t, browserCtx, s.stack)
	path := invoicePath(s.invoice) + "/down-payments"
	for _, l := range []lang{{"", "en", "ltr"}, {"fr-CA", "fr-CA", "ltr"}, {"en-XA", "en-XA", "ltr"}, {"ar-XB", "ar-XB", "rtl"}} {
		for _, width := range []int64{1280, 390} {
			t.Run(fmt.Sprintf("%s %d", l.lang, width), func(t *testing.T) {
				checkPage(t, browserCtx, s.stack, src, l.header, l.lang, l.dir, width, path)
			})
		}
	}
	ctx, c := tab(t, browserCtx, "fr-CA", 390)
	submit := chromedp.CSS(`form[action="` + path + `"] button[type="submit"]`)
	if err := chromedp.Do(ctx,
		chromedp.Navigate(s.url+invoicePath(s.invoice)),
		chromedp.WaitVisible(chromedp.CSS(`a[href="`+path+`"]`)),
		chromedp.Click(chromedp.CSS(`a[href="`+path+`"]`)),
		chromedp.WaitVisible(chromedp.CSS(downPaymentBox(s.small))),
		chromedp.Click(chromedp.CSS(downPaymentBox(s.small))),
		chromedp.Click(chromedp.CSS(downPaymentBox(s.large))),
		chromedp.SetValue(chromedp.CSS("#down-payments-date"), "2026-03-20"),
		chromedp.Click(submit),
	); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, path, "dépassent le montant brut de la facture")
	audit(t, ctx, src, "fr-CA", "ltr", 390)
	shot(t, ctx, "down-payments-refused-390")
	if checked, err := chromedp.Run(ctx, chromedp.Evaluate[int](`document.querySelectorAll('input[name="down_payment"]:checked').length`)); err != nil || checked != 2 {
		t.Errorf("the choice after the refusal: got %d checked (%v), want both kept", checked, err)
	}
	if err := chromedp.Do(ctx,
		chromedp.Click(chromedp.CSS(downPaymentBox(s.small))),
		chromedp.Click(submit),
	); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, invoicePath(s.invoice)+"?done=cleared", "Les acomptes sont lettrés avec la facture.")
	audit(t, ctx, src, "fr-CA", "ltr", 390)
	for _, e := range c.errors() {
		if !strings.Contains(e, "status of 422") { // the refused clearing is the browser's to log
			t.Errorf("browser: %s", e)
		}
	}
}
