package rdsiam_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/awscreds"
	"github.com/mustie7iy/agentic-erp/internal/platform/rdsiam"
)

var made = time.Date(2026, 11, 1, 12, 30, 45, 0, time.UTC)

type static awscreds.Credentials

func (s static) Credentials(context.Context) (awscreds.Credentials, error) {
	return awscreds.Credentials(s), nil
}

type failing struct{}

func (failing) Credentials(context.Context) (awscreds.Credentials, error) {
	return awscreds.Credentials{}, awscreds.ErrNone
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

// TestMint: the token is the AWS SDK's (feature/rds/auth BuildAuthToken):
// the endpoint and the presigned query of GET / with Action=connect and
// DBUser, for rds-db in the region, 900 seconds, the empty payload's hash
// signed. The signature is recomputed here from the Signature Version 4
// steps written out by hand (canonical request, string to sign, the key
// derived from the secret through date, region, service and
// aws4_request), apart from the signer under test.
func TestMint(t *testing.T) {
	creds := static{AccessKey: "AKIDEXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", SessionToken: "FwoGZXIvYXdz/session+token="}
	tok := rdsiam.Token{Endpoint: "erp-dev-directory.abc.eu-central-1.rds.amazonaws.com:5432", User: "erp_router_login", Region: "eu-central-1",
		Credentials: creds, Now: func() time.Time { return made }}
	got, err := tok.Mint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	query := "Action=connect&DBUser=erp_router_login&X-Amz-Algorithm=AWS4-HMAC-SHA256" +
		"&X-Amz-Credential=AKIDEXAMPLE%2F20261101%2Feu-central-1%2Frds-db%2Faws4_request&X-Amz-Date=20261101T123045Z&X-Amz-Expires=900" +
		"&X-Amz-Security-Token=FwoGZXIvYXdz%2Fsession%2Btoken%3D&X-Amz-SignedHeaders=host"
	canonical := "GET\n/\n" + query + "\nhost:erp-dev-directory.abc.eu-central-1.rds.amazonaws.com:5432\n\nhost\n" +
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n20261101T123045Z\n20261101/eu-central-1/rds-db/aws4_request\n" + hex.EncodeToString(sum[:])
	key := []byte("AWS4" + creds.SecretKey)
	for _, part := range []string{"20261101", "eu-central-1", "rds-db", "aws4_request"} {
		key = hmacSHA256(key, part)
	}
	// The query comes sorted, as it was signed: the signature before
	// X-Amz-SignedHeaders.
	want := "erp-dev-directory.abc.eu-central-1.rds.amazonaws.com:5432?" + strings.TrimSuffix(query, "&X-Amz-SignedHeaders=host") +
		"&X-Amz-Signature=" + hex.EncodeToString(hmacSHA256(key, toSign)) + "&X-Amz-SignedHeaders=host"
	if got != want {
		t.Errorf("token:\n got %s\nwant %s", got, want)
	}
	tok.Credentials = failing{}
	if _, err := tok.Mint(context.Background()); !errors.Is(err, awscreds.ErrNone) {
		t.Errorf("without credentials: got %v, want awscreds.ErrNone", err)
	}
	tok.Credentials, tok.Endpoint = creds, "bad host:5432"
	if _, err := tok.Mint(context.Background()); err == nil {
		t.Error("an endpoint no URL can name: got a token, want an error")
	}
}

// TestForURL: the token's endpoint and login are the URL's, its port
// 5432 when it names none; a URL naming no login, a password, more than
// one host or no host is refused.
func TestForURL(t *testing.T) {
	for name, c := range map[string]struct{ url, endpoint, user string }{
		"host and port": {"postgres://erp_cell_login@db.example:6543/directory?sslmode=verify-full", "db.example:6543", "erp_cell_login"},
		"default port":  {"postgresql://erp_web_login@db.example/directory", "db.example:5432", "erp_web_login"},
	} {
		t.Run(name, func(t *testing.T) {
			tok, err := rdsiam.ForURL(c.url, "eu-central-1", failing{})
			if err != nil {
				t.Fatal(err)
			}
			if tok.Endpoint != c.endpoint {
				t.Errorf("endpoint: got %q, want %q", tok.Endpoint, c.endpoint)
			}
			if tok.User != c.user {
				t.Errorf("user: got %q, want %q", tok.User, c.user)
			}
			if tok.Region != "eu-central-1" {
				t.Errorf("region: got %q, want eu-central-1", tok.Region)
			}
		})
	}
	for name, u := range map[string]string{
		"not a URL":            "postgres://u@db.example:5432/%zz",
		"another kind":         "mysql://u@db.example:3306/d",
		"no login":             "postgres://db.example:5432/d",
		"a password":           "postgres://u:secret@db.example:5432/d",
		"no host":              "postgres://u@:5432/d",
		"bad port":             "postgres://u@db.example:99999/d",
		"two hosts":            "postgres://u@a.example,b.example/d",
		"a host parameter":     "postgres://u@db.example:5432/d?host=other.example",
		"a hostaddr parameter": "postgres://u@db.example:5432/d?hostaddr=10.0.0.1",
		"a port parameter":     "postgres://u@db.example:5432/d?port=6432",
		"a port not a number":  "postgres://u@db.example:x/d",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := rdsiam.ForURL(u, "eu-central-1", failing{}); !errors.Is(err, rdsiam.ErrSettings) {
				t.Errorf("got %v, want ErrSettings", err)
			}
		})
	}
}

