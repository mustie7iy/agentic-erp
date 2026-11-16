package leases_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// active creates the lease and activates it, returning its id.
func (e env) active(t *testing.T, l lease) string {
	t.Helper()
	id := e.create(t, l)["id"].(string)
	e.Must(200, "POST", contracts+"/"+id+":activate", "", `"1"`)
	return id
}

// runBody is the JSON of a run of company 2000 for the period.
func (e env) runBody(year, period int) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "fiscal_year": %d, "fiscal_period": %d}`, e.DE, year, period)
}

// lineAmounts are the interest and depreciation of the schedule's lines
// from first to last, added up, in minor units.
func (e env) lineAmounts(t *testing.T, id string, first, last int) (interest, depreciation int64) {
	t.Helper()
	pgtest.Scan(t, `SELECT sum(interest)::bigint, sum(depreciation)::bigint FROM leases.schedule_line WHERE tenant_id = $1 AND contract_id = $2
		AND line_no BETWEEN $3 AND $4`, []any{e.Tenant.ID, id, first, last}, &interest, &depreciation)
	return interest, depreciation
}

// minor prints an amount in minor units as the API gives a EUR amount.
func minor(m int64) string { return money.Minor(m).Plain(2) + " EUR" }

// runEntry is the entry's lines as "account amount date center", by
// account number, center "-" for a line without one and "lease" for
// the fixture's CC-LEASE.
func (e env) runEntry(t *testing.T, entry any) string {
	t.Helper()
	var lines []string
	pgtest.Scan(t, `SELECT array_agg(a.number || ' ' || l.amount_tc::text || ' ' || l.posting_date::text || ' ' ||
			CASE WHEN l.cost_center_id IS NULL THEN '-' WHEN l.cost_center_id = $3 THEN 'lease' ELSE 'other' END ORDER BY a.number)
		FROM ledger.journal_line l JOIN ledger.gl_account a ON a.id = l.account_id WHERE l.tenant_id = $1 AND l.journal_entry_id = $2`,
		[]any{e.Tenant.ID, entry, e.center}, &lines)
	return strings.Join(lines, ", ")
}

// TestRunPosted (T88c; ADR-0073 §9): the run of a period posts, per
// active contract with a schedule line ending in the period, one entry
// dated the period's last day: interest expense against the liability,
// depreciation expense against accumulated depreciation, the expenses on
// the contract's cost center. A contract without such a line and a draft
// post nothing; the run is kept, audited and published, and read back.
func TestRunPosted(t *testing.T) {
	e := setup(t)
	monthly := e.active(t, office)
	e.active(t, yearly) // its first interval ends on 31 December
	e.create(t, office) // a draft
	got, _ := e.Must(201, "POST", runs, e.runBody(2026, 1), "")
	fields(t, got, map[string]any{"company_code": "2000", "company_name": "Acme GmbH", "fiscal_year": float64(2026), "fiscal_period": float64(1),
		"posting_date": "2026-01-31", "currency": "EUR", "contract_count": float64(2), "posting_count": float64(1)})
	interest, depreciation := e.lineAmounts(t, monthly, 1, 1)
	if got, want := amount(t, got, "interest_amount")+" "+amount(t, got, "depreciation_amount"), minor(interest)+" "+minor(depreciation); got != want {
		t.Errorf("run's interest and depreciation: got %s, want line 1's %s", got, want)
	}
	postings := rows(t, got, "postings")
	if len(postings) != 1 {
		t.Fatalf("postings: got %d, want 1", len(postings))
	}
	p := postings[0]
	fields(t, p, map[string]any{"lease_contract_id": monthly, "lessor_id": e.Supplier.String(), "cost_center_id": e.center.String(), "cost_center_code": "CC-LEASE",
		"first_line_no": float64(1), "last_line_no": float64(1), "journal_entry_year": float64(2026)})
	for _, field := range []string{"lessor_number", "lessor_name", "journal_entry_number"} {
		if p[field] == nil || p[field] == "" {
			t.Errorf("posting's %s: got %v, want one", field, p[field])
		}
	}
	want := fmt.Sprintf("161000 %d 2026-01-31 -, 260000 %d 2026-01-31 -, 672000 %d 2026-01-31 lease, 673000 %d 2026-01-31 lease",
		-depreciation, -interest, interest, depreciation)
	if got := e.runEntry(t, p["journal_entry_id"]); got != want {
		t.Errorf("entry: got %s, want %s", got, want)
	}
	if n := e.count(t, `SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1 AND id = $2 AND source_context = 'leases' AND document_type = 'SA'`,
		p["journal_entry_id"]); n != 1 {
		t.Errorf("the entry's source: got %d entries, want 1", n)
	}
	read, _ := e.Must(200, "GET", runs+"/"+got["id"].(string), "", "")
	if a, b := mustJSON(t, read), mustJSON(t, got); a != b {
		t.Errorf("run read back: got %s, want %s", a, b)
	}
	runEvents(t, e, got)
	if n := e.count(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'leases.run.post' AND object_id = $2`, got["id"]); n != 1 {
		t.Errorf("audit records: got %d, want 1", n)
	}
}

