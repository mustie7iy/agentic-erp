package sales_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

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
