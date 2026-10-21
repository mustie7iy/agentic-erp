// Command erp-web is the web workspace for finance users (ADR-0052): pages
// rendered on the server, calling the public API through the router as the
// signed-in person. It runs in the erp-global chart beside the router, on
// its own hostname and load balancer. Configuration comes from the
// environment (internal/web.LoadConfig).
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

	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/mustie7iy/agentic-erp/internal/platform/buildinfo"
	"github.com/mustie7iy/agentic-erp/internal/platform/proxyproto"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf"
	"github.com/mustie7iy/agentic-erp/internal/web"
)

var exit = os.Exit

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli(ctx, os.Getenv, os.Stderr, nil)
	stop()
	exit(code)
}

// cli runs erp-web and returns the process exit code: 2 for bad
// configuration, 1 for a failure while running, 0 after a clean shutdown.
func cli(ctx context.Context, getenv func(string) string, stderr io.Writer, started func(public, admin net.Listener)) int {
	cfg, err := web.LoadConfig(getenv)
	if err != nil {
		fmt.Fprintln(stderr, "erp-web: configuration:", err)
		return 2
	}
	log := telemetry.NewLogger(stderr, cfg.LogLevel)
	if err := run(ctx, cfg, log, started); err != nil {
		log.Error("erp-web stopped", "error", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, cfg web.Config, log *slog.Logger, started func(public, admin net.Listener)) error {
	build := buildinfo.Get()
	srv, err := web.New(cfg, build, log)
	if err != nil {
		return err
	}
	metrics := telemetry.NewMetrics()
	handler := telemetry.Middleware(log, metrics, tracenoop.NewTracerProvider())(srv.Handler())
	ln, adminLn, err := listen(ctx, cfg)
	if err != nil {
		return err
	}
	if cfg.Public != nil {
		go cfg.Public.Run(ctx, tlsconf.RefreshInterval, log)
		ln = tls.NewListener(ln, cfg.Public.ServerConfig(tlsconf.External, tls.NoClientCert))
	}
	servers := []*http.Server{
		{Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second},
		{Handler: adminMux(metrics), ReadHeaderTimeout: 5 * time.Second},
	}
	errc := make(chan error, 2)
	for i, l := range []net.Listener{ln, adminLn} {
		go func() { errc <- servers[i].Serve(l) }()
	}
	log.Info("erp-web started", "addr", ln.Addr().String(), "admin", adminLn.Addr().String(), "api", cfg.APIURL.String(),
		"tls", cfg.Public != nil, "version", build.Version, "revision", build.Revision)
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
	log.Info("erp-web stopped")
	return serveErr
}

// listen opens the public and the admin listener, or neither; the public
// one reads the load balancer's PROXY header from its subnets.
func listen(ctx context.Context, cfg web.Config) (net.Listener, net.Listener, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Addr)
	if err != nil {
		return nil, nil, fmt.Errorf("erp-web: listen on %s: %w", cfg.Addr, err)
	}
	adminLn, err := lc.Listen(ctx, "tcp", cfg.AdminAddr)
	if err != nil {
		_ = ln.Close() // the admin listener's failure is the one reported
		return nil, nil, fmt.Errorf("erp-web: admin listen on %s: %w", cfg.AdminAddr, err)
	}
	if len(cfg.ProxyFrom) > 0 {
		// Under the public TLS: the load balancer's header comes first.
		ln = &proxyproto.Listener{Listener: ln, From: cfg.ProxyFrom}
	}
	return ln, adminLn, nil
}

// adminMux is the admin listener's handler, for the kubelet's probes
// alone (the network policy admits no other pod): liveness, metrics and
// the build's version.
func adminMux(metrics *telemetry.Metrics) *http.ServeMux {
	admin := http.NewServeMux()
	admin.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	admin.Handle("GET /metrics", metrics.Handler())
	admin.Handle("GET /version", buildinfo.Handler())
	return admin
}
