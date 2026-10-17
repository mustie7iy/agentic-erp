package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/server/common/config"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/temporal"
)

// fakeServer stands for Temporal's server: Start and Stop return what it
// is given.
type fakeServer struct{ start, stop error }

func (f fakeServer) Start() error { return f.start }
func (f fakeServer) Stop() error  { return f.stop }

// fakeNamespaces stands for the SDK's namespace client: Register returns
// the errors given in turn, then nil.
type fakeNamespaces struct {
	client.NamespaceClient
	errs []error
}

func (f *fakeNamespaces) Register(context.Context, *workflowservice.RegisterNamespaceRequest) error {
	if len(f.errs) == 0 {
		return nil
	}
	err := f.errs[0]
	f.errs = f.errs[1:]
	return err
}

func (f *fakeNamespaces) Close() {}

// replace sets *target to v for the test.
func replace[T any](t *testing.T, target *T, v T) {
	t.Helper()
	old := *target
	*target = v
	t.Cleanup(func() { *target = old })
}

func engineCfg(t *testing.T, e *engine) (*config.Config, settings) {
	t.Helper()
	s, err := loadSettings(e.getenv)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := engineConfig(s)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, s
}

// TestServeFailures: each step's failure stops the engine with its reason.
func TestServeFailures(t *testing.T) {
	e := newEngine(t, true)
	cfg, s := engineCfg(t, e)
	boom := errors.New("boom")
	cases := map[string]struct {
		server     temporal.Server
		newErr     error
		namespaces []error
		want       string
	}{
		"server":    {newErr: boom, want: "server: boom"},
		"start":     {server: fakeServer{start: boom}, want: "start: boom"},
		"namespace": {server: fakeServer{}, namespaces: []error{serviceerror.NewPermissionDenied("no", "")}, want: "register"},
		"stop":      {server: fakeServer{stop: boom}, want: "stop: boom"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			replace(t, &newServer, func(*config.Config, settings, log.Logger) (temporal.Server, error) { return c.server, c.newErr })
			replace(t, &newNamespaceClient, func(client.Options) (client.NamespaceClient, error) {
				return &fakeNamespaces{errs: c.namespaces}, nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // serve returns once started and registered
			err := serve(ctx, cfg, s, log.NewNoopLogger())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error: got %v, want %q", err, c.want)
			}
		})
	}
	t.Run("exit code", func(t *testing.T) {
		replace(t, &newServer, func(*config.Config, settings, log.Logger) (temporal.Server, error) { return nil, boom })
		var stderr bytes.Buffer
		if got := cli(context.Background(), []string{"serve"}, e.getenv, &stderr); got != 1 {
			t.Errorf("exit code: got %d, want 1 (%s)", got, stderr.String())
		}
	})
}

// TestNamespace: an existing namespace is no error; the frontend still
// starting is waited for, within bounds; any other refusal is reported.
func TestNamespace(t *testing.T) {
	e := newEngine(t, true)
	cfg, s := engineCfg(t, e)
	replace(t, &retryEvery, time.Millisecond)
	unavailable := serviceerror.NewUnavailable("starting")
	cases := map[string]struct {
		errs []error
		wait time.Duration
		want string
	}{
		"registered":        {nil, time.Minute, ""},
		"already there":     {[]error{serviceerror.NewNamespaceAlreadyExists("erp")}, time.Minute, ""},
		"starting, then up": {[]error{unavailable, unavailable}, time.Minute, ""},
		"never up":          {[]error{unavailable, unavailable, unavailable, unavailable, unavailable}, 0, "did not answer"},
		"refused":           {[]error{serviceerror.NewInvalidArgument("bad")}, time.Minute, "register: bad"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			replace(t, &registerWait, c.wait)
			replace(t, &newNamespaceClient, func(client.Options) (client.NamespaceClient, error) { return &fakeNamespaces{errs: c.errs}, nil })
			err := ensureNamespace(context.Background(), cfg, s)
			if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Errorf("error: got %v, want %q", err, c.want)
			}
		})
	}
	t.Run("client", func(t *testing.T) {
		replace(t, &newNamespaceClient, func(client.Options) (client.NamespaceClient, error) { return nil, errors.New("boom") })
		if err := ensureNamespace(context.Background(), cfg, s); err == nil || !strings.Contains(err.Error(), "client: boom") {
			t.Errorf("error: got %v, want the client's", err)
		}
	})
	t.Run("certificate", func(t *testing.T) {
		bad := *cfg
		bad.Global.TLS.SystemWorker.CertFile = filepath.Join(e.dir, "missing.crt")
		if err := ensureNamespace(context.Background(), &bad, s); err == nil || !strings.Contains(err.Error(), "system worker certificate") {
			t.Errorf("error: got %v, want the certificate's", err)
		}
	})
}

