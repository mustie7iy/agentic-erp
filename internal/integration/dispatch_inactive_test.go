package integration_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestWebhookDispatchLeavesInactiveTenants: the dispatcher sends nothing
// of a suspended tenant, nor counts an attempt, while an active tenant's
// delivery is sent, and sends it once the tenant is active again
// (ADR-0015).
func TestWebhookDispatchLeavesInactiveTenants(t *testing.T) {
	x := newDispatch(t)
	active := webhookClient(t)
	x.hook(x.c, `"ledger.*"`)
	x.hook(active, `"ledger.*"`)
	held, sent := x.event(x.c, uuid.New()), x.event(active, uuid.New())
	x.c.Tenant.SetStatus(t, "suspended")
	x.round(1)
	if row := x.delivery(sent); row.status != "delivered" {
		t.Errorf("the active tenant's delivery: got %s, want delivered", row.status)
	}
	if row := x.delivery(held); row.status != "pending" || row.attempts != 0 {
		t.Errorf("the suspended tenant's delivery: got %s after %d attempts, want pending after 0", row.status, row.attempts)
	}
	pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'active', version = version + 1 WHERE id = $1`, x.c.Tenant.ID)
	x.round(1)
	if row := x.delivery(held); row.status != "delivered" || row.attempts != 1 {
		t.Errorf("the resumed tenant's delivery: got %s after %d attempts, want delivered after 1", row.status, row.attempts)
	}
}
