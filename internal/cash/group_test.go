package cash_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// groupRange is the buckets every read of these tests asks for, so a
// company's own forecast and the group's have the same columns.
const groupRange = "grain=week&to=2026-07-31"

// setupGroup is the drill-down's Maple Inc (3000: CAD and USD, banks,
// runs, items, memo records) with memo records of Acme Corp US (1000) in
// USD and Acme GmbH (2000) in EUR, the group currency, and CAD's planning
// rate into EUR; USD has no rate into EUR.
func setupGroup(t *testing.T) drillEnv {
	t.Helper()
	e := setupDrill(t)
	for _, m := range []struct {
		le     uuid.UUID
		fields string
	}{
		{e.US, `"amount": {"amount": "1500.00", "currency": "USD"}, "value_date": "2026-04-21", "liquidity_item": "REC_CUSTOMERS", "reference": "US-1"`},
		{e.US, `"amount": {"amount": "-0.03", "currency": "USD"}, "value_date": "2026-04-22", "liquidity_item": "OP_SUPPLIERS", "certainty": "planned"`},
		{e.DE, `"amount": {"amount": "-700.00", "currency": "EUR"}, "value_date": "2026-05-05", "liquidity_item": "OP_SUPPLIERS", "reference": "DE-1"`},
		{e.DE, `"amount": {"amount": "90.00", "currency": "EUR"}, "value_date": "2027-06-01", "liquidity_item": "REC_CUSTOMERS"`},
	} {
		e.Must(201, "POST", "/v1/memo-records", `{"legal_entity_id": "`+m.le.String()+`", `+m.fields+`}`, "")
	}
	e.rate(t, "planning", "CAD", "EUR", "2026-04-01", "0.660000000000")
	return e
}

// groupOf reads the group forecast as the client.
func groupOf(t *testing.T, c apitest.Client, query string) map[string]any {
	t.Helper()
	f, _ := c.Must(200, "GET", "/v1/group-liquidity-forecast?"+query, "", "")
	return f
}

// minor is a Money's amount in minor units of a currency of two decimals.
func minor(t *testing.T, v any) int64 {
	t.Helper()
	s := v.(map[string]any)["amount"].(string)
	n, err := strconv.ParseInt(strings.Replace(s, ".", "", 1), 10, 64)
	if err != nil {
		t.Fatalf("amount %q: %v", s, err)
	}
	return n
}

// convertedSums are a converted figure's opening, buckets' totals and
// closings, total and beyond, in minor units.
func convertedSums(t *testing.T, c map[string]any) []int64 {
	t.Helper()
	out := []int64{minor(t, c["opening"])}
	for _, b := range c["buckets"].([]any) {
		bm := b.(map[string]any)
		out = append(out, minor(t, bm["total"]), minor(t, bm["closing"]))
	}
	return append(out, minor(t, c["total"]), minor(t, c["beyond"]))
}

// companiesOf renders the group's companies as "code name read zone
// rates incomplete".
func companiesOf(f map[string]any) []string {
	var out []string
	for _, c := range f["companies"].([]any) {
		cm := c.(map[string]any)
		var rates []string
		for _, r := range cm["rates"].([]any) {
			rm := r.(map[string]any)
			rates = append(rates, fmt.Sprintf("%s %s %s", rm["currency"], rm["rate_type"], rm["rate"]))
		}
		out = append(out, fmt.Sprintf("%s %s %s %s rates=%v incomplete=%v", cm["company_code"], cm["name"], cm["read_date"], cm["time_zone"], rates,
			cm["incomplete_currencies"]))
	}
	return out
}

