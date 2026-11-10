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
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// investedEnv is runEnv's tenant with figures posted (2000's investment
// in 1000 among them), its people also holding the finalization's
// permission, and November 2026 locked in both companies, so a run is
// finalized as soon as it is computed (figures has no intercompany
// difference).
func investedEnv(t *testing.T) fixture.Env {
	t.Helper()
	e := runEnv(t)
	e.Tenant.Grant(t, "consolidation.run.finalize", e.Tenant.Human)
	sameCalendar(t, e)
	figures(t, e)
	setPeriod(t, e, []uuid.UUID{e.US, e.DE}, "closed", "locked")
	return e
}

// setOctober moves October 2026's leading-ledger period of company 1000,
// the period its acquisition is measured at, to each status in turn.
func setOctober(t *testing.T, e fixture.Env, statuses ...string) {
	t.Helper()
	for _, s := range statuses {
		pgtest.Exec(t, `UPDATE ledger.period_status SET status = $3, version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
			AND fiscal_year = 2026 AND period = 10 AND ledger_id = (SELECT id FROM ledger.ledger WHERE tenant_id = $1 AND is_leading)`, e.Tenant.ID, e.US, s)
	}
}

// investmentLines are a run's investment entries, as entryLines shows
// them, each line with its component.
func investmentLines(t *testing.T, e fixture.Env, run uuid.UUID) string {
	t.Helper()
	page, _ := e.Must(200, "GET", runsPath+"/"+run.String()+"/entries?kind=investment", "", "")
	var out []string
	for _, it := range page["items"].([]any) {
		en := it.(map[string]any)
		out = append(out, fmt.Sprintf("entry %v %s %s, debits %s", en["entry_no"], en["kind"], en["company_code"], moneyOf(en["debits"])))
		for _, x := range en["lines"].([]any) {
			l := x.(map[string]any)
			account := ""
			if a, ok := l["account"].(map[string]any); ok {
				account = " " + a["number"].(string)
			}
			out = append(out, fmt.Sprintf("  %v %s %s%s %s: %s", l["line_no"], l["group_account"].(map[string]any)["number"], l["company_code"], account,
				l["component"], moneyOf(l["amount"])))
		}
	}
	return strings.Join(out, "\n")
}

// acquisitionOf shows a run's member 1000's acquisition.
func acquisitionOf(t *testing.T, e fixture.Env, run uuid.UUID) string {
	t.Helper()
	got, _ := e.Must(200, "GET", runsPath+"/"+run.String(), "", "")
	var out []string
	for _, m := range got["members"].([]any) {
		x := m.(map[string]any)
		a, ok := x["acquisition"].(map[string]any)
		if !ok {
			out = append(out, fmt.Sprint(x["company_code"], " none"))
			continue
		}
		out = append(out, fmt.Sprintf("%s at %v-%v, rate %s from %v: net assets %s = %s, investment %s, goodwill %s, interest %s, frozen %v",
			x["company_code"], a["fiscal_year"], a["period"], a["closing_rate"], a["rate_valid_from"], moneyOf(a["net_assets"]), moneyOf(a["net_assets_group"]),
			moneyOf(a["investment"]), moneyOf(a["goodwill"]), moneyOf(a["non_controlling_interest"]), a["frozen"]))
	}
	return strings.Join(out, " | ")
}

