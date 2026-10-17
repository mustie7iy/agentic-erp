package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"text/template"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

const (
	domain = "dev.erp.example"
	cell   = "dev1"
)

// engineYAML is a cell's configuration as the erp-cell chart writes it, on
// an in-memory SQLite store instead of the cell's PostgreSQL.
var engineYAML = template.Must(template.New("engine").Parse(`
log:
  stdout: true
  level: error
persistence:
  numHistoryShards: 1
  defaultStore: default
  visibilityStore: default
  datastores:
    default:
      sql:
        pluginName: sqlite
        databaseName: "{{.Database}}"
        connectAttributes: {mode: memory, cache: shared}
        # Unused by SQLite; the loader requires them.
        connectAddr: localhost
        connectProtocol: tcp
global:
  membership:
    maxJoinDuration: 30s
    broadcastAddress: 127.0.0.1
  tls:
    internode:
      server:
        certFile: {{.NodesCert}}
        keyFile: {{.NodesKey}}
        requireClientAuth: {{.RequireClientAuth}}
        clientCaFiles: [{{.InternodeCA}}]
      client:
        serverName: localhost
        rootCaFiles: [{{.CA}}]
    frontend:
      server:
        certFile: {{.NodesCert}}
        keyFile: {{.NodesKey}}
        requireClientAuth: {{.RequireClientAuth}}
        clientCaFiles: [{{.FrontendCA}}]
      client:
        serverName: localhost
        rootCaFiles: [{{.CA}}]
    systemWorker:
      certFile: {{.SystemCert}}
      keyFile: {{.NodesKey}}
      client:
        serverName: localhost
        rootCaFiles: [{{.CA}}]
services:
  frontend:
    rpc: {grpcPort: {{index .Ports 0}}, membershipPort: {{index .Ports 1}}, bindOnLocalHost: true}
  history:
    rpc: {grpcPort: {{index .Ports 2}}, membershipPort: {{index .Ports 3}}, bindOnLocalHost: true}
  matching:
    rpc: {grpcPort: {{index .Ports 4}}, membershipPort: {{index .Ports 5}}, bindOnLocalHost: true}
  worker:
    rpc: {grpcPort: {{index .Ports 6}}, membershipPort: {{index .Ports 7}}, bindOnLocalHost: true}
clusterMetadata:
  enableGlobalNamespace: false
  failoverVersionIncrement: 10
  masterClusterName: active
  currentClusterName: active
  clusterInformation:
    active:
      enabled: true
      initialFailoverVersion: 1
      rpcName: frontend
      rpcAddress: 127.0.0.1:{{index .Ports 0}}
dcRedirectionPolicy:
  policy: noop
archival:
  history: {state: disabled}
  visibility: {state: disabled}
namespaceDefaults:
  archival:
    history: {state: disabled}
    visibility: {state: disabled}
publicClient:
  hostPort: localhost:{{index .Ports 0}}
`))

// engine is a cell's engine of the test: its certificates and settings.
type engine struct {
	dir      string
	ca       authority
	frontend string
	env      map[string]string
	certs    map[string][2]string // name: certificate, key
}

// engineOptions change the configuration from the chart's, for the
// refusals of engineConfig.
type engineOptions struct {
	optional    bool // client certificates not required
	noFrontend  bool // no CA for the frontend's clients
	noInternode bool // no CA for the internode clients
	noSystem    bool // no certificate for the system worker
}

func newEngine(t *testing.T, requireClientAuth bool) *engine {
	t.Helper()
	return newEngineWith(t, engineOptions{optional: !requireClientAuth})
}

