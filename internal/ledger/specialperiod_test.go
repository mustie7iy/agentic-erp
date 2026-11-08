package ledger_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// specialEnv is a tenant whose company 2000 keeps 0L and 2L, with 2026's
// regular periods and its special periods 13 and 14 open in both, and the
// accounts of 2000 a year-end adjustment posts to.
type specialEnv struct {
	parallelEnv
	bank, revenue string
}

func setupSpecial(t *testing.T) specialEnv {
	t.Helper()
	p := setupParallel(t)
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'open', version = version + 1 WHERE tenant_id = $1 AND fiscal_year = 2026
		AND period IN (13, 14)`, p.ten.ID)
	return specialEnv{parallelEnv: p, bank: p.account(t, "2000", "100000"), revenue: p.account(t, "2000", "400000")}
}

// inPeriod is a journal entry body naming the special period n.
func inPeriod(body string, n int) string {
	return fmt.Sprintf(`{"fiscal_period": %d, `, n) + body[1:]
}

// adjustment is an audit adjustment of company 2000 dated day, naming the
// special period n.
func (s specialEnv) adjustment(day string, n int) string {
	return inPeriod(entry(s.de, day, "EUR", line{s.bank, "100.00", ""}, line{s.revenue, "-100.00", ""}), n)
}

// setPeriod sets the status of company 2000's 2026 period n in the ledger
// code, as the period lock's transitions leave it.
func (s specialEnv) setPeriod(t *testing.T, code string, n int, status string) {
	t.Helper()
	pgtest.Exec(t, `UPDATE ledger.period_status ps SET status = $4, version = ps.version + 1 FROM ledger.ledger l
		WHERE ps.tenant_id = $1 AND l.tenant_id = ps.tenant_id AND l.id = ps.ledger_id AND l.code = $2 AND ps.legal_entity_id = $5
			AND ps.fiscal_year = 2026 AND ps.period = $3`, s.ten.ID, code, n, status, s.de)
}

// TestSpecialPeriodPosting: a manual entry naming special period 13 and
// dated in the year's last regular period posts into it in every ledger
// its company keeps, and the trial balance shows it under period 13
// (ADR-0060 §3, T64m1).
func TestSpecialPeriodPosting(t *testing.T) {
	s := setupSpecial(t)
	je, _ := s.Must(201, "POST", "/v1/journal-entries", s.adjustment("2026-12-31", 13), "")
	if got := fmt.Sprint(je["fiscal_year"], "/", je["fiscal_period"]); got != "2026/13" {
		t.Errorf("the entry's period: got %s, want 2026/13", got)
	}
	copies, _ := s.Must(200, "GET", "/v1/journal-entries?legal_entity_id="+s.de.String()+"&ledger_id="+s.local+"&fiscal_period=13", "", "")
	if got := len(copies["items"].([]any)); got != 1 {
		t.Errorf("2L's copies in period 13: got %d, want 1", got)
	}
	balances, _ := s.Must(200, "GET", "/v1/balances?legal_entity_id="+s.de.String()+"&fiscal_year=2026&fiscal_period=13&account_id="+s.bank, "", "")
	items := balances["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("the bank's balances of period 13: got %d, want 1", len(items))
	}
	if got := apitest.Object(t, items[0].(map[string]any), "debit")["amount"]; got != "100.00" {
		t.Errorf("the bank's debit in period 13: got %v, want 100.00", got)
	}
}

// TestSpecialPeriodRefusals: a special period takes only postings dated
// in its dates, of a calendar that has it, without tax codes, open in
// every ledger posted to: each ledger's period closed in turn refuses it.
func TestSpecialPeriodRefusals(t *testing.T) {
	s := setupSpecial(t)
	fixture.TaxCodes(t, s.ten, document(t), config.TaxCode{Code: "V9", Description: "Input", RatePercent: "19", Account: "140000"})
	taxed := inPeriod(`{"legal_entity_id": "`+s.de.String()+`", "document_date": "2026-12-31", "posting_date": "2026-12-31", "currency": "EUR", "lines": [
		{"account_id": "`+s.bank+`", "amount": {"amount": "1.00", "currency": "EUR"}, "tax_code": "V9"},
		{"account_id": "`+s.revenue+`", "amount": {"amount": "-1.00", "currency": "EUR"}}]}`, 13)
	s.dropPeriod(t, 16) // company 2000's calendar keeps special periods 13 to 15 only
	for _, c := range []struct {
		name, body, code, field string
		status                  int
	}{
		{"dated in period 11", s.adjustment("2026-11-30", 13), "ledger.special_period_date", "posting_date", 422},
		{"dated in no fiscal year", s.adjustment("2030-12-31", 13), "ledger.no_fiscal_period", "posting_date", 422},
		{"a special period the calendar lacks", s.adjustment("2026-12-31", 16), "ledger.no_fiscal_period", "fiscal_period", 422},
		{"a tax code", taxed, "ledger.special_period_tax_code", "lines[0].tax_code", 422},
		{"a regular period named", s.adjustment("2026-12-31", 12), "request.invalid_body", "", 400},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := s.In(t).Problem(c.status, c.code, "POST", "/v1/journal-entries", c.body, "")
			if c.field == "" {
				return
			}
			if got := fmt.Sprint(apitest.List(t, p, "errors")[0].(map[string]any)["field"]); got != c.field {
				t.Errorf("the refused field: got %s, want %s", got, c.field)
			}
		})
	}
	for _, code := range []string{"0L", "2L"} {
		t.Run("period 13 closed in "+code, func(t *testing.T) {
			s.setPeriod(t, code, 13, "closed")
			defer s.setPeriod(t, code, 13, "open")
			s.In(t).Problem(409, "ledger.period_not_open", "POST", "/v1/journal-entries", s.adjustment("2026-12-31", 13), "")
		})
	}
	if n := s.journalEntries(t, 13); n != 0 {
		t.Errorf("entries in period 13 after the refusals: got %d, want 0", n)
	}
}

// journalEntries counts company 2000's entries of 2026 in period n, every
// ledger's.
func (s specialEnv) journalEntries(t *testing.T, n int) int {
	t.Helper()
	var count int
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1 AND legal_entity_id = $2
		AND fiscal_year = 2026 AND fiscal_period = $3`, s.ten.ID, s.de, n).Scan(&count); err != nil {
		t.Fatalf("count entries: %v", err)
	}
	return count
}

