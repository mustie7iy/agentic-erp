package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf/tlstest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func environment(t *testing.T, is *authntest.Issuer) map[string]string {
	t.Helper()
	issuers, _ := json.Marshal([]any{is.Config()})
	return map[string]string{"ERP_DIRECTORY_URL": pgtest.URL(t), "ERP_ENV": "development", "ERP_OIDC_ISSUERS": string(issuers), "ERP_OPERATOR_ISSUERS": string(issuers),
		"ERP_PAGE_TOKEN_KEY": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))),
		"ERP_ROUTER_ADDR":    "127.0.0.1:0", "ERP_ROUTER_ADMIN_ADDR": "127.0.0.1:0", "ERP_SHUTDOWN_TIMEOUT": "5s"}
}

// TestRoutesToTheCell: the router serves, forwards a tenant's request to the
// cell the directory names, and stops cleanly.
func TestRoutesToTheCell(t *testing.T) {
	is := authntest.New(t)
	env := environment(t, is)
	getenv := func(k string) string { return env[k] }
	cell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"cell":"eu-1","path":"` + r.URL.Path + `"}`))
	}))
	defer cell.Close()
	tenant := uuid.New()
	dir, err := directory.Open(context.Background(), pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := dir.Upsert(context.Background(), directory.Entry{TenantID: tenant, Region: "eu-west", Cell: "eu-1", APIURL: cell.URL, Status: directory.Active}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var logs bytes.Buffer
	addr, adminAddr := make(chan string, 1), make(chan string, 1)
	done := make(chan int, 1)
	go func() {
		done <- cli(ctx, getenv, &logs, func(ln, admin net.Listener) { addr <- ln.Addr().String(); adminAddr <- admin.Addr().String() })
	}()
	var base string
	select {
	case a := <-addr:
		base = "http://" + a
	case code := <-done:
		t.Fatalf("stopped before serving: %d %s", code, logs.String())
	}
	req, _ := http.NewRequestWithContext(t.Context(), "GET", base+"/v1/balances", nil)
	req.Header.Set("Authorization", "Bearer "+is.Token(t, tenant, "ada", nil))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close() // the body is already read; a close error cannot change what is asserted
	if resp.StatusCode != 200 {
		t.Errorf("status: got %d, want 200 (body %s)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"cell":"eu-1"`) {
		t.Errorf("body: got %s, want it to contain %q", body, `"cell":"eu-1"`)
	}
	if got := resp.Header.Get("X-Erp-Cell"); got != "eu-1" {
		t.Errorf("X-Erp-Cell: got %q, want %q", got, "eu-1")
	}
	if code := getStatus(t, base+"/livez"); code != 200 {
		t.Fatalf("livez: got %d, want 200", code)
	}
	// The admin listener: liveness, metrics and the build's version, and the global control plane for operators.
	var adminBase string
	select {
	case a := <-adminAddr:
		adminBase = "http://" + a
	default:
		t.Fatal("no admin listener")
	}
	for _, path := range []string{"/livez", "/metrics", "/version"} {
		t.Run("admin "+path, func(t *testing.T) {
			if code := getStatus(t, adminBase+path); code != 200 {
				t.Fatalf("admin %s: got %d, want 200", path, code)
			}
		})
	}
	req, _ = http.NewRequestWithContext(t.Context(), "GET", adminBase+"/directory/v1/tenants/"+tenant.String(), nil)
	req.Header.Set("Authorization", "Bearer "+is.Token(t, uuid.Nil, "ops|alice", map[string]any{"tenant_id": nil}))
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("directory API: %v %v", resp, err)
	}
	_ = resp.Body.Close() // only the status is asserted; the body is not needed
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("stop: exit code got %d, want 0 (logs %s)", code, logs.String())
	}
	if !strings.Contains(logs.String(), "erp-router stopped") {
		t.Errorf("stop: logs got %s, want them to contain %q", logs.String(), "erp-router stopped")
	}
}

