package leases_test

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// yearly is the lease worked by hand in TestScheduleByHand: three yearly
// payments of 100.00 in advance at 10 percent from 1 January 2026.
var yearly = lease{commencement: "2026-01-01", term: 36, amount: "100.00", frequency: "yearly", timing: "advance", rate: "10"}

// schedule reads the contract's schedule.
func (e env) schedule(t *testing.T, id string) map[string]any {
	t.Helper()
	got, _ := e.Must(200, "GET", contracts+"/"+id+"/schedule", "", "")
	return got
}

// scheduleLines is the schedule's lines as
// "no start-end due opening/payment/interest/closing depreciation carrying".
func scheduleLines(t *testing.T, s map[string]any) string {
	t.Helper()
	var out []string
	for _, l := range rows(t, s, "lines") {
		v := func(f string) any { return l[f].(map[string]any)["amount"] }
		out = append(out, fmt.Sprintf("%v %v-%v %v %v/%v/%v/%v %v %v", l["line_no"], l["period_start"], l["period_end"], l["payment_date"], v("opening_liability"),
			v("payment"), v("interest"), v("closing_liability"), v("depreciation"), v("right_of_use_closing")))
	}
	return strings.Join(out, ", ")
}

// TestScheduleRead (T88b; ADR-0073 §8): a draft's schedule is computed on
// read: the liability at commencement without the payment due on the
// commencement date, the right-of-use asset with it, one line per payment
// with its interest, closing liability and depreciation, and the totals.
func TestScheduleRead(t *testing.T) {
	e := setup(t)
	id := e.create(t, yearly)["id"].(string)
	s := e.schedule(t, id)
	fields(t, s, map[string]any{"lease_contract_id": id, "company_code": "2000", "company_name": "Acme GmbH", "status": "draft", "currency": "EUR",
		"commencement_date": "2026-01-01", "payment_timing": "advance", "payments_per_year": float64(1), "annual_discount_rate_percent": "10"})
	for f, want := range map[string]string{"lease_liability": "173.55 EUR", "commencement_payment": "100.00 EUR", "right_of_use_asset": "273.55 EUR"} {
		if got := amount(t, s, f); got != want {
			t.Errorf("%s: got %s, want %s", f, got, want)
		}
	}
	if got, want := scheduleLines(t, s), "1 2026-01-01-2026-12-31 2026-01-01 273.55/100.00/17.36/190.91 91.19 182.36, "+
		"2 2027-01-01-2027-12-31 2027-01-01 190.91/100.00/9.09/100.00 91.18 91.18, 3 2028-01-01-2028-12-31 2028-01-01 100.00/100.00/0.00/0.00 91.18 0.00"; got != want {
		t.Errorf("lines: got %s, want %s", got, want)
	}
	totals, _ := s["totals"].(map[string]any)
	for f, want := range map[string]string{"payments": "300.00 EUR", "interest": "26.45 EUR", "depreciation": "273.55 EUR"} {
		if got := amount(t, totals, f); got != want {
			t.Errorf("totals.%s: got %s, want %s", f, got, want)
		}
	}
	t.Run("in arrears", func(t *testing.T) {
		arrears := yearly
		arrears.timing = "arrears"
		s := e.schedule(t, e.create(t, arrears)["id"].(string))
		if got, want := fmt.Sprintf("%s %s %s", amount(t, s, "lease_liability"), amount(t, s, "commencement_payment"), amount(t, s, "right_of_use_asset")),
			"248.69 EUR 0.00 EUR 248.69 EUR"; got != want {
			t.Errorf("liability, commencement payment, right-of-use: got %s, want %s", got, want)
		}
	})
	t.Run("another company's", func(t *testing.T) {
		e.As(e.Reader(t, "uma", e.US)).Problem(404, "request.not_found", "GET", contracts+"/"+id+"/schedule", "", "")
	})
}

// entryLines are the lines of the journal entry as "account amount
// date", the amount in minor units, by account number.
func (e env) entryLines(t *testing.T, entry any) string {
	t.Helper()
	rows, err := pgtest.Admin(t).Query(t.Context(), `SELECT a.number || ' ' || l.amount_tc::text || ' ' || l.posting_date::text FROM ledger.journal_line l
		JOIN ledger.gl_account a ON a.id = l.account_id WHERE l.tenant_id = $1 AND l.journal_entry_id = $2 ORDER BY a.number`, e.Tenant.ID, entry)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return strings.Join(out, ", ")
}

