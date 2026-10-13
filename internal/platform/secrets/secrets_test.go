package secrets_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets/vaulttest"
)

func TestLocal(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "key"), []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := secrets.Local{Getenv: func(k string) string { return map[string]string{"X": "from-env"}[k] }}
	for ref, want := range map[string]string{"env:X": "from-env", "file:" + filepath.Join(dir, "key"): "from-file"} {
		if got, err := l.Resolve(ctx, ref); err != nil || got != want {
			t.Errorf("%s: %q %v", ref, got, err)
		}
	}
	for _, missing := range []string{"env:Y", "env:", "file:" + filepath.Join(dir, "nope")} {
		if _, err := l.Resolve(ctx, missing); !errors.Is(err, secrets.ErrNotFound) {
			t.Errorf("%s: %v", missing, err)
		}
	}
	if _, err := l.Resolve(ctx, "file:"+dir); err == nil || errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("directory: %v", err)
	}
	if _, err := l.Resolve(ctx, "vault:a/b#c"); err == nil {
		t.Error("vault reference resolved locally")
	}
	// The defaults read the process environment and the filesystem.
	t.Setenv("ERP_SECRETS_TEST", "set")
	if got, err := (secrets.Local{}).Resolve(ctx, "env:ERP_SECRETS_TEST"); err != nil || got != "set" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := (secrets.Local{}).Resolve(ctx, "file:"+filepath.Join(dir, "nope")); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatal(err)
	}
	// A replaced file reader is used (mounted secrets in tests).
	hooked := secrets.Local{ReadFile: func(string) ([]byte, error) { return []byte("hooked"), nil }}
	if got, err := hooked.Resolve(ctx, "file:/anywhere"); err != nil || got != "hooked" {
		t.Fatalf("%q %v", got, err)
	}
	// Values that are not references are themselves.
	if got, err := secrets.Value(ctx, l, "plain-value"); err != nil || got != "plain-value" {
		t.Fatalf("%q %v", got, err)
	}
	if got, err := secrets.Value(ctx, l, "env:X"); err != nil || got != "from-env" {
		t.Fatalf("%q %v", got, err)
	}
	if secrets.IsRef("postgres://u:p@h/db") || secrets.IsRef("abc") || !secrets.IsRef("vault:x/y#z") {
		t.Fatal("IsRef")
	}
	m := secrets.Multi{secrets.SchemeEnv: l}
	if got, err := m.Resolve(ctx, "env:X"); err != nil || got != "from-env" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := m.Resolve(ctx, "vault:a/b#c"); err == nil || !strings.Contains(err.Error(), "no resolver") {
		t.Fatalf("%v", err)
	}
}

