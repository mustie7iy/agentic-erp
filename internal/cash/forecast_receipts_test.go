package cash_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/collections"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

// receiptPerms are what the receipts' people hold beside the
// drill-down's: a sales order's read, so the drill-down names the orders,
// and a snapshot's taking.
var receiptPerms = append(slices.Clone(drillPerms), "sales.order.read", "cash.snapshot.take")

// withReceipts is the drill-down's forecast (withDrill) with Maple Inc's
// customer reconciliation account 121000, an output tax code O13 of 13%,
// and sales revenue (400000) derived to an inflow item of its own,
// REC_SALES.
func withReceipts(doc *config.Config) {
	withDrill(doc)
	co := doc.Company("3000")
	co.ChartOfAccounts = append(co.ChartOfAccounts, config.Account{Number: "121000", Name: "Receivables", Type: config.Asset, CustomerReconciliation: true},
		config.Account{Number: "175000", Name: "Output HST", Type: config.Liability})
	co.TaxCodes = append(co.TaxCodes, config.TaxCode{Code: "O13", Description: "Output HST", Direction: config.TaxOutput, RatePercent: "13", Account: "175000"})
	doc.Cash.LiquidityItems = append(doc.Cash.LiquidityItems,
		config.LiquidityItem{Code: "REC_SALES", Name: "Sales receipts", Direction: config.DirectionInflow, Parent: "OPERATING"})
	doc.Cash.Derivation.Accounts = append(doc.Cash.Derivation.Accounts, config.AccountDerivation{From: "400000", To: "400000", Item: "REC_SALES"})
}

// receivable writes a customer open item of the customer straight into
// the leading ledger: amount (positive for an invoice) on the receivables
// account against the offsetting account, dated day, with the terms kept
// beside it, special G/L when special is not "".
func (e drillEnv) receivable(t *testing.T, customer uuid.UUID, day, currency string, amount int64, tm *terms, offset, special string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	e.exec(t, `SET session_replication_role = replica`)
	defer e.exec(t, `RESET session_replication_role`)
	e.exec(t, `INSERT INTO ledger.journal_entry (tenant_id, fiscal_year, id, legal_entity_id, ledger_id, fiscal_period, document_type, document_number,
			document_date, posting_date, currency_tc, currency_lc, currency_gc, rate_tc_lc, rate_lc_gc, source_context, source_document_id, posted_by)
		VALUES ($1, 2026, $2, $3, $4, extract(month FROM $5::date), 'DR', $8, $5, $5, $6, 'CAD', 'EUR', 1, 1, 'receivables', gen_random_uuid(), $7)`,
		e.Tenant.ID, id, e.le, e.leading, day, currency, e.Tenant.Human.ID, id.String())
	var gl any
	if special != "" {
		gl = special
	}
	e.exec(t, `INSERT INTO ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no, legal_entity_id, ledger_id, fiscal_period, posting_date,
			account_id, partner_id, amount_tc, amount_lc, amount_gc, special_gl)
		SELECT $1, 2026, $2, n, $3, $4, extract(month FROM $5::date), $5, CASE WHEN n = 1 THEN $6::uuid ELSE $7::uuid END,
			CASE WHEN n = 1 THEN $8::uuid END, CASE WHEN n = 1 THEN $9::bigint ELSE -$9::bigint END, CASE WHEN n = 1 THEN $9::bigint ELSE -$9::bigint END,
			CASE WHEN n = 1 THEN $9::bigint ELSE -$9::bigint END, CASE WHEN n = 1 THEN $10::text END
		FROM generate_series(1, 2) n`, e.Tenant.ID, id, e.le, e.leading, day, e.Account(t, "3000", "121000"), e.Account(t, "3000", offset), customer, amount, gl)
	if tm != nil {
		e.exec(t, `INSERT INTO ledger.open_item_terms (tenant_id, fiscal_year, journal_entry_id, line_no, payment_term_code, baseline_date, net_days)
			VALUES ($1, 2026, $2, 1, 'NT30', $3, $4)`, e.Tenant.ID, id, tm.baseline, tm.net)
	}
	return id
}

