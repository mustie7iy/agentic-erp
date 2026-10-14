package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Role is a row of identity.role with the permissions it grants today.
type Role struct {
	ID          uuid.UUID
	Code        string
	Name        string
	Description *string
	Status      string
	Permissions []string
	Version     int32
	CreatedAt   time.Time
	CreatedBy   uuid.UUID
	UpdatedAt   time.Time
	UpdatedBy   uuid.UUID
}

const roleSelect = `SELECT r.id, r.code, r.name, r.description, r.status, r.version, r.created_at, r.created_by, r.updated_at, r.updated_by,
		coalesce((SELECT array_agg(rp.permission_code ORDER BY rp.permission_code) FROM identity.role_permission rp
			WHERE rp.tenant_id = r.tenant_id AND rp.role_id = r.id
			  AND current_date >= rp.valid_from AND (rp.valid_to IS NULL OR current_date < rp.valid_to)), '{}')
	FROM identity.role r`

func scanRole(row pgx.Row) (Role, error) {
	var r Role
	err := row.Scan(&r.ID, &r.Code, &r.Name, &r.Description, &r.Status, &r.Version, &r.CreatedAt, &r.CreatedBy, &r.UpdatedAt, &r.UpdatedBy, &r.Permissions)
	return r, errs.Wrapf(err, "scan role")
}

func getRole(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Role, error) {
	q := roleSelect + ` WHERE r.id = $1`
	if lock {
		q += ` FOR UPDATE OF r`
	}
	r, err := scanRole(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return r, notFound("role")
	}
	if err != nil {
		return r, fmt.Errorf("identity: read role %s: %w", id, err)
	}
	return r, nil
}

