package controlling_test

import (
	"testing"

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
