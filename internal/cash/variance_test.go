package cash_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// varianceAt is the variance's clock: Friday 22 May 2026, after the
// period it reads.
var varianceAt = time.Date(2026, 5, 22, 15, 0, 0, 0, time.UTC)

// varianceEnv is setupSnapshot's Maple Inc with a snapshot taken on
// demand on 17 April, then what the banks showed until 21 May.
type varianceEnv struct {
	drillEnv
	snapshot string
}

// paymentOf is the run's one payment.
func (e varianceEnv) paymentOf(t *testing.T, run uuid.UUID) uuid.UUID {
	t.Helper()
	var p uuid.UUID
	pgtest.Scan(t, `SELECT id FROM payments.payment WHERE payment_run_id = $1`, []any{run}, &p)
	return p
}

// realise realises the memo record by a statement line through the API,
// a series' occurrence on the day by an override.
func (e varianceEnv) realise(t *testing.T, record uuid.UUID, occurrence string, statement uuid.UUID, no int) {
	t.Helper()
	m, tag := e.Must(200, "GET", "/v1/memo-records/"+record.String(), "", "")
	if occurrence == "" {
		e.Must(200, "POST", path(m, "realise"), lineBody(statement, no), tag)
		return
	}
	e.Must(201, "POST", path(m, "realise"), fmt.Sprintf(`{"occurrence_date": %q, "statement_id": %q, "line_no": %d}`, occurrence, statement, no), tag)
}

// setupVariance is setupSnapshot's Maple Inc with an invoice of 300.00
// parked on 10 April, its snapshot taken on 17 April, and the banks'
// lines after it (each with its forecast at the snapshot):
//   - CAD: on the 17th -800.00 paid by the run's payment (forecast that
//     day); on the 23rd 1,900.00 not matched, which realised the receipt
//     of 2,000.00 (forecast that day); on the 24th 100.00 not matched,
//     which realised the quarterly receipt's occurrence of that day; on the
//     29th -300.00 paid by the second run's payment (forecast on the 22nd,
//     the week before); on the 30th -1,000.00 paid by a later run's payment
//     of the item (forecast on the 30th; the credit memo netted into it is
//     never paid); on the 28th -250.00 paid by the payment of the down
//     payment request due then; on 5 May -400.00 paid for an invoice posted after the
//     snapshot, and on the 6th 500.00 not matched and realising nothing;
//     on the 7th -300.00 paid for the parked invoice, posted after the
//     snapshot; on the 19th -200.00 paid for the item due then, after the
//     document level. The planned payment of 12 May is never paid;
//   - USD: on the 17th -300.00 paid by the payment in transit.
func setupVariance(t *testing.T) varianceEnv {
	t.Helper()
	e := varianceEnv{drillEnv: setupSnapshot(t)}
	a := actualsEnv{forecastEnv: e.forecastEnv}
	e.docs["parked"] = e.invoice(t, e.Supplier, "invoice", "parked", "2026-04-10", "CAD", 30000, 0, nil)
	e.snapshot = fmt.Sprint(e.take(t, "Before the variance")["id"])
	cad := e.statement(t, e.main, "CAD", "2026-04-17", "2026-05-21", 1000000, 1000000, nil, "imported")
	usd := e.statement(t, e.usd, "USD", "2026-04-17", "2026-04-17", 480000, 450000, nil, "imported")
	pay := func(no int, day string, amount int64, payment uuid.UUID) {
		a.actualLine(t, cad, no, day, amount, "posted", "matched", target{payments: []uuid.UUID{payment}})
	}
	pay(1, "2026-04-17", -80000, e.paymentOf(t, e.docs["run"]))
	a.actualLine(t, cad, 2, "2026-04-23", 190000, "unmatched", "", target{})
	a.actualLine(t, cad, 3, "2026-04-24", 10000, "unmatched", "", target{})
	pay(4, "2026-04-29", -30000, e.paymentOf(t, e.docs["run2"]))
	pay(5, "2026-04-30", -100000, a.paid(t, e.Supplier, 100000, map[uuid.UUID]int64{e.docs["item"]: 100000}))
	request := a.paid(t, e.Supplier, 25000, nil)
	e.exec(t, `UPDATE payments.payment SET kind = 'down_payment', down_payment_request_id = $2 WHERE id = $1`, request, e.docs["request"])
	pay(10, "2026-04-28", -25000, request)
	later := e.item(t, e.Supplier, "2026-04-25", "CAD", -40000, nil)
	pay(6, "2026-05-05", -40000, a.paid(t, e.Supplier, 40000, map[uuid.UUID]int64{later: 40000}))
	a.actualLine(t, cad, 7, "2026-05-06", 50000, "unmatched", "", target{})
	posted := e.item(t, e.Supplier, "2026-04-20", "CAD", -30000, nil)
	e.exec(t, `SET session_replication_role = replica`)
	e.exec(t, `UPDATE ledger.journal_entry SET source_document_id = $2 WHERE id = $1`, posted, e.docs["parked"])
	e.exec(t, `RESET session_replication_role`)
	pay(8, "2026-05-07", -30000, a.paid(t, e.Supplier, 30000, map[uuid.UUID]int64{posted: 30000}))
	pay(9, "2026-05-19", -20000, a.paid(t, e.Supplier, 20000, map[uuid.UUID]int64{e.docs["may"]: 20000}))
	a.actualLine(t, usd, 1, "2026-04-17", -30000, "posted", "matched", target{payments: []uuid.UUID{e.transit}})
	e.realise(t, e.docs["receipt"], "", cad, 2)
	e.realise(t, e.docs["series"], "2026-04-24", cad, 3)
	e.API = apitest.NewWith(t, func(cfg *app.Config) { cfg.Now = func() time.Time { return varianceAt } })
	return e
}

