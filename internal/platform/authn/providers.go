package authn

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"

	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/egress"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Providers verifies tokens of identity providers it discovers lazily, one
// issuer at a time, and keeps their discovery documents and keys (ADR-0031).
// Tenants register their own providers, so none is fetched at startup: an
// unreachable provider fails its tenant's requests with 503, never the
// cell's start. go-oidc's remote key set is not used: it refetches on every
// unknown key id and fetches with no timeout. Here an unknown key id
// refetches at most once per MinRefresh, concurrent requests share one
// fetch, every fetch has a timeout and a size limit, old keys stay until a
// fetch succeeds, and in production only https reaches the provider, never
// an address that is not public unicast.
type Providers struct {
	Production   bool                       // https only, to public unicast addresses only
	Timeout      time.Duration              // per fetch; 0: DefaultFetchTimeout
	DiscoveryTTL time.Duration              // 0: DefaultDiscoveryTTL; a stale document serves as long again while refetches fail
	MinRefresh   time.Duration              // 0: DefaultMinRefresh
	Now          func() time.Time           // nil: the wall clock
	Observe      func(kind, outcome string) // nil: not observed; kind discovery or keys, outcome ok or failed

	once    sync.Once
	client  *http.Client
	mu      sync.Mutex
	issuers map[string]*remote
}

// The caches' defaults and bounds.
const (
	DefaultFetchTimeout = 5 * time.Second
	DefaultDiscoveryTTL = 24 * time.Hour
	DefaultKeysTTL      = time.Hour // keys sent without a usable Cache-Control max-age
	MinKeysTTL          = 5 * time.Minute
	MaxKeysTTL          = 24 * time.Hour
	DefaultMinRefresh   = time.Minute
	MaxDocument         = 1 << 20 // bytes of a discovery document or key set
)

// asymmetric are the signing algorithms trusted from a provider: never
// none, never a shared secret.
var asymmetric = []string{oidc.RS256, oidc.RS384, oidc.RS512, oidc.ES256, oidc.ES384, oidc.ES512, oidc.PS256, oidc.PS384, oidc.PS512, oidc.EdDSA}

var errUnknownKey = errors.New("authn: no key of the issuer verifies the token")

// remote is one issuer's cached discovery document and keys.
type remote struct {
	issuer string

	mu         sync.Mutex
	jwksURI    string
	algs       []string
	discovered time.Time // zero: never
	keys       []jose.JSONWebKey
	keysFrom   string // the key set URL keys came from: unused once the document names another
	keysUntil  time.Time
	attempt    map[string]time.Time // kind → last fetch started
	lastErr    map[string]error     // kind → how the last fetch ended
	inflight   map[string]*fetch
}

// fetch is one fetch in flight, or over, that callers wait on.
type fetch struct {
	done chan struct{}
	err  error
}

// finished is the done channel of a fetch that is over before it is handed out.
var finished = func() chan struct{} { c := make(chan struct{}); close(c); return c }()

func (p *Providers) now() time.Time { return clock.Now(p.Now) }

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// Verify checks a raw token of issuer for audience: signature with the
// issuer's keys, issuer, audience and expiry. A token that fails is 401; an
// issuer whose document or keys cannot be had is 503.
func (p *Providers) Verify(ctx context.Context, issuer, audience, raw string) (*oidc.IDToken, error) {
	r := p.remote(issuer)
	algs, err := p.discover(ctx, r)
	if err != nil {
		return nil, err
	}
	payload, err := p.verifySignature(ctx, r, raw, algs)
	if err != nil {
		return nil, err
	}
	v := oidc.NewVerifier(issuer, verified{payload: payload}, &oidc.Config{ClientID: audience, SupportedSigningAlgs: algs, Now: p.now})
	tok, err := v.Verify(ctx, raw)
	if err != nil {
		return nil, problem.Unauthorized("The bearer token is invalid or expired.").WithCause(err)
	}
	return tok, nil
}

