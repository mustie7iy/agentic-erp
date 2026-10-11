// Command erp-server runs the platform's public API (ADR-0014): the server
// generated from api/openapi.yaml behind telemetry, authentication and the
// unit of work, an admin listener for liveness, readiness and metrics, the
// outbox relay to NATS JetStream, the ledger's balance projector and the
// payment proposal worker.
// Configuration comes from the environment (internal/platform/config).
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

	"github.com/jackc/pgx/v5"
	"github.com/mustie7iy/agentic-erp/db"
	"github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi"
	"github.com/mustie7iy/agentic-erp/internal/platform/natsbus"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
	"go.opentelemetry.io/otel"
)

var exit = os.Exit

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli(ctx, os.Getenv, os.Stderr, nil)
	stop()
	exit(code)
}

// cli returns the process exit code: 2 for bad configuration, 1 for a
// failure while running, 0 after a clean shutdown.
func cli(ctx context.Context, getenv func(string) string, stderr io.Writer, started func(api, admin net.Listener)) int {
	cfg, err := config.Load(getenv)
	if err != nil {
		fmt.Fprintln(stderr, "erp-server: configuration:", err)
		return 2
	}
	log := telemetry.NewLogger(stderr, cfg.LogLevel)
	if err := run(ctx, cfg, log, started); err != nil {
		log.Error("erp-server stopped", "error", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger, started func(api, admin net.Listener)) error {
	tp, err := telemetry.Tracing(ctx, cfg.OTLPEndpoint)
	if err != nil {
		return err
	}
	defer tp.Shutdown(context.WithoutCancel(ctx))
	pool, err := database.Open(ctx, cfg.DatabaseURL, database.DefaultSettings)
	if err != nil {
		return err
	}
	defer pool.Close()
	if cfg.MigrateOnStart {
		if err := migrate(ctx, pool, log); err != nil {
			return err
		}
	}
	verifier, err := authn.New(ctx, cfg.Issuers)
	if err != nil {
		return err
	}
	pages, err := pagination.NewCodec(cfg.PageTokenKey)
	if err != nil {
		return err
	}
	keys, err := fieldcrypt.New(cfg.FieldKey)
	if err != nil {
		return err
	}
	var extractor capture.Extractor
	if cfg.LLMEnabled {
		extractor = &capture.ClaudeExtractor{LLM: llm.New()}
	}
	var connector payments.Connector
	if cfg.BankOutboxDir != "" {
		connector = payments.DirConnector{Dir: cfg.BankOutboxDir}
	}
	metrics := telemetry.NewMetrics()
	apiCfg := httpapi.Config{
		Pool: pool, AppRole: cfg.AppRole, Verifier: verifier, Pages: pages, FieldKeys: keys, Extractor: extractor, Connector: connector, Log: log,
		Metrics: metrics, Tracer: tp, Meter: otel.GetMeterProvider(), Now: time.Now,
	}
	apiHandler, err := httpapi.New(apiCfg)
	if err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	projector := &ledger.Projector{Pool: pool, Role: cfg.ProjectorRole, Log: log}
	projectorDone := make(chan struct{})
	go func() { projector.Run(runCtx); close(projectorDone) }()
	proposer := &payments.Proposer{Endpoints: httpapi.Payments(apiCfg), Pool: pool, AppRole: cfg.AppRole, WorkerRole: cfg.WorkerRole, Log: log}
	proposerDone := make(chan struct{})
	go func() { proposer.Run(runCtx); close(proposerDone) }()
	relayDone := make(chan struct{})
	if cfg.NATSURL == "" {
		log.Warn("ERP_NATS_URL not set: events stay in the outbox until a relay runs")
		close(relayDone)
	} else {
		bus, err := natsbus.Connect(cfg.NATSURL)
		if err != nil {
			return err
		}
		defer bus.Close()
		if err := bus.EnsureStreams(ctx, cfg.Cell); err != nil {
			return err
		}
		relay := &outbox.Relay{Pool: pool, Role: cfg.RelayRole, Publisher: bus, Cell: cfg.Cell, Log: log}
		go func() { relay.Run(runCtx); close(relayDone) }()
	}

	apiLn, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return err
	}
	adminLn, err := net.Listen("tcp", cfg.AdminAddr)
	if err != nil {
		apiLn.Close()
		return err
	}
	servers := []*http.Server{
		{Handler: apiHandler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second},
		{Handler: httpapi.Admin(ready(pool), metrics), ReadHeaderTimeout: 5 * time.Second},
	}
	errc := make(chan error, 2)
	for i, ln := range []net.Listener{apiLn, adminLn} {
		go func() { errc <- servers[i].Serve(ln) }()
	}
	log.Info("erp-server started", "api", apiLn.Addr().String(), "admin", adminLn.Addr().String(), "cell", cfg.Cell)
	if started != nil {
		started(apiLn, adminLn)
	}

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}
	shutdownCtx, done := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
	defer done()
	for _, s := range servers {
		s.Shutdown(shutdownCtx) // in-flight requests finish or time out
	}
	cancel()
	<-relayDone
	<-projectorDone
	<-proposerDone
	log.Info("erp-server stopped")
	return serveErr // nil after a requested shutdown; set when a listener failed
}

// migrate applies pending migrations; replicas starting together serialize
// on the runner's lock.
func migrate(ctx context.Context, pool *database.Pool, log *slog.Logger) error {
	return pool.WithConn(ctx, func(c *pgx.Conn) error {
		applied, err := db.Migrate(ctx, c)
		if err == nil {
			log.Info("migrations applied", "count", len(applied), "names", applied)
		}
		return err
	})
}

// ready reports whether this replica can serve: the database answers and
// its schema is at least the version this binary was built for.
func ready(pool *database.Pool) func(context.Context) error {
	return func(ctx context.Context) error {
		return pool.WithConn(ctx, func(c *pgx.Conn) error {
			v, err := db.CurrentVersion(ctx, c)
			if err == nil && v < db.LatestVersion() {
				err = fmt.Errorf("schema version %d, need %d", v, db.LatestVersion())
			}
			return err
		})
	}
}
