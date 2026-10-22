package web

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha1" // #nosec G505 -- x5t is the certificate's SHA-1 thumbprint by definition (RFC 7515 §4.1.7); it identifies, it does not protect
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
	"github.com/mustie7iy/agentic-erp/internal/web/session"
)

// OIDC is erp-web as a confidential client of each tenant's identity
// provider (ADR-0052 §5): the authorization code flow with PKCE (S256),
// state and nonce, the exact redirect URI from the public URL, the code
// exchanged with private_key_jwt (or a client secret where the provider
// offers nothing else), the ID token verified with the API's own verifier
// and key cache (authn.Providers) for the client, and the access token for
// the registration's audience. Every call to a provider goes through
// Providers' client: its timeout, https and public addresses only in
// production.
type OIDC struct {
	Providers *authn.Providers
	// Credentials resolve the web clients' credential references; only
	// references starting with Prefix are resolved, so a registration
	// cannot point erp-web at another of its secrets (the session key) and
	// have it sent to a provider as a client secret.
	Credentials secrets.Resolver
	Prefix      string
	Callback    string // the redirect URI: <public URL>/callback
	SignedOut   string // where the provider's logout returns: <public URL>/signed-out
	Now         func() time.Time
}

func (o *OIDC) now() time.Time { return clock.Now(o.Now) }

// signIn is one sign-in under way, sealed into its cookie: what the
// callback checks the provider's answer against.
type signIn struct {
	Code     string    `json:"code"` // the login code
	Issuer   string    `json:"issuer"`
	TenantID uuid.UUID `json:"tenant_id"` // the code's provider and tenant when the sign-in began (clientOf)
	State    string    `json:"state"`
	Nonce    string    `json:"nonce"`
	Verifier string    `json:"verifier"` // PKCE (RFC 7636): 256 random bits, 43 characters
	Return   string    `json:"return"`   // a same-site path, never a URL
	ACR      string    `json:"acr,omitempty"`
	MaxAge   *int      `json:"max_age,omitempty"`
	Previous string    `json:"previous,omitempty"` // the session a step-up replaces
	Expires  time.Time `json:"expires"`
}

// stepUp reports a sign-in made for a step-up challenge (RFC 9470).
func (t signIn) stepUp() bool { return t.ACR != "" || t.MaxAge != nil }

// challenge is PKCE's S256 code challenge of the verifier.
func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// authorizeURL is where the person signs in at the provider.
func (o *OIDC) authorizeURL(ctx context.Context, c session.Client, t signIn) (string, error) {
	e, err := o.Providers.Endpoints(ctx, c.Issuer)
	if err != nil {
		return "", err
	}
	q := url.Values{"response_type": {"code"}, "client_id": {c.ClientID}, "redirect_uri": {o.Callback}, "scope": {c.Scope},
		"state": {t.State}, "nonce": {t.Nonce}, "code_challenge": {challenge(t.Verifier)}, "code_challenge_method": {"S256"}}
	if t.ACR != "" {
		q.Set("acr_values", t.ACR)
	}
	if t.MaxAge != nil {
		q.Set("max_age", strconv.Itoa(*t.MaxAge))
	}
	return join(e.Authorization, q), nil
}

// join adds the query to an endpoint that may carry one of its own.
func join(endpoint string, q url.Values) string {
	if strings.Contains(endpoint, "?") {
		return endpoint + "&" + q.Encode()
	}
	return endpoint + "?" + q.Encode()
}

// tokenSet is the token endpoint's answer (RFC 6749 §5.1, OIDC Core §3.1.3.3).
type tokenSet struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
}

// errRefused is a provider's refusal of a grant: the sign-in failed, or
// the session cannot go on, as opposed to a provider out of reach (503).
type errRefused struct {
	status int
	code   string
}

func (e errRefused) Error() string {
	return fmt.Sprintf("web: the provider refused the grant: %d %s", e.status, e.code)
}

// exchange trades the authorization code for tokens.
func (o *OIDC) exchange(ctx context.Context, c session.Client, code, verifier string) (tokenSet, error) {
	return o.token(ctx, c, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {o.Callback}, "code_verifier": {verifier}})
}

