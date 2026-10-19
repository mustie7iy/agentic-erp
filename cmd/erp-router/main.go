// Command erp-router is the global router of 05-scalability §5.4
// (ADR-0022): it verifies each request's token, looks the tenant up in the
// directory and forwards the request to the tenant's cell. Configuration
// comes from the environment (internal/router.LoadConfig).
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/globalplane"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/buildinfo"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/proxyproto"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf"
	"github.com/mustie7iy/agentic-erp/internal/router"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

var exit = os.Exit

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli(ctx, os.Getenv, os.Stderr, nil)
	stop()
	exit(code)
}

// cli runs the router and returns the process exit code: 2 for bad
// configuration, 1 for a failure while running, 0 after a clean shutdown.
func cli(ctx context.Context, getenv func(string) string, stderr io.Writer, started func(api, admin net.Listener)) int {
	cfg, err := router.LoadConfig(getenv)
	if err != nil {
		fmt.Fprintln(stderr, "erp-router: configuration:", err)
		return 2
	}
	log := telemetry.NewLogger(stderr, cfg.LogLevel)
	if err := run(ctx, cfg, log, started); err != nil {
		log.Error("erp-router stopped", "error", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, cfg router.Config, log *slog.Logger, started func(api, admin net.Listener)) error {
	dir, err := directory.OpenWith(ctx, cfg.DirectoryURL, cfg.DirectoryPassword)
	if err != nil {
		return err
	}
	defer dir.Close()
	verifier, err := newVerifier(ctx, cfg, dir)
	if err != nil {
		return err
	}
	metrics := telemetry.NewMetrics()
	traced := telemetry.Middleware(log, metrics, tracenoop.NewTracerProvider())
	transport := cfg.Transport
	if transport == nil && cfg.Identity != nil {
		transport = cellClient(cfg).Transport
	}
	handler := traced(&router.Router{Verifier: verifier, Directory: &directory.Cache{Store: dir, TTL: cfg.CacheTTL}, Log: log, Transport: transport,
		RequireTLS: cfg.Identity != nil, PeerOf: peerOf(cfg)})
	admin, err := adminMux(ctx, cfg, dir, metrics, traced, log)
	if err != nil {
		return err
	}
	ln, adminLn, err := listen(ctx, cfg)
	if err != nil {
		return err
	}
	ln, adminLn = secure(ctx, cfg, log, ln, adminLn)
	servers := []*http.Server{
		{Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second},
		{Handler: admin, ReadHeaderTimeout: 5 * time.Second},
	}
	errc := make(chan error, 2)
	for i, l := range []net.Listener{ln, adminLn} {
		go func() { errc <- servers[i].Serve(l) }()
	}
	build := buildinfo.Get()
	log.Info("erp-router started", "addr", ln.Addr().String(), "admin", adminLn.Addr().String(), "cache", cfg.CacheTTL.String(),
		"version", build.Version, "revision", build.Revision)
	if started != nil {
		started(ln, adminLn)
	}
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}
	shutdownCtx, done := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
	defer done()
	for _, s := range servers {
		// In-flight requests finish or time out; a server that did not stop
		// in time is abandoned with the process, so its error changes nothing.
		_ = s.Shutdown(shutdownCtx)
	}
	log.Info("erp-router stopped")
	return serveErr
}

// newVerifier trusts the tenants' registered identity providers, which
// decide a token's tenant through the directory's claims (ADR-0031); the
// cell checks them again.
func newVerifier(ctx context.Context, cfg router.Config, dir *directory.Postgres) (*authn.Verifier, error) {
	registry := &authn.RegistryCache{Registry: directory.IssuerRegistry{Directory: dir}, TTL: cfg.CacheTTL}
	providers := &authn.Providers{Production: cfg.Environment == string(authn.Production)}
	return authn.NewRegistered(ctx, authn.Environment(cfg.Environment), registry, providers, cfg.Issuers, cfg.Static...)
}

// listen opens the public and the admin listener, or neither; the public
// one reads the load balancer's PROXY header from its subnets.
func listen(ctx context.Context, cfg router.Config) (net.Listener, net.Listener, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Addr)
	if err != nil {
		return nil, nil, fmt.Errorf("erp-router: listen on %s: %w", cfg.Addr, err)
	}
	adminLn, err := lc.Listen(ctx, "tcp", cfg.AdminAddr)
	if err != nil {
		_ = ln.Close() // the admin listener's failure is the one reported
		return nil, nil, fmt.Errorf("erp-router: admin listen on %s: %w", cfg.AdminAddr, err)
	}
	if len(cfg.ProxyFrom) > 0 {
		// Under the public TLS: the load balancer's header comes first.
		ln = &proxyproto.Listener{Listener: ln, From: cfg.ProxyFrom}
	}
	return ln, adminLn, nil
}

