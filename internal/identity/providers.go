package identity

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// A tenant's identity providers and authentication policy (ADR-0031). The
// active registration holding a token's issuer and audience decides the
// token's tenant, so the pair belongs to one tenant; the policy says
// whether people must prove multi-factor authentication, which a provider
// proves by its acr or amr values, and how long their sessions last. The
// operations run in the transaction the caller began in the tenant's scope
// (the control plane, which records each change in the tenant's audit trail
// and outbox); ResolveIdentityProvider runs across tenants as the control
// role, before the token's tenant is known.

func init() {
	problem.RegisterConstraint("identity_provider_issuer_audience", func() *problem.Problem {
		return problem.Conflict("identity.provider_taken", "This issuer is already registered with this audience, by this tenant or another.")
	})
}

// Statuses of an identity provider.
const (
	ProviderActive  = "active"
	ProviderRetired = "retired"
)

// The bounds of a tenant's policy and its defaults, which a tenant without
// a policy row has (identity.auth_policy).
const (
	DefaultSessionMaxAge = 12 * time.Hour
	DefaultIdleTimeout   = 30 * time.Minute
	MinSessionMaxAge     = 15 * time.Minute
	MaxSessionMaxAge     = 24 * time.Hour
	MinIdleTimeout       = 5 * time.Minute
	MaxIdleTimeout       = 8 * time.Hour
)

// ErrUnknownProvider means no active registration holds the issuer with
// any of the audiences; ErrAmbiguousProvider that more than one does.
var (
	ErrUnknownProvider   = errors.New("identity: no identity provider is registered for this issuer and audience")
	ErrAmbiguousProvider = errors.New("identity: the token's audiences name more than one registered identity provider")
)

// IdentityProvider is a row of identity.identity_provider.
type IdentityProvider struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	Issuer      string
	Audience    string
	TenantClaim *string // a claim that must carry the tenant's id; nil: none
	ACRValues   []string
	AMRValues   []string
	// CertificateBound says the provider binds its tokens to the client's
	// certificate (RFC 8705), which production requires of agents' tokens
	// (ADR-0033).
	CertificateBound bool
	Status           string
	CreatedAt        time.Time
	CreatedBy        uuid.UUID
	UpdatedAt        time.Time
	UpdatedBy        uuid.UUID
	RetiredAt        *time.Time
	Version          int32
}

// NewIdentityProvider is a registration request.
type NewIdentityProvider struct {
	Issuer           string
	Audience         string
	TenantClaim      *string
	ACRValues        []string
	AMRValues        []string
	CertificateBound bool
}

// IdentityProviderChange is a merge patch: nil fields are unchanged; a
// TenantClaim pointing at nil clears it. Issuer and audience never change.
type IdentityProviderChange struct {
	TenantClaim      **string
	ACRValues        *[]string
	AMRValues        *[]string
	CertificateBound *bool
}

// AuthPolicy is a tenant's authentication policy.
type AuthPolicy struct {
	MFARequired   bool
	SessionMaxAge time.Duration
	IdleTimeout   time.Duration
	UpdatedBy     *uuid.UUID // nil: the defaults, never changed
	Version       int32
}

// AuthPolicyChange is a merge patch of the policy: nil fields are unchanged.
type AuthPolicyChange struct {
	MFARequired   *bool
	SessionMaxAge *time.Duration
	IdleTimeout   *time.Duration
}

// Registration is what authenticating a token needs: the provider, its
// tenant and the tenant's policy.
type Registration struct {
	ProviderID  uuid.UUID
	TenantID    uuid.UUID
	Issuer      string
	Audience    string
	TenantClaim *string
	ACRValues   []string
	AMRValues   []string
	// CertificateBound says the provider binds its tokens to the client's
	// certificate (ADR-0033).
	CertificateBound bool
	Policy           AuthPolicy
}

