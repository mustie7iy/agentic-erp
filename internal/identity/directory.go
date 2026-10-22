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

// RolesHeld returns the roles the principal is assigned today, each as
// HoldsRole decides it: the approval inbox (decidable_by=me) asks once
// for all of them.
func (Directory) RolesHeld(ctx context.Context, principal uuid.UUID) ([]uuid.UUID, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	// CollectRows below returns the query's error.
	rows, _ := tx.Query(ctx, `SELECT DISTINCT role_id FROM identity.principal_role
		WHERE principal_id = $1 AND valid_from <= current_date AND (valid_to IS NULL OR valid_to > current_date)`, principal)
	roles, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("identity: roles of principal %s: %w", principal, err)
	}
	return roles, nil
}

// Actors answers who deferred work acts as: the principal that queued it,
// read in the worker's own transaction before the unit of work starts (a
// port of the contexts whose workers act as the requester: payments,
// configuration and workflows).
type Actors struct{}

// Actor returns the principal with the id as the unit of work acts as it.
func (Actors) Actor(ctx context.Context, tx pgx.Tx, id uuid.UUID) (reqctx.Principal, error) {
	p := reqctx.Principal{ID: id}
	if err := tx.QueryRow(ctx, `SELECT tenant_id, kind FROM identity.principal WHERE id = $1`, id).Scan(&p.TenantID, &p.Kind); err != nil {
		return reqctx.Principal{}, fmt.Errorf("identity: read principal %s: %w", id, err)
	}
	return p, nil
}
