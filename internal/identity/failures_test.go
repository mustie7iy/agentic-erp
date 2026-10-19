package identity_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/agents"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// fixture holds ids of existing resources for direct handler calls.
type fixture struct {
	ten                                       pgtest.Tenant
	role, permission, assignment, access, sod uuid.UUID
	legalEntity, agent                        uuid.UUID
}

// endpointCalls calls every identity handler once, with arguments that
// succeed against f (zero ids when f is nil).
func endpointCalls(f *fixture) map[string]func(ctx context.Context) error {
	if f == nil {
		f = &fixture{}
	}
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	e := &identity.Endpoints{Pages: pages, LegalEntities: tenancy.Directory{}, Certifications: agents.Certifications{Agents: identity.AutonomousAgents}}
	soon := time.Now().UTC().AddDate(0, 0, 20).Truncate(24 * time.Hour)
	profile := gen.NewOptAgentProfile(gen.AgentProfile{AutonomyLevel: 2, AgentType: gen.NewOptString("analytics"), Model: "erp-analytics", Description: "d2",
		MonthlyBudget: gen.NewOptMoney(gen.Money{Amount: "10.00", Currency: "EUR"})})
	return map[string]func(ctx context.Context) error{
		"create principal": func(ctx context.Context) error {
			_, err := e.CreatePrincipal(ctx, &gen.PrincipalCreate{Kind: "agent", DisplayName: "A", AgentProfile: profile}, gen.CreatePrincipalParams{})
			return err
		},
		"get principal": func(ctx context.Context) error {
			_, err := e.GetPrincipal(ctx, gen.GetPrincipalParams{ID: f.agent})
			return err
		},
		"list principals": func(ctx context.Context) error {
			_, err := e.ListPrincipals(ctx, gen.ListPrincipalsParams{})
			return err
		},
		"update principal": func(ctx context.Context) error {
			_, err := e.UpdatePrincipal(ctx, &gen.PrincipalUpdate{DisplayName: gen.NewOptString("B"), AgentProfile: profile},
				gen.UpdatePrincipalParams{ID: f.agent, IfMatch: "*"})
			return err
		},
		"block principal": func(ctx context.Context) error {
			_, err := e.BlockPrincipal(ctx, &gen.ReasonInput{Reason: "r"}, gen.BlockPrincipalParams{ID: f.agent, IfMatch: "*"})
			return err
		},
		"create role": func(ctx context.Context) error {
			_, err := e.CreateRole(ctx, &gen.RoleCreate{Code: "NEW", Name: "N"}, gen.CreateRoleParams{})
			return err
		},
		"get role": func(ctx context.Context) error {
			_, err := e.GetRole(ctx, gen.GetRoleParams{ID: f.role})
			return err
		},
		"list roles": func(ctx context.Context) error {
			_, err := e.ListRoles(ctx, gen.ListRolesParams{})
			return err
		},
		"update role": func(ctx context.Context) error {
			_, err := e.UpdateRole(ctx, &gen.RoleUpdate{Name: gen.NewOptString("N")}, gen.UpdateRoleParams{ID: f.role, IfMatch: "*"})
			return err
		},
		"grant role permission": func(ctx context.Context) error {
			_, err := e.GrantRolePermission(ctx, &gen.RolePermissionCreate{PermissionCode: "payables.invoice.hold"}, gen.GrantRolePermissionParams{ID: f.role})
			return err
		},
		"get role permission": func(ctx context.Context) error {
			_, err := e.GetRolePermission(ctx, gen.GetRolePermissionParams{ID: f.role, PermissionID: f.permission})
			return err
		},
		"list role permissions": func(ctx context.Context) error {
			_, err := e.ListRolePermissions(ctx, gen.ListRolePermissionsParams{ID: f.role})
			return err
		},
		"end role permission": func(ctx context.Context) error {
			_, err := e.EndRolePermission(ctx, &gen.EndInput{ValidTo: soon}, gen.EndRolePermissionParams{ID: f.role, PermissionID: f.permission, IfMatch: "*"})
			return err
		},
		"create role assignment": func(ctx context.Context) error {
			_, err := e.CreateRoleAssignment(ctx, &gen.RoleAssignmentCreate{PrincipalID: f.ten.Service.ID, RoleID: f.role}, gen.CreateRoleAssignmentParams{})
			return err
		},
		"get role assignment": func(ctx context.Context) error {
			_, err := e.GetRoleAssignment(ctx, gen.GetRoleAssignmentParams{ID: f.assignment})
			return err
		},
		"list role assignments": func(ctx context.Context) error {
			_, err := e.ListRoleAssignments(ctx, gen.ListRoleAssignmentsParams{})
			return err
		},
		"end role assignment": func(ctx context.Context) error {
			_, err := e.EndRoleAssignment(ctx, &gen.EndInput{ValidTo: soon}, gen.EndRoleAssignmentParams{ID: f.assignment, IfMatch: "*"})
			return err
		},
		"create company access": func(ctx context.Context) error {
			_, err := e.CreateCompanyAccess(ctx, &gen.CompanyAccessCreate{PrincipalID: f.ten.Service.ID, LegalEntityID: f.legalEntity}, gen.CreateCompanyAccessParams{})
			return err
		},
		"get company access": func(ctx context.Context) error {
			_, err := e.GetCompanyAccess(ctx, gen.GetCompanyAccessParams{ID: f.access})
			return err
		},
		"list company access": func(ctx context.Context) error {
			_, err := e.ListCompanyAccess(ctx, gen.ListCompanyAccessParams{})
			return err
		},
		"end company access": func(ctx context.Context) error {
			_, err := e.EndCompanyAccess(ctx, &gen.EndInput{ValidTo: soon}, gen.EndCompanyAccessParams{ID: f.access, IfMatch: "*"})
			return err
		},
		"create sod rule": func(ctx context.Context) error {
			_, err := e.CreateSodRule(ctx, &gen.SodRuleCreate{PermissionA: "ledger.entry.post", PermissionB: "ledger.period.manage", Description: "d"}, gen.CreateSodRuleParams{})
			return err
		},
		"get sod rule": func(ctx context.Context) error {
			_, err := e.GetSodRule(ctx, gen.GetSodRuleParams{ID: f.sod})
			return err
		},
		"list sod rules": func(ctx context.Context) error {
			_, err := e.ListSodRules(ctx, gen.ListSodRulesParams{})
			return err
		},
		"end sod rule": func(ctx context.Context) error {
			_, err := e.EndSodRule(ctx, &gen.EndInput{ValidTo: soon}, gen.EndSodRuleParams{ID: f.sod, IfMatch: "*"})
			return err
		},
	}
}

