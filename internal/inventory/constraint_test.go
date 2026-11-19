package inventory_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestConstraints: the database's refusals the context registers answer
// with their problems (takeStock refuses a shortage, freeze an open count
// and blocked a posting under a count first, so the database's refusals
// are reached only should a line pass them).
func TestConstraints(t *testing.T) {
	for constraint, want := range map[string]string{"stock_not_negative": "inventory.insufficient_stock",
		"count_item_one_open": "inventory.count_open", "count_posting_block": "inventory.posting_blocked"} {
		t.Run(constraint, func(t *testing.T) {
			p := problem.From(&pgconn.PgError{Code: "23514", ConstraintName: constraint})
			if p.Code != want || p.Status != 409 {
				t.Errorf("got %d %s, want 409 %s", p.Status, p.Code, want)
			}
		})
	}
}
