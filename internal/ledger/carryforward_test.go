package ledger_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// cfEnv is a tenant whose companies 1000 (USD, leading ledger alone) and
// 2000 (EUR, 0L and 2L) have the retained earnings account 330000 and
// fiscal years 2024 to 2027 (ADR-0060 §4, §5).
type cfEnv struct {
	parallelEnv
}

// carryForwardDoc is the parallel ledgers' document with each company's
// retained earnings account and journal ranges for 2024, 2025 and 2027.
func carryForwardDoc(tb testing.TB) *config.Config {
	tb.Helper()
	c := parallel(tb)
	for _, code := range []string{"1000", "2000"} {
		co := c.Company(code)
		co.ChartOfAccounts = append(co.ChartOfAccounts, config.Account{Number: "330000", Name: "Retained earnings", Type: config.Equity})
		co.YearEnd.RetainedEarningsAccount = "330000"
		for _, y := range []int{2024, 2025, 2027} {
			for _, dt := range []string{"journal_sa", "journal_ab"} {
				c.NumberRanges = append(c.NumberRanges, config.NumberRange{Company: code, DocumentType: dt, FiscalYear: y, First: 100000, Last: 999999})
			}
		}
	}
	return c
}

func setupCarryForward(tb testing.TB) cfEnv {
	tb.Helper()
	s := cfEnv{parallelEnv: setupParallel(tb)}
	doc := carryForwardDoc(tb)
	s.apply(tb, doc)
	// The ranges the configuration context keeps for the new years, as an
	// activation of the document would write them.
	pgtest.Exec(tb, `INSERT INTO config.number_range (tenant_id, id, legal_entity_id, document_type, fiscal_year, first_value, last_value, next_value,
			is_gapless, created_by)
		SELECT $1, gen_random_uuid(), le, dt, y, 100000, 999999, 100000, false, $2
		FROM unnest($3::uuid[]) le, unnest(ARRAY['journal_sa', 'journal_ab']) dt, unnest(ARRAY[2024, 2025, 2027]::smallint[]) y`,
		s.ten.ID, s.ten.Human.ID, []uuid.UUID{s.us, s.de})
	s.rate(tb, "USD", "EUR", "2024-01-01", "0.9")
	s.rate(tb, "EUR", "USD", "2024-01-01", "1.15")
	return s
}

func (s cfEnv) apply(tb testing.TB, doc *config.Config) {
	tb.Helper()
	if err := apitest.Committed(tb, s.ten, s.ten.Human, func(ctx context.Context) error {
		return ledger.Setup{LegalEntities: tenancy.Directory{}}.Apply(ctx, doc)
	}); err != nil {
		tb.Fatal(err)
	}
}

// setPeriods sets the status of a company's periods up to last of the
// years from to to, every ledger's, as the period lock's transitions
// leave them; locked periods stay locked.
func (s cfEnv) setPeriods(tb testing.TB, le uuid.UUID, from, to, last int, status string) {
	tb.Helper()
	pgtest.Exec(tb, `UPDATE ledger.period_status SET status = $6, version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
		AND fiscal_year BETWEEN $3 AND $4 AND period <= $5 AND status NOT IN ('locked', $6)`, s.ten.ID, le, from, to, last, status)
}

// business posts a year's business of the company: a sale in local
// currency, an expense in foreign currency, share capital in the first
// year, and for 2000 a posting into 2L alone and, in 2025, an audit
// adjustment in special period 13.
func (s cfEnv) business(tb testing.TB, le uuid.UUID, local, foreign string, y int) {
	tb.Helper()
	acct := func(n string) string { return s.account(tb, map[uuid.UUID]string{s.us: "1000", s.de: "2000"}[le], n) }
	bank, revenue, expense := acct("100000"), acct("400000"), acct("520000")
	day := func(md string) string { return fmt.Sprintf("%d-%s", y, md) }
	sale := fmt.Sprintf("%d.00", 1000*(y-2023))
	s.Must(201, "POST", "/v1/journal-entries", entry(le, day("03-10"), local, line{bank, sale, ""}, line{revenue, "-" + sale, ""}), "")
	s.Must(201, "POST", "/v1/journal-entries", entry(le, day("06-15"), foreign, line{expense, "333.33", ""}, line{bank, "-333.33", ""}), "")
	if y == 2024 {
		s.Must(201, "POST", "/v1/journal-entries", entry(le, day("01-05"), local, line{bank, "5000.00", ""}, line{acct("300000"), "-5000.00", ""}), "")
	}
	if le != s.de {
		return
	}
	s.Must(201, "POST", "/v1/journal-entries", with(entry(le, day("09-01"), local, line{expense, "70.00", ""}, line{bank, "-70.00", ""}),
		`"ledger_id": "`+s.local+`"`), "")
	if y == 2025 {
		s.Must(201, "POST", "/v1/journal-entries", inPeriod(entry(le, day("12-31"), local, line{expense, "25.00", ""}, line{bank, "-25.00", ""}), 13), "")
	}
}

