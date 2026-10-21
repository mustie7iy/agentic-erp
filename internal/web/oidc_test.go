package web

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
	"github.com/mustie7iy/agentic-erp/internal/web/session"
)

func pemOf(t *testing.T, typ string, der []byte) string {
	t.Helper()
	return string(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}))
}

func pkcs8(t *testing.T, key any) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pemOf(t, "PRIVATE KEY", der)
}

// TestClientAssertion: a client assertion is a JWT about and by the
// client, for the token endpoint, valid a minute, used once (its jti),
// signed with the client's RSA or EC P-256 key; a certificate after the
// key puts its thumbprints in the header, as Entra ID asks.
func TestClientAssertion(t *testing.T) {
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	_, edKey, _ := ed25519.GenerateKey(rand.Reader)
	ecDER, _ := x509.MarshalECPrivateKey(ecKey)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "erp-web"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &rsaKey.PublicKey, rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for name, c := range map[string]struct {
		pem    string
		public any
		alg    string
		x5t    bool
	}{
		"RSA, PKCS #8":             {pkcs8(t, rsaKey), &rsaKey.PublicKey, "RS256", false},
		"RSA, PKCS #1":             {pemOf(t, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(rsaKey)), &rsaKey.PublicKey, "RS256", false},
		"RSA with its certificate": {pkcs8(t, rsaKey) + pemOf(t, "CERTIFICATE", certDER), &rsaKey.PublicKey, "RS256", true},
		"EC P-256, SEC 1":          {pemOf(t, "EC PRIVATE KEY", ecDER), &ecKey.PublicKey, "ES256", false},
		"EC P-256, PKCS #8":        {pkcs8(t, ecKey), &ecKey.PublicKey, "ES256", false},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := clientAssertion(c.pem, "erp-web", "https://idp.example.com/token", now)
			if err != nil {
				t.Fatal(err)
			}
			jws, err := jose.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256, jose.ES256})
			if err != nil {
				t.Fatal(err)
			}
			payload, err := jws.Verify(c.public)
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			h := jws.Signatures[0].Protected
			if h.Algorithm != c.alg || h.KeyID == "" {
				t.Errorf("header: got alg %s kid %q, want %s with a kid", h.Algorithm, h.KeyID, c.alg)
			}
			if _, has := h.ExtraHeaders["x5t"]; has != c.x5t {
				t.Errorf("x5t: got %v, want %v", has, c.x5t)
			}
			var claims map[string]any
			if err := json.Unmarshal(payload, &claims); err != nil {
				t.Fatal(err)
			}
			if claims["iss"] != "erp-web" || claims["sub"] != "erp-web" || claims["aud"] != "https://idp.example.com/token" || claims["jti"] == "" ||
				claims["exp"] != float64(now.Add(time.Minute).Unix()) {
				t.Errorf("claims: got %v", claims)
			}
		})
	}
	for name, p := range map[string]string{
		"no key":              "not a PEM",
		"EC P-384":            pkcs8(t, p384),
		"Ed25519":             pkcs8(t, edKey),
		"a malformed RSA":     pemOf(t, "RSA PRIVATE KEY", []byte("x")),
		"a malformed EC":      pemOf(t, "EC PRIVATE KEY", []byte("x")),
		"a malformed key":     pemOf(t, "PRIVATE KEY", []byte("x")),
		"a certificate alone": pemOf(t, "CERTIFICATE", certDER),
	} {
		t.Run("refused: "+name, func(t *testing.T) {
			if _, err := clientAssertion(p, "erp-web", "https://idp/token", now); !errors.Is(err, errCredential) {
				t.Errorf("got %v, want errCredential", err)
			}
		})
	}
}

