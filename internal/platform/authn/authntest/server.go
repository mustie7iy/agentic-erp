package authntest

import (
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
)

// The authorization server: an authorization endpoint for the code flow
// with PKCE (RFC 7636, S256 only), a token endpoint that authenticates
// clients by private_key_jwt (OIDC Core §9) or client_secret_basic, issues
// ID and access tokens and rotates refresh tokens, and RP-initiated logout
// (OIDC RP-Initiated Logout 1.0), for the web workspace's sign-in
// (ADR-0052 §5). It follows the specifications closely enough to refuse
// what a real provider refuses: an unknown client or redirect URI, a
// missing or wrong code verifier, a code or refresh token used twice, a
// client assertion that is unsigned, expired, for another audience or
// replayed. It signs in whoever SignIn names, with no page of its own.

// The authorization server's paths.
const (
	AuthorizePath  = "/auth"
	TokenPath      = "/token"
	EndSessionPath = "/logout"
)

// Client is a client registered at the authorization server.
type Client struct {
	ID                    string
	RedirectURI           string           // the one redirect URI, compared exactly
	PostLogoutRedirectURI string           // where logout may return to
	Key                   crypto.PublicKey // private_key_jwt: the key client assertions are signed with
	Secret                string           // client_secret_basic, without a Key
	Refresh               bool             // the client gets refresh tokens
}

// User is whom the authorization endpoint signs in.
type User struct {
	Tenant  uuid.UUID
	Subject string
	Claims  map[string]any // in both tokens (acr, amr, …)
	// StepUp are claims added when the client asks for acr_values: the
	// second factor the user then proves. Nil: the user proves none.
	StepUp map[string]any
}

// Hooks change what the server issues, for the refusals under test.
type Hooks struct {
	IDToken     func(claims map[string]any) // changes each ID token's claims before signing
	AccessToken func(claims map[string]any) // changes each access token's claims before signing
}

type authorization struct {
	clients    map[string]Client
	user       *User
	codes      map[string]*grant
	refresh    map[string]*grant
	assertions map[string]bool // client assertion ids already used
	ttl        time.Duration
	hooks      Hooks
	tokenCalls map[string]int // grant type → calls
	logouts    []url.Values
}

// grant is what a code or refresh token stands for.
type grant struct {
	client, redirect, challenge, nonce, scope string
	user                                      User
	sid                                       string
	authTime                                  time.Time
	claims                                    map[string]any
	used                                      bool
}

func newAuthorization() authorization {
	return authorization{clients: map[string]Client{}, codes: map[string]*grant{}, refresh: map[string]*grant{}, assertions: map[string]bool{},
		ttl: time.Hour, tokenCalls: map[string]int{}}
}

// RegisterClient registers or replaces a client.
func (is *Issuer) RegisterClient(c Client) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.as.clients[c.ID] = c
}

// SignIn makes the authorization endpoint sign u in from now on; until it
// is called, every authorization is denied (access_denied).
func (is *Issuer) SignIn(u User) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.as.user = &u
}

// SetTokenTTL sets the lifetime of the tokens issued from now on (one
// hour by default).
func (is *Issuer) SetTokenTTL(d time.Duration) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.as.ttl = d
}

// SetHooks sets the hooks applied to the tokens issued from now on.
func (is *Issuer) SetHooks(h Hooks) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.as.hooks = h
}

// TokenRequests counts the token endpoint's requests of the grant type
// (authorization_code, refresh_token) so far, refused ones too.
func (is *Issuer) TokenRequests(grantType string) int {
	is.mu.Lock()
	defer is.mu.Unlock()
	return is.as.tokenCalls[grantType]
}

// Logouts are the query parameters of each RP-initiated logout so far.
func (is *Issuer) Logouts() []url.Values {
	is.mu.Lock()
	defer is.mu.Unlock()
	return slices.Clone(is.as.logouts)
}

func random() string {
	return rand.Text() // 128 random bits; crypto/rand does not fail
}

