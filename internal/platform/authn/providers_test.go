package authn_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// clock is a settable clock for the caches of authn.Providers.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Now()} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// providers is a development Providers on c's clock.
func providers(c *clock) *authn.Providers {
	return &authn.Providers{Now: c.Now}
}

// lasting is a token valid for a day, so the clock may move hours on.
func lasting(t *testing.T, is *authntest.Issuer) string {
	t.Helper()
	return is.Token(t, uuid.Nil, "ada", map[string]any{"exp": time.Now().Add(24 * time.Hour).Unix()})
}

func verify(p *authn.Providers, is *authntest.Issuer, raw string) error {
	_, err := p.Verify(context.Background(), is.URL, authntest.Audience, raw)
	return err
}

func wantStatus(t *testing.T, err error, status int) {
	t.Helper()
	if got := problem.From(err).Status; got != status {
		t.Errorf("status: got %d (%v), want %d", got, err, status)
	}
}

func wantFetches(t *testing.T, is *authntest.Issuer, discovery, keys int) {
	t.Helper()
	d, k := is.Fetches()
	if d != discovery {
		t.Errorf("discovery fetches: got %d, want %d", d, discovery)
	}
	if k != keys {
		t.Errorf("key fetches: got %d, want %d", k, keys)
	}
}

func TestProvidersFetchLazilyAndCache(t *testing.T) {
	is := authntest.NewSeparate(t)
	p := providers(newClock())
	wantFetches(t, is, 0, 0)
	tok, err := p.Verify(context.Background(), is.URL, authntest.Audience, lasting(t, is))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if tok.Subject != "ada" || tok.Issuer != is.URL {
		t.Errorf("token: got %s at %s, want ada at %s", tok.Subject, tok.Issuer, is.URL)
	}
	for range 3 {
		if err := verify(p, is, lasting(t, is)); err != nil {
			t.Fatalf("verify again: %v", err)
		}
	}
	wantFetches(t, is, 1, 1)
}

func TestProvidersShareOneFetch(t *testing.T) {
	is := authntest.NewSeparate(t)
	p := providers(newClock())
	raw := lasting(t, is)
	release := is.Block(t)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Go(func() { errs <- verify(p, is, raw) })
	}
	pollFetches(t, is, 1)
	release()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent verify: %v", err)
		}
	}
	wantFetches(t, is, 1, 1)
}

// pollFetches waits until the issuer has had n discovery requests.
func pollFetches(t *testing.T, is *authntest.Issuer, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if d, _ := is.Fetches(); d >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("discovery fetches: never reached %d", n)
		}
		<-time.After(time.Millisecond)
	}
}

func TestProvidersFollowKeyRotation(t *testing.T) {
	is := authntest.NewSeparate(t)
	c := newClock()
	p := providers(c)
	old := lasting(t, is)
	if err := verify(p, is, old); err != nil {
		t.Fatal(err)
	}
	kid := is.Rotate(t)
	c.Advance(authn.DefaultMinRefresh)
	t.Run("a new key is fetched when a token names it", func(t *testing.T) {
		if err := verify(p, is, lasting(t, is)); err != nil {
			t.Errorf("token of the new key: got %v, want verified", err)
		}
		wantFetches(t, is, 1, 2)
	})
	t.Run("old keys stay until the issuer drops them", func(t *testing.T) {
		if err := verify(p, is, old); err != nil {
			t.Errorf("token of the old key: got %v, want verified", err)
		}
	})
	t.Run("a dropped key verifies nothing once refreshed", func(t *testing.T) {
		is.Retire("k1")
		c.Advance(authn.DefaultKeysTTL + time.Second)
		wantStatus(t, verify(p, is, old), 401)
		if err := verify(p, is, lasting(t, is)); err != nil {
			t.Errorf("token of %s: got %v, want verified", kid, err)
		}
	})
}

func TestProvidersRefreshUnknownKeysAtMostOncePerInterval(t *testing.T) {
	is := authntest.NewSeparate(t)
	c := newClock()
	p := providers(c)
	if err := verify(p, is, lasting(t, is)); err != nil {
		t.Fatal(err)
	}
	t.Run("not within the interval of the last fetch", func(t *testing.T) {
		wantStatus(t, verify(p, is, is.Forge(t, uuid.Nil, "mallory", nil)), 401)
		wantFetches(t, is, 1, 1)
	})
	c.Advance(authn.DefaultMinRefresh)
	for range 20 {
		wantStatus(t, verify(p, is, is.Forge(t, uuid.Nil, "mallory", nil)), 401)
	}
	wantFetches(t, is, 1, 2)
	c.Advance(authn.DefaultMinRefresh)
	wantStatus(t, verify(p, is, is.Forge(t, uuid.Nil, "mallory", nil)), 401)
	wantFetches(t, is, 1, 3)
}

