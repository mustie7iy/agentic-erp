package cash_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// drillPerms are what the drill-down's people hold beside
// cash.forecast.read: the memo records' write, and each owning context's
// read permission, so a flow's supplier and documents are shown.
var drillPerms = []string{"cash.memo.read", "cash.memo.write", "partner.supplier.read", "ledger.entry.read", "payables.invoice.read", "payments.run.read"}

// withDrill is the liquidity items' forecast (withItems) with customer
// receipts, an inflow item under OPERATING, the default of inflows.
func withDrill(doc *config.Config) {
	withItems(doc)
	doc.Cash.LiquidityItems = append(doc.Cash.LiquidityItems,
		config.LiquidityItem{Code: "REC_CUSTOMERS", Name: "Customer receipts", Direction: config.DirectionInflow, Parent: "OPERATING"})
	doc.Cash.Derivation.Defaults.Inflow = "REC_CUSTOMERS"
}

// drillEnv is Maple Inc read on Friday 17 April 2026 with the scenario's
// documents: ids by name.
type drillEnv struct {
	forecastEnv
	docs    map[string]uuid.UUID
	transit uuid.UUID // the payment in transit
}

// memo creates a memo record of Maple Inc through the API.
func (e drillEnv) memo(t *testing.T, fields string) uuid.UUID {
	t.Helper()
	m, _ := e.Must(201, "POST", "/v1/memo-records", `{"legal_entity_id": "`+e.le.String()+`", `+fields+`}`, "")
	return uuid.MustParse(id(m))
}

// setupDrill writes Maple Inc's sources for the opening, the new sources
// and the drill-down, read on Friday 17 April 2026 (payment days Tuesday
// and Thursday, Monday the 20th a holiday, the USD bank a value day):
//   - USD: a statement closing on the 16th at 4,800.00 (not stale); a
//     payment of 300.00 released on the 16th still open on the clearing
//     account, in transit on the 17th; two planned memo records of -0.02
//     without a bank on the 22nd and the 24th;
//   - CAD: MAIN's statement closing on the 14th at 10,000.00 (stale); an
//     item of -800.00 held by a run proposed on the 17th; an item of
//     -1,000.00 of 1 April, NT30, paid Thursday the 30th; a down payment
//     request of 250.00 due Tuesday the 28th; a receipt of 2,000.00 on
//     MAIN on the 23rd (CUST-1); a quarterly receipt of 100.00 without a
//     bank from Friday the 24th; a planned payment of 500.00 on Tuesday 12
//     May; a receipt of 50.00 on 1 June 2027, after the last bucket.
func setupDrill(t *testing.T) drillEnv {
	t.Helper()
	e := drillEnv{forecastEnv: setupForecast(t, withDrill, drillPerms...), docs: map[string]uuid.UUID{}}
	e.statement(t, e.usd, "USD", "2026-04-15", "2026-04-16", 480000, 480000, nil, "imported")
	e.statement(t, e.main, "CAD", "2026-04-13", "2026-04-14", 1000000, 1000000, nil, "imported")
	e.transit, _ = e.released(t, "2026-04-16", 30000)
	var transitRun uuid.UUID
	pgtest.Scan(t, `SELECT payment_run_id FROM payments.payment WHERE id = $1`, []any{e.transit}, &transitRun)
	e.docs["transit run"] = transitRun
	held := e.item(t, e.Supplier, "2026-04-01", "CAD", -80000, nil)
	e.docs["run"] = e.run(t, e.main, "2026-04-17", "CAD", "proposed")
	pay := e.payment(t, e.docs["run"], 80000, "CAD", nil)
	e.exec(t, `INSERT INTO payments.payment_item (tenant_id, payment_id, item_fiscal_year, item_entry_id, item_line_no, amount, due_date)
		VALUES ($1, $2, 2026, $3, 1, 80000, '2026-05-01')`, e.Tenant.ID, pay, held)
	e.docs["item"] = e.item(t, e.Supplier, "2026-04-01", "CAD", -100000, &terms{baseline: "2026-04-01", net: 30})
	e.docs["request"] = e.request(t, e.Supplier, 25000, "2026-04-28")
	e.docs["receipt"] = e.memo(t, `"house_bank_id": "`+e.main.String()+`", "amount": {"amount": "2000.00", "currency": "CAD"}, "value_date": "2026-04-23", `+
		`"liquidity_item": "REC_CUSTOMERS", "reference": "CUST-1"`)
	e.docs["series"] = e.memo(t, `"amount": {"amount": "100.00", "currency": "CAD"}, "value_date": "2026-04-24", "liquidity_item": "REC_CUSTOMERS", `+
		`"recurrence": {"frequency": "quarterly"}`)
	e.memo(t, `"amount": {"amount": "-500.00", "currency": "CAD"}, "value_date": "2026-05-12", "liquidity_item": "OP_SUPPLIERS", "certainty": "planned"`)
	e.memo(t, `"amount": {"amount": "50.00", "currency": "CAD"}, "value_date": "2027-06-01", "liquidity_item": "REC_CUSTOMERS"`)
	for _, day := range []string{"2026-04-22", "2026-04-24"} {
		e.memo(t, `"amount": {"amount": "-0.02", "currency": "USD"}, "value_date": "`+day+`", "liquidity_item": "OP_SUPPLIERS", "certainty": "planned"`)
	}
	return e
}

