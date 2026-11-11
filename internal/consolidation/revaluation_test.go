package consolidation_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/consolidation"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// The translation part of a group-currency revaluation (ADR-0059 §7, As
// built; ADR-0042, the group-currency difference in consolidation; the
// accounting specialist's B4 correction 1, po:2026-11-09; IAS 21.39(c)).

// revaluedEnv is a tenant whose group ACME consolidates company 1000
// (USD) from October 2026; its bank account 100000 is revalued, the
// differences to 480000 and 680000; the average USD/EUR rates are 0.80
// from 1 January 2025, 0.64 from 30 September 2026 and 0.50 from 31
// October 2026; and revaluedFigures are posted.
func revaluedEnv(t *testing.T) fixture.Env {
	t.Helper()
	e := fixture.NewWith(t, nil, func(doc *config.Config) {
		withRuns(doc)
		doc.Consolidation.Groups[0].Members[0].FirstPeriod = "2026-10"
		us := doc.Company("1000")
		us.ChartOfAccounts = append(us.ChartOfAccounts,
			config.Account{Number: "480000", Name: "Unrealised exchange gains", Type: config.Revenue, GroupAccount: "G4000"},
			config.Account{Number: "680000", Name: "Unrealised exchange losses", Type: config.Expense, GroupAccount: "G5000"})
		us.Account("100000").RevalueForeignCurrency = true
		us.FXValuation.Accounts = append(us.FXValuation.Accounts, config.FXValuationAccount{Account: "100000", UnrealisedGain: "480000", UnrealisedLoss: "680000"})
	}, runPermissions...)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2025-01-01', 0.8, 'ECB', $2),
		       ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-09-30', 0.64, 'ECB', $2),
		       ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-10-31', 0.5, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	sameCalendar(t, e)
	revaluedFigures(t, e)
	return e
}

// revaluedFigures posts what TestRevaluationTranslationPart computes by
// hand.
//
// Company 1000 (USD) has share capital of USD 500.00 from January 2026
// (400.00 EUR at 0.80), which company 2000 (EUR) holds as its investment
// of 400.00, and EUR 1,000.00 received in August for revenue: USD
// 1,250.00 at 1/0.80, 1,000.00 EUR in group currency. Its bank's euros
// are revalued:
//
//   - at 30 September, 1/0.64: USD 1,562.50, a gain of USD 312.50 on
//     480000, reset on 1 October. The euros are worth EUR 1,000.00
//     whatever the dollar does, so the group difference is 0.00 and both
//     entries book USD 312.50 with no group amount; at the entries' rate,
//     0.64, the local amount is 200.00 EUR.
//   - at 31 October, 1/0.50: USD 2,000.00, a gain of USD 750.00, reset on
//     1 November; no group amount again, 375.00 EUR at the entries' 0.50.
func revaluedFigures(t *testing.T, e fixture.Env) {
	t.Helper()
	post(t, e, e.US, "1000", "USD", "2026-01-05", "100000", "300000", "500.00", nil)
	post(t, e, e.DE, "2000", "EUR", "2026-01-05", "150000", "100000", "400.00", nil)
	post(t, e, e.US, "1000", "EUR", "2026-08-15", "100000", "400000", "1000.00", nil)
	for _, day := range []string{"2026-09-30", "2026-10-31"} {
		e.Must(201, "POST", "/v1/revaluation-runs", fmt.Sprintf(`{"legal_entity_id": %q, "key_date": %q}`, e.US, day), "")
	}
}

// ledgerOf is a checksum of a company's ledger: its journal lines and its
// projected balances, counted and summed in each currency.
func ledgerOf(t *testing.T, e fixture.Env, company uuid.UUID) string {
	t.Helper()
	var lines, balances string
	pgtest.Scan(t, `SELECT count(*) || ' lines ' || sum(abs(l.amount_tc)) || ' ' || sum(abs(l.amount_lc)) || ' ' || sum(abs(l.amount_gc))
		FROM ledger.journal_line l JOIN ledger.journal_entry en ON en.tenant_id = l.tenant_id AND en.fiscal_year = l.fiscal_year AND en.id = l.journal_entry_id
		WHERE l.tenant_id = $1 AND en.legal_entity_id = $2`, []any{e.Tenant.ID, company}, &lines)
	pgtest.Scan(t, `SELECT count(*) || ' balances ' || coalesce(sum(debit_lc), 0) || ' ' || coalesce(sum(credit_lc), 0) || ' ' || coalesce(sum(debit_gc), 0)
			|| ' ' || coalesce(sum(credit_gc), 0)
		FROM ledger.period_balance WHERE tenant_id = $1 AND legal_entity_id = $2`, []any{e.Tenant.ID, company}, &balances)
	return lines + "; " + balances
}

