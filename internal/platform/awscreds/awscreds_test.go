package awscreds_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/awscreds"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestStaticAndEnv(t *testing.T) {
	ctx := context.Background()
	if _, err := (awscreds.Static{}).Credentials(ctx); !errors.Is(err, awscreds.ErrNone) {
		t.Fatalf("empty static: %v", err)
	}
	if c, err := (awscreds.Static{AccessKey: "AK", SecretKey: "SK"}).Credentials(ctx); err != nil || c.AccessKey != "AK" || c.SessionToken != "" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := (awscreds.Env{Getenv: env(map[string]string{"AWS_ACCESS_KEY_ID": "AK"})}).Credentials(ctx); !errors.Is(err, awscreds.ErrNone) {
		t.Fatalf("half an environment: %v", err)
	}
	c, err := (awscreds.Env{Getenv: env(map[string]string{"AWS_ACCESS_KEY_ID": "AK", "AWS_SECRET_ACCESS_KEY": "SK", "AWS_SESSION_TOKEN": "ST"})}).Credentials(ctx)
	if err != nil || c.AccessKey != "AK" || c.SecretKey != "SK" || c.SessionToken != "ST" {
		t.Fatalf("%+v %v", c, err)
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	if _, err := (awscreds.Env{}).Credentials(ctx); !errors.Is(err, awscreds.ErrNone) {
		t.Fatalf("the process environment: %v", err)
	}
}

func TestWebIdentity(t *testing.T) {
	ctx := context.Background()
	token := filepath.Join(t.TempDir(), "token")
	os.WriteFile(token, []byte("jwt-token\n"), 0o600)
	var mode atomic.Value
	mode.Store("ok")
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("Action") != "AssumeRoleWithWebIdentity" || r.Form.Get("WebIdentityToken") != "jwt-token" || r.Form.Get("RoleArn") != "arn:aws:iam::1:role/erp" || r.Form.Get("RoleSessionName") != "erp" {
			w.WriteHeader(400)
			w.Write([]byte(`<ErrorResponse><Error><Code>InvalidParameter</Code><Message>bad request</Message></Error></ErrorResponse>`))
			return
		}
		switch mode.Load() {
		case "denied":
			w.WriteHeader(403)
			w.Write([]byte(`<ErrorResponse><Error><Code>AccessDenied</Code><Message>Not authorized</Message></Error></ErrorResponse>`))
		case "garbage":
			w.Write([]byte(`<html>`))
		default:
			w.Write([]byte(`<AssumeRoleWithWebIdentityResponse><AssumeRoleWithWebIdentityResult><Credentials>
				<AccessKeyId>ASIA1</AccessKeyId><SecretAccessKey>secret</SecretAccessKey><SessionToken>session</SessionToken><Expiration>2030-01-01T00:00:00Z</Expiration>
				</Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`))
		}
	}))
	defer sts.Close()
	w := awscreds.WebIdentity{TokenFile: token, RoleARN: "arn:aws:iam::1:role/erp", Endpoint: sts.URL}
	c, err := w.Credentials(ctx)
	if err != nil || c.AccessKey != "ASIA1" || c.SecretKey != "secret" || c.SessionToken != "session" || c.Expires.Year() != 2030 {
		t.Fatalf("%+v %v", c, err)
	}
	mode.Store("denied")
	if _, err := w.Credentials(ctx); err == nil || !strings.Contains(err.Error(), "403 AccessDenied Not authorized") {
		t.Fatalf("denied: %v", err)
	}
	mode.Store("garbage")
	if _, err := w.Credentials(ctx); err == nil || !strings.Contains(err.Error(), "not an AssumeRoleWithWebIdentity response") {
		t.Fatalf("garbage: %v", err)
	}
	mode.Store("ok")
	if _, err := (awscreds.WebIdentity{}).Credentials(ctx); !errors.Is(err, awscreds.ErrNone) {
		t.Fatalf("unset: %v", err)
	}
	if _, err := (awscreds.WebIdentity{TokenFile: filepath.Join(t.TempDir(), "none"), RoleARN: "arn", Endpoint: sts.URL}).Credentials(ctx); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("missing token file: %v", err)
	}
	other := w
	other.RoleARN = "arn:aws:iam::1:role/other" // the fake refuses
	if _, err := other.Credentials(ctx); err == nil || !strings.Contains(err.Error(), "InvalidParameter") {
		t.Fatalf("refused: %v", err)
	}
	other = w
	other.Endpoint = "http://127.0.0.1:1"
	if _, err := other.Credentials(ctx); err == nil {
		t.Fatal("unreachable sts")
	}
	other.Endpoint = "::"
	if _, err := other.Credentials(ctx); err == nil {
		t.Fatal("bad endpoint")
	}
	other = w
	other.Endpoint, other.SessionName = "", "x" // the default endpoint is the real one: unreachable here or refused; either way an error
	other.Client = &http.Client{Transport: failingTransport{}}
	if _, err := other.Credentials(ctx); err == nil {
		t.Fatal("default endpoint with a dead transport")
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("no network")
}

