// Package authn authenticates requests: it verifies the OIDC bearer token
// against a trusted issuer (signature, issuer, audience, expiry) and reads
// the tenant from the token (ADR-0010, ADR-0014). Principal resolution needs
// the tenant's identity data and happens in the unit of work. Providers
// discovers the tenants' own identity providers lazily and caches their
// keys (ADR-0031).
package authn

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// DefaultTenantClaim names the claim that carries the tenant id.
const DefaultTenantClaim = "tenant_id"

// DefaultRolesClaim names the claim that carries an operator's roles, a
// JSON array of strings or a space-separated string.
const DefaultRolesClaim = "roles"

// Operator roles (ADR-0027). A token without a roles claim holds every
// role in development and none in production (or any other environment);
// one with the claim holds what it lists (nothing when the claim is
// malformed), and RoleAdmin implies all.
const (
	RoleRead      = "erp:read"      // list and read tenants, cells, entries
	RoleProvision = "erp:provision" // provision tenants
	RoleOperate   = "erp:operate"   // suspend, resume, offboard, erase, rotate keys, freeze, move
	RoleAdmin     = "erp:admin"     // everything
)

// OperatorAllowed reports whether the operator's roles permit an operation
// that requires role. Nil roles mean the token had no roles claim and the
// verifier runs in development; the verifier never returns nil for a claim
// that is present, nor for an absent one outside development.
func OperatorAllowed(c reqctx.Claims, role string) bool {
	if c.Roles == nil {
		return true
	}
	for _, r := range c.Roles {
		if r == role || r == RoleAdmin {
			return true
		}
	}
	return false
}

// Issuer is a trusted identity provider.
type Issuer struct {
	URL         string `json:"issuer"`
	Audience    string `json:"audience"`
	TenantClaim string `json:"tenant_claim"` // default DefaultTenantClaim
	RolesClaim  string `json:"roles_claim"`  // operator issuers: the claim carrying the operator's roles (default DefaultRolesClaim)
}

type issuer struct {
	verifier    *oidc.IDTokenVerifier
	tenantClaim string
	rolesClaim  string
}

// Environment is the deployment's environment as ERP_ENV names it
// (internal/platform/config). It decides what an operator token without a
// roles claim holds (ADR-0027).
type Environment string

// The environments.
const (
	Development Environment = "development"
	Production  Environment = "production"
)

// Verifier checks bearer tokens from the trusted issuers.
type Verifier struct {
	issuers map[string]issuer
	env     Environment
}

// Static is an issuer trusted with known keys instead of discovery (the
// development issuer).
type Static struct {
	Issuer Issuer
	Keys   oidc.KeySet
}

// New discovers each issuer's configuration and keys endpoint, and trusts
// the static issuers as given, for a deployment in env.
func New(ctx context.Context, env Environment, issuers []Issuer, static ...Static) (*Verifier, error) {
	if len(issuers)+len(static) == 0 {
		return nil, errors.New("authn: at least one trusted issuer is required")
	}
	v := &Verifier{issuers: map[string]issuer{}, env: env}
	for _, s := range static {
		v.issuers[s.Issuer.URL] = issuer{verifier: oidc.NewVerifier(s.Issuer.URL, s.Keys, &oidc.Config{ClientID: s.Issuer.Audience}), tenantClaim: DefaultTenantClaim, rolesClaim: DefaultRolesClaim}
	}
	for _, is := range issuers {
		if is.Audience == "" {
			return nil, fmt.Errorf("authn: issuer %s: audience is required", is.URL)
		}
		provider, err := oidc.NewProvider(ctx, is.URL)
		if err != nil {
			return nil, fmt.Errorf("authn: issuer %s: %w", is.URL, err)
		}
		claim, roles := is.TenantClaim, is.RolesClaim
		if claim == "" {
			claim = DefaultTenantClaim
		}
		if roles == "" {
			roles = DefaultRolesClaim
		}
		v.issuers[is.URL] = issuer{
			verifier:    provider.Verifier(&oidc.Config{ClientID: is.Audience}),
			tenantClaim: claim,
			rolesClaim:  roles,
		}
	}
	return v, nil
}