// carryOutTwice carries a run out as carryOut does, each step run twice,
// as an activity whose completion was lost is (ADR-0012).
func carryOutTwice(t *testing.T, e fixture.Env, h *consolidation.Endpoints, run uuid.UUID) {
	t.Helper()
	twice := func(name string, fn func(ctx context.Context) error) {
		t.Helper()
		for range 2 {
			if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, fn); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
	}
	var companies []uuid.UUID
	twice("prepare", func(ctx context.Context) error {
		var err error
		companies, err = h.PrepareConsolidationRun(ctx, run)
		return err
	})
	for _, c := range companies {
		twice("translate "+c.String(), func(ctx context.Context) error { return h.TranslateMember(ctx, run, c) })
	}
	twice("eliminate", func(ctx context.Context) error { return h.EliminateConsolidationRun(ctx, run) })
	twice("complete", func(ctx context.Context) error { return h.CompleteConsolidationRun(ctx, run) })
}

// companyRows are a run's translated rows of company 1000, as
// balanceLines shows them, and the totals of its profit and loss rows of
// the year: booked and translated.
func companyRows(t *testing.T, e fixture.Env, run uuid.UUID) (rows, profit string) {
	t.Helper()
	path := fmt.Sprintf("%s/%s/balances?legal_entity_id=%s", runsPath, run, e.US)
	page, _ := e.Must(200, "GET", path, "", "")
	var booked, translated int64
	for _, account := range []string{"G4000", "G5000"} {
		p, _ := e.Must(200, "GET", path+"&kind=balance&group_account="+account, "", "")
		booked += minorOf(t, p["totals"], "group_booked")
		translated += minorOf(t, p["totals"], "group")
	}
	return strings.Join(balanceLines(page), "\n"), fmt.Sprintf("booked %d, translated %d", booked, translated)
}

// movedLines shows what a profit and loss says a run moved out of the
// profit: per company of its translation reserve, and in its totals.
func movedLines(pl map[string]any) string {
	var out []string
	for _, it := range pl["translation_reserve"].([]any) {
		v := it.(map[string]any)
		out = append(out, fmt.Sprintf("%s %s + %s = %s, moved %s", v["company_code"], moneyOf(v["opening"]), moneyOf(v["difference"]), moneyOf(v["closing"]),
			moneyOf(v["revaluation_translation"])))
	}
	tot := pl["totals"].(map[string]any)
	return strings.Join(out, "; ") + "; totals " + columnsOf(tot["revaluation_translation"]) + "; profit " + columnsOf(tot["profit"]) +
		"; other comprehensive income " + columnsOf(tot["other_comprehensive_income"])
}