// dunningBlock writes a dunning block of the customer, or of its item
// (entry, line 1) when entry is not nil, valid to the day, released when
// released is true.
func (e drillEnv) dunningBlock(t *testing.T, customer uuid.UUID, entry *uuid.UUID, validTo string, released bool) {
	t.Helper()
	var year, line, number any
	if entry != nil {
		year, line, number = 2026, 1, "DR1"
	}
	e.exec(t, `INSERT INTO collections.dunning_block (tenant_id, id, legal_entity_id, customer_id, customer_number, fiscal_year, journal_entry_id, line_no,
			document_number, reason, valid_to, released_at, released_by, created_by, updated_by)
		VALUES ($1, gen_random_uuid(), $2, $3, 'BP', $4, $5, $6, $7, 'Disputed by phone', $8, CASE WHEN $9 THEN now() END, CASE WHEN $9 THEN $10::uuid END,
			$10, $10)`, e.Tenant.ID, e.le, customer, year, entry, line, number, validTo, released, e.Tenant.Human.ID)
}

// disputeCase writes a dispute case of the customer's item (entry, line
// 1) in the status; the case's item is the header's, which
// default_dispute_case_item writes (000403).
func (e drillEnv) disputeCase(t *testing.T, customer, entry uuid.UUID, status string) {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	e.exec(t, `INSERT INTO collections.dispute_case (tenant_id, id, legal_entity_id, customer_id, customer_number, fiscal_year, journal_entry_id, line_no,
			document_number, currency, disputed_amount, currency_lc, disputed_amount_lc, reason, status, close_reason, closed_at, closed_by, created_by,
			updated_by)
		VALUES ($1, $2, $3, $4, 'BP', 2026, $5, 1, 'DR1', 'CAD', 100, 'CAD', 100, 'Short delivery', $6, CASE WHEN $6 <> 'open' THEN 'Decided' END,
			CASE WHEN $6 <> 'open' THEN now() END, CASE WHEN $6 <> 'open' THEN $7::uuid END, $7, $7)`,
		e.Tenant.ID, id, e.le, customer, entry, status, e.Tenant.Human.ID)
}