// TestGroupLiquidityForecast: the forecast across the companies the caller
// may act in sums each company's own forecast per currency and, in the
// group currency, each company's own forecast converted into it; a
// currency without a rate is left out and listed, never counted as zero
// (ADR-0051 §9, §10, §15; T53e5).
func TestGroupLiquidityForecast(t *testing.T) {
	e := setupGroup(t)
	g := groupOf(t, e.Client, groupRange)
	want(t, "read date", g["read_date"], "2026-04-17")
	want(t, "group currency", g["group_currency"], "EUR")
	want(t, "companies", companiesOf(g), []string{
		"1000 Acme Corp US 2026-04-17 UTC rates=[] incomplete=[USD]",
		"2000 Acme GmbH 2026-04-17 UTC rates=[] incomplete=[]",
		"3000 Maple Inc 2026-04-17 UTC rates=[CAD planning 0.660000000000] incomplete=[USD]",
	})
	want(t, "closings by currency", closingsOf(t, g), []string{
		"CAD opening 10000.00 CAD banks=1 stale=true gap=false: 2026-04-17 1200.00>11200.00 2026-04-24 -1150.00>10050.00 2026-05-08 -500.00>9550.00 " +
			"2026-07-24 100.00>9650.00; total -350.00 CAD, beyond 50.00 CAD",
		"EUR opening 0.00 EUR banks=1 stale=true gap=false: 2026-05-01 -700.00>-700.00; total -700.00 EUR, beyond 90.00 EUR",
		"USD opening 4800.00 USD banks=2 stale=true gap=false: 2026-04-17 1199.95>5999.95 2026-04-24 -0.02>5999.93; total 1199.93 USD, beyond 0.00 USD",
	})
	want(t, "converted incomplete", g["converted"].(map[string]any)["incomplete_currencies"], []string{"USD"})

	// The group's figures are the sum of each company's own, over the same
	// buckets: per currency never converted, and converted into EUR.
	var sum []int64
	perCurrency := map[string]int64{}
	for _, le := range []uuid.UUID{e.US, e.DE, e.le} {
		f, _ := e.Must(200, "GET", "/v1/liquidity-forecast?legal_entity_id="+le.String()+"&display_currency=EUR&"+groupRange, "", "")
		for i, v := range convertedSums(t, f["converted"].(map[string]any)) {
			if i == len(sum) {
				sum = append(sum, 0)
			}
			sum[i] += v
		}
		for _, c := range f["currencies"].([]any) {
			cm := c.(map[string]any)
			perCurrency[cm["currency"].(string)] += minor(t, cm["total"])
			perCurrency[cm["currency"].(string)+" beyond"] += minor(t, cm["beyond"].(map[string]any)["amount"])
			perCurrency[cm["currency"].(string)+" opening"] += minor(t, cm["opening"].(map[string]any)["amount"])
		}
	}
	want(t, "converted, the companies' converted summed", convertedSums(t, g["converted"].(map[string]any)), sum)
	got := map[string]int64{}
	var codes []string
	for _, c := range g["currencies"].([]any) {
		cm := c.(map[string]any)
		codes = append(codes, cm["currency"].(string))
		got[cm["currency"].(string)] = minor(t, cm["total"])
		got[cm["currency"].(string)+" beyond"] = minor(t, cm["beyond"].(map[string]any)["amount"])
		got[cm["currency"].(string)+" opening"] = minor(t, cm["opening"].(map[string]any)["amount"])
	}
	want(t, "currencies by code", codes, []string{"CAD", "EUR", "USD"})
	want(t, "per currency, the companies' summed", got, perCurrency)

	e.rate(t, "average", "USD", "EUR", "2026-04-10", "0.900000000000")
	g = groupOf(t, e.Client, groupRange)
	want(t, "companies with a USD rate", companiesOf(g), []string{
		"1000 Acme Corp US 2026-04-17 UTC rates=[USD average 0.900000000000] incomplete=[]",
		"2000 Acme GmbH 2026-04-17 UTC rates=[] incomplete=[]",
		"3000 Maple Inc 2026-04-17 UTC rates=[CAD planning 0.660000000000 USD average 0.900000000000] incomplete=[]",
	})
	want(t, "converted complete", g["converted"].(map[string]any)["incomplete_currencies"], []string{})
}