const providerColumns = `id, tenant_id, issuer, audience, tenant_claim, acr_values, amr_values, certificate_bound, status,
	created_at, created_by, updated_at, updated_by, retired_at, version`

func scanProvider(row pgx.Row) (IdentityProvider, error) {
	var p IdentityProvider
	err := row.Scan(&p.ID, &p.TenantID, &p.Issuer, &p.Audience, &p.TenantClaim, &p.ACRValues, &p.AMRValues, &p.CertificateBound, &p.Status,
		&p.CreatedAt, &p.CreatedBy, &p.UpdatedAt, &p.UpdatedBy, &p.RetiredAt, &p.Version)
	return p, err //nolint:wrapcheck // callers wrap with the provider they read
}

// RegisterIdentityProvider registers a provider of the transaction's tenant.
func RegisterIdentityProvider(ctx context.Context, tx pgx.Tx, actor uuid.UUID, in NewIdentityProvider) (IdentityProvider, error) {
	if err := ValidateProvider(in); err != nil {
		return IdentityProvider{}, err
	}
	if err := provable(ctx, tx, in.ACRValues, in.AMRValues); err != nil {
		return IdentityProvider{}, err
	}
	p, err := scanProvider(tx.QueryRow(ctx, `INSERT INTO identity.identity_provider
		(tenant_id, id, issuer, audience, tenant_claim, acr_values, amr_values, certificate_bound, created_by, updated_by)
		VALUES (platform.current_tenant(), $1, $2, $3, $4, $5, $6, $7, $8, $8) RETURNING `+providerColumns,
		uuid.Must(uuid.NewV7()), in.Issuer, in.Audience, in.TenantClaim, values(in.ACRValues), values(in.AMRValues), in.CertificateBound, actor))
	if err != nil {
		return p, fmt.Errorf("identity: register identity provider %s: %w", in.Issuer, err)
	}
	return p, nil
}

// GetIdentityProvider reads a provider of the transaction's tenant.
func GetIdentityProvider(ctx context.Context, tx pgx.Tx, id uuid.UUID) (IdentityProvider, error) {
	return getProvider(ctx, tx, id, false)
}

