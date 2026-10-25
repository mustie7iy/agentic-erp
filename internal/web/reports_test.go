package web

import (
	"encoding/csv"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/net/html"

	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// reportData is what reportsStack's tenant holds.
type reportData struct {
	f               fixture.Env
	leading, local  uuid.UUID    // ledgers 0L and 2L, both kept by company 2000
	order           uuid.UUID    // the purchase order of company 2000's open GR/IR position
	invoice         uuid.UUID    // the supplier's invoice INV-1, due on 9 April
	accountant      pgtest.Actor // reads every report; holds every role, as the approvers do
	viewer          pgtest.Actor // may read entries and companies, not the audit trail
	outsider        pgtest.Actor // may read nothing
	bank, ledgerAcc uuid.UUID    // house bank MAIN, account 100000 of company 2000
}

// reportsStack is the workspace in front of a configured tenant whose
// accountant reads company 2000's reports: 2L kept beside 0L with a
// posting of EUR 25.00 in 2L alone in March, a USD supplier item posted
// without terms, the supplier's invoice of EUR 119.00 posted on 10 March
// under NT30 (due on 9 April), an open GR/IR position and a statement of
// house bank MAIN; a person may read entries but not the audit trail,
// and one may read nothing.
func reportsStack(t *testing.T) (*stack, *reportData) {
	t.Helper()
	d := &reportData{}
	objects := blobstore.Dir{Root: t.TempDir()}
	perms := append([]string{"payments.statement.read", "payments.statement.import", "audit.event.read"}, fixture.ClosePermissions...)
	for _, perm := range fixture.PaymentPermissions {
		if !slices.Contains(perms, perm) {
			perms = append(perms, perm)
		}
	}
	s := newStack(t, stackOptions{permissions: perms, extend: fixture.Parallel, objects: objects, seed: func(f fixture.Env) {
		d.f = f
		f.Revalues(t, fixture.Parallel)
		d.leading, d.local = f.Ledger(t, "0L"), f.Ledger(t, "2L")
		f.LedgerOnly(t, d.local, "2026-03-16", "25.00")
		d.order = f.GRIROpen(t)
		d.invoice = f.Payable(t, f.Supplier, "INV-1", "100.00", "19.00", "119.00")
		d.bank, d.ledgerAcc = f.HouseBank(t), f.Account(t, "2000", "100000")
		d.accountant = f.Person(t, "accountant")
		d.viewer = roleHolder(t, f, "viewer", "ledger.entry.read", "ledger.account.read", "tenancy.org.read")
		d.outsider = roleHolder(t, f, "outsider")
	}})
	return s, d
}

// companyQuery is a report's query for company 2000 with the fields
// given, as name, value pairs.
func companyQuery(d *reportData, kv ...string) string {
	q := url.Values{"legal_entity_id": {d.f.DE.String()}}
	for i := 0; i+1 < len(kv); i += 2 {
		q.Set(kv[i], kv[i+1])
	}
	return q.Encode()
}

// links are the page's links' addresses.
func links(p page) map[string]bool {
	out := map[string]bool{}
	walk(p.doc, func(n *html.Node) {
		if n.Data == "a" {
			out[attrOf(n, "href")] = true
		}
	})
	return out
}

// linkTo is the first link whose address begins with prefix, "" for none.
func linkTo(p page, prefix string) string {
	found := ""
	walk(p.doc, func(n *html.Node) {
		if v := attrOf(n, "href"); n.Data == "a" && found == "" && strings.HasPrefix(v, prefix) {
			found = v
		}
	})
	return found
}

// sheet is a download's records, its byte order mark checked.
func sheet(t *testing.T, what string, r response) [][]string {
	t.Helper()
	if r.status != http.StatusOK || r.header.Get("Content-Type") != "text/csv; charset=utf-8" {
		t.Fatalf("%s: got %d %s, want a CSV: %s", what, r.status, r.header.Get("Content-Type"), r.body)
	}
	body, ok := strings.CutPrefix(r.body, "\xef\xbb\xbf")
	if !ok {
		t.Errorf("%s: no byte order mark", what)
	}
	rd := csv.NewReader(strings.NewReader(body))
	rd.FieldsPerRecord = -1 // the first line holds the filters
	records, err := rd.ReadAll()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	return records
}

// TestReportsIndex: the accountant is offered every report, each house
// bank's reconciliation and the audit history's form, and the reports in
// the navigation; a person who may read nothing is told so and has no
// reports in theirs.
func TestReportsIndex(t *testing.T) {
	s, d := reportsStack(t)
	p := s.as(d.accountant).get("/reports").page(t)
	holds(t, "the index", p.mainText(), "Reports", "Trial balance", "Supplier open items", "GR/IR positions", "Ledger reconciliation",
		"Bank reconciliation", "Audit history")
	got := links(p)
	for _, want := range []string{"/reports", "/reports/trial-balance", "/reports/open-items", "/reports/gr-ir", "/reports/ledger-reconciliation",
		reportPath(d.bank)} {
		if !got[want] {
			t.Errorf("the index links %s: not in %v", want, got)
		}
	}
	if attrOf(p.form("/reports/audit"), "method") != "get" {
		t.Error("the audit history's form: not a GET form")
	}
	outsider := s.as(d.outsider).get("/reports")
	status(t, "the outsider's index", outsider, http.StatusOK)
	holds(t, "the outsider's index", outsider.page(t).mainText(), "You may read none of the reports.")
	if links(outsider.page(t))["/reports"] {
		t.Error("the outsider's navigation offers the reports")
	}
}

// TestTrialBalance: company 2000's trial balance in the leading ledger,
// its accounts named, filtered by period; a year or a period that is
// none is said before the API is called and a company that is no id
// refuses the filters; its download holds every row with the filters in
// its first line; French.
func TestTrialBalance(t *testing.T) {
	s, d := reportsStack(t)
	b := s.as(d.accountant)
	r := b.get("/reports/trial-balance?" + companyQuery(d, "fiscal_year", "2026"))
	status(t, "the trial balance", r, http.StatusOK)
	p := r.page(t)
	holds(t, "the trial balance", p.mainText(), "Trial balance", "211000 Accounts Payable - Domestic 3 USD", "520000",
		"Download as CSV")
	march := b.get("/reports/trial-balance?" + companyQuery(d, "fiscal_year", "2026", "period", "4")).page(t).mainText()
	if strings.Contains(march, "211000") {
		t.Errorf("April's trial balance: got %q, want no March posting", march)
	}
	for name, tc := range map[string]struct {
		query  string
		status int
		want   string
	}{
		"a year that is none":   {companyQuery(d, "fiscal_year", "26"), http.StatusUnprocessableEntity, "Enter the fiscal year as four digits"},
		"a period that is none": {companyQuery(d, "fiscal_year", "2026", "period", "17"), http.StatusUnprocessableEntity, "Enter a period from 1 to 16"},
		"a company, no id":      {"legal_entity_id=x", http.StatusBadRequest, "A filter in the address is not valid."},
		"a page token, none":    {companyQuery(d, "page", "nonsense"), http.StatusBadRequest, "request.invalid_page_token"},
		"a year without any":    {companyQuery(d, "fiscal_year", "2990"), http.StatusOK, "No account has postings in this ledger"},
	} {
		t.Run(name, func(t *testing.T) {
			r := b.get("/reports/trial-balance?" + tc.query)
			status(t, name, r, tc.status)
			holds(t, name, r.page(t).mainText(), tc.want)
		})
	}
	csvPath := linkTo(p, "/reports/trial-balance.csv?")
	dl := b.get(csvPath)
	if got := dl.header.Get("Content-Disposition"); got != `attachment; filename="trial-balance-2026.csv"` {
		t.Errorf("the download's name: got %q", got)
	}
	records := sheet(t, "the download", dl)
	if got := strings.Join(records[0], "|"); got != "Trial balance|fiscal_year=2026|ledger_id="+d.leading.String()+"|legal_entity_id="+d.f.DE.String() {
		t.Errorf("the download's first line: got %q", got)
	}
	if got := strings.Join(records[1], "|"); got != "Account|Period|Currency|Debits|Credits|Debits in the company's currency|Credits in the company's currency" {
		t.Errorf("the download's columns: got %q", got)
	}
	holds(t, "the download's rows", dl.body, "211000 Accounts Payable - Domestic,3,USD,0.00,100.00,0.00,80.00")
	status(t, "a download's filters, none", b.get("/reports/trial-balance.csv?legal_entity_id=x"), http.StatusBadRequest)
	fr := b.get("/reports/trial-balance?"+companyQuery(d, "fiscal_year", "2026"), "Accept-Language", "fr-CA").page(t).mainText()
	holds(t, "French", fr, "Balance de vérification", "Télécharger en CSV")
}

// TestOpenItemsReport: company 2000's supplier open items on its payables
// account, aged on 15 April: the invoice due on 9 April six days overdue,
// the USD item posted without terms apart; each item linked to its entry,
// the supplier named by number; items due by a date; a supplier number
// no supplier has is said before the API is called; the download.
func TestOpenItemsReport(t *testing.T) {
	s, d := reportsStack(t)
	b := s.as(d.accountant)
	r := b.get("/reports/open-items?" + companyQuery(d, "key_date", "2026-04-15"))
	status(t, "the open items", r, http.StatusOK)
	p := r.page(t)
	holds(t, "the open items", p.mainText(), "Supplier open items", "Aging on 2026-04-15", "Overdue 1 to 30 EUR 1 EUR -119.00",
		"No due date None EUR 1 EUR -80.00", "2026-03-10 2026-04-09 None 6 EUR -119.00 EUR -119.00")
	entry := linkTo(p, "/journal/entries/")
	if entry == "" {
		t.Fatal("an open item: not linked to its entry")
	}
	status(t, "the item's entry", b.get(entry), http.StatusOK)
	if got := field(p.form("/reports/open-items"), "key_date"); got != "2026-04-15" {
		t.Errorf("the key date: got %q", got)
	}
	named := b.get("/reports/open-items?" + companyQuery(d, "key_date", "2026-04-15", "supplier", "BP90000001")).page(t).mainText()
	holds(t, "the supplier's items", named, "Supplier: BP90000001 BP90000001", "BP90000001 BP90000001 INV-1")
	vendor := b.get("/reports/open-items?" + companyQuery(d, "key_date", "2026-04-15", "supplier", "BP90000002")).page(t).mainText()
	holds(t, "the vendor's items", vendor, "Supplier: BP90000002 BP90000002", "No open item matches the filters.")
	due := b.get("/reports/open-items?" + companyQuery(d, "key_date", "2026-04-15", "due_date_to", "2026-04-01")).page(t).mainText()
	holds(t, "due by 1 April", due, "No open item matches the filters.")
	unknown := b.get("/reports/open-items?" + companyQuery(d, "supplier", "BP00000000"))
	status(t, "an unknown supplier", unknown, http.StatusUnprocessableEntity)
	holds(t, "an unknown supplier", unknown.page(t).mainText(), "No supplier has this number.")
	status(t, "a key date, none", b.get("/reports/open-items?"+companyQuery(d, "key_date", "15.04.2026")), http.StatusUnprocessableEntity)
	other := b.get("/reports/open-items?" + companyQuery(d, "key_date", "2026-04-15", "account_id", d.f.Account(t, "1000", "211000").String()))
	holds(t, "another company's account", other.page(t).mainText(), "Overdue 1 to 30 EUR 1 EUR -119.00")
	records := sheet(t, "the download", b.get(linkTo(p, "/reports/open-items.csv?")))
	holds(t, "the download", strings.Join(records[0], "|"), "Supplier open items|account_id=", "key_date=2026-04-15")
	holds(t, "the download's rows", strings.Join(records[len(records)-1], "|")+strings.Join(records[len(records)-2], "|"),
		"2026-03-10|2026-04-09||6|-119.00|EUR|-119.00|EUR")
	status(t, "a download's filters, none", b.get("/reports/open-items.csv?legal_entity_id=x"), http.StatusBadRequest)
}

// TestGRIRReport: company 2000's open GR/IR position, every position when
// asked, and the download.
func TestGRIRReport(t *testing.T) {
	s, d := reportsStack(t)
	b := s.as(d.accountant)
	p := b.get("/reports/gr-ir?" + companyQuery(d)).page(t)
	holds(t, "the positions", p.mainText(), "GR/IR positions", d.order.String()+" 1 10 EA 4 EA EUR -72.00 2026-03-05")
	all := b.get("/reports/gr-ir?" + companyQuery(d, "all", "1")).page(t)
	if got := linkTo(all, "/reports/gr-ir.csv?"); !strings.Contains(got, "all=1") {
		t.Errorf("every position's download: got %q", got)
	}
	records := sheet(t, "the download", b.get(linkTo(p, "/reports/gr-ir.csv?")))
	if got := strings.Join(records[2], "|"); got != d.order.String()+"|1|10 EA|4 EA|EUR|-72.00|2026-03-05" {
		t.Errorf("the download's row: got %q", got)
	}
	status(t, "a company, no id", b.get("/reports/gr-ir?legal_entity_id=x"), http.StatusBadRequest)
	status(t, "a download's company, none", b.get("/reports/gr-ir.csv"), http.StatusBadRequest)
}

// TestLedgerReconReport: company 2000's leading ledger compared with 2L
// until another is chosen, the difference on account 100000 linked to
// the entry posted in 2L alone, which links to its page; 2L compared
// with the leading ledger; company 1000, keeping one ledger, has nothing
// to reconcile; the pre-close checks link to these reports.
func TestLedgerReconReport(t *testing.T) {
	s, d := reportsStack(t)
	b := s.as(d.accountant)
	p := b.get("/reports/ledger-reconciliation?" + companyQuery(d, "fiscal_year", "2026")).page(t)
	holds(t, "the reconciliation", p.mainText(), "Ledger 0L compared with ledger 2L.", "Debits and credits net to zero",
		"100000 Bank - Operating EUR EUR 0.00 EUR 25.00 EUR -25.00")
	entries := linkTo(p, "/reports/ledger-reconciliation/entries?")
	if entries == "" {
		t.Fatal("the difference: not linked to its entries")
	}
	ep := b.get(entries).page(t)
	holds(t, "the entries", ep.mainText(), "Entries behind the difference", "2L", "2026-03-16 3 EUR 25.00 EUR 25.00")
	if to := linkTo(ep, "/journal/entries/"); to == "" || b.get(to).status != http.StatusOK {
		t.Errorf("the entry behind the difference: link %q", to)
	}
	local := b.get("/reports/ledger-reconciliation?" + companyQuery(d, "fiscal_year", "2026", "ledger_id", d.local.String())).page(t).mainText()
	holds(t, "2L's reconciliation", local, "Ledger 2L compared with ledger 0L.")
	us := b.get("/reports/ledger-reconciliation?legal_entity_id=" + d.f.US.String()).page(t).mainText()
	holds(t, "company 1000's", us, "The company keeps one ledger: there is nothing to reconcile.")
	status(t, "a ledger, no id", b.get("/reports/ledger-reconciliation?"+companyQuery(d, "ledger_id", "0L")), http.StatusBadRequest)
	status(t, "a company, no id", b.get("/reports/ledger-reconciliation?legal_entity_id=x"), http.StatusBadRequest)
	status(t, "a year, none", b.get("/reports/ledger-reconciliation?"+companyQuery(d, "fiscal_year", "x")), http.StatusUnprocessableEntity)
	status(t, "entries without an account", b.get("/reports/ledger-reconciliation/entries?"+companyQuery(d)), http.StatusBadRequest)
	march := periodOfReports(t, d)
	checks := b.get("/close/checks?" + companyQuery(d, "ledger_id", d.leading.String(), "fiscal_year", "2026", "period", "3",
		"period_id", march.String())).page(t)
	got := links(checks)
	if !got["/reports/gr-ir?legal_entity_id="+d.f.DE.String()] {
		t.Errorf("the GR/IR check: not linked to its report in %v", got)
	}
	recon := linkTo(checks, "/reports/ledger-reconciliation?")
	holds(t, "the reconciliation check's report", recon, "compare_ledger_id="+d.local.String(), "period=3")
	if linkTo(checks, "/reports/ledger-reconciliation/entries?") == "" {
		t.Error("the reconciliation check's difference: not linked to its entries")
	}
}

// periodOfReports is company 2000's March 2026 in the leading ledger.
func periodOfReports(t *testing.T, d *reportData) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	pgtest.Scan(t, `SELECT id FROM ledger.period_status WHERE tenant_id = $1 AND legal_entity_id = $2 AND ledger_id = $3 AND fiscal_year = 2026 AND period = 3`,
		[]any{d.f.Tenant.ID, d.f.DE, d.leading}, &id)
	return id
}

