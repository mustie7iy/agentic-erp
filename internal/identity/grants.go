package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// RoleAssignment is a row of identity.principal_role.
type RoleAssignment struct {
	Grant
	PrincipalID, RoleID uuid.UUID
}

const assignmentColumns = grantColumns + `, principal_id, role_id`

func scanAssignment(row pgx.Row) (RoleAssignment, error) {
	var g RoleAssignment
	err := row.Scan(append(g.fields(), &g.PrincipalID, &g.RoleID)...)
	return g, errs.Wrapf(err, "scan role assignment")
}

func getAssignment(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (RoleAssignment, error) {
	q := `SELECT ` + assignmentColumns + ` FROM identity.principal_role WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	g, err := scanAssignment(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return g, notFound("role assignment")
	}
	if err != nil {
		return g, fmt.Errorf("identity: read role assignment %s: %w", id, err)
	}
	return g, nil
}

// AssignRole grants a role to a principal for a period.
func AssignRole(ctx context.Context, principal, role uuid.UUID, from, to *time.Time) (RoleAssignment, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return RoleAssignment{}, err
	}
	p, err := getPrincipal(ctx, tx, principal, false)
	if err != nil {
		return RoleAssignment{}, referenceError(err, "principal_id", "principal")
	}
	r, err := getRole(ctx, tx, role, false)
	if err != nil {
		return RoleAssignment{}, referenceError(err, "role_id", "role")
	}
	if r.Status != Active {
		return RoleAssignment{}, problem.Conflict("identity.role_inactive", "The role is inactive and cannot be granted.")
	}
	day, err := today(ctx, tx)
	if err != nil {
		return RoleAssignment{}, err
	}
	v, err := newValidity(day, from, to)
	if err != nil {
		return RoleAssignment{}, err
	}
	g, err := scanAssignment(tx.QueryRow(ctx, `INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, valid_to, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+assignmentColumns,
		actor.TenantID, uuid.Must(uuid.NewV7()), principal, role, v.From, v.To, actor.ID))
	if err != nil {
		return RoleAssignment{}, fmt.Errorf("identity: assign role %s to principal %s: %w", role, principal, err)
	}
	if err := agentApprovalsNow(ctx, tx); err != nil {
		return RoleAssignment{}, err
	}
	if err := sodConflict(ctx, tx, principal, uuid.Nil, uuid.Nil); err != nil {
		return RoleAssignment{}, err
	}
	return g, audit.Record(ctx, "identity.role_assignment.create", "identity.role_assignment", g.ID,
		map[string]any{"principal_id": principal, "role_id": role, "role_code": r.Code, "valid_from": date(g.ValidFrom), "valid_to": nullableDate(g.ValidTo)},
		outbox.Event{Context: "identity", Name: "role_assigned", AggregateType: "identity.role_assignment", AggregateID: g.ID,
			Payload: map[string]any{"role_assignment_id": g.ID, "principal_id": principal, "principal_kind": p.Kind, "role_id": role,
				"role_code": r.Code, "valid_from": date(g.ValidFrom), "valid_to": nullableDate(g.ValidTo)}})
}

// EndAssignment ends a role assignment on day to (exclusive).
func EndAssignment(ctx context.Context, id uuid.UUID, ifMatch func(RoleAssignment) error, to time.Time) (RoleAssignment, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return RoleAssignment{}, err
	}
	g, err := getAssignment(ctx, tx, id, true)
	if err != nil {
		return RoleAssignment{}, err
	}
	if err := ifMatch(g); err != nil {
		return RoleAssignment{}, err
	}
	day, err := today(ctx, tx)
	if err != nil {
		return RoleAssignment{}, err
	}
	if err := checkEnd(day, g.Grant, to); err != nil {
		return RoleAssignment{}, err
	}
	ended, err := scanAssignment(tx.QueryRow(ctx, `UPDATE identity.principal_role SET valid_to = $2, ended_by = $3 WHERE id = $1 RETURNING `+assignmentColumns,
		id, to, actor.ID))
	if err != nil {
		return RoleAssignment{}, fmt.Errorf("identity: end role assignment %s: %w", id, err)
	}
	return ended, audit.Record(ctx, "identity.role_assignment.end", "identity.role_assignment", id,
		map[string]any{"principal_id": g.PrincipalID, "role_id": g.RoleID, "valid_to": date(to)},
		revoked(ended.Grant, "role_assignment", g.PrincipalID, &g.RoleID, nil))
}