// TestInvestmentElimination (ADR-0059 §9, As built T68h2): figures'
// company 1000, first consolidated in November 2026, is measured at
// October, the period before: its bank's USD 1,300.00 (1,000.00 of 2025
// revenue, 500.00 of share capital, less October's 200.00 expense) at
// October's closing 0.85 is 1,105.00 EUR of net assets. The parent
// incorporated it, so 2000's investment of 400.00 is eliminated against
// its share capital alone, as booked (-400.00, B5(a)): the investment
// entry debits the share capital back and credits the investment, and
// its results through October (-630.00 on retained earnings) and its
// reserve stay the group's (TestConsolidatedStatements). The first final
// run freezes the acquisition; a later run reads it frozen, also once
// October has moved (a posting there changes nothing), and fails when
// 2000's investment changes (consolidation.investment_changed).
func TestInvestmentElimination(t *testing.T) {
	e := investedEnv(t)
	h := app.Consolidation(app.Config{})
	run := computed(t, e)
	want := "entry 1 investment 1000, debits 400.00 EUR\n" +
		"  1 G1500 2000 150000 investment: -400.00 EUR\n" +
		"  2 G3000 1000 acquisition_equity: 400.00 EUR"
	if got := investmentLines(t, e, run); got != want {
		t.Errorf("the investment entry: got\n%s\nwant\n%s", got, want)
	}
	measured := "2000 none | 1000 at 2026-10, rate 0.85 from 2026-10-01: net assets 1300.00 USD = 1105.00 EUR, investment 400.00 EUR, goodwill 0.00 EUR, " +
		"interest 0.00 EUR, frozen false"
	if got := acquisitionOf(t, e, run); got != measured {
		t.Errorf("the acquisition: got %s, want %s", got, measured)
	}
	_, tag := e.Must(200, "GET", runsPath+"/"+run.String(), "", "")
	p := e.Problem(409, "consolidation.period_not_locked", "POST", runsPath+"/"+run.String()+":finalize", "", tag)
	if d := p["detail"].(string); !strings.Contains(d, "company 1000's 2026/10 (open)") {
		t.Errorf("freezing from an open October: got %s, want 1000's October named", d)
	}
	setOctober(t, e, "closed", "locked")
	got := finalize(t, e, run)
	if by := got["members"].([]any)[1].(map[string]any)["acquisition"].(map[string]any)["frozen_by_run_id"]; got["status"] != "final" || by != run.String() {
		t.Fatalf("the run: got %v, frozen by %v, want final, frozen by itself", got["status"], by)
	}
	var frozen, equity string
	pgtest.Scan(t, `SELECT concat_ws(' ', origin, ownership_percent, first_fiscal_year, first_fiscal_period, local_currency, net_assets_lc, closing_rate,
			rate_valid_from, net_assets_gc, investment_gc, goodwill_gc, nci_gc, run_id = $2)
		FROM consolidation.acquisition WHERE tenant_id = $1 AND group_code = 'ACME' AND legal_entity_id = $3`, []any{e.Tenant.ID, run, e.US}, &frozen)
	if w := "incorporated 100.0000 2026 11 USD 130000 0.850000000000 2026-10-01 110500 40000 0 0 t"; frozen != w {
		t.Errorf("the frozen acquisition: got %s, want %s", frozen, w)
	}
	pgtest.Scan(t, `SELECT string_agg(group_account || '=' || amount_gc, ' ' ORDER BY group_account) FROM consolidation.acquisition_equity
		WHERE tenant_id = $1 AND legal_entity_id = $2`, []any{e.Tenant.ID, e.US}, &equity)
	if equity != "G3000=-40000" {
		t.Errorf("the frozen equity: got %s, want the share capital per group account", equity)
	}
	var audited int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'consolidation.acquisition.frozen' AND object_id = $2`,
		[]any{e.Tenant.ID, e.US}, &audited)
	if audited != 1 {
		t.Errorf("the freeze's audit records: got %d, want 1", audited)
	}
	// October moves: 1000's acquisition as frozen does not.
	// Unlocked as an approved unlock request does (finalize_test).
	pgtest.Exec(t, `UPDATE ledger.period_status SET unlock_request_id = gen_random_uuid(), version = version + 1 WHERE tenant_id = $1
		AND legal_entity_id = $2 AND fiscal_year = 2026 AND period = 10`, e.Tenant.ID, e.US)
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', unlock_request_id = NULL, version = version + 1 WHERE tenant_id = $1
		AND legal_entity_id = $2 AND fiscal_year = 2026 AND period = 10`, e.Tenant.ID, e.US)
	setOctober(t, e, "open")
	post(t, e, e.US, "1000", "USD", "2026-10-20", "100000", "400000", "100.00", nil)
	later := computed(t, e)
	if got := investmentLines(t, e, later); got != want {
		t.Errorf("the later run's investment entry: got\n%s\nwant\n%s", got, want)
	}
	if got, w := acquisitionOf(t, e, later), strings.Replace(measured, "frozen false", "frozen true", 1); got != w {
		t.Errorf("the later run's acquisition: got %s, want %s", got, w)
	}
	again, _ := e.Must(200, "GET", runsPath+"/"+later.String(), "", "")
	if by := again["members"].([]any)[1].(map[string]any)["acquisition"].(map[string]any)["frozen_by_run_id"]; by != run.String() {
		t.Errorf("the later run's acquisition: frozen by %v, want %s", by, run)
	}
	post(t, e, e.DE, "2000", "EUR", "2026-10-20", "150000", "100000", "10.00", nil)
	changed, _ := start(t, e)
	if err := eliminated(t, e, h, changed); problem.From(err).Code != "consolidation.investment_changed" ||
		!strings.Contains(problem.From(err).Detail, "410.00 EUR, differs from 400.00 EUR") {
		t.Errorf("a changed investment: got %v, want consolidation.investment_changed naming both", err)
	}
}