// Probe reads the issuer's discovery document afresh, within the fetch
// timeout and the production rules, keeping nothing: it tells whether a
// provider being registered can be reached (ADR-0031).
func (p *Providers) Probe(ctx context.Context, issuer string) error {
	ctx, cancel := context.WithTimeout(ctx, orDefault(p.Timeout, DefaultFetchTimeout))
	defer cancel()
	return p.fetchDiscovery(ctx, &remote{issuer: issuer, attempt: map[string]time.Time{}})
}

// verified hands go-oidc the payload whose signature Verify has checked,
// so the claims are checked without verifying the signature twice. It is
// made for one call of Verify, with the token that call verifies.
type verified struct{ payload string }

func (v verified) VerifySignature(context.Context, string) ([]byte, error) {
	return []byte(v.payload), nil
}

func (p *Providers) remote(issuer string) *remote {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.issuers == nil {
		p.issuers = map[string]*remote{}
	}
	r, ok := p.issuers[issuer]
	if !ok {
		r = &remote{issuer: issuer, attempt: map[string]time.Time{}, lastErr: map[string]error{}, inflight: map[string]*fetch{}}
		p.issuers[issuer] = r
	}
	return r
}

// discover returns the issuer's signing algorithms, fetching its document
// when there is none. One older than the TTL is refetched in the
// background and serves meanwhile, and for as long again while refetches
// fail, so a stalled provider holds no request up.
func (p *Providers) discover(ctx context.Context, r *remote) ([]string, error) {
	ttl := orDefault(p.DiscoveryTTL, DefaultDiscoveryTTL)
	r.mu.Lock()
	have, age := !r.discovered.IsZero(), p.now().Sub(r.discovered)
	algs := r.algs
	r.mu.Unlock()
	if have && age < 2*ttl {
		if age >= ttl {
			p.refresh(ctx, r, "discovery", p.fetchDiscovery) // in the background
		}
		return algs, nil
	}
	err := p.wait(ctx, r, "discovery", p.refresh(ctx, r, "discovery", p.fetchDiscovery))
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.discovered.IsZero() && p.now().Sub(r.discovered) < 2*ttl {
		return r.algs, nil
	}
	return nil, unavailable(r.issuer, err)
}

// verifySignature checks the token's signature with the issuer's keys. A
// token naming a key they lack waits for a refetch; expired keys are
// refetched in the background and serve meanwhile. A key the provider
// could not be asked for is 503, since the token may be valid.
func (p *Providers) verifySignature(ctx context.Context, r *remote, raw string, algs []string) (string, error) {
	jws, err := jose.ParseSigned(raw, sigAlgs(algs))
	if err != nil || len(jws.Signatures) != 1 {
		return "", problem.Unauthorized("The bearer token is malformed.").WithCause(err)
	}
	kid := jws.Signatures[0].Header.KeyID
	grace := orDefault(p.DiscoveryTTL, DefaultDiscoveryTTL)
	keys, expired := r.current(p.now(), grace)
	var fetchErr error
	switch {
	case !hasKey(keys, kid):
		fetchErr = p.wait(ctx, r, "keys", p.refresh(ctx, r, "keys", p.fetchKeys))
		keys, _ = r.current(p.now(), grace)
	case expired:
		p.refresh(ctx, r, "keys", p.fetchKeys) // in the background
	}
	if fetchErr != nil && !hasKey(keys, kid) {
		return "", unavailable(r.issuer, fetchErr)
	}
	if len(keys) == 0 {
		// A key fetch already in flight for the key set the document named
		// before is joined and its keys are not used: this request is 503,
		// the next fetches the new set (fetchDiscovery lifts the limit).
		return "", unavailable(r.issuer, nil)
	}
	for _, k := range keys {
		if kid != "" && k.KeyID != kid {
			continue
		}
		if payload, err := jws.Verify(k); err == nil {
			return string(payload), nil
		}
	}
	return "", problem.Unauthorized("The bearer token is invalid or expired.").WithCause(errUnknownKey)
}