// TestParkedForASpecialPeriod: a parked entry keeps the special period it
// names, which PATCH changes and removes, is held to the rules when parked
// and changed, and posts into it, checked again when posted.
func TestParkedForASpecialPeriod(t *testing.T) {
	s := setupSpecial(t)
	s.Problem(422, "ledger.special_period_date", "POST", "/v1/parked-journal-entries", s.adjustment("2026-11-30", 13), "")
	pe, tag := s.Must(201, "POST", "/v1/parked-journal-entries", s.adjustment("2026-12-31", 13), "")
	path := "/v1/parked-journal-entries/" + pe["id"].(string)
	if got := pe["fiscal_period"]; got != float64(13) {
		t.Errorf("the parked entry's period: got %v, want 13", got)
	}
	pe, tag = s.Must(200, "PATCH", path, `{"fiscal_period": null}`, tag)
	if got, ok := pe["fiscal_period"]; ok {
		t.Errorf("the period after it was removed: got %v, want none", got)
	}
	pe, tag = s.Must(200, "PATCH", path, `{"fiscal_period": 14}`, tag)
	if got := pe["fiscal_period"]; got != float64(14) {
		t.Errorf("the period after the change: got %v, want 14", got)
	}
	s.Problem(422, "ledger.special_period_date", "PATCH", path, `{"posting_date": "2026-11-30"}`, tag)
	pe, tag = s.Must(200, "PATCH", path, `{"header_text": "Audit adjustment"}`, tag)
	if got := pe["fiscal_period"]; got != float64(14) {
		t.Errorf("the period after another change: got %v, want 14 kept", got)
	}
	s.setPeriod(t, "2L", 14, "closed")
	s.Problem(409, "ledger.period_not_open", "POST", path+":post", "", tag)
	s.setPeriod(t, "2L", 14, "open")
	posted, _ := s.Must(200, "POST", path+":post", "", tag)
	je, _ := s.Must(200, "GET", "/v1/journal-entries/"+posted["journal_entry_id"].(string), "", "")
	if got := je["fiscal_period"]; got != float64(14) {
		t.Errorf("the posted entry's period: got %v, want 14", got)
	}
	// Posting checks the entry again: one parked for period 15, which the
	// calendar loses after it was parked, is refused.
	pe, tag = s.Must(201, "POST", "/v1/parked-journal-entries", s.adjustment("2026-12-31", 15), "")
	s.dropPeriod(t, 15)
	s.Problem(422, "ledger.no_fiscal_period", "POST", "/v1/parked-journal-entries/"+pe["id"].(string)+":post", "", tag)
}

