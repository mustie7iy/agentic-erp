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

// runPermissions are what the people of a run's tenant hold.
var runPermissions = []string{"consolidation.read", "consolidation.run.manage", "ledger.entry.read", "ledger.entry.post", "platform.operation.read",
	"platform.operation.cancel"}

// withRuns is fixture.Consolidation's group ACME (company 2000, in EUR,
// the group currency, holding 80% of 1000, in USD, from 2026-11), with
// journal entry number ranges of 2025 for both companies, so they have
// that fiscal year's periods.
func withRuns(doc *config.Config) {
	fixture.Consolidation(doc)
	for _, r := range doc.NumberRanges {
		if r.FiscalYear == 2026 && config.NumberedPerYear(r.DocumentType) {
			r.FiscalYear = 2025
			doc.NumberRanges = append(doc.NumberRanges, r)
		}
	}
}

// runEnv is a tenant with withRuns' group whose people hold
// runPermissions, and the average USD/EUR rates 0.80 from 1 January 2025,
// 0.85 from 1 October 2026 and 0.90 from 30 November 2026, November's
// closing rate.
func runEnv(t *testing.T) fixture.Env {
	t.Helper()
	e := fixture.NewWith(t, nil, withRuns, runPermissions...)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2025-01-01', 0.8, 'ECB', $2),
		       ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-10-01', 0.85, 'ECB', $2),
		       ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-11-30', 0.9, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	return e
}

// sameCalendar puts company 1000 on company 2000's fiscal calendar, as a
// group's members are (ADR-0059 §1): the fixture gives each company its
// own, with the same periods.
func sameCalendar(t *testing.T, e fixture.Env) {
	t.Helper()
	pgtest.Exec(t, `UPDATE tenancy.legal_entity u SET fiscal_calendar_id = d.fiscal_calendar_id, version = u.version + 1
		FROM tenancy.legal_entity d WHERE u.id = $1 AND d.id = $2`, e.US, e.DE)
	pgtest.Exec(t, `UPDATE ledger.period_status u SET calendar_id = d.fiscal_calendar_id, version = u.version + 1
		FROM tenancy.legal_entity d WHERE u.legal_entity_id = $1 AND d.id = $2`, e.US, e.DE)
}

// post posts a manual entry of the company on the day in its currency:
// the debit account against the credit account, the credit line naming
// the trading partner, if any.
func post(t *testing.T, e fixture.Env, company uuid.UUID, code, currency, day, debit, credit, amount string, partner *uuid.UUID) {
	t.Helper()
	tp := ""
	if partner != nil {
		tp = fmt.Sprintf(`, "trading_partner_id": %q`, *partner)
	}
	e.Must(201, "POST", "/v1/journal-entries", fmt.Sprintf(`{"legal_entity_id": %q, "document_date": %q, "posting_date": %q, "currency": %q,
		"lines": [{"account_id": %q, "amount": {"amount": %q, "currency": %q}},
		{"account_id": %q, "amount": {"amount": "-%s", "currency": %q}%s}]}`, company, day, day, currency, e.Account(t, code, debit), amount, currency,
		e.Account(t, code, credit), amount, currency, tp), "")
}

// figures posts the scenario TestConsolidationRun computes by hand. In
// company 1000 (USD): 1,000.00 of revenue in June 2025 (at 0.80), share
// capital of 500.00 in January 2026 (0.80), an expense of 200.00 in
// October 2026, before its first period (0.85), and 300.00 of revenue
// from company 2000 in November 2026 (0.85), each against the bank; in
// company 2000 (EUR) 100.00 of revenue in November.
func figures(t *testing.T, e fixture.Env) {
	t.Helper()
	post(t, e, e.US, "1000", "USD", "2025-06-15", "100000", "400000", "1000.00", nil)
	post(t, e, e.US, "1000", "USD", "2026-01-05", "100000", "300000", "500.00", nil)
	post(t, e, e.US, "1000", "USD", "2026-10-15", "520000", "100000", "200.00", nil)
	post(t, e, e.US, "1000", "USD", "2026-11-10", "100000", "400000", "300.00", &e.DE)
	post(t, e, e.DE, "2000", "EUR", "2026-11-05", "100000", "400000", "100.00", nil)
	invest(t, e, "400.00")
}

// invest posts company 2000's investment in company 1000 on 150000
// against its bank on 5 November 2026. For figures, 400.00 EUR is 1000's
// share capital, USD 500.00 as booked at 0.80, which an incorporated
// member's investment is eliminated against (ADR-0059 §9, As built
// T68h2).
func invest(t *testing.T, e fixture.Env, amount string) {
	t.Helper()
	post(t, e, e.DE, "2000", "EUR", "2026-11-05", "150000", "100000", amount, nil)
}