// authorize is the authorization endpoint (RFC 6749 §4.1.1, RFC 7636 §4.3).
// An unknown client or redirect URI is answered here, never redirected
// (RFC 6749 §4.1.2.1); any other error goes back to the client.
func (is *Issuer) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	is.mu.Lock()
	defer is.mu.Unlock()
	c, ok := is.as.clients[q.Get("client_id")]
	if !ok || q.Get("redirect_uri") != c.RedirectURI {
		http.Error(w, "unknown client or redirect_uri", http.StatusBadRequest)
		return
	}
	back := func(params url.Values) {
		params.Set("state", q.Get("state"))
		params.Set("iss", is.URL) // RFC 9207
		http.Redirect(w, r, c.RedirectURI+"?"+params.Encode(), http.StatusFound)
	}
	switch {
	case q.Get("response_type") != "code" || !slices.Contains(strings.Fields(q.Get("scope")), "openid"):
		back(url.Values{"error": {"invalid_request"}})
		return
	case q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43:
		back(url.Values{"error": {"invalid_request"}, "error_description": {"PKCE with S256 is required"}})
		return
	case is.as.user == nil:
		back(url.Values{"error": {"access_denied"}})
		return
	}
	g := &grant{client: c.ID, redirect: c.RedirectURI, challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), scope: q.Get("scope"),
		user: *is.as.user, sid: random(), authTime: time.Now(), claims: map[string]any{}}
	for k, v := range is.as.user.Claims {
		g.claims[k] = v
	}
	if q.Get("acr_values") != "" {
		for k, v := range is.as.user.StepUp {
			g.claims[k] = v
		}
	}
	code := random()
	is.as.codes[code] = g
	back(url.Values{"code": {code}})
}

// token is the token endpoint (RFC 6749 §3.2).
func (is *Issuer) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.ParseForm() != nil {
		tokenError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	is.mu.Lock()
	defer is.mu.Unlock()
	is.as.tokenCalls[r.PostForm.Get("grant_type")]++
	c, ok := is.client(r)
	if !ok {
		tokenError(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	var g *grant
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		g = is.as.codes[r.PostForm.Get("code")]
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if g == nil || g.used || g.client != c.ID || g.redirect != r.PostForm.Get("redirect_uri") ||
			base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
			if g != nil {
				g.used = true // a code tried wrongly or twice is spent (RFC 6749 §4.1.2)
			}
			tokenError(w, http.StatusBadRequest, "invalid_grant")
			return
		}
		g.used = true
	case "refresh_token":
		g = is.as.refresh[r.PostForm.Get("refresh_token")]
		if g == nil || g.client != c.ID {
			tokenError(w, http.StatusBadRequest, "invalid_grant")
			return
		}
		delete(is.as.refresh, r.PostForm.Get("refresh_token")) // rotated: the old one is spent
	default:
		tokenError(w, http.StatusBadRequest, "unsupported_grant_type")
		return
	}
	is.issue(w, c, g)
}

// issue answers a token request with tokens for g, an ID token among
// them, also on a refresh (OIDC Core §12.2 allows it, and Entra ID and
// others send one); is.mu is held.
func (is *Issuer) issue(w http.ResponseWriter, c Client, g *grant) {
	now := time.Now()
	common := func(aud string) map[string]any {
		claims := map[string]any{"iss": is.URL, "aud": aud, "sub": g.user.Subject, "iat": now.Unix(), "exp": now.Add(is.as.ttl).Unix(),
			"sid": g.sid, "auth_time": g.authTime.Unix()}
		for k, v := range g.claims {
			claims[k] = v
		}
		return claims
	}
	access := common(Audience)
	access["tenant_id"] = g.user.Tenant.String()
	access["scope"] = g.scope
	if is.as.hooks.AccessToken != nil {
		is.as.hooks.AccessToken(access)
	}
	signer := is.keys[len(is.keys)-1].signer
	out := map[string]any{"token_type": "Bearer", "expires_in": int(is.as.ttl.Seconds()), "access_token": signRaw(signer, access), "scope": g.scope}
	id := common(c.ID)
	if g.nonce != "" {
		id["nonce"] = g.nonce
	}
	if is.as.hooks.IDToken != nil {
		is.as.hooks.IDToken(id)
	}
	out["id_token"] = signRaw(signer, id)
	if c.Refresh {
		rt := random()
		is.as.refresh[rt] = g
		out["refresh_token"] = rt
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out) // a failed response write means the client is gone: no one is left to tell
}