// TestActivate (T88b; ADR-0073 §7): activating a draft posts the
// right-of-use asset against the lease liability on the commencement
// date, stores the schedule it showed, and fixes the contract: a second
// activation is refused, and a stale version too.
func TestActivate(t *testing.T) {
	e := setup(t)
	id := e.create(t, yearly)["id"].(string)
	before := e.schedule(t, id)
	got, tag := e.Must(200, "POST", contracts+"/"+id+":activate", "", `"1"`)
	if tag != `"2"` {
		t.Errorf("ETag: got %s, want \"2\"", tag)
	}
	fields(t, got, map[string]any{"status": "active", "version": float64(2), "journal_entry_year": float64(2026)})
	for f, want := range map[string]string{"lease_liability": "173.55 EUR", "right_of_use_asset": "273.55 EUR"} {
		if got := amount(t, got, f); got != want {
			t.Errorf("%s: got %s, want %s", f, got, want)
		}
	}
	if n, _ := got["journal_entry_number"].(string); n == "" {
		t.Error("journal_entry_number: got none, want the entry's")
	}
	if got, want := e.entryLines(t, got["journal_entry_id"]), "160000 27355 2026-01-01, 260000 -27355 2026-01-01"; got != want {
		t.Errorf("entry: got %s, want %s", got, want)
	}
	if n := e.count(t, `SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1 AND id = $2 AND source_context = 'leases'
		AND source_document_id = $3 AND document_type = 'SA'`, got["journal_entry_id"], id); n != 1 {
		t.Errorf("entries of the contract: got %d, want 1", n)
	}
	after := e.schedule(t, id)
	before["status"] = "active"
	if got, want := mustJSON(t, after), mustJSON(t, before); got != want {
		t.Errorf("stored schedule: got %s, want what the draft showed %s", got, want)
	}
	if n := e.count(t, `SELECT count(*) FROM leases.schedule_line WHERE tenant_id = $1 AND contract_id = $2`, id); n != 3 {
		t.Errorf("stored lines: got %d, want 3", n)
	}
	events := e.events(t, id)
	if len(events) != 2 {
		t.Fatalf("events: got %d, want 2", len(events))
	}
	fields(t, events[1], map[string]any{"status": "active", "status_before": "draft", "version": float64(2), "journal_entry_id": got["journal_entry_id"],
		"journal_entry_number": got["journal_entry_number"], "journal_entry_year": float64(2026)})
	if got := amount(t, events[1], "right_of_use_amount"); got != "273.55 EUR" {
		t.Errorf("event's right-of-use amount: got %s, want 273.55 EUR", got)
	}
	if n := e.count(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'leases.contract.activate' AND object_id = $2`, id); n != 1 {
		t.Errorf("audit records: got %d, want 1", n)
	}
	e.Problem(409, "leases.invalid_status", "POST", contracts+"/"+id+":activate", "", `"2"`)
	e.Problem(412, "request.version_mismatch", "POST", contracts+"/"+id+":activate", "", `"1"`)
	page, _ := e.Must(200, "GET", contracts+"?status=active", "", "")
	if items := rows(t, page, "items"); len(items) != 1 || amount(t, items[0], "right_of_use_asset") != "273.55 EUR" {
		t.Errorf("active contracts: got %v, want the one with its right-of-use asset", items)
	}
}

// TestActivateRefusals (T88b; ADR-0073 §7): a company without the lease
// accounts, a commencement date in a closed period or outside the fiscal
// years set up, and another company's contract are refused, and the
// contract stays a draft without a schedule.
func TestActivateRefusals(t *testing.T) {
	e := setup(t)
	closed := lease{"2026-03-01", 12, "10.00", "monthly", "advance", "5"}
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND fiscal_year = 2026 AND period = 3`,
		e.Tenant.ID)
	later := lease{"2031-01-01", 12, "10.00", "monthly", "advance", "5"}
	b := bare(t)
	cases := []struct {
		name         string
		e            env
		l            lease
		as           func(env) apitest.Client
		status       int
		code, detail string
	}{
		{"without the lease accounts", b, office, nil, 422, "leases.no_lease_accounts", "no right_of_use_asset and lease_liability account"},
		{"in a closed period", e, closed, nil, 409, "ledger.period_not_open", ""},
		{"outside the fiscal years", e, later, nil, 422, "ledger.no_fiscal_period", ""},
		{"of another company", e, office, func(e env) apitest.Client { return e.As(e.Reader(t, "uma", e.US)) }, 404, "request.not_found", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id := c.e.create(t, c.l)["id"].(string)
			client := c.e.In(t)
			if c.as != nil {
				client = c.as(c.e).In(t)
			}
			got := client.Problem(c.status, c.code, "POST", contracts+"/"+id+":activate", "", `"1"`)
			if detail, _ := got["detail"].(string); !strings.Contains(detail, c.detail) {
				t.Errorf("detail: got %q, want it to say %q", detail, c.detail)
			}
			read, _ := c.e.Must(200, "GET", contracts+"/"+id, "", "")
			fields(t, read, map[string]any{"status": "draft", "version": float64(1)})
			if n := c.e.count(t, `SELECT count(*) FROM leases.schedule_line WHERE tenant_id = $1 AND contract_id = $2`, id); n != 0 {
				t.Errorf("stored lines: got %d, want none", n)
			}
		})
	}
}

// TestActivateTwiceAtOnce (T88b; ADR-0008): of two activations of one
// contract at once, one activates it and posts and the other meets the
// version it moved (412); one entry is posted.
func TestActivateTwiceAtOnce(t *testing.T) {
	e := setup(t)
	id := e.create(t, yearly)["id"].(string)
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range codes {
		r := apitest.Req{Method: "POST", Path: contracts + "/" + id + ":activate", Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey(), IfMatch: `"1"`}
		wg.Go(func() { codes[i] = e.API.Do(r).Code })
	}
	wg.Wait()
	slices.Sort(codes)
	if !slices.Equal(codes, []int{200, 412}) {
		t.Errorf("statuses: got %v, want one 200 and one 412", codes)
	}
	if n := e.count(t, `SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1 AND source_context = 'leases' AND source_document_id = $2`, id); n != 1 {
		t.Errorf("entries: got %d, want 1", n)
	}
}
