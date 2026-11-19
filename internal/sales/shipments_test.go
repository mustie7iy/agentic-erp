package sales_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestShipmentConstraints: the database's refusals of what outbound
// deliveries hold answer with their problems (ADR-0083 §4); Ship and the
// order's change and cancellation refuse first under the order's lock, so
// these are reached only should a request pass them.
func TestShipmentConstraints(t *testing.T) {
	for constraint, want := range map[string]string{"sales_order_item_in_delivery": "sales.exceeds_open_quantity",
		"sales_order_item_removed_unshipped": "sales.order_item_delivered", "sales_order_cancelled_unshipped": "sales.order_delivered"} {
		t.Run(constraint, func(t *testing.T) {
			p := problem.From(&pgconn.PgError{Code: "23514", ConstraintName: constraint})
			if p.Code != want || p.Status != 409 {
				t.Errorf("got %d %s, want 409 %s", p.Status, p.Code, want)
			}
		})
	}
}

// TestDeliveryByGoodsIssue: an item of a material kept in stock is not
// confirmed as delivered by hand: it is delivered by an outbound
// delivery's goods issue (ADR-0083 §5), and nothing is delivered; an item
// the order lacks is refused as before; the materials are read in the
// request's transaction.
func TestDeliveryByGoodsIssue(t *testing.T) {
	e := setup(t)
	stocked := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO materials.material (tenant_id, id, number, legal_entity_id, description, base_uom_code, material_type, material_group,
		status, created_by, updated_by) VALUES ($1, $2, '100000009', $3, 'Machined bracket M10', 'EA', 'trading_good', 'BRACKETS', 'active', $4, $4)`,
		e.Tenant.ID, stocked, e.DE, e.Tenant.Human.ID)
	id, today := e.confirmed(t, e.ofMaterial(stocked, "2 EA", `, "unit_price": {"amount": "1.00", "currency": "EUR"}`))
	m := e.Problem(422, "sales.delivery_by_goods_issue", "POST", deliveries(id), delivered(today, "1 2 EA"), "")
	if f := field(m); f != "items[0].item_no" {
		t.Errorf("the field: got %v, want items[0].item_no", f)
	}
	e.Problem(422, "sales.unknown_order_item", "POST", deliveries(id), delivered(today, "9 1 EA"), "")
	h := endpoints(t, time.Now())
	order := uuid.MustParse(id)
	call := func(ctx context.Context) error {
		_, err := h.ConfirmSalesDelivery(ctx, &gen.SalesDeliveryCreate{DeliveryDate: instant(t, today+"T00:00:00Z"),
			Items: []gen.SalesDeliveryItemCreate{{ItemNo: 1, Quantity: gen.Quantity{Value: "2", Unit: "EA"}}}}, gen.ConfirmSalesDeliveryParams{ID: order})
		return err
	}
	if p := problem.From(e.inRequest(t, call)); p.Code != "sales.delivery_by_goods_issue" {
		t.Errorf("in a request: got %v, want sales.delivery_by_goods_issue", p)
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "confirm a stock item", func(ctx context.Context) error {
		if err := call(ctx); problem.From(err).Code != "sales.delivery_by_goods_issue" {
			return err
		}
		return nil
	})
}
