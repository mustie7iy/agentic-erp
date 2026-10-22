package authn_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
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

// pollKeyFetches waits until the issuer has had n key requests.
func pollKeyFetches(t *testing.T, is *authntest.Issuer, n int) {
	t.Helper()
	eventually(t, func() bool { _, k := is.Fetches(); return k >= n })
}

// eventually waits for cond, polling, for at most 5 seconds.
func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		<-time.After(time.Millisecond)
	}
}

// pollFetches waits until the issuer has had n discovery requests.
func pollFetches(t *testing.T, is *authntest.Issuer, n int) {
	t.Helper()
	eventually(t, func() bool { d, _ := is.Fetches(); return d >= n })
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
		if err := verify(p, is, old); err != nil {
			t.Errorf("expired keys serve while they are refetched: got %v, want verified", err)
		}
		pollKeyFetches(t, is, 3)
		eventually(t, func() bool { return verify(p, is, old) != nil })
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
		name         string
		cacheControl string
		fresh, stale time.Duration
	}{
		{"no header", "", authn.DefaultKeysTTL - time.Second, authn.DefaultKeysTTL + time.Second},
		{"max-age", "public, max-age=600", 599 * time.Second, 601 * time.Second},
		{"max-age below the floor", "max-age=10", authn.MinKeysTTL - time.Second, authn.MinKeysTTL + time.Second},
		{"max-age above the ceiling", "max-age=999999", authn.MaxKeysTTL - time.Second, authn.MaxKeysTTL + time.Second},
		{"no-store", "no-store", authn.MinKeysTTL - time.Second, authn.MinKeysTTL + time.Second},
		{"unreadable max-age", "max-age=soon", authn.DefaultKeysTTL - time.Second, authn.DefaultKeysTTL + time.Second},
		{"no-store after max-age", "max-age=600, no-store", authn.MinKeysTTL - time.Second, authn.MinKeysTTL + time.Second},
		{"max-age beyond any duration", "max-age=9223372036854775807", authn.MaxKeysTTL - time.Second, authn.MaxKeysTTL + time.Second},
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
			clk.Advance(c.stale - c.fresh)
			if err := verify(p, is, tok); err != nil {
				t.Fatal(err)
			}
			pollKeyFetches(t, is, 2)
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
	c.Advance(authn.DefaultDiscoveryTTL - authn.DefaultKeysTTL)
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
		{"signed with a shared secret", hs256(t, is), authntest.Audience},
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
		if !strings.Contains(problem.Cause(problem.From(err)).Error(), "not a public unicast address") {
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
	var wg sync.WaitGroup
	wg.Go(func() { _ = verify(p, is, raw) }) // holds the fetch; its outcome is not this test's
	t.Cleanup(wg.Wait)
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

// hs256 signs a token of is with a shared secret, as an attacker would to
// pass a public key off as an HMAC key.
func hs256(t *testing.T, is *authntest.Issuer) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: []byte(strings.Repeat("k", 32))}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	sig, err := signer.Sign([]byte(`{"iss": "` + is.URL + `", "aud": "` + authntest.Audience + `", "sub": "ada", "exp": ` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sig.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestProvidersServeExpiredCachesWithoutWaiting: once the document or the
// keys expire, requests are served from them while a refetch runs, so a
// stalled provider holds no request up.
func TestProvidersServeExpiredCachesWithoutWaiting(t *testing.T) {
	is := authntest.NewSeparate(t)
	c := newClock()
	p := &authn.Providers{Now: c.Now, Timeout: time.Hour}
	tok := is.Token(t, uuid.Nil, "ada", map[string]any{"exp": time.Now().Add(72 * time.Hour).Unix()})
	if err := verify(p, is, tok); err != nil {
		t.Fatal(err)
	}
	release := is.Block(t)
	c.Advance(authn.DefaultDiscoveryTTL + time.Second)
	done := make(chan error, 1)
	go func() { done <- verify(p, is, tok) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expired document and keys, provider stalled: got %v, want verified", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("expired document and keys, provider stalled: the request waited for the refetch")
	}
	release()
}

// TestProvidersAnswer503ForANewKeyTheyCannotFetch: a token naming a key the
// cache lacks while the provider is down may be valid (the provider
// rotated), so the answer is a retryable 503, not 401.
func TestProvidersAnswer503ForANewKeyTheyCannotFetch(t *testing.T) {
	is := authntest.NewSeparate(t)
	c := newClock()
	p := providers(c)
	if err := verify(p, is, lasting(t, is)); err != nil {
		t.Fatal(err)
	}
	is.Rotate(t)
	c.Advance(authn.DefaultMinRefresh)
	is.FailNext(1)
	wantStatus(t, verify(p, is, lasting(t, is)), 503)
	t.Run("until a fetch succeeds", func(t *testing.T) {
		wantStatus(t, verify(p, is, lasting(t, is)), 503)
	})
}

// TestProvidersKeepKeysWithTheirKeySet: keys fetched from a key set URL the
// document no longer names are not used.
func TestProvidersKeepKeysWithTheirKeySet(t *testing.T) {
	is := authntest.NewSeparate(t)
	c := newClock()
	p := providers(c)
	tok := is.Token(t, uuid.Nil, "ada", map[string]any{"exp": time.Now().Add(72 * time.Hour).Unix()})
	if err := verify(p, is, tok); err != nil {
		t.Fatal(err)
	}
	other := authntest.NewSeparate(t)
	is.Serve(authntest.DiscoveryPath, []byte(`{"issuer": "`+is.URL+`", "jwks_uri": "`+other.URL+`/keys"}`))
	c.Advance(authn.DefaultDiscoveryTTL + time.Second)
	_ = verify(p, is, tok) // starts the refetch of the document; served from the old one
	pollFetches(t, is, 2)
	eventually(t, func() bool { return verify(p, is, tok) != nil })
	wantStatus(t, verify(p, is, tok), 401)
	if _, k := other.Fetches(); k != 1 {
		t.Errorf("fetches of the new key set: got %d, want 1", k)
	}
}

// TestProvidersStopTrustingKeysTheyCannotRefresh: expired keys serve while
// refetches fail for as long again as a document does, no longer, even
// while the document itself is fetched afresh.
func TestProvidersStopTrustingKeysTheyCannotRefresh(t *testing.T) {
	is := authntest.NewSeparate(t)
	c := newClock()
	p := providers(c)
	tok := is.Token(t, uuid.Nil, "ada", map[string]any{"exp": time.Now().Add(72 * time.Hour).Unix()})
	if err := verify(p, is, tok); err != nil {
		t.Fatal(err)
	}
	is.Serve(authntest.KeysPath, []byte(`not json`))
	c.Advance(authn.DefaultKeysTTL + authn.DefaultDiscoveryTTL - time.Second)
	if err := verify(p, is, tok); err != nil {
		t.Errorf("within the grace: got %v, want verified with the old keys", err)
	}
	c.Advance(authn.DefaultMinRefresh)
	wantStatus(t, verify(p, is, tok), 503)
}

// TestProvidersProbe: registering a provider first reads its discovery
// document, afresh and without keeping it, so a mistyped issuer is told at
// once and a fixed one is accepted on the next try.
func TestProvidersProbe(t *testing.T) {
	is := authntest.NewSeparate(t)
	p := providers(newClock())
	if err := p.Probe(context.Background(), is.URL); err != nil {
		t.Errorf("a provider that answers: got %v, want none", err)
	}
	is.FailNext(1)
	if err := p.Probe(context.Background(), is.URL); err == nil {
		t.Error("a provider that fails: got no error, want one")
	}
	if err := p.Probe(context.Background(), is.URL); err != nil {
		t.Errorf("the next try: got %v, want none (a failed probe is not kept)", err)
	}
	if err := (&authn.Providers{Production: true}).Probe(context.Background(), is.URL); err == nil {
		t.Error("an http issuer in production: got no error, want one")
	}
}

// TestProvidersEndpoints: the provider's OAuth endpoints come from the
// discovery document Verify uses, fetched once; an endpoint whose URL is
// not allowed is left out, and a provider naming no authorization or
// token endpoint, or whose document cannot be read, is 503 (ADR-0052 §5).
func TestProvidersEndpoints(t *testing.T) {
	ctx := context.Background()
	is := authntest.NewSeparate(t)
	p := &authn.Providers{}
	e, err := p.Endpoints(ctx, is.URL)
	if err != nil {
		t.Fatal(err)
	}
	want := authn.Endpoints{Authorization: is.URL + authntest.AuthorizePath, Token: is.URL + authntest.TokenPath, EndSession: is.URL + authntest.EndSessionPath,
		IssParameter: true}
	if e != want {
		t.Errorf("endpoints: got %+v, want %+v", e, want)
	}
	if _, err := p.Verify(ctx, is.URL, authntest.Audience, is.Token(t, uuid.New(), "s", nil)); err != nil {
		t.Fatal(err)
	}
	if d, _ := is.Fetches(); d != 1 {
		t.Errorf("discovery fetches: got %d, want 1 shared with Verify", d)
	}
	if p.Client() == nil {
		t.Error("client: got nil")
	}
	for name, doc := range map[string]string{
		"no token endpoint":            `{"issuer": "%s", "jwks_uri": "%s/keys", "authorization_endpoint": "%s/auth"}`,
		"a token endpoint not allowed": `{"issuer": "%s", "jwks_uri": "%s/keys", "authorization_endpoint": "%s/auth", "token_endpoint": "ftp://x/token"}`,
	} {
		t.Run(name, func(t *testing.T) {
			other := authntest.NewSeparate(t)
			other.Serve(authntest.DiscoveryPath, []byte(strings.ReplaceAll(doc, "%s", other.URL)))
			if _, err := (&authn.Providers{}).Endpoints(ctx, other.URL); problem.From(err).Status != 503 {
				t.Errorf("got %v, want 503", err)
			}
		})
	}
	t.Run("no end session endpoint", func(t *testing.T) {
		other := authntest.NewSeparate(t)
		other.Serve(authntest.DiscoveryPath, []byte(strings.ReplaceAll(`{"issuer": "%s", "jwks_uri": "%s/keys", "authorization_endpoint": "%s/auth", "token_endpoint": "%s/token"}`, "%s", other.URL)))
		e, err := (&authn.Providers{}).Endpoints(ctx, other.URL)
		if err != nil || e.EndSession != "" {
			t.Errorf("got %+v, %v, want no end session endpoint", e, err)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		other := authntest.NewSeparate(t)
		other.FailNext(10)
		if _, err := (&authn.Providers{}).Endpoints(ctx, other.URL); problem.From(err).Status != 503 {
			t.Errorf("got %v, want 503", err)
		}
	})
}
