// Command erp-server runs the platform's public API (ADR-0014): the server
// generated from api/openapi.yaml behind telemetry, authentication and the
// unit of work, an admin listener for liveness, readiness, metrics and the
// operators' control plane (api/control.yaml), the outbox relay to NATS
// JetStream, the ledger's balance projector and the workers.
// Configuration comes from the environment (internal/platform/config).
package main

import (
	"context"
	"crypto/sha256"
	"errors"
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
	"github.com/mustie7iy/agentic-erp/internal/audit"
	"github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/controlplane"
	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/offboarding"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/awscreds"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi"
	"github.com/mustie7iy/agentic-erp/internal/platform/natsbus"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/ratelimit"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
	"github.com/mustie7iy/agentic-erp/internal/platform/workflow"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/provisioning"
	"github.com/mustie7iy/agentic-erp/internal/workflows"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

var exit = os.Exit

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr, nil)
	stop()
	exit(code)
}

// cli runs the server, or with "provision", "rewrap" or "import" as the
// first argument that command, and returns the process exit code: 2 for bad
// configuration or usage, 1 for a failure while running, 0 after a clean
// shutdown or a completed command.
func cli(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer, started func(api, admin net.Listener)) int {
	cfg, err := config.Load(getenv)
	if err != nil {
		fmt.Fprintln(stderr, "erp-server: configuration:", err)
		return 2
	}
	log := telemetry.NewLogger(stderr, cfg.LogLevel)
	if len(args) > 0 {
		switch args[0] {
		case "provision":
			return provisionCmd(ctx, args[1:], cfg, stdout, stderr)
		case "rewrap":
			return rewrapCmd(ctx, args[1:], cfg, stdout, stderr)
		case "import":
			return importCmd(ctx, args[1:], cfg, stdout, stderr)
		}
		fmt.Fprintf(stderr, "erp-server: unknown command %q (the server takes no arguments; erp-server provision creates a tenant, erp-server rewrap moves tenant keys onto the current master, erp-server import brings an exported tenant in)\n", args[0])
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
		log.Warn("ERP_DEV_ISSUER is set: the admin listener mints tokens for any subject of any tenant, and operator tokens (POST /dev/token); never in production")
	}
	verifier, err := authn.New(ctx, cfg.Issuers, static...)
	if err != nil {
		return err
	}
	pages, err := pagination.NewCodec(cfg.PageTokenKey)
	if err != nil {
		return err
	}
	keys, err := keyring(cfg, pool)
	if err != nil {
		return err
	}
	store := objects(cfg.Objects, cfg.AWS)
	if store == nil {
		log.Warn("no object storage configured (ERP_OBJECT_DIR or ERP_OBJECT_S3_*): documents and bank files are not kept, the audit chain is not anchored, offboarded tenants are not exported")
	}
	// The control plane trusts the operator issuers, and the development
	// issuer's operator tokens; without either it is not served.
	// Offboarded tenants are exported and erased by a worker, where there
	// is a store for the exports; the control plane erases on request.
	var offboarder *offboarding.Worker
	if store != nil {
		offboarder = &offboarding.Worker{Pool: pool, Store: store, AppRole: cfg.AppRole, WorkerRole: cfg.WorkerRole, Interval: offboardInterval,
			Grace: cfg.OffboardGrace, Retention: cfg.Objects.Retention, Log: log}
	}
	// With a global directory (ADR-0022) the cell keeps its tenants' entries
	// current, right after a change and in rounds.
	var sync *directory.Sync
	var dirDone = make(chan struct{})
	if cfg.DirectoryURL != "" {
		dir, err := directory.Open(ctx, cfg.DirectoryURL)
		if err != nil {
			return err
		}
		defer dir.Close()
		sync = &directory.Sync{Pool: pool, ControlRole: cfg.ControlRole, Directory: dir, Cell: cfg.Cell, APIURL: cfg.CellAPIURL,
			Region: cfg.CellRegion, ControlURL: cfg.CellControlURL, Accepting: cfg.CellAccepting, Interval: directoryInterval, Log: log}
	} else {
		close(dirDone)
	}
	var control http.Handler
	if len(cfg.OperatorIssuers)+len(static) > 0 {
		operators, err := authn.New(ctx, cfg.OperatorIssuers, static...)
		if err != nil {
			return err
		}
		service := &controlplane.Service{Pool: pool, AppRole: cfg.AppRole, ControlRole: cfg.ControlRole, Pages: pages,
			Keys: keys, Provisioner: &provisioning.Provisioner{Pool: pool, AppRole: cfg.AppRole, Keys: keys, Configuration: httpapi.Configuration(httpapi.Config{FieldKeys: keys})},
			Offboarding: offboarder, Directory: sync}
		control = controlplane.Handler(service, operators, log)
		log.Info("control plane served on the admin listener", "path", controlplane.Base, "operator_issuers", len(cfg.OperatorIssuers), "dev_issuer", cfg.DevIssuer)
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
	// The workflow engine (ADR-0012): with ERP_TEMPORAL_ADDRESS the server
	// hosts a worker and dispatches queued operations to it; without, the
	// in-process workers of the transition keep running them.
	var engine *workflow.Temporal
	var engineClient workflow.Client
	if cfg.Temporal.Address != "" {
		engine, err = workflow.Dial(ctx, workflow.Config{Address: cfg.Temporal.Address, Namespace: cfg.Temporal.Namespace})
		if err != nil {
			return err
		}
		defer engine.Close()
		engineClient = engine
		log.Info("workflow engine connected", "address", cfg.Temporal.Address, "namespace", cfg.Temporal.Namespace, "task_queue", cfg.Temporal.TaskQueue)
	}
	apiCfg := httpapi.Config{
		Pool: pool, AppRole: cfg.AppRole, Verifier: verifier, Pages: pages, FieldKeys: keys, Extractor: extractor, Connector: connector, ExtractDir: cfg.ExtractDir, Objects: store, Secrets: cfg.Secrets, Log: log,
		UploadKey: uploadKey(cfg.PageTokenKey), PublicURL: cfg.PublicURL, Workflows: engineClient,
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
	// workerErr is the workflow worker's end: nil after a requested shutdown,
	// the failure when it could not start (joined into the server's exit).
	proposerDone, workerDone, dispatchDone, workerErr := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan error, 1)
	if engine == nil {
		proposer := &payments.Proposer{Endpoints: httpapi.Payments(apiCfg), Pool: pool, AppRole: cfg.AppRole, WorkerRole: cfg.WorkerRole, Log: log}
		go func() { proposer.Run(runCtx); close(proposerDone) }()
		close(workerDone)
		close(dispatchDone)
		workerErr <- nil
	} else {
		close(proposerDone)
		activities := &workflows.Payments{Pool: pool, AppRole: cfg.AppRole, Endpoints: httpapi.Payments(apiCfg)}
		go func() {
			workerErr <- engine.Worker(runCtx, cfg.Temporal.TaskQueue, func(r workflow.Registry) { workflows.Register(r, activities) })
			close(workerDone)
		}()
		dispatcher := &workflows.Dispatcher{Pool: pool, WorkerRole: cfg.WorkerRole, Client: engine, Queue: cfg.Temporal.TaskQueue, Interval: dispatchInterval, Log: log}
		go func() { dispatcher.Run(runCtx); close(dispatchDone) }()
	}
	simulator := &configuration.SimulationWorker{Dependencies: httpapi.Configuration(apiCfg), Pool: pool, AppRole: cfg.AppRole, WorkerRole: cfg.WorkerRole, Log: log}
	simulatorDone := make(chan struct{})
	go func() { simulator.Run(runCtx); close(simulatorDone) }()
	maintenanceDone := make(chan struct{})
	go func() {
		maintain(runCtx, maintenanceInterval, func(ctx context.Context) error { return partitions(ctx, pool, log) }, log)
		close(maintenanceDone)
	}()
	if sync != nil {
		go func() { sync.Run(runCtx); close(dirDone) }()
	}
	anchorDone, offboardDone := make(chan struct{}), make(chan struct{})
	if store == nil {
		close(anchorDone)
		close(offboardDone)
	} else {
		anchorer := &audit.Anchorer{Pool: pool, Store: store, AppRole: cfg.AppRole, WorkerRole: cfg.WorkerRole, Interval: anchorInterval, Retention: cfg.Objects.Retention, Log: log}
		go func() { anchorer.Run(runCtx); close(anchorDone) }()
		go func() { offboarder.Run(runCtx); close(offboardDone) }()
	}
	// Keys older than the policy's age are rotated by the server (ADR-0028).
	rotateDone := make(chan struct{})
	if cfg.KeyRotationAge == 0 {
		close(rotateDone)
	} else {
		rotator := &controlplane.KeyRotator{Pool: pool, Keys: keys, AppRole: cfg.AppRole, WorkerRole: cfg.WorkerRole, Age: cfg.KeyRotationAge, Interval: rotationInterval, Log: log}
		go func() { rotator.Run(runCtx); close(rotateDone) }()
	}
	// Active contracts past their validity are expired in rounds (the
	// contract's ContractExpired is the job's).
	expirer := &procurement.ContractExpirer{Pool: pool, AppRole: cfg.AppRole, WorkerRole: cfg.WorkerRole, Interval: expiryInterval, Log: log}
	expireDone := make(chan struct{})
	go func() { expirer.Run(runCtx); close(expireDone) }()
	// Every tenant's audit chain is verified against its anchors in rounds
	// (ADR-0011); a chain that stops verifying is an alarm.
	verifyDone := make(chan struct{})
	if cfg.AuditVerify == 0 {
		close(verifyDone)
		log.Warn("ERP_AUDIT_VERIFY_INTERVAL is 0: audit chains are verified on request only")
	} else {
		verifier := &audit.Verifier{Pool: pool, Store: store, AppRole: cfg.AppRole, WorkerRole: cfg.WorkerRole, Interval: cfg.AuditVerify, Log: log}
		go func() { verifier.Run(runCtx); close(verifyDone) }()
	}
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
		{Handler: httpapi.Admin(ready(pool), metrics, devHandler(dev), traced(control, log, metrics, tp)), ReadHeaderTimeout: 5 * time.Second},
	}
	errc := make(chan error, 2)
	for i, ln := range []net.Listener{apiLn, adminLn} {
		go func() { errc <- servers[i].Serve(ln) }()
	}
	log.Info("erp-server started", "api", apiLn.Addr().String(), "admin", adminLn.Addr().String(), "cell", cfg.Cell, "directory", cfg.DirectoryURL != "")
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
	<-dispatchDone
	<-workerDone
	<-simulatorDone
	<-maintenanceDone
	<-anchorDone
	<-offboardDone
	<-verifyDone
	<-rotateDone
	<-expireDone
	<-dirDone
	log.Info("erp-server stopped")
	return errors.Join(serveErr, <-workerErr) // nil after a requested shutdown; set when a listener or the workflow worker failed
}

