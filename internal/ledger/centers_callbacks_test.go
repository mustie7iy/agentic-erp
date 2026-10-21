package ledger_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestCenterChangeCallbacks: the approvals callbacks act only on a pending
// change and the request it names, and a change the approval policy would
// not route to a controller is refused.
func TestCenterChangeCallbacks(t *testing.T) {
	c := setupCenters(t)
	pc, _ := c.Must(201, "POST", "/v1/profit-centers", `{"code": "PC", "name": "P", "responsible": "J", "legal_entity_ids": ["`+c.de.String()+`"],
		"valid_from": "2026-01-01", "reason": "r"}`, "")
	ch, _ := c.Must(200, "GET", "/v1/center-changes/"+pc["pending_change_id"].(string), "", "")
	change, request := uuid.MustParse(ch["id"].(string)), uuid.MustParse(ch["workflow_request_id"].(string))
	h := ledger.CenterChanges{}
	for name, fn := range map[string]func(ctx context.Context) error{
		"approve": func(ctx context.Context) error { return h.Approved(ctx, change, request) },
		"reject":  func(ctx context.Context) error { return h.Rejected(ctx, change, request, "c") },
	} {
		if err := fn(context.Background()); err == nil {
			t.Errorf("%s outside a unit of work: got no error, want one", name)
		}
	}
	for name, fn := range map[string]func(ctx context.Context) error{
		"approving on another request":  func(ctx context.Context) error { return h.Approved(ctx, change, uuid.New()) },
		"rejecting on another request":  func(ctx context.Context) error { return h.Rejected(ctx, change, uuid.New(), "c") },
		"cancelling on another request": func(ctx context.Context) error { return h.Cancelled(ctx, change, uuid.New(), "c") },
		"approving an unknown change":   func(ctx context.Context) error { return h.Approved(ctx, uuid.New(), request) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := apitest.InRequest(t, c.ten, c.ten.Human, fn); err == nil {
				t.Error("got no error, want one")
			}
		})
	}
	c.decide(t, change, "approve")
	t.Run("approving an applied change", func(t *testing.T) {
		if err := apitest.InRequest(t, c.ten, c.ten.Human, func(ctx context.Context) error { return h.Approved(ctx, change, request) }); err == nil {
			t.Error("got no error, want one")
		}
	})
	// A policy that needs no approval would change a center unchecked.
	path := "/v1/profit-centers/" + pc["id"].(string)
	err := apitest.InRequest(t, c.ten, c.ten.Human, func(ctx context.Context) error {
		e := endpoints()
		e.Approvals = opener{uuid.New(), false}
		_, err := e.AssignProfitCenter(ctx, &gen.ProfitCenterAssign{LegalEntityIds: []uuid.UUID{c.us}, Reason: "r"},
			gen.AssignProfitCenterParams{ID: uuid.MustParse(pc["id"].(string)), IfMatch: "*"})
		return err
	})
	if err == nil {
		t.Errorf("%s: a change that needs no approval: got no error, want one", path)
	}
	// A change applied after the profit center it names stopped being
	// valid is refused, and the decision with it.
	cc := c.costCenter(t, "1000", `, "profit_center_id": "`+pc["id"].(string)+`"`)
	pgtest.Exec(t, `UPDATE ledger.profit_center_period SET retired_by_change_id = $2, version = version + 1 WHERE profit_center_id = $1`, pc["id"], change)
	pgtest.Exec(t, `INSERT INTO ledger.profit_center_period (tenant_id, id, profit_center_id, valid_from, valid_to, change_id, created_by, updated_by)
		VALUES ($1, gen_random_uuid(), $2, '2026-01-01', '2026-02-01', $3, $4, $4)`, c.ten.ID, pc["id"], change, c.ten.Human.ID)
	page, _ := c.ctl.Must(200, "GET", "/v1/approval-requests?status=pending&object_id="+cc["pending_change_id"].(string), "", "")
	r := page["items"].([]any)[0].(map[string]any)
	c.ctl.Problem(422, "ledger.profit_center_invalid", "POST", "/v1/approval-requests/"+r["id"].(string)+":approve", `{"comment": "ok"}`, `"1"`)
}
