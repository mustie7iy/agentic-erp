package fixture

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Centers are the cost and profit centers of company 2000 CostCenters
// sets up (ADR-0050), with the document it applied.
type Centers struct {
	Admin  uuid.UUID // cost center 1000 (ADMIN) with profit center PC1, the default of account 520000
	Sales  uuid.UUID // cost center 2000 (SALES), without a profit center
	Locked uuid.UUID // cost center 3000 (ADMIN), locked for actual costs in 2026
	Profit uuid.UUID // profit center PC1, assigned to company 2000
	// Doc is the document applied, cost center accounting on: a later
	// application keeps it on (it is never turned off).
	Doc *config.Config
}

// Controller creates the controller role (approvals.DefaultCenterChangeRole,
// the fixed role of center changes, period unlocks and GR/IR clearings)
// if the tenant has none and gives it to Approver.
func (e Env) Controller(tb testing.TB) {
	tb.Helper()
	pgtest.Exec(tb, `INSERT INTO identity.role (tenant_id, id, code, name, status, created_by, updated_by)
		SELECT $1, gen_random_uuid(), $2, $2, 'active', $3, $3 WHERE NOT EXISTS (SELECT FROM identity.role WHERE tenant_id = $1 AND code = $2)`,
		e.Tenant.ID, approvals.DefaultCenterChangeRole, e.Tenant.Human.ID)
	pgtest.Exec(tb, `INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by)
		SELECT $1, gen_random_uuid(), $2, r.id, current_date - 1, $2 FROM identity.role r WHERE r.tenant_id = $1 AND r.code = $3
			AND NOT EXISTS (SELECT FROM identity.principal_role p WHERE p.tenant_id = $1 AND p.principal_id = $2 AND p.role_id = r.id)`,
		e.Tenant.ID, e.Approver.ID, approvals.DefaultCenterChangeRole)
}

// CostCenters gives company 2000 the centers, approved by the controller
// (approvals.DefaultCenterChangeRole, which Approver is given), and turns
// its cost center accounting on from 2026 with Admin the default of
// account 520000, as activated configurations would. extend, if not nil,
// changes the document first (GRIRAccounts). The tenant may not have
// posted in 2026 yet. Tenant.Human requests the centers, so it is given
// ledger.cost_center.change and .read.
func (e Env) CostCenters(tb testing.TB, extend func(*config.Config)) Centers {
	tb.Helper()
	for _, p := range []string{"ledger.cost_center.change", "ledger.cost_center.read"} {
		pgtest.Exec(tb, `INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, created_by)
			SELECT tenant_id, gen_random_uuid(), id, $2, current_date - 1, $3 FROM identity.role WHERE tenant_id = $1 AND code = 'all'`,
			e.Tenant.ID, p, e.Tenant.Human.ID)
	}
	e.Controller(tb)
	doc := Document(tb)
	if extend != nil {
		extend(doc)
	}
	doc.Controlling.CostCenterCategories = []config.CostCenterCategory{{Code: "ADMIN", Name: "Administration"},
		{Code: "SALES", Name: "Sales", RevenueAllowed: true}}
	e.applyControlling(tb, doc)
	var c Centers
	pc, _ := e.Must(201, "POST", "/v1/profit-centers", fmt.Sprintf(`{"code": "PC1", "name": "PC 1", "responsible": "Jane Doe",
		"legal_entity_ids": [%q], "valid_from": "2026-01-01", "reason": "r"}`, e.DE), "")
	e.Approve(tb, pc["pending_change_id"].(string))
	c.Profit = uuid.MustParse(pc["id"].(string))
	c.Admin = e.costCenter(tb, "1000", "ADMIN", fmt.Sprintf(`, "profit_center_id": %q`, c.Profit))
	c.Sales, c.Locked = e.costCenter(tb, "2000", "SALES", ""), e.costCenter(tb, "3000", "ADMIN", "")
	path := "/v1/cost-centers/" + c.Locked.String()
	_, tag := e.Must(200, "GET", path, "", "")
	lock, _ := e.Must(200, "POST", path+":lock", `{"valid_from": "2026-01-01", "valid_to": "2027-01-01", "lock_actual_cost": true, "reason": "r"}`, tag)
	e.Approve(tb, lock["id"].(string))
	co := doc.Company("2000")
	co.CostCenterAccounting = true
	co.DefaultAssignments = append(co.DefaultAssignments, config.DefaultAssignment{Account: "520000", CostCenter: "1000"})
	e.applyControlling(tb, doc)
	c.Doc = doc
	return c
}

// costCenter creates and approves a cost center of company 2000 valid from
// 2026 and returns its id.
func (e Env) costCenter(tb testing.TB, code, category, extra string) uuid.UUID {
	tb.Helper()
	cc, _ := e.Must(201, "POST", "/v1/cost-centers", fmt.Sprintf(`{"legal_entity_id": %q, "code": %q, "name": "CC %s", "responsible": "J",
		"category": %q, "valid_from": "2026-01-01"%s, "reason": "r"}`, e.DE, code, code, category, extra), "")
	e.Approve(tb, cc["pending_change_id"].(string))
	return uuid.MustParse(cc["id"].(string))
}

// applyControlling applies doc to the ledger as on 2026-01-01, so the
// switches apply from 2026, and reopens 2026's periods, which turning a
// switch on leaves as they were.
func (e Env) applyControlling(tb testing.TB, doc *config.Config) {
	tb.Helper()
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := apitest.Committed(tb, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		return ledger.Setup{LegalEntities: tenancy.Directory{}, Now: func() time.Time { return day }}.Apply(ctx, doc)
	}); err != nil {
		tb.Fatal(err)
	}
	pgtest.Exec(tb, `UPDATE ledger.period_status SET status = 'open', version = version + 1 WHERE tenant_id = $1 AND period <= 12 AND status <> 'open'`,
		e.Tenant.ID)
}
