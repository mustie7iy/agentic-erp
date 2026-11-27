package consolidation_test

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/consolidation"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

const sheetPath = "/v1/consolidation-groups/ACME/asset-history-sheet?fiscal_year="

// sheetPermissions are what the people of a sheet's tenant hold: a run's,
// and the assets' read the sheet also needs.
var sheetPermissions = append([]string{"assets.asset.read"}, runPermissions...)

// withSheet is withRuns' group ACME with fixture.AssetClasses' classes,
// company 1000 first consolidated from 2025-06.
func withSheet(doc *config.Config) {
	fixture.AssetClasses(doc)
	withRuns(doc)
	doc.Consolidation.Groups[0].Members[0].FirstPeriod = "2025-06"
}

// sheetEnv is a tenant with withSheet's group on one calendar whose
// people hold sheetPermissions, the average USD/EUR rates 0.80 from 1 May
// 2025, 0.81 from 1 August, 0.82 from 1 December 2025, 0.84 from 1 March
// 2026, 0.85 from 1 November and 0.95 from 1 December 2026, and the class
// totals sheetTotals writes.
func sheetEnv(t *testing.T) fixture.Env {
	t.Helper()
	e := fixture.NewWith(t, nil, withSheet, sheetPermissions...)
	sameCalendar(t, e)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		SELECT $1, gen_random_uuid(), 'average', 'USD', 'EUR', d::date, r, 'ECB', $2
		FROM (VALUES ('2025-05-01', 0.80), ('2025-08-01', 0.81), ('2025-12-01', 0.82), ('2026-03-01', 0.84), ('2026-11-01', 0.85),
			('2026-12-01', 0.95)) v (d, r)`, e.Tenant.ID, e.Tenant.Human.ID)
	sheetTotals(t, e)
	return e
}

// sheetTotals writes the class totals of the leading ledger by hand, each
// period's row and its year's (as keepTotals keeps them), a period's
// profit and loss columns with their booked group amounts. Company 1000
// (USD), MACHINES: 1,000.00 acquired in 2025/3, before its first period,
// and 100.00 depreciated in 2025/8 (booked EUR 80.50); 500.00 acquired in
// 2026/3, 20.01 depreciated in 2026/11 (booked 17.03), 200.00 of cost and
// 40.00 of accumulated depreciation retired in 2026/12, and an impairment
// loss of 10.00 and a reversal of 4.00 in 2026/13 (booked 9.60 and
// 3.90). Company 2000 (EUR): TOOLS, a class the configuration does not
// have, 300.00 acquired and 30.00 depreciated in 2026/6, its booked group
// amount written as 29.99 so that a sheet showing it rather than the
// local amount fails; LAND, 100.00 acquired and retired in 2025/2, so it
// opens 2026 with nothing.
func sheetTotals(t *testing.T, e fixture.Env) {
	t.Helper()
	var leading uuid.UUID
	pgtest.Scan(t, `SELECT id FROM ledger.ledger WHERE tenant_id = $1 AND is_leading`, []any{e.Tenant.ID}, &leading)
	for _, r := range []struct {
		company                                uuid.UUID
		class                                  string
		year, period                           int
		acq, ret, dep, imp, retiredAccumulated int64
		depGC, impGC, rev, revGC               int64
	}{
		{e.US, "MACHINES", 2025, 3, 100000, 0, 0, 0, 0, 0, 0, 0, 0}, {e.US, "MACHINES", 2025, 8, 0, 0, -10000, 0, 0, -8050, 0, 0, 0},
		{e.US, "MACHINES", 2026, 3, 50000, 0, 0, 0, 0, 0, 0, 0, 0}, {e.US, "MACHINES", 2026, 11, 0, 0, -2001, 0, 0, -1703, 0, 0, 0},
		{e.US, "MACHINES", 2026, 12, 0, -20000, 0, 0, 4000, 0, 0, 0, 0}, {e.US, "MACHINES", 2026, 13, 0, 0, 0, -1000, 0, 0, -960, 400, 390},
		{e.DE, "TOOLS", 2026, 6, 30000, 0, -3000, 0, 0, -2999, 0, 0, 0}, {e.DE, "LAND", 2025, 2, 10000, -10000, 0, 0, 0, 0, 0, 0, 0},
	} {
		for _, table := range []string{"class_period_total", "class_year_total"} {
			period := ", fiscal_period"
			if table == "class_year_total" {
				period = ""
			}
			pgtest.Exec(t, fmt.Sprintf(`INSERT INTO assets.%[1]s AS t (tenant_id, legal_entity_id, ledger_id, fiscal_year, class_code%[2]s,
					acquisitions, retired_cost, depreciation, impairment_losses, retired_accumulated, impairment_reversals)
				SELECT $1, $2, $3, $4, $5%[3]s, $6, $7, $8, $10, $11, $12 WHERE $9::smallint > 0
				ON CONFLICT (tenant_id, legal_entity_id, ledger_id, fiscal_year%[2]s, class_code) DO UPDATE SET acquisitions = t.acquisitions + EXCLUDED.acquisitions,
					retired_cost = t.retired_cost + EXCLUDED.retired_cost, depreciation = t.depreciation + EXCLUDED.depreciation,
					impairment_losses = t.impairment_losses + EXCLUDED.impairment_losses, retired_accumulated = t.retired_accumulated + EXCLUDED.retired_accumulated,
					impairment_reversals = t.impairment_reversals + EXCLUDED.impairment_reversals`,
				table, period, strings.Replace(period, "fiscal_period", "$9", 1)), e.Tenant.ID, r.company, leading, r.year, r.class, r.acq, r.ret, r.dep,
				r.period, r.imp, r.retiredAccumulated, r.rev)
		}
		pgtest.Exec(t, `UPDATE assets.class_period_total SET depreciation_gc = $6, impairment_losses_gc = $7, impairment_reversals_gc = $8
			WHERE tenant_id = $1 AND legal_entity_id = $2 AND fiscal_year = $3 AND fiscal_period = $4 AND class_code = $5`,
			e.Tenant.ID, r.company, r.year, r.period, r.class, r.depGC, r.impGC, r.revGC)
	}
}

// sheetColumns are the columns of a sheet's values in the response's
// order, cost then accumulated depreciation.
var sheetColumns = []string{"opening_cost", "acquisitions", "retirements", "transfers", "exchange_differences", "closing_cost", "opening_accumulated",
	"depreciation", "impairment_losses", "impairment_reversals", "retirements_accumulated", "transfers_accumulated", "exchange_differences_accumulated",
	"closing_accumulated", "opening_net_book_value", "closing_net_book_value"}

// valuesLine is a sheet's values as their amounts in sheetColumns' order,
// after the currency they are in.
func valuesLine(t *testing.T, v map[string]any) string {
	t.Helper()
	out := []string{v["closing_cost"].(map[string]any)["currency"].(string)}
	for _, c := range sheetColumns {
		out = append(out, v[c].(map[string]any)["amount"].(string))
	}
	return strings.Join(out, " ")
}

// reconciles checks that the values reconcile to the cent: each closing
// value is its opening value plus the year's columns, the exchange
// differences among them, and a net book value its cost plus its
// accumulated depreciation.
func reconciles(t *testing.T, name string, v map[string]any) {
	t.Helper()
	c := func(col string) int64 { return cents(t, v[col].(map[string]any)["amount"].(string)) }
	checks := []struct {
		what      string
		got, want int64
	}{
		{"closing cost", c("closing_cost"), c("opening_cost") + c("acquisitions") + c("retirements") + c("transfers") + c("exchange_differences")},
		{"closing accumulated depreciation", c("closing_accumulated"), c("opening_accumulated") + c("depreciation") + c("impairment_losses") +
			c("impairment_reversals") + c("retirements_accumulated") + c("transfers_accumulated") + c("exchange_differences_accumulated")},
		{"opening net book value", c("opening_net_book_value"), c("opening_cost") + c("opening_accumulated")},
		{"closing net book value", c("closing_net_book_value"), c("closing_cost") + c("closing_accumulated")},
	}
	for _, k := range checks {
		if k.got != k.want {
			t.Errorf("%s, %s: got %d, want %d", name, k.what, k.got, k.want)
		}
	}
}

// cents is a decimal amount of two digits in cents.
func cents(t *testing.T, amount string) int64 {
	t.Helper()
	r, ok := new(big.Rat).SetString(amount)
	if !ok {
		t.Fatalf("amount %q", amount)
	}
	r.Mul(r, big.NewRat(100, 1))
	if !r.IsInt() {
		t.Fatalf("amount %q has more than two digits", amount)
	}
	return r.Num().Int64()
}

// sheetLines reads a sheet as lines: each company's code, role, rates and
// totals, each class row's code, name and values, and the totals; and
// checks every row, company and the totals reconcile, and the companies'
// totals sum to the totals.
func sheetLines(t *testing.T, sheet map[string]any) []string {
	t.Helper()
	out := []string{fmt.Sprintf("%s %s %v %s..%s %s %s", sheet["group_code"], sheet["group_name"], sheet["fiscal_year"], sheet["start_date"],
		sheet["end_date"], sheet["currency"], sheet["rate_type"])}
	sum := map[string]int64{}
	for _, it := range sheet["companies"].([]any) {
		c := it.(map[string]any)
		var rates []string
		for _, r := range c["period_rates"].([]any) {
			p := r.(map[string]any)
			rates = append(rates, fmt.Sprintf("%v=%s@%s", p["period"], p["rate"], p["valid_from"]))
		}
		totals := c["totals"].(map[string]any)
		reconciles(t, "company "+c["company_code"].(string), totals)
		for _, col := range sheetColumns {
			sum[col] += cents(t, totals[col].(map[string]any)["amount"].(string))
		}
		out = append(out, fmt.Sprintf("%s %s %s opening %v@%v closing %s@%v periods %v: %s", c["company_code"], c["role"], c["local_currency"],
			c["opening_rate"], c["opening_rate_valid_from"], c["closing_rate"], c["closing_rate_valid_from"], rates, valuesLine(t, totals)))
	}
	for _, it := range sheet["items"].([]any) {
		r := it.(map[string]any)
		reconciles(t, "class "+r["class_code"].(string), r)
		out = append(out, fmt.Sprintf("%s %v: %s", r["class_code"], r["class_name"], valuesLine(t, r)))
	}
	totals := sheet["totals"].(map[string]any)
	reconciles(t, "the totals", totals)
	for _, col := range sheetColumns {
		if got := cents(t, totals[col].(map[string]any)["amount"].(string)); got != sum[col] {
			t.Errorf("the totals' %s: got %d, want the companies' %d", col, got, sum[col])
		}
	}
	return append(out, "totals: "+valuesLine(t, totals))
}

// TestConsolidatedAssetHistorySheet (ADR-0057 As built, the consolidated
// sheet; IAS 16.73(e)(viii)), computed by hand from sheetTotals. In 2026
// company 1000 (USD) opens at 2025/12's closing rate 0.82 (1,000.00 and
// -100.00: 820.00 and -82.00); its movements of the balances at their
// period's closing rates: 500.00 at 0.84 (420.00) and the retirement's
// -200.00 and 40.00 at 0.95 (-190.00, 38.00), so only periods 3 and 12
// need rates; its depreciation, impairment and reversal at their booked
// -17.03, -9.60 and 3.90 (ADR-0059 §7: income and expenses as booked); it
// closes at 0.95 (1,300.00 and -86.01: 1,235.00 and -81.7095: -81.71), the
// exchange differences 1,235.00 - 820.00 - 230.00 = 185.00 and
// -81.71 + 82.00 - 15.27 = -14.98. Company 2000 is at rate 1 with no
// differences, its depreciation its local -30.00, its TOOLS unnamed (not
// configured), its LAND, which opens with nothing and did not move, not
// shown. In 2025 company 1000, first consolidated from 2025/6, opens with
// what it held before, 1,000.00, at 2025/5's 0.80 (800.00), its
// depreciation booked -80.50, and closes at 0.82 (820.00 and -82.00),
// 2026's opening, its differences 20.00 and -1.50.
func TestConsolidatedAssetHistorySheet(t *testing.T) {
	e := sheetEnv(t)
	sheet, _ := e.Must(200, "GET", sheetPath+"2026", "", "")
	got := strings.Join(sheetLines(t, sheet), "\n")
	want := strings.Join([]string{
		"ACME Acme group 2026 2026-01-01..2026-12-31 EUR average",
		"2000 parent EUR opening <nil>@<nil> closing 1@<nil> periods []: EUR 0.00 300.00 0.00 0.00 0.00 300.00 0.00 -30.00 0.00 0.00 0.00 0.00 0.00 -30.00 0.00 270.00",
		"1000 member USD opening 0.82@2025-12-01 closing 0.95@2026-12-01 periods [3=0.84@2026-03-01 12=0.95@2026-12-01]: " +
			"EUR 820.00 420.00 -190.00 0.00 185.00 1235.00 -82.00 -17.03 -9.60 3.90 38.00 0.00 -14.98 -81.71 738.00 1153.29",
		"MACHINES Machinery and equipment: EUR 820.00 420.00 -190.00 0.00 185.00 1235.00 -82.00 -17.03 -9.60 3.90 38.00 0.00 -14.98 -81.71 738.00 1153.29",
		"TOOLS <nil>: EUR 0.00 300.00 0.00 0.00 0.00 300.00 0.00 -30.00 0.00 0.00 0.00 0.00 0.00 -30.00 0.00 270.00",
		"totals: EUR 820.00 720.00 -190.00 0.00 185.00 1535.00 -82.00 -47.03 -9.60 3.90 38.00 0.00 -14.98 -111.71 738.00 1423.29",
	}, "\n")
	if got != want {
		t.Errorf("the sheet of 2026:\ngot  %s\nwant %s", got, want)
	}
	sheet, _ = e.Must(200, "GET", sheetPath+"2025", "", "")
	got = strings.Join(sheetLines(t, sheet), "\n")
	want = strings.Join([]string{
		"ACME Acme group 2025 2025-01-01..2025-12-31 EUR average",
		"2000 parent EUR opening <nil>@<nil> closing 1@<nil> periods []: EUR 0.00 100.00 -100.00 0.00 0.00 0.00 0.00 0.00 0.00 0.00 0.00 0.00 0.00 0.00 0.00 0.00",
		"1000 member USD opening 0.8@2025-05-01 closing 0.82@2025-12-01 periods []: " +
			"EUR 800.00 0.00 0.00 0.00 20.00 820.00 0.00 -80.50 0.00 0.00 0.00 0.00 -1.50 -82.00 800.00 738.00",
		"LAND <nil>: EUR 0.00 100.00 -100.00 0.00 0.00 0.00 0.00 0.00 0.00 0.00 0.00 0.00 0.00 0.00 0.00 0.00",
		"MACHINES Machinery and equipment: EUR 800.00 0.00 0.00 0.00 20.00 820.00 0.00 -80.50 0.00 0.00 0.00 0.00 -1.50 -82.00 800.00 738.00",
		"totals: EUR 800.00 100.00 -100.00 0.00 20.00 820.00 0.00 -80.50 0.00 0.00 0.00 0.00 -1.50 -82.00 800.00 738.00",
	}, "\n")
	if got != want {
		t.Errorf("the sheet of 2025, company 1000's first year:\ngot  %s\nwant %s", got, want)
	}
}

// TestConsolidatedAssetHistorySheetRefusals: the sheet is refused as a run
// of the year's period 12 would be (a group the caller may not see whole
// unknown, a member on another calendar, a fiscal year the parent has no
// period 12 in), without the assets' read (403), for a missing fiscal
// year's first period or a rate a translation needs, and for an amount
// whose translation overflows.
func TestConsolidatedAssetHistorySheetRefusals(t *testing.T) {
	e := sheetEnv(t)
	e.As(e.Reader(t, "Reader of 2000", e.DE)).Problem(404, "consolidation.group_unknown", "GET", sheetPath+"2026", "", "")
	e.Problem(404, "consolidation.group_unknown", "GET", "/v1/consolidation-groups/NONE/asset-history-sheet?fiscal_year=2026", "", "")
	p := e.Problem(422, "consolidation.period_unknown", "GET", sheetPath+"2027", "", "")
	if f := p["errors"].([]any)[0].(map[string]any)["field"]; f != "fiscal_year" {
		t.Errorf("the refusal's field: got %v, want fiscal_year, the sheet's parameter", f)
	}
	t.Run("without the assets' read", func(t *testing.T) {
		e := fixture.NewWith(t, nil, withSheet, runPermissions...)
		e.Problem(403, "identity.permission_missing", "GET", sheetPath+"2026", "", "")
	})
	t.Run("on another calendar", func(t *testing.T) {
		e := fixture.NewWith(t, nil, withSheet, sheetPermissions...)
		e.Problem(422, "consolidation.calendar_mismatch", "GET", sheetPath+"2026", "", "")
	})
	for name, c := range map[string]struct {
		ledger func(consolidation.Ledger) consolidation.Ledger
		year   int32
		want   string
	}{
		"the year's first period gone": {func(l consolidation.Ledger) consolidation.Ledger { return hidden{l, e.DE, 2026, 1} }, 2026,
			"has no period 2026/1 in its leading ledger"},
		"the opening's period gone": {func(l consolidation.Ledger) consolidation.Ledger { return hidden{l, e.US, 2025, 12} }, 2026,
			"has no period 2025/12 in its leading ledger, the period before the opening of its 2026 asset history sheet"},
		"a movement's period gone": {func(l consolidation.Ledger) consolidation.Ledger { return hidden{l, e.US, 2026, 3} }, 2026,
			"has no period 2026/3 in its leading ledger, whose closing rate translates its asset movements of the period"},
		"an amount beyond the range": {func(l consolidation.Ledger) consolidation.Ledger { return scaled{l} }, 2026,
			"translate the asset class MACHINES of 1000"},
	} {
		t.Run(name, func(t *testing.T) {
			h := app.Consolidation(app.Config{})
			h.Ledger = c.ledger(h.Ledger)
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				_, err := h.GetConsolidatedAssetHistorySheet(ctx, gen.GetConsolidatedAssetHistorySheetParams{Code: "ACME", FiscalYear: c.year})
				return err
			})
			if !strings.Contains(fmt.Sprint(err), c.want) {
				t.Errorf("got %v, want %q", err, c.want)
			}
		})
	}
}

// TestConsolidatedAssetHistorySheetDatabaseFailures: every database
// statement of the sheet failing fails it; outside a unit of work it is
// refused.
func TestConsolidatedAssetHistorySheetDatabaseFailures(t *testing.T) {
	e := sheetEnv(t)
	h := app.Consolidation(app.Config{})
	read := func(ctx context.Context) error {
		_, err := h.GetConsolidatedAssetHistorySheet(ctx, gen.GetConsolidatedAssetHistorySheetParams{Code: "ACME", FiscalYear: 2026})
		return err
	}
	if err := read(context.Background()); err == nil || !strings.Contains(err.Error(), "outside a unit of work") {
		t.Errorf("outside a unit of work: got %v, want it refused", err)
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "the consolidated asset history sheet", read)
}

// hidden is the ledger without one company's period.
type hidden struct {
	consolidation.Ledger
	company      uuid.UUID
	year, period int16
}

func (h hidden) PeriodOf(ctx context.Context, company uuid.UUID, year, period int16) (consolidation.Period, bool, error) {
	if company == h.company && year == h.year && period == h.period {
		return consolidation.Period{}, false, nil
	}
	return h.Ledger.PeriodOf(ctx, company, year, period)
}

// scaled is the ledger with every rate a thousand million million times
// what is recorded, so a translated amount leaves the minor unit's range.
type scaled struct{ consolidation.Ledger }

func (s scaled) RateOn(ctx context.Context, rateType, from, to string, date time.Time) (consolidation.Rate, bool, error) {
	r, ok, err := s.Ledger.RateOn(ctx, rateType, from, to, date)
	if r.Rate != nil {
		r.Rate = new(big.Rat).Mul(r.Rate, big.NewRat(1_000_000_000_000_000, 1))
	}
	return r, ok, err
}
