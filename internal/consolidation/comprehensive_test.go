package consolidation_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/rules"
)

// reserveEnv is a tenant whose group ACME consolidates company 1000 (USD)
// from December 2025, with withEliminations' intercompany accounts and
// the difference of IC_BALANCES on the translation reserve's account,
// G3900; the USD/EUR rates 0.80 from 1 January 2025, 0.81 from 15
// November 2025 (November's closing, where 1000's acquisition is
// measured), 0.82 from 31 December 2025 (December's closing), 0.85 from 1
// October 2026 and 0.90 from 30 November 2026 (November's closing); and
// reserveFigures posted.
func reserveEnv(t *testing.T) fixture.Env {
	t.Helper()
	e := fixture.NewWith(t, nil, func(doc *config.Config) {
		withEliminations(doc)
		doc.Consolidation.Groups[0].Members[0].FirstPeriod = "2025-12"
		doc.Consolidation.Eliminations[0].DifferenceAccount = "G3900"
	}, runPermissions...)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2025-01-01', 0.8, 'ECB', $2),
		       ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2025-11-15', 0.81, 'ECB', $2),
		       ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2025-12-31', 0.82, 'ECB', $2),
		       ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-10-01', 0.85, 'ECB', $2),
		       ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-11-30', 0.9, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	sameCalendar(t, e)
	reserveFigures(t, e)
	return e
}

// reserveFigures posts what TestOtherComprehensiveIncome computes by
// hand. Company 1000 (USD): share capital of 500.00 in January 2025 and
// revenue of 1,000.00 in June 2025 (each booked at 0.80: 400.00 and
// 800.00 EUR), a receivable of 50.00 from company 2000 in December 2025,
// an expense of 200.00 in October 2026 (170.00 EUR at 0.85) and a
// receivable of 100.00 more from 2000 in November 2026, each against the
// bank. Company 2000 (EUR): its investment of 400.00 in 1000 and a
// payable of 40.00 to 1000 in December 2025, and one of 88.00 more in
// November 2026.
//
// At December 2025 company 1000's assets are USD 1,500.00, 1,230.00 EUR
// at 0.82, against 400.00 of share capital and 800.00 of results as
// booked: a reserve of -30.00. At November 2026 they are USD 1,300.00,
// 1,170.00 EUR at 0.90, against 400.00, 800.00 and the year's loss of
// 170.00: a reserve of -140.00. The year's difference is -110.00, a gain:
// the opening net assets' USD 1,500.00 at 0.90 less 0.82 (120.00) less
// the loss's USD 200.00 at 0.90 less 0.85 (10.00).
//
// The receivable and the payable differ by 1.00 at December 2025 (USD
// 50.00 at 0.82, 41.00, against 40.00) and by 7.00 at November 2026 (USD
// 150.00 at 0.90, 135.00, against 128.00), each within the tolerance and
// eliminated to G3900.
func reserveFigures(t *testing.T, e fixture.Env) {
	t.Helper()
	post(t, e, e.US, "1000", "USD", "2025-01-05", "100000", "300000", "500.00", nil)
	post(t, e, e.US, "1000", "USD", "2025-06-15", "100000", "400000", "1000.00", nil)
	post(t, e, e.DE, "2000", "EUR", "2025-12-05", "150000", "100000", "400.00", nil)
	postIC(t, e, e.US, "1000", "USD", "2025-12-10", icLine{"125000", "50.00", &e.DE}, icLine{"100000", "-50.00", nil})
	postIC(t, e, e.DE, "2000", "EUR", "2025-12-10", icLine{"100000", "40.00", nil}, icLine{"225000", "-40.00", &e.US})
	post(t, e, e.US, "1000", "USD", "2026-10-15", "520000", "100000", "200.00", nil)
	postIC(t, e, e.US, "1000", "USD", "2026-11-10", icLine{"125000", "100.00", &e.DE}, icLine{"100000", "-100.00", nil})
	postIC(t, e, e.DE, "2000", "EUR", "2026-11-10", icLine{"100000", "88.00", nil}, icLine{"225000", "-88.00", &e.US})
}

// startPeriod starts ACME's run of a period.
func startPeriod(t *testing.T, e fixture.Env, year, period int) uuid.UUID {
	t.Helper()
	o, _ := e.Must(202, "POST", "/v1/consolidation-runs", fmt.Sprintf(`{"group_code": "ACME", "fiscal_year": %d, "period": %d}`, year, period), "")
	return uuid.MustParse(strings.TrimPrefix(o["resource"].(string), "/v1/consolidation-runs/"))
}

