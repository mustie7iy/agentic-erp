// Package authntest runs a minimal OIDC issuer for tests: discovery, signing
// keys and token minting, for the OIDC authentication of ADR-0010
// (06-engineering-process §6.6), with key rotation and endpoints that fail,
// stall or answer other bodies for the key cache of ADR-0031. It is test support and is excluded from the
// coverage figure.
package authntest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
)

// Audience is the audience tokens are minted for.
const Audience = "erp-api"

// Issuer is a running test identity provider. Its keys can be rotated and
// its endpoints made to fail, stall or answer other bodies, for the key
// cache of authn.Providers (ADR-0031); only a separate issuer
// (NewSeparate) is changed that way, never the shared one (New).
type Issuer struct {
	URL string

	mu           sync.Mutex
	keys         []signingKey // published; the last one signs
	discoveries  int
	keyFetches   int
	failNext     int
	cacheControl string
	blocked      chan struct{}
	bodies       map[string][]byte

	as authorization // the authorization server's state (server.go)
}

type signingKey struct {
	kid    string
	key    *rsa.PrivateKey
	signer jose.Signer
}

// shared is the test binary's identity provider (New).
var shared struct {
	once sync.Once
	is   *Issuer
	err  error
}

// New returns the test identity provider: one per test binary, started on
// first use and serving until the process ends. Principals are bound to
// their issuer and subject (ADR-0010), and the principals pgtest and
// fixture create name this issuer, so its tokens authenticate them
// whichever test made the issuer or the principal.
func New(tb testing.TB) *Issuer {
	tb.Helper()
	shared.once.Do(func() { shared.is, _, shared.err = start() })
	if shared.err != nil {
		tb.Fatal(shared.err)
	}
	return shared.is
}

// URL is the issuer of New, as fixtures record it on the principals they
// create.
func URL(tb testing.TB) string {
	tb.Helper()
	return New(tb).URL
}

// NewSeparate starts another identity provider, stopped when the test ends:
// an issuer the code under test does not trust, or a second trusted one
// whose tokens name the same subjects as New's.
func NewSeparate(tb testing.TB) *Issuer {
	tb.Helper()
	is, stop, err := start()
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(stop)
	return is
}

// start makes a signing key and serves discovery and the keys; stop closes
// the server.
func start() (is *Issuer, stop func(), err error) {
	k, err := newKey("k1")
	if err != nil {
		return nil, nil, err
	}
	is = &Issuer{keys: []signingKey{k}, bodies: map[string][]byte{}, as: newAuthorization()}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	is.URL = srv.URL
	mux.HandleFunc(DiscoveryPath, is.serve(DiscoveryPath, func() any {
		return map[string]any{
			"issuer": is.URL, "jwks_uri": is.URL + KeysPath,
			"authorization_endpoint": is.URL + AuthorizePath, "token_endpoint": is.URL + TokenPath, "end_session_endpoint": is.URL + EndSessionPath,
			"id_token_signing_alg_values_supported": []string{"RS256"}, "response_types_supported": []string{"code"},
			"code_challenge_methods_supported":      []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"private_key_jwt", "client_secret_basic"},
		}
	}))
	mux.HandleFunc(AuthorizePath, is.authorize)
	mux.HandleFunc(TokenPath, is.token)
	mux.HandleFunc(EndSessionPath, is.endSession)
	mux.HandleFunc(KeysPath, is.serve(KeysPath, func() any {
		set := jose.JSONWebKeySet{}
		for _, k := range is.keys {
			set.Keys = append(set.Keys, jose.JSONWebKey{Key: &k.key.PublicKey, KeyID: k.kid, Algorithm: "RS256", Use: "sig"})
		}
		return set
	}))
	return is, srv.Close, nil
}

// The paths the issuer serves.
const (
	DiscoveryPath = "/.well-known/openid-configuration"
	KeysPath      = "/keys"
)

func newKey(kid string) (signingKey, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return signingKey{}, fmt.Errorf("authntest: signing key: %w", err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: kid}}, nil)
	if err != nil {
		return signingKey{}, fmt.Errorf("authntest: signer: %w", err)
	}
	return signingKey{kid: kid, key: key, signer: signer}, nil
}

