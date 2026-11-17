// Package vaulttest is a test double of HashiCorp Vault: a KV version 2
// engine over HTTP with token authentication and the Kubernetes login
// method, so the vault resolver of ADR-0017 can be tested without a server
// (06-engineering-process §6.6).
package vaulttest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
)

// Credentials the server accepts.
const (
	Token   = "hvs.test-token" // #nosec G101 -- a test double's token, never a real credential
	K8sRole = "erp-server"
	K8sJWT  = "eyJ.service-account.jwt"
)

// Server is the running double.
type Server struct {
	URL string
	mu  sync.Mutex
	// Secrets by "<mount>/<path>": the fields of the secret.
	Secrets map[string]map[string]any
	// Logins counts Kubernetes logins; LoginTokens are the tokens handed
	// out, each valid until Revoke.
	Logins  int
	revoked map[string]bool
	// Fail makes every call answer this status (0: serve normally).
	Fail int
	// Namespace, when set, must be sent by the client.
	Namespace string
}

// New starts a server that stops when the test ends.
func New(tb testing.TB) *Server {
	tb.Helper()
	s := &Server{Secrets: map[string]map[string]any{}, revoked: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	tb.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

// Revoke makes a login token stop working.
func (s *Server) Revoke(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revoked[token] = true
}

func (s *Server) loginToken(n int) string { return "hvs.login-" + strings.Repeat("x", n) }

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if s.Fail != 0 {
		w.WriteHeader(s.Fail)
		w.Write([]byte(`{"errors": ["the server is sealed"]}`))
		return
	}
	if s.Namespace != "" && r.Header.Get("X-Vault-Namespace") != s.Namespace {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"errors": ["no namespace"]}`))
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/")
	if path == "auth/kubernetes/login" && r.Method == http.MethodPost {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in) // the role and JWT check below decides; a body that is not JSON has neither
		if in["role"] != K8sRole || in["jwt"] != K8sJWT {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"errors": ["permission denied"]}`))
			return
		}
		s.Logins++
		_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": s.loginToken(s.Logins)}}) // a failed response write means the client is gone: no one is left to tell
		return
	}
	token := r.Header.Get("X-Vault-Token")
	if token != Token && (!strings.HasPrefix(token, "hvs.login-") || s.revoked[token]) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"errors": ["permission denied"]}`))
		return
	}
	mount, rest, ok := strings.Cut(path, "/data/")
	if !ok || r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Write([]byte(`{"errors": ["unsupported path"]}`))
		return
	}
	fields, exists := s.Secrets[mount+"/"+rest]
	if !exists {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"errors": []}`))
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": fields, "metadata": map[string]any{"version": 1}}}) // a failed response write means the client is gone: no one is left to tell
}

// Refs resolves fixed references to their values, as the server's
// resolver would from a vault that holds them: for tests of what resolves
// tenants' references (secrets.ResolveTenant), not of the vault client.
type Refs map[string]string

// Resolve implements secrets.Resolver.
func (r Refs) Resolve(_ context.Context, ref string) (string, error) {
	v, ok := r[ref]
	if !ok {
		return "", fmt.Errorf("vaulttest: %s: %w", ref, secrets.ErrNotFound)
	}
	return v, nil
}

// TenantRef is the reference of a tenant's secret at path, field value:
// vault:secret/tenants/<tenant>/<path>#value, the form a tenant may name
// (secrets.TenantRef).
func TenantRef(tenant uuid.UUID, path string) string {
	return "vault:secret/" + secrets.TenantNamespace + "/" + tenant.String() + "/" + path + "#value"
}

// ByPath resolves any tenant's reference of TenantRef's form by its path
// alone: for tests whose server is configured before their tenant exists.
// What a tenant may name is secrets.ResolveTenant's to refuse, before a
// resolver is asked.
type ByPath map[string]string

// Resolve implements secrets.Resolver.
func (b ByPath) Resolve(_ context.Context, ref string) (string, error) {
	rest, ok := strings.CutPrefix(ref, "vault:secret/"+secrets.TenantNamespace+"/")
	_, path, _ := strings.Cut(rest, "/")
	path, field, _ := strings.Cut(path, "#")
	v, found := b[path]
	if !ok || field != "value" || !found {
		return "", fmt.Errorf("vaulttest: %s: %w", ref, secrets.ErrNotFound)
	}
	return v, nil
}
