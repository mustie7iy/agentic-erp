package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/html"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"

	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// companyJSON is a legal entity as listLegalEntities gives it.
func companyJSON(id uuid.UUID, code, name, country, currency string) string {
	return fmt.Sprintf(`{"id":%q,"version":1,"created_at":"2026-10-22T10:00:00Z","created_by":%q,"updated_at":"2026-10-22T10:00:00Z","code":%q,
		"name":%q,"country_code":%q,"local_currency":%q,"fiscal_calendar_id":%q,"status":"active","extensions":{}}`,
		id, uuid.New(), code, name, country, currency, uuid.New())
}

// todayStub is the API of a tenant with a company in Toronto and one in
// Dubai, at 22:00 on 31 December in Toronto (03:00 UTC on 1 January,
// 07:00 in Dubai): getLegalEntityToday tells each company's date, which
// for Toronto is neither the UTC date nor in the UTC year; today answers
// it (nil: the API cannot tell it).
func todayStub(toronto, dubai uuid.UUID, today http.HandlerFunc) *stubbed {
	if today == nil {
		today = func(w http.ResponseWriter, r *http.Request) {
			day := map[string]string{toronto.String(): "2025-12-31", dubai.String(): "2026-01-01"}[strings.Split(r.URL.Path, "/")[3]]
			answer(200, fmt.Sprintf(`{"legal_entity_id":%q,"date":%q,"time_zone":"x"}`, strings.Split(r.URL.Path, "/")[3], day))(w, r)
		}
	}
	empty := answer(200, `{"items":[]}`)
	return &stubbed{routes: map[string]http.HandlerFunc{
		"GET /v1/me": answer(200, meJSON(permReadEntries, permParkEntries, permReadCompanies, permReadAccounts, permReadRuns, permProposeRuns,
			permReadPeriods)),
		"GET /v1/legal-entities": answer(200, `{"items":[`+companyJSON(toronto, "3000", "Maple Ltd", "CA", "CAD")+`,`+
			companyJSON(dubai, "4000", "Acme FZE", "AE", "AED")+`]}`),
		"GET /v1/legal-entities/": today,
		"GET /v1/accounts":        empty, "GET /v1/ledgers": empty, "GET /v1/house-banks": empty, "GET /v1/payment-runs": empty,
		"GET /v1/fiscal-periods": empty, "GET /v1/balances": empty, "GET /v1/open-items": empty, "GET /v1/principal-names": empty,
		"GET /v1/ledger-reconciliation": empty,
	}}
}

// TestFormsDatedOnTheCompanysDay: each form and report the workspace
// dates is dated on its company's day as the API tells it
// (getLegalEntityToday), not on the workspace's clock in UTC: a manual
// entry's document and posting dates, a payment run's run date and due
// date (of the company the form chooses first), the open items' key date,
// and the periods' and ledger reports' fiscal year (review:batch27).
func TestFormsDatedOnTheCompanysDay(t *testing.T) {
	toronto, dubai := uuid.New(), uuid.New()
	_, b, _ := stubStack(t, todayStub(toronto, dubai, nil))
	in := func(path string, company uuid.UUID) string {
		return path + "?" + ui.FieldCompany + "=" + company.String()
	}
	for name, tc := range map[string]struct {
		path   string
		fields map[string]string
	}{
		"a Toronto entry": {in(ui.NewEntryPath, toronto), map[string]string{ui.FieldDocDate: "2025-12-31", ui.FieldPostingDate: "2025-12-31"}},
		"a Dubai entry":   {in(ui.NewEntryPath, dubai), map[string]string{ui.FieldDocDate: "2026-01-01", ui.FieldPostingDate: "2026-01-01"}},
		// The propose form's first company by name is Acme FZE, in Dubai.
		"a payment run":            {ui.PaymentsPath, map[string]string{ui.FieldRunDate: "2026-01-01", ui.FieldDueBy: "2026-01-01"}},
		"Toronto's open items":     {in(ui.OpenItemsPath, toronto), map[string]string{ui.FieldKeyDate: "2025-12-31"}},
		"Dubai's open items":       {in(ui.OpenItemsPath, dubai), map[string]string{ui.FieldKeyDate: "2026-01-01"}},
		"Toronto's periods":        {in(ui.ClosePath, toronto), map[string]string{ui.FieldYear: "2025"}},
		"Toronto's trial balance":  {in(ui.TrialBalancePath, toronto), map[string]string{ui.FieldYear: "2025"}},
		"Toronto's reconciliation": {in(ui.LedgerReconPath, toronto), map[string]string{ui.FieldYear: "2025"}},
	} {
		t.Run(name, func(t *testing.T) {
			p := b.get(tc.path).page(t)
			for f, want := range tc.fields {
				if got := field(p.doc, f); got != want {
					t.Errorf("%s: got %q, want %q", f, got, want)
				}
			}
		})
	}
	p := b.get(ui.PaymentsPath).page(t)
	if got := chosen(p.doc, ui.FieldCompany); got != dubai.String() {
		t.Errorf("the propose form's company: got %s, want Dubai's %s, whose date it holds", got, dubai)
	}
}

