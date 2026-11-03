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

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// withItems is withForecast with a tree of liquidity items, OPERATING
// (rent, supplier payments, taxes) and INVESTING (capital expenditure),
// rent on 530000, capital expenditure on 510000 and taxes on 610000,
// the planning group TAX_AUTH's items taxes, and supplier payments the
// default of outflows (ADR-0051 §4).
func withItems(doc *config.Config) {
	withForecast(doc)
	doc.Cash = config.Cash{
		LiquidityItems: []config.LiquidityItem{{Code: "OPERATING", Name: "Operating", Direction: config.DirectionBoth},
			{Code: "INVESTING", Name: "Investing", Direction: config.DirectionBoth},
			{Code: "OP_RENT", Name: "Rent", Direction: config.DirectionOutflow, Parent: "OPERATING"},
			{Code: "OP_SUPPLIERS", Name: "Supplier payments", Direction: config.DirectionOutflow, Parent: "OPERATING"},
			{Code: "TAX", Name: "Taxes", Direction: config.DirectionOutflow, Parent: "OPERATING"},
			{Code: "CAPEX", Name: "Capital expenditure", Direction: config.DirectionOutflow, Parent: "INVESTING"}},
		PlanningGroups: []config.PlanningGroup{{Code: "TAX_AUTH", Name: "Tax authorities"}},
		Derivation: config.Derivation{
			Accounts: []config.AccountDerivation{{From: "530000", To: "530000", Item: "OP_RENT"}, {From: "510000", To: "510000", Item: "CAPEX"},
				{From: "610000", To: "610000", Item: "TAX"}},
			PlanningGroups: []config.PlanningGroupDerivation{{Group: "TAX_AUTH", Item: "TAX"}},
			Defaults:       config.DerivationDefaults{Outflow: "OP_SUPPLIERS"}}}
}

// docLine is an offsetting line of a document a test writes: its
// account's number, its amount (a debit positive), and its tax code and
// the account key of the tax component it posts ("" for a base line;
// "withheld" for a withholding tax withheld at invoice).
type docLine struct {
	account   string
	amount    int64
	code, key string
}