// start starts November's run of ACME and returns it with its Operation.
func start(t *testing.T, e fixture.Env) (run, op uuid.UUID) {
	t.Helper()
	o, _ := e.Must(202, "POST", "/v1/consolidation-runs", `{"group_code": "ACME", "fiscal_year": 2026, "period": 11}`, "")
	return uuid.MustParse(strings.TrimPrefix(o["resource"].(string), "/v1/consolidation-runs/")), uuid.MustParse(o["id"].(string))
}

// carryOut carries a run out as its workflow does, each step committed in
// its own transaction as the tenant's person.
func carryOut(t *testing.T, e fixture.Env, h *consolidation.Endpoints, run uuid.UUID) {
	t.Helper()
	step := func(name string, fn func(ctx context.Context) error) {
		t.Helper()
		if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, fn); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	var companies []uuid.UUID
	step("prepare", func(ctx context.Context) error {
		var err error
		companies, err = h.PrepareConsolidationRun(ctx, run)
		return err
	})
	for _, c := range companies {
		step("translate "+c.String(), func(ctx context.Context) error { return h.TranslateMember(ctx, run, c) })
	}
	step("eliminate", func(ctx context.Context) error { return h.EliminateConsolidationRun(ctx, run) })
	step("complete", func(ctx context.Context) error { return h.CompleteConsolidationRun(ctx, run) })
}

// balanceLines reads a page of a run's translated balances as one line
// per row.
func balanceLines(page map[string]any) []string {
	var out []string
	amount := func(m any) string {
		if m == nil {
			return "-"
		}
		x := m.(map[string]any)
		return x["amount"].(string) + " " + x["currency"].(string)
	}
	for _, it := range page["items"].([]any) {
		b := it.(map[string]any)
		account, partner, rate := "", "", ""
		if a, ok := b["account"].(map[string]any); ok {
			account = " " + a["number"].(string) + " " + a["name"].(string)
		}
		if p, ok := b["trading_partner"].(map[string]any); ok {
			partner = " tp " + p["code"].(string) + " " + p["name"].(string)
		}
		if r, ok := b["closing_rate"].(string); ok {
			rate = " at " + r
		}
		g := b["group_account"].(map[string]any)
		out = append(out, fmt.Sprintf("%s %s #%v %s %s %s%s%s: %s / %s / %s%s", b["company_code"], b["company_name"], b["line_no"], b["kind"], g["number"],
			g["name"], account, partner, amount(b["amount_lc"]), amount(b["amount_gc_booked"]), amount(b["amount_gc"]), rate))
	}
	return out
}

