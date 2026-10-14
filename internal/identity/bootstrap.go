package identity

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
)

// A new tenant's first administrator (ADR-0014): provisioning creates the
// tenant and then, in the same transaction, acts as a person who does not
// exist yet. These two steps make that person and give their role every
// permission; from then on the tenant is administered through the API.

// FirstAdministrator is a new tenant's first person.
type FirstAdministrator struct {
	TenantID    uuid.UUID
	ID          uuid.UUID
	Issuer      string // the identity provider (OIDC iss) whose tokens authenticate them (ADR-0010)
	Subject     string // subject at that identity provider
	DisplayName string
	Email       *string
}

// CreateFirstAdministrator creates the administrator in tx: a human with
// access to every company, active, who created themselves since nobody
// comes before them, bound to their issuer and subject. A constraint
// violation is returned wrapped, for the caller's problem mapping. An empty
// issuer is the caller's wiring mistake and is refused: the administrator
// would match no token, and nobody could administer the tenant.
func CreateFirstAdministrator(ctx context.Context, tx pgx.Tx, a FirstAdministrator) error {
	if a.Issuer == "" {
		return fmt.Errorf("identity: create administrator %s: no issuer to bind subject %q to", a.ID, a.Subject)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity.principal (tenant_id, id, kind, external_issuer, external_subject, display_name, email, all_companies, status, created_by, updated_by)
		VALUES ($1, $2, 'human', $3, $4, $5, $6, true, 'active', $2, $2)`, a.TenantID, a.ID, a.Issuer, a.Subject, a.DisplayName, a.Email); err != nil {
		return fmt.Errorf("identity: create administrator %s: %w", a.ID, err)
	}
	return nil
}

// GrantEveryPermission grants the role every permission there is, from
// today, in one grant made by the administrator in tx: the tenant's root
// role, recorded by one audit entry in ctx's buffer rather than an event per
// permission as grants made through the API are.
func GrantEveryPermission(ctx context.Context, tx pgx.Tx, a FirstAdministrator, role uuid.UUID) error {
	tag, err := tx.Exec(ctx, `INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, created_by)
		SELECT $1, gen_random_uuid(), $2, code, current_date, $3 FROM ref.permission`, a.TenantID, role, a.ID)
	if err != nil {
		return fmt.Errorf("identity: tenant %s: grant every permission to role %s: %w", a.TenantID, role, err)
	}
	return audit.Add(ctx, audit.Entry{Action: "identity.role_permission.grant", ObjectType: "identity.role", ObjectID: role, Outcome: "succeeded",
		Detail: map[string]any{"role_id": role, "permissions": "all", "count": tag.RowsAffected()}})
}