// env is a fixed environment.
func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// TestLoad: production's settings give a token function that exchanges
// the IRSA token at STS and signs for the region; each missing or wrong
// setting fails, a password setting included.
func TestLoad(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("eyJ.web.identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls int
	sts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if err := r.ParseForm(); err != nil || r.Form.Get("RoleArn") != "arn:aws:iam::222222222222:role/erp-dev1-router" || r.Form.Get("WebIdentityToken") != "eyJ.web.identity" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`<AssumeRoleWithWebIdentityResponse><AssumeRoleWithWebIdentityResult><Credentials>` +
			`<AccessKeyId>ASIAEXAMPLE</AccessKeyId><SecretAccessKey>secret</SecretAccessKey><SessionToken>tok</SessionToken>` +
			`<Expiration>2099-01-01T00:00:00Z</Expiration></Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`))
	}))
	defer sts.Close()
	settings := rdsiam.Settings{URLName: "ERP_DIRECTORY_URL", URL: "postgres://erp_router_login@db.example:5432/directory", RegionName: "ERP_DIRECTORY_REGION", Refused: "ERP_DIRECTORY_PASSWORD", STSClient: sts.Client()}
	good := map[string]string{"ERP_DIRECTORY_REGION": "eu-central-1", "AWS_ROLE_ARN": "arn:aws:iam::222222222222:role/erp-dev1-router",
		"AWS_WEB_IDENTITY_TOKEN_FILE": file, "ERP_AWS_STS_ENDPOINT": sts.URL}
	mint, err := rdsiam.Load(env(good), settings)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		tok, err := mint(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(tok, "db.example:5432?Action=connect&DBUser=erp_router_login&") || !strings.Contains(tok, "X-Amz-Credential=ASIAEXAMPLE%2F") ||
			!strings.Contains(tok, "%2Feu-central-1%2Frds-db%2Faws4_request") || !strings.Contains(tok, "X-Amz-Security-Token=tok&") {
			t.Errorf("token %q: want the router's, signed for rds-db in eu-central-1 with the role's credentials", tok)
		}
	}
	if calls != 1 {
		t.Errorf("STS calls: got %d, want 1 (the role's credentials cached)", calls)
	}
	for name, change := range map[string]map[string]string{
		"a password":       {"ERP_DIRECTORY_PASSWORD": "file:/etc/erp/secrets/directory-password"},
		"no region":        {"ERP_DIRECTORY_REGION": ""},
		"a bad region":     {"ERP_DIRECTORY_REGION": "Frankfurt"},
		"no role":          {"AWS_ROLE_ARN": ""},
		"no token file":    {"AWS_WEB_IDENTITY_TOKEN_FILE": ""},
		"STS over http":    {"ERP_AWS_STS_ENDPOINT": "http://sts.example"},
		"no URL":           {"url": ""},
		"STS not a URL":    {"ERP_AWS_STS_ENDPOINT": "https://%zz"},
		"STS without host": {"ERP_AWS_STS_ENDPOINT": "https://"},
	} {
		t.Run(name, func(t *testing.T) {
			m, s := map[string]string{}, settings
			for k, v := range good {
				m[k] = v
			}
			for k, v := range change {
				if k == "url" {
					s.URL = v
					continue
				}
				m[k] = v
			}
			if _, err := rdsiam.Load(env(m), s); !errors.Is(err, rdsiam.ErrSettings) {
				t.Errorf("got %v, want ErrSettings", err)
			}
		})
	}
	t.Run("the region's STS by default", func(t *testing.T) {
		m := map[string]string{}
		for k, v := range good {
			m[k] = v
		}
		delete(m, "ERP_AWS_STS_ENDPOINT")
		if _, err := rdsiam.Load(env(m), settings); err != nil {
			t.Errorf("got %v, want the region's endpoint taken", err)
		}
	})
}
