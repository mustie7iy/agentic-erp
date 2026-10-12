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
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
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
func cli(ctx context.Context, getenv func(string) string, stderr io.Writer, started func(net.Listener)) int {
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

func run(ctx context.Context, cfg router.Config, log *slog.Logger, started func(net.Listener)) error {
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
	handler := telemetry.Middleware(log, metrics, tracenoop.NewTracerProvider())(&router.Router{Verifier: verifier, Directory: &directory.Cache{Store: dir, TTL: cfg.CacheTTL}, Log: log, Transport: cfg.Transport})
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("erp-router started", "addr", ln.Addr().String(), "cache", cfg.CacheTTL.String())
	if started != nil {
		started(ln)
	}
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}
	shutdownCtx, done := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
	defer done()
	srv.Shutdown(shutdownCtx)
	log.Info("erp-router stopped")
	return serveErr
}
