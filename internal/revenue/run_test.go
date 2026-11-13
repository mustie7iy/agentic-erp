package revenue_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

const runs = "/v1/revenue-recognition-runs"

// runOf is the request for a run of the company and the period of 2026.
func runOf(company uuid.UUID, year, period int) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, "fiscal_year": %d, "fiscal_period": %d}`, company, year, period)
}

// ran runs revenue recognition for company 2000 and the period of 2026
// and returns the run.
func (e env) ran(t *testing.T, period int) map[string]any {
	t.Helper()
	got, _ := e.Must(201, "POST", runs, runOf(e.DE, 2026, period), "")
	return got
}

// serviceItem is the JSON of a year's support at 1200.00 EUR on the
// service revenue account, 410000.
func (e env) serviceItem() string {
	return strings.Replace(e.support(), e.account.String(), e.service.String(), 1)
}

// billed bills what is delivered of the order and not yet billed, dated
// and posted on the day.
func (e env) billed(t *testing.T, order, day string) {
	t.Helper()
	doc, tag := e.Must(201, "POST", "/v1/billing-documents", fmt.Sprintf(`{"sales_order_id": %q, "billing_date": %q}`, order, day), "")
	e.Must(200, "POST", "/v1/billing-documents/"+doc["id"].(string)+":post", fmt.Sprintf(`{"posting_date": %q}`, day), tag)
}

// active creates the contract of a confirmed order of the items and
// activates it with the prices it starts with; it returns the contract's
// and the order's ids.
func (e env) active(t *testing.T, items ...string) (id, order string) {
	t.Helper()
	id, order = e.contract(t, items...)
	e.activated(t, id)
	return id, order
}

// entryLines are the lines of the journal entry in the leading ledger,
// by line number: "account amount", in minor units, debit positive.
func (e env) entryLines(t *testing.T, entry any) string {
	t.Helper()
	var out string
	pgtest.Scan(t, `SELECT coalesce(string_agg(format('%s %s', a.number, l.amount_lc), '; ' ORDER BY l.line_no), '')
		FROM ledger.journal_line l JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id
		WHERE l.tenant_id = $1 AND l.journal_entry_id = $2`, []any{e.Tenant.ID, entry}, &out)
	return out
}

// balance is the account's balance in company 2000's leading ledger, in
// minor units, debit positive.
func (e env) balance(t *testing.T, number string) int {
	t.Helper()
	return e.count(t, `SELECT coalesce(sum(l.amount_lc), 0)::int FROM ledger.journal_line l
		JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id
		JOIN ledger.ledger g ON g.tenant_id = l.tenant_id AND g.id = l.ledger_id AND g.is_leading
		WHERE l.tenant_id = $1 AND l.legal_entity_id = $2 AND a.number = $3`, e.DE, number)
}

// revenueEntries counts the journal entries the revenue context posted.
func (e env) revenueEntries(t *testing.T) int {
	t.Helper()
	return e.count(t, `SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1 AND source_context = 'revenue'`)
}

// postings are the run's postings.
func postings(t *testing.T, run map[string]any) []map[string]any {
	t.Helper()
	raw, _ := run["postings"].([]any)
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		out[i], _ = r.(map[string]any)
	}
	return out
}

// totals writes a run's or a posting's amounts as "recognized R, billed
// B, asset A, liability L".
func totals(t *testing.T, m map[string]any) string {
	t.Helper()
	return fmt.Sprintf("recognized %s, billed %s, asset %s, liability %s", amount(t, m, "recognized_amount"), amount(t, m, "billed_amount"),
		amount(t, m, "contract_asset_amount"), amount(t, m, "contract_liability_amount"))
}

// moved writes a posting's obligations as "number account: recognized −
// billed, revenue", joined by "; ".
func moved(t *testing.T, posting map[string]any) string {
	t.Helper()
	var out []string
	for _, o := range obligations(t, posting) {
		out = append(out, fmt.Sprintf("%v %v: %s - %s, %s", o["obligation_no"], o["account_number"], amount(t, o, "recognized_amount"),
			amount(t, o, "billed_amount"), amount(t, o, "revenue_amount")))
	}
	return strings.Join(out, "; ")
}

// TestRecognitionRunAccrual: what is delivered and not billed at a
// period's end is accrued as a contract asset against revenue; once it is
// billed the next run takes the asset off, so revenue is what was
// recognized, once; a contract whose figures did not move posts nothing;
// each entry is in every ledger the company keeps; runs are audited,
// published and read back (T81d acceptance).
func TestRecognitionRunAccrual(t *testing.T) {
	e := setup(t)
	id, order := e.active(t, e.goods())
	e.delivered(t, order, 1, "50", "2026-01-15")

	january := e.ran(t, 1)
	t.Run("delivered and not billed: a contract asset", func(t *testing.T) {
		fields(t, january, map[string]any{"legal_entity_id": e.DE.String(), "company_code": "2000", "fiscal_year": float64(2026), "fiscal_period": float64(1),
			"posting_date": "2026-01-31", "currency": "EUR", "contract_count": float64(1), "posting_count": float64(1), "created_by": e.Tenant.Human.ID.String()})
		if got, want := totals(t, january), "recognized 625.00 EUR, billed 0.00 EUR, asset 625.00 EUR, liability 0.00 EUR"; got != want {
			t.Errorf("the run's totals: got %s, want %s", got, want)
		}
		if got, want := amount(t, january, "revenue_amount"), "625.00 EUR"; got != want {
			t.Errorf("what the run moved revenue by: got %s, want %s", got, want)
		}
		ps := postings(t, january)
		if len(ps) != 1 {
			t.Fatalf("postings: got %d, want 1", len(ps))
		}
		fields(t, ps[0], map[string]any{"revenue_contract_id": id, "sales_order_id": order, "customer_number": "BP70000001", "customer_name": "Northwind Traders",
			"journal_entry_year": float64(2026)})
		if got, want := totals(t, ps[0]), "recognized 625.00 EUR, billed 0.00 EUR, asset 625.00 EUR, liability 0.00 EUR"; got != want {
			t.Errorf("the posting's amounts: got %s, want %s", got, want)
		}
		if got, want := moved(t, ps[0]), "1 400000: 625.00 EUR - 0.00 EUR, 625.00 EUR"; got != want {
			t.Errorf("the posting's obligations: got %s, want %s", got, want)
		}
		if got, want := e.entryLines(t, ps[0]["journal_entry_id"]), "400000 -62500; 125000 62500"; got != want {
			t.Errorf("the entry's lines: got %s, want %s", got, want)
		}
		var kept, posted int
		pgtest.Scan(t, `SELECT (SELECT count(*) FROM ledger.company_ledger WHERE tenant_id = $1 AND legal_entity_id = $2),
			(SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1 AND source_context = 'revenue' AND document_type = 'SA' AND posting_date = '2026-01-31'
				AND reference = $3)`, []any{e.Tenant.ID, e.DE, ps[0]["sales_order_number"]}, &kept, &posted)
		if posted != kept || kept == 0 {
			t.Errorf("the contract's entries: got %d, want %d, one in every ledger the company keeps", posted, kept)
		}
	})

	e.billed(t, order, "2026-02-10")
	february := e.ran(t, 2)
	t.Run("billed: the asset is taken off against revenue", func(t *testing.T) {
		if got, want := totals(t, february), "recognized 625.00 EUR, billed 625.00 EUR, asset 0.00 EUR, liability 0.00 EUR"; got != want {
			t.Errorf("the run's totals: got %s, want %s", got, want)
		}
		if got, want := amount(t, february, "revenue_amount"), "-625.00 EUR"; got != want {
			t.Errorf("what the run moved revenue by: got %s, want %s", got, want)
		}
		ps := postings(t, february)
		if len(ps) != 1 {
			t.Fatalf("postings: got %d, want 1", len(ps))
		}
		if got, want := moved(t, ps[0]), "1 400000: 625.00 EUR - 625.00 EUR, -625.00 EUR"; got != want {
			t.Errorf("the posting's obligations: got %s, want %s", got, want)
		}
		if got, want := e.entryLines(t, ps[0]["journal_entry_id"]), "400000 62500; 125000 -62500"; got != want {
			t.Errorf("the entry's lines: got %s, want %s", got, want)
		}
		if got := e.balance(t, "125000"); got != 0 {
			t.Errorf("contract assets: got %d, want 0", got)
		}
		if got := e.balance(t, "400000"); got != -62500 {
			t.Errorf("revenue: got %d, want -62500, what was delivered, once", got)
		}
	})

	entries := e.revenueEntries(t)
	march := e.ran(t, 3)
	t.Run("nothing moved: nothing posted", func(t *testing.T) {
		fields(t, march, map[string]any{"contract_count": float64(1), "posting_count": float64(0), "posting_date": "2026-03-31"})
		if got := len(postings(t, march)); got != 0 {
			t.Errorf("postings: got %d, want none", got)
		}
		if got, want := totals(t, march), "recognized 625.00 EUR, billed 625.00 EUR, asset 0.00 EUR, liability 0.00 EUR"; got != want {
			t.Errorf("the run's totals: got %s, want %s", got, want)
		}
		if got, want := amount(t, march, "revenue_amount"), "0.00 EUR"; got != want {
			t.Errorf("what the run moved revenue by: got %s, want %s", got, want)
		}
		if got := e.revenueEntries(t); got != entries {
			t.Errorf("the revenue context's entries: got %d, want %d as before", got, entries)
		}
	})
	t.Run("read back", func(t *testing.T) {
		got, _ := e.Must(200, "GET", runs+"/"+january["id"].(string), "", "")
		if a, b := mustJSON(t, got), mustJSON(t, january); a != b {
			t.Errorf("the run read back: got %s, want the run as it answered %s", a, b)
		}
		e.Must(404, "GET", runs+"/"+uuid.NewString(), "", "")
		list, _ := e.Must(200, "GET", runs+"?legal_entity_id="+e.DE.String()+"&fiscal_year=2026", "", "")
		var periods []any
		for _, r := range apitest.List(t, list, "items") {
			run, _ := r.(map[string]any)
			periods = append(periods, run["fiscal_period"])
			if _, ok := run["postings"]; ok {
				t.Errorf("run %v in the list: got its postings, want none", run["fiscal_period"])
			}
		}
		if want := []any{float64(3), float64(2), float64(1)}; !slices.Equal(periods, want) {
			t.Errorf("the runs listed: got %v, want %v, the latest first", periods, want)
		}
		first, _ := e.Must(200, "GET", runs+"?page_size=2", "", "")
		token, _ := first["next_page_token"].(string)
		if token == "" || len(apitest.List(t, first, "items")) != 2 {
			t.Fatalf("the first page of two: got %v, want two runs and a token for the next", first)
		}
		next, _ := e.Must(200, "GET", runs+"?page_size=2&page_token="+token, "", "")
		if items := apitest.List(t, next, "items"); len(items) != 1 || items[0].(map[string]any)["id"] != january["id"] {
			t.Errorf("the second page: got %v, want January's run alone", items)
		}
		one, _ := e.Must(200, "GET", runs+"?fiscal_period=2&page_size=1", "", "")
		if items := apitest.List(t, one, "items"); len(items) != 1 || items[0].(map[string]any)["id"] != february["id"] {
			t.Errorf("the runs of period 2: got %v, want February's run alone", items)
		}
	})
	t.Run("audited and published", func(t *testing.T) {
		if n := e.count(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'revenue.recognition_run.create'
			AND object_type = 'revenue.recognition_run' AND detail->>'posting_count' = '1'`); n != 2 {
			t.Errorf("audit records of runs that posted: got %d, want 2", n)
		}
		events := apitest.Published(t, e.Tenant, outbox.Event{Context: "revenue", Name: "revenue_recognized"})
		var posted []any
		for _, ev := range events {
			posted = append(posted, ev["posting_count"])
		}
		if want := []any{float64(1), float64(1), float64(0)}; !slices.Equal(posted, want) {
			t.Errorf("RevenueRecognized events' posting counts: got %v, want %v", posted, want)
		}
		fields(t, events[0], map[string]any{"revenue_recognition_run_id": january["id"], "legal_entity_code": "2000", "posting_date": "2026-01-31"})
	})
}

