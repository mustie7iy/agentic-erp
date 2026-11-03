package closing_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// withTemplate gives the prototype's document a close template of three
// tasks, one of them company 1000's only; company 1000 a holiday on Monday
// 2 February 2026; company 2000 a Friday and Saturday weekend and the
// parallel ledger 2L, whose periods the calendar leaves out (ADR-0058 §3).
func withTemplate(doc *config.Config) {
	fixture.Parallel(doc)
	doc.Close.Tasks = []config.CloseTask{
		{Code: "AP_CUTOFF", Name: "Supplier invoices of the period entered", Kind: config.CloseManual, DueDay: 2},
		{Code: "REVIEW", Name: "Balances reviewed", Kind: config.CloseManual, DueDay: -1},
		{Code: "REPORT", Name: "Management report", Kind: config.CloseManual, DueDay: 5, DependsOn: []string{"AP_CUTOFF"}, Companies: []string{"1000"}},
	}
	doc.Calendars = []config.Calendar{
		{Company: "1000", Holidays: []config.Holiday{{Date: "2026-02-02", Name: "A Monday holiday"}}},
		{Company: "2000", Weekend: []string{"friday", "saturday"}},
	}
}

// calendar reads a close calendar as its company and one line per period:
// number, dates, status and planned due dates.
func calendar(t *testing.T, c apitest.Client, legalEntity uuid.UUID, year int) (string, []string) {
	t.Helper()
	got, _ := c.Must(200, "GET", fmt.Sprintf("/v1/close-calendar?legal_entity_id=%s&fiscal_year=%d", legalEntity, year), "", "")
	company := fmt.Sprintf("%v %v %v %v %v %v", got["company_code"], got["company_name"], got["fiscal_year"], got["ledger_code"], got["ledger_name"], got["weekend"])
	var rows []string
	for _, p := range apitest.List(t, got, "periods") {
		p := p.(map[string]any)
		rows = append(rows, fmt.Sprintf("%v %v..%v %v due %v..%v", p["period"], p["start_date"], p["end_date"], p["status"], p["first_due_on"], p["last_due_on"]))
	}
	return company, rows
}

func wantRows(t *testing.T, what string, got []string, want map[int]string, count int) {
	t.Helper()
	if len(got) != count {
		t.Errorf("%s: got %d rows, want %d: %q", what, len(got), count, got)
		return
	}
	for i, w := range want {
		if got[i-1] != w {
			t.Errorf("%s, period %d: got %q, want %q", what, i, got[i-1], w)
		}
	}
}