// varianceOf reads Maple's variance with the query.
func (e varianceEnv) varianceOf(t *testing.T, query string) map[string]any {
	t.Helper()
	v, _ := e.Must(200, "GET", "/v1/liquidity-forecast-variance?legal_entity_id="+e.le.String()+query, "", "")
	return v
}

// cellText renders a cell as "forecast actual variance percent% timing/
// amount/new/missing/unclassified", amounts without their currency.
func cellText(v any) string {
	c := v.(map[string]any)
	num := func(k string, m map[string]any) string { return strings.Fields(amount(m[k]))[0] }
	s := fmt.Sprintf("%s %s %s %v%%", num("forecast", c), num("actual", c), num("variance", c), c["variance_percent"])
	if at, ok := c["attribution"].(map[string]any); ok {
		s += fmt.Sprintf(" %s/%s/%s/%s/%s", num("timing", at), num("amount", at), num("new", at), num("missing", at), num("unclassified", at))
	}
	return s
}

// varianceLines renders each currency's buckets, total and beyond, then
// its rows.
func varianceLines(v map[string]any) []string {
	var out []string
	for _, c := range v["currencies"].([]any) {
		cm := c.(map[string]any)
		cur := cm["currency"].(string)
		for i, b := range cm["buckets"].([]any) {
			out = append(out, fmt.Sprintf("%s %v %s", cur, v["buckets"].([]any)[i].(map[string]any)["start"], cellText(b)))
		}
		out = append(out, cur+" total "+cellText(cm["total"]))
		if b, ok := cm["beyond"]; ok {
			out = append(out, cur+" beyond "+cellText(b))
		}
		for _, r := range cm["items"].([]any) {
			rm := r.(map[string]any)
			line := fmt.Sprintf("%s %s%v total %s", cur, strings.Repeat("  ", int(rm["level"].(float64))), rm["liquidity_item"], cellText(rm["total"]))
			if b, ok := rm["beyond"]; ok {
				line += " beyond " + cellText(b)
			}
			out = append(out, line)
		}
	}
	return out
}