// TestRecognitionRunPosition: a contract billed ahead of what is
// recognized stands as a contract liability; when more is recognized
// than billed the next run takes the liability off and puts a contract
// asset on, so the contract never shows both; a billing document dated
// into a period already run is caught up by the next run (T81d
// acceptance).
func TestRecognitionRunPosition(t *testing.T) {
	e := setup(t)
	id, order := e.contract(t, e.goods(), e.serviceItem())
	e.Must(200, "PATCH", contracts+"/"+id, set(overTime(2, "2026-01-01", "2026-12-31", "1200.00")), `"1"`)
	e.activated(t, id)
	e.delivered(t, order, 2, "1", "2026-01-05")
	e.billed(t, order, "2026-01-10")

	t.Run("billed ahead: a contract liability", func(t *testing.T) {
		ps := postings(t, e.ran(t, 1))
		if len(ps) != 1 {
			t.Fatalf("postings: got %d, want 1", len(ps))
		}
		// January's 31 days of 365 are 101.92 of the support's 1200.00, which is billed in full.
		if got, want := totals(t, ps[0]), "recognized 101.92 EUR, billed 1200.00 EUR, asset 0.00 EUR, liability 1098.08 EUR"; got != want {
			t.Errorf("the posting's amounts: got %s, want %s", got, want)
		}
		if got, want := moved(t, ps[0]), "1 400000: 0.00 EUR - 0.00 EUR, 0.00 EUR; 2 410000: 101.92 EUR - 1200.00 EUR, -1098.08 EUR"; got != want {
			t.Errorf("the posting's obligations: got %s, want %s", got, want)
		}
		if got, want := e.entryLines(t, ps[0]["journal_entry_id"]), "410000 109808; 245000 -109808"; got != want {
			t.Errorf("the entry's lines: got %s, want %s", got, want)
		}
		if got := e.balance(t, "410000"); got != -10192 {
			t.Errorf("service revenue: got %d, want -10192, January's share", got)
		}
	})

	e.delivered(t, order, 1, "120", "2026-02-20")
	// A second contract, delivered in part and not billed, sorted after the first by its order's number.
	_, second := e.active(t, e.goods())
	e.delivered(t, second, 1, "10", "2026-02-21")
	t.Run("more recognized than billed: the liability off, an asset on", func(t *testing.T) {
		run := e.ran(t, 2)
		// A second contract moves in the run too: the run's movement of revenue is both contracts'.
		if got, want := amount(t, run, "revenue_amount"), "1717.05 EUR"; got != want {
			t.Errorf("what the run moved revenue by: got %s, want %s", got, want)
		}
		ps := postings(t, run)
		if len(ps) != 2 {
			t.Fatalf("postings: got %d, want 2", len(ps))
		}
		if got, want := totals(t, ps[0]), "recognized 1693.97 EUR, billed 1200.00 EUR, asset 493.97 EUR, liability 0.00 EUR"; got != want {
			t.Errorf("the posting's amounts: got %s, want %s", got, want)
		}
		if got, want := amount(t, ps[0], "revenue_amount"), "1592.05 EUR"; got != want {
			t.Errorf("the entry's movement of revenue: got %s, want %s", got, want)
		}
		if got, want := e.entryLines(t, ps[0]["journal_entry_id"]), "400000 -150000; 410000 -9205; 125000 49397; 245000 109808"; got != want {
			t.Errorf("the entry's lines: got %s, want %s", got, want)
		}
		if got, want := amount(t, ps[0], "contract_asset_amount")+" "+amount(t, ps[0], "contract_liability_amount"), "493.97 EUR 0.00 EUR"; got != want {
			t.Errorf("the contract's position: got %s, want %s, never both", got, want)
		}
		if got := e.balance(t, "245000"); got != 0 {
			t.Errorf("contract liabilities: got %d, want 0, taken off", got)
		}
	})

	// Dated into February, which is run: March's run catches it up.
	e.billed(t, order, "2026-02-25")
	t.Run("billing dated into a period already run", func(t *testing.T) {
		run := e.ran(t, 3)
		if got, want := amount(t, run, "revenue_amount"), "-1398.08 EUR"; got != want {
			t.Errorf("what the run moved revenue by: got %s, want %s", got, want)
		}
		ps := postings(t, run)
		if len(ps) != 1 {
			t.Fatalf("postings: got %d, want 1", len(ps))
		}
		if got, want := totals(t, ps[0]), "recognized 1795.89 EUR, billed 2700.00 EUR, asset 0.00 EUR, liability 904.11 EUR"; got != want {
			t.Errorf("the posting's amounts: got %s, want %s", got, want)
		}
		if got, want := e.entryLines(t, ps[0]["journal_entry_id"]), "400000 150000; 410000 -10192; 125000 -49397; 245000 -90411"; got != want {
			t.Errorf("the entry's lines: got %s, want %s", got, want)
		}
		// The second contract's 125.00 stays; the first's asset is taken off.
		if got := e.balance(t, "125000"); got != 12500 {
			t.Errorf("contract assets: got %d, want 12500, the second contract's alone", got)
		}
		if got := e.balance(t, "245000"); got != -90411 {
			t.Errorf("contract liabilities: got %d, want -90411", got)
		}
		// Billing credited 2700.00; the runs leave what is recognized: the goods' 1500.00 and the second contract's 125.00, and three months of support.
		if got := e.balance(t, "400000"); got != -162500 {
			t.Errorf("revenue of goods: got %d, want -162500", got)
		}
		if got := e.balance(t, "410000"); got != -29589 {
			t.Errorf("revenue of support: got %d, want -29589", got)
		}
	})
}