// years posts 2024 to 2026 of both companies, projecting 2024 so the runs
// read projected balances and lines not projected yet, and closes them.
func (s cfEnv) years(tb testing.TB) {
	tb.Helper()
	for y := 2024; y <= 2026; y++ {
		s.setPeriods(tb, s.us, y, y, 13, ledger.Open)
		s.setPeriods(tb, s.de, y, y, 13, ledger.Open)
		s.business(tb, s.de, "EUR", "USD", y)
		s.business(tb, s.us, "USD", "EUR", y)
		if y == 2024 {
			fixture.Project(tb)
		}
	}
	s.setPeriods(tb, s.us, 2024, 2026, 16, ledger.Closed)
	s.setPeriods(tb, s.de, 2024, 2026, 16, ledger.Closed)
}

// run carries the company's year forward and returns the runs.
func (s cfEnv) run(tb testing.TB, le uuid.UUID, year int) []map[string]any {
	tb.Helper()
	body, _ := s.Must(201, "POST", "/v1/balance-carry-forwards", fmt.Sprintf(`{"legal_entity_id": %q, "fiscal_year": %d}`, le, year), "")
	var runs []map[string]any
	for _, r := range apitest.List(tb, body, "items") {
		runs = append(runs, r.(map[string]any))
	}
	return runs
}

// rows are a run's rows as "account kind currency tc lc gc", sorted.
func (s cfEnv) rows(tb testing.TB, run any) []string {
	tb.Helper()
	return s.strings(tb, `SELECT a.number || ' ' || b.kind || ' ' || b.currency_tc || ' ' || b.amount_tc || ' ' || b.amount_lc || ' ' || b.amount_gc
		FROM ledger.carry_forward_balance b JOIN ledger.gl_account a ON a.id = b.account_id WHERE b.run_id = $1 ORDER BY 1`, run)
}

// expected is what a run of the company, ledger and year should hold,
// summed from the journal lines rather than the balances the run reads:
// balance sheet accounts per currency, every year's profit and loss on
// 330000, the year's as its result.
func (s cfEnv) expected(tb testing.TB, le uuid.UUID, ledgerID any, year int) []string {
	tb.Helper()
	return s.strings(tb, `SELECT number || ' ' || kind || ' ' || currency_tc || ' ' || tc || ' ' || lc || ' ' || gc FROM (
			SELECT CASE WHEN a.is_balance_sheet THEN a.number ELSE '330000' END AS number,
				CASE WHEN NOT a.is_balance_sheet AND l.fiscal_year = $4 THEN 'result' ELSE 'balance' END AS kind, e.currency_tc,
				sum(l.amount_tc) AS tc, sum(l.amount_lc) AS lc, sum(l.amount_gc) AS gc
			FROM ledger.journal_line l JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id
			JOIN ledger.gl_account a ON a.id = l.account_id
			WHERE l.tenant_id = $1 AND l.legal_entity_id = $2 AND l.ledger_id = $3 AND l.fiscal_year <= $4
			GROUP BY 1, 2, 3 HAVING sum(l.amount_tc) <> 0 OR sum(l.amount_lc) <> 0 OR sum(l.amount_gc) <> 0) x ORDER BY 1`, s.ten.ID, le, ledgerID, year)
}

