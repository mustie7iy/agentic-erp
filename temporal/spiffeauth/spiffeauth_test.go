package spiffeauth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/url"
	"testing"
	"time"

	"go.temporal.io/server/common/authorization"
	"go.temporal.io/server/common/rpc/encryption"
	"google.golang.org/grpc/credentials"
)

const (
	server = "spiffe://dev.erp.example/dev1/erp-server"
	nodes  = "spiffe://dev.erp.example/dev1/erp-temporal"
)

func info(t *testing.T, uris ...string) *authorization.AuthInfo {
	t.Helper()
	leaf := &x509.Certificate{}
	for _, u := range uris {
		parsed, err := url.Parse(u)
		if err != nil {
			t.Fatal(err)
		}
		leaf.URIs = append(leaf.URIs, parsed)
	}
	return &authorization.AuthInfo{TLSConnection: &credentials.TLSInfo{State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{leaf}}}}}
}

func TestID(t *testing.T) {
	if got := ID("dev.erp.example", "dev1", "erp-server"); got != server {
		t.Errorf("ID: got %q, want %q", got, server)
	}
}

func TestClaims(t *testing.T) {
	m := Mapper{Server: server, Nodes: nodes, Namespace: "erp"}
	cases := map[string]struct {
		info       *authorization.AuthInfo
		subject    string
		system     authorization.Role
		namespaces map[string]authorization.Role
	}{
		"no information":     {nil, "", authorization.RoleUndefined, nil},
		"no TLS":             {&authorization.AuthInfo{}, "", authorization.RoleUndefined, nil},
		"nothing verified":   {&authorization.AuthInfo{TLSConnection: &credentials.TLSInfo{}}, "", authorization.RoleUndefined, nil},
		"no SPIFFE ID":       {info(t, "https://example.com/x"), "", authorization.RoleUndefined, nil},
		"two SPIFFE IDs":     {info(t, server, nodes), "", authorization.RoleUndefined, nil},
		"the engine's nodes": {info(t, nodes), nodes, authorization.RoleAdmin, nil},
		"the cell's server":  {info(t, "https://example.com/x", server), server, authorization.RoleUndefined, map[string]authorization.Role{"erp": authorization.RoleWriter}},
		"another identity":   {info(t, "spiffe://dev.erp.example/global/erp-router"), "spiffe://dev.erp.example/global/erp-router", authorization.RoleUndefined, nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := m.GetClaims(c.info)
			if err != nil {
				t.Fatalf("GetClaims: %v", err)
			}
			if got.Subject != c.subject {
				t.Errorf("subject: got %q, want %q", got.Subject, c.subject)
			}
			if got.System != c.system {
				t.Errorf("system role: got %v, want %v", got.System, c.system)
			}
			if len(got.Namespaces) != len(c.namespaces) || got.Namespaces["erp"] != c.namespaces["erp"] {
				t.Errorf("namespace roles: got %v, want %v", got.Namespaces, c.namespaces)
			}
		})
	}
}

// The authorizer decides on the claims: the server writes in its namespace
// and nowhere else, another identity does nothing but the health checks.
func TestAuthorizer(t *testing.T) {
	a := Authorizer()
	m := Mapper{Server: server, Nodes: nodes, Namespace: "erp"}
	claims := func(id string) *authorization.Claims {
		c, err := m.GetClaims(info(t, id))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	start := "/temporal.api.workflowservice.v1.WorkflowService/StartWorkflowExecution"
	cases := map[string]struct {
		claims   *authorization.Claims
		api, ns  string
		decision authorization.Decision
	}{
		"the server in its namespace":    {claims(server), start, "erp", authorization.DecisionAllow},
		"the server in another":          {claims(server), start, "other", authorization.DecisionDeny},
		"another identity":               {claims("spiffe://dev.erp.example/global/erp-router"), start, "erp", authorization.DecisionDeny},
		"the nodes register a namespace": {claims(nodes), "/temporal.api.workflowservice.v1.WorkflowService/RegisterNamespace", "", authorization.DecisionAllow},
		"the server may not":             {claims(server), "/temporal.api.workflowservice.v1.WorkflowService/RegisterNamespace", "", authorization.DecisionDeny},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := a.Authorize(context.Background(), c.claims, &authorization.CallTarget{APIName: c.api, Namespace: c.ns})
			if err != nil {
				t.Fatal(err)
			}
			if got.Decision != c.decision {
				t.Errorf("decision: got %v, want %v", got.Decision, c.decision)
			}
		})
	}
}

// provider stands for Temporal's TLS provider: the given configuration
// and error from every getter.
type provider struct {
	cfg *tls.Config
	err error
}

func (p provider) GetInternodeServerConfig() (*tls.Config, error) { return p.cfg, p.err }
func (p provider) GetInternodeClientConfig() (*tls.Config, error) { return p.cfg, p.err }
func (p provider) GetFrontendServerConfig() (*tls.Config, error)  { return p.cfg, p.err }
func (p provider) GetFrontendClientConfig() (*tls.Config, error)  { return p.cfg, p.err }
func (p provider) GetRemoteClusterClientConfig(string) (*tls.Config, error) {
	return p.cfg, p.err
}
func (p provider) GetExpiringCerts(time.Duration) (encryption.CertExpirationMap, encryption.CertExpirationMap, error) {
	return nil, nil, p.err
}