// TestRecognitionRunRefusals: a run is refused for a company the caller
// may not act in, a period that is no regular period, a fiscal year not
// set up, a company without its two accounts, a period that is not open,
// a period already run and a period before the last run; nothing is kept
// or posted (T81d acceptance).
func TestRecognitionRunRefusals(t *testing.T) {
	e := setup(t)
	_, order := e.active(t, e.goods())
	e.delivered(t, order, 1, "50", "2026-01-15")
	page, _ := e.Must(200, "GET", fmt.Sprintf("/v1/fiscal-periods?legal_entity_id=%s&fiscal_year=2026&order_by=period&page_size=4", e.DE), "", "")
	april := page["items"].([]any)[3].(map[string]any)
	e.Must(200, "POST", "/v1/fiscal-periods/"+april["id"].(string)+":close", "", fmt.Sprintf(`"%v"`, april["version"]))
	cases := []struct {
		name, body  string
		status      int
		code, field string
	}{
		{"a company that is not there", runOf(uuid.New(), 2026, 1), 422, "request.reference_not_found", "legal_entity_id"},
		{"a period that is no regular period", runOf(e.DE, 2026, 13), 422, "revenue.period_not_regular", "fiscal_period"},
		{"a fiscal year that is not set up", runOf(e.DE, 2031, 1), 422, "ledger.no_fiscal_period", ""},
		{"a company without its contract accounts", runOf(e.US, 2026, 1), 422, "revenue.no_contract_accounts", ""},
		{"a period that is not open", runOf(e.DE, 2026, 4), 409, "ledger.period_not_open", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e.In(t).Problem(c.status, c.code, "POST", runs, c.body, "")
			if c.field != "" && field(got) != c.field {
				t.Errorf("the field refused: got %v, want %s", field(got), c.field)
			}
		})
	}
	if n := e.count(t, `SELECT count(*) FROM revenue.recognition_run WHERE tenant_id = $1`) + e.revenueEntries(t); n != 0 {
		t.Fatalf("runs and entries after the refusals: got %d, want none", n)
	}
	e.ran(t, 2)
	entries := e.revenueEntries(t)
	t.Run("a period already run", func(t *testing.T) {
		e.In(t).Problem(409, "revenue.recognition_run_exists", "POST", runs, runOf(e.DE, 2026, 2), "")
	})
	t.Run("a period before the last run", func(t *testing.T) {
		got := e.In(t).Problem(409, "revenue.recognition_period_not_later", "POST", runs, runOf(e.DE, 2026, 1), "")
		if detail, _ := got["detail"].(string); !strings.Contains(detail, "2026/2, to 2026-02-28") {
			t.Errorf("detail: got %q, want it to name the last run, 2026/2, to 2026-02-28", detail)
		}
	})
	stranger := e.As(e.Reader(t, "US accountant", e.US))
	t.Run("another company's run", func(t *testing.T) {
		list, _ := e.Must(200, "GET", runs, "", "")
		id := apitest.List(t, list, "items")[0].(map[string]any)["id"].(string)
		stranger.In(t).Problem(404, "request.not_found", "GET", runs+"/"+id, "", "")
		theirs, _ := stranger.In(t).Must(200, "GET", runs, "", "")
		if n := len(apitest.List(t, theirs, "items")); n != 0 {
			t.Errorf("the runs listed for another company's accountant: got %d, want none", n)
		}
	})
	if n := e.count(t, `SELECT count(*) FROM revenue.recognition_run WHERE tenant_id = $1`); n != 1 || e.revenueEntries(t) != entries {
		t.Errorf("runs: got %d, want 1 and no entry more", n)
	}
	t.Run("a replay answers the run", func(t *testing.T) {
		req := apitest.Req{Method: "POST", Path: runs, Body: runOf(e.DE, 2026, 3), Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()}
		first, second := e.API.Do(req), e.API.Do(req)
		if first.Code != 201 || second.Code != 201 {
			t.Fatalf("statuses: got %d and %d, want 201 twice", first.Code, second.Code)
		}
		if a, b := mustJSON(t, apitest.JSON(t, second)), mustJSON(t, apitest.JSON(t, first)); a != b {
			t.Errorf("replay: got %s, want the first answer %s", a, b)
		}
		if n := e.count(t, `SELECT count(*) FROM revenue.recognition_run WHERE tenant_id = $1 AND fiscal_period = 3`); n != 1 {
			t.Errorf("runs of period 3: got %d, want 1", n)
		}
	})
}