// maintenanceInterval is how often a replica checks the partition horizon,
// anchorInterval how often it anchors moved chain heads, offboardInterval how
// often offboarded tenants are exported and erased; tests shorten them.
var (
	maintenanceInterval = 6 * time.Hour
	anchorInterval      = time.Minute
	offboardInterval    = time.Minute
	directoryInterval   = time.Minute
	rotationInterval    = time.Hour
	expiryInterval      = time.Hour
	dispatchInterval    = time.Second
)

// keyring builds the field encryption ring: the KMS master first when there
// is one, then the configured local masters, the legacy key when there is
// one, and the pool for lookups outside requests.
func keyring(cfg config.Config, pool *database.Pool) (*fieldcrypt.Keyring, error) {
	ring := &fieldcrypt.Keyring{Pool: pool, AppRole: cfg.AppRole}
	if cfg.KMS.Enabled() {
		ring.Masters = append(ring.Masters, fieldcrypt.KMS{Name: cfg.KMS.Name, KeyID: cfg.KMS.Key, Endpoint: cfg.KMS.Endpoint, Region: cfg.KMS.Region,
			Credentials: credentials(cfg.AWS, cfg.KMS.AccessKey, cfg.KMS.SecretKey)})
	}
	for _, mk := range cfg.FieldKeys {
		m, err := fieldcrypt.NewLocalMaster(mk.ID, mk.Secret)
		if err != nil {
			return nil, err
		}
		ring.Masters = append(ring.Masters, m)
	}
	if len(cfg.FieldKey) > 0 {
		legacy, err := fieldcrypt.New(cfg.FieldKey)
		if err != nil {
			return nil, err
		}
		ring.Legacy = legacy
	}
	return ring, nil
}

