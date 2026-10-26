package identity

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Endpoints implements the identity part of the generated API.
type Endpoints struct {
	Pages         *pagination.Codec
	LegalEntities LegalEntities
	// Certifications bounds agents' levels by their type's certification
	// (agents.Certifications, ADR-0039).
	Certifications Certifications
	// Sessions are people's sessions, which endCurrentSession ends.
	Sessions *Sessions
}

// EndCurrentSession implements POST /v1/sessions/current:end (ADR-0052 G9).
func (e *Endpoints) EndCurrentSession(ctx context.Context, _ gen.EndCurrentSessionParams) (gen.EndCurrentSessionRes, error) {
	at, err := e.Sessions.End(ctx)
	if err != nil {
		return nil, err
	}
	out := gen.SessionEnd{Ended: at != nil}
	if at != nil {
		out.EndedAt.SetTo(*at)
	}
	return &gen.SessionEndHeaders{Response: out}, nil
}

func profileInput(p gen.AgentProfile) *ProfileInput {
	in := &ProfileInput{AutonomyLevel: int16(p.AutonomyLevel), AgentType: apitypes.Ptr[string](p.AgentType), Model: p.Model, Description: p.Description}
	if p.MonthlyBudget.Set {
		in.Budget = &Budget{Amount: p.MonthlyBudget.Value.Amount, Currency: p.MonthlyBudget.Value.Currency}
	}
	return in
}

func principalToAPI(p Principal) gen.Principal {
	out := gen.Principal{
		ID: p.ID, Version: p.Version, CreatedAt: p.CreatedAt, CreatedBy: p.CreatedBy, UpdatedAt: p.UpdatedAt, UpdatedBy: gen.NewOptUUID(p.UpdatedBy),
		Kind: p.Kind, ExternalSubject: apitypes.OptString(p.ExternalSubject),
		ExternalIssuer: apitypes.OptString(p.ExternalIssuer), DisplayName: p.DisplayName, Email: apitypes.OptString(p.Email),
		AllCompanies: p.AllCompanies, Status: p.Status,
	}
	if p.Profile != nil {
		ap := gen.AgentProfile{AutonomyLevel: int32(p.Profile.AutonomyLevel), AgentType: apitypes.OptString(p.Profile.AgentType), Model: p.Profile.Model,
			Description: p.Profile.Description}
		if p.BudgetDecimal != nil {
			ap.MonthlyBudget.SetTo(gen.Money{Amount: p.BudgetDecimal.Amount, Currency: p.BudgetDecimal.Currency})
		}
		out.AgentProfile.SetTo(ap)
	}
	return out
}

func roleToAPI(r Role) gen.Role {
	return gen.Role{
		ID: r.ID, Version: r.Version, CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy, UpdatedAt: r.UpdatedAt, UpdatedBy: gen.NewOptUUID(r.UpdatedBy),
		Code: r.Code, Name: r.Name, Description: apitypes.OptString(r.Description), Status: r.Status, PermissionCodes: r.Permissions,
	}
}

// grantFields are the effective-dated fields in their generated form.
type grantFields struct {
	validTo gen.OptDate
	endedAt gen.OptDateTime
	endedBy gen.OptUUID
}

func toGrantFields(g Grant) grantFields {
	var f grantFields
	if g.ValidTo != nil {
		f.validTo.SetTo(*g.ValidTo)
	}
	if g.EndedAt != nil {
		f.endedAt.SetTo(*g.EndedAt)
	}
	if g.EndedBy != nil {
		f.endedBy.SetTo(*g.EndedBy)
	}
	return f
}

func rolePermissionToAPI(g RolePermission) gen.RolePermission {
	f := toGrantFields(g.Grant)
	return gen.RolePermission{ID: g.ID, ValidFrom: g.ValidFrom, ValidTo: f.validTo, EndedAt: f.endedAt, EndedBy: f.endedBy,
		CreatedAt: g.CreatedAt, CreatedBy: g.CreatedBy, RoleID: g.RoleID, PermissionCode: g.PermissionCode}
}