// serve answers path: counted, then stalled while blocked, failed while
// failures are due, or the body set for it, or doc as JSON.
func (is *Issuer) serve(path string, doc func() any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		is.mu.Lock()
		if path == DiscoveryPath {
			is.discoveries++
		} else {
			is.keyFetches++
		}
		blocked := is.blocked
		is.mu.Unlock()
		if blocked != nil {
			select {
			case <-blocked:
			case <-r.Context().Done():
				return
			}
		}
		is.mu.Lock()
		defer is.mu.Unlock()
		if is.failNext > 0 {
			is.failNext--
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if is.cacheControl != "" && path == KeysPath {
			w.Header().Set("Cache-Control", is.cacheControl)
		}
		w.Header().Set("Content-Type", "application/json")
		if body, ok := is.bodies[path]; ok {
			_, _ = w.Write(body) // a failed response write means the client is gone: no one is left to tell
			return
		}
		_ = json.NewEncoder(w).Encode(doc()) // a failed response write means the client is gone: no one is left to tell
	}
}

// Fetches counts the requests for discovery and for the keys so far.
func (is *Issuer) Fetches() (discovery, keys int) {
	is.mu.Lock()
	defer is.mu.Unlock()
	return is.discoveries, is.keyFetches
}

// Rotate publishes a new key next to the others and signs with it from now
// on; it returns the new key's id.
func (is *Issuer) Rotate(tb testing.TB) string {
	tb.Helper()
	is.mu.Lock()
	defer is.mu.Unlock()
	k, err := newKey(fmt.Sprintf("k%d", len(is.keys)+1))
	if err != nil {
		tb.Fatal(err)
	}
	is.keys = append(is.keys, k)
	return k.kid
}

// Retire stops publishing the key with the id.
func (is *Issuer) Retire(kid string) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.keys = slices.DeleteFunc(is.keys, func(k signingKey) bool { return k.kid == kid })
}

// FailNext answers the next n requests 503.
func (is *Issuer) FailNext(n int) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.failNext = n
}

// SetCacheControl sends the header with the keys from now on.
func (is *Issuer) SetCacheControl(v string) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.cacheControl = v
}

// Serve answers path with body from now on instead of its document.
func (is *Issuer) Serve(path string, body []byte) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.bodies[path] = body
}

// Block stalls every request until release is called (or the client gives
// up); release is called when the test ends at the latest.
func (is *Issuer) Block(tb testing.TB) (release func()) {
	tb.Helper()
	ch := make(chan struct{})
	var once sync.Once
	release = func() {
		once.Do(func() {
			is.mu.Lock()
			is.blocked = nil
			is.mu.Unlock()
			close(ch)
		})
	}
	is.mu.Lock()
	is.blocked = ch
	is.mu.Unlock()
	tb.Cleanup(release)
	return release
}

// Config is the trusted-issuer configuration for this issuer.
func (is *Issuer) Config() authn.Issuer {
	return authn.Issuer{URL: is.URL, Audience: Audience}
}

// Token mints a valid token for subject in tenant; extra claims override
// the defaults.
func (is *Issuer) Token(tb testing.TB, tenant uuid.UUID, subject string, extra map[string]any) string {
	tb.Helper()
	is.mu.Lock()
	signer := is.keys[len(is.keys)-1].signer
	is.mu.Unlock()
	return sign(tb, signer, is.claims(tenant, subject, extra))
}

// Forge mints a token like Token's, signed by a key the issuer never
// published, under a key id of its own.
func (is *Issuer) Forge(tb testing.TB, tenant uuid.UUID, subject string, extra map[string]any) string {
	tb.Helper()
	k, err := newKey(uuid.NewString())
	if err != nil {
		tb.Fatal(err)
	}
	return sign(tb, k.signer, is.claims(tenant, subject, extra))
}

func (is *Issuer) claims(tenant uuid.UUID, subject string, extra map[string]any) map[string]any {
	claims := map[string]any{
		"iss": is.URL, "aud": Audience, "sub": subject, "tenant_id": tenant.String(),
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	}
	for k, v := range extra {
		claims[k] = v
	}
	return claims
}

func sign(tb testing.TB, signer jose.Signer, claims map[string]any) string {
	tb.Helper()
	payload, _ := json.Marshal(claims) // test claims are strings, numbers and lists
	sig, err := signer.Sign(payload)
	if err != nil {
		tb.Fatal(err)
	}
	raw, err := sig.CompactSerialize()
	if err != nil {
		tb.Fatal(err)
	}
	return raw
}