// closingsOf renders each currency's opening, then its buckets with a
// total as "start total>closing", and its total and beyond.
func closingsOf(t *testing.T, f map[string]any) []string {
	t.Helper()
	var out []string
	for _, c := range f["currencies"].([]any) {
		cm := c.(map[string]any)
		o := cm["opening"].(map[string]any)
		line := fmt.Sprintf("%s opening %s banks=%v stale=%v gap=%v:", cm["currency"], amount(o["amount"]), o["banks"], o["stale"], o["gap"])
		for _, b := range cm["buckets"].([]any) {
			bm := b.(map[string]any)
			if !strings.HasPrefix(amount(bm["total"]), "0.00 ") {
				line += fmt.Sprintf(" %s %s>%s", bm["start"], strings.Fields(amount(bm["total"]))[0], strings.Fields(amount(bm["closing"]))[0])
			}
		}
		out = append(out, fmt.Sprintf("%s; total %s, beyond %s", line, amount(cm["total"]), amount(cm["beyond"].(map[string]any)["amount"])))
	}
	return out
}

// TestLiquidityForecastOpeningAndSources: each currency opens at its
// banks' statements before the read date and closes each bucket at the
// opening plus the totals so far; payments in transit and memo records,
// with a bank or none, are sources of their own, a series expanded over
// the horizon and a one-off record after it beyond (ADR-0051 §7, §9,
// T53e4).
func TestLiquidityForecastOpeningAndSources(t *testing.T) {
	e := setupDrill(t)
	f := e.forecastOf(t, "")
	want(t, "closings", closingsOf(t, f), []string{
		"CAD opening 10000.00 CAD banks=1 stale=true gap=false: 2026-04-17 -800.00>9200.00 2026-04-23 2000.00>11200.00 2026-04-24 100.00>11300.00 " +
			"2026-04-28 -250.00>11050.00 2026-04-30 -1000.00>10050.00 2026-05-08 -500.00>9550.00 2026-07-17 100.00>9650.00 2026-10-17 100.00>9750.00 " +
			"2027-01-17 100.00>9850.00; total -150.00 CAD, beyond 50.00 CAD",
		"USD opening 4800.00 USD banks=1 stale=false gap=false: 2026-04-17 -300.00>4500.00 2026-04-22 -0.02>4499.98 2026-04-24 -0.02>4499.96; " +
			"total -300.04 USD, beyond 0.00 USD",
	})
	want(t, "flows", flowsOf(t, f), []string{
		"CAD day 2026-04-17..2026-04-17: payment_run/confirmed/late=false -800.00 CAD x1 = -800.00 CAD",
		"CAD day 2026-04-23..2026-04-23: memo/expected/late=false 2000.00 CAD x1 = 2000.00 CAD",
		"CAD day 2026-04-24..2026-04-24: memo/expected/late=false 100.00 CAD x1 = 100.00 CAD",
		"CAD day 2026-04-28..2026-04-28: down_payment/expected/late=false -250.00 CAD x1 = -250.00 CAD",
		"CAD day 2026-04-30..2026-04-30: payable/expected/late=false -1000.00 CAD x1 = -1000.00 CAD",
		"CAD week 2026-05-08..2026-05-14: memo/planned/late=false -500.00 CAD x1 = -500.00 CAD",
		"CAD month 2026-07-17..2026-08-16: memo/expected/late=false 100.00 CAD x1 = 100.00 CAD",
		"CAD month 2026-10-17..2026-11-16: memo/expected/late=false 100.00 CAD x1 = 100.00 CAD",
		"CAD month 2027-01-17..2027-02-16: memo/expected/late=false 100.00 CAD x1 = 100.00 CAD",
		"CAD total -150.00 CAD, beyond 50.00 CAD x1, blocked 0.00 CAD x0",
		"USD day 2026-04-17..2026-04-17: in_transit/confirmed/late=false -300.00 USD x1 = -300.00 USD",
		"USD day 2026-04-22..2026-04-22: memo/planned/late=false -0.02 USD x1 = -0.02 USD",
		"USD day 2026-04-24..2026-04-24: memo/planned/late=false -0.02 USD x1 = -0.02 USD",
		"USD total -300.04 USD, beyond 0.00 USD x0, blocked 0.00 USD x0",
	})
	want(t, "CAD rows", itemsOf(t, f, "CAD"), []string{
		"OPERATING Operating (<nil>): [2026-04-17 -800.00, 2026-04-23 2000.00, 2026-04-24 100.00, 2026-04-28 -250.00, 2026-04-30 -1000.00, " +
			"2026-05-08 -500.00, 2026-07-17 100.00, 2026-10-17 100.00, 2027-01-17 100.00] total -150.00 CAD beyond 50.00 CAD",
		"  OP_SUPPLIERS Supplier payments (OPERATING): [2026-04-17 -800.00, 2026-04-28 -250.00, 2026-04-30 -1000.00, 2026-05-08 -500.00] total -2550.00 CAD beyond 0.00 CAD",
		"    down_payment/expected: [2026-04-28 -250.00] total -250.00 CAD beyond 0.00 CAD",
		"    memo/planned: [2026-05-08 -500.00] total -500.00 CAD beyond 0.00 CAD",
		"    payable/expected: [2026-04-30 -1000.00] total -1000.00 CAD beyond 0.00 CAD",
		"    payment_run/confirmed: [2026-04-17 -800.00] total -800.00 CAD beyond 0.00 CAD",
		"  REC_CUSTOMERS Customer receipts (OPERATING): [2026-04-23 2000.00, 2026-04-24 100.00, 2026-07-17 100.00, 2026-10-17 100.00, 2027-01-17 100.00] " +
			"total 2400.00 CAD beyond 50.00 CAD",
		"    memo/expected: [2026-04-23 2000.00, 2026-04-24 100.00, 2026-07-17 100.00, 2026-10-17 100.00, 2027-01-17 100.00] total 2400.00 CAD beyond 50.00 CAD",
	})
	want(t, "USD in transit by its supplier's default", itemsOf(t, f, "USD")[1], "  OP_SUPPLIERS Supplier payments (OPERATING): "+
		"[2026-04-17 -300.00, 2026-04-22 -0.02, 2026-04-24 -0.02] total -300.04 USD beyond 0.00 USD")
	if _, ok := f["converted"]; ok {
		t.Errorf("converted: got %v, want none unless asked", f["converted"])
	}

	t.Run("the certainty cut keeps the opening", func(t *testing.T) {
		want(t, "actual", closingsOf(t, e.forecastOf(t, "&min_certainty=actual")), []string{
			"CAD opening 10000.00 CAD banks=1 stale=true gap=false:; total 0.00 CAD, beyond 0.00 CAD",
			"USD opening 4800.00 USD banks=1 stale=false gap=false:; total 0.00 USD, beyond 0.00 USD",
		})
	})
}

