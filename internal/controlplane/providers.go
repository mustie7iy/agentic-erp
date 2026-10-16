package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
	"github.com/mustie7iy/agentic-erp/internal/provisioning"
)

// A tenant's identity providers and authentication policy (ADR-0031): the
// operators administer them here, in the tenant's scope as the application
// role, like a status change. Each change is recorded in the tenant's audit
// trail as the operator; registering and retiring a provider and changing
// the policy are published on the identity channel. With a global
// directory, a provider's issuer and audience are claimed there before the
// registration commits, so they belong to one tenant across cells.

// IdentityProvider is the IdentityProvider schema of the contract.
type IdentityProvider struct {
	ID          uuid.UUID  `json:"id"`
	TenantID    uuid.UUID  `json:"tenant_id"`
	Issuer      string     `json:"issuer"`
	Audience    string     `json:"audience"`
	TenantClaim *string    `json:"tenant_claim"`
	ACRValues   []string   `json:"acr_values"`
	AMRValues   []string   `json:"amr_values"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	RetiredAt   *time.Time `json:"retired_at"`
	Version     int32      `json:"version"`
}

func providerView(p identity.IdentityProvider) IdentityProvider {
	return IdentityProvider{ID: p.ID, TenantID: p.TenantID, Issuer: p.Issuer, Audience: p.Audience, TenantClaim: p.TenantClaim,
		ACRValues: p.ACRValues, AMRValues: p.AMRValues, Status: p.Status, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
		RetiredAt: p.RetiredAt, Version: p.Version}
}

// AuthPolicy is the AuthPolicy schema of the contract.
type AuthPolicy struct {
	TenantID      uuid.UUID `json:"tenant_id"`
	MFARequired   bool      `json:"mfa_required"`
	SessionMaxAge int64     `json:"session_max_age_seconds"`
	IdleTimeout   int64     `json:"idle_timeout_seconds"`
	Version       int32     `json:"version"`
}

func policyView(tenant uuid.UUID, p identity.AuthPolicy) AuthPolicy {
	return AuthPolicy{TenantID: tenant, MFARequired: p.MFARequired, SessionMaxAge: int64(p.SessionMaxAge / time.Second),
		IdleTimeout: int64(p.IdleTimeout / time.Second), Version: p.Version}
}

// DefaultWriteTimeout bounds a write's transaction (PostgreSQL's
// transaction_timeout): a registration claims its issuer in the directory
// before it commits, and a round leaves a claim alone for
// directory.ClaimGrace, so the registration must commit or fail well
// within it.
const DefaultWriteTimeout = time.Minute

func (s *Service) writeTimeout() time.Duration {
	if s.WriteTimeout > 0 {
		return s.WriteTimeout
	}
	return DefaultWriteTimeout
}

// inTenant runs fn in the tenant's scope as the application role; a tenant
// the cell does not have is 404. Without an operator the call reads and
// commits nothing. With one it writes as the operator, within the write timeout,
// holding the tenant's row so offboarding cannot start meanwhile; once
// offboarding has started the registry and policy no longer change (409
// tenancy.invalid_transition), while a suspended tenant's still do.
func (s *Service) inTenant(ctx context.Context, tenant uuid.UUID, op *Operator, fn func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := s.begin(ctx, database.Scope{Role: s.AppRole, TenantID: tenant, ReadOnly: op == nil})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	t, err := read(ctx, tx, tenant, op != nil)
	if err != nil {
		return err
	}
	if op == nil {
		return fn(ctx, tx)
	}
	if t.Status == Offboarded {
		return problem.Conflict("tenancy.invalid_transition", "The tenant is offboarded; its identity providers and authentication policy no longer change.")
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('transaction_timeout', $1, true)`, fmt.Sprintf("%dms", s.writeTimeout().Milliseconds())); err != nil {
		return fmt.Errorf("controlplane: bound the transaction in tenant %s: %w", tenant, err)
	}
	if err := database.SetActor(ctx, tx, op.ID); err != nil {
		return err
	}
	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("controlplane: commit in tenant %s: %w", tenant, err)
	}
	return nil
}

// change is one recorded change: the audit entry, and the event when it is
// published.
type change struct {
	entry audit.Entry
	event *outbox.Event
}

// record writes the change as the operator: the actor of the row triggers,
// the event, and the audit entry naming the operator.
func (s *Service) record(ctx context.Context, tx pgx.Tx, op Operator, tenant uuid.UUID, c change) error {
	trace := reqctx.TraceID(ctx)
	if c.event != nil {
		if _, err := outbox.Write(ctx, tx, tenant, op.ID, trace, *c.event); err != nil {
			return err
		}
	}
	c.entry.Outcome = "succeeded"
	c.entry.Detail["operator"], c.entry.Detail["operator_issuer"] = op.Subject, op.Issuer
	return audit.Write(ctx, tx, reqctx.Principal{ID: op.ID, Kind: reqctx.KindSystem, TenantID: tenant}, trace, s.now(), c.entry)
}