func TestVault(t *testing.T) {
	ctx := context.Background()
	srv := vaulttest.New(t)
	srv.Secrets["secret/erp/field-key"] = map[string]any{"value": "the-key", "number": 7}
	// With a token.
	v := &secrets.Vault{Address: srv.URL, Token: vaulttest.Token}
	if got, err := v.Resolve(ctx, "vault:secret/erp/field-key#value"); err != nil || got != "the-key" {
		t.Fatalf("%q %v", got, err)
	}
	for ref, want := range map[string]string{
		"vault:secret/erp/field-key#other":  "not found",
		"vault:secret/erp/field-key#number": "not found",
		"vault:secret/erp/missing#value":    "not found",
		"vault:secret/erp/field-key":        "vault:<mount>/<path>#<field>",
		"vault:secret#value":                "vault:<mount>/<path>#<field>",
		"vault:secret/#value":               "vault:<mount>/<path>#<field>",
		"env:X":                             "vault:<mount>/<path>#<field>",
	} {
		if _, err := v.Resolve(ctx, ref); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", ref, err)
		}
	}
	if _, err := (&secrets.Vault{Address: srv.URL, Token: "wrong"}).Resolve(ctx, "vault:secret/erp/field-key#value"); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("wrong token: %v", err)
	}
	// A namespace is sent when configured.
	srv.Namespace = "finance"
	if _, err := v.Resolve(ctx, "vault:secret/erp/field-key#value"); err == nil || !strings.Contains(err.Error(), "no namespace") {
		t.Fatalf("without namespace: %v", err)
	}
	v.Namespace = "finance"
	if got, err := v.Resolve(ctx, "vault:secret/erp/field-key#value"); err != nil || got != "the-key" {
		t.Fatalf("with namespace: %q %v", got, err)
	}
	srv.Namespace = ""
	v.Namespace = ""
	// Kubernetes login: once, then the token is reused; a revoked token is renewed once.
	k := &secrets.Vault{Address: srv.URL, K8sRole: vaulttest.K8sRole, K8sJWT: vaulttest.K8sJWT}
	for range 2 {
		if got, err := k.Resolve(ctx, "vault:secret/erp/field-key#value"); err != nil || got != "the-key" {
			t.Fatalf("%q %v", got, err)
		}
	}
	if srv.Logins != 1 {
		t.Fatalf("%d logins", srv.Logins)
	}
	srv.Revoke("hvs.login-x")
	if got, err := k.Resolve(ctx, "vault:secret/erp/field-key#value"); err != nil || got != "the-key" || srv.Logins != 2 {
		t.Fatalf("after revocation: %q %v (%d logins)", got, err, srv.Logins)
	}
	srv.Revoke("hvs.login-xx")
	srv.Revoke("hvs.login-xxx")
	if _, err := k.Resolve(ctx, "vault:secret/erp/field-key#value"); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("renewed token refused too: %v", err)
	}
	if _, err := (&secrets.Vault{Address: srv.URL, K8sRole: "other", K8sJWT: vaulttest.K8sJWT}).Resolve(ctx, "vault:secret/erp/field-key#value"); err == nil || !strings.Contains(err.Error(), "login") {
		t.Fatalf("wrong role: %v", err)
	}
	if _, err := (&secrets.Vault{Address: srv.URL}).Resolve(ctx, "vault:secret/erp/field-key#value"); err == nil || !strings.Contains(err.Error(), "no token") {
		t.Fatalf("nothing to authenticate with: %v", err)
	}
	// The server failing, unreachable, or answering nonsense.
	srv.Fail = http.StatusServiceUnavailable
	if _, err := v.Resolve(ctx, "vault:secret/erp/field-key#value"); err == nil || !strings.Contains(err.Error(), "sealed") {
		t.Fatalf("sealed: %v", err)
	}
	if _, err := k.Resolve(ctx, "vault:secret/erp/field-key#value"); err == nil || !strings.Contains(err.Error(), "sealed") {
		t.Fatalf("sealed login: %v", err)
	}
	srv.Fail = 0
	for _, bad := range []*secrets.Vault{{Address: "http://127.0.0.1:1", Token: "t"}, {Address: "://bad", Token: "t"},
		{Address: "http://127.0.0.1:1", K8sRole: vaulttest.K8sRole, K8sJWT: vaulttest.K8sJWT}} {
		if _, err := bad.Resolve(ctx, "vault:secret/erp/field-key#value"); err == nil {
			t.Errorf("%s: resolved", bad.Address)
		}
	}
	nonsense := &secrets.Vault{Address: srv.URL, Token: vaulttest.Token, Client: &http.Client{Transport: fixed{body: "not json"}}}
	if _, err := nonsense.Resolve(ctx, "vault:secret/erp/field-key#value"); err == nil || !strings.Contains(err.Error(), "not a KV v2 secret") {
		t.Fatalf("nonsense: %v", err)
	}
	noToken := &secrets.Vault{Address: srv.URL, K8sRole: vaulttest.K8sRole, K8sJWT: vaulttest.K8sJWT, Client: &http.Client{Transport: fixed{body: `{"auth": {}}`}}}
	if _, err := noToken.Resolve(ctx, "vault:secret/erp/field-key#value"); err == nil || !strings.Contains(err.Error(), "no client token") {
		t.Fatalf("login without token: %v", err)
	}
	plain := &secrets.Vault{Address: srv.URL, Token: vaulttest.Token, Client: &http.Client{Transport: fixed{status: 500, body: "boom"}}}
	if _, err := plain.Resolve(ctx, "vault:secret/erp/field-key#value"); err == nil || !strings.Contains(err.Error(), "500: Internal Server Error") {
		t.Fatalf("plain failure: %v", err)
	}
	broken := &secrets.Vault{Address: srv.URL, Token: vaulttest.Token, Client: &http.Client{Transport: fixed{broken: true}}}
	if _, err := broken.Resolve(ctx, "vault:secret/erp/field-key#value"); err == nil {
		t.Fatal("broken body resolved")
	}
}

// fixed answers every request the same way.
type fixed struct {
	status int
	body   string
	broken bool
}

func (f fixed) RoundTrip(*http.Request) (*http.Response, error) {
	status := f.status
	if status == 0 {
		status = 200
	}
	body := io.NopCloser(strings.NewReader(f.body))
	if f.broken {
		body = failingReader{}
	}
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: body}, nil
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }
func (failingReader) Close() error             { return nil }
