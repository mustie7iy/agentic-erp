package ledger_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
)

// decided applies, rejects or cancels a change a handler just opened, as
// the approvals context does when its request completes.
func decided(ctx context.Context, res any, err error, outcome string) error {
	if err != nil {
		return err
	}
	var ch gen.CenterChange
	switch r := res.(type) {
	case *gen.LockCostCenterOKHeaders:
		ch = r.Response
	default:
		return fmt.Errorf("unexpected response %T", res)
	}
	h := ledger.CenterChanges{}
	switch outcome {
	case "approve":
		return h.Approved(ctx, ch.ID, ch.WorkflowRequestID.Value)
	case "reject":
		return h.Rejected(ctx, ch.ID, ch.WorkflowRequestID.Value, "no")
	}
	return h.Cancelled(ctx, ch.ID, ch.WorkflowRequestID.Value, "withdrawn")
}

// created decides the create change of a center a handler just requested.
func created(ctx context.Context, h *ledger.Endpoints, pending gen.OptUUID, outcome string) error {
	res, err := h.GetCenterChange(ctx, gen.GetCenterChangeParams{ID: pending.Value})
	if err != nil {
		return err
	}
	return decided(ctx, &gen.LockCostCenterOKHeaders{Response: res.(*gen.CenterChangeHeaders).Response}, nil, outcome)
}

// TestCenterDatabaseFailures calls every center handler and callback
// outside a unit of work and with each of its database statements failing
// in turn.
func TestCenterDatabaseFailures(t *testing.T) {
	c := setupCenters(t)
	seg := uuid.MustParse(c.segment(t, "SEG"))
	pcPath := c.profitCenter(t, "PC", "", c.de)
	pc := uuid.MustParse(pcPath[len("/v1/profit-centers/"):])
	cc := c.costCenter(t, "1000", fmt.Sprintf(`, "profit_center_id": %q`, pc))
	c.decide(t, cc["pending_change_id"], "approve")
	ccID := uuid.MustParse(cc["id"].(string))
	h := endpoints()
	change := uuid.MustParse(cc["pending_change_id"].(string))
	for name, call := range centerCalls(h, c, seg, pc, ccID, change) {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
			apitest.EveryFailure(t, c.ten, c.ten.Human, name, call)
		})
	}
}