func TestProvidersKeepKeysForTheirCacheLifetime(t *testing.T) {
	for _, c := range []struct {
		name          string
		cacheControl  string
		fresh, staleo time.Duration
	}{
		{"no header", "", authn.DefaultKeysTTL - time.Second, authn.DefaultKeysTTL + time.Second},
		{"max-age", "public, max-age=600", 599 * time.Second, 601 * time.Second},
		{"max-age below the floor", "max-age=10", authn.MinKeysTTL - time.Second, authn.MinKeysTTL + time.Second},
		{"max-age above the ceiling", "max-age=999999", authn.MaxKeysTTL - time.Second, authn.MaxKeysTTL + time.Second},
		{"no-store", "no-store", authn.MinKeysTTL - time.Second, authn.MinKeysTTL + time.Second},
		{"unreadable max-age", "max-age=soon", authn.DefaultKeysTTL - time.Second, authn.DefaultKeysTTL + time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			is := authntest.NewSeparate(t)
			is.SetCacheControl(c.cacheControl)
			clk := newClock()
			p := &authn.Providers{Now: clk.Now, DiscoveryTTL: 72 * time.Hour}
			tok := is.Token(t, uuid.Nil, "ada", map[string]any{"exp": time.Now().Add(48 * time.Hour).Unix()})
			if err := verify(p, is, tok); err != nil {
				t.Fatal(err)
			}
			clk.Advance(c.fresh)
			if err := verify(p, is, tok); err != nil {
				t.Fatal(err)
			}
			wantFetches(t, is, 1, 1)
			clk.Advance(c.staleo - c.fresh)
			if err := verify(p, is, tok); err != nil {
				t.Fatal(err)
			}
			wantFetches(t, is, 1, 2)
		})
	}
}

func TestProvidersServeStaleKeysWhileTheIssuerIsDown(t *testing.T) {
	is := authntest.NewSeparate(t)
	c := newClock()
	p := providers(c)
	tok := is.Token(t, uuid.Nil, "ada", map[string]any{"exp": time.Now().Add(72 * time.Hour).Unix()})
	if err := verify(p, is, tok); err != nil {
		t.Fatal(err)
	}
	is.FailNext(100)
	c.Advance(authn.DefaultKeysTTL + time.Second)
	if err := verify(p, is, tok); err != nil {
		t.Errorf("keys expired, issuer down: got %v, want verified with the old keys", err)
	}
	c.Advance(authn.DefaultDiscoveryTTL + time.Second)
	if err := verify(p, is, tok); err != nil {
		t.Errorf("discovery expired, issuer down: got %v, want verified with the old document", err)
	}
	c.Advance(authn.DefaultDiscoveryTTL)
	wantStatus(t, verify(p, is, tok), 503)
}

func TestProvidersAnswer503WhenTheyCannotFetch(t *testing.T) {
	tooLarge := []byte(`{"keys": [], "pad": "` + strings.Repeat("x", authn.MaxDocument) + `"}`)
	for _, c := range []struct {
		name  string
		setup func(is *authntest.Issuer)
	}{
		{"discovery fails", func(is *authntest.Issuer) { is.FailNext(1) }},
		{"keys fail", func(is *authntest.Issuer) { is.Serve(authntest.KeysPath, []byte(`not json`)) }},
		{"keys too large", func(is *authntest.Issuer) { is.Serve(authntest.KeysPath, tooLarge) }},
		{"discovery not json", func(is *authntest.Issuer) { is.Serve(authntest.DiscoveryPath, []byte(`<html>`)) }},
		{"discovery names another issuer", func(is *authntest.Issuer) {
			is.Serve(authntest.DiscoveryPath, []byte(`{"issuer": "https://elsewhere.example.com", "jwks_uri": "`+is.URL+`/keys"}`))
		}},
		{"discovery names no keys", func(is *authntest.Issuer) {
			is.Serve(authntest.DiscoveryPath, []byte(`{"issuer": "`+is.URL+`"}`))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			is := authntest.NewSeparate(t)
			c.setup(is)
			err := verify(providers(newClock()), is, lasting(t, is))
			wantStatus(t, err, 503)
			if got := problem.From(err).Code; got != "platform.identity_provider_unavailable" {
				t.Errorf("code: got %q, want platform.identity_provider_unavailable", got)
			}
		})
	}
}

func TestProvidersTimeOut(t *testing.T) {
	is := authntest.NewSeparate(t)
	is.Block(t)
	p := &authn.Providers{Timeout: 50 * time.Millisecond}
	wantStatus(t, verify(p, is, lasting(t, is)), 503)
}