// TestRevaluationTranslationPart: revaluedFigures' October run, each step
// run twice.
//
// Company 1000's bank is USD 2,500.00 (500.00, the euros' 1,250.00 and
// October's gain of 750.00; September's is reset), 1,250.00 EUR at 0.50.
// Its results before October, its first period, are the revenue of
// 1,000.00 EUR and September's gain, booked with no group amount: the
// run keeps the gain's USD 312.50 at its entry's 0.64, 200.00 EUR, in
// the results, 1,200.00 EUR, and takes the difference to the reserve.
// The year's profit is 480000's: USD 312.50 debit (the reset) and 750.00
// credit, booked 0.00 EUR; the run keeps 200.00 debit and 375.00 credit,
// a profit of 175.00 EUR, and moves the difference, 175.00 debit, to the
// reserve. The reserve is what balances the company: 1,250.00 of assets
// against 400.00 of share capital, 1,200.00 of earlier results and
// 175.00 of profit, 525.00 debit; as booked it was 150.00, and the
// earlier results' 200.00 and the year's 175.00 are the rest.
//
// So the consolidated profit is 175.00 where the booked group amounts
// gave none, other comprehensive income holds the 175.00 moved, the
// balance sheet sums to zero, and company 1000's ledger is as it was.
// With October's revaluation run cancelled, its entries' reversals take
// their parts back: the year moves the reset's 200.00 credit alone.
func TestRevaluationTranslationPart(t *testing.T) {
	e := revaluedEnv(t)
	h := app.Consolidation(app.Config{})
	before := ledgerOf(t, e, e.US)
	run := startPeriod(t, e, 2026, 10)
	carryOutTwice(t, e, h, run)
	t.Run("rows", func(t *testing.T) {
		rows, profit := companyRows(t, e, run)
		want := strings.Join([]string{
			"1000 Acme Corp US #1 balance G1000 Other assets 100000 Bank - Operating: 2500.00 USD / 1400.00 EUR / 1250.00 EUR at 0.5",
			"1000 Acme Corp US #2 balance G3000 Share capital 300000 Share Capital: -500.00 USD / -400.00 EUR / -400.00 EUR",
			"1000 Acme Corp US #3 prior_results G3800 Retained earnings: -1562.50 USD / -1000.00 EUR / -1200.00 EUR",
			"1000 Acme Corp US #4 cta G3900 Currency translation reserve: - / - / 525.00 EUR",
			"1000 Acme Corp US #5 balance G4000 Revenue 480000 Unrealised exchange gains: -437.50 USD / 0.00 EUR / -175.00 EUR",
		}, "\n")
		if rows != want {
			t.Errorf("company 1000's rows:\ngot\n%s\nwant\n%s", rows, want)
		}
		if w := "booked 0, translated -17500"; profit != w {
			t.Errorf("company 1000's profit and loss rows: got %s, want %s", profit, w)
		}
	})
	t.Run("statements", func(t *testing.T) {
		bs, pl := statements(t, e, run)
		balanced(t, bs, pl)
		want := "1000 0.00 EUR + 525.00 EUR = 525.00 EUR, moved 175.00 EUR; 2000 0.00 EUR + 0.00 EUR = 0.00 EUR, moved 0.00 EUR; " +
			"totals 175.00 EUR / 0.00 EUR / 175.00 EUR; profit -175.00 EUR / 0.00 EUR / -175.00 EUR; " +
			"other comprehensive income 525.00 EUR / 0.00 EUR / 525.00 EUR"
		if got := movedLines(pl); got != want {
			t.Errorf("the profit and loss:\ngot  %s\nwant %s", got, want)
		}
		if _, ok := bs["totals"].(map[string]any)["revaluation_translation"]; ok {
			t.Error("the balance sheet's totals: got revaluation_translation, want none")
		}
	})
	t.Run("the ledger", func(t *testing.T) {
		if got := ledgerOf(t, e, e.US); got != before {
			t.Errorf("company 1000's ledger after the run:\ngot  %s\nwant %s", got, before)
		}
	})
	t.Run("a cancelled revaluation run", func(t *testing.T) {
		october, etag := revaluedRun(t, e)
		e.Must(200, "POST", "/v1/revaluation-runs/"+october+":cancel", `{"reason": "October's rate was keyed wrongly."}`, etag)
		again := startPeriod(t, e, 2026, 10)
		carryOut(t, e, h, again)
		rows, profit := companyRows(t, e, again)
		if w := "1000 Acme Corp US #5 balance G4000 Revenue 480000 Unrealised exchange gains: 312.50 USD / 0.00 EUR / 200.00 EUR"; !strings.HasSuffix(rows, w) {
			t.Errorf("company 1000's rows:\ngot\n%s\nwant them to end with\n%s", rows, w)
		}
		if w := "booked 0, translated 20000"; profit != w {
			t.Errorf("company 1000's profit and loss rows: got %s, want %s", profit, w)
		}
		bs, pl := statements(t, e, again)
		balanced(t, bs, pl)
		want := "1000 0.00 EUR + 525.00 EUR = 525.00 EUR, moved -200.00 EUR; 2000 0.00 EUR + 0.00 EUR = 0.00 EUR, moved 0.00 EUR; " +
			"totals -200.00 EUR / 0.00 EUR / -200.00 EUR; profit 200.00 EUR / 0.00 EUR / 200.00 EUR; " +
			"other comprehensive income 525.00 EUR / 0.00 EUR / 525.00 EUR"
		if got := movedLines(pl); got != want {
			t.Errorf("the profit and loss:\ngot  %s\nwant %s", got, want)
		}
	})
}

// revaluedRun is company 1000's posted revaluation run of 31 October with
// its ETag.
func revaluedRun(t *testing.T, e fixture.Env) (id, etag string) {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/revaluation-runs?legal_entity_id="+e.US.String(), "", "")
	for _, it := range apitest.List(t, page, "items") {
		if r := it.(map[string]any); r["key_date"] == "2026-10-31" {
			id = r["id"].(string)
		}
	}
	_, etag = e.Must(200, "GET", "/v1/revaluation-runs/"+id, "", "")
	return id, etag
}