// TestRecognitionRunSchedule: a contract whose schedule needs a day in
// no fiscal year of the company refuses the run, naming its order and
// the obligation; nothing is kept.
func TestRecognitionRunSchedule(t *testing.T) {
	e := setup(t)
	id, order := e.contract(t, e.goods(), e.support())
	e.Must(200, "PATCH", contracts+"/"+id, set(overTime(2, "2026-07-01", "2027-06-30", "1200.00")), `"1"`)
	e.activated(t, id)
	number := e.contractOf(t, order)["sales_order_number"].(string)
	got := e.Problem(422, "revenue.no_fiscal_period", "POST", runs, runOf(e.DE, 2026, 7), "")
	want := "Revenue contract of sales order " + number + ": Obligation 2 (Support for the fiscal year): a day of its service period, 2027-01-01, lies in no fiscal year"
	if detail, _ := got["detail"].(string); !strings.HasPrefix(detail, want) {
		t.Errorf("detail: got %q, want it to start with %q", detail, want)
	}
	if n := e.count(t, `SELECT count(*) FROM revenue.recognition_run WHERE tenant_id = $1`); n != 0 {
		t.Errorf("runs: got %d, want none", n)
	}
}

// TestRecognitionRunReads: a run reads the active contracts alone; a
// draft is left out; an order blocked for credit keeps its contract but
// takes no delivery, so its contract, activated, recognizes nothing
// (integration's note on T77b); more contracts than a run's bound are
// refused.
func TestRecognitionRunReads(t *testing.T) {
	e := setup(t)
	e.contract(t, e.goods()) // a draft
	e.Must(201, "POST", "/v1/credit-limits", fmt.Sprintf(`{"legal_entity_id": %q, "customer_id": %q, "limit": {"amount": "100.00", "currency": "EUR"}}`,
		e.DE, e.customer), "")
	draft := e.draft(t, "EUR", e.goods())["id"].(string)
	confirmed, _ := e.Must(200, "POST", "/v1/sales-orders/"+draft+":confirm", "", `"1"`)
	if confirmed["credit_block"] != "blocked" {
		t.Fatalf("the order over the limit: got credit block %v, want blocked", confirmed["credit_block"])
	}
	blocked, _ := e.Must(201, "POST", contracts, forOrder(draft), "")
	e.activated(t, blocked["id"].(string))
	e.Problem(409, "sales.order_credit_blocked", "POST", "/v1/sales-orders/"+draft+"/deliveries",
		`{"delivery_date": "2026-01-15", "items": [{"item_no": 1, "quantity": {"value": "50", "unit": "EA"}}]}`, "")
	t.Run("a blocked order's contract recognizes nothing", func(t *testing.T) {
		run := e.ran(t, 1)
		fields(t, run, map[string]any{"contract_count": float64(1), "posting_count": float64(0)})
		if got, want := totals(t, run), "recognized 0.00 EUR, billed 0.00 EUR, asset 0.00 EUR, liability 0.00 EUR"; got != want {
			t.Errorf("the run's totals: got %s, want %s", got, want)
		}
		if n := e.revenueEntries(t); n != 0 {
			t.Errorf("the revenue context's entries: got %d, want none", n)
		}
	})
	t.Run("more contracts than a run reads", func(t *testing.T) {
		h := endpoints(t)
		h.MaxContracts = 1
		other := e.draft(t, "EUR", e.item("Fitting kit", "KIT", "1", "10.00", "EUR"))["id"].(string)
		e.Must(200, "POST", "/v1/sales-orders/"+other+":confirm", "", `"1"`)
		second, _ := e.Must(201, "POST", contracts, forOrder(other), "")
		e.activated(t, second["id"].(string))
		err := e.inRequest(t, func(ctx context.Context) error {
			_, err := h.CreateRevenueRecognitionRun(ctx, &gen.RevenueRecognitionRunCreate{LegalEntityID: e.DE, FiscalYear: 2026, FiscalPeriod: 2},
				gen.CreateRevenueRecognitionRunParams{})
			return err
		})
		if p := problem.From(err); p.Code != "revenue.recognition_run_too_large" {
			t.Errorf("got %v, want revenue.recognition_run_too_large", p)
		}
	})
}