// receiptsScenario writes Maple Inc's customers' items and sales orders,
// read on Friday 17 April 2026 (Monday the 20th a holiday):
//   - Northwind (BP70000001): an invoice of 1,190.00 of 1 April, net 30,
//     due Friday 1 May (sales revenue); an invoice of 500.00 of 1 March
//     without terms and a credit memo of 200.00 of 10 April without terms,
//     both overdue, received together on the read date: 300.00 (an
//     expense account's 500.00 by the inflows' default, the credit's
//     revenue back); a USD invoice of 100.00 of 15 April, net 3, due
//     Saturday the 18th, received Tuesday the 21st; a down payment
//     (special G/L) left out; a released block of its first invoice and an
//     expired one of its USD invoice, neither holding them back;
//   - Fabrikam (BP70000002), blocked for dunning until the year's end: an
//     invoice of 300.00 due Friday the 24th, apart;
//   - Contoso (BP70000003): an invoice of 400.00 due the 24th in an open
//     dispute case, apart, and one of 250.00 whose case was rejected,
//     received that day;
//   - Litware (BP70000004): a credit of 80.00 alone, no receipt;
//   - Northwind's sales orders: SO-A confirmed on 10 April for a delivery
//     on the 30th, 1,000.00, due NT30 on Saturday 30 May, received Monday 1
//     June; SO-B confirmed on 1 February without a delivery date, 200.00
//     and 300.00, the first with 13% output tax (O13), the second with a
//     code that is no output code of Maple's and so adds none: 526.00,
//     billed by the lead days on 3 March and due on 2 April, received on
//     the read date; SO-D of 1 April delivered on the 1st and
//     billed by a document not posted, 1,000.00 still, due Friday 1 May; a
//     draft, an order blocked for credit and one billed and posted in full
//     adding nothing.
func (e drillEnv) receiptsScenario(t *testing.T) {
	t.Helper()
	north, fab := e.Customer(t, "BP70000001", "Northwind"), e.Customer(t, "BP70000002", "Fabrikam")
	contoso, litware := e.Customer(t, "BP70000003", "Contoso"), e.Customer(t, "BP70000004", "Litware")
	invoice := e.receivable(t, north, "2026-04-01", "CAD", 119000, &terms{baseline: "2026-04-01", net: 30}, "400000", "")
	e.docs["invoice"] = invoice
	e.docs["overdue"] = e.receivable(t, north, "2026-03-01", "CAD", 50000, nil, "520000", "")
	e.docs["credit"] = e.receivable(t, north, "2026-04-10", "CAD", -20000, nil, "400000", "")
	usd := e.receivable(t, north, "2026-04-15", "USD", 10000, &terms{baseline: "2026-04-15", net: 3}, "400000", "")
	e.docs["usd"] = usd
	e.receivable(t, north, "2026-04-01", "CAD", 99900, nil, "400000", "A")
	e.dunningBlock(t, north, &invoice, "2026-12-31", true)
	e.dunningBlock(t, north, &usd, "2026-04-16", false)
	e.docs["blocked"] = e.receivable(t, fab, "2026-04-01", "CAD", 30000, &terms{baseline: "2026-04-01", net: 23}, "400000", "")
	e.dunningBlock(t, fab, nil, "2026-12-31", false)
	e.docs["disputed"] = e.receivable(t, contoso, "2026-04-01", "CAD", 40000, &terms{baseline: "2026-04-01", net: 23}, "400000", "")
	e.disputeCase(t, contoso, e.docs["disputed"], "open")
	e.docs["rejected"] = e.receivable(t, contoso, "2026-04-01", "CAD", 25000, &terms{baseline: "2026-04-01", net: 23}, "400000", "")
	e.disputeCase(t, contoso, e.docs["rejected"], "rejected")
	e.receivable(t, litware, "2026-04-01", "CAD", -8000, nil, "400000", "")

	e.docs["SO-A"] = e.SalesOrder(t, e.le, north, "CAD", "2026-04-10", "confirmed", fixture.SalesItem{Net: 100000})
	e.docs["SO-B"] = e.SalesOrder(t, e.le, north, "CAD", "2026-02-01", "confirmed", fixture.SalesItem{Net: 20000}, fixture.SalesItem{Net: 30000})
	e.docs["SO-D"] = e.SalesOrder(t, e.le, north, "CAD", "2026-04-01", "confirmed", fixture.SalesItem{Net: 100000})
	e.BillingDocument(t, e.docs["SO-D"], "2026-04-02", false)
	e.SalesOrder(t, e.le, north, "CAD", "2026-04-01", "draft", fixture.SalesItem{Net: 70000})
	blocked := e.SalesOrder(t, e.le, north, "CAD", "2026-04-01", "confirmed", fixture.SalesItem{Net: 70000})
	billed := e.SalesOrder(t, e.le, north, "CAD", "2026-04-01", "confirmed", fixture.SalesItem{Net: 70000})
	e.BillingDocument(t, billed, "2026-04-02", true)
	e.exec(t, `SET session_replication_role = replica`)
	defer e.exec(t, `RESET session_replication_role`)
	e.exec(t, `UPDATE sales.sales_order SET requested_delivery_date = '2026-04-30' WHERE id = $1`, e.docs["SO-A"])
	e.exec(t, `UPDATE sales.sales_order SET requested_delivery_date = '2026-04-01' WHERE id = $1`, e.docs["SO-D"])
	e.exec(t, `UPDATE sales.sales_order SET credit_block = 'blocked' WHERE id = $1`, blocked)
	e.exec(t, `UPDATE sales.sales_order_item SET tax_code = CASE item_no WHEN 1 THEN 'O13' ELSE 'ZZ' END WHERE sales_order_id = $1`, e.docs["SO-B"])
}

// receiptsOf renders a forecast's currencies as their buckets with flows
// of receipts, then the total and what customers held back owe.
func receiptsOf(t *testing.T, f map[string]any) []string {
	t.Helper()
	buckets := f["buckets"].([]any)
	var out []string
	for _, c := range f["currencies"].([]any) {
		cm := c.(map[string]any)
		for i, b := range cm["buckets"].([]any) {
			var flows []string
			for _, fl := range b.(map[string]any)["flows"].([]any) {
				fm := fl.(map[string]any)
				flows = append(flows, fmt.Sprintf("%s/%s %s x%v", fm["source"], fm["certainty"], amount(fm["amount"]), fm["count"]))
			}
			if len(flows) > 0 {
				col := buckets[i].(map[string]any)
				out = append(out, fmt.Sprintf("%s %s: %s = %s", cm["currency"], col["start"], strings.Join(flows, ", "), amount(b.(map[string]any)["total"])))
			}
		}
		held := cm["blocked_receipts"].(map[string]any)
		out = append(out, fmt.Sprintf("%s total %s, held %s x%v", cm["currency"], amount(cm["total"]), amount(held["amount"]), held["count"]))
	}
	return out
}

