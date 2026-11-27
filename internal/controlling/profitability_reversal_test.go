package controlling_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// reversed records the reversal of the order's goods issue's delivery on
// the day, as sales' ReverseIssue writes it (S2b3).
func (p profitable) reversed(t *testing.T, order, day string) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO sales.delivery_reversal (tenant_id, delivery_id, sales_order_id, reversal_date, material_document_id,
			material_document_number, created_by)
		SELECT $1, d.id, d.sales_order_id, $3, gen_random_uuid(), 'MD2', $4
		FROM (SELECT set_config('app.tenant_id', $1::uuid::text, true)) scoped, sales.delivery d WHERE d.sales_order_id = $2`,
		p.Tenant.ID, order, day, p.Tenant.Human.ID)
}

// TestProfitabilityCostOfSalesReversed (ADR-0083 As built S2b3): a goods
// issue's reversal takes its cost off the segment's cost of sales in the
// period of the reversal's date, the goods issue's period unchanged, so
// the year's cost nets out; the reversal of an order in another currency
// is left out and counted.
func TestProfitabilityCostOfSalesReversed(t *testing.T) {
	p := setupProfitable(t)
	shipped := p.order(t, p.northwind, "EUR", p.bracket("4"))
	p.goodsIssue(t, shipped, "2026-03-09", "4", 3000, "EUR")
	p.reversed(t, shipped, "2026-04-02")
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-01-01', 0.8, 'ECB', $2)`, p.Tenant.ID, p.Tenant.Human.ID)
	usd := p.order(t, p.northwind, "USD", p.installed("100.00", "USD"))
	p.goodsIssue(t, usd, "2026-03-09", "1", 900, "EUR")
	p.reversed(t, usd, "2026-04-02")
	cost := func(from, to string) (any, any) {
		t.Helper()
		r, _ := p.Must(200, "GET", report+"?legal_entity_id="+p.DE.String()+"&fiscal_year=2026&fiscal_period_from="+from+"&fiscal_period_to="+to, "", "")
		return r["totals"].(map[string]any)["cost_of_sales"].(map[string]any)["amount"], r["foreign_currency_documents"]
	}
	for _, c := range []struct {
		name, from, to, cost string
		foreign              float64
	}{
		{"March, the goods issue's period", "3", "3", "30.00", 1},
		{"April, the reversal's period", "4", "4", "-30.00", 1},
		{"the year", "1", "12", "0.00", 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, foreign := cost(c.from, c.to)
			if got != c.cost {
				t.Errorf("cost of sales: got %v, want %s", got, c.cost)
			}
			if foreign != c.foreign {
				t.Errorf("foreign currency documents: got %v, want %v", foreign, c.foreign)
			}
		})
	}
}

// returned bills and posts on the day what the order's goods issue
// delivered, takes all of its first item back by a returns order and
// records its receipt into stock on the day it came back, at the cost in
// EUR, as sales' ConfirmReceipt writes it (S2b4).
func (p profitable) returned(t *testing.T, order, billed, back, quantity string, cost int64) {
	t.Helper()
	doc, tag := p.Must(201, "POST", "/v1/billing-documents", fmt.Sprintf(`{"sales_order_id": %q, "billing_date": %q}`, order, billed), "")
	p.Must(200, "POST", "/v1/billing-documents/"+doc["id"].(string)+":post", fmt.Sprintf(`{"posting_date": %q}`, billed), tag)
	r, tag := p.Must(201, "POST", "/v1/returns-orders", fmt.Sprintf(`{"billing_document_id": %q, "reason": "defective",
		"items": [{"billing_item_no": 1, "quantity": {"value": %q, "unit": "EA"}}]}`, doc["id"], quantity), "")
	p.Must(200, "POST", "/v1/returns-orders/"+r["id"].(string)+":confirm", "", tag)
	receipt, scoped := uuid.Must(uuid.NewV7()), `FROM (SELECT set_config('app.tenant_id', $1::uuid::text, true)) scoped`
	pgtest.Exec(t, `INSERT INTO sales.return_receipt (tenant_id, id, returns_order_id, receipt_no, receipt_date, created_by)
		SELECT $1, $2, $3, 1, $4, $5 `+scoped, p.Tenant.ID, receipt, r["id"], back, p.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO sales.return_receipt_item (tenant_id, receipt_id, returns_order_id, item_no, quantity)
		SELECT $1, $2, $3, 1, $4::numeric `+scoped, p.Tenant.ID, receipt, r["id"], quantity)
	pgtest.Exec(t, `INSERT INTO sales.return_receipt_posting (tenant_id, receipt_id, returns_order_id, material_document_id, material_document_number,
			currency, created_by) SELECT $1, $2, $3, gen_random_uuid(), 'MD3', 'EUR', $4 `+scoped, p.Tenant.ID, receipt, r["id"], p.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO sales.return_receipt_item_cost (tenant_id, receipt_id, item_no, cost_amount) SELECT $1, $2, 1, $3 `+scoped,
		p.Tenant.ID, receipt, cost)
}

// TestProfitabilityCostOfSalesReturned (ADR-0083 As built S2b4): a
// customer's return received into stock takes the cost it took back off
// the segment's cost of sales in the period of the receipt's date, the
// goods issue's period unchanged; one of an order in another currency is
// left out and counted.
func TestProfitabilityCostOfSalesReturned(t *testing.T) {
	p := setupProfitable(t)
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-01-01', 0.8, 'ECB', $2)`, p.Tenant.ID, p.Tenant.Human.ID)
	shipped := p.order(t, p.northwind, "EUR", p.bracket("4"))
	p.goodsIssue(t, shipped, "2026-03-09", "4", 3000, "EUR")
	p.returned(t, shipped, "2026-03-10", "2026-04-02", "1", 750)
	usd := p.order(t, p.northwind, "USD", p.installed("100.00", "USD"))
	p.goodsIssue(t, usd, "2026-03-09", "1", 900, "EUR")
	p.returned(t, usd, "2026-03-10", "2026-04-02", "1", 900)
	for _, c := range []struct {
		name, from, to, cost string
		foreign              float64
	}{
		{"March, the goods issue's period", "3", "3", "30.00", 2},
		{"April, the return's period", "4", "4", "-7.50", 1},
		{"the year", "1", "12", "22.50", 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, _ := p.Must(200, "GET", report+"?legal_entity_id="+p.DE.String()+"&fiscal_year=2026&fiscal_period_from="+c.from+"&fiscal_period_to="+c.to, "", "")
			if got := r["totals"].(map[string]any)["cost_of_sales"].(map[string]any)["amount"]; got != c.cost {
				t.Errorf("cost of sales: got %v, want %s", got, c.cost)
			}
			if got := r["foreign_currency_documents"]; got != c.foreign {
				t.Errorf("foreign currency documents: got %v, want %v (the USD order's billing and goods issue in March, its return in April)", got, c.foreign)
			}
		})
	}
}