func getProvider(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (IdentityProvider, error) {
	sql := `SELECT ` + providerColumns + ` FROM identity.identity_provider WHERE id = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	p, err := scanProvider(tx.QueryRow(ctx, sql, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, problem.NotFoundOf("identity provider")
	}
	if err != nil {
		return p, fmt.Errorf("identity: read identity provider %s: %w", id, err)
	}
	return p, nil
}

// ListIdentityProviders lists the tenant's providers, retired ones too, in
// the order they were registered. A tenant has a handful, so there are no
// pages.
func ListIdentityProviders(ctx context.Context, tx pgx.Tx) ([]IdentityProvider, error) {
	rows, _ := tx.Query(ctx, `SELECT `+providerColumns+` FROM identity.identity_provider ORDER BY created_at, id`)
	ps, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (IdentityProvider, error) { return scanProvider(row) })
	if err != nil {
		return nil, fmt.Errorf("identity: list identity providers: %w", err)
	}
	return ps, nil
}

// UpdateIdentityProvider changes an active provider's tenant claim and
// values, guarded by ifMatch.
func UpdateIdentityProvider(ctx context.Context, tx pgx.Tx, actor, id uuid.UUID, ifMatch func(int32) error, ch IdentityProviderChange) (IdentityProvider, error) {
	p, err := activeProvider(ctx, tx, id, ifMatch)
	if err != nil {
		return p, err
	}
	in := NewIdentityProvider{Issuer: p.Issuer, Audience: p.Audience, TenantClaim: p.TenantClaim, ACRValues: p.ACRValues, AMRValues: p.AMRValues,
		CertificateBound: p.CertificateBound}
	if ch.TenantClaim != nil {
		in.TenantClaim = *ch.TenantClaim
	}
	if ch.ACRValues != nil {
		in.ACRValues = *ch.ACRValues
	}
	if ch.AMRValues != nil {
		in.AMRValues = *ch.AMRValues
	}
	if ch.CertificateBound != nil {
		in.CertificateBound = *ch.CertificateBound
	}
	if err := ValidateProvider(in); err != nil {
		return p, err
	}
	if err := provable(ctx, tx, in.ACRValues, in.AMRValues); err != nil {
		return p, err
	}
	p, err = scanProvider(tx.QueryRow(ctx, `UPDATE identity.identity_provider SET tenant_claim = $2, acr_values = $3, amr_values = $4,
		certificate_bound = $5, updated_by = $6, version = version + 1 WHERE id = $1 RETURNING `+providerColumns,
		id, in.TenantClaim, values(in.ACRValues), values(in.AMRValues), in.CertificateBound, actor))
	if err != nil {
		return p, fmt.Errorf("identity: update identity provider %s: %w", id, err)
	}
	return p, nil
}

// RetireIdentityProvider retires an active provider, guarded by ifMatch; its
// tokens authenticate no more. A tenant keeps one active provider at least:
// its active providers are read FOR UPDATE, so two retirements cannot each
// leave the other's as the last.
func RetireIdentityProvider(ctx context.Context, tx pgx.Tx, actor, id uuid.UUID, ifMatch func(int32) error) (IdentityProvider, error) {
	var active int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT FROM identity.identity_provider WHERE status = 'active' FOR UPDATE) a`).Scan(&active); err != nil {
		return IdentityProvider{}, fmt.Errorf("identity: lock active identity providers: %w", err)
	}
	p, err := activeProvider(ctx, tx, id, ifMatch)
	if err != nil {
		return p, err
	}
	if active < 2 {
		return p, problem.Conflict("identity.last_provider", "This is the tenant's last active identity provider; register its replacement first.")
	}
	p, err = scanProvider(tx.QueryRow(ctx, `UPDATE identity.identity_provider SET status = 'retired', retired_at = now(),
		updated_by = $2, version = version + 1 WHERE id = $1 RETURNING `+providerColumns, id, actor))
	if err != nil {
		return p, fmt.Errorf("identity: retire identity provider %s: %w", id, err)
	}
	return p, nil
}

// activeProvider reads a provider FOR UPDATE and checks it is active and
// matches ifMatch.
func activeProvider(ctx context.Context, tx pgx.Tx, id uuid.UUID, ifMatch func(int32) error) (IdentityProvider, error) {
	p, err := getProvider(ctx, tx, id, true)
	if err != nil {
		return p, err
	}
	if err := ifMatch(p.Version); err != nil {
		return p, err
	}
	if p.Status != ProviderActive {
		return p, problem.Conflict("identity.provider_retired", "The identity provider is retired; register a new one instead.")
	}
	return p, nil
}

// ReadAuthPolicy reads the tenant's policy, the defaults when it has none.
func ReadAuthPolicy(ctx context.Context, tx pgx.Tx) (AuthPolicy, error) {
	return readPolicy(ctx, tx, false)
}

const policyColumns = `mfa_required, extract(epoch FROM session_max_age)::bigint, extract(epoch FROM idle_timeout)::bigint`

func readPolicy(ctx context.Context, tx pgx.Tx, lock bool) (AuthPolicy, error) {
	sql := `SELECT ` + policyColumns + `, updated_by, version FROM identity.auth_policy`
	if lock {
		sql += ` FOR UPDATE`
	}
	var p AuthPolicy
	var age, idle int64
	err := tx.QueryRow(ctx, sql).Scan(&p.MFARequired, &age, &idle, &p.UpdatedBy, &p.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return AuthPolicy{SessionMaxAge: DefaultSessionMaxAge, IdleTimeout: DefaultIdleTimeout, Version: 1}, nil
	}
	if err != nil {
		return p, fmt.Errorf("identity: read authentication policy: %w", err)
	}
	p.SessionMaxAge, p.IdleTimeout = time.Duration(age)*time.Second, time.Duration(idle)*time.Second
	return p, nil
}

