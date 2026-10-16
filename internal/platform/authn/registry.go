package authn

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Registration is a tenant's identity provider as the verifier trusts it
// (ADR-0031): its issuer and audience decide the tenant.
type Registration struct {
	ProviderID  uuid.UUID
	TenantID    uuid.UUID
	Issuer      string
	Audience    string
	TenantClaim *string // a claim that must carry the tenant's id; nil: none
}

// The registry's answers for a token no registration, or more than one,
// holds; anything else it returns is a failure to answer.
var (
	ErrUnknownIssuer   = errors.New("authn: no registered identity provider holds the token's issuer and audience")
	ErrAmbiguousIssuer = errors.New("authn: the token's audiences name more than one registered identity provider")
)

// Registry resolves a token's issuer and audiences to the one active
// registration holding them: the cell's identity providers, or the global
// directory's claims for the router.
type Registry interface {
	Resolve(ctx context.Context, issuer string, audiences []string) (Registration, error)
}

// verifyRegistered verifies a token of an issuer the deployment does not
// configure: the registration its issuer and audience resolve to decides
// the tenant, its provider's keys the signature, and a registered tenant
// claim must name that tenant. Only a resolved issuer is ever fetched.
func (v *Verifier) verifyRegistered(ctx context.Context, raw, iss string, auds []string) (reqctx.Claims, string, error) {
	if v.registry == nil || !plausible(iss, auds) {
		return reqctx.Claims{}, "", problem.Unauthorized("The token's issuer is not trusted.")
	}
	reg, err := v.registry.Resolve(ctx, iss, auds)
	switch {
	case errors.Is(err, ErrUnknownIssuer), errors.Is(err, ErrAmbiguousIssuer):
		return reqctx.Claims{}, "", problem.Unauthorized("The token's issuer is not trusted.").WithCause(err)
	case err != nil:
		return reqctx.Claims{}, "", problem.Unavailable("The identity provider registry could not be read; try again shortly.", 1).WithCause(err)
	}
	tok, err := v.providers.Verify(ctx, reg.Issuer, reg.Audience, raw)
	if err != nil {
		return reqctx.Claims{}, "", err
	}
	var claims map[string]any
	_ = tok.Claims(&claims) // the payload was decoded during verification
	if reg.TenantClaim != nil {
		if named, _ := claims[*reg.TenantClaim].(string); named != reg.TenantID.String() {
			return reqctx.Claims{}, "", problem.Unauthorized("The token names another tenant than its identity provider's.")
		}
	}
	// A registered provider's tokens are a tenant's principals', never an
	// operator's: they hold no operator role.
	c := reqctx.Claims{Issuer: reg.Issuer, Subject: tok.Subject, ProviderID: reg.ProviderID, Roles: []string{}}
	facts(&c, tok, claims)
	return c, reg.TenantID.String(), nil
}

// The bounds of what a registration can hold (migration 000056): a token
// whose issuer or audiences exceed them is refused before any lookup, since
// they are not yet verified and a client could otherwise have the server
// look up, and cache, whatever it sends.
var registrableIssuer = regexp.MustCompile(`^https?://[^\s/?#]+(/[^\s?#]*)?$`)

const (
	maxIssuer         = 512
	maxAudience       = 255
	maxTokenAudiences = 8
)

func plausible(iss string, auds []string) bool {
	if len(iss) > maxIssuer || !registrableIssuer.MatchString(iss) || len(auds) == 0 || len(auds) > maxTokenAudiences {
		return false
	}
	for _, a := range auds {
		if a == "" || len(a) > maxAudience {
			return false
		}
	}
	return true
}

// facts reads the token's facts that multi-factor and sessions are
// decided on; absent or malformed claims leave them empty.
func facts(c *reqctx.Claims, tok *oidc.IDToken, claims map[string]any) {
	c.IssuedAt = tok.IssuedAt
	var at struct {
		AuthTime json.RawMessage `json:"auth_time"`
	}
	_ = tok.Claims(&at) // the payload was decoded during verification
	if secs, err := strconv.ParseInt(string(at.AuthTime), 10, 64); err == nil {
		c.AuthTime = time.Unix(secs, 0)
	}
	c.ACR, _ = claims["acr"].(string)
	c.SessionID, _ = claims["sid"].(string)
	if amr, ok := claims["amr"].([]any); ok {
		for _, m := range amr {
			s, ok := m.(string)
			if !ok {
				c.AMR = nil
				return
			}
			c.AMR = append(c.AMR, s)
		}
	}
}

// RegistryCache keeps a registry's answers for TTL, registrations and
// unknown issuers alike, so a token costs a lookup per issuer and cache
// period; failures to answer are not kept. A retired provider may thus
// authenticate for up to TTL (ADR-0031). Issuers and audiences come from
// tokens not yet verified (bounded in size before the lookup), so the cache
// holds at most MaxEntries: when full it drops its expired answers, and
// empties itself if that is not enough, rather than growing with what a
// client sends.
type RegistryCache struct {
	Registry   Registry
	TTL        time.Duration    // 0: DefaultRegistryTTL
	MaxEntries int              // 0: DefaultRegistryEntries
	Now        func() time.Time // nil: the wall clock

	mu      sync.Mutex
	entries map[string]registryEntry
}

// The cache's defaults.
const (
	DefaultRegistryTTL     = 30 * time.Second
	DefaultRegistryEntries = 10000
)

type registryEntry struct {
	reg   Registration
	err   error
	until time.Time
}

// Resolve implements Registry.
func (c *RegistryCache) Resolve(ctx context.Context, issuer string, audiences []string) (Registration, error) {
	// Each part is length-prefixed, so no audience can pass for two.
	var key strings.Builder
	for _, part := range append([]string{issuer}, slices.Sorted(slices.Values(audiences))...) {
		key.WriteString(strconv.Itoa(len(part)) + ":" + part)
	}
	now := clock.Now(c.Now)
	c.mu.Lock()
	e, ok := c.entries[key.String()]
	c.mu.Unlock()
	if ok && now.Before(e.until) {
		return e.reg, e.err
	}
	reg, err := c.Registry.Resolve(ctx, issuer, audiences)
	if err != nil && !errors.Is(err, ErrUnknownIssuer) && !errors.Is(err, ErrAmbiguousIssuer) {
		return reg, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	limit := orDefaultInt(c.MaxEntries, DefaultRegistryEntries)
	if len(c.entries) >= limit {
		for k, e := range c.entries {
			if !now.Before(e.until) {
				delete(c.entries, k)
			}
		}
	}
	if c.entries == nil || len(c.entries) >= limit {
		c.entries = map[string]registryEntry{}
	}
	c.entries[key.String()] = registryEntry{reg: reg, err: err, until: now.Add(orDefault(c.TTL, DefaultRegistryTTL))}
	return reg, err
}

// Len is how many answers the cache holds.
func (c *RegistryCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func orDefaultInt(n, def int) int {
	if n > 0 {
		return n
	}
	return def
}