// TestGroupLiquidityForecastAccess: only the legal entities the caller may
// act in are read; one outside its access reaches no figure, list or
// count, and a caller in none reads an empty forecast (ADR-0051 §15).
func TestGroupLiquidityForecastAccess(t *testing.T) {
	e := setupGroup(t)
	two := e.As(e.Reader(t, "reader of 1000 and 3000", e.US, e.le))
	g := groupOf(t, two, groupRange)
	want(t, "companies", companiesOf(g), []string{
		"1000 Acme Corp US 2026-04-17 UTC rates=[] incomplete=[USD]",
		"3000 Maple Inc 2026-04-17 UTC rates=[CAD planning 0.660000000000] incomplete=[USD]",
	})
	var codes []string
	for _, c := range g["currencies"].([]any) {
		codes = append(codes, c.(map[string]any)["currency"].(string))
	}
	want(t, "currencies, without Acme GmbH's EUR", codes, []string{"CAD", "USD"})
	var sum []int64
	for _, le := range []uuid.UUID{e.US, e.le} {
		f, _ := e.Must(200, "GET", "/v1/liquidity-forecast?legal_entity_id="+le.String()+"&display_currency=EUR&"+groupRange, "", "")
		for i, v := range convertedSums(t, f["converted"].(map[string]any)) {
			if i == len(sum) {
				sum = append(sum, 0)
			}
			sum[i] += v
		}
	}
	want(t, "converted, the two companies' alone", convertedSums(t, g["converted"].(map[string]any)), sum)

	none := e.As(e.Reader(t, "reader of no company"))
	g = groupOf(t, none, "")
	want(t, "no company", fmt.Sprintf("%v %v %v %v", g["read_date"], g["companies"], g["currencies"], g["converted"].(map[string]any)["incomplete_currencies"]),
		"2026-04-17 [] [] []")
	want(t, "the default buckets", len(g["buckets"].([]any)), 34)
	want(t, "an empty total", convertedSums(t, g["converted"].(map[string]any))[0:1], []int64{0})
	none.Problem(422, "request.validation_failed", "GET", "/v1/group-liquidity-forecast?min_certainty=sure", "", "")
	none.Problem(422, "request.validation_failed", "GET", "/v1/group-liquidity-forecast?grain=week", "", "")
}

// TestGroupLiquidityForecastReadDates: each company is read on its own
// date, in its own zone; the group's buckets start on the earliest, so
// Toronto's evening is in the first bucket when UTC is on the next day.
func TestGroupLiquidityForecastReadDates(t *testing.T) {
	e := setupForecast(t, func(doc *config.Config) {
		withForecast(doc)
		doc.Company("3000").TimeZone = "America/Toronto"
	})
	evening := time.Date(2026, 4, 18, 2, 0, 0, 0, time.UTC) // 22:00 on the 17th in Toronto
	var g cash.GroupForecast
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		var err error
		g, err = app.Cash(app.Config{Now: func() time.Time { return evening }}).GroupForecast(ctx, cash.GroupForecastQuery{Grain: "day", To: &evening})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var dates []string
	for _, c := range g.Companies {
		dates = append(dates, c.Code+" "+c.ReadDate.Format(time.DateOnly)+" "+c.TimeZone)
	}
	want(t, "the companies' read dates", dates, []string{"1000 2026-04-18 UTC", "2000 2026-04-18 UTC", "3000 2026-04-17 America/Toronto"})
	want(t, "the group's read date", g.ReadDate.Format(time.DateOnly), "2026-04-17")
	want(t, "the buckets, from the 17th to the 18th", len(g.Buckets), 2)
}

