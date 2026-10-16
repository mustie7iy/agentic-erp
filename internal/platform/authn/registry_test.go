package authn_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// registry is an in-memory registry: registrations by issuer and audience,
// and a failure to answer with.
type registry struct {
	mu    sync.Mutex
	regs  map[string]authn.Registration
	err   error
	calls int
}

func (r *registry) add(reg authn.Registration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.regs == nil {
		r.regs = map[string]authn.Registration{}
	}
	r.regs[reg.Issuer+" "+reg.Audience] = reg
}

func (r *registry) Resolve(_ context.Context, issuer string, audiences []string) (authn.Registration, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.err != nil {
		return authn.Registration{}, r.err
	}
	var found []authn.Registration
	for _, aud := range audiences {
		if reg, ok := r.regs[issuer+" "+aud]; ok {
			found = append(found, reg)
		}
	}
	switch len(found) {
	case 0:
		return authn.Registration{}, authn.ErrUnknownIssuer
	case 1:
		return found[0], nil
	}
	return authn.Registration{}, authn.ErrAmbiguousIssuer
}

// registered is a development verifier trusting the static issuers and,
// through reg, the registered ones.
func registered(t *testing.T, reg authn.Registry, static ...*authntest.Issuer) *authn.Verifier {
	t.Helper()
	var cfg []authn.Issuer
	for _, is := range static {
		cfg = append(cfg, is.Config())
	}
	v, err := authn.NewRegistered(context.Background(), authn.Development, reg, &authn.Providers{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestRegisteredIssuersDecideTheTenant(t *testing.T) {
	a, b := authntest.NewSeparate(t), authntest.NewSeparate(t)
	tenantA, tenantB, provider := uuid.New(), uuid.New(), uuid.New()
	reg := &registry{}
	reg.add(authn.Registration{ProviderID: provider, TenantID: tenantA, Issuer: a.URL, Audience: authntest.Audience})
	v := registered(t, reg)
	ctx := context.Background()

	t.Run("the registration's tenant, whatever the token claims", func(t *testing.T) {
		c, err := v.Verify(ctx, a.Token(t, tenantB, "ada", nil))
		if err != nil {
			t.Fatal(err)
		}
		if c.TenantID != tenantA || c.Issuer != a.URL || c.Subject != "ada" || c.ProviderID != provider {
			t.Errorf("claims: got tenant %s, issuer %s, subject %s, provider %s, want %s, %s, ada, %s", c.TenantID, c.Issuer, c.Subject, c.ProviderID, tenantA, a.URL, provider)
		}
	})
	t.Run("a token naming no tenant", func(t *testing.T) {
		c, err := v.Verify(ctx, a.Token(t, uuid.Nil, "ada", map[string]any{"tenant_id": nil}))
		if err != nil || c.TenantID != tenantA {
			t.Errorf("got %s, %v, want tenant %s", c.TenantID, err, tenantA)
		}
	})
	t.Run("an audience among several", func(t *testing.T) {
		if _, err := v.Verify(ctx, a.Token(t, uuid.Nil, "ada", map[string]any{"aud": []string{"other", authntest.Audience}})); err != nil {
			t.Errorf("got %v, want verified", err)
		}
	})
	t.Run("an issuer no tenant registered is never fetched", func(t *testing.T) {
		_, err := v.Verify(ctx, b.Token(t, tenantA, "mallory", nil))
		wantStatus(t, err, 401)
		wantFetches(t, b, 0, 0)
	})
	t.Run("an operator token is never a registered one", func(t *testing.T) {
		_, err := v.VerifyOperator(ctx, a.Token(t, uuid.Nil, "ops", nil))
		wantStatus(t, err, 401)
	})
	t.Run("a token signed elsewhere", func(t *testing.T) {
		_, err := v.Verify(ctx, a.Forge(t, uuid.Nil, "mallory", nil))
		wantStatus(t, err, 401)
	})
}

func TestRegisteredTenantClaim(t *testing.T) {
	is := authntest.NewSeparate(t)
	tenant := uuid.New()
	claim := "erp_tenant"
	reg := &registry{}
	reg.add(authn.Registration{TenantID: tenant, Issuer: is.URL, Audience: authntest.Audience, TenantClaim: &claim})
	v := registered(t, reg)
	for _, c := range []struct {
		name  string
		extra map[string]any
		ok    bool
	}{
		{"names the tenant", map[string]any{claim: tenant.String()}, true},
		{"names another", map[string]any{claim: uuid.NewString()}, false},
		{"names none", nil, false},
		{"not a string", map[string]any{claim: 7}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := v.Verify(context.Background(), is.Token(t, uuid.Nil, "ada", c.extra))
			if c.ok {
				if err != nil {
					t.Errorf("got %v, want verified", err)
				}
				return
			}
			wantStatus(t, err, 401)
		})
	}
}

func TestRegistryFailures(t *testing.T) {
	is := authntest.NewSeparate(t)
	for _, c := range []struct {
		name   string
		err    error
		status int
	}{
		{"unknown", authn.ErrUnknownIssuer, 401},
		{"ambiguous", authn.ErrAmbiguousIssuer, 401},
		{"unreachable", errors.New("database down"), 503},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := registered(t, &registry{err: c.err})
			_, err := v.Verify(context.Background(), is.Token(t, uuid.Nil, "ada", nil))
			wantStatus(t, err, c.status)
		})
	}
	t.Run("a token whose audience is not a string or list", func(t *testing.T) {
		v := registered(t, &registry{})
		_, err := v.Verify(context.Background(), is.Token(t, uuid.Nil, "ada", map[string]any{"aud": 7}))
		wantStatus(t, err, 401)
	})
}