// TestFormsUndatedWithoutTheCompanysDay: when the API cannot tell the
// company's date, a form's dates are left for the person to type, never
// the workspace's UTC day; a report's year, a filter, is the clock's.
func TestFormsUndatedWithoutTheCompanysDay(t *testing.T) {
	toronto, dubai := uuid.New(), uuid.New()
	s, b, _ := stubStack(t, todayStub(toronto, dubai, answer(409, problemJSON(409, "config.not_bootstrapped"))))
	in := func(path string) string { return path + "?" + ui.FieldCompany + "=" + toronto.String() }
	year := strconv.Itoa(s.clock.Now().Year())
	for name, tc := range map[string]struct {
		path   string
		fields map[string]string
	}{
		"an entry":        {in(ui.NewEntryPath), map[string]string{ui.FieldDocDate: "", ui.FieldPostingDate: ""}},
		"a payment run":   {ui.PaymentsPath, map[string]string{ui.FieldRunDate: "", ui.FieldDueBy: ""}},
		"open items":      {in(ui.OpenItemsPath), map[string]string{ui.FieldKeyDate: ""}},
		"periods":         {in(ui.ClosePath), map[string]string{ui.FieldYear: year}},
		"a trial balance": {in(ui.TrialBalancePath), map[string]string{ui.FieldYear: year}},
	} {
		t.Run(name, func(t *testing.T) {
			p := b.get(tc.path).page(t)
			for f, want := range tc.fields {
				if got := field(p.doc, f); got != want {
					t.Errorf("%s: got %q, want %q", f, got, want)
				}
			}
		})
	}
	if !strings.Contains(s.logs.String(), "the company's date could not be read") {
		t.Error("the refused date: not logged")
	}
}

// chosen is the value of the option chosen in the select of the name, as
// a browser sends it (formValues).
func chosen(doc *html.Node, name string) string {
	v := ""
	walk(doc, func(n *html.Node) {
		if n.Data == "select" && attrOf(n, "name") == name && v == "" {
			v = formValues(n).Get(name)
		}
	})
	return v
}

// TestEntryDatedOnTheCompanysDayByTheAPI: against the real API, a new
// entry of company 1000, in Toronto, is dated Toronto's day and one of
// company 2000, in Dubai, Dubai's, by the API's clock (the stack's, which
// is the wall clock: a test cannot move the provider's tokens' clock).
func TestEntryDatedOnTheCompanysDayByTheAPI(t *testing.T) {
	zones := map[string]string{"1000": "America/Toronto", "2000": "Asia/Dubai"}
	var f fixture.Env
	s := newStack(t, stackOptions{permissions: fixture.JournalPermissions, seed: func(e fixture.Env) { f = e }, extend: func(c *config.Config) {
		for i, co := range c.Companies {
			c.Companies[i].TimeZone = zones[co.Code]
		}
	}})
	b := s.browser()
	b.signIn()
	for code, company := range map[string]uuid.UUID{"1000": f.US, "2000": f.DE} {
		t.Run(zones[code], func(t *testing.T) {
			loc, err := time.LoadLocation(zones[code])
			if err != nil {
				t.Fatal(err)
			}
			before := time.Now().In(loc).Format(time.DateOnly)
			p := b.get(ui.NewEntryPath + "?" + ui.FieldCompany + "=" + company.String()).page(t)
			after := time.Now().In(loc).Format(time.DateOnly)
			if got := field(p.doc, ui.FieldPostingDate); got != before && got != after {
				t.Errorf("posting date: got %q, want %s, the company's day", got, after)
			}
		})
	}
}
