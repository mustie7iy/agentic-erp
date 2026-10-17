package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Principals' binding to the tenant's identity providers (ADR-0031). A
// principal with a subject is bound when its issuer is an active
// registered provider of its tenant; one that is not (the development
// issuer 000046 bound some to, a retired provider's) is refused until an
// operator binds it to a provider with its subject there.

// TenantPrincipal is a principal with its binding.
type TenantPrincipal struct {
	ID              uuid.UUID
	Kind            string
	DisplayName     string
	ExternalIssuer  *string
	ExternalSubject *string
	Status          string
	Bound           bool
	Version         int32
	CreatedAt       time.Time
}

// PrincipalSort orders a listing of principals: oldest first.
var PrincipalSort = pagination.Sort{Default: "created_at", Fields: []pagination.Field{{Name: "created_at", Column: "created_at", Type: "timestamptz"}}}

const tenantPrincipalSelect = `SELECT id, kind, display_name, external_issuer, external_subject, status, bound, version, created_at FROM (
	SELECT p.*, (p.external_subject IS NULL OR EXISTS (SELECT FROM identity.identity_provider ip
		WHERE ip.status = 'active' AND ip.issuer = p.external_issuer)) AS bound
	FROM identity.principal p) s`

func scanTenantPrincipal(row pgx.CollectableRow) (TenantPrincipal, error) {
	var p TenantPrincipal
	err := row.Scan(&p.ID, &p.Kind, &p.DisplayName, &p.ExternalIssuer, &p.ExternalSubject, &p.Status, &p.Bound, &p.Version, &p.CreatedAt)
	return p, err //nolint:wrapcheck // the caller wraps with what it read
}

// ListTenantPrincipals lists one page of the tenant's principals, only the
// unbound ones unless all is set.
func ListTenantPrincipals(ctx context.Context, tx pgx.Tx, all bool, ks *pagination.Keyset) ([]TenantPrincipal, error) {
	q, args := ks.SQL(tenantPrincipalSelect+` WHERE ($1 OR NOT bound)`, []any{all})
	rows, _ := tx.Query(ctx, q, args...)
	ps, err := pgx.CollectRows(rows, scanTenantPrincipal)
	if err != nil {
		return nil, fmt.Errorf("identity: list principals: %w", err)
	}
	return ps, nil
}

// Binding is the result of binding a principal: the principal and the
// issuer it was bound to before.
type Binding struct {
	Principal      TenantPrincipal
	PreviousIssuer *string
}

// BindPrincipal binds the principal to the active provider with its
// subject there, guarded by ifMatch, and returns the event and the audit
// entry the caller records with who made the change.
func BindPrincipal(ctx context.Context, tx pgx.Tx, actor, id uuid.UUID, ifMatch func(int32) error, providerID uuid.UUID, subject string) (Binding, outbox.Event, audit.Entry, error) {
	if subject == "" || len(subject) > 255 {
		return Binding{}, outbox.Event{}, audit.Entry{}, problem.Unprocessable("identity.invalid_subject", "The subject is invalid.",
			problem.FieldError{Field: "subject", Code: "invalid", Message: "1 to 255 characters"})
	}
	prov, err := getProvider(ctx, tx, providerID, true) // held: a concurrent retirement waits for the binding
	if err != nil {
		return Binding{}, outbox.Event{}, audit.Entry{}, problem.Reference(err, "identity_provider_id", "identity provider", "an active identity provider of the tenant")
	}
	if prov.Status != ProviderActive {
		return Binding{}, outbox.Event{}, audit.Entry{}, problem.Unprocessable("identity.provider_retired", "The identity provider is retired.",
			problem.FieldError{Field: "identity_provider_id", Code: "retired", Message: "an active identity provider of the tenant"})
	}
	var previous, previousSubject *string
	var version int32
	err = tx.QueryRow(ctx, `SELECT external_issuer, external_subject, version FROM identity.principal WHERE id = $1 FOR UPDATE`, id).Scan(&previous, &previousSubject, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return Binding{}, outbox.Event{}, audit.Entry{}, problem.NotFoundOf("principal")
	}
	if err != nil {
		return Binding{}, outbox.Event{}, audit.Entry{}, fmt.Errorf("identity: read principal %s: %w", id, err)
	}
	if err := ifMatch(version); err != nil {
		return Binding{}, outbox.Event{}, audit.Entry{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE identity.principal SET external_issuer = $2, external_subject = $3, updated_by = $4, version = version + 1 WHERE id = $1`,
		id, prov.Issuer, subject, actor); err != nil {
		return Binding{}, outbox.Event{}, audit.Entry{}, fmt.Errorf("identity: bind principal %s: %w", id, err)
	}
	rows, _ := tx.Query(ctx, tenantPrincipalSelect+` WHERE id = $1`, id)
	p, err := pgx.CollectExactlyOneRow(rows, scanTenantPrincipal)
	if err != nil {
		return Binding{}, outbox.Event{}, audit.Entry{}, fmt.Errorf("identity: read bound principal %s: %w", id, err)
	}
	event := outbox.Event{Context: "identity", Name: "principal_bound", AggregateType: "identity.principal", AggregateID: id,
		Payload: map[string]any{"principal_id": id, "previous_issuer": previous, "external_issuer": prov.Issuer}}
	entry := audit.Entry{Action: "identity.principal.bind", ObjectType: "identity.principal", ObjectID: id,
		Detail: map[string]any{"previous_issuer": previous, "previous_subject": previousSubject, "external_issuer": prov.Issuer, "external_subject": subject, "provider_id": prov.ID}}
	return Binding{Principal: p, PreviousIssuer: previous}, event, entry, nil
}