// refresh trades a refresh token for new tokens.
func (o *OIDC) refresh(ctx context.Context, c session.Client, refreshToken string) (tokenSet, error) {
	return o.token(ctx, c, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}})
}

// maxTokenResponse bounds what a token endpoint may answer.
const maxTokenResponse = 1 << 20

// token calls the token endpoint as the client, following no redirect.
func (o *OIDC) token(ctx context.Context, c session.Client, form url.Values) (tokenSet, error) {
	e, err := o.Providers.Endpoints(ctx, c.Issuer)
	if err != nil {
		return tokenSet{}, err
	}
	secret, err := o.authenticate(ctx, c, e.Token, form)
	if err != nil {
		return tokenSet{}, err
	}
	// The endpoint parsed as an allowed URL (authn.Providers), and POST
	// is a method: the request is always made.
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.Token, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if secret != "" {
		// RFC 6749 §2.3.1: the id and secret form-encoded, then Basic.
		req.SetBasicAuth(url.QueryEscape(c.ClientID), url.QueryEscape(secret))
	}
	hc := *o.Providers.Client()
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return tokenSet{}, unreachable(c.Issuer, err)
	}
	defer func() { _ = resp.Body.Close() }() // read in full or abandoned; closing loses nothing
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponse))
	if err != nil {
		return tokenSet{}, unreachable(c.Issuer, err)
	}
	return parseTokens(c.Issuer, resp.StatusCode, body)
}

// parseTokens reads a token endpoint's answer: tokens, a refusal (4xx,
// RFC 6749 §5.2), or a provider in trouble (anything else).
func parseTokens(issuer string, status int, body []byte) (tokenSet, error) {
	if status >= 400 && status < 500 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &e) // a refusal without a code is still a refusal
		return tokenSet{}, errRefused{status: status, code: e.Error}
	}
	var ts tokenSet
	if status != http.StatusOK || json.Unmarshal(body, &ts) != nil || ts.AccessToken == "" || !strings.EqualFold(ts.TokenType, "Bearer") {
		return tokenSet{}, unreachable(issuer, fmt.Errorf("web: the token endpoint answered %d without bearer tokens", status))
	}
	return ts, nil
}

func unreachable(issuer string, cause error) error {
	p := problem.Unavailable("The identity provider could not be reached; try again shortly.", 30)
	return p.WithCause(fmt.Errorf("web: token endpoint of %s: %w", issuer, cause))
}

// authenticate adds the client's authentication to a token request: a
// client assertion signed with its key (private_key_jwt, RFC 7523) in the
// form; or it returns the client's secret, which token sends as HTTP Basic
// (client_secret_basic).
func (o *OIDC) authenticate(ctx context.Context, c session.Client, tokenURL string, form url.Values) (string, error) {
	credential, err := o.credential(ctx, c.CredentialRef)
	if err != nil {
		return "", err
	}
	switch c.AuthMethod {
	case "client_secret_basic":
		return credential, nil
	case "private_key_jwt":
		assertion, err := clientAssertion(credential, c.ClientID, tokenURL, o.now())
		if err != nil {
			return "", err
		}
		form.Set("client_id", c.ClientID)
		form.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
		form.Set("client_assertion", assertion)
		return "", nil
	}
	return "", fmt.Errorf("%w: client %s: unknown authentication method %q", errCredential, c.ClientID, c.AuthMethod)
}

// errCredential means a web client's credential cannot be had: a
// configuration fault, which no retry by the person mends.
var errCredential = errors.New("web: the web client's credential cannot be used")

// credential resolves a credential reference under Prefix.
func (o *OIDC) credential(ctx context.Context, ref string) (string, error) {
	if o.Prefix == "" || !strings.HasPrefix(ref, o.Prefix) || strings.Contains(ref, "..") {
		return "", fmt.Errorf("%w: %s is not under %s (ERP_WEB_CLIENT_CREDENTIALS)", errCredential, ref, o.Prefix)
	}
	v, err := o.Credentials.Resolve(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errCredential, err)
	}
	return v, nil
}

