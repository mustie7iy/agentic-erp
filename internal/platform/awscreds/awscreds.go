// Package awscreds resolves AWS credentials for the platform's own
// Signature Version 4 clients (the S3 store, the KMS master) without the AWS
// SDK (ADR-0024): static keys from settings, else the ambient ones a
// deployment provides: the AWS_* environment, a web identity (IRSA: a
// Kubernetes service account token exchanged at STS for a role), or the
// instance metadata service (an instance or task role). Temporary
// credentials are cached and renewed before they expire.
package awscreds

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
)

// Credentials sign requests. SessionToken and Expires are empty for
// long-lived keys.
type Credentials struct {
	AccessKey    string
	SecretKey    string
	SessionToken string
	Expires      time.Time
}

// Provider yields credentials; ErrNone when it has none to give (the next
// provider of a chain is tried).
type Provider interface {
	Credentials(ctx context.Context) (Credentials, error)
}

// ErrNone means a provider has no credentials here (not an error of the
// provider: the environment is not set, the metadata service is absent).
var ErrNone = errors.New("awscreds: no credentials")

// Static are keys from settings.
type Static struct {
	AccessKey, SecretKey string
}

// Credentials implements Provider.
func (s Static) Credentials(context.Context) (Credentials, error) {
	if s.AccessKey == "" || s.SecretKey == "" {
		return Credentials{}, ErrNone
	}
	return Credentials{AccessKey: s.AccessKey, SecretKey: s.SecretKey}, nil
}

// Env reads AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and AWS_SESSION_TOKEN.
type Env struct {
	Getenv func(string) string // nil: os.Getenv
}

func (e Env) getenv(name string) string {
	if e.Getenv != nil {
		return e.Getenv(name)
	}
	return os.Getenv(name)
}

// Credentials implements Provider.
func (e Env) Credentials(context.Context) (Credentials, error) {
	c := Credentials{AccessKey: e.getenv("AWS_ACCESS_KEY_ID"), SecretKey: e.getenv("AWS_SECRET_ACCESS_KEY"), SessionToken: e.getenv("AWS_SESSION_TOKEN")}
	if c.AccessKey == "" || c.SecretKey == "" {
		return Credentials{}, ErrNone
	}
	return c, nil
}

// WebIdentity exchanges a service account token (AWS_WEB_IDENTITY_TOKEN_FILE)
// for a role's temporary credentials (AWS_ROLE_ARN) at STS, the way IRSA
// does.
type WebIdentity struct {
	TokenFile   string
	RoleARN     string
	SessionName string       // default erp
	Endpoint    string       // default https://sts.amazonaws.com
	Client      *http.Client // nil: a client with a 10 s timeout
}

// Credentials implements Provider with AssumeRoleWithWebIdentity.
func (w WebIdentity) Credentials(ctx context.Context) (Credentials, error) {
	if w.TokenFile == "" || w.RoleARN == "" {
		return Credentials{}, ErrNone
	}
	token, err := os.ReadFile(w.TokenFile)
	if err != nil {
		return Credentials{}, fmt.Errorf("awscreds: web identity token: %w", err)
	}
	session := w.SessionName
	if session == "" {
		session = "erp"
	}
	endpoint := w.Endpoint
	if endpoint == "" {
		endpoint = "https://sts.amazonaws.com"
	}
	form := url.Values{"Action": {"AssumeRoleWithWebIdentity"}, "Version": {"2011-06-15"}, "RoleArn": {w.RoleARN}, "RoleSessionName": {session},
		"WebIdentityToken": {strings.TrimSpace(string(token))}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Credentials{}, fmt.Errorf("awscreds: sts: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/xml")
	resp, err := client(w.Client).Do(req)
	if err != nil {
		return Credentials{}, fmt.Errorf("awscreds: sts: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()               // only read from: closing frees the connection and cannot change the answer
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // a body cut short fails to decode below, or shortens a refusal's message
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Code    string `xml:"Error>Code"`
			Message string `xml:"Error>Message"`
		}
		_ = xml.Unmarshal(raw, &e) // a body that is not the STS error document leaves the fields empty
		return Credentials{}, fmt.Errorf("awscreds: sts: %d %s %s", resp.StatusCode, e.Code, strings.TrimSpace(e.Message))
	}
	var out struct {
		AccessKey    string `xml:"AssumeRoleWithWebIdentityResult>Credentials>AccessKeyId"`
		SecretKey    string `xml:"AssumeRoleWithWebIdentityResult>Credentials>SecretAccessKey"`
		SessionToken string `xml:"AssumeRoleWithWebIdentityResult>Credentials>SessionToken"`
		Expiration   string `xml:"AssumeRoleWithWebIdentityResult>Credentials>Expiration"`
	}
	if err := xml.Unmarshal(raw, &out); err != nil || out.AccessKey == "" {
		return Credentials{}, fmt.Errorf("awscreds: sts: answer is not an AssumeRoleWithWebIdentity response")
	}
	expires, err := expiry(out.SessionToken, out.Expiration)
	if err != nil {
		return Credentials{}, fmt.Errorf("awscreds: sts: %w", err)
	}
	return Credentials{AccessKey: out.AccessKey, SecretKey: out.SecretKey, SessionToken: out.SessionToken, Expires: expires}, nil
}

// expiry parses the end of credentials that came with the session token.
// Absent on keys without a token, it is zero, which Cached treats as
// long-lived. Absent on temporary credentials (a token), or present but
// unreadable, it is an error: taking it for "never" would keep using the
// credentials after they end.
func expiry(token, s string) (time.Time, error) {
	if s == "" {
		if token != "" {
			return time.Time{}, errors.New("a session token without an expiration")
		}
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("expiration %q: %w", s, err)
	}
	return t, nil
}

// IMDS reads the instance's role credentials from the EC2 instance metadata
// service, version 2 (a session token first).
type IMDS struct {
	Endpoint string       // default http://169.254.169.254
	Client   *http.Client // nil: imdsClient, direct with a 2 s timeout, the service being link-local
}

// imdsClient reaches the service directly, never through HTTP_PROXY: its
// plaintext is exempt from TLS only because it never leaves the host
// (ADR-0033), and Go's proxy rules exempt loopback, not link-local
// addresses, so the role's credentials would otherwise cross the network.
func imdsClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert // the standard library's default is an *http.Transport
	t.Proxy = nil
	return &http.Client{Timeout: 2 * time.Second, Transport: t}
}