func assignmentToAPI(g RoleAssignment) gen.RoleAssignment {
	f := toGrantFields(g.Grant)
	return gen.RoleAssignment{ID: g.ID, ValidFrom: g.ValidFrom, ValidTo: f.validTo, EndedAt: f.endedAt, EndedBy: f.endedBy,
		CreatedAt: g.CreatedAt, CreatedBy: g.CreatedBy, PrincipalID: g.PrincipalID, RoleID: g.RoleID}
}

func accessToAPI(g CompanyAccess) gen.CompanyAccess {
	f := toGrantFields(g.Grant)
	return gen.CompanyAccess{ID: g.ID, ValidFrom: g.ValidFrom, ValidTo: f.validTo, EndedAt: f.endedAt, EndedBy: f.endedBy,
		CreatedAt: g.CreatedAt, CreatedBy: g.CreatedBy, PrincipalID: g.PrincipalID, LegalEntityID: g.LegalEntityID}
}

func sodToAPI(r SodRule) gen.SodRule {
	f := toGrantFields(r.Grant)
	return gen.SodRule{ID: r.ID, ValidFrom: r.ValidFrom, ValidTo: f.validTo, EndedAt: f.endedAt, EndedBy: f.endedBy,
		CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy, PermissionA: r.PermissionA, PermissionB: r.PermissionB, Description: r.Description}
}

func matchGrant[G, A any](header string, toAPI func(G) A) func(G) error {
	return func(g G) error { return etag.Match(header, etag.Of(new(toAPI(g)))) }
}

// list runs a keyset-paged query and converts its rows.
func list[R, A any](ctx context.Context, tx pgx.Tx, ks *pagination.Keyset, query string, args []any, scan func(pgx.Row) (R, error),
	key func(R) (string, uuid.UUID), toAPI func(R) A) ([]A, gen.OptString, error) {
	q, args := ks.SQL(query, args)
	rows, _ := tx.Query(ctx, q, args...) // CollectRows returns the query's error
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (R, error) { return scan(r) })
	if err != nil {
		return nil, gen.OptString{}, fmt.Errorf("identity: list %T rows: %w", *new(R), err)
	}
	out, next := apitypes.PageOf(ks, items, key, toAPI)
	return out, next, nil
}

func dateKey(g Grant) (string, uuid.UUID) { return date(g.ValidFrom), g.ID }

// validOn filters grants valid on a date (or all of them).
const validOn = `($%d::date IS NULL OR (valid_from <= $%[1]d AND (valid_to IS NULL OR valid_to > $%[1]d)))`

// --- principals

// CreatePrincipal implements POST /v1/principals.
func (e *Endpoints) CreatePrincipal(ctx context.Context, req *gen.PrincipalCreate, _ gen.CreatePrincipalParams) (gen.CreatePrincipalRes, error) {
	in := NewPrincipal{Kind: req.Kind, ExternalSubject: apitypes.Ptr[string](req.ExternalSubject), DisplayName: req.DisplayName,
		Email: apitypes.Ptr[string](req.Email), AllCompanies: req.AllCompanies.Value}
	if req.AgentProfile.Set {
		in.Profile = profileInput(req.AgentProfile.Value)
	}
	p, err := CreatePrincipal(ctx, e.Certifications, in)
	if err != nil {
		return nil, err
	}
	return &gen.PrincipalHeaders{ETag: gen.NewOptString(etag.Format(p.Version)), Location: gen.NewOptString("/v1/principals/" + p.ID.String()),
		Response: principalToAPI(p)}, nil
}

