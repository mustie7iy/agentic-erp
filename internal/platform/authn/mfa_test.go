package authn_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func TestCheckMFA(t *testing.T) {
	rule := reqctx.MFA{Required: true, ACRValues: []string{"urn:loa:2", `say "hi"`}, AMRValues: []string{"mfa", "hwk"}}
	for _, c := range []struct {
		name   string
		claims reqctx.Claims
		ok     bool
	}{
		{"not required", reqctx.Claims{}, true},
		{"an accepted acr", reqctx.Claims{MFA: rule, ACR: "urn:loa:2"}, true},
		{"an accepted amr", reqctx.Claims{MFA: rule, AMR: []string{"pwd", "hwk"}}, true},
		{"neither", reqctx.Claims{MFA: rule, ACR: "urn:loa:1", AMR: []string{"pwd"}}, false},
		{"nothing", reqctx.Claims{MFA: rule}, false},
		{"an empty acr", reqctx.Claims{MFA: reqctx.MFA{Required: true, AMRValues: []string{"mfa"}}}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := authn.CheckMFA(c.claims)
			if c.ok {
				if err != nil {
					t.Errorf("got %v, want accepted", err)
				}
				return
			}
			p := problem.From(err)
			if p.Status != 401 || p.Code != "identity.mfa_required" {
				t.Fatalf("got %d %s, want 401 identity.mfa_required", p.Status, p.Code)
			}
			challenge := p.Header.Get("WWW-Authenticate")
			if !strings.HasPrefix(challenge, `Bearer error="insufficient_user_authentication"`) {
				t.Errorf("challenge: got %q, want the RFC 9470 error", challenge)
			}
			if len(c.claims.MFA.ACRValues) > 0 && !strings.Contains(challenge, `acr_values="urn:loa:2 say \"hi\""`) {
				t.Errorf("challenge: got %q, want the provider's acr values, quoted", challenge)
			}
		})
	}
	t.Run("no acr values to ask for", func(t *testing.T) {
		p := problem.From(authn.CheckMFA(reqctx.Claims{MFA: reqctx.MFA{Required: true, AMRValues: []string{"mfa"}}}))
		if strings.Contains(p.Header.Get("WWW-Authenticate"), "acr_values") {
			t.Errorf("challenge: got %q, want no acr_values", p.Header.Get("WWW-Authenticate"))
		}
	})
}

// TestRegisteredMFARule: the verifier carries the registration's
// multi-factor rule to the claims, for the unit of work to apply once it
// knows the principal is a person.
func TestRegisteredMFARule(t *testing.T) {
	is := authntest.NewSeparate(t)
	reg := &registry{}
	reg.add(authn.Registration{TenantID: uuid.New(), Issuer: is.URL, Audience: authntest.Audience, MFARequired: true, ACRValues: []string{"a"}, AMRValues: []string{"mfa"}})
	c, err := registered(t, reg).Verify(context.Background(), is.Token(t, uuid.Nil, "ada", nil))
	if err != nil {
		t.Fatal(err)
	}
	if !c.MFA.Required || strings.Join(c.MFA.ACRValues, ",") != "a" || strings.Join(c.MFA.AMRValues, ",") != "mfa" {
		t.Errorf("rule: got %+v, want required, acr a, amr mfa", c.MFA)
	}
}

// blocking is a registry that holds every answer until release is closed.
type blocking struct {
	registry
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blocking) Resolve(ctx context.Context, issuer string, audiences []string) (authn.Registration, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return b.registry.Resolve(ctx, issuer, audiences)
}

// TestRegistryCacheSharesALookup: concurrent requests for the same issuer
// and audiences share one lookup.
func TestRegistryCacheSharesALookup(t *testing.T) {
	b := &blocking{started: make(chan struct{}), release: make(chan struct{})}
	cache := &authn.RegistryCache{Registry: b}
	ctx := context.Background()
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() { _, _ = cache.Resolve(ctx, "https://x.example.com", []string{"erp"}) })
	}
	<-b.started
	close(b.release)
	wg.Wait()
	if b.calls != 1 {
		t.Errorf("registry calls: got %d, want 1", b.calls)
	}
}

// TestRegistryCacheBoundsMisses: unknown issuers, which any client can
// send, are kept within their own smaller bound and never push out the
// registrations.
func TestRegistryCacheBoundsMisses(t *testing.T) {
	inner := &registry{}
	inner.add(authn.Registration{TenantID: uuid.New(), Issuer: "https://known.example.com", Audience: "erp"})
	cache := &authn.RegistryCache{Registry: inner, MaxEntries: 100, MaxMisses: 2}
	ctx := context.Background()
	if _, err := cache.Resolve(ctx, "https://known.example.com", []string{"erp"}); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		_, _ = cache.Resolve(ctx, "https://spray-"+strings.Repeat("x", i)+".example.com", []string{"erp"})
	}
	if n := cache.Len(); n > 3 {
		t.Errorf("entries: got %d, want the registration and at most 2 misses", n)
	}
	calls := inner.calls
	if _, err := cache.Resolve(ctx, "https://known.example.com", []string{"erp"}); err != nil || inner.calls != calls {
		t.Errorf("the registration after the spray: got %v and %d more calls, want it still cached", err, inner.calls-calls)
	}
}

// TestRegistryCacheWaiterGivesUp: a request waiting on another's lookup
// stops waiting when its client gives up.
func TestRegistryCacheWaiterGivesUp(t *testing.T) {
	b := &blocking{started: make(chan struct{}), release: make(chan struct{})}
	cache := &authn.RegistryCache{Registry: b}
	var wg sync.WaitGroup
	wg.Go(func() { _, _ = cache.Resolve(context.Background(), "https://x.example.com", []string{"erp"}) })
	<-b.started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.Resolve(ctx, "https://x.example.com", []string{"erp"}); err == nil {
		t.Error("a waiter whose client gave up: got an answer, want its context's error")
	}
	close(b.release)
	wg.Wait()
}
