package revenue_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// issuedAndReversed records a goods issue's delivery of the quantity of
// the order's first item on the day and its reversal on another, as
// shipping's goods issue and its reversal have sales write them (ADR-0083
// §5 and As built S2b3).
func (e env) issuedAndReversed(t *testing.T, order, quantity, day, reversed string) {
	t.Helper()
	delivery := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO sales.delivery (tenant_id, id, sales_order_id, delivery_no, delivery_date, outbound_delivery_id, outbound_delivery_number,
			material_document_id, material_document_number, created_by)
		SELECT $1, $2, $3, 1, $4, gen_random_uuid(), 'OD1', gen_random_uuid(), 'MD1', $5 FROM (SELECT set_config('app.tenant_id', $1::uuid::text, true)) scoped`,
		e.Tenant.ID, delivery, order, day, e.Tenant.Human.ID)
	pgtest.Exec(t, `INSERT INTO sales.delivery_item (tenant_id, delivery_id, sales_order_id, order_item_no, quantity, cost_amount, cost_currency)
		SELECT $1, $2, $3, 1, $4::numeric, 100, 'EUR' FROM (SELECT set_config('app.tenant_id', $1::uuid::text, true)) scoped`,
		e.Tenant.ID, delivery, order, quantity)
	pgtest.Exec(t, `INSERT INTO sales.delivery_reversal (tenant_id, delivery_id, sales_order_id, reversal_date, material_document_id,
			material_document_number, created_by)
		SELECT $1, $2, $3, $4, gen_random_uuid(), 'MD2', $5 FROM (SELECT set_config('app.tenant_id', $1::uuid::text, true)) scoped`,
		e.Tenant.ID, delivery, order, reversed, e.Tenant.Human.ID)
}

// TestScheduleGoodsIssueReversed (ADR-0083 As built S2b3): a point-in-time
// obligation scheduled by deliveries loses a reversed goods issue's share
// in the period of the reversal's date, the goods issue's period keeping
// what it recognized, so after the reversal nothing of it is recognized
// and its quantity is unscheduled again.
func TestScheduleGoodsIssueReversed(t *testing.T) {
	e := setup(t)
	id, order := e.contract(t, e.goods())
	e.activated(t, id)
	e.issuedAndReversed(t, order, "60", "2026-01-15", "2026-02-10")
	t.Run("before the reversal", func(t *testing.T) {
		goods := e.mustSchedule(t, id, "2026-02-01").Obligations[0]
		if got, want := rows(goods.Periods), "2026/1 750.00 750.00 0.00 60 EA; 2026/2 -750.00 0.00 -750.00 -60 EA"; got != want {
			t.Errorf("the goods' periods: got %s, want %s", got, want)
		}
	})
	t.Run("after the reversal", func(t *testing.T) {
		goods := e.mustSchedule(t, id, "2026-03-01").Obligations[0]
		if got, want := rows(goods.Periods), "2026/1 750.00 750.00 0.00 60 EA; 2026/2 -750.00 -750.00 0.00 -60 EA"; got != want {
			t.Errorf("the goods' periods: got %s, want %s", got, want)
		}
		if got, want := sums(goods), "1500.00 = 0.00 + 0.00 + 1500.00"; got != want {
			t.Errorf("the goods' sums: got %s, want %s", got, want)
		}
	})
}