// checkAdds checks that every cell's attribution adds up to its variance.
func checkAdds(t *testing.T, v map[string]any) {
	t.Helper()
	cents := func(m any) int64 {
		var n int64
		if _, err := fmt.Sscan(strings.ReplaceAll(strings.Fields(amount(m))[0], ".", ""), &n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	check := func(what string, c any) {
		cm := c.(map[string]any)
		at := cm["attribution"].(map[string]any)
		sum := cents(at["timing"]) + cents(at["amount"]) + cents(at["new"]) + cents(at["missing"]) + cents(at["unclassified"])
		if got, want := sum, cents(cm["variance"]); got != want {
			t.Errorf("%s: the attribution adds up to %d, want the variance %d", what, got, want)
		}
	}
	for _, c := range v["currencies"].([]any) {
		cm := c.(map[string]any)
		check(fmt.Sprint(cm["currency"], " total"), cm["total"])
		for i, b := range cm["buckets"].([]any) {
			check(fmt.Sprint(cm["currency"], " bucket ", i), b)
		}
		for _, r := range cm["items"].([]any) {
			rm := r.(map[string]any)
			check(fmt.Sprint(cm["currency"], " ", rm["liquidity_item"]), rm["total"])
			for i, b := range rm["buckets"].([]any) {
				check(fmt.Sprint(cm["currency"], " ", rm["liquidity_item"], " bucket ", i), b)
			}
		}
	}
}

// TestForecastVariance: the snapshot of 17 April against the banks' lines
// to 21 May (ADR-0051 §11, T53f3), by week: each document the snapshot
// forecast matched to the line that paid it (a run's payment, a payment
// in transit, a supplier item by its entry, a parked invoice by the entry
// it posted, a memo record and a series' occurrence by the lines that
// realised them), its part attributed to timing, amount or missing; what
// pays nothing forecast new, or unclassified when not matched; the days
// after the document level per item only.
func TestForecastVariance(t *testing.T) {
	e := setupVariance(t)
	v := e.varianceOf(t, "&from=2026-04-17&to=2026-05-21&lag=0&grain=week")
	want(t, "header", fmt.Sprint(v["company_code"], " ", v["read_date"], " ", v["time_zone"], " ", v["from"], " ", v["to"], " ", v["lag"], " ", v["grain"], " ",
		v["beyond"]), "3000 2026-05-22 UTC 2026-04-17 2026-05-21 0 week map[from:2026-05-18 to:2026-05-21]")
	s := v["snapshot"].(map[string]any)
	want(t, "snapshot", fmt.Sprint(s["id"], " ", s["kind"], " ", s["name"], " ", s["snapshot_date"], " ", s["document_level_until"]),
		e.snapshot+" on_demand Before the variance 2026-04-17 2026-05-17")
	want(t, "variance", varianceLines(v), []string{
		// The run's payment as forecast; the receipt 100.00 short; the second run's payment a week late.
		"CAD 2026-04-17 900.00 1100.00 200.00 22.22% 300.00/-100.00/0.00/0.00/0.00",
		// The series' occurrence, the request and the item as forecast; the credit memo never paid.
		"CAD 2026-04-24 -1050.00 -1450.00 -400.00 38.10% -300.00/0.00/0.00/-100.00/0.00",
		// The parked invoice paid as forecast once posted; a later invoice new; a receipt not matched.
		"CAD 2026-05-01 -300.00 -200.00 100.00 33.33% 0.00/0.00/-400.00/0.00/500.00",
		"CAD 2026-05-08 -500.00 0.00 500.00 100.00% 0.00/0.00/0.00/500.00/0.00",
		"CAD 2026-05-15 0.00 0.00 0.00 <nil>% 0.00/0.00/0.00/0.00/0.00",
		"CAD total -950.00 -550.00 400.00 42.11% 0.00/-100.00/-400.00/400.00/500.00",
		"CAD beyond -200.00 -200.00 0.00 0.00%",
		"CAD OPERATING total -950.00 -550.00 400.00 42.11% 0.00/-100.00/-400.00/400.00/500.00 beyond -200.00 -200.00 0.00 0.00%",
		"CAD   OP_SUPPLIERS total -3050.00 -3050.00 0.00 0.00% 0.00/0.00/-400.00/400.00/0.00 beyond -200.00 -200.00 0.00 0.00%",
		"CAD   REC_CUSTOMERS total 2100.00 2500.00 400.00 19.05% 0.00/-100.00/0.00/0.00/500.00 beyond 0.00 0.00 0.00 <nil>%",
		// The payment in transit as forecast; the planned memo records never paid.
		"USD 2026-04-17 -300.02 -300.00 0.02 0.01% 0.00/0.00/0.00/0.02/0.00",
		"USD 2026-04-24 -0.02 0.00 0.02 100.00% 0.00/0.00/0.00/0.02/0.00",
		"USD 2026-05-01 0.00 0.00 0.00 <nil>% 0.00/0.00/0.00/0.00/0.00",
		"USD 2026-05-08 0.00 0.00 0.00 <nil>% 0.00/0.00/0.00/0.00/0.00",
		"USD 2026-05-15 0.00 0.00 0.00 <nil>% 0.00/0.00/0.00/0.00/0.00",
		"USD total -300.04 -300.00 0.04 0.01% 0.00/0.00/0.00/0.04/0.00",
		"USD beyond 0.00 0.00 0.00 <nil>%",
		"USD OPERATING total -300.04 -300.00 0.04 0.01% 0.00/0.00/0.00/0.04/0.00 beyond 0.00 0.00 0.00 <nil>%",
		"USD   OP_SUPPLIERS total -300.04 -300.00 0.04 0.01% 0.00/0.00/0.00/0.04/0.00 beyond 0.00 0.00 0.00 <nil>%",
	})
	checkAdds(t, v)
	day := e.varianceOf(t, "&from=2026-04-17&to=2026-04-23&lag=0")
	want(t, "by day", varianceLines(day)[5:7], []string{
		"CAD 2026-04-22 -300.00 0.00 300.00 100.00% 300.00/0.00/0.00/0.00/0.00",
		"CAD 2026-04-23 2000.00 1900.00 -100.00 5.00% 0.00/-100.00/0.00/0.00/0.00",
	})
	checkAdds(t, day)
}

// TestForecastVarianceLags: the daily snapshot of a date is compared
// before one taken on demand that day; a lag of 30 days attributes the
// period's first day alone, the snapshot keeping the rest per bucket, and
// a period ending inside a snapshot's bucket is compared up to the day
// before it.
func TestForecastVarianceLags(t *testing.T) {
	e := setupVariance(t)
	daily := uuid.Must(uuid.NewV7())
	e.exec(t, `INSERT INTO cash.snapshot (tenant_id, id, legal_entity_id, kind, snapshot_date, time_zone, horizon_end, config_version, taken_by)
		VALUES ($1, $2, $3, 'daily', '2026-04-17', 'UTC', '2027-04-16', 1, $4)`, e.Tenant.ID, daily, e.le, e.Tenant.Human.ID)
	v := e.varianceOf(t, "&from=2026-04-24&to=2026-04-24")
	s := v["snapshot"].(map[string]any)
	want(t, "the daily snapshot, a week before", fmt.Sprint(s["id"], " ", s["kind"], " ", v["lag"]), daily.String()+" daily 7")
	want(t, "nothing forecast", varianceLines(v), []string{
		"CAD 2026-04-24 0.00 100.00 100.00 <nil>% 0.00/0.00/0.00/0.00/100.00", "CAD total 0.00 100.00 100.00 <nil>% 0.00/0.00/0.00/0.00/100.00",
		"CAD OPERATING total 0.00 100.00 100.00 <nil>% 0.00/0.00/0.00/0.00/100.00",
		"CAD   REC_CUSTOMERS total 0.00 100.00 100.00 <nil>% 0.00/0.00/0.00/0.00/100.00"})
	e.exec(t, `DELETE FROM cash.snapshot WHERE id = $1`, daily)
	v = e.varianceOf(t, "&from=2026-05-17&to=2026-05-21&lag=30&grain=week")
	want(t, "a month before", fmt.Sprint(v["snapshot"].(map[string]any)["id"], " ", v["buckets"], " ", v["beyond"]),
		e.snapshot+" [map[end:2026-05-17 grain:week start:2026-05-17]] map[from:2026-05-18 to:2026-05-21]")
	v = e.varianceOf(t, "&from=2026-05-17&to=2026-05-19&lag=30")
	want(t, "the bucket after the document level holding the last day", fmt.Sprint(v["beyond"]), "<nil>")
	e.API = apitest.NewWith(t, func(cfg *app.Config) {
		cfg.Now = func() time.Time { return time.Date(2026, 6, 9, 15, 0, 0, 0, time.UTC) }
	})
	v = e.varianceOf(t, "&from=2026-05-17&to=2026-06-08&lag=30&grain=month")
	want(t, "up to the bucket holding the last day", fmt.Sprint(v["beyond"]), "map[from:2026-05-18 to:2026-06-04]")
	want(t, "per item beyond", varianceLines(v)[2:3], []string{"CAD beyond -200.00 -200.00 0.00 0.00%"})
}

// TestForecastVarianceRefusals: a lag not offered, a grain not known, a
// period backwards, after the company's date or past the snapshot's
// horizon, or longer than its grain's limit is refused; a period whose
// snapshot was not taken is a conflict; another company's legal entity is
// not found.
func TestForecastVarianceRefusals(t *testing.T) {
	e := setupVariance(t)
	base := "/v1/liquidity-forecast-variance?legal_entity_id=" + e.le.String()
	for _, c := range []struct{ name, query, field string }{
		{"a lag not offered", "&from=2026-04-22&to=2026-04-30&lag=5", "lag"},
		{"a grain not known", "&from=2026-04-17&to=2026-04-30&lag=0&grain=year", "grain"},
		{"a period backwards", "&from=2026-04-17&to=2026-04-16&lag=0", "to"},
		{"too many days", "&from=2026-04-17&to=2026-07-31&lag=0", "to"},
		{"after the company's date", "&from=2026-04-17&to=2026-05-23&lag=0", "to"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := e.Problem(422, "request.validation_failed", "GET", base+c.query, "", "")
			want(t, "field", fmt.Sprint(p["errors"].([]any)[0].(map[string]any)["field"]), c.field)
		})
	}
	e.Problem(409, "cash.snapshot_not_taken", "GET", base+"&from=2026-04-17&to=2026-04-30", "", "")
	other := e.Reader(t, "reader of 1000", e.US)
	e.As(other).Problem(404, "request.not_found", "GET", base+"&from=2026-04-17&to=2026-04-30&lag=0", "", "")
	e.API = apitest.NewWith(t, func(cfg *app.Config) {
		cfg.Now = func() time.Time { return time.Date(2027, 5, 1, 12, 0, 0, 0, time.UTC) }
	})
	p := e.Problem(422, "request.validation_failed", "GET", base+"&from=2026-04-17&to=2027-04-20&lag=0&grain=month", "", "")
	want(t, "past the horizon", fmt.Sprint(p["errors"].([]any)[0].(map[string]any)["message"]), "on or before 2027-04-16, the snapshot's horizon")
}

// TestForecastVarianceTooLarge: a variance whose sums are out of the range
// of an amount is refused.
func TestForecastVarianceTooLarge(t *testing.T) {
	e := setupVariance(t)
	a := actualsEnv{forecastEnv: e.forecastEnv}
	lines := e.statement(t, e.main, "CAD", "2026-04-20", "2026-04-20", 0, 0, nil, "imported")
	a.actualLine(t, lines, 1, "2026-04-20", math.MaxInt64-1, "unmatched", "", target{})
	a.actualLine(t, lines, 2, "2026-04-20", math.MaxInt64-1, "unmatched", "", target{})
	e.Problem(422, "cash.amount_too_large", "GET", "/v1/liquidity-forecast-variance?legal_entity_id="+e.le.String()+"&from=2026-04-17&to=2026-04-21&lag=0", "", "")
}

// TestForecastVarianceFailures calls the variance outside a unit of work
// and with each of its database statements failing in turn.
func TestForecastVarianceFailures(t *testing.T) {
	e := setupVariance(t)
	from, to := time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC), time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
	read := func(ctx context.Context) error {
		_, err := app.Cash(app.Config{Now: func() time.Time { return varianceAt }}).Variance(ctx, cash.VarianceQuery{LegalEntity: e.le, From: from, To: to})
		return err
	}
	if err := read(context.Background()); !errors.Is(err, reqctx.ErrOutsideRequest) {
		t.Errorf("outside a unit of work: got %v, want reqctx.ErrOutsideRequest", err)
	}
	if _, err := (ledger.OpenItems{}).Sources(context.Background(), "payables", nil); !errors.Is(err, reqctx.ErrOutsideRequest) {
		t.Errorf("source documents outside a unit of work: got %v, want reqctx.ErrOutsideRequest", err)
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "variance", read)
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := app.Cash(app.Config{Now: func() time.Time { return varianceAt }}).GetLiquidityForecastVariance(ctx,
			gen.GetLiquidityForecastVarianceParams{LegalEntityID: e.le, From: from, To: to, Lag: gen.NewOptInt32(5)})
		return err
	})
	if p := problem.From(err); p == nil || p.Code != "request.validation_failed" || len(p.Errors) != 1 || p.Errors[0].Field != "lag" {
		t.Errorf("a lag not offered: got %v, want request.validation_failed on lag", err)
	}
}

