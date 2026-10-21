// Package authz authorizes every operation against the contract: the
// operation's x-permission (or its entity's, x-entity-permissions) must be
// among the principal's current permissions, operations marked
// x-agent: false refuse agents and services, an agent acts within its
// autonomy level (ADR-0036),
// and a principal whose permissions break a segregation-of-duties rule may
// not use either permission of the rule (ADR-0010). Company access depends
// on the resource and is checked by the handler with Companies.
package authz

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Authorizer checks operations from the contract.
type Authorizer struct {
	ops map[string]api.Operation
}

// New uses the operations of the API contract.
func New(ops map[string]api.Operation) *Authorizer { return &Authorizer{ops: ops} }

var errOutsideRequest = errors.New("authz: no transaction and principal in context")

// Permissions returns the principal's permissions today: through active roles
// and grants whose validity includes the current date.
func Permissions(ctx context.Context) (map[string]bool, error) {
	tx, ok := reqctx.TxFrom(ctx)
	p, ok2 := reqctx.PrincipalFrom(ctx)
	if !ok || !ok2 {
		return nil, errOutsideRequest
	}
	// CollectRows below returns the query's error.
	rows, _ := tx.Query(ctx, `SELECT DISTINCT rp.permission_code
		FROM identity.principal_role pr
		JOIN identity.role r ON r.tenant_id = pr.tenant_id AND r.id = pr.role_id AND r.status = 'active'
		JOIN identity.role_permission rp ON rp.tenant_id = pr.tenant_id AND rp.role_id = pr.role_id
		WHERE pr.principal_id = $1
		  AND current_date >= pr.valid_from AND (pr.valid_to IS NULL OR current_date < pr.valid_to)
		  AND current_date >= rp.valid_from AND (rp.valid_to IS NULL OR current_date < rp.valid_to)`, p.ID)
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("authz: permissions: %w", err)
	}
	set := make(map[string]bool, len(codes)+len(PersonsOwn))
	for _, c := range codes {
		set[c] = true
	}
	if p.Kind == reqctx.KindHuman {
		for _, c := range PersonsOwn {
			set[c] = true
		}
	}
	return set, nil
}

// PersonsOwn are the permissions every person holds, whatever their roles:
// acting on their own sign-in alone. Ending one's own session must not
// depend on a role an administrator remembered to grant (ADR-0052 G9), and
// a person without roles must be able to sign out too.
var PersonsOwn = []string{"identity.session.end"}

// Require checks one permission, for checks beyond the operation's own (for
// example the approval permission of an object type).
func Require(ctx context.Context, permission string) error {
	set, err := Permissions(ctx)
	if err != nil {
		return err
	}
	if !set[permission] {
		return problem.Forbidden("identity.permission_missing", "This action requires the permission "+permission+".")
	}
	return nil
}

// Authorize checks the operation's agent rule, permission and the
// segregation-of-duties rules that involve the permission.
func (a *Authorizer) Authorize(ctx context.Context, operationID string) error {
	return a.AuthorizeEntity(ctx, operationID, "")
}

// AuthorizeEntity is Authorize for an operation whose permission depends on
// its {entity} path parameter (x-entity-permissions): the permission is the
// entity's, and an entity the operation does not list is 404, as nothing is
// served for it. entity is ignored by an operation with x-permission.
func (a *Authorizer) AuthorizeEntity(ctx context.Context, operationID, entity string) error {
	op, ok := a.ops[operationID]
	if !ok {
		return fmt.Errorf("authz: operation %q is not in the contract", operationID)
	}
	p, ok := reqctx.PrincipalFrom(ctx)
	tx, ok2 := reqctx.TxFrom(ctx)
	if !ok || !ok2 {
		return errOutsideRequest
	}
	if !op.AgentAllowed && p.Kind != reqctx.KindHuman {
		return problem.Forbidden("identity.agent_not_allowed", "Only a person may perform this operation.")
	}
	if p.Kind == reqctx.KindAgent {
		if err := withinAutonomy(ctx, tx, p.ID, op); err != nil {
			return err
		}
	}
	permission := op.Permission
	if op.EntityPermissions != nil {
		if permission = op.EntityPermissions[entity]; permission == "" {
			return problem.NotFound("Nothing is served for this entity.")
		}
	}
	set, err := Permissions(ctx)
	if err != nil {
		return err
	}
	if !set[permission] {
		return problem.Forbidden("identity.permission_missing", "This action requires the permission "+permission+".")
	}
	return checkSoD(ctx, tx, permission, set)
}

// withinAutonomy refuses an agent an operation above its autonomy level,
// read from its profile; an agent without one is at level 0.
func withinAutonomy(ctx context.Context, tx pgx.Tx, agent uuid.UUID, op api.Operation) error {
	need := op.Level()
	if need == api.LevelReads {
		return nil
	}
	return atLevel(ctx, tx, agent, need, "the operation")
}