// Verify checks a raw token of a tenant's principal and returns its claims.
// Failures are 401 problems that do not say which check failed beyond a
// short reason.
func (v *Verifier) Verify(ctx context.Context, raw string) (reqctx.Claims, error) {
	claims, s, err := v.verify(ctx, raw)
	if err != nil {
		return reqctx.Claims{}, err
	}
	tenant, err := uuid.Parse(s)
	if err != nil {
		return reqctx.Claims{}, problem.Unauthorized("The token does not name a tenant.").WithCause(err)
	}
	claims.TenantID = tenant
	return claims, nil
}

// VerifyOperator checks a raw token of a platform operator: the same checks
// as Verify, from a verifier built on the operator issuers, but the token
// must name no tenant, so a tenant's token is never an operator's.
func (v *Verifier) VerifyOperator(ctx context.Context, raw string) (reqctx.Claims, error) {
	claims, s, err := v.verify(ctx, raw)
	if err != nil {
		return reqctx.Claims{}, err
	}
	if s != "" {
		return reqctx.Claims{}, problem.Unauthorized("An operator token must not name a tenant.")
	}
	return claims, nil
}

// verify checks signature, issuer, audience and expiry, and returns the
// claims without a tenant and the tenant claim as the token carries it.
func (v *Verifier) verify(ctx context.Context, raw string) (reqctx.Claims, string, error) {
	iss, err := unverifiedIssuer(raw)
	if err != nil {
		return reqctx.Claims{}, "", problem.Unauthorized("The bearer token is malformed.").WithCause(err)
	}
	is, ok := v.issuers[iss]
	if !ok {
		return reqctx.Claims{}, "", problem.Unauthorized("The token's issuer is not trusted.")
	}
	tok, err := is.verifier.Verify(ctx, raw)
	if err != nil {
		return reqctx.Claims{}, "", problem.Unauthorized("The bearer token is invalid or expired.").WithCause(err)
	}
	var claims map[string]any
	_ = tok.Claims(&claims) // the payload was decoded during verification
	s, _ := claims[is.tenantClaim].(string)
	var rs []string
	if raw, present := claims[is.rolesClaim]; present {
		rs = roles(raw)
	} else if v.env != Development {
		// Outside development an absent claim holds no role (ADR-0027,
		// amended): a production issuer that stops emitting roles must not
		// widen access. Nil, every role, is development's alone.
		rs = []string{}
	}
	return reqctx.Claims{Issuer: iss, Subject: tok.Subject, Roles: rs}, s, nil
}

// roles reads a present roles claim: a JSON array of strings, or one string
// of space-separated roles (the scope style). Only an absent claim means
// every role (ADR-0027); a malformed one (null, a number, an object, an
// array holding anything but strings) fails closed as no role at all, never
// nil, so a broken issuer cannot widen access.
func roles(v any) []string {
	out := []string{}
	switch c := v.(type) {
	case []any:
		for _, r := range c {
			s, ok := r.(string)
			if !ok {
				return []string{}
			}
			if s != "" {
				out = append(out, s)
			}
		}
	case string:
		out = append(out, strings.Fields(c)...)
	}
	return out
}

// unverifiedIssuer reads `iss` to choose the verifier; nothing else is
// trusted before the signature is checked.
func unverifiedIssuer(raw string) (string, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", errors.New("not a JWS compact token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("authn: decode token payload: %w", err)
	}
	var c struct {
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return "", fmt.Errorf("authn: parse token payload: %w", err)
	}
	return c.Iss, nil
}

// Middleware authenticates every request as a tenant's principal and puts
// the claims in its context. Requests without a valid bearer token get 401.
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return middleware(v.Verify, next)
}

// OperatorMiddleware authenticates every request as a platform operator
// (VerifyOperator) and puts the claims in its context.
func (v *Verifier) OperatorMiddleware(next http.Handler) http.Handler {
	return middleware(v.VerifyOperator, next)
}

func middleware(verify func(context.Context, string) (reqctx.Claims, error), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, raw, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || raw == "" {
			problem.Write(w, r, problem.Unauthorized("A bearer token is required."))
			return
		}
		claims, err := verify(r.Context(), raw)
		if err != nil {
			problem.Write(w, r, problem.From(err))
			return
		}
		next.ServeHTTP(w, r.WithContext(reqctx.WithClaims(r.Context(), claims)))
	})
}