// clientAssertion is a JWT the client signs with its private key (OIDC
// Core §9 private_key_jwt, RFC 7523 §3): about and by the client, for the
// token endpoint, used once, valid a minute. The PEM may hold the key's
// certificate after it, whose thumbprints then go in the header (x5t,
// x5t#S256), as Microsoft Entra ID asks; kid is the key's RFC 7638
// thumbprint.
func clientAssertion(pemData, clientID, audience string, now time.Time) (string, error) {
	key, cert, err := parseCredential([]byte(pemData))
	if err != nil {
		return "", err
	}
	alg := jose.RS256
	if _, ok := key.(*ecdsa.PrivateKey); ok {
		alg = jose.ES256
	}
	jwk := jose.JSONWebKey{Key: key}
	public := jwk.Public()
	thumb, _ := public.Thumbprint(crypto.SHA256) // RSA and P-256 keys always have one
	jwk.KeyID = base64.RawURLEncoding.EncodeToString(thumb)
	opts := (&jose.SignerOptions{}).WithType("JWT")
	if cert != nil {
		s1, s256 := sha1.Sum(cert.Raw), sha256.Sum256(cert.Raw) // #nosec G401 -- x5t is SHA-1 by definition (RFC 7515 §4.1.7)
		opts = opts.WithHeader("x5t", base64.RawURLEncoding.EncodeToString(s1[:])).WithHeader("x5t#S256", base64.RawURLEncoding.EncodeToString(s256[:]))
	}
	// parseCredential gives an RSA or P-256 key, for which go-jose makes a
	// signer for RS256 or ES256 and signs, with crypto/rand, which does not
	// fail: neither error can occur.
	signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: jwk}, opts)
	claims, _ := json.Marshal(map[string]any{"iss": clientID, "sub": clientID, "aud": audience, "jti": session.NewSecret(),
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(time.Minute).Unix()}) // strings and numbers always encode
	sig, _ := signer.Sign(claims)
	return sig.CompactSerialize() //nolint:wrapcheck // a signature just made always serializes
}

// parseCredential reads a PEM private key (PKCS #8, PKCS #1 or SEC 1),
// RSA or EC P-256, and the certificate after it, if any.
func parseCredential(data []byte) (crypto.Signer, *x509.Certificate, error) {
	var key crypto.Signer
	var cert *x509.Certificate
	for block, rest := pem.Decode(data); block != nil; block, rest = pem.Decode(rest) {
		switch block.Type {
		case "PRIVATE KEY", "RSA PRIVATE KEY", "EC PRIVATE KEY":
			key = parseKey(block)
		case "CERTIFICATE":
			cert, _ = x509.ParseCertificate(block.Bytes) // a malformed certificate leaves the header without thumbprints
		}
	}
	ec, isEC := key.(*ecdsa.PrivateKey)
	r, isRSA := key.(*rsa.PrivateKey)
	if key == nil || (isEC && ec.Curve != elliptic.P256()) || (isRSA && r.N.BitLen() < 2048) {
		return nil, nil, fmt.Errorf("%w: a PEM RSA (2048 bits at least) or EC P-256 private key is required", errCredential)
	}
	return key, cert, nil
}

func parseKey(block *pem.Block) crypto.Signer {
	switch block.Type {
	case "RSA PRIVATE KEY":
		k, _ := x509.ParsePKCS1PrivateKey(block.Bytes) // nil when malformed, refused by the caller
		if k == nil {
			return nil
		}
		return k
	case "EC PRIVATE KEY":
		k, _ := x509.ParseECPrivateKey(block.Bytes) // nil when malformed, refused by the caller
		if k == nil {
			return nil
		}
		return k
	}
	k, _ := x509.ParsePKCS8PrivateKey(block.Bytes) // nil when malformed, refused by the caller
	switch k := k.(type) {
	case *rsa.PrivateKey:
		return k
	case *ecdsa.PrivateKey:
		return k
	}
	return nil
}

// signedIn is what the tokens say of whoever signed in.
type signedIn struct {
	Subject      string
	ACR          string
	AccessExpiry time.Time
}

// errInvalidTokens means the provider's tokens do not prove this sign-in.
var errInvalidTokens = errors.New("web: the provider's tokens do not prove the sign-in")