// uploadKey derives the key that signs the API's own upload URLs from the
// page-token key, so every replica signs and checks the same way without
// one more setting; both keys guard short-lived URLs of one deployment.
func uploadKey(pageTokenKey []byte) []byte {
	h := sha256.New()
	h.Write([]byte("erp upload urls\n"))
	h.Write(pageTokenKey)
	return h.Sum(nil)
}

// credentials are an AWS client's: the access keys from settings when set,
// else the deployment's ambient ones (ADR-0024), cached.
func credentials(aws config.AWS, accessKey, secretKey string) awscreds.Provider {
	return awscreds.Default(awscreds.Static{AccessKey: accessKey, SecretKey: secretKey}, aws.Getenv, aws.STSEndpoint, aws.IMDSEndpoint)
}

// objects opens the configured object store, or nil without one.
func objects(o config.Objects, aws config.AWS) blobstore.Store {
	switch {
	case o.Dir != "":
		return blobstore.Dir{Root: o.Dir}
	case o.Endpoint != "":
		return blobstore.S3{Endpoint: o.Endpoint, Bucket: o.Bucket, Region: o.Region, Credentials: credentials(aws, o.AccessKey, o.SecretKey)}
	}
	return nil
}

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
	var parts, oldest []string
	for _, c := range outbox.Contexts {
		parts = append(parts, "SELECT count(*) FROM "+pgx.Identifier{c, "outbox"}.Sanitize()+" WHERE published_at IS NULL")
		oldest = append(oldest, "SELECT min(occurred_at) FROM "+pgx.Identifier{c, "outbox"}.Sanitize()+" WHERE published_at IS NULL")
	}
	m.Gauge("erp_outbox_backlog", "Events waiting in the outbox for the relay.",
		count(cfg.RelayRole, "SELECT sum(n)::float8 FROM ("+strings.Join(parts, " UNION ALL ")+") AS b(n)"))
	// Lag is what an alert watches (05 §5.6): the age of the oldest waiting
	// row, one index lookup per outbox however large the backlog.
	m.Gauge("erp_outbox_lag_seconds", "Age of the oldest event waiting in the outbox for the relay.",
		count(cfg.RelayRole, "SELECT coalesce(extract(epoch FROM now() - min(t)), 0)::float8 FROM ("+strings.Join(oldest, " UNION ALL ")+") AS b(t)"))
	m.Gauge("erp_projection_backlog", "Journal entries the balance projector has not reached.",
		count(cfg.ProjectorRole, "SELECT count(*)::float8 FROM ledger.balance_projection WHERE projected_at IS NULL"))
	m.Gauge("erp_projection_lag_seconds", "Age of the oldest journal entry the balance projector has not reached.",
		count(cfg.ProjectorRole, "SELECT coalesce(extract(epoch FROM now() - min(enqueued_at)), 0)::float8 FROM ledger.balance_projection WHERE projected_at IS NULL"))
	m.Gauge("erp_payment_proposals_queued", "Payment proposals waiting for the worker.",
		count(cfg.WorkerRole, `SELECT count(*)::float8 FROM platform.operation o JOIN payments.payment_run r ON r.tenant_id = o.tenant_id AND r.operation_id = o.id
			WHERE o.kind = $1 AND r.status = 'proposing' AND o.status IN ('queued', 'cancelled')`, payments.ProposeKind))
	m.Gauge("erp_simulations_queued", "Configuration simulations waiting for the worker.",
		count(cfg.WorkerRole, `SELECT count(*)::float8 FROM platform.operation WHERE kind = $1 AND status = 'queued'`, configuration.SimulateKind))
	m.Gauge("erp_audit_chains_unanchored", "Tenants whose audit chain head moved since its last anchor.",
		count(cfg.WorkerRole, `SELECT count(*)::float8 FROM audit.chain_head h
			WHERE h.seq > coalesce((SELECT max(x.seq) FROM audit.anchor x WHERE x.tenant_id = h.tenant_id), 0)`))
	m.Gauge("erp_audit_chains_tampered", "Tenants whose latest scheduled audit verification found the chain broken.",
		count(cfg.WorkerRole, `SELECT count(*)::float8 FROM (SELECT DISTINCT ON (tenant_id) ok FROM audit.verification ORDER BY tenant_id, verified_at DESC) v WHERE NOT v.ok`))
}

// traced wraps the control plane in the request telemetry of the public
// API (trace, log line, metrics per operation), or stays nil without one.
func traced(control http.Handler, log *slog.Logger, m *telemetry.Metrics, tp *sdktrace.TracerProvider) http.Handler {
	if control == nil {
		return nil
	}
	return telemetry.Middleware(log, m, tp)(control)
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