// TestLiquidityForecastGrain: one grain over a range replaces the
// company's buckets, a memo series expanded up to its last bucket; the
// range is refused without both, before the read date, or longer than the
// grain's limit (ADR-0051 §9, T53e4).
func TestLiquidityForecastGrain(t *testing.T) {
	e := setupDrill(t)
	f := e.forecastOf(t, "&grain=week&to=2026-05-20")
	want(t, "buckets", fmt.Sprint(f["buckets"]), "[map[end:2026-04-23 grain:week start:2026-04-17] map[end:2026-04-30 grain:week start:2026-04-24] "+
		"map[end:2026-05-07 grain:week start:2026-05-01] map[end:2026-05-14 grain:week start:2026-05-08] map[end:2026-05-21 grain:week start:2026-05-15]]")
	want(t, "closings", closingsOf(t, f)[0], "CAD opening 10000.00 CAD banks=1 stale=true gap=false: 2026-04-17 1200.00>11200.00 "+
		"2026-04-24 -1150.00>10050.00 2026-05-08 -500.00>9550.00; total -450.00 CAD, beyond 50.00 CAD")
	f = e.forecastOf(t, "&grain=month&to=2026-04-17")
	want(t, "a month holding the read date", fmt.Sprint(f["buckets"]), "[map[end:2026-05-16 grain:month start:2026-04-17]]")
	for name, c := range map[string]struct{ query, field string }{
		"a grain without the last day":    {"&grain=day", "to"},
		"the last day without a grain":    {"&to=2026-05-01", "grain"},
		"a grain not known":               {"&grain=year&to=2026-05-01", "grain"},
		"a last day before the read date": {"&grain=day&to=2026-04-16", "to"},
		"more than 92 days":               {"&grain=day&to=2026-07-18", "to"},
	} {
		t.Run(name, func(t *testing.T) {
			p := e.Problem(422, "request.validation_failed", "GET", "/v1/liquidity-forecast?legal_entity_id="+e.le.String()+c.query, "", "")
			want(t, "field", fieldOf(p), c.field)
		})
	}
	want(t, "92 days are allowed", len(e.forecastOf(t, "&grain=day&to=2026-07-17")["buckets"].([]any)), 92)
}