func (s cfEnv) strings(tb testing.TB, sql string, args ...any) []string {
	tb.Helper()
	rows, _ := pgtest.Admin(tb).Query(context.Background(), sql, args...)
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		tb.Fatalf("query: %v", err)
	}
	return out
}

// sumsToZero fails unless a run's rows sum to zero per transaction
// currency in transaction, local and group currency (ADR-0060 §5).
func (s cfEnv) sumsToZero(tb testing.TB, run any) {
	tb.Helper()
	if off := s.strings(tb, `SELECT currency_tc || ' ' || sum(amount_tc) || ' ' || sum(amount_lc) || ' ' || sum(amount_gc) FROM ledger.carry_forward_balance
		WHERE run_id = $1 GROUP BY currency_tc HAVING sum(amount_tc) <> 0 OR sum(amount_lc) <> 0 OR sum(amount_gc) <> 0`, run); len(off) > 0 {
		tb.Errorf("run %v's sums per currency: got %v, want zero in every amount", run, off)
	}
}

// TestCarryForwardChained: three years carried forward one after the
// other in each ledger, the first from every year and the next two chained,
// hold what the journal lines sum to, each ledger its own figures, the
// result being each year's alone, and every run sums to zero per currency
// in every amount (ADR-0060 §5, §10). A company in USD with the group in
// EUR carries local and group amounts apart.
func TestCarryForwardChained(t *testing.T) {
	s := setupCarryForward(t)
	s.years(t)
	var last []map[string]any
	for y := 2024; y <= 2026; y++ {
		runs := s.run(t, s.de, y)
		if got := fmt.Sprintf("%v %v", runs[0]["ledger_code"], runs[1]["ledger_code"]); got != "0L 2L" {
			t.Fatalf("%d's runs: got ledgers %s, want 0L 2L", y, got)
		}
		for i, r := range runs {
			wantBasis := map[bool]string{true: ledger.BasisAllYears, false: ledger.BasisChained}[y == 2024]
			if r["basis"] != wantBasis {
				t.Errorf("%d in %v: got basis %v, want %s", y, r["ledger_code"], r["basis"], wantBasis)
			}
			if y > 2024 && r["chained_from_id"] != last[i]["id"] {
				t.Errorf("%d in %v: got chained from %v, want %v", y, r["ledger_code"], r["chained_from_id"], last[i]["id"])
			}
			if got, want := s.rows(t, r["id"]), s.expected(t, s.de, r["ledger_id"], y); !slices.Equal(got, want) {
				t.Errorf("%d in %v: got rows %v, want %v", y, r["ledger_code"], got, want)
			}
			s.sumsToZero(t, r["id"])
		}
		last = runs
	}
	if s.rows(t, last[0]["id"])[0] == s.rows(t, last[1]["id"])[0] {
		t.Errorf("0L's and 2L's bank: got the same %v, want 2L's own postings in 2L", s.rows(t, last[0]["id"])[0])
	}
	us := s.run(t, s.us, 2026)[0]
	if got, want := s.rows(t, us["id"]), s.expected(t, s.us, s.leading, 2026); !slices.Equal(got, want) {
		t.Errorf("1000 from every year: got rows %v, want %v", got, want)
	}
	s.sumsToZero(t, us["id"])
	s.representation(t, last[0])
}

