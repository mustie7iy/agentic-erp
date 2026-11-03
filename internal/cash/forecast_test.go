package cash_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// withForecast is withPosition with Maple Inc running payments on
// Tuesdays and Thursdays.
func withForecast(doc *config.Config) {
	withPosition(doc)
	doc.Company("3000").Cash.PaymentDays = []string{"tue", "thu"}
}

// forecastEnv is Maple Inc read at readAt (Friday 17 April 2026; Monday
// the 20th a holiday) with its supplier paid by method T, which ranks MAIN
// in CAD and USD (a day at the bank) in USD, and its vendor blocked for
// payment.
type forecastEnv struct {
	positionEnv
	payables uuid.UUID
}

func setupForecast(t *testing.T, extend func(*config.Config), permissions ...string) forecastEnv {
	t.Helper()
	e := forecastEnv{positionEnv: setupPositionWith(t, extend, append([]string{"cash.forecast.read"}, permissions...)...)}
	e.payables = e.Account(t, "3000", "211000")
	e.exec(t, `UPDATE partner.supplier_company SET payment_methods = '{T}', version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2`,
		e.Tenant.ID, e.le)
	e.exec(t, `UPDATE partner.supplier_company SET is_payment_blocked = true, version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
		AND partner_id = $3`, e.Tenant.ID, e.le, e.Vendor)
	return e
}

// terms are an item's payment terms: net days from the baseline date and
// an optional discount tier.
type terms struct {
	baseline      string
	net, discDays int
	discPct       string
}