func TestExitCodes(t *testing.T) {
	is := authntest.New(t)
	busy, _ := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	defer func() { _ = busy.Close() }() // best-effort cleanup of the port the cases collide with
	for name, c := range map[string]struct {
		mutate func(env map[string]string)
		code   int
		want   string
	}{
		"configuration": {func(e map[string]string) { delete(e, "ERP_DIRECTORY_URL") }, 2, "ERP_DIRECTORY_URL"},
		"directory": {func(e map[string]string) {
			e["ERP_DIRECTORY_URL"] = "postgres://nobody@127.0.0.1:1/x?connect_timeout=1"
		}, 1, "erp-router stopped"},
		"issuer": {func(e map[string]string) {
			e["ERP_OIDC_ISSUERS"] = `[{"issuer":"http://127.0.0.1:1","audience":"erp-api"}]`
		}, 1, "erp-router stopped"},
		"address in use": {func(e map[string]string) { e["ERP_ROUTER_ADDR"] = busy.Addr().String() }, 1, "address already in use"},
		"admin in use":   {func(e map[string]string) { e["ERP_ROUTER_ADMIN_ADDR"] = busy.Addr().String() }, 1, "address already in use"},
		"operator issuer": {func(e map[string]string) {
			e["ERP_OPERATOR_ISSUERS"] = `[{"issuer":"http://127.0.0.1:1","audience":"erp-control"}]`
		}, 1, "erp-router stopped"},
		"page key": {func(e map[string]string) {
			e["ERP_PAGE_TOKEN_KEY"] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))) + "x"
		}, 2, "ERP_PAGE_TOKEN_KEY"},
		"listener closed": {func(map[string]string) {}, 1, "closed"},
	} {
		t.Run(name, func(t *testing.T) {
			env := environment(t, is)
			c.mutate(env)
			var logs bytes.Buffer
			started := func(_, _ net.Listener) {}
			if name == "listener closed" {
				started = func(ln, _ net.Listener) {
					// Closing provokes the serve error under test; a failed close shows as a wrong exit code.
					_ = ln.Close()
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			code := cli(ctx, func(k string) string { return env[k] }, &logs, started)
			cancel()
			if code != c.code {
				t.Errorf("exit code: got %d, want %d (logs %s)", code, c.code, logs.String())
			}
			if !strings.Contains(logs.String(), c.want) {
				t.Errorf("logs: got %s, want them to contain %q", logs.String(), c.want)
			}
		})
	}
	// main wires the signal context and exits with the code.
	exit = func(int) {}
	defer func() { exit = nil }()
	t.Setenv("ERP_DIRECTORY_URL", "")
	main()
}

// TestProductionOperatorWithoutRoles: the router passes ERP_ENV through to
// the operator verifier, so in production an operator token without a roles
// claim holds no role on the global control plane (ADR-0027, amended),
// while one that carries the role is let through.
func TestProductionOperatorWithoutRoles(t *testing.T) {
	is := authntest.New(t)
	env := environment(t, is)
	ca := production(t, env)
	client := trusting(ca)
	ctx, cancel := context.WithCancel(context.Background())
	var logs bytes.Buffer
	adminAddr := make(chan string, 1)
	done := make(chan int, 1)
	go func() {
		done <- cli(ctx, func(k string) string { return env[k] }, &logs, func(_, admin net.Listener) { adminAddr <- admin.Addr().String() })
	}()
	defer func() {
		cancel()
		if code := <-done; code != 0 {
			t.Errorf("stop: exit code got %d, want 0 (logs %s)", code, logs.String())
		}
	}()
	var base string
	select {
	case a := <-adminAddr:
		base = "https://" + a // the admin listener serves TLS as the router's identity (ADR-0033)
	case code := <-done:
		done <- code // the deferred stop reads the exit code again
		t.Fatalf("stopped before serving: %d %s", code, logs.String())
	}
	get := func(extra map[string]any) int {
		req, _ := http.NewRequestWithContext(t.Context(), "GET", base+"/directory/v1/tenants/"+uuid.NewString(), nil)
		req.Header.Set("Authorization", "Bearer "+is.Token(t, uuid.Nil, "ops|alice", extra))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close() // only the status is asserted
		return resp.StatusCode
	}
	if got := get(map[string]any{"tenant_id": nil}); got != http.StatusForbidden {
		t.Errorf("no roles claim: got %d, want 403", got)
	}
	if got := get(map[string]any{"tenant_id": nil, "roles": []string{"erp:read"}}); got != http.StatusNotFound {
		t.Errorf("with erp:read: got %d, want 404 (an unknown tenant, past the role check)", got)
	}
}

const trustDomain = "erp.example"

// production turns a configuration into production's (ADR-0031,
// ADR-0033): no shared issuers, the directory over verified TLS, the
// router's SPIFFE identity, a public certificate and the trust domain.
// It returns the authority that issued them all.
func production(t *testing.T, env map[string]string) *tlstest.CA {
	t.Helper()
	ca, dir := tlstest.NewCA(t), t.TempDir()
	id := ca.Issue(t, tlstest.Options{URIs: []string{tlsconf.ID(trustDomain, "global", "erp-router")}})
	public := ca.Issue(t, tlstest.Options{})
	env["ERP_ENV"] = "production"
	delete(env, "ERP_OIDC_ISSUERS") // production trusts tenants' registered providers only (ADR-0031)
	env["ERP_DIRECTORY_URL"] = pgtest.TLS(t, env["ERP_DIRECTORY_URL"])
	env["ERP_TLS_CA"] = "file:" + tlstest.WriteFile(t, dir, "ca.crt", ca.PEM)
	env["ERP_TLS_CERT"] = "file:" + tlstest.WriteFile(t, dir, "tls.crt", id.CertPEM)
	env["ERP_TLS_KEY"] = "file:" + tlstest.WriteFile(t, dir, "tls.key", id.KeyPEM)
	env["ERP_ROUTER_PUBLIC_CERT"] = "file:" + tlstest.WriteFile(t, dir, "public.crt", public.CertPEM)
	env["ERP_ROUTER_PUBLIC_KEY"] = "file:" + tlstest.WriteFile(t, dir, "public.key", public.KeyPEM)
	env["ERP_TRUST_DOMAIN"] = trustDomain
	return ca
}

// trusting is a client that trusts ca, presenting no certificate.
func trusting(ca *tlstest.CA) *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: ca.Pool()}}}
}