// TestConsolidationRun (ADR-0059 §6, §7): November's run of ACME is
// queued with its Operation and, carried out, computed: company 2000, in
// the group currency, at rate 1 with no translation difference, its
// investment in 1000 among its rows; company
// 1000's bank at the closing rate 0.90, its share capital and November's
// revenue from 2000 at their booked group amounts, its revenue of 2025 and
// October's expense, before its first period, on retained earnings, and
// the difference on the reserve, each company's rows summing to zero. The
// run reads back with its companies, rates and periods; the balances list
// filters and totals.
func TestConsolidationRun(t *testing.T) {
	e := runEnv(t)
	sameCalendar(t, e)
	figures(t, e)
	h := app.Consolidation(app.Config{})
	run, op := start(t, e)
	queued, _ := e.Must(200, "GET", "/v1/consolidation-runs/"+run.String(), "", "")
	if got := fmt.Sprint(queued["status"], " ", queued["group_name"], " ", len(queued["members"].([]any))); got != "queued Acme group 0" {
		t.Errorf("the queued run: got %s, want queued, named, without members", got)
	}
	carryOut(t, e, h, run)
	got, tag := e.Must(200, "GET", "/v1/consolidation-runs/"+run.String(), "", "")
	if s := fmt.Sprint(got["status"], " ", got["period_start"], " ", got["period_end"], " ", got["group_currency"], " ", got["rate_type"]); s !=
		"computed 2026-11-01 2026-11-30 EUR average" {
		t.Errorf("the run: got %s, want computed for November in EUR at average rates", s)
	}
	if tag != `"3"` {
		t.Errorf("the run's ETag: got %s, want \"3\" (queued, running, computed)", tag)
	}
	var members []string
	for _, m := range got["members"].([]any) {
		x := m.(map[string]any)
		members = append(members, fmt.Sprint(x["company_code"], " ", x["company_name"], " ", x["local_currency"], " ", x["role"], " ", x["ownership_percent"],
			" ", x["first_fiscal_year"], "/", x["first_fiscal_period"], " ", x["closing_rate"], " ", x["rate_valid_from"], " ", x["period_status"]))
	}
	if s, want := strings.Join(members, " | "), "2000 Acme GmbH EUR parent <nil> <nil>/<nil> 1 <nil> open | 1000 Acme Corp US USD member 100 2026/11 0.9 2026-11-30 open"; s != want {
		t.Errorf("the members: got %s, want %s", s, want)
	}
	page, _ := e.Must(200, "GET", "/v1/consolidation-runs/"+run.String()+"/balances", "", "")
	want := []string{
		"1000 Acme Corp US #1 balance G1000 Other assets 100000 Bank - Operating: 1600.00 USD / 1285.00 EUR / 1440.00 EUR at 0.9",
		"1000 Acme Corp US #2 balance G3000 Share capital 300000 Share Capital: -500.00 USD / -400.00 EUR / -400.00 EUR",
		"1000 Acme Corp US #3 prior_results G3800 Retained earnings: -800.00 USD / -630.00 EUR / -630.00 EUR",
		"1000 Acme Corp US #4 cta G3900 Currency translation reserve: - / - / -155.00 EUR",
		"1000 Acme Corp US #5 balance G4000 Revenue 400000 Sales Revenue tp 2000 Acme GmbH: -300.00 USD / -255.00 EUR / -255.00 EUR",
		"2000 Acme GmbH #1 balance G1000 Other assets 100000 Bank - Operating: -300.00 EUR / -300.00 EUR / -300.00 EUR at 1",
		"2000 Acme GmbH #2 balance G1500 Investments in subsidiaries 150000 Investment in Acme Corp US: 400.00 EUR / 400.00 EUR / 400.00 EUR at 1",
		"2000 Acme GmbH #3 balance G4000 Revenue 400000 Sales Revenue: -100.00 EUR / -100.00 EUR / -100.00 EUR",
	}
	if s, w := strings.Join(balanceLines(page), "\n"), strings.Join(want, "\n"); s != w {
		t.Errorf("the translated balances: got\n%s\nwant\n%s", s, w)
	}
	totals := fmt.Sprint(page["totals"])
	if want := "map[group:map[amount:0.00 currency:EUR] group_booked:map[amount:0.00 currency:EUR] local:[map[amount:0.00 currency:EUR] map[amount:0.00 currency:USD]]]"; totals != want {
		t.Errorf("the totals: got %s, want %s", totals, want)
	}
	var opStatus string
	pgtest.Scan(t, `SELECT status || ' ' || resource FROM platform.operation WHERE id = $1`, []any{op}, &opStatus)
	if opStatus != "succeeded /v1/consolidation-runs/"+run.String() {
		t.Errorf("the Operation: got %s, want it succeeded with the run", opStatus)
	}
	balanceFilters(t, e, run)
}

// balanceFilters: a run's balances by company, kind, group account,
// account and trading partner, each with the totals of what it selects,
// and paged by company code and line.
func balanceFilters(t *testing.T, e fixture.Env, run uuid.UUID) {
	t.Helper()
	path := "/v1/consolidation-runs/" + run.String() + "/balances"
	for _, c := range []struct{ name, query, lines, totals string }{
		{"company 1000", "legal_entity_id=" + e.US.String(), "1000#1 1000#2 1000#3 1000#4 1000#5", "local USD 0.00, booked 0.00, group 0.00"},
		{"the translation differences", "kind=cta", "1000#4", "local , booked 0.00, group -155.00"},
		{"the revenue", "group_account=G4000", "1000#5 2000#3", "local EUR -100.00 USD -300.00, booked -355.00, group -355.00"},
		{"the bank of 2000", "account_id=" + e.Account(t, "2000", "100000").String(), "2000#1", "local EUR -300.00, booked -300.00, group -300.00"},
		{"trading partner 2000", "trading_partner_id=" + e.DE.String(), "1000#5", "local USD -300.00, booked -255.00, group -255.00"},
	} {
		t.Run(c.name, func(t *testing.T) {
			page, _ := e.In(t).Must(200, "GET", path+"?"+c.query, "", "")
			var lines []string
			for _, it := range page["items"].([]any) {
				b := it.(map[string]any)
				lines = append(lines, fmt.Sprint(b["company_code"], "#", b["line_no"]))
			}
			if got := strings.Join(lines, " "); got != c.lines {
				t.Errorf("the rows: got %s, want %s", got, c.lines)
			}
			tot := page["totals"].(map[string]any)
			var local []string
			for _, m := range tot["local"].([]any) {
				local = append(local, m.(map[string]any)["currency"].(string)+" "+m.(map[string]any)["amount"].(string))
			}
			got := fmt.Sprintf("local %s, booked %s, group %s", strings.Join(local, " "), tot["group_booked"].(map[string]any)["amount"],
				tot["group"].(map[string]any)["amount"])
			if got != c.totals {
				t.Errorf("the totals: got %s, want %s", got, c.totals)
			}
		})
	}
	var seen []string
	next := path + "?page_size=3"
	for next != "" {
		page, _ := e.Must(200, "GET", next, "", "")
		for _, it := range page["items"].([]any) {
			b := it.(map[string]any)
			seen = append(seen, fmt.Sprint(b["company_code"], "#", b["line_no"]))
		}
		next = ""
		if tok, ok := page["next_page_token"].(string); ok {
			next = path + "?page_size=3&page_token=" + tok
		}
	}
	if got, want := strings.Join(seen, " "), "1000#1 1000#2 1000#3 1000#4 1000#5 2000#1 2000#2 2000#3"; got != want {
		t.Errorf("the pages: got %s, want %s", got, want)
	}
}