// current returns the keys of the key set the document names, and whether
// they are past their lifetime. Past it they serve while refetches fail for
// as long again as a document does (grace), no longer: a key the provider
// revoked while it could not be reached is not trusted for ever.
func (r *remote) current(now time.Time, grace time.Duration) ([]jose.JSONWebKey, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.keysFrom != r.jwksURI || !now.Before(r.keysUntil.Add(grace)) {
		return nil, true
	}
	return r.keys, !now.Before(r.keysUntil)
}

func hasKey(keys []jose.JSONWebKey, kid string) bool {
	return slices.ContainsFunc(keys, func(k jose.JSONWebKey) bool { return kid == "" || k.KeyID == kid })
}

func sigAlgs(algs []string) []jose.SignatureAlgorithm {
	out := make([]jose.SignatureAlgorithm, len(algs))
	for i, a := range algs {
		out[i] = jose.SignatureAlgorithm(a)
	}
	return out
}

// refresh starts one fetch of kind for r, unless one started less than
// MinRefresh ago, and returns the fetch in flight, or the last one done.
// The fetch runs detached from the caller, so one client giving up does not
// fail the others.
func (p *Providers) refresh(ctx context.Context, r *remote, kind string, do func(context.Context, *remote) error) *fetch {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, running := r.inflight[kind]
	if !running {
		if last, ok := r.attempt[kind]; ok && p.now().Sub(last) < orDefault(p.MinRefresh, DefaultMinRefresh) {
			return &fetch{done: finished, err: r.lastErr[kind]} // the last fetch's outcome stands until the next
		}
		f = &fetch{done: make(chan struct{})}
		r.inflight[kind], r.attempt[kind] = f, p.now()
		go p.run(context.WithoutCancel(ctx), r, kind, f, do)
	}
	return f
}

// wait waits for the fetch f, or until the caller gives up.
func (p *Providers) wait(ctx context.Context, r *remote, kind string, f *fetch) error {
	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		return fmt.Errorf("authn: wait for %s of %s: %w", kind, r.issuer, ctx.Err())
	}
}

func (p *Providers) run(ctx context.Context, r *remote, kind string, f *fetch, do func(context.Context, *remote) error) {
	ctx, cancel := context.WithTimeout(ctx, orDefault(p.Timeout, DefaultFetchTimeout))
	defer cancel()
	f.err = do(ctx, r)
	if p.Observe != nil {
		outcome := "ok"
		if f.err != nil {
			outcome = "failed"
		}
		p.Observe(kind, outcome)
	}
	r.mu.Lock()
	delete(r.inflight, kind)
	r.lastErr[kind] = f.err
	r.mu.Unlock()
	close(f.done)
}