// TestAuditHistory: an entry's page links its audit history, which says
// who posted it and when, its details as data; a period's and a parked
// entry's link theirs; an id that is none is said; a person who may not
// read the audit trail is offered no link and refused the history.
func TestAuditHistory(t *testing.T) {
	s, d := reportsStack(t)
	b := s.as(d.accountant)
	items := b.get("/reports/open-items?" + companyQuery(d, "key_date", "2026-04-15", "supplier", "BP90000001")).page(t)
	entry := b.get(linkTo(items, "/journal/entries/")).page(t)
	history := linkTo(entry, "/reports/audit?")
	if history == "" {
		t.Fatal("the entry's page: no audit history")
	}
	h := b.get(history).page(t).mainText()
	holds(t, "the entry's history", h, "Audit history", "ledger.journal_entry.post", "Kind Person", "Outcome succeeded", "document_number",
		"source_context payables")
	period := b.get(periodPath(periodOfReports(t, d))).page(t)
	if linkTo(period, "/reports/audit?") == "" {
		t.Error("the period's page: no audit history")
	}
	bad := b.get("/reports/audit?object_id=5100000123")
	status(t, "an id that is none", bad, http.StatusUnprocessableEntity)
	holds(t, "an id that is none", bad.page(t).mainText(), "Enter a document's id")
	holds(t, "a document without events", b.get("/reports/audit?object_id="+uuid.NewString()).page(t).mainText(), "No event is recorded for this document.")
	holds(t, "the form alone", b.get("/reports/audit").page(t).mainText(), "Document id")
	viewer := s.as(d.viewer)
	if linkTo(viewer.get(linkTo(items, "/journal/entries/")).page(t), "/reports/audit?") != "" {
		t.Error("the viewer is offered the audit history")
	}
	refused := viewer.get(history)
	status(t, "the viewer's history", refused, http.StatusForbidden)
	holds(t, "the viewer's history", refused.page(t).mainText(), "The report could not be shown", "You do not have the permission this needs.")
}