// TestLiquidityForecastConverted: the currencies converted into a display
// currency at one rate each, the company's rate type (planning) valid on
// the read date, each opening and bucket total rounded once, half away
// from zero, then summed, the closings the converted opening plus the
// converted totals; a currency without a rate is listed incomplete and
// left out, never counted as zero; an unknown display currency is refused
// (ADR-0051 §10, T53e4).
func TestLiquidityForecastConverted(t *testing.T) {
	e := setupDrill(t)
	e.rate(t, "planning", "USD", "CAD", "2026-04-15", "1.25")
	c := e.forecastOf(t, "&display_currency=CAD")["converted"].(map[string]any)
	var buckets []string
	for _, b := range c["buckets"].([]any) {
		bm := b.(map[string]any)
		if !strings.HasPrefix(amount(bm["total"]), "0.00 ") {
			buckets = append(buckets, fmt.Sprintf("%s %s>%s", bm["start"], strings.Fields(amount(bm["total"]))[0], strings.Fields(amount(bm["closing"]))[0]))
		}
	}
	want(t, "converted", fmt.Sprint(c["currency"], " ", c["rates"], " ", c["incomplete_currencies"], " opening ", amount(c["opening"]), " total ",
		amount(c["total"]), " beyond ", amount(c["beyond"])),
		"CAD [map[currency:USD rate:1.250000000000 rate_type:planning stale:false valid_from:2026-04-15]] [] opening 16000.00 CAD total -525.06 CAD beyond 50.00 CAD")
	// USD's -0.02 on the 22nd and on the 24th are -0.025 CAD each, -0.03
	// rounded half away from zero: -0.06 together, where -0.04 USD at once
	// would be -0.05.
	want(t, "buckets", buckets, []string{"2026-04-17 -1175.00>14825.00", "2026-04-22 -0.03>14824.97", "2026-04-23 2000.00>16824.97",
		"2026-04-24 99.97>16924.94", "2026-04-28 -250.00>16674.94", "2026-04-30 -1000.00>15674.94", "2026-05-08 -500.00>15174.94",
		"2026-07-17 100.00>15274.94", "2026-10-17 100.00>15374.94", "2027-01-17 100.00>15474.94"})

	c = e.forecastOf(t, "&display_currency=EUR")["converted"].(map[string]any)
	want(t, "no rate into EUR", fmt.Sprint(c["rates"], " ", c["incomplete_currencies"], " ", amount(c["opening"]), " ", amount(c["total"])),
		"[] [CAD USD] 0.00 EUR 0.00 EUR")
	p := e.Problem(422, "cash.unknown_currency", "GET", "/v1/liquidity-forecast?legal_entity_id="+e.le.String()+"&display_currency=XYZ", "", "")
	want(t, "field", fieldOf(p), "display_currency")
}