// secure serves the public listener over TLS with the public certificate
// and the admin listener over TLS 1.3 as the router's identity, verifying
// a client's certificate when one is given, when the router has them; and
// keeps both read again until ctx ends (ADR-0033).
func secure(ctx context.Context, cfg router.Config, log *slog.Logger, ln, adminLn net.Listener) (net.Listener, net.Listener) {
	if cfg.Public != nil {
		go cfg.Public.Run(ctx, tlsconf.RefreshInterval, log)
		// Agents present client certificates, self-signed or not: the
		// handshake proves they hold the key, and the cell checks a
		// certificate-bound token against it (RFC 8705, ADR-0033).
		ln = tls.NewListener(ln, cfg.Public.ServerConfig(tlsconf.External, tls.RequestClientCert))
	}
	if cfg.Identity != nil {
		go cfg.Identity.Run(ctx, tlsconf.RefreshInterval, log)
		if cfg.Identity.HasCertificate() {
			adminLn = tls.NewListener(adminLn, cfg.Identity.ServerConfig(tlsconf.Internal, tls.VerifyClientCertIfGiven))
		}
	}
	return ln, adminLn
}

// cellClient reaches the cells as the router's identity, each only as
// the identity peerOf names for it; nil without an identity, which leaves
// the callers their defaults (ADR-0033).
func cellClient(cfg router.Config) *http.Client {
	if cfg.Identity == nil {
		return nil
	}
	return cfg.Identity.InternalClient(30 * time.Second)
}

// peerOf names the identity of the cell the directory names, which alone
// the router accepts for it (ADR-0033); nil without an identity.
func peerOf(cfg router.Config) func(cell string) string {
	if cfg.Identity == nil {
		return nil
	}
	return func(cell string) string { return tlsconf.ID(cfg.TrustDomain, cell, "erp-server") }
}

// adminMux is the admin listener's handler: liveness, metrics, the build's
// version, and with
// operator issuers the global control plane (api/directory.yaml).
func adminMux(ctx context.Context, cfg router.Config, dir *directory.Postgres, metrics *telemetry.Metrics, traced func(http.Handler) http.Handler, log *slog.Logger) (*http.ServeMux, error) {
	admin := http.NewServeMux()
	admin.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	admin.Handle("GET /metrics", metrics.Handler())
	admin.Handle("GET /version", buildinfo.Handler())
	if len(cfg.OperatorIssuers) > 0 {
		operators, err := authn.New(ctx, authn.Environment(cfg.Environment), cfg.OperatorIssuers, cfg.Static...)
		if err != nil {
			return nil, err
		}
		pages, _ := pagination.NewCodec(cfg.PageTokenKey) // the key's length was checked with the configuration
		plane := &globalplane.Service{Directory: dir, Pages: pages, Client: cellClient(cfg), RequireTLS: cfg.Identity != nil, PeerOf: peerOf(cfg)}
		admin.Handle(globalplane.Base+"/", traced(globalplane.Handler(plane, operators, log)))
		log.Info("global control plane served on the admin listener", "path", globalplane.Base, "operator_issuers", len(cfg.OperatorIssuers))
	}
	return admin, nil
}