// TestCredentials: only references under the prefix are resolved.
func TestCredentials(t *testing.T) {
	o := &OIDC{Credentials: secrets.Local{Getenv: func(k string) string { return map[string]string{"WEB_ACME": "s3cret"}[k] }}, Prefix: "env:WEB_"}
	ctx := context.Background()
	if v, err := o.credential(ctx, "env:WEB_ACME"); err != nil || v != "s3cret" {
		t.Errorf("under the prefix: got %q, %v", v, err)
	}
	for _, ref := range []string{"env:ERP_WEB_SESSION_KEY", "env:WEB_../X", "env:WEB_NONE"} {
		if _, err := o.credential(ctx, ref); !errors.Is(err, errCredential) {
			t.Errorf("%s: got %v, want errCredential", ref, err)
		}
	}
	if _, err := (&OIDC{Credentials: o.Credentials}).credential(ctx, "env:WEB_ACME"); !errors.Is(err, errCredential) {
		t.Errorf("no prefix: got %v, want errCredential", err)
	}
	if _, err := o.authenticate(ctx, session.Client{ClientID: "c", AuthMethod: "tls_client_auth", CredentialRef: "env:WEB_ACME"}, "https://idp/token", url.Values{}); !errors.Is(err, errCredential) {
		t.Errorf("an unknown method: got %v, want errCredential", err)
	}
	if _, err := o.authenticate(ctx, session.Client{ClientID: "c", AuthMethod: "private_key_jwt", CredentialRef: "env:WEB_ACME"}, "https://idp/token", url.Values{}); !errors.Is(err, errCredential) {
		t.Errorf("a secret where a key is needed: got %v, want errCredential", err)
	}
}

// TestParseTokens: a token endpoint's answers.
func TestParseTokens(t *testing.T) {
	var refused errRefused
	if _, err := parseTokens("i", 400, []byte(`{"error":"invalid_grant"}`)); !errors.As(err, &refused) || refused.code != "invalid_grant" || !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("400: got %v, want the refusal", err)
	}
	if _, err := parseTokens("i", 401, []byte(`not json`)); !errors.As(err, &refused) {
		t.Errorf("401 without a body: got %v, want a refusal", err)
	}
	for name, c := range map[string]struct {
		status int
		body   string
	}{
		"a server error":     {500, `{}`},
		"not JSON":           {200, `<html>`},
		"no access token":    {200, `{"token_type":"Bearer"}`},
		"not a bearer token": {200, `{"access_token":"a","token_type":"DPoP"}`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseTokens("i", c.status, []byte(c.body)); problem.From(err).Status != http.StatusServiceUnavailable {
				t.Errorf("got %v, want 503", err)
			}
		})
	}
	if ts, err := parseTokens("i", 200, []byte(`{"access_token":"a","token_type":"bearer","refresh_token":"r"}`)); err != nil || ts.RefreshToken != "r" {
		t.Errorf("tokens: got %+v, %v", ts, err)
	}
}

func TestJoin(t *testing.T) {
	q := url.Values{"a": {"1"}}
	if got := join("https://idp/auth?tenant=x", q); got != "https://idp/auth?tenant=x&a=1" {
		t.Errorf("with a query: got %s", got)
	}
	if got := join("https://idp/auth", q); got != "https://idp/auth?a=1" {
		t.Errorf("without: got %s", got)
	}
}

// TestTokenEndpointOutOfReach: a token endpoint that cannot be reached is
// 503, as is a provider whose keys cannot be had when its tokens are
// verified.
func TestTokenEndpointOutOfReach(t *testing.T) {
	is := authntest.NewSeparate(t)
	is.Serve(authntest.DiscoveryPath, []byte(`{"issuer": "`+is.URL+`", "jwks_uri": "http://127.0.0.1:1/keys", "authorization_endpoint": "`+is.URL+
		`/auth", "token_endpoint": "http://127.0.0.1:1/token"}`))
	o := &OIDC{Providers: &authn.Providers{Timeout: time.Second}, Credentials: secrets.Local{Getenv: func(string) string { return "s" }}, Prefix: "env:"}
	c := session.Client{Issuer: is.URL, ClientID: "erp-web", Audience: authntest.Audience, AuthMethod: "client_secret_basic", CredentialRef: "env:S"}
	if _, err := o.exchange(context.Background(), c, "code", "verifier"); problem.From(err).Status != http.StatusServiceUnavailable {
		t.Errorf("exchange: got %v, want 503", err)
	}
	_, err := o.verifyTokens(context.Background(), c, tokenSet{IDToken: is.Token(t, [16]byte{}, "s", map[string]any{"aud": "erp-web"})}, signIn{Nonce: "n"})
	if problem.From(err).Status != http.StatusServiceUnavailable {
		t.Errorf("verify: got %v, want 503", err)
	}
	if got := o.logoutURL(context.Background(), session.Client{Issuer: "http://127.0.0.1:1"}, "id"); got != "" {
		t.Errorf("logout at an unreachable provider: got %q, want none", got)
	}
	down := authntest.NewSeparate(t)
	down.FailNext(20)
	c.Issuer = down.URL
	if _, err := o.refresh(context.Background(), c, "rt"); problem.From(err).Status != http.StatusServiceUnavailable {
		t.Errorf("no discovery: got %v, want 503", err)
	}
	for name, h := range map[string]http.HandlerFunc{
		"a redirect": func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) },
		"a body cut short": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte(`{"access`))
			w.(http.Flusher).Flush()    // the headers and a part of the body go out
			panic(http.ErrAbortHandler) // then the connection breaks
		},
	} {
		t.Run(name, func(t *testing.T) {
			tokenSrv := httptest.NewServer(h)
			defer tokenSrv.Close()
			other := authntest.NewSeparate(t)
			other.Serve(authntest.DiscoveryPath, []byte(`{"issuer": "`+other.URL+`", "jwks_uri": "`+other.URL+`/keys", "authorization_endpoint": "`+other.URL+
				`/auth", "token_endpoint": "`+tokenSrv.URL+`/token"}`))
			c.Issuer = other.URL
			if _, err := o.refresh(context.Background(), c, "rt"); problem.From(err).Status != http.StatusServiceUnavailable {
				t.Errorf("got %v, want 503", err)
			}
		})
	}
}