// document writes a supplier document of the partner straight into the
// leading ledger, in CAD: what it owes (negative for an invoice) on the
// payables account against the lines, dated day and due 30 days later,
// with a tax item for each tax line and a withholding tax withheld at
// invoice for a line of key "withheld".
func (e forecastEnv) document(t *testing.T, partner uuid.UUID, day string, owed int64, lines ...docLine) uuid.UUID {
	t.Helper()
	id := e.item(t, partner, day, "CAD", owed, &terms{baseline: day, net: 30})
	e.exec(t, `SET session_replication_role = replica`)
	defer e.exec(t, `RESET session_replication_role`)
	e.exec(t, `DELETE FROM ledger.journal_line WHERE journal_entry_id = $1 AND line_no = 2`, id)
	for i, l := range lines {
		var code any
		if l.code != "" {
			code = l.code
		}
		e.exec(t, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period, posting_date,
				account_id, amount_tc, amount_lc, amount_gc, tax_code)
			VALUES ($1, 2026, $2, $3, $4, $5, extract(month FROM $6::date), $6, $7, $8, $8, $8, $9)`,
			e.Tenant.ID, id, i+2, e.le, e.leading, day, e.Account(t, "3000", l.account), l.amount, code)
		switch l.key {
		case "":
		case "withheld":
			e.exec(t, `INSERT INTO ledger.open_item_withholding (tenant_id, fiscal_year, journal_entry_id, line_no, code, posting, rate_pct, base_amount, amount,
					account_id) VALUES ($1, 2026, $2, 1, 'W10', 'invoice', 10, $3, $4, $5)`, e.Tenant.ID, id, -10*l.amount, -l.amount, e.Account(t, "3000", l.account))
		default:
			e.exec(t, `INSERT INTO ledger.tax_item (tenant_id, fiscal_year, journal_entry_id, item_no, legal_entity_id, code, component, direction, kind, account_key,
					account_id, tax_date, tax_rate_id, rate_pct, recoverable_pct, base_tc, tax_tc, recoverable_tc, base_lc, tax_lc, recoverable_lc)
				VALUES ($1, 2026, $2, $3, $4, $5, $6, 'input', 'standard', $6, $7, $8, gen_random_uuid(), 10, 0, 0, $9, 0, 0, $9, 0)`,
				e.Tenant.ID, id, i+1, e.le, l.code, l.key, e.Account(t, "3000", l.account), day, l.amount)
		}
	}
	return id
}

// itemsScenario writes Maple Inc's documents of every source, read on
// Friday 17 April 2026 (payment days Tuesday and Thursday):
//   - an invoice of 1,152.00 of 1 April, due 1 May, paid Thursday 30 April:
//     rent of 500.00 and equipment of 300.00 with 152.00 input tax on code
//     V1, which follows them in proportion (95.00 and 57.00), and a service
//     of 200.00 under reverse charge RC, whose self-assessed pair on 140000
//     and 610000 (taxes) moves no cash: rent 595.00, capital expenditure
//     357.00 and, by the default, supplier payments 200.00;
//   - a tax authority's (planning group TAX_AUTH) invoice of 300.00 of 1
//     March, overdue, paid Tuesday the 21st: taxes, by its planning group;
//     its request of 100.00 due Wednesday the 22nd, no lines: taxes; and
//     a proposed run's payment of 50.00 to it settling no item: taxes;
//   - rent of 1,000.00 of 10 April less 100.00 withheld at invoice (owed
//     to the authority, not the supplier), due Sunday 10 May, paid Thursday
//     7 May: rent 900.00;
//   - a proposed run's payment of 700.00 of the 17th settling an item of
//     equipment of 800.00 and a rent credit memo of 100.00: capital
//     expenditure 800.00 and rent back 100.00;
//   - a parked invoice of 200.00 of 15 April, its item on 530000, due 15
//     May, paid Thursday the 14th: rent, planned;
//   - the blocked vendor's item of 700.00 of 1 April, due 1 May: supplier
//     payments, apart.
func (e forecastEnv) itemsScenario(t *testing.T) {
	t.Helper()
	s1, tax, s3, s4 := e.NewSupplier(t, "BP90000031"), e.NewSupplier(t, "BP90000032"), e.NewSupplier(t, "BP90000033"), e.NewSupplier(t, "BP90000034")
	e.exec(t, `UPDATE partner.supplier_company SET planning_group = 'TAX_AUTH', version = version + 1 WHERE partner_id = $1 AND legal_entity_id = $2`, tax, e.le)
	e.document(t, s1, "2026-04-01", -115200, docLine{"530000", 50000, "V1", ""}, docLine{"510000", 30000, "V1", ""}, docLine{"140000", 15200, "V1", "input"},
		docLine{"540000", 20000, "RC", ""}, docLine{"140000", 1000, "RC", "self_input"}, docLine{"610000", -1000, "RC", "self_output"})
	e.item(t, tax, "2026-03-01", "CAD", -30000, nil)
	e.request(t, tax, 10000, "2026-04-22")
	toTax := e.payment(t, e.run(t, e.main, "2026-04-17", "CAD", "proposed"), 5000, "CAD", nil)
	e.exec(t, `UPDATE payments.payment SET supplier_id = $2 WHERE id = $1`, toTax, tax)
	e.document(t, s3, "2026-04-10", -90000, docLine{"530000", 100000, "", ""}, docLine{"400000", -10000, "", "withheld"})
	equipment := e.document(t, s1, "2026-04-01", -80000, docLine{"510000", 80000, "", ""})
	credit := e.document(t, s1, "2026-04-02", 10000, docLine{"530000", -10000, "", ""})
	pay := e.payment(t, e.run(t, e.main, "2026-04-17", "CAD", "proposed"), 70000, "CAD", nil)
	e.exec(t, `UPDATE payments.payment SET supplier_id = $2 WHERE id = $1`, pay, s1)
	e.exec(t, `INSERT INTO payments.payment_item (tenant_id, payment_id, item_fiscal_year, item_entry_id, item_line_no, amount, due_date)
		VALUES ($1, $2, 2026, $3, 1, 80000, '2026-05-01'), ($1, $2, 2026, $4, 1, -10000, '2026-04-02')`, e.Tenant.ID, pay, equipment, credit)
	parked := e.invoice(t, s4, "invoice", "parked", "2026-04-15", "CAD", 20000, 0, nil)
	e.exec(t, `INSERT INTO payables.supplier_invoice_item (tenant_id, supplier_invoice_id, item_no, quantity, uom_code, amount, account_id, tax_code)
		VALUES ($1, $2, 1, 1, 'EA', 20000, $3, 'V0'), ($1, $2, 2, 1, 'EA', 0, $3, NULL)`, e.Tenant.ID, parked, e.Account(t, "3000", "530000"))
	e.item(t, e.Vendor, "2026-04-01", "CAD", -70000, nil)
}

// itemsOf renders a forecast's rows by liquidity item of the currency:
// each item indented by its level with its buckets that have an amount,
// its total and beyond, then its own flows by source and certainty.
func itemsOf(t *testing.T, f map[string]any, cur string) []string {
	t.Helper()
	buckets := f["buckets"].([]any)
	nonzero := func(ms []any) string {
		var out []string
		for i, m := range ms {
			if a := amount(m); !strings.HasPrefix(a, "0.00 ") {
				out = append(out, fmt.Sprint(buckets[i].(map[string]any)["start"], " ", strings.TrimSuffix(a, " "+cur)))
			}
		}
		return strings.Join(out, ", ")
	}
	var out []string
	for _, c := range f["currencies"].([]any) {
		cm := c.(map[string]any)
		if cm["currency"] != cur {
			continue
		}
		for _, r := range cm["items"].([]any) {
			rm := r.(map[string]any)
			level := int(rm["level"].(float64))
			out = append(out, fmt.Sprintf("%s%v %v (%v): [%s] total %s beyond %s", strings.Repeat("  ", level), rm["liquidity_item"], rm["name"], rm["parent"],
				nonzero(rm["buckets"].([]any)), amount(rm["total"]), amount(rm["beyond"])))
			for _, fl := range rm["flows"].([]any) {
				fm := fl.(map[string]any)
				out = append(out, fmt.Sprintf("%s  %s/%s: [%s] total %s beyond %s", strings.Repeat("  ", level), fm["source"], fm["certainty"],
					nonzero(fm["buckets"].([]any)), amount(fm["total"]), amount(fm["beyond"])))
			}
		}
	}
	return out
}

// TestLiquidityForecastItems: the forecast's rows by liquidity item and
// its hierarchy (ADR-0051 §4, §9, T53e3): each flow split over the items
// of the documents it pays, by their offsetting lines in proportion, tax
// following its base lines, self-assessed pairs and withholding at
// invoice left out, then the planning group and the default; parents sum
// their children, and the top rows add up to the buckets' totals.
func TestLiquidityForecastItems(t *testing.T) {
	e := setupForecast(t, withItems)
	e.itemsScenario(t)
	f := e.forecastOf(t, "")
	want(t, "flows", flowsOf(t, f), []string{
		"CAD day 2026-04-17..2026-04-17: payment_run/confirmed/late=false -750.00 CAD x2 = -750.00 CAD",
		"CAD day 2026-04-21..2026-04-21: payable/expected/late=false -300.00 CAD x1 = -300.00 CAD",
		"CAD day 2026-04-22..2026-04-22: down_payment/expected/late=false -100.00 CAD x1 = -100.00 CAD",
		"CAD day 2026-04-30..2026-04-30: payable/expected/late=false -1152.00 CAD x1 = -1152.00 CAD",
		"CAD week 2026-05-01..2026-05-07: payable/expected/late=false -900.00 CAD x1, payable_blocked/expected/late=false -700.00 CAD x1 = -900.00 CAD",
		"CAD week 2026-05-08..2026-05-14: invoice_unposted/planned/late=false -200.00 CAD x1 = -200.00 CAD",
		"CAD total -3402.00 CAD, beyond 0.00 CAD x0, blocked -700.00 CAD x1",
	})
	rows := []string{
		"INVESTING Investing (<nil>): [2026-04-17 -800.00, 2026-04-30 -357.00] total -1157.00 CAD beyond 0.00 CAD",
		"  CAPEX Capital expenditure (INVESTING): [2026-04-17 -800.00, 2026-04-30 -357.00] total -1157.00 CAD beyond 0.00 CAD",
		"    payable/expected: [2026-04-30 -357.00] total -357.00 CAD beyond 0.00 CAD",
		"    payment_run/confirmed: [2026-04-17 -800.00] total -800.00 CAD beyond 0.00 CAD",
		"OPERATING Operating (<nil>): [2026-04-17 50.00, 2026-04-21 -300.00, 2026-04-22 -100.00, 2026-04-30 -795.00, 2026-05-01 -900.00, " +
			"2026-05-08 -200.00] total -2245.00 CAD beyond 0.00 CAD",
		"  OP_RENT Rent (OPERATING): [2026-04-17 100.00, 2026-04-30 -595.00, 2026-05-01 -900.00, 2026-05-08 -200.00] total -1595.00 CAD beyond 0.00 CAD",
		"    invoice_unposted/planned: [2026-05-08 -200.00] total -200.00 CAD beyond 0.00 CAD",
		"    payable/expected: [2026-04-30 -595.00, 2026-05-01 -900.00] total -1495.00 CAD beyond 0.00 CAD",
		"    payment_run/confirmed: [2026-04-17 100.00] total 100.00 CAD beyond 0.00 CAD",
		"  OP_SUPPLIERS Supplier payments (OPERATING): [2026-04-30 -200.00] total -200.00 CAD beyond 0.00 CAD",
		"    payable/expected: [2026-04-30 -200.00] total -200.00 CAD beyond 0.00 CAD",
		"    payable_blocked/expected: [2026-05-01 -700.00] total -700.00 CAD beyond 0.00 CAD",
		"  TAX Taxes (OPERATING): [2026-04-17 -50.00, 2026-04-21 -300.00, 2026-04-22 -100.00] total -450.00 CAD beyond 0.00 CAD",
		"    down_payment/expected: [2026-04-22 -100.00] total -100.00 CAD beyond 0.00 CAD",
		"    payable/expected: [2026-04-21 -300.00] total -300.00 CAD beyond 0.00 CAD",
		"    payment_run/confirmed: [2026-04-17 -50.00] total -50.00 CAD beyond 0.00 CAD",
	}
	want(t, "rows", itemsOf(t, f, "CAD"), rows)

	t.Run("with the blocked suppliers", func(t *testing.T) {
		with := append([]string{}, rows...)
		with[4] = "OPERATING Operating (<nil>): [2026-04-17 50.00, 2026-04-21 -300.00, 2026-04-22 -100.00, 2026-04-30 -795.00, 2026-05-01 -1600.00, " +
			"2026-05-08 -200.00] total -2945.00 CAD beyond 0.00 CAD"
		with[9] = "  OP_SUPPLIERS Supplier payments (OPERATING): [2026-04-30 -200.00, 2026-05-01 -700.00] total -900.00 CAD beyond 0.00 CAD"
		want(t, "rows", itemsOf(t, e.forecastOf(t, "&include_blocked=true"), "CAD"), with)
	})
	t.Run("the certainty cut", func(t *testing.T) {
		want(t, "confirmed rows", itemsOf(t, e.forecastOf(t, "&min_certainty=confirmed"), "CAD"), []string{
			"INVESTING Investing (<nil>): [2026-04-17 -800.00] total -800.00 CAD beyond 0.00 CAD",
			"  CAPEX Capital expenditure (INVESTING): [2026-04-17 -800.00] total -800.00 CAD beyond 0.00 CAD",
			"    payment_run/confirmed: [2026-04-17 -800.00] total -800.00 CAD beyond 0.00 CAD",
			"OPERATING Operating (<nil>): [2026-04-17 50.00] total 50.00 CAD beyond 0.00 CAD",
			"  OP_RENT Rent (OPERATING): [2026-04-17 100.00] total 100.00 CAD beyond 0.00 CAD",
			"    payment_run/confirmed: [2026-04-17 100.00] total 100.00 CAD beyond 0.00 CAD",
			"  TAX Taxes (OPERATING): [2026-04-17 -50.00] total -50.00 CAD beyond 0.00 CAD",
			"    payment_run/confirmed: [2026-04-17 -50.00] total -50.00 CAD beyond 0.00 CAD",
		})
	})
}

// TestLiquidityForecastUnclassified: with no derivation rule and no
// default, a flow has no item: one row without liquidity_item holds it,
// beyond the last bucket too.
func TestLiquidityForecastUnclassified(t *testing.T) {
	e := setupForecast(t, withForecast)
	e.forecastScenario(t)
	want(t, "rows", itemsOf(t, e.forecastOf(t, ""), "CAD"), []string{
		"<nil> <nil> (<nil>): [2026-04-17 -800.00, 2026-04-21 -1180.00, 2026-05-01 -500.00] total -2480.00 CAD beyond -4000.00 CAD",
		"  payable/expected: [2026-04-21 -1180.00, 2026-05-01 -500.00] total -1680.00 CAD beyond -4000.00 CAD",
		"  payment_run/confirmed: [2026-04-17 -800.00] total -800.00 CAD beyond 0.00 CAD",
	})
}

// TestLiquidityForecastItemsFailures: the forecast fails with the ports
// that read the documents' offsetting lines, the accounts of the unposted
// invoices' items and what the runs' payments pay.
func TestLiquidityForecastItemsFailures(t *testing.T) {
	e := setupForecast(t, withItems)
	e.itemsScenario(t)
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "forecast", func(ctx context.Context) error {
		_, err := app.Cash(app.Config{Now: func() time.Time { return readAt }}).Forecast(ctx, cash.ForecastQuery{LegalEntity: e.le})
		return err
	})
	if _, err := (ledger.OpenItems{}).Offsetting(context.Background(), nil); !errors.Is(err, reqctx.ErrOutsideRequest) {
		t.Errorf("offsetting lines outside a unit of work: got %v, want reqctx.ErrOutsideRequest", err)
	}
	if _, err := (payments.CashPosition{}).Paying(context.Background(), e.le, nil, readAt); !errors.Is(err, reqctx.ErrOutsideRequest) {
		t.Errorf("what payments pay outside a unit of work: got %v, want reqctx.ErrOutsideRequest", err)
	}
}

// outOfRange writes an invoice whose tax cannot be shared out over its
// base lines in the range of an amount.
func (e forecastEnv) outOfRange(t *testing.T) {
	t.Helper()
	e.document(t, e.NewSupplier(t, "BP90000039"), "2026-04-01", -100, docLine{"530000", math.MaxInt64, "V1", ""},
		docLine{"510000", -math.MaxInt64 + 1, "V1", ""}, docLine{"140000", math.MaxInt64, "V1", "input"})
}

// TestLiquidityForecastItemsOutOfRange: a document whose lines cannot be
// split in the range of an amount fails the forecast rather than
// classifying it wrongly.
func TestLiquidityForecastItemsOutOfRange(t *testing.T) {
	e := setupForecast(t, withItems)
	e.outOfRange(t)
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := app.Cash(app.Config{Now: func() time.Time { return readAt }}).Forecast(ctx, cash.ForecastQuery{LegalEntity: e.le})
		return err
	})
	if !errors.Is(err, money.ErrOverflow) {
		t.Errorf("got %v, want money.ErrOverflow", err)
	}
}
