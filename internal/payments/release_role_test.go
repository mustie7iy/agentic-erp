package payments_test

import (
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestReleaseRole: the release step is decided by holders of the active
// configuration's payments.release_role alone, and a configuration asking
// for more than one releaser (payments.release_approvals) refuses the
// release rather than letting one person stand in for several. Each
// refusal leaves the run proposed, so the holder still releases it.
func TestReleaseRole(t *testing.T) {
	e := setup(t)
	e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier GmbH", "2026-01-01", "", "create")
	e.invoice(t, e.Supplier, "INV-ROLE-1", "100.00", "2026-03-01")
	run := e.run(t, "2026-04-15", "2026-04-15")
	path := "/v1/payment-runs/" + id(run)
	clerk := e.Reader(t, "clerk", e.US, e.DE) // the release permission, not the release role
	_, tag := e.Must(200, "GET", path, "", "")

	t.Run("without the role", func(t *testing.T) {
		e.As(clerk).Problem(403, "payments.release_role_missing", "POST", path+":release", "", tag)
		e.As(clerk).Problem(403, "payments.release_role_missing", "POST", path+":reject", `{"reason": "no"}`, tag)
	})
	t.Run("the role ended", func(t *testing.T) {
		holder := e.Person(t, "ended")
		pgtest.Exec(t, `UPDATE identity.principal_role SET valid_to = current_date, ended_by = $1
			WHERE principal_id = $1 AND role_id = (SELECT id FROM identity.role WHERE tenant_id = $2 AND code = 'Z_TREASURY_MGR')`, holder.ID, e.Tenant.ID)
		e.As(holder).Problem(403, "payments.release_role_missing", "POST", path+":release", "", tag)
	})
	t.Run("the role is not active", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE identity.role SET status = 'inactive', version = version + 1 WHERE tenant_id = $1 AND code = 'Z_TREASURY_MGR'`, e.Tenant.ID)
		defer pgtest.Exec(t, `UPDATE identity.role SET status = 'active', version = version + 1 WHERE tenant_id = $1 AND code = 'Z_TREASURY_MGR'`, e.Tenant.ID)
		e.As(e.Approver).Problem(422, "workflow.unknown_role", "POST", path+":release", "", tag)
	})
	t.Run("more than one releaser asked for", func(t *testing.T) {
		// Validation refuses such a document now; one activated before it
		// is set here with the version's immutability trigger off.
		approvals := func(n int) {
			pgtest.Exec(t, fmt.Sprintf(`DO $$ BEGIN SET LOCAL session_replication_role = replica;
				UPDATE config.config_version v SET document = jsonb_set(document, '{payments,release_approvals}', '%d')
				FROM config.active_version a WHERE a.tenant_id = v.tenant_id AND a.number = v.number AND v.tenant_id = '%s'; END $$`, n, e.Tenant.ID))
		}
		approvals(2)
		defer approvals(1)
		e.As(e.Approver).Problem(422, "payments.release_approvals_unsupported", "POST", path+":release", "", tag)
		if got, _ := e.Must(200, "GET", path, "", ""); got["status"] != "proposed" {
			t.Fatalf("run status after the refusal: got %v, want proposed", got["status"])
		}
	})
	t.Run("the holder releases", func(t *testing.T) {
		released := e.in(t).release(t, run, "")
		if released["decided_by"] != e.Approver.ID.String() {
			t.Errorf("released decided by: got %v, want %v", released["decided_by"], e.Approver.ID)
		}
	})
}