// drill lists the drill-down's flows of a query as "date source certainty
// counted amount share: supplier; documents (count)" lines, every page
// read in pages of size, and returns the first page with them.
func (e drillEnv) drill(t *testing.T, query string, size int) (map[string]any, []string) {
	t.Helper()
	var out []string
	var first map[string]any
	token := ""
	for {
		q := "/v1/liquidity-forecast-flows?legal_entity_id=" + e.le.String() + query + fmt.Sprintf("&page_size=%d", size)
		if token != "" {
			q += "&page_token=" + url.QueryEscape(token)
		}
		p, _ := e.Must(200, "GET", q, "", "")
		if first == nil {
			first = p
		}
		for _, it := range p["items"].([]any) {
			out = append(out, e.flowLine(it.(map[string]any)))
		}
		next, _ := p["next_page_token"].(string)
		if next == "" {
			return first, out
		}
		for name, id := range e.docs {
			if strings.Contains(next, id.String()) {
				t.Errorf("page token: got %s naming the %s, want no document in it", next, name)
			}
		}
		token = next
	}
}

// flowLine renders a listed flow, its documents by the scenario's names.
func (e drillEnv) flowLine(f map[string]any) string {
	name := func(id any) string {
		for n, d := range e.docs {
			if d.String() == id {
				return n
			}
		}
		return fmt.Sprint(id)
	}
	var docs []string
	for _, d := range f["documents"].([]any) {
		dm := d.(map[string]any)
		docs = append(docs, fmt.Sprintf("%s %s %v %s", dm["kind"], name(dm["id"]), dm["number"] != nil, strings.Fields(amount(dm["amount"]))[0]))
	}
	if len(docs) == 0 {
		docs = []string{"none shown"}
	}
	supplier := "-"
	if s, ok := f["supplier"].(map[string]any); ok {
		supplier = fmt.Sprint(s["number"])
	}
	return fmt.Sprintf("%s %s/%s counted=%v %s share %s: %s; %s (%v)", f["date"], f["source"], f["certainty"], f["counted"], amount(f["amount"]),
		strings.Fields(amount(f["share"]))[0], supplier, strings.Join(docs, ", "), f["document_count"])
}

