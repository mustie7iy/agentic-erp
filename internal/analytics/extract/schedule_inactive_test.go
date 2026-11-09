package extract_test

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestSchedulerLeavesInactiveTenants: a suspended or offboarded tenant
// marked changed is not claimed, its queue row left as it was, and is
// extracted once it is active again (ADR-0015).
func TestSchedulerLeavesInactiveTenants(t *testing.T) {
	for _, status := range []string{"suspended", "offboarded"} {
		t.Run(status, func(t *testing.T) {
			c := newScheduled(t)
			c.post(t, "2026-03-02", "1.00")
			c.mark(t, "ledger")
			c.Tenant.SetStatus(t, status)
			c.runOnce(t)
			if started, done := c.due(t); started != nil || done != nil {
				t.Errorf("queue while %s: got started %v done %v, want it unclaimed", status, started, done)
			}
			pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'active', version = version + 1 WHERE id = $1`, c.Tenant.ID)
			c.runOnce(t)
			if started, done := c.due(t); started == nil || done == nil {
				t.Errorf("queue once active: got started %v done %v, want the run recorded", started, done)
			}
		})
	}
}