func TestDatabaseFailures(t *testing.T) {
	c, ten := admin(t)
	f := &fixture{ten: ten}
	pgtest.Route(t, ten.ID, "erp-analytics", pgtest.Certify(t, "analytics", 2)) // the profile the calls set is certified
	agent, _ := c.Must(201, "POST", "/v1/principals", `{"kind": "agent", "display_name": "A", "agent_profile": {"autonomy_level": 0, "model": "erp-default", "description": "d"}}`, "")
	f.agent = uuid.MustParse(id(agent))
	f.role = uuid.MustParse(role(t, c, "R"))
	p, _ := c.Must(201, "POST", "/v1/roles/"+f.role.String()+"/permissions", `{"permission_code": "payables.invoice.read"}`, "")
	f.permission = uuid.MustParse(id(p))
	_, a, _ := assign(c, ten.Agent.ID, f.role.String(), "")
	f.assignment = uuid.MustParse(id(a))
	f.legalEntity = uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, 'DE01', 'Muster GmbH', 'DE', 'EUR', gen_random_uuid(), 'active', $3, $3)`, ten.ID, f.legalEntity, ten.Human.ID)
	g, _ := c.Must(201, "POST", "/v1/company-access", `{"principal_id": "`+ten.Agent.ID.String()+`", "legal_entity_id": "`+f.legalEntity.String()+`"}`, "")
	f.access = uuid.MustParse(id(g))
	r, _ := c.Must(201, "POST", "/v1/sod-rules", `{"permission_a": "partner.bank.change", "permission_b": "payments.run.propose", "description": "d"}`, "")
	f.sod = uuid.MustParse(id(r))
	for name, call := range endpointCalls(f) {
		t.Run(name, func(t *testing.T) {
			apitest.EveryFailure(t, ten, ten.Human, name, call)
		})
	}
	// Page sizes are checked by the handlers too, for callers other than the generated router.
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	e := &identity.Endpoints{Pages: pages}
	zero := gen.NewOptInt32(0)
	for name, call := range map[string]func(ctx context.Context) error{
		"principals": func(ctx context.Context) error {
			_, err := e.ListPrincipals(ctx, gen.ListPrincipalsParams{PageSize: zero})
			return err
		},
		"roles": func(ctx context.Context) error {
			_, err := e.ListRoles(ctx, gen.ListRolesParams{PageSize: zero})
			return err
		},
		"role permissions": func(ctx context.Context) error {
			_, err := e.ListRolePermissions(ctx, gen.ListRolePermissionsParams{ID: f.role, PageSize: zero})
			return err
		},
		"assignments": func(ctx context.Context) error {
			_, err := e.ListRoleAssignments(ctx, gen.ListRoleAssignmentsParams{PageSize: zero})
			return err
		},
		"company access": func(ctx context.Context) error {
			_, err := e.ListCompanyAccess(ctx, gen.ListCompanyAccessParams{PageSize: zero})
			return err
		},
		"sod rules": func(ctx context.Context) error {
			_, err := e.ListSodRules(ctx, gen.ListSodRulesParams{PageSize: zero})
			return err
		},
	} {
		t.Run("page size 0 "+name, func(t *testing.T) {
			if err := apitest.InRequest(t, ten, ten.Human, call); err == nil {
				t.Error("got nil error, want page_size 0 refused")
			}
		})
	}
}