func TestIMDS(t *testing.T) {
	ctx := context.Background()
	var mode atomic.Value
	mode.Store("ok")
	imds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token":
			if mode.Load() == "v1only" || r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") == "" {
				w.WriteHeader(403)
				return
			}
			w.Write([]byte("imds-token"))
		case r.Header.Get("X-aws-ec2-metadata-token") != "imds-token":
			w.WriteHeader(401)
		case r.URL.Path == "/latest/meta-data/iam/security-credentials/":
			if mode.Load() == "norole" {
				w.WriteHeader(404)
				return
			}
			w.Write([]byte("erp-role\n"))
		case r.URL.Path == "/latest/meta-data/iam/security-credentials/erp-role":
			if mode.Load() == "garbage" {
				w.Write([]byte("{"))
				return
			}
			if mode.Load() == "rolefail" {
				w.WriteHeader(500)
				return
			}
			w.Write([]byte(`{"Code":"Success","AccessKeyId":"ASIA2","SecretAccessKey":"secret2","Token":"session2","Expiration":"2030-01-01T00:00:00Z"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer imds.Close()
	m := awscreds.IMDS{Endpoint: imds.URL}
	c, err := m.Credentials(ctx)
	if err != nil || c.AccessKey != "ASIA2" || c.SessionToken != "session2" || c.Expires.Year() != 2030 {
		t.Fatalf("%+v %v", c, err)
	}
	mode.Store("v1only")
	if _, err := m.Credentials(ctx); !errors.Is(err, awscreds.ErrNone) {
		t.Fatalf("no v2 token: %v", err)
	}
	mode.Store("norole")
	if _, err := m.Credentials(ctx); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("no role: %v", err)
	}
	mode.Store("garbage")
	if _, err := m.Credentials(ctx); err == nil || !strings.Contains(err.Error(), "not a credentials document") {
		t.Fatalf("garbage: %v", err)
	}
	mode.Store("rolefail")
	if _, err := m.Credentials(ctx); err == nil || !strings.Contains(err.Error(), "erp-role: 500") {
		t.Fatalf("role document failing: %v", err)
	}
	if _, err := (awscreds.IMDS{Endpoint: "http://127.0.0.1:1"}).Credentials(ctx); !errors.Is(err, awscreds.ErrNone) {
		t.Fatalf("no service: %v", err)
	}
	if _, err := (awscreds.IMDS{Endpoint: "::"}).Credentials(ctx); err == nil || errors.Is(err, awscreds.ErrNone) {
		t.Fatalf("bad endpoint: %v", err)
	}
	// The default endpoint, with a client that cannot reach it: no service.
	if _, err := (awscreds.IMDS{Client: &http.Client{Transport: failingTransport{}}}).Credentials(ctx); !errors.Is(err, awscreds.ErrNone) {
		t.Fatalf("default endpoint: %v", err)
	}
	// A GET failing after the token.
	cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.Write([]byte("tok"))
			return
		}
		panic(http.ErrAbortHandler)
	}))
	defer cut.Close()
	if _, err := (awscreds.IMDS{Endpoint: cut.URL}).Credentials(ctx); err == nil || errors.Is(err, awscreds.ErrNone) {
		t.Fatalf("get aborted: %v", err)
	}
}

type fixed struct {
	c     awscreds.Credentials
	err   error
	calls int
}

func (f *fixed) Credentials(context.Context) (awscreds.Credentials, error) {
	f.calls++
	return f.c, f.err
}

func TestChainCachedAndDefault(t *testing.T) {
	ctx := context.Background()
	none := &fixed{err: awscreds.ErrNone}
	broken := &fixed{err: errors.New("provider down")}
	good := &fixed{c: awscreds.Credentials{AccessKey: "AK"}}
	if c, err := (awscreds.Chain{none, good, broken}).Credentials(ctx); err != nil || c.AccessKey != "AK" || broken.calls != 0 {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := (awscreds.Chain{none, broken}).Credentials(ctx); err == nil || errors.Is(err, awscreds.ErrNone) {
		t.Fatalf("broken provider: %v", err)
	}
	if _, err := (awscreds.Chain{none}).Credentials(ctx); !errors.Is(err, awscreds.ErrNone) {
		t.Fatalf("nothing: %v", err)
	}
	// Temporary credentials are renewed shortly before they expire; long-lived ones are kept.
	clock := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	temp := &fixed{c: awscreds.Credentials{AccessKey: "T", Expires: clock.Add(time.Hour)}}
	cached := &awscreds.Cached{Provider: temp, Now: func() time.Time { return clock }}
	for i := 0; i < 3; i++ {
		cached.Credentials(ctx)
	}
	if temp.calls != 1 {
		t.Fatalf("%d calls within validity", temp.calls)
	}
	clock = clock.Add(56 * time.Minute) // within 5 minutes of expiry
	cached.Credentials(ctx)
	if temp.calls != 2 {
		t.Fatalf("%d calls near expiry", temp.calls)
	}
	long := &awscreds.Cached{Provider: good, Early: time.Minute}
	long.Credentials(ctx)
	long.Credentials(ctx)
	if good.calls != 2 { // one from the chain above, one here
		t.Fatalf("%d calls for long-lived credentials", good.calls)
	}
	if _, err := (&awscreds.Cached{Provider: broken}).Credentials(ctx); err == nil {
		t.Fatal("cached failure")
	}
	// The default chain: static first, then the environment, then nothing here (no STS variables, no metadata service).
	if c, err := awscreds.Default(awscreds.Static{AccessKey: "S", SecretKey: "K"}, env(map[string]string{"AWS_ACCESS_KEY_ID": "E", "AWS_SECRET_ACCESS_KEY": "K"}), "", "http://127.0.0.1:1").Credentials(ctx); err != nil || c.AccessKey != "S" {
		t.Fatalf("static first: %+v %v", c, err)
	}
	if c, err := awscreds.Default(awscreds.Static{}, env(map[string]string{"AWS_ACCESS_KEY_ID": "E", "AWS_SECRET_ACCESS_KEY": "K"}), "", "http://127.0.0.1:1").Credentials(ctx); err != nil || c.AccessKey != "E" {
		t.Fatalf("environment: %+v %v", c, err)
	}
	if _, err := awscreds.Default(awscreds.Static{}, env(map[string]string{}), "", "http://127.0.0.1:1").Credentials(ctx); !errors.Is(err, awscreds.ErrNone) {
		t.Fatalf("nothing ambient: %v", err)
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "P")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "Q")
	if c, err := awscreds.Default(awscreds.Static{}, nil, "", "http://127.0.0.1:1").Credentials(ctx); err != nil || c.AccessKey != "P" {
		t.Fatalf("process environment: %+v %v", c, err)
	}
}