// TestRecognitionRunAtOnce: of two runs of one company and period at
// once, one is kept and posts; the other waits for the company's lock
// and is then refused, so nothing is posted twice.
func TestRecognitionRunAtOnce(t *testing.T) {
	e := setup(t)
	_, order := e.active(t, e.goods())
	e.delivered(t, order, 1, "50", "2026-01-15")
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range codes {
		req := apitest.Req{Method: "POST", Path: runs, Body: runOf(e.DE, 2026, 1), Tenant: e.Tenant.ID, As: e.Tenant.Human, Key: apitest.NewKey()}
		wg.Go(func() { codes[i] = e.API.Do(req).Code })
	}
	wg.Wait()
	slices.Sort(codes)
	if !slices.Equal(codes, []int{201, 409}) {
		t.Errorf("statuses: got %v, want one 201 and one 409", codes)
	}
	if got := e.balance(t, "125000"); got != 62500 {
		t.Errorf("contract assets: got %d, want 62500, posted once", got)
	}
}

// TestRecognitionRunRules: the database's own rules for a run kept by
// another writer answer as the run's checks do: one run per company and
// period, each for a later period than the company's last.
func TestRecognitionRunRules(t *testing.T) {
	e := setup(t)
	const keep = `INSERT INTO revenue.recognition_run (tenant_id, id, legal_entity_id, fiscal_year, fiscal_period, posting_date, currency, contract_count,
		posting_count, recognized_amount, billed_amount, contract_asset_amount, contract_liability_amount, revenue_amount, created_by)
		VALUES ($1, gen_random_uuid(), $2, 2026, $3, $4, 'EUR', 0, 0, 0, 0, 0, 0, 0, $5)`
	for _, c := range []struct {
		name   string
		period int
		day    string
		code   string
	}{
		{"a second run of the period", 2, "2026-02-28", "revenue.recognition_run_exists"},
		{"a run for an earlier period", 1, "2026-01-31", "revenue.recognition_period_not_later"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := e.inRequest(t, func(ctx context.Context) error {
				tx, p, err := reqctx.Request(ctx)
				if err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, keep, p.TenantID, e.DE, 2, "2026-02-28", p.ID); err != nil {
					return err
				}
				_, err = tx.Exec(ctx, keep, p.TenantID, e.DE, c.period, c.day, p.ID)
				return err
			})
			if p := problem.From(err); p.Code != c.code {
				t.Errorf("got %v, want %s", p, c.code)
			}
		})
	}
}