// TestClaimsCarryTheTokensFacts: what multi-factor and sessions are decided
// on later (T37e, T37f) is read from the token here, for static and
// registered issuers alike.
func TestClaimsCarryTheTokensFacts(t *testing.T) {
	is := authntest.NewSeparate(t)
	reg := &registry{}
	reg.add(authn.Registration{TenantID: uuid.New(), Issuer: is.URL, Audience: authntest.Audience})
	auth := time.Now().Add(-time.Minute).Truncate(time.Second)
	extra := map[string]any{"auth_time": auth.Unix(), "acr": "urn:loa:2", "amr": []string{"pwd", "mfa"}, "sid": "s-1"}
	static := authntest.NewSeparate(t)
	for name, v := range map[string]struct {
		v   *authn.Verifier
		tok string
	}{
		"registered": {registered(t, reg), is.Token(t, uuid.Nil, "ada", extra)},
		"static":     {registered(t, &registry{}, static), static.Token(t, uuid.New(), "ada", extra)},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := v.v.Verify(context.Background(), v.tok)
			if err != nil {
				t.Fatal(err)
			}
			if !c.AuthTime.Equal(auth) || c.IssuedAt.IsZero() || c.ACR != "urn:loa:2" || !slices.Equal(c.AMR, []string{"pwd", "mfa"}) || c.SessionID != "s-1" {
				t.Errorf("claims: got auth_time %v, iat %v, acr %q, amr %v, sid %q", c.AuthTime, c.IssuedAt, c.ACR, c.AMR, c.SessionID)
			}
		})
	}
	t.Run("absent or malformed facts are empty", func(t *testing.T) {
		c, err := registered(t, reg).Verify(context.Background(), is.Token(t, uuid.Nil, "ada", map[string]any{"amr": "mfa", "acr": 3, "auth_time": "soon"}))
		if err != nil {
			t.Fatal(err)
		}
		if !c.AuthTime.IsZero() || c.ACR != "" || c.AMR != nil || c.SessionID != "" {
			t.Errorf("claims: got %+v, want no facts", c)
		}
		c, err = registered(t, reg).Verify(context.Background(), is.Token(t, uuid.Nil, "ada", map[string]any{"amr": []any{"mfa", 3}}))
		if err != nil {
			t.Fatal(err)
		}
		if c.AMR != nil {
			t.Errorf("amr with a number: got %v, want none", c.AMR)
		}
	})
}