// verifyTokens checks the tokens of a code exchange (OIDC Core §3.1.3.7):
// the ID token's signature, issuer, audience (the client), expiry and
// nonce, its azp where it names several audiences, its auth_time where
// max_age was asked; the access token's signature, issuer, audience (the
// registration's) and expiry, and that both are of one subject.
func (o *OIDC) verifyTokens(ctx context.Context, c session.Client, ts tokenSet, t signIn) (signedIn, error) {
	id, err := o.Providers.Verify(ctx, c.Issuer, c.ClientID, ts.IDToken)
	if err != nil {
		return signedIn{}, refusedOr(err)
	}
	var claims struct {
		Azp      string `json:"azp"`
		ACR      string `json:"acr"`
		AuthTime *int64 `json:"auth_time"`
	}
	_ = id.Claims(&claims) // decoded during verification
	switch {
	case subtle.ConstantTimeCompare([]byte(id.Nonce), []byte(t.Nonce)) != 1 || t.Nonce == "":
		return signedIn{}, fmt.Errorf("%w: nonce", errInvalidTokens)
	case len(id.Audience) > 1 && claims.Azp != c.ClientID:
		return signedIn{}, fmt.Errorf("%w: azp", errInvalidTokens)
	case t.MaxAge != nil && (claims.AuthTime == nil || o.now().Sub(time.Unix(*claims.AuthTime, 0)) > time.Duration(*t.MaxAge)*time.Second+clockSkew ||
		time.Unix(*claims.AuthTime, 0).After(o.now().Add(clockSkew))):
		return signedIn{}, fmt.Errorf("%w: auth_time", errInvalidTokens)
	}
	access, err := o.verifyAccess(ctx, c, ts.AccessToken, id.Subject)
	if err != nil {
		return signedIn{}, err
	}
	return signedIn{Subject: id.Subject, ACR: claims.ACR, AccessExpiry: access.Expiry}, nil
}

// verifyID checks an ID token from a refresh (OIDC Core §12.2): the
// provider's, for the client, of the session's subject.
func (o *OIDC) verifyID(ctx context.Context, c session.Client, raw, subject string) (*oidc.IDToken, error) {
	id, err := o.Providers.Verify(ctx, c.Issuer, c.ClientID, raw)
	if err != nil {
		return nil, refusedOr(err)
	}
	if id.Subject != subject {
		return nil, fmt.Errorf("%w: the ID token is another subject's", errInvalidTokens)
	}
	return id, nil
}

// clockSkew is how far a provider's clock may be from erp-web's.
const clockSkew = 2 * time.Minute

// verifyAccess checks an access token is the registration's, of subject.
func (o *OIDC) verifyAccess(ctx context.Context, c session.Client, raw, subject string) (*oidc.IDToken, error) {
	access, err := o.Providers.Verify(ctx, c.Issuer, c.Audience, raw)
	if err != nil {
		return nil, refusedOr(err)
	}
	if access.Subject != subject {
		return nil, fmt.Errorf("%w: the access token is another subject's", errInvalidTokens)
	}
	return access, nil
}

// refusedOr keeps a provider out of reach (503) as it is, and makes any
// other failure of verification an invalid sign-in.
func refusedOr(err error) error {
	if problem.From(err).Status == http.StatusServiceUnavailable {
		return err
	}
	return fmt.Errorf("%w: %w", errInvalidTokens, err)
}

// logoutURL is the provider's RP-initiated logout for the session's ID
// token (OIDC RP-Initiated Logout 1.0 §2), "" when it offers none or
// cannot be asked.
func (o *OIDC) logoutURL(ctx context.Context, c session.Client, idToken string) string {
	e, err := o.Providers.Endpoints(ctx, c.Issuer)
	if err != nil || e.EndSession == "" {
		return ""
	}
	return join(e.EndSession, url.Values{"id_token_hint": {idToken}, "client_id": {c.ClientID}, "post_logout_redirect_uri": {o.SignedOut}})
}

// acrAllowed reports whether the acr a step-up sign-in proved is one the
// challenge asked for.
func acrAllowed(asked, got string) bool {
	return slices.Contains(strings.Fields(asked), got)
}