func newEngineWith(t *testing.T, o engineOptions) *engine {
	t.Helper()
	e := &engine{dir: t.TempDir(), certs: map[string][2]string{}}
	e.ca = newAuthority(t, e.dir, "ca")
	for name, id := range map[string]string{
		"nodes": idOf("erp-temporal"), "server": idOf("erp-server"),
		"router": "spiffe://" + domain + "/global/erp-router", "anonymous": "",
	} {
		c, k := e.ca.issue(t, e.dir, name, id)
		e.certs[name] = [2]string{c, k}
	}
	other := newAuthority(t, e.dir, "other")
	c, k := other.issue(t, e.dir, "impostor", idOf("erp-server"))
	e.certs["impostor"] = [2]string{c, k}
	ports := freePorts(t, 8)
	e.frontend = fmt.Sprintf("localhost:%d", ports[0])
	var buf bytes.Buffer
	unless := func(off bool, v string) string {
		if off {
			return ""
		}
		return v
	}
	err := engineYAML.Execute(&buf, map[string]any{
		"Database": fmt.Sprintf("erp%d", time.Now().UnixNano()), "CA": e.ca.file, "RequireClientAuth": !o.optional,
		"FrontendCA": unless(o.noFrontend, e.ca.file), "InternodeCA": unless(o.noInternode, e.ca.file),
		"NodesCert": e.certs["nodes"][0], "NodesKey": e.certs["nodes"][1], "SystemCert": unless(o.noSystem, e.certs["nodes"][0]),
		"Ports": ports,
	})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(e.dir, "temporal.yaml")
	if err := os.WriteFile(file, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "password"), []byte("pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.env = map[string]string{
		"ERP_TEMPORAL_CONFIG": file, "ERP_TRUST_DOMAIN": domain, "ERP_CELL": cell,
		"ERP_TEMPORAL_DATABASE_PASSWORD": "file:" + filepath.Join(e.dir, "password"),
	}
	return e
}

func idOf(service string) string { return "spiffe://" + domain + "/" + cell + "/" + service }

func (e *engine) getenv(k string) string { return e.env[k] }

// client dials the frontend as the named certificate ("none" for none) at
// TLS minVersion.
func (e *engine) client(t *testing.T, name, namespace string, minVersion uint16) client.Client {
	t.Helper()
	cert := e.certs[name]
	tlsCfg := e.ca.clientTLS(t, cert[0], cert[1], minVersion)
	c, err := client.NewLazyClient(client.Options{HostPort: e.frontend, Namespace: namespace, Logger: quiet{},
		ConnectionOptions: client.ConnectionOptions{TLS: tlsCfg}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func freePorts(t *testing.T, n int) []int {
	t.Helper()
	var ports []int
	var lns []net.Listener
	for range n {
		ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		lns = append(lns, ln)
		ports = append(ports, ln.Addr().(*net.TCPAddr).Port)
	}
	for _, ln := range lns {
		_ = ln.Close() // probed only for a free port
	}
	return ports
}

// syncBuffer is a writer the engine's goroutine and the test share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestServe: the engine starts on the chart's configuration, registers the
// cell's namespace as its own identity, admits the cell's server there by
// its SPIFFE identity over TLS 1.3, and refuses another identity, another
// namespace, another authority, no certificate and TLS 1.2; it stops when
// its context ends.
func TestServe(t *testing.T) {
	// An in-memory SQLite store gets its schema from the plugin.
	e := newEngine(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr syncBuffer
	done := make(chan int, 1)
	go func() { done <- cli(ctx, []string{"serve"}, e.getenv, &stderr) }()

	// The namespace is registered once the engine is up, and usable once
	// every service has read it: poll the first start until then.
	server := e.client(t, "server", "erp", tls.VersionTLS13)
	var run client.WorkflowRun
	deadline := time.Now().Add(90 * time.Second)
	for {
		var err error
		run, err = server.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{TaskQueue: "erp", ID: "check"}, "Check")
		if err == nil {
			break
		}
		var missing *serviceerror.NamespaceNotFound
		var unavailable *serviceerror.Unavailable
		if !errors.As(err, &missing) && !errors.As(err, &unavailable) {
			t.Fatalf("the cell's server was refused a workflow in its namespace: %v", err)
		}
		select {
		case code := <-done:
			t.Fatalf("the engine stopped with %d: %s", code, stderr.String())
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("the namespace did not become usable: %v\n%s", err, stderr.String())
		}
	}
	if run.GetID() != "check" {
		t.Errorf("workflow id: got %q, want check", run.GetID())
	}
	t.Run("refusals", func(t *testing.T) { refusals(t, e) })

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code: got %d, want 0 (%s)", code, stderr.String())
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("the engine did not stop")
	}
}

func refusals(t *testing.T, e *engine) {
	t.Helper()
	start := func(c client.Client) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{TaskQueue: "erp", ID: "refused"}, "Check")
		return err
	}
	var denied *serviceerror.PermissionDenied
	for name, c := range map[string]client.Client{
		"another identity":  e.client(t, "router", "erp", tls.VersionTLS13),
		"another namespace": e.client(t, "server", "other", tls.VersionTLS13),
		"no SPIFFE ID":      e.client(t, "anonymous", "erp", tls.VersionTLS13),
	} {
		t.Run(name, func(t *testing.T) {
			if err := start(c); !errors.As(err, &denied) {
				t.Errorf("error: got %v, want permission denied", err)
			}
		})
	}
	for name, c := range map[string]client.Client{
		"another authority": e.client(t, "impostor", "erp", tls.VersionTLS13),
		"no certificate":    e.client(t, "none", "erp", tls.VersionTLS13),
		"TLS 1.2":           e.client(t, "server", "erp", tls.VersionTLS12),
	} {
		t.Run(name, func(t *testing.T) {
			err := start(c)
			if err == nil || errors.As(err, &denied) {
				t.Errorf("error: got %v, want the connection refused", err)
			}
		})
	}
}