// dropPeriod takes company 2000's 2026 period n out of its calendar: a
// calendar with fewer special periods (0 to 4, ledger.fiscal_calendar).
func (s specialEnv) dropPeriod(t *testing.T, n int) {
	t.Helper()
	pgtest.Exec(t, `DELETE FROM ledger.period_status WHERE tenant_id = $1 AND legal_entity_id = $2 AND fiscal_year = 2026 AND period = $3`, s.ten.ID, s.de, n)
	pgtest.Exec(t, `DELETE FROM ledger.fiscal_period WHERE tenant_id = $1 AND calendar_id = $2 AND fiscal_year = 2026 AND period = $3`, s.ten.ID, s.deCal, n)
}

// TestReverseIntoASpecialPeriod: the API's reversal may name a special
// period, by a manual entry's rules, and reverses every copy of its
// posting group there; the contexts' reversals never do.
func TestReverseIntoASpecialPeriod(t *testing.T) {
	s := setupSpecial(t)
	fixture.TaxCodes(t, s.ten, document(t), config.TaxCode{Code: "V9", Description: "Input", RatePercent: "19", Account: "140000"})
	orig, tag := s.Must(201, "POST", "/v1/journal-entries", entry(s.de, "2026-12-15", "EUR", line{s.bank, "50.00", ""}, line{s.revenue, "-50.00", ""}), "")
	march, marchTag := s.Must(201, "POST", "/v1/journal-entries", entry(s.de, "2026-03-15", "EUR", line{s.bank, "5.00", ""}, line{s.revenue, "-5.00", ""}), "")
	taxed, taxedTag := s.Must(201, "POST", "/v1/journal-entries", `{"legal_entity_id": "`+s.de.String()+`", "document_date": "2026-12-15", "posting_date": "2026-12-15",
		"currency": "EUR", "lines": [{"account_id": "`+s.bank+`", "amount": {"amount": "1.00", "currency": "EUR"}, "tax_code": "V9"},
		{"account_id": "`+s.revenue+`", "amount": {"amount": "-1.00", "currency": "EUR"}}]}`, "")
	reverse := func(je map[string]any) string { return "/v1/journal-entries/" + je["id"].(string) + ":reverse" }
	s.Problem(422, "ledger.special_period_date", "POST", reverse(march), `{"fiscal_period": 13}`, marchTag)
	s.Problem(422, "ledger.special_period_tax_code", "POST", reverse(taxed), `{"posting_date": "2026-12-31", "fiscal_period": 13}`, taxedTag)
	rev, _ := s.Must(200, "POST", reverse(orig), `{"posting_date": "2026-12-31", "fiscal_period": 13}`, tag)
	if got := fmt.Sprint(rev["fiscal_year"], "/", rev["fiscal_period"], " ", rev["reversal_of_id"]); got != "2026/13 "+orig["id"].(string) {
		t.Errorf("the reversal: got %s, want 2026/13 reversing %s", got, orig["id"])
	}
	if n := s.journalEntries(t, 13); n != 2 {
		t.Errorf("entries in period 13, the reversal in 0L and 2L: got %d, want 2", n)
	}
	pgtest.Exec(t, `UPDATE tenancy.legal_entity SET status = 'inactive', version = version + 1 WHERE tenant_id = $1 AND id = $2`, s.ten.ID, s.de)
	s.Problem(422, "ledger.legal_entity_not_active", "POST", reverse(march), `{"posting_date": "2026-12-31", "fiscal_period": 13}`, marchTag)
}