// Credentials implements Provider; an absent service is ErrNone.
func (m IMDS) Credentials(ctx context.Context) (Credentials, error) {
	endpoint := m.Endpoint
	if endpoint == "" {
		endpoint = "http://169.254.169.254"
	}
	c := m.Client
	if c == nil {
		c = imdsClient()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint+"/latest/api/token", nil)
	if err != nil {
		return Credentials{}, fmt.Errorf("awscreds: imds: %w", err)
	}
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "21600")
	resp, err := c.Do(req)
	if err != nil {
		return Credentials{}, ErrNone // no metadata service here
	}
	token, _ := io.ReadAll(io.LimitReader(resp.Body, 4096)) // a token cut short is refused by the reads that use it
	_ = resp.Body.Close()                                   // the token has been read; closing frees the connection and cannot change it
	if resp.StatusCode != http.StatusOK {
		return Credentials{}, ErrNone
	}
	get := func(path string) ([]byte, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/latest/meta-data/iam/security-credentials/"+path, nil) // the endpoint parsed above
		req.Header.Set("X-aws-ec2-metadata-token", string(token))
		resp, err := c.Do(req)
		if err != nil {
			return nil, fmt.Errorf("awscreds: imds: %w", err)
		}
		defer func() { _ = resp.Body.Close() }() // only read from: closing frees the connection and cannot change the answer
		// A body cut short fails at the next step: the role it names is not
		// found, or the credentials document does not parse.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("awscreds: imds: %s: %d", path, resp.StatusCode)
		}
		return body, nil
	}
	role, err := get("")
	if err != nil {
		return Credentials{}, err
	}
	raw, err := get(strings.TrimSpace(strings.Split(string(role), "\n")[0]))
	if err != nil {
		return Credentials{}, err
	}
	return imdsDocument(raw)
}

// imdsDocument reads the credentials document of the instance's role.
func imdsDocument(raw []byte) (Credentials, error) {
	var out struct {
		Code            string
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string
		Token           string
		Expiration      string
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.AccessKeyID == "" {
		return Credentials{}, fmt.Errorf("awscreds: imds: answer is not a credentials document")
	}
	expires, err := expiry(out.Token, out.Expiration)
	if err != nil {
		return Credentials{}, fmt.Errorf("awscreds: imds: %w", err)
	}
	return Credentials{AccessKey: out.AccessKeyID, SecretKey: out.SecretAccessKey, SessionToken: out.Token, Expires: expires}, nil
}

// Chain tries providers in order and uses the first that has credentials.
type Chain []Provider

// Credentials implements Provider; ErrNone when no provider has any.
func (c Chain) Credentials(ctx context.Context) (Credentials, error) {
	for _, p := range c {
		creds, err := p.Credentials(ctx)
		if errors.Is(err, ErrNone) {
			continue
		}
		return creds, err
	}
	return Credentials{}, ErrNone
}

// Cached keeps a provider's credentials until shortly before they expire.
type Cached struct {
	Provider Provider
	Early    time.Duration    // renew this long before expiry; default 5 minutes
	Now      func() time.Time // nil: time.Now

	mu    sync.Mutex
	creds Credentials
	held  bool
}

// Credentials implements Provider.
func (c *Cached) Credentials(ctx context.Context) (Credentials, error) {
	now := clock.Now(c.Now)
	early := c.Early
	if early <= 0 {
		early = 5 * time.Minute
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.held && (c.creds.Expires.IsZero() || now.Before(c.creds.Expires.Add(-early))) {
		return c.creds, nil
	}
	creds, err := c.Provider.Credentials(ctx)
	if err != nil {
		return Credentials{}, err
	}
	c.creds, c.held = creds, true
	return creds, nil
}

// Default is the ambient chain of a deployment: the environment, a web
// identity, the instance metadata service; with static keys given they come
// first. Temporary credentials are cached. STS is reached with stsClient
// (nil: a client with a 10 s timeout); the instance metadata service,
// link-local, with its own.
func Default(static Static, getenv func(string) string, stsEndpoint, imdsEndpoint string, stsClient *http.Client) Provider {
	get := getenv
	if get == nil {
		get = os.Getenv
	}
	return &Cached{Provider: Chain{static, Env{Getenv: get},
		WebIdentity{TokenFile: get("AWS_WEB_IDENTITY_TOKEN_FILE"), RoleARN: get("AWS_ROLE_ARN"), SessionName: get("AWS_ROLE_SESSION_NAME"), Endpoint: stsEndpoint, Client: stsClient},
		IMDS{Endpoint: imdsEndpoint}}}
}

func client(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: 10 * time.Second}
}