// TestGroupLiquidityForecastOneReading: one response reads the clock once,
// so a read that spans midnight gives every company the read date its
// buckets start on, never the next day's for some of them.
func TestGroupLiquidityForecastOneReading(t *testing.T) {
	e := setupForecast(t, withForecast)
	before := time.Date(2026, 4, 17, 23, 59, 59, 0, time.UTC)
	reads := 0
	midnight := func() time.Time { // the first reading before midnight, every later one after it
		reads++
		if reads == 1 {
			return before
		}
		return before.Add(2 * time.Second)
	}
	var g cash.GroupForecast
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		var err error
		g, err = app.Cash(app.Config{Now: midnight}).GroupForecast(ctx, cash.GroupForecastQuery{})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var dates []string
	for _, c := range g.Companies {
		dates = append(dates, c.Code+" "+c.ReadDate.Format(time.DateOnly))
	}
	want(t, "the companies' read dates", dates, []string{"1000 2026-04-17", "2000 2026-04-17", "3000 2026-04-17"})
	want(t, "the group's read date", g.ReadDate.Format(time.DateOnly), "2026-04-17")
	want(t, "the clock's readings", reads, 1)
}

// TestGroupLiquidityForecastTooLarge: a company's amounts, or the
// companies' together, out of the range of an amount refuse the read.
func TestGroupLiquidityForecastTooLarge(t *testing.T) {
	e := setupForecast(t, withForecast)
	e.item(t, e.Supplier, "2026-04-01", "CAD", -9_000_000_000_000_000_000, nil)
	de := e
	de.le, de.payables, de.expense = e.DE, e.Account(t, "2000", "211000"), e.Account(t, "2000", "520000")
	de.item(t, e.Supplier, "2026-04-01", "CAD", -9_000_000_000_000_000_000, nil)
	e.Problem(422, "cash.amount_too_large", "GET", "/v1/group-liquidity-forecast", "", "")
	e.As(e.Reader(t, "reader of 3000", e.le)).Must(200, "GET", "/v1/group-liquidity-forecast", "", "")
	e.item(t, e.Supplier, "2026-04-02", "CAD", -9_000_000_000_000_000_000, nil)
	e.As(e.Reader(t, "reader of 3000 again", e.le)).Problem(422, "cash.amount_too_large", "GET", "/v1/group-liquidity-forecast", "", "")
}

// noGroupCurrency is the tenancy port unable to read the group currency.
type noGroupCurrency struct{ cash.LegalEntities }

func (noGroupCurrency) GroupCurrency(context.Context) (string, error) {
	return "", errors.New("tenant down")
}

// TestGroupLiquidityForecastFailures: the group forecast fails with its
// ports and every database statement.
func TestGroupLiquidityForecastFailures(t *testing.T) {
	e := setupGroup(t)
	read := func(change func(*cash.Endpoints)) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			c := app.Cash(app.Config{Now: func() time.Time { return readAt }})
			change(c)
			_, err := c.GroupForecast(ctx, cash.GroupForecastQuery{})
			return err
		}
	}
	keep := func(*cash.Endpoints) {}
	if err := read(keep)(context.Background()); !errors.Is(err, reqctx.ErrOutsideRequest) {
		t.Errorf("outside a unit of work: got %v, want reqctx.ErrOutsideRequest", err)
	}
	for name, c := range map[string]struct {
		change func(*cash.Endpoints)
		want   string
	}{
		"the directory down":        {func(c *cash.Endpoints) { c.LegalEntities = failingEntities{} }, "cash: legal entities: directory down"},
		"the group currency unread": {func(c *cash.Endpoints) { c.LegalEntities = noGroupCurrency{c.LegalEntities} }, "cash: group currency: tenant down"},
		"a zone that does not load": {func(c *cash.Endpoints) { c.Settings = zoneLost{c.Settings} }, "America/Nowhere"},
		"an item's payment unknown": {func(c *cash.Endpoints) { c.Payments = failingPays{c.Payments} }, "not a percent"},
	} {
		t.Run(name, func(t *testing.T) {
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, read(c.change))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want %q", err, c.want)
			}
		})
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "group forecast", read(keep))
}