// TestApprovedIntoASpecialPeriod: an entry parked for a special period
// and posted on its approval is checked again when it is approved: posted
// into the period, or refused once the calendar lost it, the request left
// pending.
func TestApprovedIntoASpecialPeriod(t *testing.T) {
	s := setupSpecial(t)
	m := manualEntries()
	submit := func(n int) (uuid.UUID, uuid.UUID) {
		t.Helper()
		pe, _ := s.Must(201, "POST", "/v1/parked-journal-entries", s.adjustment("2026-12-31", n), "")
		id, request := uuid.MustParse(pe["id"].(string)), uuid.New()
		h := endpoints()
		h.Approvals = opener{request, true}
		if err := apitest.Committed(t, s.ten, s.ten.Human, func(ctx context.Context) error {
			_, err := h.PostParkedJournalEntry(ctx, gen.PostParkedJournalEntryParams{ID: id, IfMatch: "*"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return id, request
	}
	approve := func(id, request uuid.UUID) error {
		return apitest.Committed(t, s.ten, s.ten.Human, func(ctx context.Context) error { return m.Approved(ctx, id, request) })
	}
	id, request := submit(13)
	if err := approve(id, request); err != nil {
		t.Fatalf("approved: %v", err)
	}
	if n := s.journalEntries(t, 13); n != 2 {
		t.Errorf("entries in period 13 after the approval, 0L's and 2L's: got %d, want 2", n)
	}
	id, request = submit(15)
	s.dropPeriod(t, 15)
	if got := problem.From(approve(id, request)).Code; got != "ledger.no_fiscal_period" {
		t.Errorf("approved after the calendar lost period 15: got %q, want ledger.no_fiscal_period", got)
	}
}

// TestCloseChecksLeaveSpecialOut: a period close's parked-entry check
// leaves out an entry parked for a special period, dated in the period it
// checks: it is the year close's (ADR-0060 §3, decision 14).
func TestCloseChecksLeaveSpecialOut(t *testing.T) {
	s := setupSpecial(t)
	regular, _ := s.Must(201, "POST", "/v1/parked-journal-entries", entry(s.de, "2026-12-15", "EUR", line{s.bank, "1.00", ""}, line{s.revenue, "-1.00", ""}), "")
	s.Must(201, "POST", "/v1/parked-journal-entries", s.adjustment("2026-12-31", 13), "")
	var n int
	var found []ledger.OpenParked
	if err := apitest.Committed(t, s.ten, s.ten.Human, func(ctx context.Context) (err error) {
		n, found, err = ledger.CloseChecks{}.Parked(ctx, s.de, date("2026-12-01"), date("2026-12-31"), []string{ledger.EntryParked}, 20)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n != 1 || len(found) != 1 {
		t.Fatalf("the parked entries of period 12: got %d (%d named), want 1, the regular one", n, len(found))
	}
	if got := found[0].ID.String(); got != regular["id"] {
		t.Errorf("the entry named: got %s, want the regular one %v", got, regular["id"])
	}
}

// TestSpecialPeriodDatabaseFailures: every statement of the special-period
// paths failing in turn fails the call.
func TestSpecialPeriodDatabaseFailures(t *testing.T) {
	s := setupSpecial(t)
	h := endpoints()
	u := uuid.MustParse
	create := gen.JournalEntryCreate{LegalEntityID: s.de, DocumentDate: date("2026-12-31"), PostingDate: date("2026-12-31"), Currency: "EUR",
		FiscalPeriod: gen.NewOptInt32(13), Lines: []gen.JournalLineInput{{AccountID: u(s.bank), Amount: gen.Money{Amount: "1.00", Currency: "EUR"}},
			{AccountID: u(s.revenue), Amount: gen.Money{Amount: "-1.00", Currency: "EUR"}}}}
	orig, _ := s.Must(201, "POST", "/v1/journal-entries", entry(s.de, "2026-12-15", "EUR", line{s.bank, "2.00", ""}, line{s.revenue, "-2.00", ""}), "")
	parked, _ := s.Must(201, "POST", "/v1/parked-journal-entries", s.adjustment("2026-12-31", 13), "")
	for name, call := range map[string]func(ctx context.Context) error{
		"post": func(ctx context.Context) error {
			_, err := h.PostJournalEntry(ctx, &create, gen.PostJournalEntryParams{})
			return err
		},
		"park": func(ctx context.Context) error {
			_, err := h.ParkJournalEntry(ctx, &create, gen.ParkJournalEntryParams{})
			return err
		},
		"change parked": func(ctx context.Context) error {
			_, err := h.UpdateParkedJournalEntry(ctx, &gen.ParkedJournalEntryUpdate{FiscalPeriod: gen.NewOptNilInt32(14)},
				gen.UpdateParkedJournalEntryParams{ID: u(parked["id"].(string)), IfMatch: "*"})
			return err
		},
		"reverse": func(ctx context.Context) error {
			_, err := h.ReverseJournalEntry(ctx, gen.NewOptJournalEntryReverse(gen.JournalEntryReverse{PostingDate: gen.NewOptDate(date("2026-12-31")),
				FiscalPeriod: gen.NewOptInt32(13)}), gen.ReverseJournalEntryParams{ID: u(orig["id"].(string)), IfMatch: "*"})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) { apitest.EveryFailure(t, s.ten, s.ten.Human, name, call) })
	}
}