func centerCalls(h *ledger.Endpoints, c centers, seg, pc, cc, change uuid.UUID) map[string]func(ctx context.Context) error {
	day := date("2026-05-01")
	newCC := func(code string) *gen.CostCenterCreate {
		return &gen.CostCenterCreate{LegalEntityID: c.de, Code: code, Name: "N", Responsible: "R", Category: "ADMIN",
			ProfitCenterID: gen.NewOptUUID(pc), ValidFrom: date("2026-01-01"), Reason: "r"}
	}
	newPC := func(code string) *gen.ProfitCenterCreate {
		return &gen.ProfitCenterCreate{Code: code, Name: "N", Responsible: "R", SegmentID: gen.NewOptUUID(seg), LegalEntityIds: []uuid.UUID{c.de},
			ValidFrom: date("2026-01-01"), Reason: "r"}
	}
	return map[string]func(ctx context.Context) error{
		"create segment": func(ctx context.Context) error {
			_, err := h.CreateSegment(ctx, &gen.SegmentCreate{Code: "NEW", Name: "N"}, gen.CreateSegmentParams{})
			return err
		},
		"update segment": func(ctx context.Context) error {
			_, err := h.UpdateSegment(ctx, &gen.SegmentUpdate{Name: gen.NewOptString("M")}, gen.UpdateSegmentParams{ID: seg, IfMatch: "*"})
			return err
		},
		"get segment": func(ctx context.Context) error {
			_, err := h.GetSegment(ctx, gen.GetSegmentParams{ID: seg})
			return err
		},
		"list segments": func(ctx context.Context) error {
			_, err := h.ListSegments(ctx, gen.ListSegmentsParams{})
			return err
		},
		"create and approve a cost center": func(ctx context.Context) error {
			res, err := h.CreateCostCenter(ctx, newCC("NEW1"), gen.CreateCostCenterParams{})
			if err != nil {
				return err
			}
			return created(ctx, h, res.(*gen.CostCenterHeaders).Response.PendingChangeID, "approve")
		},
		"create and withdraw a cost center": func(ctx context.Context) error {
			res, err := h.CreateCostCenter(ctx, newCC("NEW2"), gen.CreateCostCenterParams{})
			if err != nil {
				return err
			}
			return created(ctx, h, res.(*gen.CostCenterHeaders).Response.PendingChangeID, "cancel")
		},
		"get cost center": func(ctx context.Context) error {
			_, err := h.GetCostCenter(ctx, gen.GetCostCenterParams{ID: cc})
			return err
		},
		"list cost centers": func(ctx context.Context) error {
			_, err := h.ListCostCenters(ctx, gen.ListCostCentersParams{})
			return err
		},
		"update cost center texts": func(ctx context.Context) error {
			_, err := h.UpdateCostCenter(ctx, &gen.CenterTextsUpdate{Name: gen.NewOptString("M")}, gen.UpdateCostCenterParams{ID: cc, IfMatch: "*"})
			return err
		},
		"redate a cost center": func(ctx context.Context) error {
			res, err := h.RedateCostCenter(ctx, &gen.CenterRedate{ValidTo: gen.NewOptNilDate(date("2027-01-01")), Reason: "r"},
				gen.RedateCostCenterParams{ID: cc, IfMatch: "*"})
			return decided(ctx, res, err, "approve")
		},
		"lock a cost center": func(ctx context.Context) error {
			res, err := h.LockCostCenter(ctx, &gen.CostCenterLock{ValidFrom: day, LockPlan: gen.NewOptBool(true), Reason: "r"},
				gen.LockCostCenterParams{ID: cc, IfMatch: "*"})
			return decided(ctx, res, err, "approve")
		},
		"move a cost center, rejected": func(ctx context.Context) error {
			res, err := h.MoveCostCenter(ctx, &gen.CostCenterMove{ValidFrom: day, Category: gen.NewOptString("SALES"), Reason: "r"},
				gen.MoveCostCenterParams{ID: cc, IfMatch: "*"})
			return decided(ctx, res, err, "reject")
		},
		"create and reject a profit center": func(ctx context.Context) error {
			res, err := h.CreateProfitCenter(ctx, newPC("NEWPC"), gen.CreateProfitCenterParams{})
			if err != nil {
				return err
			}
			return created(ctx, h, res.(*gen.ProfitCenterHeaders).Response.PendingChangeID, "reject")
		},
		"get profit center": func(ctx context.Context) error {
			_, err := h.GetProfitCenter(ctx, gen.GetProfitCenterParams{ID: pc})
			return err
		},
		"list profit centers": func(ctx context.Context) error {
			_, err := h.ListProfitCenters(ctx, gen.ListProfitCentersParams{})
			return err
		},
		"update profit center texts": func(ctx context.Context) error {
			_, err := h.UpdateProfitCenter(ctx, &gen.CenterTextsUpdate{Responsible: gen.NewOptString("M")}, gen.UpdateProfitCenterParams{ID: pc, IfMatch: "*"})
			return err
		},
		"redate a profit center": func(ctx context.Context) error {
			res, err := h.RedateProfitCenter(ctx, &gen.CenterRedate{ValidFrom: gen.NewOptDate(date("2025-01-01")), Reason: "r"},
				gen.RedateProfitCenterParams{ID: pc, IfMatch: "*"})
			return decided(ctx, res, err, "approve")
		},
		"lock a profit center": func(ctx context.Context) error {
			res, err := h.LockProfitCenter(ctx, &gen.ProfitCenterLock{ValidFrom: day, Locked: true, Reason: "r"}, gen.LockProfitCenterParams{ID: pc, IfMatch: "*"})
			return decided(ctx, res, err, "approve")
		},
		"move a profit center": func(ctx context.Context) error {
			res, err := h.MoveProfitCenter(ctx, &gen.ProfitCenterMove{SegmentID: gen.NilUUID{Value: seg}, Reason: "r"}, gen.MoveProfitCenterParams{ID: pc, IfMatch: "*"})
			return decided(ctx, res, err, "approve")
		},
		"assign a profit center": func(ctx context.Context) error {
			res, err := h.AssignProfitCenter(ctx, &gen.ProfitCenterAssign{LegalEntityIds: []uuid.UUID{c.us}, Reason: "r"},
				gen.AssignProfitCenterParams{ID: pc, IfMatch: "*"})
			return decided(ctx, res, err, "approve")
		},
		"list center changes": func(ctx context.Context) error {
			_, err := h.ListCenterChanges(ctx, gen.ListCenterChangesParams{})
			return err
		},
		"get center change": func(ctx context.Context) error {
			_, err := h.GetCenterChange(ctx, gen.GetCenterChangeParams{ID: change})
			return err
		},
	}
}
