// Package authn authenticates requests: it verifies the OIDC bearer token
// against a trusted issuer (signature, issuer, audience, expiry) and reads
// the tenant from the token (ADR-0010, ADR-0014). Principal resolution needs
// the tenant's identity data and happens in the unit of work.
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

// Issuer is a trusted identity provider.
type Issuer struct {
	URL         string `json:"issuer"`
	Audience    string `json:"audience"`
	TenantClaim string `json:"tenant_claim"` // default DefaultTenantClaim
}

type issuer struct {
	verifier    *oidc.IDTokenVerifier
	tenantClaim string
}

// Verifier checks bearer tokens from the trusted issuers.
type Verifier struct {
	issuers map[string]issuer
}

// Static is an issuer trusted with known keys instead of discovery (the
// development issuer).
type Static struct {
	Issuer Issuer
	Keys   oidc.KeySet
}

// New discovers each issuer's configuration and keys endpoint, and trusts
// the static issuers as given.
func New(ctx context.Context, issuers []Issuer, static ...Static) (*Verifier, error) {
	if len(issuers)+len(static) == 0 {
		return nil, errors.New("authn: at least one trusted issuer is required")
	}
	v := &Verifier{issuers: map[string]issuer{}}
	for _, s := range static {
		v.issuers[s.Issuer.URL] = issuer{verifier: oidc.NewVerifier(s.Issuer.URL, s.Keys, &oidc.Config{ClientID: s.Issuer.Audience}), tenantClaim: DefaultTenantClaim}
	}
	for _, is := range issuers {
		if is.Audience == "" {
			return nil, fmt.Errorf("authn: issuer %s: audience is required", is.URL)
		}
		provider, err := oidc.NewProvider(ctx, is.URL)
		if err != nil {
			return nil, fmt.Errorf("authn: issuer %s: %w", is.URL, err)
		}
		claim := is.TenantClaim
		if claim == "" {
			claim = DefaultTenantClaim
		}
		v.issuers[is.URL] = issuer{
			verifier:    provider.Verifier(&oidc.Config{ClientID: is.Audience}),
			tenantClaim: claim,
		}
	}
	return v, nil
}

// Verify checks a raw token and returns its claims. Failures are 401 problems
// that do not say which check failed beyond a short reason.
func (v *Verifier) Verify(ctx context.Context, raw string) (reqctx.Claims, error) {
	iss, err := unverifiedIssuer(raw)
	if err != nil {
		return reqctx.Claims{}, problem.Unauthorized("The bearer token is malformed.").WithCause(err)
	}
	is, ok := v.issuers[iss]
	if !ok {
		return reqctx.Claims{}, problem.Unauthorized("The token's issuer is not trusted.")
	}
	tok, err := is.verifier.Verify(ctx, raw)
	if err != nil {
		return reqctx.Claims{}, problem.Unauthorized("The bearer token is invalid or expired.").WithCause(err)
	}
	var claims map[string]any
	_ = tok.Claims(&claims) // the payload was decoded during verification
	s, _ := claims[is.tenantClaim].(string)
	tenant, err := uuid.Parse(s)
	if err != nil {
		return reqctx.Claims{}, problem.Unauthorized("The token does not name a tenant.").WithCause(err)
	}
	return reqctx.Claims{Issuer: iss, Subject: tok.Subject, TenantID: tenant}, nil
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
		return "", err
	}
	var c struct {
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return "", err
	}
	return c.Iss, nil
}

// Middleware authenticates every request and puts the claims in its
// context. Requests without a valid bearer token get 401.
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, raw, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || raw == "" {
			problem.Write(w, r, problem.Unauthorized("A bearer token is required."))
			return
		}
		claims, err := v.Verify(r.Context(), raw)
		if err != nil {
			problem.Write(w, r, problem.From(err))
			return
		}
		next.ServeHTTP(w, r.WithContext(reqctx.WithClaims(r.Context(), claims)))
	})
}