// client authenticates the token request's client: a client assertion
// (private_key_jwt) or HTTP Basic (client_secret_basic); is.mu is held.
func (is *Issuer) client(r *http.Request) (Client, bool) {
	if r.PostForm.Get("client_assertion_type") == "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" {
		return is.assertion(r.PostForm.Get("client_assertion"), r.PostForm.Get("client_id"))
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		return Client{}, false
	}
	id, err1 := url.QueryUnescape(id)
	secret, err2 := url.QueryUnescape(secret)
	c, known := is.as.clients[id]
	return c, known && err1 == nil && err2 == nil && c.Key == nil && c.Secret != "" && secret == c.Secret
}

// assertion checks a client assertion (RFC 7523 §3): signed with the
// client's key, issued by and about the client, for this token endpoint,
// unexpired and never used before.
func (is *Issuer) assertion(raw, clientID string) (Client, bool) {
	jws, err := jose.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256, jose.ES256, jose.PS256})
	if err != nil {
		return Client{}, false
	}
	var claims struct {
		Iss, Sub, Jti string
		Aud           any
		Exp           int64
	}
	if err := json.Unmarshal(jws.UnsafePayloadWithoutVerification(), &claims); err != nil {
		return Client{}, false
	}
	c, known := is.as.clients[claims.Sub]
	if !known || c.Key == nil {
		return Client{}, false
	}
	if _, err := jws.Verify(c.Key); err != nil {
		return Client{}, false
	}
	aud := map[string]bool{}
	switch a := claims.Aud.(type) {
	case string:
		aud[a] = true
	case []any:
		for _, v := range a {
			s, _ := v.(string)
			aud[s] = true
		}
	}
	if claims.Iss != c.ID || (clientID != "" && clientID != c.ID) || !aud[is.URL+TokenPath] || claims.Jti == "" ||
		is.as.assertions[claims.Jti] || time.Unix(claims.Exp, 0).Before(time.Now()) {
		return Client{}, false
	}
	is.as.assertions[claims.Jti] = true
	return c, true
}

// endSession is RP-initiated logout: it records the request and returns
// to the client's registered post-logout URI, with its state.
func (is *Issuer) endSession(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	is.mu.Lock()
	is.as.logouts = append(is.as.logouts, q)
	c, ok := is.as.clients[q.Get("client_id")]
	is.mu.Unlock()
	if back := q.Get("post_logout_redirect_uri"); ok && back != "" && back == c.PostLogoutRedirectURI {
		u := back
		if st := q.Get("state"); st != "" {
			u += "?" + url.Values{"state": {st}}.Encode()
		}
		http.Redirect(w, r, u, http.StatusFound) // #nosec G710 -- only to the client's registered post-logout URI
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("signed out")) // a failed response write means the client is gone: no one is left to tell
}

func tokenError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code}) // a failed response write means the client is gone: no one is left to tell
}

// signRaw signs claims; is test support, so a failure to sign, which a
// valid key never has, yields an empty token the client refuses.
func signRaw(signer jose.Signer, claims map[string]any) string {
	payload, _ := json.Marshal(claims) // test claims are strings, numbers and lists
	sig, err := signer.Sign(payload)
	if err != nil {
		return ""
	}
	raw, _ := sig.CompactSerialize() // a signature just made always serializes
	return raw
}

// TokenFor mints an access token as the token endpoint would for u, with
// session id sid, for tests that need a session's token without the flow.
func (is *Issuer) TokenFor(tb testing.TB, u User, sid string) string {
	tb.Helper()
	extra := map[string]any{"sid": sid, "auth_time": time.Now().Unix()}
	for k, v := range u.Claims {
		extra[k] = v
	}
	return is.Token(tb, u.Tenant, u.Subject, extra)
}