// revoked is AccessRevoked for an ended role assignment or company access.
func revoked(g Grant, kind string, principal uuid.UUID, role, legalEntity *uuid.UUID) outbox.Event {
	return outbox.Event{Context: "identity", Name: "access_revoked", AggregateType: "identity." + kind, AggregateID: g.ID,
		Payload: map[string]any{"assignment_id": g.ID, "assignment_kind": kind, "principal_id": principal, "role_id": role,
			"legal_entity_id": legalEntity, "valid_from": date(g.ValidFrom), "valid_to": nullableDate(g.ValidTo),
			"ended_at": g.EndedAt.UTC().Format(time.RFC3339Nano)}}
}

// CompanyAccess is a row of identity.company_access.
type CompanyAccess struct {
	Grant
	PrincipalID, LegalEntityID uuid.UUID
}

const accessColumns = grantColumns + `, principal_id, legal_entity_id`

func scanAccess(row pgx.Row) (CompanyAccess, error) {
	var g CompanyAccess
	err := row.Scan(append(g.fields(), &g.PrincipalID, &g.LegalEntityID)...)
	return g, errs.Wrapf(err, "scan company access")
}

func getAccess(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (CompanyAccess, error) {
	q := `SELECT ` + accessColumns + ` FROM identity.company_access WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	g, err := scanAccess(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return g, notFound("company access grant")
	}
	if err != nil {
		return g, fmt.Errorf("identity: read company access %s: %w", id, err)
	}
	return g, nil
}

// GrantAccess lets a principal act in a legal entity for a period.
func GrantAccess(ctx context.Context, les LegalEntities, principal, legalEntity uuid.UUID, from, to *time.Time) (CompanyAccess, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return CompanyAccess{}, err
	}
	if _, err := getPrincipal(ctx, tx, principal, false); err != nil {
		return CompanyAccess{}, referenceError(err, "principal_id", "principal")
	}
	if _, ok, err := les.LegalEntityStatus(ctx, legalEntity); err != nil || !ok {
		if err == nil {
			err = notFound("legal entity")
		}
		return CompanyAccess{}, referenceError(err, "legal_entity_id", "legal entity")
	}
	day, err := today(ctx, tx)
	if err != nil {
		return CompanyAccess{}, err
	}
	v, err := newValidity(day, from, to)
	if err != nil {
		return CompanyAccess{}, err
	}
	g, err := scanAccess(tx.QueryRow(ctx, `INSERT INTO identity.company_access (tenant_id, id, principal_id, legal_entity_id, valid_from, valid_to, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+accessColumns,
		actor.TenantID, uuid.Must(uuid.NewV7()), principal, legalEntity, v.From, v.To, actor.ID))
	if err != nil {
		return CompanyAccess{}, fmt.Errorf("identity: grant company %s to principal %s: %w", legalEntity, principal, err)
	}
	return g, audit.Record(ctx, "identity.company_access.create", "identity.company_access", g.ID,
		map[string]any{"principal_id": principal, "legal_entity_id": legalEntity, "valid_from": date(g.ValidFrom), "valid_to": nullableDate(g.ValidTo)},
		outbox.Event{Context: "identity", Name: "company_access_granted", AggregateType: "identity.company_access", AggregateID: g.ID,
			Payload: map[string]any{"company_access_id": g.ID, "principal_id": principal, "legal_entity_id": legalEntity,
				"valid_from": date(g.ValidFrom), "valid_to": nullableDate(g.ValidTo)}})
}

// EndAccess ends a company access grant on day to (exclusive).
func EndAccess(ctx context.Context, id uuid.UUID, ifMatch func(CompanyAccess) error, to time.Time) (CompanyAccess, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return CompanyAccess{}, err
	}
	g, err := getAccess(ctx, tx, id, true)
	if err != nil {
		return CompanyAccess{}, err
	}
	if err := ifMatch(g); err != nil {
		return CompanyAccess{}, err
	}
	day, err := today(ctx, tx)
	if err != nil {
		return CompanyAccess{}, err
	}
	if err := checkEnd(day, g.Grant, to); err != nil {
		return CompanyAccess{}, err
	}
	ended, err := scanAccess(tx.QueryRow(ctx, `UPDATE identity.company_access SET valid_to = $2, ended_by = $3 WHERE id = $1 RETURNING `+accessColumns,
		id, to, actor.ID))
	if err != nil {
		return CompanyAccess{}, fmt.Errorf("identity: end company access %s: %w", id, err)
	}
	return ended, audit.Record(ctx, "identity.company_access.end", "identity.company_access", id,
		map[string]any{"principal_id": g.PrincipalID, "legal_entity_id": g.LegalEntityID, "valid_to": date(to)},
		revoked(ended.Grant, "company_access", g.PrincipalID, nil, &g.LegalEntityID))
}

