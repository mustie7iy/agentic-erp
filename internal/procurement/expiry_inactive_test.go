package procurement_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// TestContractExpiryLeavesInactiveTenants: the expirer leaves a suspended
// tenant's contract past its validity active, with no event, while an
// active tenant's is expired, and expires it once the tenant is active
// again (ADR-0015).
func TestContractExpiryLeavesInactiveTenants(t *testing.T) {
	at := time.Date(2026, 3, 1, 9, 30, 0, 0, time.UTC)
	x := &procurement.ContractExpirer{Pool: pgtest.Pool(t), AppRole: "erp_app", WorkerRole: "erp_worker", Settings: configuration.Settings{},
		LegalEntities: tenancy.Directory{}, Partners: partner.Payees{}, Now: func() time.Time { return at }}
	ctx := context.Background()
	var contracts [2]uuid.UUID
	active, suspended := setup(t), setup(t)
	for i, e := range []env{active, suspended} {
		c := e.contract(t, e.Supplier.String(), "")
		e.act(t, "/v1/contracts/"+id(c), "release", "")
		contracts[i] = uuid.MustParse(id(c))
		pgtest.Exec(t, `UPDATE procurement.contract SET valid_from = '2025-01-01', valid_to = '2026-02-28', version = version + 1 WHERE id = $1`, contracts[i])
	}
	status := func(c uuid.UUID) string {
		t.Helper()
		var s string
		pgtest.Scan(t, `SELECT status || '/' || (SELECT count(*) FROM audit.audit_event a WHERE a.object_id = c.id AND a.action = 'procurement.contract.expire') FROM procurement.contract c WHERE id = $1`,
			[]any{c}, &s)
		return s
	}
	left := status(contracts[1])
	suspended.Tenant.SetStatus(t, "suspended")
	if _, err := x.RunOnce(ctx); err != nil {
		t.Fatalf("round while suspended: %v", err)
	}
	if got := status(contracts[0]); !strings.HasPrefix(got, "expired/") {
		t.Errorf("the active tenant's contract: got %s, want expired", got)
	}
	if got := status(contracts[1]); got != left {
		t.Errorf("the suspended tenant's contract: got %s, want it as it was, %s", got, left)
	}
	pgtest.Exec(t, `UPDATE tenancy.tenant SET status = 'active', version = version + 1 WHERE id = $1`, suspended.Tenant.ID)
	if _, err := x.RunOnce(ctx); err != nil {
		t.Fatalf("round once active again: %v", err)
	}
	if got := status(contracts[1]); !strings.HasPrefix(got, "expired/") {
		t.Errorf("the resumed tenant's contract: got %s, want expired", got)
	}
}