// UpdateAuthPolicy changes the tenant's policy, guarded by ifMatch, holding
// its row FOR UPDATE (see provable).
func UpdateAuthPolicy(ctx context.Context, tx pgx.Tx, actor uuid.UUID, ifMatch func(int32) error, ch AuthPolicyChange) (AuthPolicy, error) {
	if err := ensurePolicy(ctx, tx); err != nil {
		return AuthPolicy{}, err
	}
	p, err := readPolicy(ctx, tx, true)
	if err != nil {
		return p, err
	}
	if err := ifMatch(p.Version); err != nil {
		return p, err
	}
	p = changed(p, ch)
	if err := validPolicy(p); err != nil {
		return p, err
	}
	if p.MFARequired {
		if err := allProvable(ctx, tx); err != nil {
			return p, err
		}
	}
	var age, idle int64
	err = tx.QueryRow(ctx, `UPDATE identity.auth_policy SET mfa_required = $1, session_max_age = make_interval(secs => $2),
		idle_timeout = make_interval(secs => $3), updated_by = $4, version = version + 1
		RETURNING `+policyColumns+`, updated_by, version`, p.MFARequired, p.SessionMaxAge.Seconds(), p.IdleTimeout.Seconds(), actor).
		Scan(&p.MFARequired, &age, &idle, &p.UpdatedBy, &p.Version)
	if err != nil {
		return p, fmt.Errorf("identity: update authentication policy: %w", err)
	}
	p.SessionMaxAge, p.IdleTimeout = time.Duration(age)*time.Second, time.Duration(idle)*time.Second
	return p, nil
}

// ensurePolicy creates the tenant's policy row with the defaults when it has
// none, so there is a row to lock; a concurrent creation waits on it. The
// defaults are version 1, as the row they become is.
func ensurePolicy(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `INSERT INTO identity.auth_policy (tenant_id) VALUES (platform.current_tenant()) ON CONFLICT DO NOTHING`); err != nil {
		return fmt.Errorf("identity: create authentication policy: %w", err)
	}
	return nil
}

func changed(p AuthPolicy, ch AuthPolicyChange) AuthPolicy {
	if ch.MFARequired != nil {
		p.MFARequired = *ch.MFARequired
	}
	if ch.SessionMaxAge != nil {
		p.SessionMaxAge = *ch.SessionMaxAge
	}
	if ch.IdleTimeout != nil {
		p.IdleTimeout = *ch.IdleTimeout
	}
	return p
}

// provable refuses a provider that can prove multi-factor by neither acr
// nor amr values while the tenant's policy requires it. It holds the policy
// row FOR SHARE until commit, so the check and UpdateAuthPolicy's, which
// holds it FOR UPDATE, never both pass on what the other has not yet
// committed: whichever comes second waits and sees the first's change.
// Registering such a provider therefore creates the tenant's policy row
// with the defaults if it has none (updated_by NULL: never changed).
func provable(ctx context.Context, tx pgx.Tx, acr, amr []string) error {
	if len(acr)+len(amr) > 0 {
		return nil
	}
	if err := ensurePolicy(ctx, tx); err != nil {
		return err
	}
	var required bool
	if err := tx.QueryRow(ctx, `SELECT mfa_required FROM identity.auth_policy FOR SHARE`).Scan(&required); err != nil {
		return fmt.Errorf("identity: read authentication policy: %w", err)
	}
	if required {
		return mfaUnprovable()
	}
	return nil
}