// eliminated carries a run out to its elimination step and returns that
// step's error.
func eliminated(t *testing.T, e fixture.Env, h *consolidation.Endpoints, run uuid.UUID) error {
	t.Helper()
	var companies []uuid.UUID
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		var err error
		companies, err = h.PrepareConsolidationRun(ctx, run)
		return err
	}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	for _, c := range companies {
		if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return h.TranslateMember(ctx, run, c) }); err != nil {
			t.Fatalf("translate %s: %v", c, err)
		}
	}
	return apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return h.EliminateConsolidationRun(ctx, run) })
}

// TestInvestmentFirstPeriodOne (ADR-0059 §9, As built T68h2): a member
// first consolidated in January 2026 is measured at December 2025, the
// previous fiscal year's last period, at its closing 0.82: 1000's bank of
// USD 1,500.00 (June 2025's revenue and share capital) is 1,230.00 EUR.
// November's run eliminates 2000's 400.00 against its share capital as
// booked at 0.80; 2025's revenue (-800.00) stays on the group's retained
// earnings, October's expense (170.00) in its results, and its reserve,
// -140.00, whole.
func TestInvestmentFirstPeriodOne(t *testing.T) {
	e := fixture.NewWith(t, nil, func(doc *config.Config) {
		withRuns(doc)
		doc.Consolidation.Groups[0].Members[0].FirstPeriod = "2026-01"
	}, runPermissions...)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2025-01-01', 0.8, 'ECB', $2),
		       ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2025-12-31', 0.82, 'ECB', $2),
		       ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-10-01', 0.85, 'ECB', $2),
		       ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-11-30', 0.9, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	sameCalendar(t, e)
	post(t, e, e.US, "1000", "USD", "2025-06-15", "100000", "400000", "1000.00", nil)
	post(t, e, e.US, "1000", "USD", "2025-06-15", "100000", "300000", "500.00", nil)
	post(t, e, e.US, "1000", "USD", "2026-10-15", "520000", "100000", "200.00", nil)
	invest(t, e, "400.00")
	run := computed(t, e)
	want := "entry 1 investment 1000, debits 400.00 EUR\n" +
		"  1 G1500 2000 150000 investment: -400.00 EUR\n" +
		"  2 G3000 1000 acquisition_equity: 400.00 EUR"
	if got := investmentLines(t, e, run); got != want {
		t.Errorf("the investment entry: got\n%s\nwant\n%s", got, want)
	}
	if got, w := acquisitionOf(t, e, run), "2000 none | 1000 at 2025-12, rate 0.82 from 2025-12-31: net assets 1500.00 USD = 1230.00 EUR, investment "+
		"400.00 EUR, goodwill 0.00 EUR, interest 0.00 EUR, frozen false"; got != w {
		t.Errorf("the acquisition: got %s, want %s", got, w)
	}
	bs, _ := e.Must(200, "GET", statementPath(run, "balance_sheet"), "", "")
	if got, w := statementRow(bs, "G3800")+", "+statementTotals(bs, "assets", "liabilities", "equity", "profit"), "G3800 Retained earnings equity "+
		"(retained_earnings): -800.00 EUR / 0.00 EUR / -800.00 EUR, assets 1170.00 EUR / -400.00 EUR / 770.00 EUR, liabilities 0.00 EUR / 0.00 EUR / "+
		"0.00 EUR, equity -1340.00 EUR / 400.00 EUR / -940.00 EUR, profit 170.00 EUR / 0.00 EUR / 170.00 EUR"; got != w {
		t.Errorf("the balance sheet: got %s, want %s", got, w)
	}
}

