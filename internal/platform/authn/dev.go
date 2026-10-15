package authn

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// The development issuer stands in for a tenant's identity provider and for
// the operators' on a developer's machine: the server mints tokens for any
// subject of any tenant, and operator tokens, on its admin listener. It is enabled explicitly (ERP_DEV_ISSUER)
// and the server logs a warning while it runs; nothing about it belongs in
// production, where tokens come from the tenant's identity provider.

// DevIssuerURL and DevAudience are the issuer and audience of development
// tokens.
const (
	DevIssuerURL = "erp-dev"
	DevAudience  = "erp-api"
)

// DevTokenTTL is how long a development token is valid.
const DevTokenTTL = time.Hour

// DevIssuer signs development tokens with a key made at start.
type DevIssuer struct {
	key    *rsa.PrivateKey
	signer jose.Signer
	Now    func() time.Time
}

// NewDevIssuer makes a signing key.
func NewDevIssuer() *DevIssuer {
	// Generating an RSA key from the system's random source and making a
	// signer from it cannot fail on supported platforms.
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "dev"}}, nil)
	return &DevIssuer{key: key, signer: signer}
}

// Static is the issuer as the verifier trusts it, without discovery.
func (d *DevIssuer) Static() Static {
	return Static{Issuer: Issuer{URL: DevIssuerURL, Audience: DevAudience}, Keys: &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&d.key.PublicKey}}}
}

func (d *DevIssuer) now() time.Time {
	return clock.Now(d.Now)
}

// Token mints a token for the subject in the tenant; with no tenant
// (uuid.Nil) it mints an operator token, which names none.
func (d *DevIssuer) Token(tenant uuid.UUID, subject string) string {
	now := d.now()
	claims := map[string]any{"iss": DevIssuerURL, "aud": DevAudience, "sub": subject, "iat": now.Unix(), "exp": now.Add(DevTokenTTL).Unix()}
	if tenant != uuid.Nil {
		claims[DefaultTenantClaim] = tenant.String()
	}
	payload, _ := json.Marshal(claims) // strings and numbers always encode
	// Signing a JSON payload with a valid RSA key cannot fail.
	sig, _ := d.signer.Sign(payload)
	tok, _ := sig.CompactSerialize() // a signature just made always serializes
	return tok
}

// Handler mints tokens: POST {"tenant_id": ..., "subject": ...} answers
// {"token": ..., "expires_in": seconds}; without tenant_id the token is an
// operator's. It belongs on the admin listener.
func (d *DevIssuer) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			TenantID string `json:"tenant_id"`
			Subject  string `json:"subject"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
			problem.Write(w, r, problem.BadRequest("request.invalid_body", "The body must be JSON with tenant_id and subject."))
			return
		}
		var tenant uuid.UUID
		var err error
		if in.TenantID != "" {
			tenant, err = uuid.Parse(in.TenantID)
		}
		if err != nil || in.Subject == "" {
			problem.Write(w, r, problem.BadRequest("request.invalid_body", "tenant_id must be a UUID when given and subject must not be empty."))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"token": d.Token(tenant, in.Subject), "expires_in": int(DevTokenTTL.Seconds())}) // a failed response write means the client is gone: no one is left to tell
	})
}