// representation checks a run as GET returns it: its company, ledger,
// retained earnings account, who ran it and 2026's result per transaction
// currency: the sale of EUR 3,000.00 and the expense of USD 333.33 (EUR
// 300.00 at 0.9), each carried in its own currency.
func (s cfEnv) representation(t *testing.T, posted map[string]any) {
	t.Helper()
	run, tag := s.Must(200, "GET", "/v1/balance-carry-forwards/"+posted["id"].(string), "", "")
	if tag == "" {
		t.Error("ETag: got none, want the run's")
	}
	for k, want := range map[string]any{"company_code": "2000", "company_name": "Acme GmbH", "ledger_name": "Leading ledger", "from_fiscal_year": 2026.0,
		"to_fiscal_year": 2027.0, "status": "current", "local_currency": "EUR", "group_currency": "EUR", "created_by_name": "human"} {
		if run[k] != want {
			t.Errorf("%s: got %v, want %v", k, run[k], want)
		}
	}
	if got := apitest.Object(t, run, "retained_earnings_account")["number"]; got != "330000" {
		t.Errorf("retained earnings account: got %v, want 330000", got)
	}
	results := map[string]string{}
	for _, tot := range apitest.List(t, run, "totals") {
		m := tot.(map[string]any)
		results[m["currency"].(string)] = fmt.Sprint(apitest.Object(t, m, "result")["amount"], " ", apitest.Object(t, m, "result_lc")["amount"])
	}
	if got := fmt.Sprint(results); got != "map[EUR:-3000.00 -3000.00 USD:333.33 300.00]" {
		t.Errorf("2026's results per currency: got %s, want the sale's EUR -3000.00 and the expense's USD 333.33 (EUR 300.00)", got)
	}
	for k, want := range map[string]string{"result_lc": "-2700.00", "result_gc": "-2700.00"} {
		if got := apitest.Object(t, run, k)["amount"]; got != want {
			t.Errorf("2026's %s over every currency: got %v, want %s", k, got, want)
		}
	}
	for _, c := range []string{"lc", "gc"} {
		if d, cr := apitest.Object(t, run, "debit_"+c)["amount"], apitest.Object(t, run, "credit_"+c)["amount"]; d != cr {
			t.Errorf("the run's %s totals: got debit %v and credit %v, want them equal", c, d, cr)
		}
	}
	eur := apitest.List(t, run, "totals")[0].(map[string]any)
	if d, c := apitest.Object(t, eur, "debit_lc")["amount"], apitest.Object(t, eur, "credit_lc")["amount"]; d != c {
		t.Errorf("EUR's local totals: got debit %v and credit %v, want them equal", d, c)
	}
}

// TestCarryForwardFromEveryYear: a year carried forward without the
// previous year's run, from every year, holds the same rows, balance and
// result alike, as the chain of runs in a tenant with the same postings.
func TestCarryForwardFromEveryYear(t *testing.T) {
	chained, summed := setupCarryForward(t), setupCarryForward(t)
	chained.years(t)
	summed.years(t)
	for y := 2024; y <= 2025; y++ {
		chained.run(t, chained.de, y)
	}
	a, b := chained.run(t, chained.de, 2026), summed.run(t, summed.de, 2026)
	for i := range a {
		if a[i]["basis"] != ledger.BasisChained || b[i]["basis"] != ledger.BasisAllYears {
			t.Fatalf("bases: got %v and %v, want chained and all_years", a[i]["basis"], b[i]["basis"])
		}
		if got, want := summed.rows(t, b[i]["id"]), chained.rows(t, a[i]["id"]); !slices.Equal(got, want) {
			t.Errorf("%v from every year: got %v, want the chain's %v", b[i]["ledger_code"], got, want)
		}
	}
}

