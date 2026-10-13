// Package authntest runs a minimal OIDC issuer for tests: discovery, signing
// keys and token minting. It is test support and is excluded from the
// coverage figure.
package authntest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
)

// Audience is the audience tokens are minted for.
const Audience = "erp-api"

// Issuer is a running test identity provider.
type Issuer struct {
	URL    string
	key    *rsa.PrivateKey
	signer jose.Signer
}

// New starts an issuer that stops when the test ends.
func New(tb testing.TB) *Issuer {
	tb.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		tb.Fatal(err)
	}
	is := &Issuer{key: key}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "k1"}}, nil)
	if err != nil {
		tb.Fatal(err)
	}
	is.signer = signer
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	tb.Cleanup(srv.Close)
	is.URL = srv.URL
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		// A failed response write means the client is gone: no one is left to tell.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": is.URL, "jwks_uri": is.URL + "/keys",
			"authorization_endpoint": is.URL + "/auth", "token_endpoint": is.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}}) // a failed response write means the client is gone: no one is left to tell
	})
	return is
}

// Config is the trusted-issuer configuration for this issuer.
func (is *Issuer) Config() authn.Issuer {
	return authn.Issuer{URL: is.URL, Audience: Audience}
}

// Token mints a valid token for subject in tenant; extra claims override
// the defaults.
func (is *Issuer) Token(tb testing.TB, tenant uuid.UUID, subject string, extra map[string]any) string {
	tb.Helper()
	claims := map[string]any{
		"iss": is.URL, "aud": Audience, "sub": subject, "tenant_id": tenant.String(),
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	}
	for k, v := range extra {
		claims[k] = v
	}
	payload, _ := json.Marshal(claims)
	sig, err := is.signer.Sign(payload)
	if err != nil {
		tb.Fatal(err)
	}
	raw, err := sig.CompactSerialize()
	if err != nil {
		tb.Fatal(err)
	}
	return raw
}