func TestRegistryCache(t *testing.T) {
	c := newClock()
	inner := &registry{}
	known := authn.Registration{TenantID: uuid.New(), Issuer: "https://known.example.com", Audience: "erp"}
	inner.add(known)
	cache := &authn.RegistryCache{Registry: inner, TTL: 30 * time.Second, Now: c.Now, MaxEntries: 3}
	ctx := context.Background()
	resolve := func(issuer string, auds ...string) error {
		_, err := cache.Resolve(ctx, issuer, auds)
		return err
	}
	t.Run("hits and misses are kept for the TTL", func(t *testing.T) {
		if err := resolve(known.Issuer, "erp"); err != nil {
			t.Fatal(err)
		}
		if err := resolve("https://unknown.example.com", "erp"); !errors.Is(err, authn.ErrUnknownIssuer) {
			t.Fatalf("got %v, want ErrUnknownIssuer", err)
		}
		_ = resolve(known.Issuer, "erp")                  // a hit
		_ = resolve("https://unknown.example.com", "erp") // a miss, kept
		_ = resolve(known.Issuer, "erp", "other")         // other audiences: another entry
		if inner.calls != 3 {
			t.Errorf("registry calls: got %d, want 3", inner.calls)
		}
		c.Advance(30 * time.Second)
		_ = resolve(known.Issuer, "erp")
		if inner.calls != 4 {
			t.Errorf("registry calls after the TTL: got %d, want 4", inner.calls)
		}
	})
	t.Run("failures are not kept", func(t *testing.T) {
		inner.err = errors.New("database down")
		before := inner.calls
		_ = resolve("https://new.example.com", "erp")
		_ = resolve("https://new.example.com", "erp")
		if inner.calls != before+2 {
			t.Errorf("registry calls: got %d, want %d", inner.calls, before+2)
		}
		inner.err = nil
	})
	t.Run("the cache stays within its bound", func(t *testing.T) {
		for i := range 10 {
			_ = resolve("https://spray-"+strings.Repeat("x", i)+".example.com", "erp")
		}
		if n := cache.Len(); n > 3 {
			t.Errorf("entries: got %d, want at most 3", n)
		}
	})
	t.Run("defaults", func(t *testing.T) {
		d := &authn.RegistryCache{Registry: inner}
		for _, aud := range []string{"erp", "other"} { // the second entry checks the default bound
			if _, err := d.Resolve(ctx, known.Issuer, []string{aud}); err != nil && !errors.Is(err, authn.ErrUnknownIssuer) {
				t.Errorf("got %v, want an answer", err)
			}
		}
		if d.Len() != 2 {
			t.Errorf("entries: got %d, want 2", d.Len())
		}
	})
}

func TestNewRegisteredNeedsARegistry(t *testing.T) {
	if _, err := authn.NewRegistered(context.Background(), authn.Development, nil, &authn.Providers{}, nil); err == nil {
		t.Error("no registry and no issuer: got a verifier, want an error")
	}
	if _, err := authn.NewRegistered(context.Background(), authn.Development, &registry{}, nil, nil); err == nil {
		t.Error("a registry without providers: got a verifier, want an error")
	}
	_, err := authn.NewRegistered(context.Background(), authn.Development, &registry{}, &authn.Providers{}, []authn.Issuer{{URL: "https://x.example.com"}})
	if p := problem.From(err); err == nil || !strings.Contains(err.Error(), "audience") {
		t.Errorf("an issuer without an audience: got %v (%v), want refused", err, p)
	}
}
