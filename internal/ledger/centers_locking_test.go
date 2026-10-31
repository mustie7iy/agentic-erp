package ledger_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// rowLockWaits reports whether a transaction of the test database waits
// for a row lock another holds.
func rowLockWaits(t *testing.T) func() bool {
	t.Helper()
	admin := pgtest.Admin(t)
	return func() bool {
		var n int
		if err := admin.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND wait_event IN ('transactionid', 'tuple')`).Scan(&n); err != nil {
			t.Fatalf("read the lock waits: %v", err)
		}
		return n > 0
	}
}

// TestCenterChangesSerialize: two requests on one center queue on it, so
// the second sees the first's pending change; and a profit center's
// redate waits for a cost center's move to it, then sees the period it
// wrote and is refused, so no cost center period outlives its profit
// center.
func TestCenterChangesSerialize(t *testing.T) {
	c := setupCenters(t)
	h := endpoints()
	cc := c.costCenter(t, "1000", "")
	c.decide(t, cc["pending_change_id"], "approve")
	ccID := uuid.MustParse(cc["id"].(string))
	t.Run("two requests", func(t *testing.T) {
		lock := func(ctx context.Context) error {
			_, err := h.LockCostCenter(ctx, &gen.CostCenterLock{ValidFrom: date("2026-05-01"), LockPlan: gen.NewOptBool(true), Reason: "r"},
				gen.LockCostCenterParams{ID: ccID, IfMatch: "*"})
			return err
		}
		move := func(ctx context.Context) error {
			_, err := h.MoveCostCenter(ctx, &gen.CostCenterMove{ValidFrom: date("2026-05-01"), Category: gen.NewOptString("SALES"), Reason: "r"},
				gen.MoveCostCenterParams{ID: ccID, IfMatch: "*"})
			return err
		}
		first, second := serializeOn(t, c.ten, "the cost center", rowLockWaits(t), lock, move)
		if first != nil {
			t.Fatalf("first request: got %v, want it committed", first)
		}
		if got := problem.From(second).Code; got != "ledger.center_change_pending" {
			t.Errorf("second request: got %v, want ledger.center_change_pending", second)
		}
		page, _ := c.Must(200, "GET", fmt.Sprintf("/v1/center-changes?cost_center_id=%s&status=pending", ccID), "", "")
		c.decide(t, page["items"].([]any)[0].(map[string]any)["id"], "reject")
	})
	t.Run("a redate and a move to the profit center", func(t *testing.T) {
		c := c.in(t)
		pc := c.profitCenter(t, "PC", "", c.de)
		_, tag := c.Must(200, "GET", pc, "", "")
		redate, _ := c.Must(200, "POST", pc+":redate", `{"valid_to": "2026-09-01", "reason": "Closing"}`, tag)
		_, tag = c.Must(200, "GET", "/v1/cost-centers/"+ccID.String(), "", "")
		move, _ := c.Must(200, "POST", "/v1/cost-centers/"+ccID.String()+":move", fmt.Sprintf(`{"valid_from": "2026-06-01", "profit_center_id": %q, "reason": "r"}`,
			pc[len("/v1/profit-centers/"):]), tag)
		approve := func(ch map[string]any) func(ctx context.Context) error {
			return func(ctx context.Context) error {
				return changes().Approved(ctx, uuid.MustParse(ch["id"].(string)), uuid.MustParse(ch["workflow_request_id"].(string)))
			}
		}
		first, second := serializeOn(t, c.ten, "the profit center", rowLockWaits(t), approve(move), approve(redate))
		if first != nil {
			t.Fatalf("move: got %v, want it committed", first)
		}
		if got := problem.From(second).Code; got != "ledger.profit_center_in_use" {
			t.Errorf("redate after the move: got %v, want ledger.profit_center_in_use", second)
		}
	})
}
