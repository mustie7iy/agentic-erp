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
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mustie7iy/agentic-erp/db"
	"github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
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
	"github.com/mustie7iy/agentic-erp/internal/platform/ratelimit"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
	"go.opentelemetry.io/otel"
)

var exit = os.Exit

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr, nil)
	stop()
	exit(code)
}

// cli runs the server, or with "provision" as the first argument the
// provisioning command, and returns the process exit code: 2 for bad
// configuration or usage, 1 for a failure while running, 0 after a clean
// shutdown or a provisioned tenant.
func cli(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer, started func(api, admin net.Listener)) int {
	cfg, err := config.Load(getenv)
	if err != nil {
		fmt.Fprintln(stderr, "erp-server: configuration:", err)
		return 2
	}
	log := telemetry.NewLogger(stderr, cfg.LogLevel)
	if len(args) > 0 && args[0] == "provision" {
		return provisionCmd(ctx, args[1:], cfg, stdout, stderr)
	}
	if len(args) > 0 {
		fmt.Fprintf(stderr, "erp-server: unknown command %q (the server takes no arguments; erp-server provision creates a tenant)\n", args[0])
		return 2
	}
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
	settings := database.DefaultSettings
	settings.MaxConns = cfg.DBMaxConns
	pool, err := database.Open(ctx, cfg.DatabaseURL, settings)
	if err != nil {
		return err
	}
	defer pool.Close()
	if cfg.MigrateOnStart {
		if err := migrate(ctx, pool, log); err != nil {
			return err
		}
	}
	if err := partitions(ctx, pool, log); err != nil {
		return err
	}
	var static []authn.Static
	var dev *authn.DevIssuer
	if cfg.DevIssuer {
		dev = authn.NewDevIssuer()
		static = append(static, dev.Static())
		log.Warn("ERP_DEV_ISSUER is set: the admin listener mints tokens for any subject of any tenant (POST /dev/token); never in production")
	}
	verifier, err := authn.New(ctx, cfg.Issuers, static...)
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
	backlogs(pool, cfg, metrics)
	apiCfg := httpapi.Config{
		Pool: pool, AppRole: cfg.AppRole, Verifier: verifier, Pages: pages, FieldKeys: keys, Extractor: extractor, Connector: connector, ExtractDir: cfg.ExtractDir, Log: log,
		Limits: ratelimit.New(cfg.TenantQuota, cfg.PrincipalQuota), RequestTimeout: cfg.RequestTimeout,
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
	simulator := &configuration.SimulationWorker{Dependencies: httpapi.Configuration(apiCfg), Pool: pool, AppRole: cfg.AppRole, WorkerRole: cfg.WorkerRole, Log: log}
	simulatorDone := make(chan struct{})
	go func() { simulator.Run(runCtx); close(simulatorDone) }()
	maintenanceDone := make(chan struct{})
	go func() {
		maintain(runCtx, maintenanceInterval, func(ctx context.Context) error { return partitions(ctx, pool, log) }, log)
		close(maintenanceDone)
	}()
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
		{Handler: httpapi.Admin(ready(pool), metrics, devHandler(dev)), ReadHeaderTimeout: 5 * time.Second},
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
	<-simulatorDone
	<-maintenanceDone
	log.Info("erp-server stopped")
	return serveErr // nil after a requested shutdown; set when a listener failed
}

// maintenanceInterval is how often a replica checks the partition horizon;
// tests shorten it.
var maintenanceInterval = 6 * time.Hour

// partitions creates the partitions the coming months and years need.
func partitions(ctx context.Context, pool *database.Pool, log *slog.Logger) error {
	return pool.WithConn(ctx, func(c *pgx.Conn) error {
		return pgx.BeginFunc(ctx, c, func(tx pgx.Tx) error {
			created, err := db.EnsurePartitions(ctx, tx, time.Now())
			if err == nil && len(created) > 0 {
				log.Info("partitions created", "names", created)
			}
			return err
		})
	})
}

// maintain runs job every interval while the server runs; a failure is
// logged and retried at the next interval (readiness reports a partition
// horizon that got too short meanwhile).
func maintain(ctx context.Context, interval time.Duration, job func(context.Context) error, log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		if err := job(ctx); err != nil && ctx.Err() == nil {
			log.ErrorContext(ctx, "partition maintenance failed", "error", err)
		}
	}
}

// backlogs exposes the queues the workers drain (05-scalability §5.7:
// projector lag is a monitored metric), read on each scrape as the role
// that drains them.
func backlogs(pool *database.Pool, cfg config.Config, m *telemetry.Metrics) {
	count := func(role, sql string, args ...any) func(context.Context) (float64, error) {
		return func(ctx context.Context) (float64, error) {
			var n float64
			err := pool.InTx(ctx, database.Scope{Role: role, ReadOnly: true}, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, sql, args...).Scan(&n)
			})
			return n, err
		}
	}
	var parts []string
	for _, c := range outbox.Contexts {
		parts = append(parts, "SELECT count(*) FROM "+pgx.Identifier{c, "outbox"}.Sanitize()+" WHERE published_at IS NULL")
	}
	m.Gauge("erp_outbox_backlog", "Events waiting in the outbox for the relay.",
		count(cfg.RelayRole, "SELECT sum(n)::float8 FROM ("+strings.Join(parts, " UNION ALL ")+") AS b(n)"))
	m.Gauge("erp_projection_backlog", "Journal entries the balance projector has not reached.",
		count(cfg.ProjectorRole, "SELECT count(*)::float8 FROM ledger.balance_projection WHERE projected_at IS NULL"))
	m.Gauge("erp_payment_proposals_queued", "Payment proposals waiting for the worker.",
		count(cfg.WorkerRole, `SELECT count(*)::float8 FROM platform.operation o JOIN payments.payment_run r ON r.tenant_id = o.tenant_id AND r.operation_id = o.id
			WHERE o.kind = $1 AND r.status = 'proposing' AND o.status IN ('queued', 'cancelled')`, payments.ProposeKind))
	m.Gauge("erp_simulations_queued", "Configuration simulations waiting for the worker.",
		count(cfg.WorkerRole, `SELECT count(*)::float8 FROM platform.operation WHERE kind = $1 AND status = 'queued'`, configuration.SimulateKind))
}

// devHandler is the development issuer's token endpoint, or nil without one.
func devHandler(d *authn.DevIssuer) http.Handler {
	if d == nil {
		return nil
	}
	return d.Handler()
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

// ready reports whether this replica can serve: the database answers, its
// schema is at least the version this binary was built for, and the
// partitions reach far enough ahead.
func ready(pool *database.Pool) func(context.Context) error {
	return func(ctx context.Context) error {
		return pool.WithConn(ctx, func(c *pgx.Conn) error {
			v, err := db.CurrentVersion(ctx, c)
			if err != nil {
				return err
			}
			if v < db.LatestVersion() {
				return fmt.Errorf("schema version %d, need %d", v, db.LatestVersion())
			}
			// Horizon only reads the catalog: once the version check passed it answers.
			months, years, err := db.Horizon(ctx, c, time.Now())
			if err == nil && (months < db.MinAuditMonths || years < db.MinLedgerYears) {
				err = fmt.Errorf("partition horizon: %d audit months and %d ledger years ahead, need %d and %d", months, years, db.MinAuditMonths, db.MinLedgerYears)
			}
			return err
		})
	}
}