// TestCarryForwardRerun: carrying a year forward again supersedes its
// current run, which names its successor, and publishes the new run with
// the one it superseded and the year's result per currency.
func TestCarryForwardRerun(t *testing.T) {
	s := setupCarryForward(t)
	s.years(t)
	first := s.run(t, s.us, 2026)[0]
	second := s.run(t, s.us, 2026)[0]
	old, _ := s.Must(200, "GET", "/v1/balance-carry-forwards/"+first["id"].(string), "", "")
	if old["status"] != "superseded" || old["superseded_by_id"] != second["id"] || old["superseded_at"] == nil {
		t.Errorf("the first run: got %v by %v at %v, want superseded by %v", old["status"], old["superseded_by_id"], old["superseded_at"], second["id"])
	}
	if !slices.Equal(s.rows(t, first["id"]), s.rows(t, second["id"])) {
		t.Error("the rerun's rows: got others, want the same with nothing posted since")
	}
	events := apitest.Published(t, s.ten, outbox.Event{Context: "ledger", Name: "balance_carried_forward"})
	if len(events) != 2 {
		t.Fatalf("BalanceCarriedForward: got %d, want 2", len(events))
	}
	last := events[1]
	if last["run_id"] != second["id"] || last["superseded_run_id"] != first["id"] {
		t.Errorf("the rerun's event: got run %v superseding %v, want %v superseding %v", last["run_id"], last["superseded_run_id"], second["id"], first["id"])
	}
	if got := apitest.Object(t, last, "result_lc")["amount"]; got != second["result_lc"].(map[string]any)["amount"] {
		t.Errorf("the event's local result: got %v, want the run's %v", got, second["result_lc"])
	}
	var currencies []string
	for _, r := range last["results"].([]any) {
		currencies = append(currencies, r.(map[string]any)["currency"].(string))
	}
	if got := strings.Join(currencies, " "); got != "EUR USD" {
		t.Errorf("the event's results: got %s, want EUR USD", got)
	}
	var audited int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'ledger.carry_forward_run.run'`, []any{s.ten.ID}, &audited)
	if audited != 2 {
		t.Errorf("audit records: got %d, want 2", audited)
	}
}

// TestCarryForwardStale: a run chains only from a previous year's run no
// period of which has been opened since; carried forward again, it chains.
func TestCarryForwardStale(t *testing.T) {
	s := setupCarryForward(t)
	s.years(t)
	s.run(t, s.us, 2025)
	s.setPeriods(t, s.us, 2025, 2025, 12, ledger.Open)
	s.setPeriods(t, s.us, 2025, 2025, 12, ledger.Closed)
	p := s.Problem(409, "ledger.carry_forward_stale", "POST", "/v1/balance-carry-forwards", fmt.Sprintf(`{"legal_entity_id": %q, "fiscal_year": 2026}`, s.us), "")
	if detail := p["detail"].(string); !strings.Contains(detail, "2025/1,") || !strings.Contains(detail, "carry 2025 forward again") {
		t.Errorf("detail: got %q, want the periods opened and the year to carry forward again", detail)
	}
	again := s.run(t, s.us, 2025)[0]
	if got := s.run(t, s.us, 2026)[0]; got["chained_from_id"] != again["id"] {
		t.Errorf("2026 after 2025 again: got chained from %v, want %v", got["chained_from_id"], again["id"])
	}
}

// TestCarryForwardStaleUnrecorded: a period of an earlier year set up
// after the previous year's run, which the run did not read, makes it
// stale once opened (ADR-0060 §6), though it is closed again.
func TestCarryForwardStaleUnrecorded(t *testing.T) {
	s := setupCarryForward(t)
	s.years(t)
	s.run(t, s.us, 2025)
	doc := carryForwardDoc(t)
	for _, dt := range []string{"journal_sa", "journal_ab"} {
		doc.NumberRanges = append(doc.NumberRanges, config.NumberRange{Company: "1000", DocumentType: dt, FiscalYear: 2023, First: 100000, Last: 999999})
	}
	s.apply(t, doc) // 2023's periods, closed, read by no run
	s.setPeriods(t, s.us, 2023, 2023, 1, ledger.Open)
	s.setPeriods(t, s.us, 2023, 2023, 1, ledger.Closed)
	p := s.Problem(409, "ledger.carry_forward_stale", "POST", "/v1/balance-carry-forwards", fmt.Sprintf(`{"legal_entity_id": %q, "fiscal_year": 2026}`, s.us), "")
	if detail := p["detail"].(string); !strings.Contains(detail, "periods 2023/1 were opened") {
		t.Errorf("detail: got %q, want 2023/1 named", detail)
	}
}

// TestCarryForwardRefusals: each precondition refuses the run (ADR-0060
// §5) and nothing is kept.
func TestCarryForwardRefusals(t *testing.T) {
	s := setupCarryForward(t)
	s.years(t)
	body := func(le uuid.UUID, year int, extra string) string {
		return fmt.Sprintf(`{"legal_entity_id": %q, "fiscal_year": %d%s}`, le, year, extra)
	}
	s.setPeriods(t, s.de, 2026, 2026, 5, ledger.Open)
	for _, c := range []struct {
		name, body, code string
		status           int
	}{
		{"an unknown company", body(uuid.New(), 2026, ""), "request.reference_not_found", 422},
		{"an unknown ledger", body(s.us, 2026, `, "ledger_ids": ["`+uuid.NewString()+`"]`), "request.reference_not_found", 422},
		{"a ledger the company does not keep", body(s.us, 2026, `, "ledger_ids": ["`+s.local+`"]`), "ledger.ledger_not_kept", 422},
		{"a year not set up", body(s.us, 2023, ""), "ledger.no_fiscal_period", 422},
		{"the next year not set up", body(s.us, 2027, ""), "ledger.next_year_not_set_up", 409},
		{"periods open", body(s.de, 2026, ""), "ledger.carry_forward_periods_open", 409},
	} {
		t.Run(c.name, func(t *testing.T) {
			s.In(t).Problem(c.status, c.code, "POST", "/v1/balance-carry-forwards", c.body, "")
		})
	}
	t.Run("the open periods named", func(t *testing.T) {
		p := s.In(t).Problem(409, "ledger.carry_forward_periods_open", "POST", "/v1/balance-carry-forwards", body(s.de, 2026, `, "ledger_ids": ["`+s.local+`"]`), "")
		if detail := p["detail"].(string); !strings.Contains(detail, "2026/1 in 2L") || strings.Contains(detail, "0L") {
			t.Errorf("detail: got %q, want 2L's open periods alone", detail)
		}
	})
	t.Run("no retained earnings account", func(t *testing.T) {
		bare := setupParallel(t)
		bare.In(t).Problem(409, "ledger.retained_earnings_missing", "POST", "/v1/balance-carry-forwards", body(bare.us, 2026, ""), "")
	})
	t.Run("a run of a company the caller cannot see", func(t *testing.T) {
		run := s.run(t, s.us, 2026)[0]
		other := setupParallel(t)
		other.In(t).Problem(404, "request.not_found", "GET", "/v1/balance-carry-forwards/"+run["id"].(string), "", "")
	})
	var runs int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.carry_forward_run WHERE tenant_id = $1 AND legal_entity_id = $2`, []any{s.ten.ID, s.de}, &runs)
	if runs != 0 {
		t.Errorf("runs of 2000 after the refusals: got %d, want 0", runs)
	}
}