// TestGetCloseCalendar: each regular period of the leading ledger, in
// order, with its status and the first and last due date of the tasks the
// template gives the company, counted in its business days: its weekend
// and holidays.
func TestGetCloseCalendar(t *testing.T) {
	e := fixture.NewWith(t, nil, withTemplate, "closing.close.read", "ledger.period.read")
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2 AND fiscal_year = 2026 AND period = 1`,
		e.Tenant.ID, e.US)

	company, rows := calendar(t, e.Client, e.US, 2026)
	if want := "1000 Acme Corp US 2026 0L Leading ledger [saturday sunday]"; company != want {
		t.Errorf("company 1000: got %q, want %q", company, want)
	}
	// January ends on a Saturday: REVIEW on Friday the 30th; AP_CUTOFF two
	// business days after, the Monday holiday skipped; REPORT five after.
	// December ends on a Thursday, and 2027 has no holidays configured.
	wantRows(t, "company 1000", rows, map[int]string{
		1:  "1 2026-01-01..2026-01-31 closed due 2026-01-30..2026-02-09",
		2:  "2 2026-02-01..2026-02-28 open due 2026-02-27..2026-03-06",
		12: "12 2026-12-01..2026-12-31 open due 2026-12-30..2027-01-07",
	}, 12)

	company, rows = calendar(t, e.Client, e.DE, 2026)
	if want := "2000 Acme GmbH 2026 0L Leading ledger [friday saturday]"; company != want {
		t.Errorf("company 2000: got %q, want %q", company, want)
	}
	// Friday and Saturday are its weekend: January's REVIEW is Thursday the
	// 29th and AP_CUTOFF Monday 2 February, after Sunday the 1st; REPORT is
	// company 1000's only.
	wantRows(t, "company 2000, leading ledger only", rows, map[int]string{
		1: "1 2026-01-01..2026-01-31 open due 2026-01-29..2026-02-02",
		7: "7 2026-07-01..2026-07-31 open due 2026-07-30..2026-08-03",
	}, 12)

	got, _ := e.Must(200, "GET", fmt.Sprintf("/v1/close-calendar?legal_entity_id=%s&fiscal_year=2026", e.US), "", "")
	id := apitest.String(t, apitest.List(t, got, "periods")[0].(map[string]any), "fiscal_period_id")
	period, _ := e.Must(200, "GET", "/v1/fiscal-periods/"+id, "", "")
	if got := fmt.Sprintf("%v %v %v", period["legal_entity_id"], period["fiscal_year"], period["period"]); got != e.US.String()+" 2026 1" {
		t.Errorf("the row's fiscal period: got %s, want company 1000's period 1 of 2026", got)
	}
	if period["ledger_id"] != e.Ledger(t, config.LeadingLedger).String() {
		t.Errorf("the row's ledger: got %v, want the leading ledger", period["ledger_id"])
	}
}

// TestCloseCalendarWithoutRows: a legal entity the caller cannot act in, or
// that does not exist, has neither company nor rows; a year without
// periods has the company and no rows; without a template task for the
// company, no row has planned dates.
func TestCloseCalendarWithoutRows(t *testing.T) {
	e := fixture.NewWith(t, nil, withTemplate, "closing.close.read")
	reader := e.As(e.Reader(t, "reader", e.US))
	cases := []struct {
		name        string
		client      apitest.Client
		legalEntity uuid.UUID
		year        int
		company     string
	}{
		{"a company the caller cannot act in", reader, e.DE, 2026, "<nil> <nil> 2026 0L Leading ledger <nil>"},
		{"a legal entity that does not exist", e.Client, uuid.Must(uuid.NewV7()), 2026, "<nil> <nil> 2026 0L Leading ledger <nil>"},
		{"a year without periods", e.Client, e.US, 2999, "1000 Acme Corp US 2999 0L Leading ledger [saturday sunday]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			company, rows := calendar(t, tc.client, tc.legalEntity, tc.year)
			if company != tc.company {
				t.Errorf("company: got %q, want %q", company, tc.company)
			}
			if len(rows) != 0 {
				t.Errorf("rows: got %q, want none", rows)
			}
		})
	}
	_, rows := calendar(t, reader, e.US, 2026)
	if len(rows) != 12 {
		t.Errorf("rows of the reader's own company: got %d, want 12", len(rows))
	}

	bare := fixture.NewWith(t, nil, nil, "closing.close.read")
	_, rows = calendar(t, bare.Client, bare.US, 2026)
	wantRows(t, "without a template", rows, map[int]string{1: "1 2026-01-01..2026-01-31 open due <nil>..<nil>"}, 12)
}

// TestCloseCalendarRefusals: the year and legal entity are required, and a
// tenant without a configuration has no template (409
// config.not_bootstrapped).
func TestCloseCalendarRefusals(t *testing.T) {
	ten := pgtest.NewTenant(t, "closing.close.read")
	c := apitest.Client{T: t, API: apitest.New(t), Tenant: ten, As: ten.Human}
	c.Problem(409, "config.not_bootstrapped", "GET", "/v1/close-calendar?legal_entity_id="+uuid.NewString()+"&fiscal_year=2026", "", "")
	if status, _, _ := c.Do("GET", "/v1/close-calendar?legal_entity_id="+uuid.NewString(), "", ""); status != 400 {
		t.Errorf("without a fiscal year: got %d, want 400", status)
	}
	other := pgtest.NewTenant(t)
	c = apitest.Client{T: t, API: c.API, Tenant: other, As: other.Human}
	c.Problem(403, "identity.permission_missing", "GET", "/v1/close-calendar?legal_entity_id="+uuid.NewString()+"&fiscal_year=2026", "", "")
}

// TestDatabaseFailures calls the handler with each of its database
// statements failing in turn.
func TestDatabaseFailures(t *testing.T) {
	e := fixture.NewWith(t, nil, withTemplate, "closing.close.read")
	h := app.Closing(app.Config{})
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "close calendar", func(ctx context.Context) error {
		_, err := h.GetCloseCalendar(ctx, gen.GetCloseCalendarParams{LegalEntityID: e.US, FiscalYear: 2026})
		return err
	})
	if _, err := h.GetCloseCalendar(context.Background(), gen.GetCloseCalendarParams{LegalEntityID: e.US, FiscalYear: 2026}); err == nil {
		t.Error("outside a unit of work: got no error, want one")
	}
}
