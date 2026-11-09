package cash_test

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestCashExportLeavesInactiveTenants: the exporter sends nothing of a
// suspended tenant, nor counts an attempt, and sends its exports once the
// tenant is active again (ADR-0015).
func TestCashExportLeavesInactiveTenants(t *testing.T) {
	e := setupExport(t)
	s := e.take(t, "Board meeting April")
	e.Tenant.SetStatus(t, "suspended")
	pending := func() string {
		t.Helper()
		var got string
		pgtest.Scan(t, `SELECT count(*) FILTER (WHERE status = 'pending' AND attempts = 0) || '/' || count(*) FROM cash.export WHERE snapshot_id = $1`,
			[]any{s["id"]}, &got)
		return got
	}
	e.round(t, 0)
	want(t, "the suspended tenant's exports pending without an attempt", pending(), "2/2")
	pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'active', version = version + 1 WHERE id = $1`, e.Tenant.ID)
	e.round(t, 2)
	want(t, "the resumed tenant's exports pending", pending(), "0/2")
}