// CreateRole inserts an active role without permissions.
func CreateRole(ctx context.Context, code, name string, description *string) (Role, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return Role{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO identity.role (tenant_id, id, code, name, description, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, 'active', $6, $6)`, actor.TenantID, id, code, name, description, actor.ID); err != nil {
		return Role{}, fmt.Errorf("identity: insert role %s: %w", code, err)
	}
	r, err := getRole(ctx, tx, id, false)
	if err != nil {
		return Role{}, err
	}
	return r, audit.Record(ctx, "identity.role.create", "identity.role", id, map[string]any{"code": code},
		outbox.Event{Context: "identity", Name: "role_created", AggregateType: "identity.role", AggregateID: id,
			Payload: map[string]any{"role_id": id, "code": r.Code, "name": r.Name, "status": r.Status}})
}

// UpdateRole changes the name and description. description set to a nil
// pointer value clears it.
func UpdateRole(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, name *string, description **string) (Role, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return Role{}, err
	}
	r, err := getRole(ctx, tx, id, true)
	if err != nil {
		return Role{}, err
	}
	if err := ifMatch(r.Version); err != nil {
		return Role{}, err
	}
	newName, newDesc := r.Name, r.Description
	if name != nil {
		newName = *name
	}
	if description != nil {
		newDesc = *description
	}
	if _, err := tx.Exec(ctx, `UPDATE identity.role SET name = $2, description = $3, version = version + 1, updated_by = $4 WHERE id = $1`,
		id, newName, newDesc, actor.ID); err != nil {
		return Role{}, fmt.Errorf("identity: update role %s: %w", id, err)
	}
	updated, err := getRole(ctx, tx, id, false)
	if err != nil {
		return Role{}, err
	}
	return updated, audit.Record(ctx, "identity.role.update", "identity.role", id,
		map[string]any{"changed_fields": changedFields(r.Name != newName, "name", !same(r.Description, newDesc), "description")})
}

// RolePermission is a row of identity.role_permission.
type RolePermission struct {
	Grant
	RoleID         uuid.UUID
	PermissionCode string
}

const rolePermissionColumns = grantColumns + `, role_id, permission_code`

func scanRolePermission(row pgx.Row) (RolePermission, error) {
	var g RolePermission
	err := row.Scan(append(g.fields(), &g.RoleID, &g.PermissionCode)...)
	return g, errs.Wrapf(err, "scan role permission")
}

func getRolePermission(ctx context.Context, tx pgx.Tx, role, id uuid.UUID, lock bool) (RolePermission, error) {
	q := `SELECT ` + rolePermissionColumns + ` FROM identity.role_permission WHERE role_id = $1 AND id = $2`
	if lock {
		q += ` FOR UPDATE`
	}
	g, err := scanRolePermission(tx.QueryRow(ctx, q, role, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return g, notFound("permission grant of this role")
	}
	if err != nil {
		return g, fmt.Errorf("identity: read permission grant %s of role %s: %w", id, role, err)
	}
	return g, nil
}

// GrantPermission adds an effective-dated permission to a role.
func GrantPermission(ctx context.Context, role uuid.UUID, permission string, from, to *time.Time) (RolePermission, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return RolePermission{}, err
	}
	if _, err := getRole(ctx, tx, role, false); err != nil {
		return RolePermission{}, err
	}
	var known bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM ref.permission WHERE code = $1)`, permission).Scan(&known); err != nil {
		return RolePermission{}, fmt.Errorf("identity: check permission %s: %w", permission, err)
	}
	if !known {
		return RolePermission{}, unknownPermission("permission_code")
	}
	day, err := database.Today(ctx, tx)
	if err != nil {
		return RolePermission{}, err
	}
	v, err := newValidity(day, from, to)
	if err != nil {
		return RolePermission{}, err
	}
	g, err := scanRolePermission(tx.QueryRow(ctx, `INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, valid_to, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+rolePermissionColumns,
		actor.TenantID, uuid.Must(uuid.NewV7()), role, permission, v.From, v.To, actor.ID))
	if err != nil {
		return RolePermission{}, fmt.Errorf("identity: grant permission %s to role %s: %w", permission, role, err)
	}
	if err := agentApprovalsNow(ctx, tx); err != nil {
		return RolePermission{}, err
	}
	if err := sodConflict(ctx, tx, uuid.Nil, role, uuid.Nil); err != nil {
		return RolePermission{}, err
	}
	return g, audit.Record(ctx, "identity.role_permission.grant", "identity.role_permission", g.ID,
		map[string]any{"role_id": role, "permission_code": permission, "valid_from": date(g.ValidFrom), "valid_to": nullableDate(g.ValidTo)},
		outbox.Event{Context: "identity", Name: "role_permission_granted", AggregateType: "identity.role_permission", AggregateID: g.ID,
			Payload: map[string]any{"role_permission_id": g.ID, "role_id": role, "permission_code": permission,
				"valid_from": date(g.ValidFrom), "valid_to": nullableDate(g.ValidTo)}})
}

func unknownPermission(field string) error {
	return problem.Unprocessable("identity.unknown_permission", "No such permission in the platform catalog.",
		problem.FieldError{Field: field, Code: "unknown", Message: "a permission code from the platform catalog"})
}

// EndPermission ends a role's permission grant on day to (exclusive).
func EndPermission(ctx context.Context, role, id uuid.UUID, ifMatch func(RolePermission) error, to time.Time) (RolePermission, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return RolePermission{}, err
	}
	g, err := getRolePermission(ctx, tx, role, id, true)
	if err != nil {
		return RolePermission{}, err
	}
	if err := ifMatch(g); err != nil {
		return RolePermission{}, err
	}
	day, err := database.Today(ctx, tx)
	if err != nil {
		return RolePermission{}, err
	}
	if err := checkEnd(day, g.Grant, to); err != nil {
		return RolePermission{}, err
	}
	ended, err := scanRolePermission(tx.QueryRow(ctx, `UPDATE identity.role_permission SET valid_to = $2, ended_by = $3 WHERE id = $1 RETURNING `+rolePermissionColumns,
		id, to, actor.ID))
	if err != nil {
		return RolePermission{}, fmt.Errorf("identity: end permission grant %s of role %s: %w", id, role, err)
	}
	return ended, audit.Record(ctx, "identity.role_permission.end", "identity.role_permission", id,
		map[string]any{"role_id": role, "permission_code": g.PermissionCode, "valid_to": date(to)},
		outbox.Event{Context: "identity", Name: "role_permission_ended", AggregateType: "identity.role_permission", AggregateID: id,
			Payload: map[string]any{"role_permission_id": id, "role_id": role, "permission_code": g.PermissionCode,
				"valid_from": date(g.ValidFrom), "valid_to": date(to), "ended_at": ended.EndedAt.UTC().Format(time.RFC3339Nano)}})
}