func TestSystemTLS(t *testing.T) {
	e := newEngine(t, true)
	cfg, _ := engineCfg(t, e)
	notPEM := filepath.Join(e.dir, "not.pem")
	if err := os.WriteFile(notPEM, []byte("no certificate here"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		roots []string
		want  string
	}{
		"missing CA": {[]string{filepath.Join(e.dir, "missing.crt")}, "system worker CA"},
		"not a PEM":  {[]string{notPEM}, "no certificate"},
	} {
		t.Run(name, func(t *testing.T) {
			w := cfg.Global.TLS.SystemWorker
			w.Client.RootCAFiles = c.roots
			if _, err := systemTLS(w); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error: got %v, want %q", err, c.want)
			}
		})
	}
}

// TestEngineConfigChecks: each TLS setting a production engine needs is
// required, and the node's address comes from POD_IP.
func TestEngineConfigChecks(t *testing.T) {
	e := newEngine(t, true)
	_, s := engineCfg(t, e)
	s.address = "10.0.0.7"
	cfg, err := engineConfig(s)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Global.Membership.BroadcastAddress != "10.0.0.7" {
		t.Errorf("broadcast address: got %q, want POD_IP's", cfg.Global.Membership.BroadcastAddress)
	}
	if got := cfg.Persistence.DataStores["default"].SQL.Password; got != "pw" {
		t.Errorf("password: got %q, want the secret's", got)
	}
	for name, c := range map[string]struct {
		o    engineOptions
		want string
	}{
		"frontend without CAs":  {engineOptions{noFrontend: true}, "frontend.server must require"},
		"internode without CAs": {engineOptions{noInternode: true}, "internode.server must require"},
		"no system certificate": {engineOptions{noSystem: true}, "systemWorker must present"},
	} {
		t.Run(name, func(t *testing.T) {
			bad, err := loadSettings(newEngineWith(t, c.o).getenv)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := engineConfig(bad); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error: got %v, want %q", err, c.want)
			}
		})
	}
}

// TestMainExits: main exits with cli's code.
func TestMainExits(t *testing.T) {
	exited := -1
	replace(t, &exit, func(code int) { exited = code })
	main() // the test binary's arguments are no command
	if exited != 2 {
		t.Errorf("exit code: got %d, want 2", exited)
	}
	q := quiet{}
	q.Debug("x")
	q.Info("x")
	q.Warn("x")
	q.Error("x")
}

// TestNewServerTLS: TLS settings Temporal cannot use stop the start.
func TestNewServerTLS(t *testing.T) {
	cfg, s := engineCfg(t, newEngine(t, true))
	cfg.Global.TLS.Frontend.Server.CertData = "both a file and data"
	if _, err := newServer(cfg, s, log.NewNoopLogger()); err == nil || !strings.Contains(err.Error(), "tls:") {
		t.Errorf("error: got %v, want the TLS settings'", err)
	}
}

func TestCheckConfig(t *testing.T) {
	good := newEngine(t, true).env["ERP_TEMPORAL_CONFIG"]
	bad := newEngine(t, false).env["ERP_TEMPORAL_CONFIG"]
	// A configuration Temporal's server would refuse when it starts: its
	// visibility on a store the configuration does not define.
	content, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	invalid := filepath.Join(t.TempDir(), "invalid.yaml")
	if err := os.WriteFile(invalid, []byte(strings.Replace(string(content), "visibilityStore: default", "visibilityStore: missing", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		file string
		code int
		want string
	}{
		"accepted": {good, 0, "a configuration serve accepts"},
		"refused":  {bad, 1, "must require client certificates"},
		"invalid":  {invalid, 1, "missing"},
		"none":     {"", 2, "ERP_TEMPORAL_CONFIG is required"},
	} {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer
			got := cli(context.Background(), []string{"check-config"}, func(k string) string {
				return map[string]string{"ERP_TEMPORAL_CONFIG": c.file}[k]
			}, &stderr)
			if got != c.code || !strings.Contains(stderr.String(), c.want) {
				t.Errorf("got %d, %q, want %d and %q", got, stderr.String(), c.code, c.want)
			}
		})
	}
}