func TestProvidersRefuseInvalidTokens(t *testing.T) {
	is := authntest.NewSeparate(t)
	c := newClock()
	p := providers(c)
	for _, tc := range []struct{ name, raw, audience string }{
		{"another audience", lasting(t, is), "other"},
		{"expired", is.Token(t, uuid.Nil, "ada", map[string]any{"exp": time.Now().Add(-time.Minute).Unix()}), authntest.Audience},
		{"another issuer's token", lasting(t, authntest.NewSeparate(t)), authntest.Audience},
		{"not a token", "not.a.token", authntest.Audience},
		{"unsigned", "eyJhbGciOiJub25lIn0.eyJzdWIiOiJhZGEifQ.", authntest.Audience},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.Verify(context.Background(), is.URL, tc.audience, tc.raw)
			wantStatus(t, err, 401)
		})
	}
}

func TestProvidersInProduction(t *testing.T) {
	is := authntest.NewSeparate(t)
	p := &authn.Providers{Production: true}
	t.Run("an http issuer", func(t *testing.T) {
		wantStatus(t, verify(p, is, lasting(t, is)), 503)
		wantFetches(t, is, 0, 0)
	})
	t.Run("an https issuer on loopback", func(t *testing.T) {
		https := "https" + strings.TrimPrefix(is.URL, "http")
		_, err := p.Verify(context.Background(), https, authntest.Audience, lasting(t, is))
		wantStatus(t, err, 503)
		if !strings.Contains(problem.Cause(problem.From(err)).Error(), "not allowed") {
			t.Errorf("cause: got %v, want the address refused", problem.Cause(problem.From(err)))
		}
		wantFetches(t, is, 0, 0)
	})
}

func TestProvidersWaitersGiveUp(t *testing.T) {
	is := authntest.NewSeparate(t)
	is.Block(t)
	p := providers(newClock())
	raw := lasting(t, is)
	go func() { _ = verify(p, is, raw) }() // holds the fetch; its outcome is not this test's
	pollFetches(t, is, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.Verify(ctx, is.URL, authntest.Audience, raw)
	wantStatus(t, err, 503)
}

func TestProvidersObserveFetches(t *testing.T) {
	is := authntest.NewSeparate(t)
	var mu sync.Mutex
	var seen []string
	p := &authn.Providers{Observe: func(kind, outcome string) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, kind+" "+outcome)
	}}
	is.FailNext(1)
	wantStatus(t, verify(p, is, lasting(t, is)), 503)
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(seen, ", "); got != "discovery failed" {
		t.Errorf("observed: got %q, want discovery failed", got)
	}
}

func TestProvidersDoNotRetryAFailedFetchAtOnce(t *testing.T) {
	is := authntest.NewSeparate(t)
	c := newClock()
	p := providers(c)
	is.FailNext(1)
	wantStatus(t, verify(p, is, lasting(t, is)), 503)
	wantStatus(t, verify(p, is, lasting(t, is)), 503)
	wantFetches(t, is, 1, 0)
	c.Advance(authn.DefaultMinRefresh)
	if err := verify(p, is, lasting(t, is)); err != nil {
		t.Errorf("after the interval: got %v, want verified", err)
	}
	t.Run("keys", func(t *testing.T) {
		is := authntest.NewSeparate(t)
		c := newClock()
		p := providers(c)
		is.Serve(authntest.KeysPath, []byte(`{"keys": []}`))
		wantStatus(t, verify(p, is, lasting(t, is)), 503)
		wantStatus(t, verify(p, is, lasting(t, is)), 503)
		wantFetches(t, is, 1, 1)
	})
}

func TestProvidersDefaultToRS256(t *testing.T) {
	is := authntest.NewSeparate(t)
	is.Serve(authntest.DiscoveryPath, []byte(`{"issuer": "`+is.URL+`", "jwks_uri": "`+is.URL+`/keys", "id_token_signing_alg_values_supported": ["none", "HS256"]}`))
	if err := verify(providers(newClock()), is, lasting(t, is)); err != nil {
		t.Errorf("no asymmetric algorithm advertised: got %v, want RS256 verified", err)
	}
}

// TestProvidersHandleBrokenTransfers serves the keys from a server of the
// test's own: cut short, or redirected in a loop.
func TestProvidersHandleBrokenTransfers(t *testing.T) {
	is := authntest.NewSeparate(t)
	for _, c := range []struct {
		name string
		keys http.HandlerFunc
	}{
		{"cut short", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte(`{"keys":`)) // the client is meant to see a short body
		}},
		{"redirected in a loop", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, r.URL.Path, http.StatusFound)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(c.keys)
			t.Cleanup(srv.Close)
			is.Serve(authntest.DiscoveryPath, []byte(`{"issuer": "`+is.URL+`", "jwks_uri": "`+srv.URL+`/keys"}`))
			wantStatus(t, verify(providers(newClock()), is, lasting(t, is)), 503)
		})
	}
}
