// Package identity is the identity context (03-data-model §3.4, ADR-0010):
// principals (people, agents, services), roles and the effective-dated
// grants that give principals permissions and company access, and the
// segregation-of-duties rules that limit which permissions one principal
// may hold together.
//
// Grants are insert-only except for ending them, so the access a principal
// had on any past date can be reconstructed: they cannot start in the past,
// and an end can only be brought forward, never into the past. Three rules
// are enforced on every grant, in the request's transaction:
//   - agents never hold approval permissions (the database's constraint
//     trigger, made immediate here so the answer is a 409);
//   - no principal holds both permissions of a segregation-of-duties rule
//     on any day from today on;
//   - periods of the same pair never overlap (exclusion constraints).
//
// Identity administration is human-only; the contract marks it so and the
// pipeline enforces it.
package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

func init() {
	// The subject alone (principal_subject) is unique until the contract
	// step drops that index; then the issuer and subject are (ADR-0010).
	subjectTaken := func() *problem.Problem {
		return problem.Conflict("identity.subject_taken", "Another principal already has this identity provider subject.")
	}
	for name, p := range map[string]func() *problem.Problem{
		"principal_subject":        subjectTaken,
		"principal_issuer_subject": subjectTaken,
		"role_tenant_id_code_key": func() *problem.Problem {
			return problem.Conflict("identity.role_code_taken", "Another role already has this code.")
		},
		"agent_approval_permission": func() *problem.Problem {
			return problem.Conflict("identity.agent_approval_permission",
				"Agents may not hold approval permissions: the role would give an agent an approval permission.")
		},
	} {
		problem.RegisterConstraint(name, p)
	}
	overlap := func() *problem.Problem {
		return problem.Conflict("identity.grant_overlaps", "An existing grant for the same pair overlaps this period; end it first or start after it.")
	}
	for _, name := range []string{
		"principal_role_tenant_id_principal_id_role_id_daterange_excl",
		"role_permission_tenant_id_role_id_permission_code_daterang_excl",
		"company_access_tenant_id_principal_id_legal_entity_id_date_excl",
		"sod_rule_tenant_id_permission_a_permission_b_daterange_excl",
	} {
		problem.RegisterConstraint(name, overlap)
	}
}

// LegalEntities is what identity needs to know about the tenancy context's
// legal entities, answered in the request's transaction.
type LegalEntities interface {
	LegalEntityStatus(ctx context.Context, id uuid.UUID) (status string, ok bool, err error)
}

// Grant holds the fields every effective-dated grant has.
type Grant struct {
	ID        uuid.UUID
	ValidFrom time.Time
	ValidTo   *time.Time
	CreatedAt time.Time
	CreatedBy uuid.UUID
	EndedAt   *time.Time
	EndedBy   *uuid.UUID
}

const grantColumns = `id, valid_from, valid_to, created_at, created_by, ended_at, ended_by`

func (g *Grant) fields() []any {
	return []any{&g.ID, &g.ValidFrom, &g.ValidTo, &g.CreatedAt, &g.CreatedBy, &g.EndedAt, &g.EndedBy}
}

// Validity is the requested period of a new grant.
type Validity struct {
	From time.Time
	To   *time.Time
}

// newValidity defaults the start to today and refuses periods that start in
// the past or end before they start.
func newValidity(today time.Time, from, to *time.Time) (Validity, error) {
	v := Validity{From: today, To: to}
	if from != nil {
		v.From = *from
	}
	if v.From.Before(today) {
		return v, problem.Unprocessable("identity.backdated_grant", "A grant cannot start in the past; the access history must stay as it was.",
			problem.FieldError{Field: "valid_from", Code: "in_past", Message: "today or later"})
	}
	if v.To != nil && !v.To.After(v.From) {
		return v, problem.Unprocessable("identity.invalid_validity", "valid_to must be after valid_from.",
			problem.FieldError{Field: "valid_to", Code: "before_start", Message: "after valid_from"})
	}
	return v, nil
}

// checkEnd validates ending a grant on day to (exclusive).
func checkEnd(today time.Time, g Grant, to time.Time) error {
	field := func(code, msg string) problem.FieldError {
		return problem.FieldError{Field: "valid_to", Code: code, Message: msg}
	}
	switch {
	case to.Before(today):
		return problem.Unprocessable("identity.invalid_end", "A grant cannot be ended in the past.", field("in_past", "today or later"))
	case g.ValidTo != nil && !to.Before(*g.ValidTo):
		return problem.Unprocessable("identity.invalid_end", "An end can only be brought forward.", field("not_earlier", "before the current valid_to"))
	case !to.After(g.ValidFrom):
		return problem.Unprocessable("identity.invalid_end", "valid_to must be after valid_from.", field("before_start", "after valid_from"))
	}
	return nil
}

// Validity values as event fields: dates as YYYY-MM-DD, open ends as null.
func date(t time.Time) string { return t.Format(time.DateOnly) }

func nullableDate(t *time.Time) any {
	if t == nil {
		return nil
	}
	return date(*t)
}

// conflict is one principal that would hold both permissions of a rule.
type conflict struct {
	Principal                uuid.UUID
	PermissionA, PermissionB string
}

// sodConflict finds a principal that holds, on some day from today on, both
// permissions of a segregation-of-duties rule in force that day. The search
// is limited to one principal, the holders of one role, or one rule (the
// zero uuid means no limit), which covers every grant that can create a
// conflict.
func sodConflict(ctx context.Context, tx pgx.Tx, principal, role, rule uuid.UUID) error {
	var c conflict
	err := tx.QueryRow(ctx, `WITH held AS (
			SELECT pr.principal_id, rp.permission_code,
			       daterange(pr.valid_from, pr.valid_to) * daterange(rp.valid_from, rp.valid_to) * daterange(current_date, NULL) AS period
			FROM identity.principal_role pr
			JOIN identity.role r ON r.tenant_id = pr.tenant_id AND r.id = pr.role_id AND r.status = 'active'
			JOIN identity.role_permission rp ON rp.tenant_id = pr.tenant_id AND rp.role_id = pr.role_id
			WHERE ($1::uuid IS NULL OR pr.principal_id = $1)
			  AND ($2::uuid IS NULL OR pr.principal_id IN (SELECT principal_id FROM identity.principal_role WHERE role_id = $2))
		)
		SELECT a.principal_id, s.permission_a, s.permission_b
		FROM identity.sod_rule s
		JOIN held a ON a.permission_code = s.permission_a
		JOIN held b ON b.principal_id = a.principal_id AND b.permission_code = s.permission_b
		WHERE ($3::uuid IS NULL OR s.id = $3)
		  AND NOT isempty(a.period * b.period * daterange(s.valid_from, s.valid_to))
		LIMIT 1`, nilIfZero(principal), nilIfZero(role), nilIfZero(rule)).Scan(&c.Principal, &c.PermissionA, &c.PermissionB)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("identity: check segregation of duties: %w", err)
	}
	return problem.Conflict("identity.sod_conflict", "Segregation of duties: principal "+c.Principal.String()+" would hold both "+
		c.PermissionA+" and "+c.PermissionB+".")
}

func nilIfZero(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// agentApprovalsNow runs the deferred agent-approval constraint triggers
// immediately, so a violation fails this request with 409 instead of its
// commit.
func agentApprovalsNow(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SET CONSTRAINTS identity.principal_role_agent_approvals, identity.role_permission_agent_approvals IMMEDIATE`)
	if err != nil {
		return fmt.Errorf("identity: check agent approvals: %w", err)
	}
	return nil
}
