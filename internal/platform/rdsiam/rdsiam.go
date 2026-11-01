// Package rdsiam authenticates a database login with RDS IAM database
// authentication (ADR-0035, amendment 2026-10-28 and its As built of
// 2026-11-01): in production the router's, the cells' and erp-web's
// directory logins present, for each new connection, a token minted from
// the workload's IRSA role (ADR-0024) in place of a password.
//
// A token is a Signature Version 4 presigned GET of the database's
// endpoint with Action=connect and DBUser, for the service rds-db, valid
// 900 seconds, signing the empty payload's hash, with the scheme cut off
// (AWS SDK for Go v2, feature/rds/auth, BuildAuthToken; RDS user guide,
// "IAM database authentication": "Authentication tokens are generated
// using AWS Signature Version 4. Each token has a lifetime of 15
// minutes."). It is checked only when a connection authenticates ("The
// token is only used for authentication and doesn't affect the session
// after it is established"), so a pool mints one per new connection and
// none for the connections it holds.
package rdsiam

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/awscreds"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/sigv4"
)

const (
	// Service is the signing name of RDS IAM database authentication.
	Service = "rds-db"
	// Validity is a token's lifetime, the X-Amz-Expires it is signed with.
	Validity = 900 * time.Second
	// emptyPayload is the SHA-256 of an empty body, which the AWS SDK
	// signs as the token request's payload.
	emptyPayload = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// ErrSettings means production's IAM authentication is not configured:
// startup fails rather than connect another way.
var ErrSettings = errors.New("rdsiam: settings")

// Token mints the tokens of one login at one endpoint.
type Token struct {
	Endpoint    string // host:port, as the connection names the server
	User        string // the database login, its case as in the database
	Region      string // the database's AWS region
	Credentials awscreds.Provider
	Now         func() time.Time // nil: time.Now
}

// Mint returns a fresh token from the credentials the provider holds now.
func (t Token) Mint(ctx context.Context) (string, error) {
	c, err := t.Credentials.Credentials(ctx)
	if err != nil {
		return "", fmt.Errorf("rdsiam: credentials: %w", err)
	}
	u := url.URL{Scheme: "https", Host: t.Endpoint, RawQuery: url.Values{"Action": {"connect"}, "DBUser": {t.User}}.Encode()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("rdsiam: token request: %w", err)
	}
	signed := sigv4.PresignPayload(req, sigv4.Credentials{AccessKey: c.AccessKey, SecretKey: c.SecretKey, SessionToken: c.SessionToken},
		t.Region, Service, emptyPayload, Validity, clock.Now(t.Now))
	return strings.TrimPrefix(signed, "https://"), nil
}

// ForURL is the login's tokens for a postgres URL naming one host: its
// host and port (5432 when none is named) and its user. A token is signed
// for the endpoint the connection names, so a URL with fallback hosts,
// each of which would need its own, is refused.
func ForURL(dbURL, region string, creds awscreds.Provider) (Token, error) {
	u, err := url.Parse(dbURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.User == nil || u.User.Username() == "" {
		return Token{}, fmt.Errorf("%w: want a postgres URL naming its login", ErrSettings)
	}
	if _, set := u.User.Password(); set {
		return Token{}, fmt.Errorf("%w: the URL carries a password; the login authenticates with IAM tokens alone", ErrSettings)
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = "5432"
	}
	if n, err := strconv.Atoi(port); host == "" || strings.Contains(u.Host, ",") || err != nil || n < 1 || n > 65535 {
		return Token{}, fmt.Errorf("%w: want a URL naming one host and port, not %q", ErrSettings, u.Host)
	}
	return Token{Endpoint: net.JoinHostPort(host, port), User: u.User.Username(), Region: region, Credentials: creds}, nil
}

// Settings name, for one client, the settings Load reads.
type Settings struct {
	URLName    string // the database URL's setting, for messages
	URL        string // its value
	RegionName string // the setting naming the database's AWS region
	Refused    string // the password setting production refuses
	// STSClient reaches STS; nil: a client with a 10 s timeout and the
	// system's roots.
	STSClient *http.Client
}

// regionPattern is an AWS region's name, such as eu-central-1.
var regionPattern = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)

// Load reads production's IAM authentication of a login: the database's
// region, and the workload's IRSA role (AWS_ROLE_ARN and
// AWS_WEB_IDENTITY_TOKEN_FILE, which EKS sets for a service account
// annotated with its role), exchanged at STS (ERP_AWS_STS_ENDPOINT, an
// https URL; default the region's endpoint) and cached until shortly
// before the credentials end. No other credentials are tried, so a pod
// without its role fails startup rather than sign as its node; and a
// password setting is refused, so a password is never used in its place.
func Load(getenv func(string) string, s Settings) (func(context.Context) (string, error), error) {
	var errs []error
	if getenv(s.Refused) != "" {
		errs = append(errs, fmt.Errorf("%w: %s must not be set with ERP_ENV=production: the login authenticates with RDS IAM tokens", ErrSettings, s.Refused))
	}
	region := getenv(s.RegionName)
	if !regionPattern.MatchString(region) {
		errs = append(errs, fmt.Errorf("%w: %s is required with ERP_ENV=production: the directory's AWS region, such as eu-central-1, which its IAM tokens are signed for", ErrSettings, s.RegionName))
	}
	role, file := getenv("AWS_ROLE_ARN"), getenv("AWS_WEB_IDENTITY_TOKEN_FILE")
	if role == "" || file == "" {
		errs = append(errs, fmt.Errorf("%w: AWS_ROLE_ARN and AWS_WEB_IDENTITY_TOKEN_FILE are required with ERP_ENV=production: the workload's IRSA role, which may connect as its login", ErrSettings))
	}
	sts := getenv("ERP_AWS_STS_ENDPOINT")
	if sts == "" {
		sts = "https://sts." + region + ".amazonaws.com"
	}
	if u, err := url.Parse(sts); err != nil || u.Scheme != "https" || u.Host == "" {
		errs = append(errs, fmt.Errorf("%w: ERP_AWS_STS_ENDPOINT must be an https URL", ErrSettings))
	}
	creds := &awscreds.Cached{Provider: awscreds.WebIdentity{TokenFile: file, RoleARN: role, SessionName: getenv("AWS_ROLE_SESSION_NAME"), Endpoint: sts, Client: s.STSClient}}
	t, err := ForURL(s.URL, region, creds)
	if err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", s.URLName, err))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return t.Mint, nil
}