// runEvents checks the run's LeaseRunPosted against the run.
func runEvents(t *testing.T, e env, run map[string]any) {
	t.Helper()
	var events []map[string]any
	for _, p := range apitest.Published(t, e.Tenant, outbox.Event{Context: "leases", Name: "lease_run_posted"}) {
		if p["lease_run_id"] == run["id"] {
			events = append(events, p)
		}
	}
	if len(events) != 1 {
		t.Fatalf("events: got %d, want 1", len(events))
	}
	fields(t, events[0], map[string]any{"legal_entity_code": "2000", "fiscal_year": float64(2026), "fiscal_period": float64(1), "posting_date": "2026-01-31",
		"contract_count": float64(2), "posting_count": float64(1)})
	if got, want := amount(t, events[0], "interest_amount"), amount(t, run, "interest_amount"); got != want {
		t.Errorf("event's interest: got %s, want %s", got, want)
	}
}

// TestRunsInOrder (T88c; ADR-0073 §9): one run per company and period,
// each for a later period than the last; a later run catches up the
// lines of a period that was not run, each line posted once, and a
// replay answers the same run.
func TestRunsInOrder(t *testing.T) {
	e := setup(t)
	id := e.active(t, office)
	key := apitest.NewKey()
	first := e.API.Do(apitest.Req{Method: "POST", Path: runs, Body: e.runBody(2026, 1), Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: key})
	replay := e.API.Do(apitest.Req{Method: "POST", Path: runs, Body: e.runBody(2026, 1), Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: key})
	if first.Code != 201 || replay.Code != 201 || replay.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("replay: got %d and %d replayed %q, want 201 twice, replayed", first.Code, replay.Code, replay.Header().Get("Idempotent-Replayed"))
	}
	if a, b := mustJSON(t, apitest.JSON(t, first)), mustJSON(t, apitest.JSON(t, replay)); a != b {
		t.Errorf("replay: got %s, want the first answer %s", b, a)
	}
	e.Problem(409, "leases.run_exists", "POST", runs, e.runBody(2026, 1), "")
	later, _ := e.Must(201, "POST", runs, e.runBody(2026, 3), "")
	p := rows(t, later, "postings")
	if len(p) != 1 {
		t.Fatalf("postings of 2026/3: got %d, want 1", len(p))
	}
	interest, depreciation := e.lineAmounts(t, id, 2, 3)
	if got, want := fmt.Sprint(p[0]["first_line_no"], " ", p[0]["last_line_no"], " ", amount(t, p[0], "interest_amount"), " ", amount(t, p[0], "depreciation_amount")),
		fmt.Sprint(2, " ", 3, " ", minor(interest), " ", minor(depreciation)); got != want {
		t.Errorf("2026/3 catching up 2026/2: got %s, want %s", got, want)
	}
	e.Problem(409, "leases.run_period_not_later", "POST", runs, e.runBody(2026, 2), "")
	fourth, _ := e.Must(201, "POST", runs, e.runBody(2026, 4), "")
	if n := e.count(t, `SELECT count(*) FROM leases.run_posting WHERE tenant_id = $1 AND contract_id = $2`, id); n != 3 {
		t.Errorf("postings of the contract after 2026/4: got %d, want 3", n)
	}
	page, _ := e.Must(200, "GET", runs+"?legal_entity_id="+e.DE.String()+"&fiscal_year=2026", "", "")
	var periods []string
	for _, r := range rows(t, page, "items") {
		periods = append(periods, fmt.Sprint(r["fiscal_period"], ":", r["posting_count"], ":", r["postings"]))
	}
	if got, want := strings.Join(periods, " "), "4:1:<nil> 3:1:<nil> 1:1:<nil>"; got != want {
		t.Errorf("runs listed: got %s, want %s", got, want)
	}
	first2, _ := e.Must(200, "GET", runs+"?page_size=2", "", "")
	next, _ := first2["next_page_token"].(string)
	rest, _ := e.Must(200, "GET", runs+"?page_size=2&page_token="+next, "", "")
	if items := rows(t, rest, "items"); next == "" || len(items) != 1 || items[0]["fiscal_period"] != float64(1) {
		t.Errorf("second page: got %v after token %q, want the run of 2026/1", items, next)
	}
	if p := rows(t, fourth, "postings"); len(p) != 1 || p[0]["first_line_no"] != float64(4) {
		t.Errorf("2026/4: got %v, want line 4 posted", fourth)
	}
}