// ListProviders lists the tenant's identity providers.
func (s *Service) ListProviders(ctx context.Context, tenant uuid.UUID) ([]IdentityProvider, error) {
	out := []IdentityProvider{}
	err := s.inTenant(ctx, tenant, nil, func(ctx context.Context, tx pgx.Tx) error {
		ps, err := identity.ListIdentityProviders(ctx, tx)
		for _, p := range ps {
			out = append(out, providerView(p))
		}
		return err
	})
	return out, err
}

// GetProvider reads one identity provider of the tenant.
func (s *Service) GetProvider(ctx context.Context, tenant, id uuid.UUID) (IdentityProvider, error) {
	var out IdentityProvider
	err := s.inTenant(ctx, tenant, nil, func(ctx context.Context, tx pgx.Tx) error {
		p, err := identity.GetIdentityProvider(ctx, tx, id)
		out = providerView(p)
		return err
	})
	return out, err
}

// RegisterProvider registers an identity provider of the tenant; with key,
// a retry answers the provider the first request registered (replayed).
// In production the issuer is https.
func (s *Service) RegisterProvider(ctx context.Context, op Operator, tenant uuid.UUID, in identity.NewIdentityProvider, key *provisioning.Idempotency) (IdentityProvider, bool, error) {
	if s.Production && !strings.HasPrefix(in.Issuer, "https://") {
		return IdentityProvider{}, false, problem.Unprocessable("identity.invalid_provider", "The identity provider is invalid.",
			problem.FieldError{Field: "issuer", Code: "invalid", Message: "an https URL in production"})
	}
	var out IdentityProvider
	replayed := false
	err := s.inTenant(ctx, tenant, &op, func(ctx context.Context, tx pgx.Tx) error {
		stored, found, err := s.claimKey(ctx, tx, op, tenant, key)
		if err != nil || found {
			out, replayed = stored, found
			return err
		}
		p, err := identity.RegisterIdentityProvider(ctx, tx, op.ID, in)
		if err != nil {
			return err
		}
		if err := s.claim(ctx, tenant, p); err != nil {
			return err
		}
		out = providerView(p)
		event := identity.RegisteredEvent(p)
		if err := s.record(ctx, tx, op, tenant, change{event: &event, entry: identity.ProviderEntry("register", p)}); err != nil {
			return err
		}
		return s.storeProvider(ctx, tx, tenant, key, out)
	})
	return out, replayed, err
}

func (s *Service) claim(ctx context.Context, tenant uuid.UUID, p identity.IdentityProvider) error {
	if s.Directory == nil {
		return nil
	}
	return s.Directory.ClaimIssuer(ctx, tenant, p.ID, p.Issuer, p.Audience)
}

// claimKey claims the registration's key before anything else, as the unit
// of work does (uow): a concurrent request with the same key waits for this
// one, then replays it. A key past its expiry is claimed afresh, as for
// provisioning. found reports a key already holding an answer: the provider
// the first request registered. The key is the operator's, for the same
// request; anything else is 422.
func (s *Service) claimKey(ctx context.Context, tx pgx.Tx, op Operator, tenant uuid.UUID, key *provisioning.Idempotency) (IdentityProvider, bool, error) {
	if key == nil {
		return IdentityProvider{}, false, nil
	}
	var fresh bool
	err := tx.QueryRow(ctx, `INSERT INTO platform.idempotency_key AS k (tenant_id, key, principal_id, request_hash, status, expires_at)
		VALUES ($1, $2, $3, $4, 'in_progress', $5)
		ON CONFLICT (tenant_id, key) DO UPDATE SET principal_id = EXCLUDED.principal_id, request_hash = EXCLUDED.request_hash,
			status = 'in_progress', response_code = NULL, response_body = NULL, response_headers = '{}', created_at = now(), expires_at = EXCLUDED.expires_at
		WHERE k.expires_at < now()
		RETURNING true`, tenant, provisioning.KeyPrefix+key.Key, op.ID, key.RequestHash, s.now().Add(uow.Retention)).Scan(&fresh)
	if err == nil {
		return IdentityProvider{}, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return IdentityProvider{}, false, fmt.Errorf("controlplane: claim idempotency key of tenant %s: %w", tenant, err)
	}
	var owner uuid.UUID
	var hash, body []byte
	if err := tx.QueryRow(ctx, `SELECT principal_id, request_hash, response_body::text FROM platform.idempotency_key WHERE tenant_id = $1 AND key = $2`,
		tenant, provisioning.KeyPrefix+key.Key).Scan(&owner, &hash, &body); err != nil {
		return IdentityProvider{}, false, fmt.Errorf("controlplane: read idempotency key of tenant %s: %w", tenant, err)
	}
	if owner != op.ID || !bytes.Equal(hash, key.RequestHash) {
		return IdentityProvider{}, false, problem.Unprocessable("request.idempotency_key_reused", "This Idempotency-Key was already used for a different request.")
	}
	var p IdentityProvider
	_ = json.Unmarshal(body, &p) // the body was stored by storeProvider from this type
	return p, true, nil
}

