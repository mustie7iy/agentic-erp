package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// valueIs waits until the field with the id holds the value.
func valueIs(t *testing.T, ctx context.Context, id, want string) {
	t.Helper()
	var last string
	pgtest.Eventually(t, 10*time.Second, func() bool {
		got, err := chromedp.Run(ctx, chromedp.Evaluate[string](`(document.getElementById("`+id+`") || {value: ""}).value`))
		last = got
		return err == nil && got == want
	}, "%s did not become %q: it is %q", id, want, lastSeen{&last})
}

// totalsShow waits until the form's running totals say the text.
func totalsShow(t *testing.T, ctx context.Context, want string) {
	t.Helper()
	var last string
	pgtest.Eventually(t, 10*time.Second, func() bool {
		got, err := chromedp.Run(ctx, chromedp.Evaluate[string](`document.getElementById("entry-totals").innerText.replace(/\s+/g, " ")`))
		last = got
		return err == nil && strings.Contains(got, want)
	}, "the running totals did not say %q: %q", want, lastSeen{&last})
}

// refocus moves the focus to the page's start, as a page loaded has it,
// so audit's first tab stop is the skip link after a person typed on it.
func refocus(t *testing.T, ctx context.Context) {
	t.Helper()
	if err := chromedp.Do(ctx, chromedp.Evaluate[chromedp.Void](`(() => { const b = document.body; b.tabIndex = -1; b.focus(); b.removeAttribute("tabindex") })()`)); err != nil {
		t.Fatal(err)
	}
}

// choose chooses the option of the select as a person does: its value
// set and its change sent, which htmx acts on.
func choose(t *testing.T, ctx context.Context, sel, value string) {
	t.Helper()
	if err := chromedp.Do(ctx, chromedp.SetValue(chromedp.CSS(sel), value),
		chromedp.Evaluate[chromedp.Void](`document.querySelector("`+sel+`").dispatchEvent(new Event("change", {bubbles: true}))`)); err != nil {
		t.Fatal(err)
	}
}