// GetPrincipal implements GET /v1/principals/{id}.
func (e *Endpoints) GetPrincipal(ctx context.Context, params gen.GetPrincipalParams) (gen.GetPrincipalRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	p, err := getPrincipal(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	return &gen.GetPrincipalOKHeaders{ETag: gen.NewOptString(etag.Format(p.Version)), Response: principalToAPI(p)}, nil
}

var principalSort = pagination.Sort{Default: "display_name", Fields: []pagination.Field{
	{Name: "display_name", Column: "p.display_name", Type: "text"}, {Name: "created_at", Column: "p.created_at", Type: "timestamptz"},
}}

// ListPrincipals implements GET /v1/principals.
func (e *Endpoints) ListPrincipals(ctx context.Context, params gen.ListPrincipalsParams) (gen.ListPrincipalsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	ks, err := e.Pages.Keyset(principalSort, "listPrincipals|"+params.Kind.Value+"|"+params.Status.Value,
		apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	byName := ks.Field() == "display_name"
	items, next, err := list(ctx, tx, ks, principalSelect+` WHERE ($1 = '' OR p.kind = $1) AND ($2 = '' OR p.status = $2)`,
		[]any{params.Kind.Value, params.Status.Value}, scanPrincipal,
		func(p Principal) (string, uuid.UUID) {
			if byName {
				return p.DisplayName, p.ID
			}
			return p.CreatedAt.UTC().Format(time.RFC3339Nano), p.ID
		}, principalToAPI)
	if err != nil {
		return nil, err
	}
	return &gen.PrincipalPage{Items: items, NextPageToken: next}, nil
}

// UpdatePrincipal implements PATCH /v1/principals/{id}.
func (e *Endpoints) UpdatePrincipal(ctx context.Context, req *gen.PrincipalUpdate, params gen.UpdatePrincipalParams) (gen.UpdatePrincipalRes, error) {
	ch := PrincipalChange{DisplayName: apitypes.Ptr[string](req.DisplayName), Email: apitypes.OptNilString(req.Email)}
	if req.AllCompanies.Set {
		ch.AllCompanies = &req.AllCompanies.Value
	}
	if req.AgentProfile.Set {
		ch.Profile = profileInput(req.AgentProfile.Value)
	}
	p, err := UpdatePrincipal(ctx, e.Certifications, params.ID, etag.IfMatch(params.IfMatch), ch)
	if err != nil {
		return nil, err
	}
	return &gen.UpdatePrincipalOKHeaders{ETag: gen.NewOptString(etag.Format(p.Version)), Response: principalToAPI(p)}, nil
}

// BlockPrincipal implements POST /v1/principals/{id}:block.
func (e *Endpoints) BlockPrincipal(ctx context.Context, req *gen.ReasonInput, params gen.BlockPrincipalParams) (gen.BlockPrincipalRes, error) {
	p, err := BlockPrincipal(ctx, params.ID, etag.IfMatch(params.IfMatch), req.Reason)
	if err != nil {
		return nil, err
	}
	return &gen.UpdatePrincipalOKHeaders{ETag: gen.NewOptString(etag.Format(p.Version)), Response: principalToAPI(p)}, nil
}

// --- roles

// CreateRole implements POST /v1/roles.
func (e *Endpoints) CreateRole(ctx context.Context, req *gen.RoleCreate, _ gen.CreateRoleParams) (gen.CreateRoleRes, error) {
	r, err := CreateRole(ctx, req.Code, req.Name, apitypes.Ptr[string](req.Description))
	if err != nil {
		return nil, err
	}
	return &gen.RoleHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Location: gen.NewOptString("/v1/roles/" + r.ID.String()),
		Response: roleToAPI(r)}, nil
}

// GetRole implements GET /v1/roles/{id}.
func (e *Endpoints) GetRole(ctx context.Context, params gen.GetRoleParams) (gen.GetRoleRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	r, err := getRole(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	return &gen.GetRoleOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: roleToAPI(r)}, nil
}

var roleSort = pagination.Sort{Default: "code", Fields: []pagination.Field{
	{Name: "code", Column: "r.code", Type: "text"}, {Name: "name", Column: "r.name", Type: "text"},
}}

// ListRoles implements GET /v1/roles.
func (e *Endpoints) ListRoles(ctx context.Context, params gen.ListRolesParams) (gen.ListRolesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	ks, err := e.Pages.Keyset(roleSort, "listRoles|"+params.Status.Value, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	byName := ks.Field() == "name"
	items, next, err := list(ctx, tx, ks, roleSelect+` WHERE ($1 = '' OR r.status = $1)`, []any{params.Status.Value}, scanRole,
		func(r Role) (string, uuid.UUID) {
			if byName {
				return r.Name, r.ID
			}
			return r.Code, r.ID
		}, roleToAPI)
	if err != nil {
		return nil, err
	}
	return &gen.RolePage{Items: items, NextPageToken: next}, nil
}

// UpdateRole implements PATCH /v1/roles/{id}.
func (e *Endpoints) UpdateRole(ctx context.Context, req *gen.RoleUpdate, params gen.UpdateRoleParams) (gen.UpdateRoleRes, error) {
	r, err := UpdateRole(ctx, params.ID, etag.IfMatch(params.IfMatch), apitypes.Ptr[string](req.Name), apitypes.OptNilString(req.Description))
	if err != nil {
		return nil, err
	}
	return &gen.UpdateRoleOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: roleToAPI(r)}, nil
}

// --- role permissions

// GrantRolePermission implements POST /v1/roles/{id}/permissions.
func (e *Endpoints) GrantRolePermission(ctx context.Context, req *gen.RolePermissionCreate, params gen.GrantRolePermissionParams) (gen.GrantRolePermissionRes, error) {
	g, err := GrantPermission(ctx, params.ID, req.PermissionCode, apitypes.Ptr[time.Time](req.ValidFrom), apitypes.Ptr[time.Time](req.ValidTo))
	if err != nil {
		return nil, err
	}
	out := rolePermissionToAPI(g)
	return &gen.RolePermissionHeaders{ETag: apitypes.ETagOf(out), Location: gen.NewOptString("/v1/roles/" + g.RoleID.String() + "/permissions/" + g.ID.String()),
		Response: out}, nil
}

// GetRolePermission implements GET /v1/roles/{id}/permissions/{permission_id}.
func (e *Endpoints) GetRolePermission(ctx context.Context, params gen.GetRolePermissionParams) (gen.GetRolePermissionRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	g, err := getRolePermission(ctx, tx, params.ID, params.PermissionID, false)
	if err != nil {
		return nil, err
	}
	out := rolePermissionToAPI(g)
	return &gen.GetRolePermissionOKHeaders{ETag: apitypes.ETagOf(out), Response: out}, nil
}

var validFromSort = pagination.Sort{Default: "valid_from", Fields: []pagination.Field{{Name: "valid_from", Column: "valid_from", Type: "date"}}}

// ListRolePermissions implements GET /v1/roles/{id}/permissions.
func (e *Endpoints) ListRolePermissions(ctx context.Context, params gen.ListRolePermissionsParams) (gen.ListRolePermissionsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := getRole(ctx, tx, params.ID, false); err != nil {
		return nil, err
	}
	on := apitypes.Ptr[time.Time](params.ValidOn)
	ks, err := e.Pages.Keyset(validFromSort, "listRolePermissions|"+params.ID.String()+"|"+dateParam(on),
		apitypes.Page(gen.OptString{}, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	items, next, err := list(ctx, tx, ks, `SELECT `+rolePermissionColumns+` FROM identity.role_permission WHERE role_id = $1 AND `+fmt.Sprintf(validOn, 2),
		[]any{params.ID, on}, scanRolePermission, func(g RolePermission) (string, uuid.UUID) { return dateKey(g.Grant) }, rolePermissionToAPI)
	if err != nil {
		return nil, err
	}
	return &gen.RolePermissionPage{Items: items, NextPageToken: next}, nil
}

// EndRolePermission implements POST /v1/roles/{id}/permissions/{permission_id}:end.
func (e *Endpoints) EndRolePermission(ctx context.Context, req *gen.EndInput, params gen.EndRolePermissionParams) (gen.EndRolePermissionRes, error) {
	g, err := EndPermission(ctx, params.ID, params.PermissionID, matchGrant(params.IfMatch, rolePermissionToAPI), req.ValidTo)
	if err != nil {
		return nil, err
	}
	out := rolePermissionToAPI(g)
	return &gen.EndRolePermissionOKHeaders{ETag: apitypes.ETagOf(out), Response: out}, nil
}

// --- role assignments

// CreateRoleAssignment implements POST /v1/role-assignments.
func (e *Endpoints) CreateRoleAssignment(ctx context.Context, req *gen.RoleAssignmentCreate, _ gen.CreateRoleAssignmentParams) (gen.CreateRoleAssignmentRes, error) {
	g, err := AssignRole(ctx, req.PrincipalID, req.RoleID, apitypes.Ptr[time.Time](req.ValidFrom), apitypes.Ptr[time.Time](req.ValidTo))
	if err != nil {
		return nil, err
	}
	out := assignmentToAPI(g)
	return &gen.RoleAssignmentHeaders{ETag: apitypes.ETagOf(out), Location: gen.NewOptString("/v1/role-assignments/" + g.ID.String()), Response: out}, nil
}

// GetRoleAssignment implements GET /v1/role-assignments/{id}.
func (e *Endpoints) GetRoleAssignment(ctx context.Context, params gen.GetRoleAssignmentParams) (gen.GetRoleAssignmentRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	g, err := getAssignment(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	out := assignmentToAPI(g)
	return &gen.GetRoleAssignmentOKHeaders{ETag: apitypes.ETagOf(out), Response: out}, nil
}

// ListRoleAssignments implements GET /v1/role-assignments.
func (e *Endpoints) ListRoleAssignments(ctx context.Context, params gen.ListRoleAssignmentsParams) (gen.ListRoleAssignmentsRes, error) {
	items, next, err := listGrants(ctx, e, grantList{name: "listRoleAssignments", columns: assignmentColumns, table: "identity.principal_role", other: "role_id"},
		params.PrincipalID, params.RoleID, params.ValidOn, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken),
		scanAssignment, func(g RoleAssignment) Grant { return g.Grant }, assignmentToAPI)
	if err != nil {
		return nil, err
	}
	return &gen.RoleAssignmentPage{Items: items, NextPageToken: next}, nil
}

// EndRoleAssignment implements POST /v1/role-assignments/{id}:end.
func (e *Endpoints) EndRoleAssignment(ctx context.Context, req *gen.EndInput, params gen.EndRoleAssignmentParams) (gen.EndRoleAssignmentRes, error) {
	g, err := EndAssignment(ctx, params.ID, matchGrant(params.IfMatch, assignmentToAPI), req.ValidTo)
	if err != nil {
		return nil, err
	}
	out := assignmentToAPI(g)
	return &gen.EndRoleAssignmentOKHeaders{ETag: apitypes.ETagOf(out), Response: out}, nil
}

// --- company access

// CreateCompanyAccess implements POST /v1/company-access.
func (e *Endpoints) CreateCompanyAccess(ctx context.Context, req *gen.CompanyAccessCreate, _ gen.CreateCompanyAccessParams) (gen.CreateCompanyAccessRes, error) {
	g, err := GrantAccess(ctx, e.LegalEntities, req.PrincipalID, req.LegalEntityID, apitypes.Ptr[time.Time](req.ValidFrom), apitypes.Ptr[time.Time](req.ValidTo))
	if err != nil {
		return nil, err
	}
	out := accessToAPI(g)
	return &gen.CompanyAccessHeaders{ETag: apitypes.ETagOf(out), Location: gen.NewOptString("/v1/company-access/" + g.ID.String()), Response: out}, nil
}

// GetCompanyAccess implements GET /v1/company-access/{id}.
func (e *Endpoints) GetCompanyAccess(ctx context.Context, params gen.GetCompanyAccessParams) (gen.GetCompanyAccessRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	g, err := getAccess(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	out := accessToAPI(g)
	return &gen.GetCompanyAccessOKHeaders{ETag: apitypes.ETagOf(out), Response: out}, nil
}

// ListCompanyAccess implements GET /v1/company-access.
func (e *Endpoints) ListCompanyAccess(ctx context.Context, params gen.ListCompanyAccessParams) (gen.ListCompanyAccessRes, error) {
	items, next, err := listGrants(ctx, e, grantList{name: "listCompanyAccess", columns: accessColumns, table: "identity.company_access", other: "legal_entity_id"},
		params.PrincipalID, params.LegalEntityID, params.ValidOn, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken),
		scanAccess, func(g CompanyAccess) Grant { return g.Grant }, accessToAPI)
	if err != nil {
		return nil, err
	}
	return &gen.CompanyAccessPage{Items: items, NextPageToken: next}, nil
}

// grantList describes a list of grants filtered by principal, by one other
// id and by the date they are valid on. Its fields are constant SQL
// identifiers written in this package, never input.
type grantList struct {
	name, columns, table, other string
}

// listGrants pages through the grants of q matching the filters, by the
// date they start.
func listGrants[R, A any](ctx context.Context, e *Endpoints, q grantList, principalID, otherID gen.OptUUID, validDate gen.OptDate, page pagination.Request,
	scan func(pgx.Row) (R, error), grant func(R) Grant, toAPI func(R) A) ([]A, gen.OptString, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, gen.OptString{}, err
	}
	principal, other, on := apitypes.Ptr[uuid.UUID](principalID), apitypes.Ptr[uuid.UUID](otherID), apitypes.Ptr[time.Time](validDate)
	ks, err := e.Pages.Keyset(validFromSort, q.name+"|"+uuidParam(principal)+"|"+uuidParam(other)+"|"+dateParam(on), page)
	if err != nil {
		return nil, gen.OptString{}, err
	}
	return list(ctx, tx, ks, `SELECT `+q.columns+` FROM `+q.table+`
		WHERE ($1::uuid IS NULL OR principal_id = $1) AND ($2::uuid IS NULL OR `+q.other+` = $2) AND `+fmt.Sprintf(validOn, 3),
		[]any{principal, other, on}, scan, func(r R) (string, uuid.UUID) { return dateKey(grant(r)) }, toAPI)
}

// EndCompanyAccess implements POST /v1/company-access/{id}:end.
func (e *Endpoints) EndCompanyAccess(ctx context.Context, req *gen.EndInput, params gen.EndCompanyAccessParams) (gen.EndCompanyAccessRes, error) {
	g, err := EndAccess(ctx, params.ID, matchGrant(params.IfMatch, accessToAPI), req.ValidTo)
	if err != nil {
		return nil, err
	}
	out := accessToAPI(g)
	return &gen.EndCompanyAccessOKHeaders{ETag: apitypes.ETagOf(out), Response: out}, nil
}

// --- segregation-of-duties rules

// CreateSodRule implements POST /v1/sod-rules.
func (e *Endpoints) CreateSodRule(ctx context.Context, req *gen.SodRuleCreate, _ gen.CreateSodRuleParams) (gen.CreateSodRuleRes, error) {
	r, err := CreateSodRule(ctx, req.PermissionA, req.PermissionB, req.Description, apitypes.Ptr[time.Time](req.ValidFrom), apitypes.Ptr[time.Time](req.ValidTo))
	if err != nil {
		return nil, err
	}
	out := sodToAPI(r)
	return &gen.SodRuleHeaders{ETag: apitypes.ETagOf(out), Location: gen.NewOptString("/v1/sod-rules/" + r.ID.String()), Response: out}, nil
}

// GetSodRule implements GET /v1/sod-rules/{id}.
func (e *Endpoints) GetSodRule(ctx context.Context, params gen.GetSodRuleParams) (gen.GetSodRuleRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	r, err := getSod(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	out := sodToAPI(r)
	return &gen.GetSodRuleOKHeaders{ETag: apitypes.ETagOf(out), Response: out}, nil
}

var createdSort = pagination.Sort{Default: "created_at", Fields: []pagination.Field{{Name: "created_at", Column: "created_at", Type: "timestamptz"}}}

// ListSodRules implements GET /v1/sod-rules.
func (e *Endpoints) ListSodRules(ctx context.Context, params gen.ListSodRulesParams) (gen.ListSodRulesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	on := apitypes.Ptr[time.Time](params.ValidOn)
	ks, err := e.Pages.Keyset(createdSort, "listSodRules|"+dateParam(on), apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	items, next, err := list(ctx, tx, ks, `SELECT `+sodColumns+` FROM identity.sod_rule WHERE `+fmt.Sprintf(validOn, 1), []any{on}, scanSod,
		func(r SodRule) (string, uuid.UUID) { return r.CreatedAt.UTC().Format(time.RFC3339Nano), r.ID }, sodToAPI)
	if err != nil {
		return nil, err
	}
	return &gen.SodRulePage{Items: items, NextPageToken: next}, nil
}

// EndSodRule implements POST /v1/sod-rules/{id}:end.
func (e *Endpoints) EndSodRule(ctx context.Context, req *gen.EndInput, params gen.EndSodRuleParams) (gen.EndSodRuleRes, error) {
	r, err := EndSodRule(ctx, params.ID, matchGrant(params.IfMatch, sodToAPI), req.ValidTo)
	if err != nil {
		return nil, err
	}
	out := sodToAPI(r)
	return &gen.EndSodRuleOKHeaders{ETag: apitypes.ETagOf(out), Response: out}, nil
}

func uuidParam(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

func dateParam(d *time.Time) string {
	if d == nil {
		return ""
	}
	return date(*d)
}