// TestLiquidityForecastReceipts: the customers' open items and the open
// sales orders are the forecast's receipts (ADR-0051 §4, §5, As built
// T53l1): each item on its net due date, an overdue one on the read date,
// netted per customer as its payments net them, special G/L items left
// out; the items collections holds back apart, outside the totals unless
// asked; each confirmed order's part not yet posted as an invoice planned
// on its delivery date, else its order date plus the lead days, plus its
// payment term's net days; each derived to its liquidity items by its
// lines, drilled down to the customer and its documents, kept by a
// snapshot and summed by the group forecast.
func TestLiquidityForecastReceipts(t *testing.T) {
	e := drillEnv{forecastEnv: setupForecast(t, withReceipts, receiptPerms...), docs: map[string]uuid.UUID{}}
	e.receiptsScenario(t)
	want(t, "the receipts", receiptsOf(t, e.forecastOf(t, "")), []string{
		"CAD 2026-04-17: receivable/expected 300.00 CAD x1, sales_order/planned 526.00 CAD x1 = 826.00 CAD",
		"CAD 2026-04-24: receivable/expected 250.00 CAD x1, receivable_blocked/expected 700.00 CAD x2 = 250.00 CAD",
		"CAD 2026-05-01: receivable/expected 1190.00 CAD x1, sales_order/planned 1000.00 CAD x1 = 2190.00 CAD",
		"CAD 2026-05-29: sales_order/planned 1000.00 CAD x1 = 1000.00 CAD",
		"CAD total 4266.00 CAD, held 700.00 CAD x2",
		"USD 2026-04-21: receivable/expected 100.00 USD x1 = 100.00 USD",
		"USD total 100.00 USD, held 0.00 USD x0",
	})
	with := receiptsOf(t, e.forecastOf(t, "&include_blocked=true"))
	want(t, "held back, counted when asked", with[1], "CAD 2026-04-24: receivable/expected 250.00 CAD x1, receivable_blocked/expected 700.00 CAD x2 = 950.00 CAD")
	want(t, "the certainty cut", receiptsOf(t, e.forecastOf(t, "&min_certainty=expected"))[:2], []string{
		"CAD 2026-04-17: receivable/expected 300.00 CAD x1 = 300.00 CAD",
		"CAD 2026-04-24: receivable/expected 250.00 CAD x1, receivable_blocked/expected 700.00 CAD x2 = 250.00 CAD",
	})

	_, flows := e.drill(t, "&currency=CAD", 50)
	slices.Sort(flows) // flows of a day are paged by a digest of their documents' random ids
	want(t, "the drill-down", flows, []string{
		"2026-04-17 receivable/expected counted=true 300.00 CAD share 300.00: BP70000001; journal_entry overdue true 500.00, journal_entry credit true -200.00 (2)",
		"2026-04-17 sales_order/planned counted=true 526.00 CAD share 526.00: BP70000001; sales_order SO-B true 526.00 (1)",
		"2026-04-24 receivable/expected counted=true 250.00 CAD share 250.00: BP70000003; journal_entry rejected true 250.00 (1)",
		"2026-04-24 receivable_blocked/expected counted=false 300.00 CAD share 300.00: BP70000002; journal_entry blocked true 300.00 (1)",
		"2026-04-24 receivable_blocked/expected counted=false 400.00 CAD share 400.00: BP70000003; journal_entry disputed true 400.00 (1)",
		"2026-05-01 receivable/expected counted=true 1190.00 CAD share 1190.00: BP70000001; journal_entry invoice true 1190.00 (1)",
		"2026-05-01 sales_order/planned counted=true 1000.00 CAD share 1000.00: BP70000001; sales_order SO-D true 1000.00 (1)",
		"2026-06-01 sales_order/planned counted=true 1000.00 CAD share 1000.00: BP70000001; sales_order SO-A true 1000.00 (1)",
	})
	p, _ := e.drill(t, "&currency=CAD&liquidity_item=REC_SALES", 50)
	want(t, "sales revenue's receipts, the tax following its line", amount(p["total"]), "3766.00 CAD")
	p, _ = e.drill(t, "&currency=CAD&liquidity_item=REC_CUSTOMERS", 50)
	want(t, "the inflows' default", amount(p["total"]), "500.00 CAD")

	t.Run("a snapshot keeps the receipts", func(t *testing.T) {
		s := e.take(t, "Receipts")
		cad := s["currencies"].([]any)[0].(map[string]any)
		want(t, "the snapshot's totals", fmt.Sprint(amount(cad["total"]), " held ", amount(cad["blocked_receipts"])), "4266.00 CAD held 700.00 CAD")
		var lines []string
		for _, source := range []string{"receivable_blocked", "sales_order"} {
			l, _ := e.Must(200, "GET", fmt.Sprintf("/v1/cash-snapshots/%v/flows?currency=CAD&source=%s&page_size=500", s["id"], source), "", "")
			for _, it := range l["items"].([]any) {
				m := it.(map[string]any)
				who := "-"
				if c, ok := m["customer"].(map[string]any); ok {
					who = fmt.Sprint(c["number"])
				}
				doc := "per bucket"
				if d, ok := m["document"].(map[string]any); ok {
					doc = fmt.Sprint(d["kind"])
				}
				lines = append(lines, fmt.Sprintf("%v %v %v %s %s %s", m["value_date"], m["source"], m["liquidity_item"], amount(m["amount"]), who, doc))
			}
		}
		slices.Sort(lines)
		want(t, "the snapshot's lines", lines, []string{
			"2026-04-17 sales_order REC_SALES 526.00 CAD BP70000001 sales_order",
			"2026-04-24 receivable_blocked REC_SALES 300.00 CAD BP70000002 journal_entry",
			"2026-04-24 receivable_blocked REC_SALES 400.00 CAD BP70000003 journal_entry",
			"2026-05-01 sales_order REC_SALES 1000.00 CAD BP70000001 sales_order",
			"2026-05-29 sales_order REC_SALES 1000.00 CAD - per bucket",
		})
	})
	t.Run("the group forecast sums them", func(t *testing.T) {
		g, _ := e.Must(200, "GET", "/v1/group-liquidity-forecast", "", "")
		cad := g["currencies"].([]any)[0].(map[string]any)
		held := cad["blocked_receipts"].(map[string]any)
		want(t, "the group's CAD", fmt.Sprint(amount(cad["total"]), " held ", amount(held["amount"]), " x", held["count"]), "4266.00 CAD held 700.00 CAD x2")
	})
	t.Run("the analytics shares mark what is held back", func(t *testing.T) {
		var held []string
		err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			shares, err := app.Cash(app.Config{Now: func() time.Time { return readAt }}).ExtractForecastShares(ctx, e.le)
			for _, s := range shares {
				if s.Blocked {
					held = append(held, fmt.Sprintf("%s %d", s.Source, s.Amount))
				}
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(held)
		want(t, "the blocked shares", held, []string{"receivable_blocked 30000", "receivable_blocked 40000"})
	})
}

// TestLiquidityForecastReceiptsRefused: an open sales order whose
// payment term is not configured refuses the forecast rather than guess
// its due date.
func TestLiquidityForecastReceiptsRefused(t *testing.T) {
	e := drillEnv{forecastEnv: setupForecast(t, withReceipts, receiptPerms...), docs: map[string]uuid.UUID{}}
	north := e.Customer(t, "BP70000001", "Northwind")
	order := e.SalesOrder(t, e.le, north, "CAD", "2026-04-10", "confirmed", fixture.SalesItem{Net: 100000})
	e.exec(t, `SET session_replication_role = replica`)
	e.exec(t, `UPDATE sales.sales_order SET payment_term_code = 'NOPE' WHERE id = $1`, order)
	e.exec(t, `RESET session_replication_role`)
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := app.Cash(app.Config{Now: func() time.Time { return readAt }}).Forecast(ctx, cash.ForecastQuery{LegalEntity: e.le})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "payment term NOPE is not configured") {
		t.Errorf("a term not configured: got %v, want it refused", err)
	}
}

// TestLiquidityForecastReceiptsFailures: every database failure of the
// receipts' sources fails the forecast; their published reads need the
// request's transaction.
func TestLiquidityForecastReceiptsFailures(t *testing.T) {
	e := drillEnv{forecastEnv: setupForecast(t, withReceipts, receiptPerms...), docs: map[string]uuid.UUID{}}
	e.receiptsScenario(t)
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "forecast", func(ctx context.Context) error {
		_, err := app.Cash(app.Config{Now: func() time.Time { return readAt }}).Forecast(ctx, cash.ForecastQuery{LegalEntity: e.le})
		return err
	})
	ctx := context.Background()
	if _, _, err := (collections.Holds{}).Held(ctx, e.le, readAt); !errors.Is(err, reqctx.ErrOutsideRequest) {
		t.Errorf("holds outside a unit of work: got %v, want reqctx.ErrOutsideRequest", err)
	}
	if _, err := (&sales.Endpoints{}).CashOrders(ctx, e.le); !errors.Is(err, reqctx.ErrOutsideRequest) {
		t.Errorf("open sales orders outside a unit of work: got %v, want reqctx.ErrOutsideRequest", err)
	}
}