// TestRunNothingDue (T88c): a run whose contracts have no line ending in
// the period posts nothing and is kept with zero amounts.
func TestRunNothingDue(t *testing.T) {
	e := setup(t)
	e.active(t, yearly)
	got, _ := e.Must(201, "POST", runs, e.runBody(2026, 1), "")
	fields(t, got, map[string]any{"contract_count": float64(1), "posting_count": float64(0), "postings": nil})
	if a := amount(t, got, "interest_amount") + " " + amount(t, got, "depreciation_amount"); a != "0.00 EUR 0.00 EUR" {
		t.Errorf("amounts: got %s, want 0.00 EUR 0.00 EUR", a)
	}
}

// TestRunRefusals (T88c; ADR-0073 §9): a company the caller may not act
// in, a period that is no regular one or of a year not set up, a company
// without the run's accounts and a closed period are refused, and no run
// is kept.
func TestRunRefusals(t *testing.T) {
	e := setup(t)
	e.active(t, office)
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND fiscal_year = 2026 AND period = 5`,
		e.Tenant.ID)
	partial := withAccounts(t, false)
	cases := []struct {
		name         string
		client       apitest.Client
		body         string
		status       int
		code, detail string
	}{
		{"another company", e.As(e.Reader(t, "uma", e.US)), e.runBody(2026, 1), 422, "request.reference_not_found", ""},
		{"a special period", e.Client, e.runBody(2026, 13), 422, "ledger.period_not_regular", ""},
		{"a year not set up", e.Client, e.runBody(2031, 1), 422, "ledger.no_fiscal_period", ""},
		{"a closed period", e.Client, e.runBody(2026, 5), 409, "ledger.period_not_open", ""},
		{"without the run's accounts", partial.Client, partial.runBody(2026, 1), 422, "leases.no_lease_accounts",
			"no lease_interest and right_of_use_depreciation and right_of_use_accumulated_depreciation account"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.client.In(t).Problem(c.status, c.code, "POST", runs, c.body, "")
			if detail, _ := got["detail"].(string); !strings.Contains(detail, c.detail) {
				t.Errorf("detail: got %q, want it to say %q", detail, c.detail)
			}
		})
	}
	if n := e.count(t, `SELECT count(*) FROM leases.run WHERE tenant_id = $1`); n != 0 {
		t.Errorf("runs kept: got %d, want none", n)
	}
	t.Run("another company's run", func(t *testing.T) {
		run, _ := e.Must(201, "POST", runs, e.runBody(2026, 1), "")
		uma := e.As(e.Reader(t, "uma2", e.US)).In(t)
		uma.Problem(404, "request.not_found", "GET", runs+"/"+run["id"].(string), "", "")
		page, _ := uma.Must(200, "GET", runs, "", "")
		if items := rows(t, page, "items"); len(items) != 0 {
			t.Errorf("runs listed to another company's reader: got %d, want none", len(items))
		}
	})
}

// TestRunTooLarge (T88c): a company with more active contracts than the
// run's bound is refused, naming the bound.
func TestRunTooLarge(t *testing.T) {
	e := setup(t)
	e.active(t, office)
	e.active(t, yearly)
	h := endpoints(t)
	h.MaxRunContracts = 1
	err := e.inRequest(t, func(ctx context.Context) error {
		_, err := h.PostRun(ctx, e.DE, 2026, 1)
		return err
	})
	if p := problem.From(err); p.Code != "leases.run_too_large" || !strings.Contains(p.Detail, "more than 1 active") {
		t.Errorf("got %v, want leases.run_too_large naming the bound", err)
	}
}

// TestRunAmountTooLarge (T88c): a run whose contracts' interest adds up
// beyond an amount is refused, and nothing is kept.
func TestRunAmountTooLarge(t *testing.T) {
	e := setup(t)
	for range 2 {
		id := e.active(t, office)
		pgtest.Exec(t, `UPDATE leases.schedule_line SET interest = interest + 5000000000000000000, closing_liability = closing_liability + 5000000000000000000
			WHERE tenant_id = $1 AND contract_id = $2 AND line_no = 1`, e.Tenant.ID, id)
	}
	e.Problem(422, "leases.amount_too_large", "POST", runs, e.runBody(2026, 1), "")
	if n := e.count(t, `SELECT count(*) FROM leases.run WHERE tenant_id = $1`); n != 0 {
		t.Errorf("runs kept: got %d, want none", n)
	}
}

// TestRunConstraintProblems (T88c): the run's database rules answer as
// the run's own checks do.
func TestRunConstraintProblems(t *testing.T) {
	for constraint, code := range map[string]string{"lease_run_once": "leases.run_exists", "lease_run_in_order": "leases.run_period_not_later"} {
		t.Run(constraint, func(t *testing.T) {
			if got := problem.From(&pgconn.PgError{Code: "23514", ConstraintName: constraint}).Code; got != code {
				t.Errorf("got %s, want %s", got, code)
			}
		})
	}
}

// TestRunTwiceAtOnce (T88c; ADR-0008): of two runs of one company and
// period at once, one posts and the other finds it (409); each line is
// posted once.
func TestRunTwiceAtOnce(t *testing.T) {
	e := setup(t)
	id := e.active(t, office)
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range codes {
		r := apitest.Req{Method: "POST", Path: runs, Body: e.runBody(2026, 1), Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()}
		wg.Go(func() { codes[i] = e.API.Do(r).Code })
	}
	wg.Wait()
	slices.Sort(codes)
	if !slices.Equal(codes, []int{201, 409}) {
		t.Errorf("statuses: got %v, want one 201 and one 409", codes)
	}
	if n := e.count(t, `SELECT count(*) FROM leases.run_posting WHERE tenant_id = $1 AND contract_id = $2`, id); n != 1 {
		t.Errorf("postings: got %d, want 1", n)
	}
}

// TestRunDatabaseFailures calls the run's handlers outside a unit of work
// and with each of their database statements failing in turn.
func TestRunDatabaseFailures(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	e.active(t, office)
	run, _ := e.Must(201, "POST", runs, e.runBody(2026, 1), "")
	id := uuid.MustParse(run["id"].(string))
	calls := map[string]func(ctx context.Context) error{
		"post": func(ctx context.Context) error {
			_, err := h.CreateLeaseRun(ctx, &gen.LeaseRunCreate{LegalEntityID: e.DE, FiscalYear: 2026, FiscalPeriod: 2}, gen.CreateLeaseRunParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetLeaseRun(ctx, gen.GetLeaseRunParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListLeaseRuns(ctx, gen.ListLeaseRunsParams{})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call)
		})
	}
	t.Run("list page size", func(t *testing.T) {
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := h.ListLeaseRuns(ctx, gen.ListLeaseRunsParams{PageSize: gen.NewOptInt32(0)})
			return err
		})
		if p := problem.From(err); p.Code != "request.invalid_page_size" {
			t.Errorf("got %v, want request.invalid_page_size", p)
		}
	})
}