// TestForecastVarianceNettedPayment: a line paying a payment whose items
// net to nothing is one piece, known by its payment: new when the
// snapshot did not forecast the payment.
func TestForecastVarianceNettedPayment(t *testing.T) {
	e := setupVariance(t)
	a := actualsEnv{forecastEnv: e.forecastEnv}
	invoice, credit := e.item(t, e.Supplier, "2026-05-01", "CAD", -10000, nil), e.item(t, e.Supplier, "2026-05-01", "CAD", 10000, nil)
	lines := e.statement(t, e.main, "CAD", "2026-05-12", "2026-05-12", 0, -500, nil, "imported")
	a.actualLine(t, lines, 1, "2026-05-12", -500, "posted", "matched",
		target{payments: []uuid.UUID{a.paid(t, e.Supplier, 100, map[uuid.UUID]int64{invoice: 10000, credit: -10000})}})
	v := e.varianceOf(t, "&from=2026-04-17&to=2026-05-21&lag=0&grain=week")
	want(t, "the week of the netted payment", varianceLines(v)[3], "CAD 2026-05-08 -500.00 -5.00 495.00 99.00% 0.00/0.00/-5.00/500.00/0.00")
	checkAdds(t, v)
}

// TestForecastVarianceEarly: a document forecast after the period's end
// and paid in it is timing, whatever the period's end, as it is with a
// later end; a line the snapshot keeps naming no document matches
// nothing and is missing.
func TestForecastVarianceEarly(t *testing.T) {
	e := setupVariance(t)
	lines := e.statement(t, e.main, "CAD", "2026-04-21", "2026-04-21", 0, -50000, nil, "imported")
	actualsEnv{forecastEnv: e.forecastEnv}.actualLine(t, lines, 1, "2026-04-21", -50000, "unmatched", "", target{})
	var planned uuid.UUID
	pgtest.Scan(t, `SELECT id FROM cash.memo_record WHERE legal_entity_id = $1 AND amount = -50000`, []any{e.le}, &planned)
	e.API = apitest.NewWith(t, func(cfg *app.Config) { cfg.Now = func() time.Time { return readAt } })
	e.realise(t, planned, "", lines, 1)
	e.API = apitest.NewWith(t, func(cfg *app.Config) { cfg.Now = func() time.Time { return varianceAt } })
	short := e.varianceOf(t, "&from=2026-04-17&to=2026-04-21&lag=0")
	want(t, "paid early, the period ending before its forecast", varianceLines(short)[4], "CAD 2026-04-21 0.00 -500.00 -500.00 <nil>% -500.00/0.00/0.00/0.00/0.00")
	long := e.varianceOf(t, "&from=2026-04-17&to=2026-05-21&lag=0")
	want(t, "paid early, the period holding its forecast", fmt.Sprint(varianceLines(long)[4], " ", varianceLines(long)[25]), "CAD 2026-04-21 0.00 -500.00 -500.00 <nil>% -500.00/0.00/0.00/0.00/0.00"+
		" CAD 2026-05-12 -500.00 0.00 500.00 100.00% 500.00/0.00/0.00/0.00/0.00")
	e.exec(t, `INSERT INTO cash.snapshot_flow (tenant_id, snapshot_id, line_no, flow_no, value_date, currency, source, certainty, late, liquidity_item, amount, flow_count)
		VALUES ($1, $2, 999, 999, '2026-04-21', 'CAD', 'memo', 'planned', false, 'OP_SUPPLIERS', -100, 1)`, e.Tenant.ID, uuid.MustParse(e.snapshot))
	short = e.varianceOf(t, "&from=2026-04-17&to=2026-04-21&lag=0")
	want(t, "a line naming no document", varianceLines(short)[4], "CAD 2026-04-21 -1.00 -500.00 -499.00 49900.00% -500.00/0.00/0.00/1.00/0.00")
	checkAdds(t, short)
}
