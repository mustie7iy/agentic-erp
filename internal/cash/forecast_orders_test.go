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
	"github.com/mustie7iy/agentic-erp/internal/payables"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// orderPerms are what the order source's people hold beside the
// drill-down's: an order's read, so the drill-down names the orders.
var orderPerms = append(slices.Clone(drillPerms), "procurement.order.read", "cash.snapshot.take")

// orderLine is an item of an order a test writes: quantity of EA at a
// price per unit in minor units, the account's number, a tax code and a
// delivery date ("" for none).
type orderLine struct {
	qty                  string
	price                int64
	account, code, deliv string
}

// purchaseOrder writes a purchase order of Maple Inc in CAD straight into
// procurement in the status, numbered number, with its items.
func (e forecastEnv) purchaseOrder(t *testing.T, supplier uuid.UUID, number, term, ordered, status string, lines ...orderLine) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	e.exec(t, `SET session_replication_role = replica`) // a released order's items, and a term that is not configured
	defer e.exec(t, `RESET session_replication_role`)
	e.exec(t, `INSERT INTO procurement.purchase_order (tenant_id, id, document_number, legal_entity_id, supplier_id, currency, order_date,
			payment_term_code, status, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, 'CAD', $6, $7, $8, $9, $9)`,
		e.Tenant.ID, id, number, e.le, supplier, ordered, term, status, e.Tenant.Human.ID)
	for i, l := range lines {
		var code, deliv any
		if l.code != "" {
			code = l.code
		}
		if l.deliv != "" {
			deliv = l.deliv
		}
		e.exec(t, `INSERT INTO procurement.purchase_order_item (tenant_id, purchase_order_id, item_no, description, quantity, uom_code, unit_price,
				account_id, tax_code, valuation_price, valuation_price_unit, delivery_date)
			VALUES ($1, $2, $3, 'Goods', $4::numeric, 'EA', $5, $6, $7, $5, 1, $8)`,
			e.Tenant.ID, id, i+1, l.qty, l.price, e.Account(t, "3000", l.account), code, deliv)
	}
	return id
}

// invoiced writes a supplier document of the kind in the status on the
// order's item, of the quantity.
func (e forecastEnv) invoiced(t *testing.T, supplier, order uuid.UUID, item int, kind, status, qty string) uuid.UUID {
	t.Helper()
	v := e.invoice(t, supplier, kind, "parked", "2026-04-15", "CAD", 100, 0, &order)
	e.exec(t, `SET session_replication_role = replica`)
	defer e.exec(t, `RESET session_replication_role`)
	if status == "posted" { // its entry is not read: a posted invoice's open item is the ledger's
		e.exec(t, `UPDATE payables.supplier_invoice SET status = 'posted', journal_entry_year = 2026, journal_entry_id = gen_random_uuid() WHERE id = $1`, v)
	}
	e.exec(t, `INSERT INTO payables.supplier_invoice_item (tenant_id, supplier_invoice_id, item_no, purchase_order_id, po_item_no, quantity, uom_code, amount)
		VALUES ($1, $2, 1, $3, $4, $5::numeric, 'EA', 100)`, e.Tenant.ID, v, order, item, qty)
	return v
}