// allProvable refuses requiring multi-factor while an active provider can
// prove it by neither acr nor amr values.
func allProvable(ctx context.Context, tx pgx.Tx) error {
	var unprovable bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM identity.identity_provider
		WHERE status = 'active' AND cardinality(acr_values) = 0 AND cardinality(amr_values) = 0)`).Scan(&unprovable); err != nil {
		return fmt.Errorf("identity: check identity providers prove multi-factor: %w", err)
	}
	if unprovable {
		return mfaUnprovable()
	}
	return nil
}

func mfaUnprovable() *problem.Problem {
	return problem.Unprocessable("identity.mfa_unprovable",
		"The tenant requires multi-factor authentication, and an active identity provider would prove it by neither acr nor amr values.")
}

// ResolveIdentityProvider finds the active registration holding the issuer
// with one of the token's audiences, across tenants, with its tenant's
// policy. tx runs as the control role (ADR-0031). A token whose audiences
// name two registrations is refused even when both are one tenant's: the
// registrations may require different proofs, and a token must say which
// one it is for.
func ResolveIdentityProvider(ctx context.Context, tx pgx.Tx, issuer string, audiences []string) (Registration, error) {
	rows, _ := tx.Query(ctx, `SELECT p.id, p.tenant_id, p.issuer, p.audience, p.tenant_claim, p.acr_values, p.amr_values, p.certificate_bound,
			coalesce(a.mfa_required, false), coalesce(extract(epoch FROM a.session_max_age)::bigint, $3),
			coalesce(extract(epoch FROM a.idle_timeout)::bigint, $4)
		FROM identity.identity_provider p LEFT JOIN identity.auth_policy a ON a.tenant_id = p.tenant_id
		WHERE p.issuer = $1 AND p.audience = ANY($2) AND p.status = 'active'`,
		issuer, audiences, int64(DefaultSessionMaxAge.Seconds()), int64(DefaultIdleTimeout.Seconds()))
	regs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Registration, error) {
		var r Registration
		var age, idle int64
		err := row.Scan(&r.ProviderID, &r.TenantID, &r.Issuer, &r.Audience, &r.TenantClaim, &r.ACRValues, &r.AMRValues, &r.CertificateBound,
			&r.Policy.MFARequired, &age, &idle)
		r.Policy.SessionMaxAge, r.Policy.IdleTimeout = time.Duration(age)*time.Second, time.Duration(idle)*time.Second
		return r, err //nolint:wrapcheck // wrapped below with the issuer
	})
	switch {
	case err != nil:
		return Registration{}, fmt.Errorf("identity: resolve identity provider %s: %w", issuer, err)
	case len(regs) == 0:
		return Registration{}, ErrUnknownProvider
	case len(regs) > 1:
		return Registration{}, ErrAmbiguousProvider
	}
	return regs[0], nil
}

// values stores absent values as an empty array, never NULL.
func values(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

var tenantClaim = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.:/-]{0,63}$`)

// ValidateProvider checks a registration as the database does, answering 422
// with the field instead of a constraint violation, and what the database
// cannot tell: an http issuer is allowed on loopback only, for a provider
// on a developer's machine. In production such an issuer is never fetched
// (authn.Providers allows https only and no loopback address, T37b), and
// the control plane refuses to register it there (T37c).
func ValidateProvider(in NewIdentityProvider) error {
	invalid := func(field, msg string) error {
		return problem.Unprocessable("identity.invalid_provider", "The identity provider is invalid.",
			problem.FieldError{Field: field, Code: "invalid", Message: msg})
	}
	if msg := issuerProblem(in.Issuer); msg != "" {
		return invalid("issuer", msg)
	}
	if in.Audience == "" || len(in.Audience) > 255 {
		return invalid("audience", "1 to 255 characters")
	}
	if in.TenantClaim != nil && !tenantClaim.MatchString(*in.TenantClaim) {
		return invalid("tenant_claim", "a claim name: a letter or _, then up to 63 letters, digits or _.:/-")
	}
	if !validValues(in.ACRValues, 255) {
		return invalid("acr_values", "at most 16 values of 1 to 255 printable ASCII characters without spaces")
	}
	if !validValues(in.AMRValues, 64) {
		return invalid("amr_values", "at most 16 values of 1 to 64 printable ASCII characters without spaces")
	}
	return nil
}