// RequireWrites refuses an agent below level 2 a part of an operation that
// does more than propose, though the operation itself proposes (creating
// an order from a requisition reserves the requisition, ADR-0036); what
// names the part. People and services are not asked; an agent at level 0
// never gets this far (its operation is refused first), and level 3 passes
// as 2.
func RequireWrites(ctx context.Context, what string) error {
	tx, ok := reqctx.TxFrom(ctx)
	p, ok2 := reqctx.PrincipalFrom(ctx)
	if !ok || !ok2 {
		return errOutsideRequest
	}
	if p.Kind != reqctx.KindAgent {
		return nil
	}
	return atLevel(ctx, tx, p.ID, api.LevelWrites, what)
}

// Level is agent's autonomy level, read from its profile in tx; an agent
// without one is at level 0.
func Level(ctx context.Context, tx pgx.Tx, agent uuid.UUID) (int32, error) {
	var level int32
	if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT autonomy_level FROM identity.agent_profile WHERE principal_id = $1), 0)`, agent).Scan(&level); err != nil {
		return 0, fmt.Errorf("authz: autonomy level: %w", err)
	}
	return level, nil
}

// atLevel refuses an agent whose level is below need.
func atLevel(ctx context.Context, tx pgx.Tx, agent uuid.UUID, need int, what string) error {
	level, err := Level(ctx, tx, agent)
	if err != nil {
		return err
	}
	if int(level) < need {
		return problem.Forbidden("identity.autonomy_insufficient",
			fmt.Sprintf("This agent's autonomy level is %d; %s needs %d (an administrator raises it in the agent's profile).", level, what, need))
	}
	return nil
}

// checkSoD refuses a permission while the principal also holds the other
// permission of a rule in force today. The identity context refuses grants
// that would create such a conflict; this check covers conflicts that exist
// anyway (rules introduced while grants were already in place, or grants
// made outside the API).
func checkSoD(ctx context.Context, tx pgx.Tx, permission string, held map[string]bool) error {
	// CollectRows below returns the query's error.
	rows, _ := tx.Query(ctx, `SELECT CASE WHEN permission_a = $1 THEN permission_b ELSE permission_a END
		FROM identity.sod_rule
		WHERE (permission_a = $1 OR permission_b = $1)
		  AND current_date >= valid_from AND (valid_to IS NULL OR current_date < valid_to)`, permission)
	others, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("authz: sod rules: %w", err)
	}
	for _, other := range others {
		if held[other] {
			return problem.Forbidden("identity.sod_conflict",
				"Segregation of duties: you hold both "+permission+" and "+other+", so you may use neither until one is removed.")
		}
	}
	return nil
}

// Access is the set of legal entities a principal may act in today.
type Access struct {
	All bool        // all_companies: every legal entity, present and future
	IDs []uuid.UUID // otherwise, the legal entities of current company access grants
}

// Allows reports whether the legal entity is accessible.
func (a Access) Allows(legalEntity uuid.UUID) bool {
	if a.All {
		return true
	}
	for _, id := range a.IDs {
		if id == legalEntity {
			return true
		}
	}
	return false
}

// Visible finishes reading one company-scoped row by id, given the read's
// error and the row's legal entity. No row, or a row in a legal entity
// outside access, is notFound: the caller cannot tell the two apart, so ids
// of other companies cannot be probed (04-api-design §4.9). Any other read
// error comes back wrapped with the formatted context, as errs.Wrapf does;
// nil means the row is there and the caller may see it.
func Visible(err error, access Access, legalEntity uuid.UUID, notFound error, format string, args ...any) error {
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !access.Allows(legalEntity)) {
		return notFound
	}
	return errs.Wrapf(err, format, args...)
}

// Companies returns the principal's legal entity access. Contexts filter
// every read and write of company-scoped resources with it, and answer 404
// for resources outside it, so ids cannot be probed (04-api-design §4.9).
func Companies(ctx context.Context) (Access, error) {
	tx, ok := reqctx.TxFrom(ctx)
	p, ok2 := reqctx.PrincipalFrom(ctx)
	if !ok || !ok2 {
		return Access{}, errOutsideRequest
	}
	var a Access
	err := tx.QueryRow(ctx, `SELECT p.all_companies, coalesce(array_agg(ca.legal_entity_id) FILTER (WHERE ca.id IS NOT NULL), '{}')
		FROM identity.principal p
		LEFT JOIN identity.company_access ca ON ca.tenant_id = p.tenant_id AND ca.principal_id = p.id
		  AND current_date >= ca.valid_from AND (ca.valid_to IS NULL OR current_date < ca.valid_to)
		WHERE p.id = $1
		GROUP BY p.all_companies`, p.ID).Scan(&a.All, &a.IDs)
	if err != nil {
		return Access{}, fmt.Errorf("authz: company access: %w", err)
	}
	return a, nil
}