// TestEntryFormJobsInTheBrowser: the accountant's jobs on the new entry's
// form (T73h2), each state checked by axe. At a desktop's width they
// open company 2000's form, choose company 1000 (htmx renders the form
// for it: USD and its ledgers and accounts, the page not loaded again),
// choose company 2000 again (EUR), type two lines whose running totals
// htmx says as each amount changes, and post the entry at once. At a
// phone's width they post one above the limit, which is parked for them
// and said so, and park a new entry with a CSV file's lines.
func TestEntryFormJobsInTheBrowser(t *testing.T) {
	s := newJournaling(t)
	src := axe(t)
	browserCtx := browse(t)
	signIn(t, browserCtx, s.stack)
	desk, dc := tab(t, browserCtx, "", 1280)
	if err := chromedp.Do(desk, chromedp.Navigate(s.url+"/journal/new?legal_entity_id="+s.f.DE.String()), chromedp.WaitVisible(chromedp.CSS("#entry-company")),
		chromedp.Evaluate[chromedp.Void](`window.loadedOnce = true`)); err != nil {
		t.Fatal(err)
	}
	valueIs(t, desk, "entry-currency", "EUR")
	choose(t, desk, "#entry-company", s.f.US.String())
	valueIs(t, desk, "entry-currency", "USD")
	refocus(t, desk)
	audit(t, desk, src, "en", "ltr", 1280)
	choose(t, desk, "#entry-company", s.f.DE.String())
	valueIs(t, desk, "entry-currency", "EUR")
	if err := chromedp.Do(desk,
		chromedp.SetValue(chromedp.CSS("#entry-postingdate"), "2026-03-15"),
		chromedp.SetValue(chromedp.CSS("#entry-docdate"), "2026-03-15"),
		chromedp.SendKeys(chromedp.CSS("#line-1-line_account"), "520000"),
		chromedp.SendKeys(chromedp.CSS("#line-1-line_amount"), "40.00\t"),
	); err != nil {
		t.Fatal(err)
	}
	totalsShow(t, desk, "Difference EUR 40.00")
	if err := chromedp.Do(desk,
		chromedp.SendKeys(chromedp.CSS("#line-2-line_account"), "100000"),
		chromedp.SendKeys(chromedp.CSS("#line-2-line_amount"), "-40.00\t"),
	); err != nil {
		t.Fatal(err)
	}
	totalsShow(t, desk, "Difference EUR 0.00")
	once, err := chromedp.Run(desk, chromedp.Evaluate[bool](`window.loadedOnce === true`))
	if err != nil || !once {
		t.Errorf("the company chosen and the totals said by loading the page again, not by htmx (%v)", err)
	}
	refocus(t, desk)
	audit(t, desk, src, "en", "ltr", 1280)
	if err := chromedp.Do(desk, chromedp.Click(chromedp.CSS("#entry-post-button"))); err != nil {
		t.Fatal(err)
	}
	waitForText(t, desk, "?done=posted", "The entry is posted: this is it.")
	audit(t, desk, src, "en", "ltr", 1280)
	phone, pc := tab(t, browserCtx, "", 390)
	open := func() {
		t.Helper()
		if err := chromedp.Do(phone, chromedp.Navigate(s.url+"/journal/new?legal_entity_id="+s.f.DE.String()), chromedp.WaitVisible(chromedp.CSS("#entry-file")),
			chromedp.SetValue(chromedp.CSS("#entry-postingdate"), "2026-03-15"),
			chromedp.SetValue(chromedp.CSS("#entry-docdate"), "2026-03-15")); err != nil {
			t.Fatal(err)
		}
	}
	open()
	if err := chromedp.Do(phone,
		chromedp.SendKeys(chromedp.CSS("#entry-header"), "Year-end accrual by phone"),
		chromedp.SendKeys(chromedp.CSS("#line-1-line_account"), "520000"),
		chromedp.SendKeys(chromedp.CSS("#line-1-line_amount"), "12000.00"),
		chromedp.SendKeys(chromedp.CSS("#line-2-line_account"), "100000"),
		chromedp.SendKeys(chromedp.CSS("#line-2-line_amount"), "-12000.00\t"),
	); err != nil {
		t.Fatal(err)
	}
	totalsShow(t, phone, "Debits EUR 12,000.00")
	refocus(t, phone)
	audit(t, phone, src, "en", "ltr", 390)
	if err := chromedp.Do(phone, chromedp.Click(chromedp.CSS("#entry-post-button"))); err != nil {
		t.Fatal(err)
	}
	waitForText(t, phone, "?done=limit", "The entry is above the company's limit for posting at once")
	audit(t, phone, src, "en", "ltr", 390)
	file := filepath.Join(t.TempDir(), "lines.csv")
	if err := os.WriteFile(file, []byte("account,amount,line_text\n520000,30.00,Paper\n530000,20.00,Toner\n100000,-50.00,\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	open()
	if err := chromedp.Do(phone,
		chromedp.SendKeys(chromedp.CSS("#entry-header"), "Parked from a file"),
		chromedp.SetUploadFiles(chromedp.CSS("#entry-file"), []string{file}),
		chromedp.Click(chromedp.CSS("#entry-file-button")),
	); err != nil {
		t.Fatal(err)
	}
	waitForText(t, phone, "?done=parked", "The entry is parked.")
	audit(t, phone, src, "en", "ltr", 390)
	var lines int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.parked_entry_line l JOIN ledger.parked_entry e ON e.tenant_id = l.tenant_id AND e.id = l.parked_entry_id
		WHERE e.tenant_id = $1 AND e.header_text = 'Parked from a file'`, []any{s.f.Tenant.ID}, &lines)
	if lines != 3 {
		t.Errorf("the entry parked from a file: got %d lines, want the file's 3", lines)
	}
	for _, e := range append(dc.errors(), pc.errors()...) {
		t.Errorf("browser: %s", e)
	}
}