// issuerProblem says what is wrong with an issuer URL, or "".
func issuerProblem(issuer string) string {
	const want = "an https URL of at most 512 characters, without credentials, query or fragment (http on loopback only)"
	u, err := url.Parse(issuer)
	if err != nil || len(issuer) > 512 || strings.ContainsAny(issuer, " \t\r\n") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(issuer, "#") {
		return want
	}
	switch u.Scheme {
	case "https":
		return ""
	case "http":
		if loopback(u.Hostname()) {
			return ""
		}
	}
	return want
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validValues(vs []string, maxLen int) bool {
	if len(vs) > 16 {
		return false
	}
	for _, v := range vs {
		if v == "" || len(v) > maxLen || strings.IndexFunc(v, func(r rune) bool { return r <= ' ' || r > '~' }) >= 0 {
			return false // printable ASCII without spaces: acr_values is a space-separated list (RFC 9470)
		}
	}
	return true
}

// validPolicy checks the policy's bounds as the database does, answering
// 422 with the field.
func validPolicy(p AuthPolicy) error {
	invalid := func(field, msg string) error {
		return problem.Unprocessable("identity.invalid_policy", "The authentication policy is invalid.",
			problem.FieldError{Field: field, Code: "out_of_range", Message: msg})
	}
	if p.SessionMaxAge < MinSessionMaxAge || p.SessionMaxAge > MaxSessionMaxAge || p.SessionMaxAge%time.Second != 0 {
		return invalid("session_max_age", "whole seconds from 15 minutes to 24 hours")
	}
	if p.IdleTimeout < MinIdleTimeout || p.IdleTimeout > MaxIdleTimeout || p.IdleTimeout%time.Second != 0 || p.IdleTimeout > p.SessionMaxAge {
		return invalid("idle_timeout", "whole seconds from 5 minutes to 8 hours, at most the session lifetime")
	}
	return nil
}

// ProviderKey is what decides a token's tenant: an active provider's issuer
// and audience, and its tenant.
type ProviderKey struct {
	ID       uuid.UUID
	TenantID uuid.UUID
	Issuer   string
	Audience string
}

// ActiveIdentityProviders lists every tenant's active providers, across
// tenants: tx runs as the control role, for the cell's claims in the global
// directory (ADR-0031).
func ActiveIdentityProviders(ctx context.Context, tx pgx.Tx) ([]ProviderKey, error) {
	rows, _ := tx.Query(ctx, `SELECT id, tenant_id, issuer, audience FROM identity.identity_provider WHERE status = 'active' ORDER BY tenant_id, issuer, audience`)
	keys, err := pgx.CollectRows(rows, pgx.RowToStructByPos[ProviderKey])
	if err != nil {
		return nil, fmt.Errorf("identity: list active identity providers: %w", err)
	}
	return keys, nil
}

// Registry resolves a token's issuer and audiences to the cell's active
// registration holding them, as the control role across tenants: the
// cell's authn.Registry (ADR-0031), behind an authn.RegistryCache.
type Registry struct {
	Pool        *database.Pool
	ControlRole string // erp_control
	// Begin starts the transaction; nil means Pool.Begin. Tests inject
	// failures.
	Begin database.BeginFunc
}

// Resolve implements authn.Registry.
func (r Registry) Resolve(ctx context.Context, issuer string, audiences []string) (authn.Registration, error) {
	tx, err := database.BeginOr(ctx, r.Pool, r.Begin, database.Scope{Role: r.ControlRole, ReadOnly: true})
	if err != nil {
		return authn.Registration{}, fmt.Errorf("identity: resolve identity provider %s: %w", issuer, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	reg, err := ResolveIdentityProvider(ctx, tx, issuer, audiences)
	switch {
	case errors.Is(err, ErrUnknownProvider):
		return authn.Registration{}, authn.ErrUnknownIssuer
	case errors.Is(err, ErrAmbiguousProvider):
		return authn.Registration{}, authn.ErrAmbiguousIssuer
	case err != nil:
		return authn.Registration{}, err
	}
	return authn.Registration{ProviderID: reg.ProviderID, TenantID: reg.TenantID, Issuer: reg.Issuer, Audience: reg.Audience, TenantClaim: reg.TenantClaim,
		MFARequired: reg.Policy.MFARequired, ACRValues: reg.ACRValues, AMRValues: reg.AMRValues,
		SessionMaxAge: reg.Policy.SessionMaxAge, IdleTimeout: reg.Policy.IdleTimeout}, nil
}

// RegisteredEvent is the IdentityProviderRegistered event of a
// registration, for whoever registers it (the control plane, provisioning).
func RegisteredEvent(p IdentityProvider) outbox.Event {
	return outbox.Event{Context: "identity", Name: "identity_provider_registered", AggregateType: "identity.identity_provider", AggregateID: p.ID,
		Payload: map[string]any{"provider_id": p.ID, "issuer": p.Issuer, "audience": p.Audience, "tenant_claim": p.TenantClaim, "acr_values": p.ACRValues, "amr_values": p.AMRValues}}
}

// RetiredEvent is the IdentityProviderRetired event of a retirement.
func RetiredEvent(p IdentityProvider) outbox.Event {
	return outbox.Event{Context: "identity", Name: "identity_provider_retired", AggregateType: "identity.identity_provider", AggregateID: p.ID,
		Payload: map[string]any{"provider_id": p.ID, "issuer": p.Issuer, "audience": p.Audience, "retired_at": p.RetiredAt}}
}

// ProviderEntry is the audit entry of a change of the provider (verb:
// register, update, retire); the caller adds who made it.
func ProviderEntry(verb string, p IdentityProvider) audit.Entry {
	return audit.Entry{Action: "identity.identity_provider." + verb, ObjectType: "identity.identity_provider", ObjectID: p.ID,
		Detail: map[string]any{"issuer": p.Issuer, "audience": p.Audience, "tenant_claim": p.TenantClaim, "acr_values": p.ACRValues, "amr_values": p.AMRValues,
			"certificate_bound": p.CertificateBound}}
}

// ProductionIssuer refuses an issuer other than https where production
// runs: http on loopback is for a provider on a developer's machine.
func ProductionIssuer(issuer string) error {
	if strings.HasPrefix(issuer, "https://") {
		return nil
	}
	return problem.Unprocessable("identity.invalid_provider", "The identity provider is invalid.",
		problem.FieldError{Field: "issuer", Code: "invalid", Message: "an https URL in production"})
}

// ProviderUnreachable is the answer for a provider whose discovery document
// could not be read when it was registered.
func ProviderUnreachable(err error) error {
	return problem.Unprocessable("identity.provider_unreachable", "The identity provider's discovery document could not be read.",
		problem.FieldError{Field: "issuer", Code: "unreachable", Message: "an issuer whose /.well-known/openid-configuration the cell can read"}).WithCause(err)
}

// RequireActiveProvider refuses a tenant without an active identity
// provider, in tx in the tenant's scope: its people could not sign in. A
// production cell checks it before a tenant comes in by an import.
func RequireActiveProvider(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) error {
	var has bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM identity.identity_provider WHERE tenant_id = $1 AND status = 'active')`, tenant).Scan(&has); err != nil {
		return fmt.Errorf("identity: read the identity providers of tenant %s: %w", tenant, err)
	}
	if !has {
		return problem.Unprocessable("identity.provider_required",
			"The tenant has no active identity provider, so its people could not sign in; register one where it comes from first.")
	}
	return nil
}