// item writes a supplier open item of the partner straight into the
// leading ledger: amount (negative for an invoice) on the payables account
// against the expense account, dated day, with the terms kept beside it.
func (e forecastEnv) item(t *testing.T, partner uuid.UUID, day, currency string, amount int64, tm *terms) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	e.exec(t, `SET session_replication_role = replica`)
	defer e.exec(t, `RESET session_replication_role`)
	e.exec(t, `INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type, document_number,
			document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, source_document_id, posted_by)
		VALUES ($1, 2026, $2, $3, $4, extract(month FROM $5::date), 'KR', $8, $5, $5, $6, 'CAD', 'EUR', 1, 1, 'payables', gen_random_uuid(), $7)`,
		e.Tenant.ID, id, e.le, e.leading, day, currency, e.Tenant.Human.ID, id.String())
	e.exec(t, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period, posting_date,
			account_id, partner_id, amount_tc, amount_lc, amount_gc)
		SELECT $1, 2026, $2, n, $3, $4, extract(month FROM $5::date), $5, CASE WHEN n = 1 THEN $6::uuid ELSE $7::uuid END,
			CASE WHEN n = 1 THEN $8::uuid END, CASE WHEN n = 1 THEN $9::bigint ELSE -$9::bigint END, CASE WHEN n = 1 THEN $9::bigint ELSE -$9::bigint END,
			CASE WHEN n = 1 THEN $9::bigint ELSE -$9::bigint END
		FROM generate_series(1, 2) n`, e.Tenant.ID, id, e.le, e.leading, day, e.payables, e.expense, partner, amount)
	if tm != nil {
		var days, pct any
		if tm.discPct != "" {
			days, pct = tm.discDays, tm.discPct
		}
		e.exec(t, `INSERT INTO ledger.open_item_terms (tenant_id, fiscal_year, journal_entry_id, line_no, payment_term_code, baseline_date, net_days,
				discount_days, discount_pct) VALUES ($1, 2026, $2, 1, 'T', $3, $4, $5, $6)`, e.Tenant.ID, id, tm.baseline, tm.net, days, pct)
	}
	return id
}

// forecastScenario writes Maple Inc's supplier items and runs:
//   - CAD -1,000.00 of 1 April, 2% until the 21st (a Tuesday), net 30:
//     paid on the 21st within the tier, 980.00;
//   - CAD -500.00 of 10 April, 2% until Monday the 20th (whose last payment
//     day, Thursday the 16th, is before the read), net 30 (Sunday 10 May):
//     paid in full on Thursday 7 May;
//   - CAD -300.00 of 1 March without terms, NT30: overdue, paid on the next
//     payment day, Tuesday the 21st; and a CAD 100.00 credit memo of 15
//     April netted then: one payment of 1,180.00 that day with the tier's;
//   - USD -2,000.00 of 1 April, NT30 (Friday 1 May): paid Thursday 30 April,
//     at the USD bank a day later;
//   - CAD -4,000.00 of 30 April due in 365 days (Friday 30 April 2027,
//     paid the Thursday before): beyond the horizon, which ends on 16 April
//     2027;
//   - the vendor's USD -700.00, blocked for payment: apart;
//   - CAD -800.00 held by a proposed run, whose CAD 800.00 payment counts on
//     the run date, the 17th, instead.
//
// The vendor's item is shown apart on its net due date, Friday 1 May, and
// summed in blocked, outside the totals.
func (e forecastEnv) forecastScenario(t *testing.T) {
	t.Helper()
	e.item(t, e.Supplier, "2026-04-01", "CAD", -100000, &terms{baseline: "2026-04-01", net: 30, discDays: 20, discPct: "2"})
	e.item(t, e.Supplier, "2026-04-10", "CAD", -50000, &terms{baseline: "2026-04-10", net: 30, discDays: 10, discPct: "2"})
	e.item(t, e.Supplier, "2026-03-01", "CAD", -30000, nil)
	e.item(t, e.Supplier, "2026-04-15", "CAD", 10000, nil)
	e.item(t, e.Supplier, "2026-04-01", "USD", -200000, nil)
	e.item(t, e.Supplier, "2026-04-30", "CAD", -400000, &terms{baseline: "2026-04-30", net: 365})
	e.item(t, e.Vendor, "2026-04-01", "USD", -70000, nil)
	held := e.item(t, e.Supplier, "2026-04-01", "CAD", -80000, nil)
	pay := e.payment(t, e.run(t, e.main, "2026-04-17", "CAD", "proposed"), 80000, "CAD", nil)
	e.exec(t, `INSERT INTO payments.payment_item (tenant_id, payment_id, item_fiscal_year, item_entry_id, item_line_no, amount, due_date)
		VALUES ($1, $2, 2026, $3, 1, 80000, '2026-05-01')`, e.Tenant.ID, pay, held)
}

// forecastOf reads the forecast as the human.
func (e forecastEnv) forecastOf(t *testing.T, query string) map[string]any {
	t.Helper()
	f, _ := e.Must(200, "GET", "/v1/liquidity-forecast?legal_entity_id="+e.le.String()+query, "", "")
	return f
}

// flowsOf renders a forecast's currencies as "currency: bucket flows"
// lines for the buckets with flows, then the total and what is beyond.
func flowsOf(t *testing.T, f map[string]any) []string {
	t.Helper()
	buckets := f["buckets"].([]any)
	var out []string
	for _, c := range f["currencies"].([]any) {
		cm := c.(map[string]any)
		for i, b := range cm["buckets"].([]any) {
			bm, col := b.(map[string]any), buckets[i].(map[string]any)
			if bm["start"] != col["start"] {
				t.Errorf("bucket %d of %s: starts %v, the column %v", i, cm["currency"], bm["start"], col["start"])
			}
			var flows []string
			for _, fl := range bm["flows"].([]any) {
				fm := fl.(map[string]any)
				flows = append(flows, fmt.Sprintf("%s/%s/late=%v %s x%v", fm["source"], fm["certainty"], fm["late"], amount(fm["amount"]), fm["count"]))
			}
			if len(flows) > 0 {
				out = append(out, fmt.Sprintf("%s %s %s..%s: %s = %s", cm["currency"], col["grain"], col["start"], col["end"], strings.Join(flows, ", "),
					amount(bm["total"])))
			}
		}
		beyond, blocked := cm["beyond"].(map[string]any), cm["blocked"].(map[string]any)
		out = append(out, fmt.Sprintf("%s total %s, beyond %s x%v, blocked %s x%v", cm["currency"], amount(cm["total"]), amount(beyond["amount"]),
			beyond["count"], amount(blocked["amount"]), blocked["count"]))
	}
	return out
}

// noUSD is the USD bank's currency in a forecast with no USD flow: listed
// for its opening (T53e4), with nothing in its buckets.
const noUSD = "USD total 0.00 USD, beyond 0.00 USD x0, blocked 0.00 USD x0"

// TestLiquidityForecast: Maple Inc's forecast per currency in its default
// buckets (ADR-0051 §5, §9): each supplier item expected when a run would
// pay it, at what the run would pay, credit memos netting, items held by a
// run counted as the run's payment, a blocked supplier's shown apart, and
// what falls after the last bucket beyond it.
func TestLiquidityForecast(t *testing.T) {
	e := setupForecast(t, withForecast)
	e.forecastScenario(t)
	f := e.forecastOf(t, "")
	want(t, "header", fmt.Sprint(f["read_date"], " ", f["time_zone"], " ", f["min_certainty"], " ", len(f["buckets"].([]any))), "2026-04-17 UTC planned 34")
	bs := f["buckets"].([]any)
	want(t, "buckets", fmt.Sprint(bs[0], bs[13], bs[14], bs[24], bs[25], bs[33]), "map[end:2026-04-17 grain:day start:2026-04-17] "+
		"map[end:2026-04-30 grain:day start:2026-04-30] map[end:2026-05-07 grain:week start:2026-05-01] map[end:2026-07-16 grain:week start:2026-07-10] "+
		"map[end:2026-08-16 grain:month start:2026-07-17] map[end:2027-04-16 grain:month start:2027-03-17]")
	want(t, "flows", flowsOf(t, f), []string{
		"CAD day 2026-04-17..2026-04-17: payment_run/confirmed/late=false -800.00 CAD x1 = -800.00 CAD",
		"CAD day 2026-04-21..2026-04-21: payable/expected/late=false -1180.00 CAD x1 = -1180.00 CAD",
		"CAD week 2026-05-01..2026-05-07: payable/expected/late=false -500.00 CAD x1 = -500.00 CAD",
		"CAD total -2480.00 CAD, beyond -4000.00 CAD x1, blocked 0.00 CAD x0",
		"USD week 2026-05-01..2026-05-07: payable/expected/late=false -2000.00 USD x1, payable_blocked/expected/late=false -700.00 USD x1 = -2000.00 USD",
		"USD total -2000.00 USD, beyond 0.00 USD x0, blocked -700.00 USD x1",
	})
	want(t, "blocked not counted", f["include_blocked"], false)

	t.Run("the certainty cut", func(t *testing.T) {
		want(t, "confirmed and above", flowsOf(t, e.forecastOf(t, "&min_certainty=confirmed")), []string{
			"CAD day 2026-04-17..2026-04-17: payment_run/confirmed/late=false -800.00 CAD x1 = -800.00 CAD",
			"CAD total -800.00 CAD, beyond 0.00 CAD x0, blocked 0.00 CAD x0", noUSD,
		})
		want(t, "actual only", flowsOf(t, e.forecastOf(t, "&min_certainty=actual")), []string{"CAD total 0.00 CAD, beyond 0.00 CAD x0, blocked 0.00 CAD x0", noUSD})
		e.Problem(422, "request.validation_failed", "GET", "/v1/liquidity-forecast?legal_entity_id="+e.le.String()+"&min_certainty=likely", "", "")
	})
}

// TestLiquidityForecastNetting: a supplier's items net as a run nets them
// (ADR-0051 §5): a day whose items owe nothing net pays nothing, as the
// proposer excludes a supplier with a debit balance, and its credits are
// carried to the supplier's next day; what is still a debit balance after
// the last is no cash flow.
func TestLiquidityForecastNetting(t *testing.T) {
	e := setupForecast(t, withForecast)
	onlyCredit, moreCredit, later := e.NewSupplier(t, "BP90000011"), e.NewSupplier(t, "BP90000012"), e.NewSupplier(t, "BP90000013")
	e.item(t, onlyCredit, "2026-04-15", "CAD", 10000, nil)
	e.item(t, moreCredit, "2026-04-15", "CAD", 100000, nil)
	e.item(t, moreCredit, "2026-03-01", "CAD", -30000, nil)
	e.item(t, later, "2026-04-15", "CAD", 10000, nil)
	// 2% until Tuesday 5 May: the run of the 21st leaves it for its tier, so
	// the credit memo alone is a debit balance then, and nets on the 5th.
	e.item(t, later, "2026-04-15", "CAD", -100000, &terms{baseline: "2026-04-15", net: 30, discDays: 20, discPct: "2"})
	want(t, "flows", flowsOf(t, e.forecastOf(t, "")), []string{
		"CAD week 2026-05-01..2026-05-07: payable/expected/late=false -880.00 CAD x1 = -880.00 CAD",
		"CAD total -880.00 CAD, beyond 0.00 CAD x0, blocked 0.00 CAD x0", noUSD,
	})
}

// TestLiquidityForecastSettings: a company that takes no discounts expects
// an item on its net due date; one naming no payment days on the day
// itself; its own buckets are the columns; a run's payment dated before
// the read date is late, expected the working day after it.
func TestLiquidityForecastSettings(t *testing.T) {
	e := setupForecast(t, func(doc *config.Config) {
		withPosition(doc)
		no := false
		doc.Company("3000").Cash.TakeDiscounts = &no
		doc.Company("3000").Cash.Buckets = []config.Bucket{{Grain: config.GrainWeek, Count: 2}, {Grain: config.GrainMonth, Count: 1}}
	})
	e.item(t, e.Supplier, "2026-04-01", "CAD", -100000, &terms{baseline: "2026-04-01", net: 30, discDays: 20, discPct: "2"})
	e.item(t, e.Supplier, "2026-04-01", "USD", -50000, &terms{baseline: "2026-04-01", net: 20, discDays: 10, discPct: "2"})
	e.payment(t, e.run(t, e.usd, "2026-04-14", "USD", "proposed"), 30000, "USD", nil)
	f := e.forecastOf(t, "")
	want(t, "buckets", fmt.Sprint(f["buckets"]), "[map[end:2026-04-23 grain:week start:2026-04-17] map[end:2026-04-30 grain:week start:2026-04-24] "+
		"map[end:2026-05-31 grain:month start:2026-05-01]]")
	want(t, "flows", flowsOf(t, f), []string{
		"CAD month 2026-05-01..2026-05-31: payable/expected/late=false -1000.00 CAD x1 = -1000.00 CAD",
		"CAD total -1000.00 CAD, beyond 0.00 CAD x0, blocked 0.00 CAD x0",
		"USD week 2026-04-17..2026-04-23: payable/expected/late=false -500.00 USD x1, payment_run/confirmed/late=true -300.00 USD x1 = -800.00 USD",
		"USD total -800.00 USD, beyond 0.00 USD x0, blocked 0.00 USD x0",
	})
}

// TestLiquidityForecastRefusals: a legal entity the caller cannot act in
// is not found; sums beyond an amount are refused; an item whose payment
// cannot be computed, or a company zone that does not load, fails the read
// rather than forecasting a wrong amount or day.
func TestLiquidityForecastRefusals(t *testing.T) {
	e := setupForecast(t, withForecast)
	other := e.Reader(t, "reader of 1000", e.US)
	e.As(other).Problem(404, "request.not_found", "GET", "/v1/liquidity-forecast?legal_entity_id="+e.le.String(), "", "")
	e.Problem(404, "request.not_found", "GET", "/v1/liquidity-forecast?legal_entity_id="+uuid.NewString(), "", "")

	e.item(t, e.Supplier, "2026-04-01", "CAD", -9_000_000_000_000_000_000, nil)
	e.item(t, e.Supplier, "2026-04-02", "CAD", -9_000_000_000_000_000_000, nil)
	e.Problem(422, "cash.amount_too_large", "GET", "/v1/liquidity-forecast?legal_entity_id="+e.le.String(), "", "")

	endpoints := app.Cash(app.Config{Now: func() time.Time { return readAt }})
	endpoints.Payments = failingPays{endpoints.Payments}
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := endpoints.Forecast(ctx, cash.ForecastQuery{LegalEntity: e.le})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "not a percent") {
		t.Errorf("an item whose payment cannot be computed: got %v, want the read failed", err)
	}
	endpoints = app.Cash(app.Config{Now: func() time.Time { return readAt }})
	endpoints.Settings = zoneLost{endpoints.Settings}
	err = apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := endpoints.Forecast(ctx, cash.ForecastQuery{LegalEntity: e.le})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "America/Nowhere") {
		t.Errorf("a zone that does not load: got %v, want the company's zone refused", err)
	}
}

// failingPays is the payments port whose items cannot say what they pay.
type failingPays struct{ cash.Payments }

func (f failingPays) Payables(ctx context.Context, legalEntity uuid.UUID, on time.Time) ([]cash.Payable, error) {
	ps, err := f.Payments.Payables(ctx, legalEntity, on)
	for i := range ps {
		ps[i].Pays = func(time.Time) (money.Minor, error) { return 0, errors.New("not a percent") }
	}
	return ps, err
}

// TestLiquidityForecastFailures: the forecast fails with its ports.
func TestLiquidityForecastFailures(t *testing.T) {
	e := setupForecast(t, withForecast)
	e.forecastScenario(t)
	read := func(ctx context.Context) error {
		_, err := app.Cash(app.Config{Now: func() time.Time { return readAt }}).Forecast(ctx, cash.ForecastQuery{LegalEntity: e.le})
		return err
	}
	if err := read(context.Background()); !errors.Is(err, reqctx.ErrOutsideRequest) {
		t.Errorf("outside a unit of work: got %v, want reqctx.ErrOutsideRequest", err)
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "forecast", read)
}
