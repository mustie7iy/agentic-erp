package authn_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

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
	waitFor(t, func() bool { return cache.Waiting() == 10 }) // every request has joined the lookup
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

// stuck is a registry whose first lookup hangs until its context ends, as
// on a connection gone half-open; later lookups answer.
type stuck struct {
	registry
	mu    sync.Mutex
	stuck bool
}

func (s *stuck) Resolve(ctx context.Context, issuer string, audiences []string) (authn.Registration, error) {
	s.mu.Lock()
	first := !s.stuck
	s.stuck = true
	s.mu.Unlock()
	if first {
		<-ctx.Done()
		return authn.Registration{}, ctx.Err()
	}
	return s.registry.Resolve(ctx, issuer, audiences)
}

// TestRegistryCacheLookupsHaveADeadline: a shared lookup that hangs ends at
// its own deadline, its failure is not kept, and the next request looks up
// afresh, so one stuck connection never locks a tenant out.
func TestRegistryCacheLookupsHaveADeadline(t *testing.T) {
	s := &stuck{}
	s.add(authn.Registration{TenantID: uuid.New(), Issuer: "https://x.example.com", Audience: "erp"})
	cache := &authn.RegistryCache{Registry: s, LookupTimeout: 50 * time.Millisecond}
	ctx := context.Background()
	if _, err := cache.Resolve(ctx, "https://x.example.com", []string{"erp"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a hanging lookup: got %v, want its deadline", err)
	}
	if _, err := cache.Resolve(ctx, "https://x.example.com", []string{"erp"}); err != nil {
		t.Errorf("the next request: got %v, want a fresh lookup's answer", err)
	}
}

// TestRegistryCacheFollowersOutliveTheLeader: the request that started a
// shared lookup giving up does not fail the others waiting on it.
func TestRegistryCacheFollowersOutliveTheLeader(t *testing.T) {
	b := &blocking{started: make(chan struct{}), release: make(chan struct{})}
	b.add(authn.Registration{TenantID: uuid.New(), Issuer: "https://x.example.com", Audience: "erp"})
	cache := &authn.RegistryCache{Registry: b}
	leader, cancel := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() { _, err := cache.Resolve(leader, "https://x.example.com", []string{"erp"}); leaderDone <- err }()
	<-b.started
	follower := make(chan error, 1)
	go func() {
		_, err := cache.Resolve(context.Background(), "https://x.example.com", []string{"erp"})
		follower <- err
	}()
	waitFor(t, func() bool { return cache.Waiting() == 2 })
	cancel()
	if err := <-leaderDone; err == nil {
		t.Error("the leader whose client gave up: got an answer, want its context's error")
	}
	close(b.release)
	if err := <-follower; err != nil {
		t.Errorf("the follower: got %v, want the shared answer", err)
	}
}

// panicking is a registry that panics.
type panicking struct{}

func (panicking) Resolve(context.Context, string, []string) (authn.Registration, error) {
	panic("registry broke")
}

// TestRegistryCacheSurvivesAPanic: a registry panicking in a shared lookup
// fails the requests waiting on it rather than the process.
func TestRegistryCacheSurvivesAPanic(t *testing.T) {
	cache := &authn.RegistryCache{Registry: panicking{}}
	_, err := cache.Resolve(context.Background(), "https://x.example.com", []string{"erp"})
	if err == nil || !strings.Contains(err.Error(), "registry broke") {
		t.Errorf("got %v, want the panic as an error", err)
	}
}

// waitFor polls cond for at most 5 seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		<-time.After(time.Millisecond)
	}
}