// TestRoutesOverMutualTLS: with its TLS material the router serves the
// public over TLS with its public certificate and reaches each cell over
// mutual TLS 1.3 as its SPIFFE identity, accepting only a cell's identity in the
// trust domain; a cell whose URL is not https, or whose certificate names
// another service, is not reached (ADR-0033).
func TestRoutesOverMutualTLS(t *testing.T) {
	is := authntest.New(t)
	env := environment(t, is)
	issuers := env["ERP_OIDC_ISSUERS"]
	ca := production(t, env)
	// Development's shared issuer, whose tokens name their tenant; the rest
	// is production's: with an identity the router reaches cells over TLS
	// only.
	env["ERP_OIDC_ISSUERS"], env["ERP_ENV"] = issuers, "development"
	var forwarded atomic.Value
	cell := func(id string) (*httptest.Server, *atomic.Value) {
		var peer atomic.Value
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			peer.Store(tlsconf.PeerID(r.TLS))
			forwarded.Store(r.Header.Get(tlsconf.ClientCertHeader))
			w.Write([]byte(`{}`))
		}))
		srv.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{ca.Issue(t, tlstest.Options{URIs: []string{id}}).Pair(t)},
			ClientCAs: ca.Pool(), ClientAuth: tls.RequireAndVerifyClientCert}
		srv.StartTLS()
		t.Cleanup(srv.Close)
		return srv, &peer
	}
	eu1, peer := cell(tlsconf.ID(trustDomain, "eu-1", "erp-server"))
	impostor, _ := cell(tlsconf.ID(trustDomain, "global", "erp-router"))
	// A cell's genuine certificate, but another cell's than the directory
	// names: a stolen cell certificate impersonates no other cell.
	eu2, _ := cell(tlsconf.ID(trustDomain, "eu-2", "erp-server"))
	dir, err := directory.Open(context.Background(), pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	good, plain, wrong, otherCell := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for tenant, url := range map[uuid.UUID]string{good: eu1.URL, plain: strings.Replace(eu1.URL, "https://", "http://", 1), wrong: impostor.URL, otherCell: eu2.URL} {
		if err := dir.Upsert(context.Background(), directory.Entry{TenantID: tenant, Region: "eu-west", Cell: "eu-1", APIURL: url, Status: directory.Active}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	var logs pgtest.SyncBuffer // the router logs on its goroutines while the test reads
	addrs := make(chan [2]string, 1)
	done := make(chan int, 1)
	go func() {
		done <- cli(ctx, func(k string) string { return env[k] }, &logs, func(ln, admin net.Listener) { addrs <- [2]string{ln.Addr().String(), admin.Addr().String()} })
	}()
	defer func() {
		cancel()
		if code := <-done; code != 0 {
			t.Errorf("stop: exit code got %d, want 0 (logs %s)", code, logs.String())
		}
	}()
	var a [2]string
	select {
	case a = <-addrs:
	case code := <-done:
		done <- code // the deferred stop reads the exit code again
		t.Fatalf("stopped before serving: %d %s", code, logs.String())
	}
	// An agent's own certificate, from no authority the router knows: the
	// handshake proves it holds the key, and the cell gets its thumbprint.
	agent := tlstest.NewCA(t).Issue(t, tlstest.Options{}).Pair(t)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: ca.Pool(), Certificates: []tls.Certificate{agent}}}}
	for _, c := range []struct {
		name   string
		tenant uuid.UUID
		want   int
	}{
		{"the tenant's cell", good, http.StatusOK},
		{"a cell without https", plain, http.StatusServiceUnavailable},
		{"a certificate of another service", wrong, http.StatusBadGateway},
		{"a certificate of another cell", otherCell, http.StatusBadGateway},
	} {
		t.Run(c.name, func(t *testing.T) {
			req, _ := http.NewRequestWithContext(t.Context(), "GET", "https://"+a[0]+"/v1/balances", nil)
			req.Header.Set("Authorization", "Bearer "+is.Token(t, c.tenant, "ada", nil))
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close() // only the status is asserted
			if resp.StatusCode != c.want {
				t.Errorf("status: got %d, want %d", resp.StatusCode, c.want)
			}
		})
	}
	if got, want := peer.Load(), tlsconf.ID(trustDomain, "global", "erp-router"); got != want {
		t.Errorf("the cell's peer: got %v, want %s", got, want)
	}
	if got, want := forwarded.Load(), tlsconf.Thumbprint(agent.Leaf); got != want {
		t.Errorf("forwarded thumbprint: got %v, want the agent's certificate's %s", got, want)
	}
	req, _ := http.NewRequestWithContext(t.Context(), "GET", "https://"+a[1]+"/livez", nil)
	resp, err := trusting(ca).Do(req)
	if err != nil {
		t.Fatalf("admin probe over TLS without a certificate: %v", err)
	}
	_ = resp.Body.Close() // only the status is asserted
	if resp.StatusCode != http.StatusOK {
		t.Errorf("admin /livez: got %d, want 200", resp.StatusCode)
	}
}