// fetchDiscovery reads the issuer's discovery document; it must name the
// issuer exactly and a key set URL as trusted as the issuer.
func (p *Providers) fetchDiscovery(ctx context.Context, r *remote) error {
	var doc struct {
		Issuer  string   `json:"issuer"`
		JWKSURI string   `json:"jwks_uri"`
		Algs    []string `json:"id_token_signing_alg_values_supported"`
	}
	if _, err := p.get(ctx, strings.TrimSuffix(r.issuer, "/")+"/.well-known/openid-configuration", &doc); err != nil {
		return err
	}
	if doc.Issuer != r.issuer {
		return fmt.Errorf("authn: discovery of %s names issuer %q", r.issuer, doc.Issuer)
	}
	if _, err := p.allowedURL(doc.JWKSURI); err != nil {
		return fmt.Errorf("authn: discovery of %s: jwks_uri: %w", r.issuer, err)
	}
	algs := slices.DeleteFunc(slices.Clone(doc.Algs), func(a string) bool { return !slices.Contains(asymmetric, a) })
	if len(algs) == 0 {
		algs = []string{oidc.RS256} // the OIDC default
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if doc.JWKSURI != r.jwksURI {
		delete(r.attempt, "keys") // the new key set is fetched at once, whenever the old one was
	}
	r.jwksURI, r.algs, r.discovered = doc.JWKSURI, algs, p.now()
	return nil
}

// fetchKeys reads the issuer's key set, kept for the response's max-age
// within MinKeysTTL and MaxKeysTTL (DefaultKeysTTL without one).
func (p *Providers) fetchKeys(ctx context.Context, r *remote) error {
	r.mu.Lock()
	uri := r.jwksURI
	r.mu.Unlock()
	var set jose.JSONWebKeySet
	h, err := p.get(ctx, uri, &set)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys, r.keysFrom, r.keysUntil = set.Keys, uri, p.now().Add(keysTTL(h.Get("Cache-Control")))
	return nil
}

// keysTTL reads how long keys are kept from their Cache-Control header:
// no-store or no-cache anywhere in it mean MinKeysTTL; max-age is clamped
// to MinKeysTTL..MaxKeysTTL; anything else is DefaultKeysTTL.
func keysTTL(cacheControl string) time.Duration {
	ttl := DefaultKeysTTL
	for _, d := range strings.Split(cacheControl, ",") {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "no-store" || d == "no-cache" {
			return MinKeysTTL
		}
		if v, ok := strings.CutPrefix(d, "max-age="); ok {
			if s, err := strconv.ParseUint(v, 10, 64); err == nil {
				ttl = time.Duration(min(s, uint64(MaxKeysTTL/time.Second))) * time.Second
				ttl = max(ttl, MinKeysTTL)
			}
		}
	}
	return ttl
}

// get fetches a JSON document of at most MaxDocument bytes into v.
func (p *Providers) get(ctx context.Context, u string, v any) (http.Header, error) {
	parsed, err := p.allowedURL(u)
	if err != nil {
		return nil, err
	}
	req := (&http.Request{Method: http.MethodGet, URL: parsed, Host: parsed.Host, Header: http.Header{"Accept": {"application/json"}}}).WithContext(ctx)
	resp, err := p.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("authn: fetch %s: %w", u, err)
	}
	defer func() { _ = resp.Body.Close() }() // the body is read or abandoned; closing a response body loses nothing
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("authn: fetch %s: status %d", u, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxDocument+1))
	if err != nil {
		return nil, fmt.Errorf("authn: read %s: %w", u, err)
	}
	if len(body) > MaxDocument {
		return nil, fmt.Errorf("authn: %s is larger than %d bytes", u, MaxDocument)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return nil, fmt.Errorf("authn: parse %s: %w", u, err)
	}
	return resp.Header, nil
}

// allowedURL parses a provider URL: https, or http outside production.
func (p *Providers) allowedURL(u string) (*url.URL, error) {
	parsed, err := url.Parse(u)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && (p.Production || parsed.Scheme != "http")) {
		return nil, fmt.Errorf("authn: %q is not an allowed provider URL", u)
	}
	return parsed, nil
}

func (p *Providers) httpClient() *http.Client {
	p.once.Do(func() {
		dialer := &net.Dialer{Timeout: orDefault(p.Timeout, DefaultFetchTimeout)}
		if p.Production {
			dialer.Control = egress.RefusePrivate
		}
		p.client = &http.Client{
			Transport: &http.Transport{DialContext: dialer.DialContext, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
				ForceAttemptHTTP2: true, MaxIdleConnsPerHost: 2, IdleConnTimeout: 90 * time.Second},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 3 {
					return errors.New("authn: too many redirects")
				}
				_, err := p.allowedURL(req.URL.String())
				return err
			},
		}
	})
	return p.client
}

// unavailable is the 503 of an issuer whose document or keys cannot be had.
func unavailable(issuer string, cause error) error {
	if cause == nil {
		cause = fmt.Errorf("authn: %s has no usable document or keys", issuer)
	}
	p := problem.New(http.StatusServiceUnavailable, "platform.identity_provider_unavailable", "Identity provider unavailable",
		"The identity provider could not be reached; try again shortly.")
	p.RetryAfter = 30
	return p.WithCause(cause)
}