// TestRetainedEarningsKept: once activated, the retained earnings account
// can be changed but not removed (ADR-0060 §4: the determination is never
// deleted).
func TestRetainedEarningsKept(t *testing.T) {
	s := setupCarryForward(t)
	removed, changed := carryForwardDoc(t), carryForwardDoc(t)
	removed.Company("2000").YearEnd.RetainedEarningsAccount = ""
	changed.Company("2000").YearEnd.RetainedEarningsAccount = "300000"
	for name, c := range map[string]struct {
		doc  *config.Config
		want int
	}{"removed": {removed, 1}, "changed": {changed, 0}} {
		t.Run(name, func(t *testing.T) {
			var fs []config.Finding
			if err := apitest.InRequest(t, s.ten, s.ten.Human, func(ctx context.Context) (err error) {
				fs, err = ledger.Setup{LegalEntities: tenancy.Directory{}}.Check(ctx, carryForwardDoc(t), c.doc)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, f := range fs {
				if f.Path == "companies[2000].year_end.retained_earnings_account" {
					n++
				}
			}
			if n != c.want {
				t.Errorf("findings on the account: got %v, want %d", fs, c.want)
			}
		})
	}
}

// TestCarryForwardDatabaseFailures: a run chained and superseding the
// year's run, and its read, fail with each statement they make.
func TestCarryForwardDatabaseFailures(t *testing.T) {
	s := setupCarryForward(t)
	s.years(t)
	s.run(t, s.de, 2025)
	run := s.run(t, s.de, 2026)[0]
	h := endpoints()
	id := uuid.MustParse(run["id"].(string))
	for name, call := range map[string]func(ctx context.Context) error{
		"run": func(ctx context.Context) error {
			_, err := h.RunBalanceCarryForward(ctx, &gen.BalanceCarryForwardRequest{LegalEntityID: s.de, FiscalYear: 2026}, gen.RunBalanceCarryForwardParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetBalanceCarryForward(ctx, gen.GetBalanceCarryForwardParams{ID: id})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
			apitest.EveryFailure(t, s.ten, s.ten.Human, name, call)
		})
	}
}