// TestInvestmentOpeningEntry (ADR-0059 As built T68h2; the pilot's case):
// company 1000's opening entry, migrated into November 2026, its first
// consolidated period, carries its bank of USD 1,500.00, its share capital
// of 500.00 and its retained earnings of 1,000.00 (on 330000, its retained
// earnings account, ADR-0060 §4), at the entry's 0.85 (po:2026-11-05).
// October, where the acquisition is measured, holds nothing; the share
// capital is read through the run's period, as the investment is, so
// 2000's 425.00 is eliminated against it alone, and the retained earnings
// (-850.00, on G3000 as the fixture maps every equity account) stay the
// group's. The elimination step run twice writes the entry and the
// acquisition once; the final run freezes the share capital.
func TestInvestmentOpeningEntry(t *testing.T) {
	e := fixture.NewWith(t, nil, func(doc *config.Config) {
		co := doc.Company("1000")
		co.ChartOfAccounts = append(co.ChartOfAccounts, config.Account{Number: "330000", Name: "Retained earnings", Type: config.Equity})
		co.YearEnd.RetainedEarningsAccount = "330000"
		withRuns(doc)
	}, runPermissions...)
	e.Tenant.Grant(t, "consolidation.run.finalize", e.Tenant.Human)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-10-01', 0.85, 'ECB', $2),
		       ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-11-30', 0.9, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	sameCalendar(t, e)
	e.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-11-01", "posting_date": "2026-11-01",
		"currency": "USD", "header_text": "Opening balances", "lines": [{"account_id": %q, "amount": {"amount": "1500.00", "currency": "USD"}},
		{"account_id": %q, "amount": {"amount": "-500.00", "currency": "USD"}}, {"account_id": %q, "amount": {"amount": "-1000.00", "currency": "USD"}}]}`,
		e.US, e.Account(t, "1000", "100000"), e.Account(t, "1000", "300000"), e.Account(t, "1000", "330000")), "")
	invest(t, e, "425.00")
	setPeriod(t, e, []uuid.UUID{e.US, e.DE}, "closed", "locked")
	setOctober(t, e, "closed", "locked")
	h := app.Consolidation(app.Config{})
	run, _ := start(t, e)
	for i := range 2 {
		if err := eliminatedAgain(t, e, h, run, i > 0); err != nil {
			t.Fatalf("eliminate %d: %v", i+1, err)
		}
	}
	var counts string
	pgtest.Scan(t, `SELECT (SELECT count(*) FROM consolidation.run_entry WHERE run_id = $1 AND kind = 'investment') || ' ' ||
		(SELECT count(*) FROM consolidation.run_acquisition WHERE run_id = $1)`, []any{run}, &counts)
	if counts != "1 1" {
		t.Errorf("investment entries and acquisitions after two eliminations: got %s, want 1 1", counts)
	}
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return h.CompleteConsolidationRun(ctx, run) }); err != nil {
		t.Fatal(err)
	}
	want := "entry 1 investment 1000, debits 425.00 EUR\n" +
		"  1 G1500 2000 150000 investment: -425.00 EUR\n" +
		"  2 G3000 1000 acquisition_equity: 425.00 EUR"
	if got := investmentLines(t, e, run); got != want {
		t.Errorf("the investment entry: got\n%s\nwant\n%s", got, want)
	}
	if got, w := acquisitionOf(t, e, run), "2000 none | 1000 at 2026-10, rate 0.85 from 2026-10-01: net assets 0.00 USD = 0.00 EUR, investment "+
		"425.00 EUR, goodwill 0.00 EUR, interest 0.00 EUR, frozen false"; got != w {
		t.Errorf("the acquisition: got %s, want %s", got, w)
	}
	bs, _ := e.Must(200, "GET", statementPath(run, "balance_sheet"), "", "")
	if got, w := statementRow(bs, "G3000")+", "+statementTotals(bs, "assets", "liabilities", "equity", "profit"), "G3000 Share capital equity: "+
		"-1275.00 EUR / 425.00 EUR / -850.00 EUR, assets 1350.00 EUR / -425.00 EUR / 925.00 EUR, liabilities 0.00 EUR / 0.00 EUR / 0.00 EUR, equity -1350.00 EUR / 425.00 EUR / -925.00 EUR, profit 0.00 EUR / "+
		"0.00 EUR / 0.00 EUR"; got != w {
		t.Errorf("the balance sheet: got %s, want %s", got, w)
	}
	if got := finalize(t, e, run); got["status"] != "final" {
		t.Fatalf("the run: got %v, want final", got["status"])
	}
	var equity string
	pgtest.Scan(t, `SELECT string_agg(group_account || '=' || amount_gc, ' ' ORDER BY group_account) FROM consolidation.acquisition_equity
		WHERE tenant_id = $1 AND legal_entity_id = $2`, []any{e.Tenant.ID, e.US}, &equity)
	if equity != "G3000=-42500" {
		t.Errorf("the frozen equity: got %s, want the share capital alone", equity)
	}
}

// eliminatedAgain carries a run out to its elimination step, or, again,
// runs that step alone, and returns its error.
func eliminatedAgain(t *testing.T, e fixture.Env, h *consolidation.Endpoints, run uuid.UUID, again bool) error {
	t.Helper()
	if !again {
		return eliminated(t, e, h, run)
	}
	return apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return h.EliminateConsolidationRun(ctx, run) })
}

// TestInvestmentRefusals: a member held at less than 100%, or acquired,
// needs goodwill or a non-controlling interest, not built, and a run of
// it is refused (consolidation.structure_unsupported); one without a
// closing rate for the period before its first, or without that period,
// is refused at creation; and a member whose investment differs from its
// share capital fails the elimination step
// (consolidation.investment_mismatch): an incorporated subsidiary has no
// goodwill, and the difference waits for the specialist.
func TestInvestmentRefusals(t *testing.T) {
	for _, c := range []struct{ name, ownership, origin, detail string }{
		{"held at 80%", "80", config.OriginIncorporated, "held at 80% and was incorporated by the parent"},
		{"acquired", "100", config.OriginAcquired, "held at 100% and was acquired"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := fixture.NewWith(t, nil, func(doc *config.Config) {
				withRuns(doc)
				m := &doc.Consolidation.Groups[0].Members[0]
				m.OwnershipPercent, m.Origin = c.ownership, c.origin
			}, runPermissions...)
			sameCalendar(t, e)
			p := e.Problem(422, "consolidation.structure_unsupported", "POST", runsPath, `{"group_code": "ACME", "fiscal_year": 2026, "period": 11}`, "")
			if d := p["detail"].(string); !strings.Contains(d, "Company 1000, a member of group ACME, is "+c.detail) || !strings.Contains(d, "B5-B7 and B9") {
				t.Errorf("the refusal: got %s, want the member and the leaves it waits for named", d)
			}
		})
	}
	t.Run("no rate for the period before the first", func(t *testing.T) {
		e := fixture.NewWith(t, nil, withRuns, runPermissions...)
		sameCalendar(t, e)
		pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
			VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2025-01-01', 0.8, 'ECB', $2),
			       ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-11-30', 0.9, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
		p := e.Problem(422, "consolidation.rate_stale", "POST", runsPath, `{"group_code": "ACME", "fiscal_year": 2026, "period": 11}`, "")
		if d := p["detail"].(string); !strings.Contains(d, "company 1000 for 2026/10") {
			t.Errorf("the refusal: got %s, want October's rate of 1000 named", d)
		}
	})
	t.Run("no period before the first", func(t *testing.T) {
		e := fixture.NewWith(t, nil, func(doc *config.Config) {
			fixture.Consolidation(doc)
			doc.Consolidation.Groups[0].Members[0].FirstPeriod = "2026-01"
		}, runPermissions...)
		sameCalendar(t, e)
		pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
			VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-11-30', 0.9, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
		p := e.Problem(422, "consolidation.period_unknown", "POST", runsPath, `{"group_code": "ACME", "fiscal_year": 2026, "period": 11}`, "")
		if d := p["detail"].(string); !strings.Contains(d, "Company 1000 has no period 2025/12") {
			t.Errorf("the refusal: got %s, want December 2025 of 1000 named", d)
		}
	})
	t.Run("an investment that is not the share capital", func(t *testing.T) {
		e := runEnv(t)
		sameCalendar(t, e)
		figures(t, e)
		invest(t, e, "5.00")
		run, _ := start(t, e)
		err := eliminated(t, e, app.Consolidation(app.Config{}), run)
		if p := problem.From(err); p.Code != "consolidation.investment_mismatch" || !strings.Contains(p.Detail, "405.00 EUR on account 150000, differs from "+
			"its share capital, 400.00 EUR") || !strings.Contains(p.Detail, "has no retained earnings account (companies[].year_end.retained_earnings_account)") {
			t.Errorf("the elimination: got %v, want consolidation.investment_mismatch naming both", err)
		}
	})
}

// TestFinalizeAgainstAnotherFrozen: two runs of November computed on
// different acquisitions of 1000 (October moved between them, 1000's share
// capital and 2000's investment with it); finalizing the first freezes its acquisition, and
// finalizing the second, whose acquisition differs, is refused
// (consolidation.acquisition_changed) rather than consolidated on figures
// other than the frozen ones. A run computed after the freeze finalizes.
func TestFinalizeAgainstAnotherFrozen(t *testing.T) {
	e := investedEnv(t)
	first := computed(t, e)
	post(t, e, e.US, "1000", "USD", "2026-10-20", "100000", "300000", "100.00", nil)
	post(t, e, e.DE, "2000", "EUR", "2026-10-20", "150000", "100000", "85.00", nil)
	second := computed(t, e)
	setOctober(t, e, "closed", "locked")
	if got := acquisitionOf(t, e, second); !strings.Contains(got, "net assets 1400.00 USD = 1190.00 EUR, investment 485.00 EUR") {
		t.Fatalf("the second run's acquisition: got %s, want October's move in it", got)
	}
	finalize(t, e, first)
	_, tag := e.Must(200, "GET", runsPath+"/"+second.String(), "", "")
	p := e.Problem(409, "consolidation.acquisition_changed", "POST", runsPath+"/"+second.String()+":finalize", "", tag)
	if d := p["detail"].(string); !strings.Contains(d, "of 1000 (frozen by run "+first.String()+")") {
		t.Errorf("the refusal: got %s, want 1000 and the freezing run named", d)
	}
	var stillFinal string
	pgtest.Scan(t, `SELECT status FROM consolidation.run WHERE id = $1`, []any{first}, &stillFinal)
	if stillFinal != "final" {
		t.Errorf("the first run after the refusal: got %s, want it final still", stillFinal)
	}
}

// TestInvestmentInGroupCurrency: a member in the group currency is
// measured at rate 1, its ledger's period before its first not needed:
// group EURO's 5000, held whole from June 2026, without postings, has an
// acquisition of nothing and no investment entry; a run of May, before
// its first period, has no member and no acquisition.
func TestInvestmentInGroupCurrency(t *testing.T) {
	e := fixture.NewWith(t, []fixture.Company{fixture.MapleCompany, france, italia}, func(doc *config.Config) {
		withGroups(doc)
		m := &doc.Consolidation.Groups[1].Members[0]
		m.OwnershipPercent, m.Origin = "100", config.OriginIncorporated
	}, runPermissions...)
	ids := entities(t, e, "4000", "5000")
	pgtest.Exec(t, `UPDATE tenancy.legal_entity u SET fiscal_calendar_id = d.fiscal_calendar_id, version = u.version + 1
		FROM tenancy.legal_entity d WHERE u.id = $1 AND d.id = $2`, ids[1], ids[0])
	pgtest.Exec(t, `UPDATE ledger.period_status u SET calendar_id = d.fiscal_calendar_id, version = u.version + 1
		FROM tenancy.legal_entity d WHERE u.legal_entity_id = $1 AND d.id = $2`, ids[1], ids[0])
	h := app.Consolidation(app.Config{})
	for _, c := range []struct {
		period int
		want   string
	}{{6, "4000 none | 5000 at 2026-5, rate 1 from <nil>: net assets 0.00 EUR = 0.00 EUR, investment 0.00 EUR, goodwill 0.00 EUR, interest 0.00 EUR, frozen false"},
		{5, "4000 none"}} {
		o, _ := e.Must(202, "POST", runsPath, fmt.Sprintf(`{"group_code": "EURO", "fiscal_year": 2026, "period": %d}`, c.period), "")
		run := uuid.MustParse(strings.TrimPrefix(o["resource"].(string), runsPath+"/"))
		carryOut(t, e, h, run)
		if got := acquisitionOf(t, e, run); got != c.want {
			t.Errorf("the run of period %d: got %s, want %s", c.period, got, c.want)
		}
		if got := investmentLines(t, e, run); got != "" {
			t.Errorf("the run of period %d's investment entries: got %s, want none", c.period, got)
		}
	}
}

// TestInvestmentStops: a member's investment account the parent's ledger
// does not have is the elimination step's error, and so is a balance of
// the member before its first period on an account without a group
// account (consolidation.account_unmapped).
func TestInvestmentStops(t *testing.T) {
	e := runEnv(t)
	sameCalendar(t, e)
	figures(t, e)
	h := app.Consolidation(app.Config{})
	for _, c := range []struct {
		name   string
		ledger func(consolidation.Ledger) consolidation.Ledger
		want   string
	}{
		{"no investment account", func(l consolidation.Ledger) consolidation.Ledger { return renamed{l} }, "150000 of member 1000 is not an account of the parent"},
		{"an unmapped account before the first period", func(l consolidation.Ledger) consolidation.Ledger { return renamedOf{l, e.US} }, "consolidation.account_unmapped"},
	} {
		t.Run(c.name, func(t *testing.T) {
			run, _ := start(t, e)
			stub := app.Consolidation(app.Config{})
			stub.Ledger = c.ledger(stub.Ledger)
			var companies []uuid.UUID
			if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				var err error
				companies, err = h.PrepareConsolidationRun(ctx, run)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for _, co := range companies {
				if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return h.TranslateMember(ctx, run, co) }); err != nil {
					t.Fatal(err)
				}
			}
			err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return stub.EliminateConsolidationRun(ctx, run) })
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("the elimination: got %v, want %s", err, c.want)
			}
			if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				return h.FailConsolidationRun(ctx, run, problem.Conflict("test.stopped", "stopped"))
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// renamedOf is the ledger with one company's accounts renumbered, so none
// of them has a group account in the configuration.
type renamedOf struct {
	consolidation.Ledger
	company uuid.UUID
}

func (r renamedOf) Accounts(ctx context.Context, company uuid.UUID) ([]consolidation.Account, error) {
	if company != r.company {
		return r.Ledger.Accounts(ctx, company)
	}
	return renamed{r.Ledger}.Accounts(ctx, company)
}