// TestReportsAsOthers: a person who may read entries but not the audit
// trail has the ledger's reports and no audit history; one who may read
// nothing is refused each report, said in its words.
func TestReportsAsOthers(t *testing.T) {
	s, d := reportsStack(t)
	viewer := s.as(d.viewer)
	idx := viewer.get("/reports").page(t)
	if links(idx)["/reports/trial-balance"] != true || len(idx.all("form", "")) != 1 {
		t.Errorf("the viewer's index: got links %v and %d forms, want the ledger's reports and the sign-out alone", links(idx), len(idx.all("form", "")))
	}
	outsider := s.as(d.outsider)
	for _, path := range []string{"/reports/trial-balance?" + companyQuery(d), "/reports/open-items?" + companyQuery(d), "/reports/gr-ir?" + companyQuery(d),
		"/reports/ledger-reconciliation?" + companyQuery(d, "compare_ledger_id", d.local.String(), "ledger_id", d.leading.String())} {
		r := outsider.get(path)
		status(t, path, r, http.StatusForbidden)
		holds(t, path, r.page(t).mainText(), "The report could not be shown", "You do not have the permission this needs.")
	}
	refused := outsider.get("/reports/trial-balance.csv?" + companyQuery(d, "fiscal_year", "2026"))
	status(t, "the outsider's download", refused, http.StatusForbidden)
	holds(t, "the outsider's download", refused.page(t).mainText(), "The report could not be shown", "Back to the report")
}
