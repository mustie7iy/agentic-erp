package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Directory answers other contexts' questions about roles, in the
// request's transaction (a port of the workflow context, whose approval
// steps name roles).
type Directory struct{}

// RoleByCode returns the id of the active role with the code, or ok = false.
func (Directory) RoleByCode(ctx context.Context, code string) (uuid.UUID, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return uuid.Nil, false, err
	}
	var id uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM identity.role WHERE code = $1 AND status = 'active'`, code).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return id, false, fmt.Errorf("identity: read role %s: %w", code, err)
	}
	return id, true, nil
}

// HoldsRole reports whether the principal is assigned the role today.
func (Directory) HoldsRole(ctx context.Context, principal, role uuid.UUID) (bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return false, err
	}
	var ok bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM identity.principal_role
		WHERE principal_id = $1 AND role_id = $2 AND valid_from <= current_date AND (valid_to IS NULL OR valid_to > current_date))`,
		principal, role).Scan(&ok)
	if err != nil {
		return ok, fmt.Errorf("identity: check role %s of principal %s: %w", role, principal, err)
	}
	return ok, nil
}
