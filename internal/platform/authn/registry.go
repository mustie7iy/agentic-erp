package authn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"runtime/debug"
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
	// The tenant's multi-factor rule for people (the cell's registry
	// knows it; the router's does not, and the cell applies it).
	MFARequired bool
	ACRValues   []string
	AMRValues   []string
	// The tenant's session rule for people (the cell's registry).
	SessionMaxAge time.Duration
	IdleTimeout   time.Duration
	// CertificateBound says the provider binds its tokens to the client's
	// certificate (RFC 8705, ADR-0033; the cell's registry).
	CertificateBound bool
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
	c := reqctx.Claims{Issuer: reg.Issuer, Subject: tok.Subject, ProviderID: reg.ProviderID, Roles: []string{},
		MFA:     reqctx.MFA{Required: reg.MFARequired, ACRValues: reg.ACRValues, AMRValues: reg.AMRValues},
		Session: reqctx.SessionRule{MaxAge: reg.SessionMaxAge, Idle: reg.IdleTimeout}, CertificateBound: reg.CertificateBound}
	facts(&c, tok, claims)
	if err := confirmation(&c, claims); err != nil {
		return reqctx.Claims{}, "", err
	}
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

// confirmation reads the token's cnf (RFC 7800): absent, the token is a
// bearer token; a non-empty x5t#S256, it is bound to that certificate
// (RFC 8705). Any other confirmation (DPoP's jkt, a malformed one) is
// refused: taken as a bearer token, a stolen bound token would work
// without its proof (ADR-0033).
func confirmation(c *reqctx.Claims, claims map[string]any) error {
	raw, present := claims["cnf"]
	if !present {
		return nil
	}
	cnf, _ := raw.(map[string]any)
	thumb, _ := cnf["x5t#S256"].(string)
	if thumb == "" {
		return problem.Unauthorized("The token's confirmation (cnf) is not a certificate binding this API accepts.")
	}
	c.CertThumbprint = thumb
	return nil
}

// facts reads the token's facts that multi-factor and sessions are
// decided on; absent or malformed claims leave them empty.
func facts(c *reqctx.Claims, tok *oidc.IDToken, claims map[string]any) {
	c.IssuedAt, c.Expiry = tok.IssuedAt, tok.Expiry
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
// authenticate for up to TTL (ADR-0031). Concurrent requests for the same
// answer share one lookup. Issuers and audiences come from tokens not yet
// verified (bounded in size before the lookup), so the registrations held
// are at most MaxEntries and the unknown issuers, which any client can
// send, at most MaxMisses, kept apart so a spray of them never pushes a
// registration out: a full map drops its expired answers, and empties
// itself if that is not enough.
type RegistryCache struct {
	Registry   Registry
	TTL        time.Duration // 0: DefaultRegistryTTL
	MaxEntries int           // 0: DefaultRegistryEntries
	MaxMisses  int           // 0: DefaultRegistryMisses
	// LookupTimeout bounds a shared lookup, which outlives the request that
	// started it; 0: DefaultLookupTimeout.
	LookupTimeout time.Duration
	Now           func() time.Time // nil: the wall clock

	mu       sync.Mutex
	hits     map[string]registryEntry
	misses   map[string]registryEntry
	inflight map[string]*lookup
	waiting  int
}

// The cache's defaults.
const (
	DefaultRegistryTTL     = 30 * time.Second
	DefaultRegistryEntries = 10000
	DefaultRegistryMisses  = 1000
	DefaultLookupTimeout   = 5 * time.Second
)

type registryEntry struct {
	reg   Registration
	err   error
	until time.Time
}

// lookup is one registry lookup that concurrent requests wait on.
type lookup struct {
	done chan struct{}
	reg  Registration
	err  error
}

// Resolve implements Registry.
func (c *RegistryCache) Resolve(ctx context.Context, issuer string, audiences []string) (Registration, error) {
	key := cacheKey(issuer, audiences)
	c.mu.Lock()
	if e, ok := c.cached(key); ok {
		c.mu.Unlock()
		return e.reg, e.err
	}
	l, running := c.inflight[key]
	if !running {
		l = &lookup{done: make(chan struct{})}
		if c.inflight == nil {
			c.inflight = map[string]*lookup{}
		}
		c.inflight[key] = l
	}
	c.waiting++
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.waiting--
		c.mu.Unlock()
	}()
	if !running {
		// Detached from this request's cancellation, so the others waiting
		// on it do not fail because this one's client gave up, and bounded
		// by its own deadline, so a lookup stuck on a connection fails them
		// all in time rather than keeping the issuer locked out.
		lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), orDefault(c.LookupTimeout, DefaultLookupTimeout))
		go c.run(lctx, cancel, key, issuer, audiences, l)
	}
	select {
	case <-l.done:
		return l.reg, l.err
	case <-ctx.Done():
		return Registration{}, fmt.Errorf("authn: wait for the registry: %w", ctx.Err())
	}
}