// TestLiquidityForecastDrillDown: a bucket's flows of a currency, every
// flow or those of an item and its descendants with their shares, in
// pages whose tokens name no document, each with its supplier and the
// documents it pays to read in their own contexts; the total of the
// counted shares and the count over every page (ADR-0051 §9, T53e4).
func TestLiquidityForecastDrillDown(t *testing.T) {
	e := setupDrill(t)
	p, flows := e.drill(t, "&currency=CAD&from=2026-04-17&to=2026-04-30", 2)
	want(t, "the days' flows", flows, []string{
		"2026-04-17 payment_run/confirmed counted=true -800.00 CAD share -800.00: BP90000001; payment_run run true -800.00 (1)",
		"2026-04-23 memo/expected counted=true 2000.00 CAD share 2000.00: -; memo_record receipt true 2000.00 (1)",
		"2026-04-24 memo/expected counted=true 100.00 CAD share 100.00: -; memo_record series false 100.00 (1)",
		"2026-04-28 down_payment/expected counted=true -250.00 CAD share -250.00: BP90000001; down_payment_request request true -250.00 (1)",
		"2026-04-30 payable/expected counted=true -1000.00 CAD share -1000.00: BP90000001; journal_entry item true -1000.00 (1)",
	})
	want(t, "the page", fmt.Sprint(p["read_date"], " ", p["from"], " ", p["to"], " ", amount(p["total"]), " x", p["count"], " ", len(p["items"].([]any))),
		"2026-04-17 2026-04-17 2026-04-30 50.00 CAD x5 2")

	_, flows = e.drill(t, "&currency=CAD&liquidity_item=REC_CUSTOMERS", 50)
	want(t, "customer receipts, after the last bucket too", len(flows), 6)
	p, _ = e.drill(t, "&currency=CAD&liquidity_item=OPERATING&from=2026-05-01", 50)
	want(t, "a parent's descendants", fmt.Sprint(amount(p["total"]), " x", p["count"], " ", p["liquidity_item"]), "-150.00 CAD x5 OPERATING")
	p, _ = e.drill(t, "&currency=CAD&unclassified=true", 50)
	want(t, "nothing unclassified", fmt.Sprint(amount(p["total"]), " x", p["count"], " ", p["unclassified"]), "0.00 CAD x0 true")
	p, flows = e.drill(t, "&currency=USD&min_certainty=confirmed", 50)
	want(t, "USD in transit", flows, []string{
		"2026-04-17 in_transit/confirmed counted=true -300.00 USD share -300.00: BP90000001; payment_run transit run true -300.00 (1)"})
	want(t, "certainty", p["min_certainty"], "confirmed")
	// The payment settled a USD item on rent (cleared since), and a second
	// open line of it is on the clearing account: each line is a flow of
	// its own, classified at its own amount over the item it pays, so the
	// rent row adds up to the bucket. Flows of a day are paged in their
	// keys' order, a digest of the payment's random id: sorted here.
	paid := e.item(t, e.Supplier, "2026-04-01", "USD", -30000, nil)
	e.exec(t, `SET session_replication_role = replica`)
	e.exec(t, `UPDATE ledger.journal_line SET account_id = $2 WHERE journal_entry_id = $1 AND line_no = 2`, paid, e.Account(t, "3000", "530000"))
	e.exec(t, `INSERT INTO ledger.clearing_item (tenant_id, fiscal_year, journal_entry_id, line_no, clearing_id) VALUES ($1, 2026, $2, 1, gen_random_uuid())`,
		e.Tenant.ID, paid)
	e.exec(t, `RESET session_replication_role`)
	e.exec(t, `INSERT INTO payments.payment_item (tenant_id, payment_id, item_fiscal_year, item_entry_id, item_line_no, amount, due_date)
		VALUES ($1, $2, 2026, $3, 1, 30000, '2026-05-01')`, e.Tenant.ID, e.transit, paid)
	e.entry(t, e.leading, e.clearing, "2026-04-16", "USD", -100, -135, "payments", &e.transit)
	p, flows = e.drill(t, "&currency=USD&min_certainty=confirmed&liquidity_item=OP_RENT", 1)
	slices.Sort(flows)
	want(t, "two lines of one payment, by the rent they pay", fmt.Sprint(flows, " ", amount(p["total"])), fmt.Sprint([]string{
		"2026-04-17 in_transit/confirmed counted=true -1.00 USD share -1.00: BP90000001; payment_run transit run true -1.00 (1)",
		"2026-04-17 in_transit/confirmed counted=true -300.00 USD share -300.00: BP90000001; payment_run transit run true -300.00 (1)"}, " -301.00 USD"))
	f := e.forecastOf(t, "&min_certainty=confirmed")
	want(t, "the rent row, the bucket", itemsOf(t, f, "USD")[1], "  OP_RENT Rent (OPERATING): [2026-04-17 -301.00] total -301.00 USD beyond 0.00 USD")

	for name, c := range map[string]struct {
		status      int
		query, code string
	}{
		"an item with the unclassified": {422, "&currency=CAD&liquidity_item=OPERATING&unclassified=true", "request.validation_failed"},
		"a range ending before it":      {422, "&currency=CAD&from=2026-05-01&to=2026-04-30", "request.validation_failed"},
		"a currency not known":          {422, "&currency=XYZ", "cash.unknown_currency"},
		"a certainty not known":         {422, "&currency=CAD&min_certainty=likely", "request.validation_failed"},
		"a page token of other filters": {400, "&currency=USD&page_token=" + url.QueryEscape(firstToken(t, e)), "request.invalid_page_token"},
		"a page size of none":           {400, "&currency=CAD&page_size=0", "request.invalid_parameter"},
	} {
		t.Run(name, func(t *testing.T) {
			e.Problem(c.status, c.code, "GET", "/v1/liquidity-forecast-flows?legal_entity_id="+e.le.String()+c.query, "", "")
		})
	}
	t.Run("sums beyond an amount", func(t *testing.T) {
		e.item(t, e.Supplier, "2026-04-01", "CAD", -9_000_000_000_000_000_000, nil)
		e.item(t, e.Supplier, "2026-04-02", "CAD", -9_000_000_000_000_000_000, nil)
		e.Problem(422, "cash.amount_too_large", "GET", "/v1/liquidity-forecast-flows?legal_entity_id="+e.le.String()+"&currency=CAD", "", "")
	})
	other := e.Reader(t, "reader of 1000", e.US)
	e.As(other).Problem(404, "request.not_found", "GET", "/v1/liquidity-forecast-flows?legal_entity_id="+e.le.String()+"&currency=CAD", "", "")
}