// storeProvider keeps the registration's answer with its key, in its
// transaction, for a retry to replay.
func (s *Service) storeProvider(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, key *provisioning.Idempotency, p IdentityProvider) error {
	if key == nil {
		return nil
	}
	body, _ := json.Marshal(p) // ids, strings, lists and times
	if _, err := tx.Exec(ctx, `UPDATE platform.idempotency_key SET status = 'completed', response_code = 201, response_body = $3
		WHERE tenant_id = $1 AND key = $2`, tenant, provisioning.KeyPrefix+key.Key, body); err != nil {
		return fmt.Errorf("controlplane: store idempotency key of tenant %s: %w", tenant, err)
	}
	return nil
}

// UpdateProvider changes an active provider, guarded by If-Match.
func (s *Service) UpdateProvider(ctx context.Context, op Operator, tenant, id uuid.UUID, ifMatch string, ch identity.IdentityProviderChange) (IdentityProvider, error) {
	var out IdentityProvider
	err := s.inTenant(ctx, tenant, &op, func(ctx context.Context, tx pgx.Tx) error {
		p, err := identity.UpdateIdentityProvider(ctx, tx, op.ID, id, etag.IfMatch(ifMatch), ch)
		if err != nil {
			return err
		}
		out = providerView(p)
		return s.record(ctx, tx, op, tenant, change{entry: identity.ProviderEntry("update", p)})
	})
	return out, err
}

// RetireProvider retires an active provider, guarded by If-Match, and then
// gives its claim in the directory up.
func (s *Service) RetireProvider(ctx context.Context, op Operator, tenant, id uuid.UUID, ifMatch string) (IdentityProvider, error) {
	var out IdentityProvider
	err := s.inTenant(ctx, tenant, &op, func(ctx context.Context, tx pgx.Tx) error {
		p, err := identity.RetireIdentityProvider(ctx, tx, op.ID, id, etag.IfMatch(ifMatch))
		if err != nil {
			return err
		}
		out = providerView(p)
		event := identity.RetiredEvent(p)
		return s.record(ctx, tx, op, tenant, change{event: &event, entry: identity.ProviderEntry("retire", p)})
	})
	if err == nil && s.Directory != nil {
		s.Directory.ReleaseIssuer(ctx, tenant, out.ID, out.Issuer, out.Audience)
	}
	return out, err
}

// AuthPolicy reads the tenant's authentication policy.
func (s *Service) AuthPolicy(ctx context.Context, tenant uuid.UUID) (AuthPolicy, error) {
	var out AuthPolicy
	err := s.inTenant(ctx, tenant, nil, func(ctx context.Context, tx pgx.Tx) error {
		p, err := identity.ReadAuthPolicy(ctx, tx)
		out = policyView(tenant, p)
		return err
	})
	return out, err
}

// UpdateAuthPolicy changes the tenant's authentication policy, guarded by
// If-Match.
func (s *Service) UpdateAuthPolicy(ctx context.Context, op Operator, tenant uuid.UUID, ifMatch string, ch identity.AuthPolicyChange) (AuthPolicy, error) {
	var out AuthPolicy
	err := s.inTenant(ctx, tenant, &op, func(ctx context.Context, tx pgx.Tx) error {
		p, err := identity.UpdateAuthPolicy(ctx, tx, op.ID, etag.IfMatch(ifMatch), ch)
		if err != nil {
			return err
		}
		out = policyView(tenant, p)
		facts := map[string]any{"mfa_required": out.MFARequired, "session_max_age_seconds": out.SessionMaxAge, "idle_timeout_seconds": out.IdleTimeout}
		event := outbox.Event{Context: "identity", Name: "auth_policy_changed", AggregateType: "identity.auth_policy", AggregateID: tenant, Payload: facts}
		entry := audit.Entry{Action: "identity.auth_policy.update", ObjectType: "identity.auth_policy", ObjectID: tenant, Detail: map[string]any{}}
		for k, v := range facts {
			entry.Detail[k] = v
		}
		return s.record(ctx, tx, op, tenant, change{event: &event, entry: entry})
	})
	return out, err
}