// reserveLines shows a profit and loss's translation reserve, one line
// per company, and the run it opens from.
func reserveLines(pl map[string]any) string {
	opening := "none"
	if id, ok := pl["opening_run_id"].(string); ok {
		opening = id
	}
	out := []string{"opening run " + opening}
	for _, it := range pl["translation_reserve"].([]any) {
		v := it.(map[string]any)
		held := ""
		if pct, ok := v["ownership_percent"].(string); ok {
			held = " at " + pct + "%"
		}
		out = append(out, fmt.Sprintf("%s %s %s%s: %s + %s = %s, interest %s, owners %s", v["company_code"], v["company_name"], v["role"], held,
			moneyOf(v["opening"]), moneyOf(v["difference"]), moneyOf(v["closing"]), moneyOf(v["non_controlling_interests"]), moneyOf(v["owners_of_the_parent"])))
	}
	return strings.Join(out, "\n")
}

// comprehensiveNames are the profit and loss's totals in order.
var comprehensiveNames = []string{"revenue", "expenses", "profit", "non_controlling_interests", "owners_of_the_parent", "other_comprehensive_income",
	"total_comprehensive_income", "comprehensive_income_non_controlling_interests", "comprehensive_income_owners_of_the_parent",
	"revaluation_translation"}

// minorOf is a figure's column in minor units of a currency of two.
func minorOf(t *testing.T, figure any, column string) int64 {
	t.Helper()
	amount := figure.(map[string]any)[column].(map[string]any)["amount"].(string)
	n, err := strconv.ParseInt(strings.Replace(amount, ".", "", 1), 10, 64)
	if err != nil {
		t.Fatalf("amount %q: %v", amount, err)
	}
	return n
}

// balanced checks a run's statements' identities in each column: the
// balance sheet's assets, liabilities, equity and profit sum to zero; the
// profit is the revenue and the expenses, so it carries no translation
// difference; and total comprehensive income is the profit and other
// comprehensive income, attributed whole.
func balanced(t *testing.T, bs, pl map[string]any) {
	t.Helper()
	b, p := bs["totals"].(map[string]any), pl["totals"].(map[string]any)
	for _, col := range []string{"reported", "eliminations", "consolidated"} {
		if got := minorOf(t, b["assets"], col) + minorOf(t, b["liabilities"], col) + minorOf(t, b["equity"], col) + minorOf(t, b["profit"], col); got != 0 {
			t.Errorf("the balance sheet's %s column: got %d, want 0", col, got)
		}
		if got, w := minorOf(t, p["profit"], col), minorOf(t, p["revenue"], col)+minorOf(t, p["expenses"], col); got != w {
			t.Errorf("the profit's %s column: got %d, want the revenue and expenses %d", col, got, w)
		}
		if got, w := minorOf(t, p["total_comprehensive_income"], col), minorOf(t, p["profit"], col)+minorOf(t, p["other_comprehensive_income"], col); got != w {
			t.Errorf("total comprehensive income's %s column: got %d, want %d", col, got, w)
		}
		if got, w := minorOf(t, p["comprehensive_income_non_controlling_interests"], col)+minorOf(t, p["comprehensive_income_owners_of_the_parent"], col),
			minorOf(t, p["total_comprehensive_income"], col); got != w {
			t.Errorf("total comprehensive income's attribution, %s column: got %d, want %d", col, got, w)
		}
	}
}

// statements reads a run's balance sheet and profit and loss.
func statements(t *testing.T, e fixture.Env, run uuid.UUID) (bs, pl map[string]any) {
	t.Helper()
	bs, _ = e.Must(200, "GET", statementPath(run, "balance_sheet"), "", "")
	pl, _ = e.Must(200, "GET", statementPath(run, "profit_and_loss"), "", "")
	return bs, pl
}