// SodRule is a row of identity.sod_rule.
type SodRule struct {
	Grant
	PermissionA, PermissionB, Description string
}

const sodColumns = grantColumns + `, permission_a, permission_b, description`

func scanSod(row pgx.Row) (SodRule, error) {
	var r SodRule
	err := row.Scan(append(r.fields(), &r.PermissionA, &r.PermissionB, &r.Description)...)
	return r, errs.Wrapf(err, "scan segregation-of-duties rule")
}

func getSod(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (SodRule, error) {
	q := `SELECT ` + sodColumns + ` FROM identity.sod_rule WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	r, err := scanSod(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return r, notFound("segregation-of-duties rule")
	}
	if err != nil {
		return r, fmt.Errorf("identity: read segregation-of-duties rule %s: %w", id, err)
	}
	return r, nil
}

// CreateSodRule adds a rule, stored with its pair in canonical order. It is
// refused while any principal would break it: remove the conflicting
// grants first, so the rule holds from the day it starts.
func CreateSodRule(ctx context.Context, a, b, description string, from, to *time.Time) (SodRule, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return SodRule{}, err
	}
	if a == b {
		return SodRule{}, problem.Unprocessable("identity.invalid_sod_rule", "A rule needs two different permissions.",
			problem.FieldError{Field: "permission_b", Code: "same_as_a", Message: "differs from permission_a"})
	}
	if b < a {
		a, b = b, a
	}
	var known int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM ref.permission WHERE code IN ($1, $2)`, a, b).Scan(&known); err != nil {
		return SodRule{}, fmt.Errorf("identity: check permissions %s and %s: %w", a, b, err)
	}
	if known != 2 {
		return SodRule{}, unknownPermission("permission_a")
	}
	day, err := today(ctx, tx)
	if err != nil {
		return SodRule{}, err
	}
	v, err := newValidity(day, from, to)
	if err != nil {
		return SodRule{}, err
	}
	r, err := scanSod(tx.QueryRow(ctx, `INSERT INTO identity.sod_rule (tenant_id, id, permission_a, permission_b, description, valid_from, valid_to, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+sodColumns,
		actor.TenantID, uuid.Must(uuid.NewV7()), a, b, description, v.From, v.To, actor.ID))
	if err != nil {
		return SodRule{}, fmt.Errorf("identity: create segregation-of-duties rule %s/%s: %w", a, b, err)
	}
	if err := sodConflict(ctx, tx, uuid.Nil, uuid.Nil, r.ID); err != nil {
		return SodRule{}, err
	}
	return r, audit.Record(ctx, "identity.sod_rule.create", "identity.sod_rule", r.ID,
		map[string]any{"permission_a": a, "permission_b": b, "valid_from": date(r.ValidFrom), "valid_to": nullableDate(r.ValidTo)},
		sodEvent(r, "created"))
}

func sodEvent(r SodRule, change string) outbox.Event {
	var endedAt any
	if r.EndedAt != nil {
		endedAt = r.EndedAt.UTC().Format(time.RFC3339Nano)
	}
	return outbox.Event{Context: "identity", Name: "sod_rule_changed", AggregateType: "identity.sod_rule", AggregateID: r.ID,
		Payload: map[string]any{"sod_rule_id": r.ID, "change": change, "permission_a": r.PermissionA, "permission_b": r.PermissionB,
			"description": r.Description, "valid_from": date(r.ValidFrom), "valid_to": nullableDate(r.ValidTo), "ended_at": endedAt}}
}

// EndSodRule retires a rule on day to (exclusive).
func EndSodRule(ctx context.Context, id uuid.UUID, ifMatch func(SodRule) error, to time.Time) (SodRule, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return SodRule{}, err
	}
	r, err := getSod(ctx, tx, id, true)
	if err != nil {
		return SodRule{}, err
	}
	if err := ifMatch(r); err != nil {
		return SodRule{}, err
	}
	day, err := today(ctx, tx)
	if err != nil {
		return SodRule{}, err
	}
	if err := checkEnd(day, r.Grant, to); err != nil {
		return SodRule{}, err
	}
	ended, err := scanSod(tx.QueryRow(ctx, `UPDATE identity.sod_rule SET valid_to = $2, ended_by = $3 WHERE id = $1 RETURNING `+sodColumns, id, to, actor.ID))
	if err != nil {
		return SodRule{}, fmt.Errorf("identity: end segregation-of-duties rule %s: %w", id, err)
	}
	return ended, audit.Record(ctx, "identity.sod_rule.end", "identity.sod_rule", id,
		map[string]any{"permission_a": r.PermissionA, "permission_b": r.PermissionB, "valid_to": date(to)}, sodEvent(ended, "ended"))
}