// TestLiquidityForecastDrillDownPages: a run's payments of one day, which
// name the same run, are each a flow with a key of its own, so a page
// token still points after the flow it was issued at when the run changes
// between two pages: the next page neither skips nor repeats a flow.
func TestLiquidityForecastDrillDownPages(t *testing.T) {
	e := setupDrill(t)
	second := uuid.Must(uuid.NewV7())
	e.exec(t, `INSERT INTO payments.payment (tenant_id, id, payment_run_id, supplier_id, bank_account_id, amount, currency, end_to_end_id, created_by)
		VALUES ($1, $2, $3, $4, gen_random_uuid(), 10000, 'CAD', $5, $6)`, e.Tenant.ID, second, e.docs["run"], e.Vendor, endToEnd(second), e.Tenant.Human.ID)
	query := "/v1/liquidity-forecast-flows?legal_entity_id=" + e.le.String() + "&currency=CAD&from=2026-04-17&to=2026-04-17&page_size=1"
	first, _ := e.Must(200, "GET", query, "", "")
	items := first["items"].([]any)
	want(t, "the first page", fmt.Sprint(first["count"], " ", len(items)), "2 1")
	shown := amount(items[0].(map[string]any)["amount"])
	// The payment shown first leaves the run; the other is the next page.
	gone, rest := second, "-800.00 CAD"
	if shown == "-800.00 CAD" {
		pgtest.Scan(t, `SELECT id FROM payments.payment WHERE payment_run_id = $1 AND amount = 80000`, []any{e.docs["run"]}, &gone)
		rest = "-100.00 CAD"
	}
	e.exec(t, `DELETE FROM payments.payment_item WHERE payment_id = $1`, gone)
	e.exec(t, `DELETE FROM payments.payment WHERE id = $1`, gone)
	next, _ := e.Must(200, "GET", query+"&page_token="+url.QueryEscape(first["next_page_token"].(string)), "", "")
	got := []string{}
	for _, it := range next["items"].([]any) {
		got = append(got, amount(it.(map[string]any)["amount"]))
	}
	want(t, "the next page after the first payment left the run", got, []string{rest})
}

// firstToken is the next page token of the CAD flows in pages of one.
func firstToken(t *testing.T, e drillEnv) string {
	t.Helper()
	p, _ := e.Must(200, "GET", "/v1/liquidity-forecast-flows?legal_entity_id="+e.le.String()+"&currency=CAD&page_size=1", "", "")
	return p["next_page_token"].(string)
}