// TestConsolidationRunProjected: a run reads the projected balances and
// the lines not projected yet alike (ledger.ConsolidationReads,
// BalancesThrough): TestConsolidationRun's figures, most of them
// projected and November's revenue from 2000 half projected, half
// pending, translate as when all are pending, the trading partner's
// projected and pending parts taken off the account's total once. A
// special period of an earlier year counts among the earlier results; one
// of the run's year (after its regular periods) does not: 50.00 of
// expenses in period 13 of 2025 (at 40.00 EUR) and 70.00 in period 13 of
// 2026, written as projected balances.
func TestConsolidationRunProjected(t *testing.T) {
	e := runEnv(t)
	sameCalendar(t, e)
	post(t, e, e.US, "1000", "USD", "2025-06-15", "100000", "400000", "1000.00", nil)
	post(t, e, e.US, "1000", "USD", "2026-01-05", "100000", "300000", "500.00", nil)
	post(t, e, e.US, "1000", "USD", "2026-10-15", "520000", "100000", "200.00", nil)
	post(t, e, e.US, "1000", "USD", "2026-11-10", "100000", "400000", "100.00", &e.DE)
	fixture.Project(t)
	post(t, e, e.US, "1000", "USD", "2026-11-10", "100000", "400000", "200.00", &e.DE)
	post(t, e, e.DE, "2000", "EUR", "2026-11-05", "100000", "400000", "100.00", nil)
	invest(t, e, "400.00")
	for _, p := range []struct {
		year       int
		lc, gc     int64
		currencyTC string
	}{{2025, 5000, 4000, "USD"}, {2026, 7000, 6300, "USD"}} {
		pgtest.Exec(t, `INSERT INTO ledger.period_balance (tenant_id, ledger_id, legal_entity_id, account_id, fiscal_year, fiscal_period, currency_tc,
				debit_tc, debit_lc, debit_gc)
			SELECT $1, id, $2, $3, $4, 13, $5, $6, $6, $7 FROM ledger.ledger WHERE tenant_id = $1 AND is_leading`,
			e.Tenant.ID, e.US, e.Account(t, "1000", "520000"), p.year, p.currencyTC, p.lc, p.gc)
	}
	h := app.Consolidation(app.Config{})
	run, _ := start(t, e)
	carryOut(t, e, h, run)
	page, _ := e.Must(200, "GET", "/v1/consolidation-runs/"+run.String()+"/balances", "", "")
	want := []string{
		"1000 Acme Corp US #1 balance G1000 Other assets 100000 Bank - Operating: 1600.00 USD / 1285.00 EUR / 1440.00 EUR at 0.9",
		"1000 Acme Corp US #2 balance G3000 Share capital 300000 Share Capital: -500.00 USD / -400.00 EUR / -400.00 EUR",
		"1000 Acme Corp US #3 prior_results G3800 Retained earnings: -750.00 USD / -590.00 EUR / -590.00 EUR",
		"1000 Acme Corp US #4 cta G3900 Currency translation reserve: - / - / -195.00 EUR",
		"1000 Acme Corp US #5 balance G4000 Revenue 400000 Sales Revenue tp 2000 Acme GmbH: -300.00 USD / -255.00 EUR / -255.00 EUR",
		"2000 Acme GmbH #1 balance G1000 Other assets 100000 Bank - Operating: -300.00 EUR / -300.00 EUR / -300.00 EUR at 1",
		"2000 Acme GmbH #2 balance G1500 Investments in subsidiaries 150000 Investment in Acme Corp US: 400.00 EUR / 400.00 EUR / 400.00 EUR at 1",
		"2000 Acme GmbH #3 balance G4000 Revenue 400000 Sales Revenue: -100.00 EUR / -100.00 EUR / -100.00 EUR",
	}
	if s, w := strings.Join(balanceLines(page), "\n"), strings.Join(want, "\n"); s != w {
		t.Errorf("the translated balances: got\n%s\nwant\n%s", s, w)
	}
}