// TestOtherComprehensiveIncome (ADR-0059 §11, As built: other
// comprehensive income; the accounting specialist's B3): reserveFigures'
// December 2025 run, the group's first, shows company 1000's whole
// reserve, -30.00, as the year's other comprehensive income, opening at
// zero. Once it is final, November 2026's run names it as its opening
// run, however often it is prepared, reads it in the configuration
// version it recorded, and shows the year's difference,
// -110.00, between the reserve's opening -30.00 and closing -140.00, all
// of it the owners' (1000 is held whole), the change of the eliminations
// on the reserve (7.00 less 1.00) beside it, and total comprehensive
// income: the loss of 170.00 and the gain. The balance sheet keeps the
// accumulated reserve and balances, and its consolidated reserve is the
// opening run's and the year's other comprehensive income. The statement
// does not change when the opening run is superseded, but the run is
// stale from then on; a run started then, from locked periods, finds no
// final run of 2025, opens at zero and is not stale until December 2025
// is finalized again, after which it says so on its reads and its
// finalization is refused; the balance sheet shows neither the opening
// run nor the reserves.
func TestOtherComprehensiveIncome(t *testing.T) {
	e := reserveEnv(t)
	e.Tenant.Grant(t, "consolidation.run.finalize", e.Tenant.Human)
	h := app.Consolidation(app.Config{})
	first := startPeriod(t, e, 2025, 12)
	carryOut(t, e, h, first)
	bs, pl := statements(t, e, first)
	balanced(t, bs, pl)
	if got, w := reserveLines(pl), "opening run none\n1000 Acme Corp US member at 100%: 0.00 EUR + -30.00 EUR = -30.00 EUR, interest 0.00 EUR, owners -30.00 EUR\n"+
		"2000 Acme GmbH parent: 0.00 EUR + 0.00 EUR = 0.00 EUR, interest 0.00 EUR, owners 0.00 EUR"; got != w {
		t.Errorf("2025's reserves: got\n%s\nwant\n%s", got, w)
	}
	if got, w := columnsOf(pl["totals"].(map[string]any)["other_comprehensive_income"]), "-30.00 EUR / 1.00 EUR / -29.00 EUR"; got != w {
		t.Errorf("2025's other comprehensive income: got %s, want %s", got, w)
	}
	pgtest.Exec(t, `UPDATE consolidation.run SET status = 'final', finalized_by = $2, finalized_at = now(), version = version + 1 WHERE id = $1`, first,
		e.Tenant.Human.ID)
	// Another configuration version, the same but for an entity's empty
	// rules: the opening run is read in its own.
	fixture.ActivateRules(t, e.Tenant.ID, "journal_entry", map[string]rules.Rule{})
	run, _ := start(t, e)
	for range 2 {
		if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.PrepareConsolidationRun(ctx, run)
			return err
		}); err != nil {
			t.Fatalf("prepare: %v", err)
		}
	}
	carryOut(t, e, h, run)
	bs, pl = statements(t, e, run)
	balanced(t, bs, pl)
	reserves := "opening run " + first.String() + "\n1000 Acme Corp US member at 100%: -30.00 EUR + -110.00 EUR = -140.00 EUR, interest 0.00 EUR, " +
		"owners -110.00 EUR\n2000 Acme GmbH parent: 0.00 EUR + 0.00 EUR = 0.00 EUR, interest 0.00 EUR, owners 0.00 EUR"
	if got := reserveLines(pl); got != reserves {
		t.Errorf("2026's reserves: got\n%s\nwant\n%s", got, reserves)
	}
	totals := "revenue 0.00 EUR / 0.00 EUR / 0.00 EUR, expenses 170.00 EUR / 0.00 EUR / 170.00 EUR, profit 170.00 EUR / 0.00 EUR / 170.00 EUR, " +
		"non_controlling_interests 0.00 EUR / 0.00 EUR / 0.00 EUR, owners_of_the_parent 170.00 EUR / 0.00 EUR / 170.00 EUR, " +
		"other_comprehensive_income -110.00 EUR / 6.00 EUR / -104.00 EUR, total_comprehensive_income 60.00 EUR / 6.00 EUR / 66.00 EUR, " +
		"comprehensive_income_non_controlling_interests 0.00 EUR / 0.00 EUR / 0.00 EUR, " +
		"comprehensive_income_owners_of_the_parent 60.00 EUR / 6.00 EUR / 66.00 EUR, revaluation_translation 0.00 EUR / 0.00 EUR / 0.00 EUR"
	if got := statementTotals(pl, comprehensiveNames...); got != totals {
		t.Errorf("2026's totals: got %s, want %s", got, totals)
	}
	if got, w := statementRow(bs, "G3900"), "G3900 Currency translation reserve equity (cta): -140.00 EUR / 7.00 EUR / -133.00 EUR"; got != w {
		t.Errorf("2026's reserve on the balance sheet: got %s, want %s (2025's -29.00 and the year's -104.00)", got, w)
	}
	if got := staleness(t, e, run, pl); got != "false false" {
		t.Errorf("the run opening from the final run of 2025: got stale %s, want false on the run and its statement", got)
	}
	if _, shown := bs["translation_reserve"]; shown || bs["opening_run_id"] != nil {
		t.Errorf("the balance sheet: got reserves %v and opening run %v, want neither", bs["translation_reserve"], bs["opening_run_id"])
	}
	if got := statementTotals(bs, "assets", "liabilities", "equity", "profit"); strings.Contains(got, "more") {
		t.Errorf("the balance sheet's totals: got %s, want the four alone", got)
	}
	page, _ := e.Must(200, "GET", statementPath(run, "profit_and_loss")+"?page_size=1", "", "")
	if got := reserveLines(page) + "\n" + statementTotals(page, comprehensiveNames...); got != reserves+"\n"+totals {
		t.Errorf("a page of the profit and loss: got\n%s\nwant the whole statement's reserves and totals", got)
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "profit and loss", func(ctx context.Context) error {
		_, err := app.Consolidation(app.Config{}).GetConsolidatedStatement(ctx, gen.GetConsolidatedStatementParams{ID: run, Statement: "profit_and_loss"})
		return err
	})
	pgtest.Exec(t, `UPDATE consolidation.run SET status = 'superseded', superseded_by = gen_random_uuid(), version = version + 1 WHERE id = $1`, first)
	_, after := statements(t, e, run)
	if got := reserveLines(after) + "\n" + statementTotals(after, comprehensiveNames...); got != reserves+"\n"+totals {
		t.Errorf("the profit and loss once its opening run is superseded: got\n%s\nwant it unchanged", got)
	}
	if got := staleness(t, e, run, after); got != "true true" {
		t.Errorf("the run once its opening run is superseded: got stale %s, want true on the run and its statement", got)
	}
	setPeriod(t, e, []uuid.UUID{e.US, e.DE}, "closed", "locked")
	later, _ := start(t, e)
	carryOut(t, e, h, later)
	bs, pl = statements(t, e, later)
	balanced(t, bs, pl)
	if got, w := reserveLines(pl), "opening run none\n1000 Acme Corp US member at 100%: 0.00 EUR + -140.00 EUR = -140.00 EUR, interest 0.00 EUR, "+
		"owners -140.00 EUR\n2000 Acme GmbH parent: 0.00 EUR + 0.00 EUR = 0.00 EUR, interest 0.00 EUR, owners 0.00 EUR"; got != w {
		t.Errorf("a run without a final run of 2025: got\n%s\nwant\n%s", got, w)
	}
	if got, w := columnsOf(pl["totals"].(map[string]any)["other_comprehensive_income"]), "-140.00 EUR / 7.00 EUR / -133.00 EUR"; got != w {
		t.Errorf("its other comprehensive income: got %s, want %s", got, w)
	}
	if got := staleness(t, e, later, pl); got != "false false" {
		t.Errorf("the run without an opening run: got stale %s, want false while 2025 has no final run", got)
	}
	again := startPeriod(t, e, 2025, 12)
	carryOut(t, e, h, again)
	pgtest.Exec(t, `UPDATE consolidation.run SET status = 'final', finalized_by = $2, finalized_at = now(), version = version + 1 WHERE id = $1`, again,
		e.Tenant.Human.ID)
	_, pl = statements(t, e, later)
	if got := staleness(t, e, later, pl); got != "true true" {
		t.Errorf("the run once December 2025 is finalized after it started: got stale %s, want true on the run and its statement", got)
	}
	_, tag := e.Must(200, "GET", runsPath+"/"+later.String(), "", "")
	p := e.Problem(409, "consolidation.run_stale", "POST", runsPath+"/"+later.String()+":finalize", "", tag)
	if w := "it opened from none, and the final run is now " + again.String(); !strings.Contains(fmt.Sprint(p["detail"]), w) {
		t.Errorf("the refusal: got %v, want it to say %q", p["detail"], w)
	}
	list, _ := e.Must(200, "GET", runsPath+"?status=computed&fiscal_year=2026", "", "")
	var stale []string
	for _, it := range list["items"].([]any) {
		r := it.(map[string]any)
		stale = append(stale, fmt.Sprint(r["id"] == run.String() || r["id"] == later.String(), " ", r["stale"]))
	}
	if got, w := strings.Join(stale, ", "), "true true, true true"; got != w {
		t.Errorf("the computed runs of 2026 in the list: got %s, want %s (both, stale)", got, w)
	}
}

// staleness shows a run's stale on its own read and on a statement of it.
func staleness(t *testing.T, e fixture.Env, run uuid.UUID, statement map[string]any) string {
	t.Helper()
	r, _ := e.Must(200, "GET", runsPath+"/"+run.String(), "", "")
	return fmt.Sprint(r["stale"], " ", statement["stale"])
}
