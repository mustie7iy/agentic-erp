package ledger_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestDefaultCentersByTheClock: whether a change leaves a default center
// usable from today is judged by the endpoints' and the approval's clock,
// the one the setup reads the same rule by, not the database's date.
func TestDefaultCentersByTheClock(t *testing.T) {
	a := setupAssigned(t)
	march := at(t, "2026-03-01")
	h := endpoints()
	h.Now = march
	untilMay := gen.NewOptDate(date("2026-05-01"))
	for name, call := range map[string]func(ctx context.Context) error{
		"a cost center's lock": func(ctx context.Context) error {
			_, err := h.LockCostCenter(ctx, &gen.CostCenterLock{ValidFrom: date("2026-04-01"), ValidTo: untilMay, LockActualCost: gen.NewOptBool(true), Reason: "r"},
				gen.LockCostCenterParams{ID: a.cc1000, IfMatch: "*"})
			return err
		},
		"a profit center's lock": func(ctx context.Context) error {
			_, err := h.LockProfitCenter(ctx, &gen.ProfitCenterLock{ValidFrom: date("2026-04-01"), ValidTo: untilMay, Locked: true, Reason: "r"},
				gen.LockProfitCenterParams{ID: a.dummy, IfMatch: "*"})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := apitest.Committed(t, a.ten, a.ten.Human, func(ctx context.Context) error {
				err := call(ctx)
				if p := problem.From(err); p.Code != "ledger.default_center_in_use" {
					t.Errorf("an April lock requested in March: got %v, want ledger.default_center_in_use", err)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("the approval", func(t *testing.T) {
		path := "/v1/cost-centers/" + a.cc1000.String()
		_, tag := a.in(t).Must(200, "GET", path, "", "")
		ch, _ := a.in(t).Must(200, "POST", path+":lock", `{"valid_from": "2026-04-01", "valid_to": "2026-05-01", "lock_actual_cost": true, "reason": "r"}`, tag)
		err := apitest.Committed(t, a.ten, a.ten.Human, func(ctx context.Context) error {
			return ledger.CenterChanges{Now: march}.Approved(ctx, uuid.MustParse(ch["id"].(string)), uuid.MustParse(ch["workflow_request_id"].(string)))
		})
		if p := problem.From(err); p.Code != "ledger.default_center_in_use" {
			t.Errorf("approved in March: got %v, want ledger.default_center_in_use", err)
		}
	})
}