// TestMaxAge: a sign-in that asked max_age needs the ID token's auth_time
// within it.
func TestMaxAge(t *testing.T) {
	is := authntest.NewSeparate(t)
	o := &OIDC{Providers: &authn.Providers{}}
	c := session.Client{Issuer: is.URL, ClientID: "erp-web", Audience: authntest.Audience}
	zero := 0
	for name, at := range map[string]any{"none": nil, "an hour ago": time.Now().Add(-time.Hour).Unix()} {
		t.Run(name, func(t *testing.T) {
			claims := map[string]any{"aud": "erp-web", "nonce": "n"}
			if at != nil {
				claims["auth_time"] = at
			}
			ts := tokenSet{IDToken: is.Token(t, [16]byte{}, "s", claims), AccessToken: is.Token(t, [16]byte{}, "s", nil)}
			if _, err := o.verifyTokens(context.Background(), c, ts, signIn{Nonce: "n", MaxAge: &zero}); !errors.Is(err, errInvalidTokens) {
				t.Errorf("got %v, want errInvalidTokens", err)
			}
		})
	}
}

// TestChallengeParams reads RFC 9470 challenges as the cell writes them,
// and others.
func TestChallengeParams(t *testing.T) {
	for in, want := range map[string]map[string]string{
		`Bearer error="insufficient_user_authentication", error_description="Multi-factor authentication is required.", acr_values="urn:a urn:b"`: {
			"error": "insufficient_user_authentication", "error_description": "Multi-factor authentication is required.", "acr_values": "urn:a urn:b"},
		`Bearer error=invalid_token, max_age=0`: {"error": "invalid_token", "max_age": "0"},
		`Bearer realm="a \"quoted\" \\ value"`:  {"realm": `a "quoted" \ value`},
		`Bearer error="unterminated`:            {"error": "unterminated"},
		`Bearer error="escape at the end\`:      {"error": "escape at the end"},
		`Bearer`:                                {},
		`Bearer stray`:                          {},
	} {
		got := params(in)
		if len(got) != len(want) {
			t.Errorf("%s: got %v, want %v", in, got, want)
			continue
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: %s got %q, want %q", in, k, got[k], v)
			}
		}
	}
}

func TestStepUpParams(t *testing.T) {
	if got := stepUpACR(" urn:a  urn:b "); got != "urn:a urn:b" {
		t.Errorf("acr: got %q", got)
	}
	for _, v := range []string{"urn:a\x00", strings.Repeat("a", 1025)} {
		if got := stepUpACR(v); got != "" {
			t.Errorf("acr %q: got %q, want none", v, got)
		}
	}
	for v, want := range map[string]int{"0": 0, "300": 300} {
		if got := stepUpMaxAge(v); got == nil || *got != want {
			t.Errorf("max_age %q: got %v", v, got)
		}
	}
	for _, v := range []string{"", "-1", "x", "86401"} {
		if got := stepUpMaxAge(v); got != nil {
			t.Errorf("max_age %q: got %d, want none", v, *got)
		}
	}
}
