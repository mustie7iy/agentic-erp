package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
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
//
// Actor reads the principal whatever its status: a job's steps that only
// record how it ended (a run failed, its slot freed) still act as its
// requester. The steps that do the work call MayAct first, so a requester
// no longer active is refused as a request of theirs is (PO 2026-11-03).
type Actors struct{}

// Actor returns the principal with the id as the unit of work acts as it.
func (Actors) Actor(ctx context.Context, tx pgx.Tx, id uuid.UUID) (reqctx.Principal, error) {
	p := reqctx.Principal{ID: id}
	if err := tx.QueryRow(ctx, `SELECT tenant_id, kind FROM identity.principal WHERE id = $1`, id).Scan(&p.TenantID, &p.Kind); err != nil {
		return reqctx.Principal{}, fmt.Errorf("identity: read principal %s: %w", id, err)
	}
	return p, nil
}

// Active tells whether the principal with the id is active: a template's
// owner, for whom the ledger's recurrer parks entries (ADR-0056).
func (Actors) Active(ctx context.Context, tx pgx.Tx, id uuid.UUID) (bool, error) {
	status, err := statusOf(ctx, tx, id)
	return status == "active", err
}

// MayAct refuses deferred work acting as a principal no longer active
// (blocked or deprovisioned since it queued the work) with the problem a
// request of that principal is refused with, 403
// identity.principal_blocked (uow's resolve): a business problem, which
// fails the job instead of being retried (PO 2026-11-03).
func (Actors) MayAct(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	status, err := statusOf(ctx, tx, id)
	if err != nil || status == "active" {
		return err
	}
	return problem.Forbidden("identity.principal_blocked", "The principal the work acts as, its requester, is "+status+"; the work is stopped.")
}

// statusOf reads the principal's status.
func statusOf(ctx context.Context, tx pgx.Tx, id uuid.UUID) (string, error) {
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM identity.principal WHERE id = $1`, id).Scan(&status); err != nil {
		return "", fmt.Errorf("identity: read the status of principal %s: %w", id, err)
	}
	return status, nil
}