// Waiting is how many requests wait on registry lookups: a gauge for
// metrics, which the tests use to know every request has joined a lookup.
func (c *RegistryCache) Waiting() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.waiting
}

// cacheKey is the issuer and the sorted audiences, each part
// length-prefixed, so no audience can pass for two.
func cacheKey(issuer string, audiences []string) string {
	var key strings.Builder
	for _, part := range append([]string{issuer}, slices.Sorted(slices.Values(audiences))...) {
		key.WriteString(strconv.Itoa(len(part)) + ":" + part)
	}
	return key.String()
}

// cached returns a live answer; c.mu is held.
func (c *RegistryCache) cached(key string) (registryEntry, bool) {
	now := clock.Now(c.Now)
	for _, m := range []map[string]registryEntry{c.hits, c.misses} {
		if e, ok := m[key]; ok && now.Before(e.until) {
			return e, true
		}
	}
	return registryEntry{}, false
}

// run looks the answer up, keeps it unless the registry failed to answer,
// and hands it to every request waiting on l.
func (c *RegistryCache) run(ctx context.Context, cancel context.CancelFunc, key, issuer string, audiences []string, l *lookup) {
	defer cancel()
	l.reg, l.err = c.resolve(ctx, issuer, audiences)
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inflight, key)
	defer close(l.done)
	now := clock.Now(c.Now)
	e := registryEntry{reg: l.reg, err: l.err, until: now.Add(orDefault(c.TTL, DefaultRegistryTTL))}
	switch {
	case l.err == nil:
		c.hits = keep(c.hits, key, e, now, orDefaultInt(c.MaxEntries, DefaultRegistryEntries))
	case errors.Is(l.err, ErrUnknownIssuer), errors.Is(l.err, ErrAmbiguousIssuer):
		c.misses = keep(c.misses, key, e, now, orDefaultInt(c.MaxMisses, DefaultRegistryMisses))
	}
}

// resolve asks the registry, turning a panic into an error: the lookup
// runs on a goroutine of its own, where no request's recovery catches it,
// and the requests waiting on it must get an answer.
func (c *RegistryCache) resolve(ctx context.Context, issuer string, audiences []string) (reg Registration, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("authn: the registry panicked: %v\n%s", r, debug.Stack())
		}
	}()
	return c.Registry.Resolve(ctx, issuer, audiences)
}

// keep stores e in m, which holds at most limit answers: when full it
// drops the expired ones, and starts afresh if that is not enough.
func keep(m map[string]registryEntry, key string, e registryEntry, now time.Time, limit int) map[string]registryEntry {
	if len(m) >= limit {
		for k, old := range m {
			if !now.Before(old.until) {
				delete(m, k)
			}
		}
	}
	if m == nil || len(m) >= limit {
		m = map[string]registryEntry{}
	}
	m[key] = e
	return m
}

// Len is how many answers the cache holds.
func (c *RegistryCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.hits) + len(c.misses)
}

func orDefaultInt(n, def int) int {
	if n > 0 {
		return n
	}
	return def
}