// TestLiquidityForecastDrillDownGated: a caller holding cash.forecast.read
// alone sees the flows and their amounts, never their suppliers or
// documents, which document_count still counts; one barred from a read
// permission by a segregation-of-duties rule is treated as not holding it.
// Data is left out, never refused (04 §4.4, T53e4).
func TestLiquidityForecastDrillDownGated(t *testing.T) {
	e := setupDrill(t)
	query := "&currency=CAD&from=2026-04-17&to=2026-04-30"
	pgtest.Exec(t, `INSERT INTO identity.sod_rule (tenant_id, id, permission_a, permission_b, description, valid_from, created_by)
		VALUES ($1, gen_random_uuid(), 'cash.memo.write', 'ledger.entry.read', 'd', current_date, $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	_, flows := e.drill(t, query, 50)
	want(t, "journal entries barred", flows[4], "2026-04-30 payable/expected counted=true -1000.00 CAD share -1000.00: BP90000001; none shown (1)")
	for _, p := range []string{"partner.supplier.read", "payables.invoice.read", "payments.run.read", "cash.memo.read"} {
		e.Tenant.Revoke(t, p, e.Tenant.Human)
	}
	_, flows = e.drill(t, query, 50)
	want(t, "the flows alone", flows, []string{
		"2026-04-17 payment_run/confirmed counted=true -800.00 CAD share -800.00: -; none shown (1)",
		"2026-04-23 memo/expected counted=true 2000.00 CAD share 2000.00: -; none shown (1)",
		"2026-04-24 memo/expected counted=true 100.00 CAD share 100.00: -; none shown (1)",
		"2026-04-28 down_payment/expected counted=true -250.00 CAD share -250.00: -; none shown (1)",
		"2026-04-30 payable/expected counted=true -1000.00 CAD share -1000.00: -; none shown (1)",
	})
}

// TestLiquidityForecastBlockedDrillDown: a blocked supplier's flow is
// listed whether counted or not, and counts in the total only with
// include_blocked.
func TestLiquidityForecastBlockedDrillDown(t *testing.T) {
	e := setupDrill(t)
	e.docs["vendor's"] = e.item(t, e.Vendor, "2026-04-01", "CAD", -70000, nil)
	query := "&currency=CAD&from=2026-05-01&to=2026-05-07&liquidity_item=OP_SUPPLIERS"
	p, flows := e.drill(t, query, 50)
	want(t, "listed, not counted", fmt.Sprint(flows, " ", amount(p["total"])),
		"[2026-05-01 payable_blocked/expected counted=false -700.00 CAD share -700.00: BP90000002; journal_entry vendor's true -700.00 (1)] 0.00 CAD")
	p, _ = e.drill(t, query+"&include_blocked=true", 50)
	want(t, "counted when asked", fmt.Sprint(amount(p["total"]), " ", p["include_blocked"]), "-700.00 CAD true")
}

// TestLiquidityForecastDrillFailures: the forecast with a display currency
// and the drill-down showing suppliers and documents fail with their
// ports.
func TestLiquidityForecastDrillFailures(t *testing.T) {
	e := setupDrill(t)
	e.rate(t, "planning", "USD", "CAD", "2026-04-15", "1.25")
	endpoints := func() *cash.Endpoints { return app.Cash(app.Config{Now: func() time.Time { return readAt }}) }
	reads := map[string]func(ctx context.Context) error{
		"converted": func(ctx context.Context) error {
			_, err := endpoints().Forecast(ctx, cash.ForecastQuery{LegalEntity: e.le, Display: "CAD"})
			return err
		},
		"drilled": func(ctx context.Context) error {
			_, err := endpoints().Flows(ctx, cash.FlowsQuery{LegalEntity: e.le, Currency: "CAD", PageSize: 50})
			return err
		},
	}
	for name, read := range reads {
		t.Run(name, func(t *testing.T) { apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, read) })
	}
	// The contract refuses a page size out of range before the handler; the
	// handler refuses it too, as every list does (pagination.Size).
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := endpoints().ListLiquidityForecastFlows(ctx, gen.ListLiquidityForecastFlowsParams{LegalEntityID: e.le, Currency: "CAD",
			PageSize: gen.NewOptInt32(0)})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "page_size") {
		t.Errorf("a page size of none: got %v, want it refused", err)
	}
	failing := endpoints()
	failing.Partners = failingPartners{}
	err = apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := failing.Flows(ctx, cash.FlowsQuery{LegalEntity: e.le, Currency: "CAD", PageSize: 50})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "names of the forecast's suppliers") {
		t.Errorf("the suppliers' names not read: got %v, want the read failed", err)
	}
}

// failingPartners cannot name partners.
type failingPartners struct{}

func (failingPartners) Names(context.Context, []uuid.UUID) (map[uuid.UUID]cash.PartnerName, error) {
	return nil, errors.New("partner names unavailable")
}