// getStatus gets the URL and returns the response's status, its body closed.
func getStatus(t *testing.T, u string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}

// TestBehindTheLoadBalancer: with ERP_ROUTER_PROXY_FROM naming the load
// balancer's addresses, the router reads its PROXY v2 header and the cell
// sees the client's address in X-Forwarded-For, not the balancer's.
func TestBehindTheLoadBalancer(t *testing.T) {
	is := authntest.New(t)
	env := environment(t, is)
	env["ERP_ROUTER_PROXY_FROM"] = "127.0.0.0/8"
	forwarded := make(chan string, 1)
	cell := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded <- r.Header.Get("X-Forwarded-For")
		w.Write([]byte(`{}`))
	}))
	defer cell.Close()
	tenant := uuid.New()
	dir, err := directory.Open(context.Background(), pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := dir.Upsert(context.Background(), directory.Entry{TenantID: tenant, Region: "eu-west", Cell: "eu-1", APIURL: cell.URL, Status: directory.Active}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs bytes.Buffer
	addr := make(chan string, 1)
	done := make(chan int, 1)
	go func() {
		done <- cli(ctx, func(k string) string { return env[k] }, &logs, func(ln, _ net.Listener) { addr <- ln.Addr().String() })
	}()
	var base string
	select {
	case a := <-addr:
		base = a
	case code := <-done:
		t.Fatalf("stopped before serving: %d %s", code, logs.String())
	}
	// As the balancer: the header, then the client's bytes.
	header := append([]byte("\r\n\r\n\x00\r\nQUIT\n"), 0x21, 0x11, 0, 12, 203, 0, 113, 7, 127, 0, 0, 1)
	header = binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(header, 51000), 443)
	balancer := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, a string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, a)
		if err == nil {
			_, err = c.Write(header)
		}
		return c, err
	}}}
	req, err := http.NewRequestWithContext(t.Context(), "GET", "http://"+base+"/v1/balances", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+is.Token(t, tenant, "ada", nil))
	resp, err := balancer.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close() // only the status and what the cell saw are asserted
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if got := <-forwarded; got != "203.0.113.7" {
		t.Errorf("X-Forwarded-For at the cell: got %q, want the client's 203.0.113.7", got)
	}
	// A connection from the balancer's addresses without the header is not
	// served: net/http answers the failed read with a bare 400 (behind the
	// public TLS, the handshake fails), and nothing reaches the cell.
	// The client may see the 400 or, should the router close first, a
	// broken connection: either way the request is not served, which the
	// cell's silence below asserts.
	r, err := http.DefaultClient.Do(req.Clone(t.Context()))
	if err == nil {
		_ = r.Body.Close() // only the status is asserted
		if r.StatusCode != http.StatusBadRequest {
			t.Errorf("a request without the balancer's header: got %d, want 400", r.StatusCode)
		}
	}
	select {
	case got := <-forwarded:
		t.Errorf("a request without the balancer's header reached the cell (X-Forwarded-For %q)", got)
	default:
	}
}