// TestServeSettings: what serve refuses before it starts anything.
func TestServeSettings(t *testing.T) {
	e := newEngine(t, true)
	without := func(k string) func(string) string {
		return func(key string) string {
			if key == k {
				return ""
			}
			return e.env[key]
		}
	}
	with := func(k, v string) func(string) string {
		return func(key string) string {
			if key == k {
				return v
			}
			return e.env[key]
		}
	}
	cases := map[string]struct {
		getenv func(string) string
		want   string
	}{
		"no configuration": {without("ERP_TEMPORAL_CONFIG"), "ERP_TEMPORAL_CONFIG is required"},
		"no trust domain":  {without("ERP_TRUST_DOMAIN"), "ERP_TRUST_DOMAIN is required"},
		"no cell":          {without("ERP_CELL"), "ERP_CELL is required"},
		"no password":      {without("ERP_TEMPORAL_DATABASE_PASSWORD"), "ERP_TEMPORAL_DATABASE_PASSWORD is required"},
		"unreadable":       {with("ERP_TEMPORAL_DATABASE_PASSWORD", "file:"+filepath.Join(e.dir, "missing")), "ERP_TEMPORAL_DATABASE_PASSWORD: read"},
		"short retention":  {with("ERP_TEMPORAL_RETENTION", "1h"), "ERP_TEMPORAL_RETENTION must be"},
		"bad retention":    {with("ERP_TEMPORAL_RETENTION", "a week"), "ERP_TEMPORAL_RETENTION must be"},
		"missing file":     {with("ERP_TEMPORAL_CONFIG", filepath.Join(e.dir, "missing.yaml")), "configuration"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer
			if got := cli(context.Background(), []string{"serve"}, c.getenv, &stderr); got != 2 {
				t.Errorf("exit code: got %d, want 2 (%s)", got, stderr.String())
			}
			if !strings.Contains(stderr.String(), c.want) {
				t.Errorf("stderr: got %q, want %q in it", stderr.String(), c.want)
			}
		})
	}
	t.Run("retention and namespace", func(t *testing.T) {
		s, err := loadSettings(with("ERP_TEMPORAL_RETENTION", "48h"))
		if err != nil {
			t.Fatal(err)
		}
		if s.retention != 48*time.Hour || s.namespace != "erp" || s.server != idOf("erp-server") || s.nodes != idOf("erp-temporal") {
			t.Errorf("settings: got %+v", s)
		}
		if s.password != "pw" {
			t.Errorf("password: got %q, want the file's content without its line break", s.password)
		}
	})
}

// TestEngineConfig: a configuration that would let a client in without a
// certificate of the environment's CA is refused.
func TestEngineConfig(t *testing.T) {
	e := newEngine(t, false)
	var stderr bytes.Buffer
	if got := cli(context.Background(), []string{"serve"}, e.getenv, &stderr); got != 2 || !strings.Contains(stderr.String(), "must require client certificates") {
		t.Errorf("got %d, %q, want 2 and the refusal", got, stderr.String())
	}
}

func TestUsage(t *testing.T) {
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"none":    {nil, "usage: erp-temporal"},
		"unknown": {[]string{"run"}, "unknown command"},
	} {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer
			if got := cli(context.Background(), c.args, func(string) string { return "" }, &stderr); got != 2 || !strings.Contains(stderr.String(), c.want) {
				t.Errorf("got %d, %q, want 2 and %q", got, stderr.String(), c.want)
			}
		})
	}
}
