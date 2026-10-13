// Command erp-router is the global router of 05-scalability §5.4
// (ADR-0022): it verifies each request's token, looks the tenant up in the
// directory and forwards the request to the tenant's cell. Configuration
// comes from the environment (internal/router.LoadConfig).
package main

import (
	"context"
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
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
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
	verifier, err := authn.New(ctx, cfg.Issuers, cfg.Static...)
	if err != nil {
		return err
	}
	dir, err := directory.Open(ctx, cfg.DirectoryURL)
	if err != nil {
		return err
	}
	defer dir.Close()
	metrics := telemetry.NewMetrics()
	traced := telemetry.Middleware(log, metrics, tracenoop.NewTracerProvider())
	handler := traced(&router.Router{Verifier: verifier, Directory: &directory.Cache{Store: dir, TTL: cfg.CacheTTL}, Log: log, Transport: cfg.Transport})
	admin, err := adminMux(ctx, cfg, dir, metrics, traced, log)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("erp-router: listen on %s: %w", cfg.Addr, err)
	}
	adminLn, err := net.Listen("tcp", cfg.AdminAddr)
	if err != nil {
		_ = ln.Close() // the admin listener's failure is the one reported
		return fmt.Errorf("erp-router: admin listen on %s: %w", cfg.AdminAddr, err)
	}
	servers := []*http.Server{
		{Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second},
		{Handler: admin, ReadHeaderTimeout: 5 * time.Second},
	}
	errc := make(chan error, 2)
	for i, l := range []net.Listener{ln, adminLn} {
		go func() { errc <- servers[i].Serve(l) }()
	}
	log.Info("erp-router started", "addr", ln.Addr().String(), "admin", adminLn.Addr().String(), "cache", cfg.CacheTTL.String())
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

// adminMux is the admin listener's handler: liveness, metrics, and with
// operator issuers the global control plane (api/directory.yaml).
func adminMux(ctx context.Context, cfg router.Config, dir *directory.Postgres, metrics *telemetry.Metrics, traced func(http.Handler) http.Handler, log *slog.Logger) (*http.ServeMux, error) {
	admin := http.NewServeMux()
	admin.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	admin.Handle("GET /metrics", metrics.Handler())
	if len(cfg.OperatorIssuers) > 0 {
		operators, err := authn.New(ctx, cfg.OperatorIssuers, cfg.Static...)
		if err != nil {
			return nil, err
		}
		pages, _ := pagination.NewCodec(cfg.PageTokenKey) // the key's length was checked with the configuration
		admin.Handle(globalplane.Base+"/", traced(globalplane.Handler(&globalplane.Service{Directory: dir, Pages: pages}, operators, log)))
		log.Info("global control plane served on the admin listener", "path", globalplane.Base, "operator_issuers", len(cfg.OperatorIssuers))
	}
	return admin, nil
}
