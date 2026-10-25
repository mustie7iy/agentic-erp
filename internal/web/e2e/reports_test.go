package e2e

import (
	"fmt"
	"net/url"
	"slices"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// reporting is the stack of a configured tenant whose accountant reads
// company 2000's reports: 2L kept beside 0L with a posting in 2L alone in
// March, a USD supplier item posted without terms, the supplier's invoice
// due on 9 April and an open GR/IR position.
type reporting struct {
	stack
	f              fixture.Env
	accountant     pgtest.Actor
	leading, local uuid.UUID
	entry          uuid.UUID // the invoice's journal entry
}

func newReporting(t *testing.T) reporting {
	t.Helper()
	perms := append([]string{"tenancy.org.read", "payments.statement.read", "audit.event.read"}, fixture.ClosePermissions...)
	for _, p := range fixture.PaymentPermissions {
		if !slices.Contains(perms, p) {
			perms = append(perms, p)
		}
	}
	f := fixture.NewWith(t, nil, fixture.Parallel, perms...)
	f.Revalues(t, fixture.Parallel)
	s := reporting{f: f, accountant: f.Person(t, "accountant"), leading: f.Ledger(t, "0L"), local: f.Ledger(t, "2L")}
	f.LedgerOnly(t, s.local, "2026-03-16", "25.00")
	f.GRIROpen(t)
	invoice := f.Payable(t, f.Supplier, "INV-1", "100.00", "19.00", "119.00")
	pgtest.Scan(t, `SELECT id FROM ledger.journal_entry WHERE tenant_id = $1 AND source_document_id = $2 AND ledger_id = $3`,
		[]any{f.Tenant.ID, invoice, s.leading}, &s.entry)
	s.stack = stackFor(t, f.Tenant, s.accountant)
	return s
}

// query is company 2000's report query with the fields given, as name,
// value pairs.
func (s reporting) query(kv ...string) string {
	q := url.Values{"legal_entity_id": {s.f.DE.String()}}
	for i := 0; i+1 < len(kv); i += 2 {
		q.Set(kv[i], kv[i+1])
	}
	return q.Encode()
}

// checkReportPages checks the reports' index, the trial balance, the open
// items with their aging, the GR/IR positions, the ledger reconciliation,
// the entries behind its difference and an entry's audit history, in the
// languages at a desktop's and a phone's width, with every check of
// TestPagesInTheBrowser and axe at zero.
func checkReportPages(t *testing.T, s reporting, langs []lang) {
	t.Helper()
	browserCtx := browse(t)
	src := axe(t)
	signIn(t, browserCtx, s.stack)
	paths := []string{"/reports", "/reports/trial-balance?" + s.query("fiscal_year", "2026"), "/reports/open-items?" + s.query("key_date", "2026-04-15"),
		"/reports/gr-ir?" + s.query(), "/reports/ledger-reconciliation?" + s.query("fiscal_year", "2026"),
		"/reports/ledger-reconciliation/entries?" + s.query("ledger_id", s.leading.String(), "compare_ledger_id", s.local.String(), "fiscal_year", "2026",
			"account_id", s.f.Account(t, "2000", "100000").String(), "currency", "EUR"),
		"/reports/audit?object_id=" + s.entry.String()}
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

// TestReportsInTheBrowser: the reports in English and French.
func TestReportsInTheBrowser(t *testing.T) {
	checkReportPages(t, newReporting(t), []lang{{"", "en", "ltr"}, {"fr-CA", "fr-CA", "ltr"}})
}

// TestReportsInPseudoLocalesInTheBrowser: the reports in both
// pseudo-locales.
func TestReportsInPseudoLocalesInTheBrowser(t *testing.T) {
	checkReportPages(t, newReporting(t), []lang{{"en-XA", "en-XA", "ltr"}, {"ar-XB", "ar-XB", "rtl"}})
}

// TestReportsJobsInTheBrowser: the accountant's reports as they read
// them, with the mouse, each state checked by axe. At a desktop's width
// they open the supplier open items from the index, choose company 2000
// and a key date, and drill down from the overdue invoice to its entry
// and on to its audit history. At a phone's they open the ledger
// reconciliation, choose company 2000 and drill down from the difference
// to the entry posted in 2L alone.
func TestReportsJobsInTheBrowser(t *testing.T) {
	s := newReporting(t)
	src := axe(t)
	browserCtx := browse(t)
	signIn(t, browserCtx, s.stack)
	desk, dc := tab(t, browserCtx, "", 1280)
	if err := chromedp.Do(desk,
		chromedp.Navigate(s.url+"/reports"),
		chromedp.WaitVisible(chromedp.CSS(`a[href="/reports/open-items"]`)),
		chromedp.Click(chromedp.CSS(`a[href="/reports/open-items"]`)),
		chromedp.WaitVisible(chromedp.CSS("#items-company")),
		chromedp.SetValue(chromedp.CSS("#items-company"), s.f.DE.String()),
		chromedp.SetValue(chromedp.CSS("#items-key-date"), "2026-04-15"),
		chromedp.Click(chromedp.CSS(`form[action="/reports/open-items"] button[type=submit]`)),
	); err != nil {
		t.Fatal(err)
	}
	waitForText(t, desk, "/reports/open-items", "Aging on 2026-04-15")
	audit(t, desk, src, "en", "ltr", 1280)
	entry := `a[href="/journal/entries/` + s.entry.String() + `"]`
	if err := chromedp.Do(desk, chromedp.Click(chromedp.CSS(entry))); err != nil {
		t.Fatal(err)
	}
	waitForText(t, desk, "/journal/entries/"+s.entry.String(), "Show its audit history")
	audit(t, desk, src, "en", "ltr", 1280)
	if err := chromedp.Do(desk, chromedp.Click(chromedp.CSS(`a[href^="/reports/audit?"]`))); err != nil {
		t.Fatal(err)
	}
	waitForText(t, desk, "/reports/audit", "ledger.journal_entry.post")
	audit(t, desk, src, "en", "ltr", 1280)
	phone, pc := tab(t, browserCtx, "", 390)
	if err := chromedp.Do(phone,
		chromedp.Navigate(s.url+"/reports"),
		chromedp.WaitVisible(chromedp.CSS(`a[href="/reports/ledger-reconciliation"]`)),
		chromedp.Click(chromedp.CSS(`a[href="/reports/ledger-reconciliation"]`)),
		chromedp.WaitVisible(chromedp.CSS("#report-company")),
		chromedp.SetValue(chromedp.CSS("#report-company"), s.f.DE.String()),
		chromedp.SetValue(chromedp.CSS("#report-year"), "2026"),
		chromedp.Click(chromedp.CSS(`form[action="/reports/ledger-reconciliation"] button[type=submit]`)),
	); err != nil {
		t.Fatal(err)
	}
	waitForText(t, phone, "/reports/ledger-reconciliation", "Ledger 0L compared with ledger 2L.")
	audit(t, phone, src, "en", "ltr", 390)
	if err := chromedp.Do(phone, chromedp.Click(chromedp.CSS(`a[href^="/reports/ledger-reconciliation/entries?"]`))); err != nil {
		t.Fatal(err)
	}
	waitForText(t, phone, "/reports/ledger-reconciliation/entries", "Entries behind the difference")
	audit(t, phone, src, "en", "ltr", 390)
	if err := chromedp.Do(phone, chromedp.Click(chromedp.CSS(`table a[href^="/journal/entries/"]`))); err != nil {
		t.Fatal(err)
	}
	waitForText(t, phone, "/journal/entries/", "Reverse the entry")
	for _, e := range append(dc.errors(), pc.errors()...) {
		t.Errorf("browser: %s", e)
	}
}