func TestTLS13(t *testing.T) {
	per := &tls.Config{MinVersion: tls.VersionTLS12}
	base := &tls.Config{MinVersion: tls.VersionTLS12, GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) { return per, nil }}
	p := TLS13{TLSConfigProvider: provider{cfg: base}}
	getters := map[string]func() (*tls.Config, error){
		"internode server": p.GetInternodeServerConfig, "internode client": p.GetInternodeClientConfig,
		"frontend server": p.GetFrontendServerConfig, "frontend client": p.GetFrontendClientConfig,
		"remote cluster": func() (*tls.Config, error) { return p.GetRemoteClusterClientConfig("x") },
	}
	for name, get := range getters {
		t.Run(name, func(t *testing.T) {
			cfg, err := get()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.MinVersion != tls.VersionTLS13 {
				t.Errorf("minimum version: got %x, want TLS 1.3", cfg.MinVersion)
			}
			perConn, err := cfg.GetConfigForClient(nil)
			if err != nil {
				t.Fatal(err)
			}
			if perConn.MinVersion != tls.VersionTLS13 {
				t.Errorf("per-connection minimum version: got %x, want TLS 1.3", perConn.MinVersion)
			}
		})
	}
	if base.MinVersion != tls.VersionTLS12 {
		t.Errorf("the provider's own configuration changed: %x", base.MinVersion)
	}
	if _, _, err := p.GetExpiringCerts(time.Hour); err != nil {
		t.Errorf("expiring certificates: %v", err)
	}
	t.Run("off", func(t *testing.T) {
		cfg, err := TLS13{TLSConfigProvider: provider{}}.GetFrontendServerConfig()
		if cfg != nil || err != nil {
			t.Errorf("got %v, %v, want no configuration", cfg, err)
		}
	})
	t.Run("failing", func(t *testing.T) {
		boom := errors.New("boom")
		failing := TLS13{TLSConfigProvider: provider{err: boom}}
		if _, err := failing.GetFrontendServerConfig(); !errors.Is(err, boom) {
			t.Errorf("configuration error: got %v, want %v", err, boom)
		}
		if _, _, err := failing.GetExpiringCerts(time.Hour); !errors.Is(err, boom) {
			t.Errorf("expiring certificates error: got %v, want %v", err, boom)
		}
	})
	t.Run("plain", func(t *testing.T) {
		cfg, err := TLS13{TLSConfigProvider: provider{cfg: &tls.Config{}}}.GetFrontendClientConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.MinVersion != tls.VersionTLS13 {
			t.Errorf("minimum version: got %x, want TLS 1.3", cfg.MinVersion)
		}
		if cfg.GetConfigForClient != nil {
			t.Error("per-connection callback: got one, want none")
		}
	})
}

// TestPin: the engine's own connections admit a verified peer of its own
// identity alone, and still run the configuration's own verification.
func TestPin(t *testing.T) {
	state := func(uris ...string) tls.ConnectionState { return info(t, uris...).TLSConnection.State }
	boom := errors.New("boom")
	var ran bool
	base := &tls.Config{VerifyConnection: func(tls.ConnectionState) error { ran = true; return nil }}
	p := TLS13{TLSConfigProvider: provider{cfg: base}, Nodes: nodes}
	for name, get := range map[string]func() (*tls.Config, error){
		"internode server": p.GetInternodeServerConfig, "internode client": p.GetInternodeClientConfig,
		"frontend client": p.GetFrontendClientConfig,
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := get()
			if err != nil {
				t.Fatal(err)
			}
			if err := cfg.VerifyConnection(state(server)); err == nil {
				t.Error("the cell's server: got admitted, want refused")
			}
			if err := cfg.VerifyConnection(tls.ConnectionState{}); err == nil {
				t.Error("no verified certificate: got admitted, want refused")
			}
			ran = false
			if err := cfg.VerifyConnection(state(nodes)); err != nil {
				t.Errorf("the engine's nodes: got %v, want admitted", err)
			}
			if !ran {
				t.Error("the configuration's own verification did not run")
			}
		})
	}
	t.Run("own verification fails", func(t *testing.T) {
		failing := TLS13{TLSConfigProvider: provider{cfg: &tls.Config{VerifyConnection: func(tls.ConnectionState) error { return boom }}}, Nodes: nodes}
		cfg, err := failing.GetInternodeServerConfig()
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.VerifyConnection(state(nodes)); !errors.Is(err, boom) {
			t.Errorf("got %v, want %v", err, boom)
		}
	})
	t.Run("frontend server", func(t *testing.T) {
		cfg, err := p.GetFrontendServerConfig()
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.VerifyConnection(state(server)); err != nil {
			t.Errorf("the cell's server at the frontend: got %v, want admitted (the claims decide)", err)
		}
	})
	t.Run("no own verification", func(t *testing.T) {
		cfg, err := TLS13{TLSConfigProvider: provider{cfg: &tls.Config{}}, Nodes: nodes}.GetInternodeClientConfig()
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.VerifyConnection(state(nodes)); err != nil {
			t.Errorf("got %v, want admitted", err)
		}
	})
}