// ordersScenario writes Maple Inc's purchase orders, read on Friday 17
// April 2026 (payment days Tuesday and Thursday), SK20 being 2% within 20
// days, net 30:
//   - PO-A of supplier a, SK20, ordered 1 April: item 1, 10 EA at 100.00
//     on capital expenditure (510000) with V1 (19%), delivered 24 April,
//     of which 4 EA are posted, 2 parked (an invoice_unposted flow of its
//     own) and 3 on a cancelled invoice: 4 EA open, 476.00 with its tax,
//     less the order's open down payment request of 100.00 (its own
//     down_payment flow; a cancelled one of 999.00 not), 2% until Thursday
//     14 May, so 368.48 then; item 2, 5 EA at 20.00
//     (520000) and item 4, 2 EA at 10.00 with a code Maple does not have,
//     credited 1 EA by a credit memo of nothing invoiced, both without a
//     delivery date, so invoiced 30 days after the order, Friday 1 May:
//     one document of 120.00 without tax, 2% until Thursday 21 May,
//     117.60; item 3, 3 EA at 10.00 invoiced in SAP before migration:
//     nothing;
//   - PO-B of supplier a, a term Maple does not have, ordered 10 April: 1
//     EA at 50.00 delivered that day, due by the supplier's NT30 on Sunday
//     10 May, paid Thursday the 7th; and 1 EA at 5.00 invoiced twice:
//     nothing;
//   - PO-C of the vendor blocked for payment, SK20: 1 EA at 70.00
//     delivered 1 May, apart on its net due date, 31 May;
//   - PO-D a draft and PO-E closed, with open items: nothing.
func (e forecastEnv) ordersScenario(t *testing.T) map[string]uuid.UUID {
	t.Helper()
	a := e.NewSupplier(t, "BP90000071")
	e.exec(t, `INSERT INTO partner.payment_term (tenant_id, code, description, net_days, discount_days, discount_pct, created_by, updated_by)
		VALUES ($1, 'SK20', '2% 20, net 30', 30, 20, 2, $2, $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	docs := map[string]uuid.UUID{}
	docs["PO-A"] = e.purchaseOrder(t, a, "PO-A", "SK20", "2026-04-01", "released",
		orderLine{"10", 10000, "510000", "V1", "2026-04-24"}, orderLine{"5", 2000, "520000", "", ""}, orderLine{"3", 1000, "520000", "", ""},
		orderLine{"2", 1000, "520000", "Q9", ""})
	e.invoiced(t, a, docs["PO-A"], 1, "invoice", "posted", "4")
	docs["parked"] = e.invoiced(t, a, docs["PO-A"], 1, "invoice", "parked", "2")
	cancelled := e.invoiced(t, a, docs["PO-A"], 1, "invoice", "parked", "3")
	e.exec(t, `UPDATE payables.supplier_invoice SET status = 'cancelled', version = version + 1 WHERE id = $1`, cancelled)
	e.invoiced(t, a, docs["PO-A"], 4, "credit_memo", "posted", "1")
	e.exec(t, `INSERT INTO payables.migrated_invoice_item (tenant_id, migrated_invoice_id, item_no, reference, kind, purchase_order_id, po_item_no, quantity,
			created_by) VALUES ($1, gen_random_uuid(), 1, 'SAP-1', 'invoice', $2, 3, 3, $3)`, e.Tenant.ID, docs["PO-A"], e.Tenant.Human.ID)
	for status, amount := range map[string]int64{"open": 10000, "cancelled": 99900} {
		r := e.request(t, a, amount, "2026-04-22")
		e.exec(t, `SET session_replication_role = replica`)
		e.exec(t, `UPDATE payables.down_payment_request SET purchase_order_id = $2, status = $3,
			cancel_reason = CASE WHEN $3 = 'cancelled' THEN 'no longer needed' END WHERE id = $1`, r, docs["PO-A"], status)
		e.exec(t, `RESET session_replication_role`)
	}
	docs["PO-B"] = e.purchaseOrder(t, a, "PO-B", "NOPE", "2026-04-10", "released",
		orderLine{"1", 5000, "520000", "", "2026-04-10"}, orderLine{"1", 500, "520000", "", "2026-04-10"})
	e.invoiced(t, a, docs["PO-B"], 2, "invoice", "posted", "2")
	docs["PO-C"] = e.purchaseOrder(t, e.Vendor, "PO-C", "SK20", "2026-04-01", "released", orderLine{"1", 7000, "520000", "", "2026-05-01"})
	e.purchaseOrder(t, a, "PO-D", "SK20", "2026-04-01", "draft", orderLine{"1", 9900, "520000", "", ""})
	e.purchaseOrder(t, a, "PO-E", "SK20", "2026-04-01", "closed", orderLine{"1", 9900, "520000", "", ""})
	return docs
}

// TestLiquidityForecastOrders: the released orders' open values are
// forecast as the invoices they expect (planned, ADR-0051 §5, decision P7,
// T53g): net of what is invoiced, posted or not, plus the tax their codes
// give on the read date, invoiced on the items' delivery dates, else the
// order date plus the lead days, and paid by the orders' terms; a blocked
// vendor's apart; each flow's liquidity items derived from the items'
// accounts, and drilled down to the orders.
func TestLiquidityForecastOrders(t *testing.T) {
	e := drillEnv{forecastEnv: setupForecast(t, withDrill, orderPerms...), docs: map[string]uuid.UUID{}}
	for name, id := range e.ordersScenario(t) {
		e.docs[name] = id
	}
	_, flows := e.drill(t, "&currency=CAD&include_blocked=true", 50)
	var orders []string
	for _, f := range flows {
		if strings.Contains(f, "purchase_order") {
			orders = append(orders, f)
		}
	}
	want(t, "order flows", orders, []string{
		"2026-05-07 purchase_order/planned counted=true -50.00 CAD share -50.00: BP90000071; purchase_order PO-B true -50.00 (1)",
		"2026-05-14 purchase_order/planned counted=true -368.48 CAD share -368.48: BP90000071; purchase_order PO-A true -368.48 (1)",
		"2026-05-21 purchase_order/planned counted=true -117.60 CAD share -117.60: BP90000071; purchase_order PO-A true -117.60 (1)",
		"2026-05-31 payable_blocked/planned counted=true -70.00 CAD share -70.00: BP90000002; purchase_order PO-C true -70.00 (1)",
	})
	_, capex := e.drill(t, "&currency=CAD&liquidity_item=CAPEX", 50)
	want(t, "capital expenditure, the item's account with its tax", capex, []string{
		"2026-05-14 purchase_order/planned counted=true -368.48 CAD share -368.48: BP90000071; purchase_order PO-A true -368.48 (1)",
	})
	f := e.forecastOf(t, "")
	cad := f["currencies"].([]any)[0].(map[string]any)
	want(t, "blocked, the vendor's order", amount(cad["blocked"].(map[string]any)["amount"]), "-70.00 CAD")
	planned := e.forecastOf(t, "&min_certainty=expected")
	for _, c := range planned["currencies"].([]any) {
		for _, b := range c.(map[string]any)["buckets"].([]any) {
			for _, fl := range b.(map[string]any)["flows"].([]any) {
				if s := fl.(map[string]any)["source"]; s == cash.SourcePurchaseOrder {
					t.Errorf("certainty cut at expected: got a %s flow, want none", s)
				}
			}
		}
	}

	t.Run("a snapshot keeps each order", func(t *testing.T) {
		s := e.take(t, "Orders")
		p, _ := e.Must(200, "GET", fmt.Sprintf("/v1/cash-snapshots/%v/flows?currency=CAD&source=purchase_order&page_size=500", s["id"]), "", "")
		var lines []string
		for _, it := range p["items"].([]any) {
			l := it.(map[string]any)
			doc := "per bucket to " + fmt.Sprint(l["bucket_end"])
			if d, ok := l["document"].(map[string]any); ok {
				doc = fmt.Sprintf("%v %v", d["kind"], d["number"])
			}
			lines = append(lines, fmt.Sprintf("%v %v %s %s", l["value_date"], l["liquidity_item"], amount(l["amount"]), doc))
		}
		want(t, "the snapshot's order lines", lines, []string{
			"2026-05-07 OP_SUPPLIERS -50.00 CAD purchase_order PO-B",
			"2026-05-14 CAPEX -368.48 CAD purchase_order PO-A",
			"2026-05-18 OP_SUPPLIERS -117.60 CAD per bucket to 2026-05-21",
		})
	})
}

// TestLiquidityForecastOrdersFailures: every database failure of the order
// source fails the forecast; its ports need the request's transaction.
func TestLiquidityForecastOrdersFailures(t *testing.T) {
	e := setupForecast(t, withDrill, orderPerms...)
	e.ordersScenario(t)
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "forecast", func(ctx context.Context) error {
		_, err := app.Cash(app.Config{Now: func() time.Time { return readAt }}).Forecast(ctx, cash.ForecastQuery{LegalEntity: e.le})
		return err
	})
	ctx := context.Background()
	if _, err := (&payables.Endpoints{}).OpenOrders(ctx, e.le, readAt); !errors.Is(err, reqctx.ErrOutsideRequest) {
		t.Errorf("open orders outside a unit of work: got %v, want reqctx.ErrOutsideRequest", err)
	}
	if _, err := (&payables.Endpoints{}).InvoiceOrders(ctx, nil); !errors.Is(err, reqctx.ErrOutsideRequest) {
		t.Errorf("invoices' orders outside a unit of work: got %v, want reqctx.ErrOutsideRequest", err)
	}
	if _, err := (procurement.Orders{}).Released(ctx, e.le); !errors.Is(err, reqctx.ErrOutsideRequest) {
		t.Errorf("released orders outside a unit of work: got %v, want reqctx.ErrOutsideRequest", err)
	}
}

// TestForecastVarianceOrder: an order the snapshot forecast, paid through
// the invoice entered on it after the snapshot, is matched by the order:
// its variance is the amount paid less, not a missing order and a new
// payment (ADR-0051 §11, T53g).
func TestForecastVarianceOrder(t *testing.T) {
	e := drillEnv{forecastEnv: setupForecast(t, withDrill, orderPerms...), docs: map[string]uuid.UUID{}}
	a := actualsEnv{forecastEnv: e.forecastEnv}
	s := e.NewSupplier(t, "BP90000072")
	order := e.purchaseOrder(t, s, "PO-V", "NT30", "2026-04-01", "released", orderLine{"1", 30000, "520000", "", "2026-04-17"})
	e.take(t, "Before the order is paid")
	invoice := e.invoice(t, s, "invoice", "parked", "2026-04-17", "CAD", 25000, 0, &order)
	item := e.item(t, s, "2026-04-17", "CAD", -25000, nil)
	e.exec(t, `SET session_replication_role = replica`)
	e.exec(t, `UPDATE ledger.journal_entry SET source_document_id = $2 WHERE id = $1`, item, invoice)
	e.exec(t, `RESET session_replication_role`)
	cad := e.statement(t, e.main, "CAD", "2026-05-14", "2026-05-14", 1000000, 975000, nil, "imported")
	a.actualLine(t, cad, 1, "2026-05-14", -25000, "posted", "matched", target{payments: []uuid.UUID{a.paid(t, s, 25000, map[uuid.UUID]int64{item: 25000})}})
	v := varianceEnv{drillEnv: e}
	v.API = apitest.NewWith(t, func(cfg *app.Config) { cfg.Now = func() time.Time { return varianceAt } })
	got := varianceLines(v.varianceOf(t, "&from=2026-04-17&to=2026-05-16&lag=0&grain=month"))
	want(t, "the CAD total, the order paid 50.00 less", got[slices.IndexFunc(got, func(l string) bool { return strings.HasPrefix(l, "CAD total") })],
		"CAD total -300.00 -250.00 50.00 16.67% 0.00/50.00/0.00/0.00/0.00")
}
