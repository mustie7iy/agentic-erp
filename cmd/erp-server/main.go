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
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/db"
	"github.com/mustie7iy/agentic-erp/internal/agents"
	"github.com/mustie7iy/agentic-erp/internal/analytics/extract"
	"github.com/mustie7iy/agentic-erp/internal/analytics/store"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/audit"
	"github.com/mustie7iy/agentic-erp/internal/capture"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/controlplane"
	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/identity"
	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/offboarding"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/awscreds"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/buildinfo"
	"github.com/mustie7iy/agentic-erp/internal/platform/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/egress"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi"
	"github.com/mustie7iy/agentic-erp/internal/platform/natsbus"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/ratelimit"
	"github.com/mustie7iy/agentic-erp/internal/platform/rules"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf"
	"github.com/mustie7iy/agentic-erp/internal/platform/worker"
	"github.com/mustie7iy/agentic-erp/internal/platform/workflow"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/provisioning"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
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

// cli runs the server, or with "provision", "rewrap", "import", "dr" or
// "migrate" as the first argument that command, and returns the process
// exit code: 2 for bad configuration or usage, 1 for a failure while
// running, 0 after a clean shutdown or a completed command.
func cli(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer, started func(api, admin net.Listener)) int {
	if len(args) > 0 && args[0] == "migrate" {
		return migrateCmd(ctx, args[1:], getenv, stderr)
	}
	if len(args) > 0 && args[0] == "dr" {
		return drCmd(ctx, args[1:], getenv, stdout, stderr)
	}
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
		case "register-provider":
			return registerProviderCmd(ctx, args[1:], cfg, stdout, stderr)
		case "disable-connector":
			return disableConnectorCmd(ctx, args[1:], cfg, stdout, stderr)
		case "prices":
			return pricesCmd(ctx, args[1:], cfg, stdout, stderr)
		case "certify":
			return certifyCmd(ctx, args[1:], cfg, stdout, stderr)
		}
		fmt.Fprintf(stderr, "erp-server: unknown command %q (the server takes no arguments; erp-server provision creates a tenant, erp-server rewrap moves tenant keys onto the current master, erp-server import brings an exported tenant in, erp-server register-provider registers a tenant's identity provider, erp-server disable-connector disables a tenant's connector, erp-server migrate applies the pending migrations, erp-server prices load records models' prices, erp-server certify load records an evaluation's certification, erp-server dr verify and reconcile check a restored cell)\n", args[0])
		return 2
	}
	if err := run(ctx, cfg, log, started); err != nil {
		log.Error("erp-server stopped", "error", err)
		return 1
	}
	return 0
}

// run starts the server and serves until ctx ends, a listener fails or a
// critical worker fails. The
// steps run in startup order; what a step opens is closed by run's defers,
// in the reverse order, once the workers stopped.
func run(ctx context.Context, cfg config.Config, log *slog.Logger, started func(api, admin net.Listener)) error {
	tp, err := telemetry.Tracing(ctx, cfg.OTLPEndpoint)
	if err != nil {
		return err
	}
	// With a context that cannot end, Shutdown fails only when an exporter
	// fails to stop, which the OTLP HTTP client never does (the experimental
	// OTEL_GO_X_OBSERVABILITY instruments aside, which are not enabled);
	// spans the last export loses are reported through otel's error handler.
	defer func() { _ = tp.Shutdown(context.WithoutCancel(ctx)) }()
	s := &server{cfg: cfg, log: log, tp: tp, tls: cfg.TLSMaterial, failed: make(chan struct{})}
	closePools, err := s.openPools(ctx)
	if err != nil {
		return err
	}
	defer closePools()
	if err := s.prepare(ctx); err != nil {
		return err
	}
	dir, err := s.openDirectory(ctx)
	if err != nil {
		return err
	}
	if dir != nil {
		defer dir.Close()
	}
	if err := s.controlPlane(ctx); err != nil {
		return err
	}
	s.collaborators()
	engine, err := s.dialEngine(ctx)
	if err != nil {
		return err
	}
	if engine != nil {
		defer engine.Close()
	}
	if err := s.newAPI(); err != nil {
		return err
	}
	if err := s.register(ctx); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	startWorkersHook(s, runCtx)
	bus, err := s.connectBus()
	if err != nil {
		return s.stop(cancel, err)
	}
	if bus != nil {
		defer bus.Close()
	}
	if err := s.startRelay(ctx, runCtx, bus); err != nil {
		return s.stop(cancel, err)
	}
	return s.serve(ctx, cancel, started)
}

// prepare checks the object store, migrates the schema, requires the
// tenants' identity providers and builds the verifier, in that order. The
// object store is checked first: a misconfigured cell is refused before it
// migrates anything.
func (s *server) prepare(ctx context.Context) error {
	if err := s.openObjects(ctx); err != nil {
		return err
	}
	if err := s.prepareSchema(ctx); err != nil {
		return err
	}
	if err := s.requireProviders(ctx); err != nil {
		return err
	}
	if err := s.refuseEBICS(ctx); err != nil {
		return err
	}
	return s.authenticate(ctx)
}

// stop stops the workers and waits for them, so that run's deferred closes
// (the pool, the engine, the directory) never run under a worker still using
// them, and returns err joined with a critical worker's failure, when one
// had one. Once wait returned, every worker's fail happened before, so
// failure is read without a lock.
func (s *server) stop(cancel context.CancelFunc, err error) error {
	cancel()
	s.wait()
	return errors.Join(err, s.failure)
}

// server is what run wires together, one step after the other, before it
// serves.
type server struct {
	cfg  config.Config
	log  *slog.Logger
	tp   *sdktrace.TracerProvider
	pool *database.Pool
	// replica is the cell's read replica, which the analytics extract
	// reads (DATABASE_REPLICA_URL); nil: the primary.
	replica *database.Pool

	static       []authn.Static
	dev          *authn.DevIssuer
	verifier     *authn.Verifier
	beginCheck   database.BeginFunc // the startup check's transaction; nil: the pool's (tests inject failures)
	providers    *authn.Providers   // tenants' identity providers' documents and keys (ADR-0031)
	pages        *pagination.Codec
	keys         *fieldcrypt.Keyring
	store        blobstore.Store
	offboarder   *offboarding.Worker
	sync         *directory.Sync
	control      http.Handler
	extractor    capture.Extractor
	connector    payments.Connector
	metrics      *telemetry.Metrics
	engine       *workflow.Temporal
	engineClient workflow.Client
	tls          *tlsconf.Store  // nil: no TLS material configured (development)
	gateway      *agents.Gateway // nil: no model provider configured
	apiCfg       app.Config
	apiHandler   http.Handler
	simulator    *configuration.SimulationWorker
	registry     *configuration.Replica // records this replica and the configuration keys it knows

	// workers counts the background workers spawn started and wait waits
	// for; running has every one of them by name, true until it stopped.
	workers sync.WaitGroup
	mu      sync.Mutex
	running map[string]bool
	// failed is closed when a critical worker (spawnCritical) fails, which
	// serve watches to stop the server; failure is that first failure,
	// joined into the server's exit by stop.
	failed   chan struct{}
	failOnce sync.Once
	failure  error
}

// openPool opens the database pool with the configured size.
func openPool(ctx context.Context, cfg config.Config) (*database.Pool, error) {
	settings := database.DefaultSettings
	settings.MaxConns = cfg.DBMaxConns
	return database.Open(ctx, cfg.DatabaseURL, settings)
}

// openPools opens the primary's pool and, when the cell extracts, the
// read replica's; the function returned closes both.
func (s *server) openPools(ctx context.Context) (func(), error) {
	var err error
	if s.pool, err = openPool(ctx, s.cfg); err != nil {
		return nil, err
	}
	if s.replica, err = openReplica(ctx, s.cfg, s.cfg.Objects.Enabled()); err != nil {
		s.pool.Close()
		return nil, err
	}
	return func() {
		if s.replica != nil {
			s.replica.Close()
		}
		s.pool.Close()
	}, nil
}

// openReplica opens the read replica's pool when one is configured and
// the cell extracts (an object store configured), sized for the extract
// alone: one tenant's run at a time per replica.
func openReplica(ctx context.Context, cfg config.Config, extracts bool) (*database.Pool, error) {
	if cfg.DatabaseReplicaURL == "" || !extracts {
		return nil, nil
	}
	settings := database.DefaultSettings
	settings.MaxConns = 2
	pool, err := database.Open(ctx, cfg.DatabaseReplicaURL, settings)
	if err != nil {
		return nil, fmt.Errorf("the read replica: %w", err)
	}
	return pool, nil
}

// prepareSchema migrates when configured to, and creates the partitions
// the coming periods need.
func (s *server) prepareSchema(ctx context.Context) error {
	if s.cfg.MigrateOnStart {
		if err := migrate(ctx, s.pool, s.log); err != nil {
			return err
		}
	}
	return partitions(ctx, s.pool, s.log)
}

// requireProviders refuses, in production, to start while an active tenant
// has no active identity provider: its people could not sign in, and
// nothing else would say so (ADR-0031). Provisioning registers one, the
// last one cannot be retired and a production import refuses a tenant
// without one, so a change by hand gets here, or a tenant from before; the
// remedy, erp-server register-provider, needs no running server.
func (s *server) requireProviders(ctx context.Context) error {
	if s.cfg.Environment != config.Production {
		return nil
	}
	tx, err := database.BeginOr(ctx, s.pool, s.beginCheck, database.Scope{Role: s.cfg.ControlRole, ReadOnly: true})
	if err != nil {
		return fmt.Errorf("erp-server: check identity providers: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	tenants, err := tenancy.AllTenants(ctx, tx)
	if err != nil {
		return fmt.Errorf("erp-server: read tenants: %w", err)
	}
	keys, err := identity.ActiveIdentityProviders(ctx, tx)
	if err != nil {
		return err
	}
	with := map[uuid.UUID]bool{}
	for _, k := range keys {
		with[k.TenantID] = true
	}
	var without []string
	for _, t := range tenants {
		if t.Status == "active" && !with[t.ID] {
			without = append(without, t.ID.String())
		}
	}
	if len(without) > 0 {
		return fmt.Errorf("erp-server: ERP_ENV=production: active tenants without an active identity provider, whose people cannot sign in: %s; "+
			"register one with erp-server register-provider -tenant <id> -issuer <url> -audience <aud>, which needs no running server, then start again", strings.Join(without, ", "))
	}
	return nil
}

// refuseEBICS refuses, in production, to start while a tenant has an active
// bank_ebics connector: EBICS is refused there until it has been certified
// with a bank (ADR-0038, T44d), and failing closed keeps a channel the
// API would refuse from going unnoticed. Disabling it is the remedy, by
// erp-server disable-connector, which needs no running server.
func (s *server) refuseEBICS(ctx context.Context) error {
	if s.cfg.Environment != config.Production {
		return nil
	}
	tx, err := database.BeginOr(ctx, s.pool, s.beginCheck, database.Scope{Role: s.cfg.ControlRole, ReadOnly: true})
	if err != nil {
		return fmt.Errorf("erp-server: check EBICS connectors: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	cs, err := integration.ActiveEBICSConnectors(ctx, tx)
	if err != nil {
		return err
	}
	if len(cs) == 0 {
		return nil
	}
	named := make([]string, len(cs))
	for i, c := range cs {
		named[i] = "tenant " + c.TenantID.String() + " connector " + c.ID.String()
	}
	return fmt.Errorf("erp-server: ERP_ENV=production: EBICS is refused until it has been certified with a bank, and these bank_ebics connectors are active: %s; "+
		"disable each with erp-server disable-connector -tenant <id> -connector <id> -by <operator> -reason <text>, which needs no running server, then start again", strings.Join(named, ", "))
}

// authenticate sets up who the API trusts (the configured issuers and the
// development issuer), the page-token codec and the field encryption ring.
func (s *server) authenticate(ctx context.Context) error {
	if s.cfg.DevIssuer {
		s.dev = authn.NewDevIssuer()
		s.static = append(s.static, s.dev.Static())
		s.log.Warn("ERP_DEV_ISSUER is set: the admin listener mints tokens for any subject of any tenant, and operator tokens (POST /dev/token); never in production, where ERP_ENV=production refuses it")
	}
	var err error
	// Tenants' tokens come from the identity providers they registered,
	// resolved by issuer and audience in the cell (ADR-0031), and from the
	// issuers the deployment configures for every tenant.
	registry := &authn.RegistryCache{Registry: identity.Registry{Pool: s.pool, ControlRole: s.cfg.ControlRole}}
	s.providers = &authn.Providers{Production: s.cfg.Environment == config.Production}
	if s.verifier, err = authn.NewRegistered(ctx, authn.Environment(s.cfg.Environment), registry, s.providers, s.cfg.Issuers, s.static...); err != nil {
		return err
	}
	if s.pages, err = pagination.NewCodec(s.cfg.PageTokenKey); err != nil {
		return err
	}
	if s.keys, err = keyring(s.cfg, s.pool); err != nil {
		return err
	}
	return nil
}

// openObjects opens the object store and, where there is one, the worker
// that exports and erases offboarded tenants.
func (s *server) openObjects(ctx context.Context) error {
	s.store = objects(s.cfg)
	if s.store == nil {
		s.log.Warn("no object storage configured (ERP_OBJECT_DIR or ERP_OBJECT_S3_*): documents and bank files are not kept, the audit chain is not anchored, offboarded tenants are not exported (ERP_ENV=production refuses this)")
	}
	// Objects stated as KMS-encrypted are kept only in a bucket whose
	// default encryption is KMS, which covers the presigned uploads too;
	// the store's own writes ask for the same algorithm and key (ADR-0033).
	// The configuration made sure the store is S3.
	if s3, ok := s.store.(blobstore.S3); ok && s.cfg.AtRest.Objects == "sse-kms" {
		enc, err := s3.BucketEncryption(ctx)
		if err != nil {
			return fmt.Errorf("erp-server: object store: %w", err)
		}
		s3.Encryption = enc
		s.store = s3
	}
	// The control plane trusts the operator issuers, and the development
	// issuer's operator tokens; without either it is not served.
	// Offboarded tenants are exported and erased by a worker, where there
	// is a store for the exports; the control plane erases on request.
	if s.store != nil {
		s.offboarder = &offboarding.Worker{Pool: s.pool, Store: s.store, AppRole: s.cfg.AppRole, WorkerRole: s.cfg.WorkerRole, Interval: offboardInterval,
			Grace: s.cfg.OffboardGrace, Retention: s.cfg.Objects.Retention, Log: s.log}
	}
	return nil
}

// openDirectory connects the global directory (ADR-0022), where there is
// one: the cell keeps its tenants' entries current, right after a change
// and in rounds. Without one it returns nil.
func (s *server) openDirectory(ctx context.Context) (*directory.Postgres, error) {
	if s.cfg.DirectoryURL == "" {
		return nil, nil
	}
	dir, err := directory.OpenWith(ctx, s.cfg.DirectoryURL, s.cfg.DirectoryPassword)
	if err != nil {
		return nil, err
	}
	s.sync = &directory.Sync{Pool: s.pool, ControlRole: s.cfg.ControlRole, Directory: dir, Cell: s.cfg.Cell, APIURL: s.cfg.CellAPIURL,
		Region: s.cfg.CellRegion, ControlURL: s.cfg.CellControlURL, Accepting: s.cfg.CellAccepting, Providers: cellIssuers, Interval: directoryInterval, Log: s.log}
	return dir, nil
}

// cellIssuers lists the cell's active identity providers for the global
// directory's claims (ADR-0031): the server joins the identity context and
// the directory, so neither imports the other.
func cellIssuers(ctx context.Context, tx pgx.Tx) ([]directory.Issuer, error) {
	keys, err := identity.ActiveIdentityProviders(ctx, tx)
	if err != nil {
		return nil, err
	}
	out := make([]directory.Issuer, len(keys))
	for i, k := range keys {
		out[i] = directory.Issuer{Issuer: k.Issuer, Audience: k.Audience, TenantID: k.TenantID, ProviderID: k.ID}
		if w := k.Web; w != nil {
			out[i].Web = &directory.WebClient{LoginCode: w.LoginCode, ClientID: w.ClientID, AuthMethod: w.AuthMethod, CredentialRef: w.CredentialRef, Scope: w.Scope}
		}
	}
	return out, nil
}

// claim is how provisioning claims a tenant's provider in the global
// directory, nil without one.
func (s *server) claim() func(ctx context.Context, tenant, provider uuid.UUID, issuer, audience string) error {
	if s.sync == nil {
		return nil
	}
	return s.sync.ClaimIssuer
}

// controlPlane builds the operators' API, served when there is an issuer
// for operator tokens.
func (s *server) controlPlane(ctx context.Context) error {
	if len(s.cfg.OperatorIssuers)+len(s.static) == 0 {
		return nil
	}
	operators, err := authn.New(ctx, authn.Environment(s.cfg.Environment), s.cfg.OperatorIssuers, s.static...)
	if err != nil {
		return err
	}
	service := &controlplane.Service{Pool: s.pool, AppRole: s.cfg.AppRole, ControlRole: s.cfg.ControlRole, Pages: s.pages,
		Keys: s.keys, Provisioner: &provisioning.Provisioner{Pool: s.pool, AppRole: s.cfg.AppRole, Keys: s.keys, Issuers: s.cfg.TenantIssuers(), Configuration: app.Configuration(app.Config{FieldKeys: s.keys}),
			Production: s.cfg.Environment == config.Production, Discover: s.providers.Probe, Claim: s.claim()},
		Offboarding: s.offboarder, Directory: s.sync, Production: s.cfg.Environment == config.Production, Discover: s.providers.Probe}
	s.control = controlplane.Handler(service, operators, s.log)
	s.log.Info("control plane served on the admin listener", "path", controlplane.Base, "operator_issuers", len(s.cfg.OperatorIssuers), "dev_issuer", s.cfg.DevIssuer)
	return nil
}

// modelTimeout bounds one call to the model provider: a long answer takes
// minutes, which no request deadline would allow (api.ModelCallTimeout).
const modelTimeout = api.ModelCallTimeout

// modelClient reaches the model provider: the TLS material's client when
// there is one (ADR-0033), within modelTimeout.
func modelClient(cfg config.Config) *http.Client {
	c := &http.Client{Timeout: modelTimeout}
	if cfg.TLSMaterial != nil {
		c = cfg.TLSMaterial.HTTPClient(modelTimeout)
	}
	// The provider's key travels in a header that a redirect would carry
	// to wherever it points: the provider answers, or the call fails.
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

// collaborators sets up the optional outside services (document extraction,
// the bank connector) and the metrics with the queues' backlogs.
func (s *server) collaborators() {
	s.metrics = newMetrics(s.pool, s.cfg, s.log, s.store != nil) // first: the gateway counts its calls in them
	if s.cfg.Model.Enabled() {
		s.gateway = &agents.Gateway{Provider: agents.Anthropic{Endpoint: s.cfg.Model.Endpoint, Key: s.cfg.Model.APIKey, Client: modelClient(s.cfg)},
			Pool: s.pool, AppRole: s.cfg.AppRole, Log: s.log, Timeout: modelTimeout, AgentBudget: identity.AgentBudget, AgentKind: identity.AgentKind, Metrics: s.metrics,
			Countries: tenancy.Countries}
		// Capture's model calls go through the gateway too, in the
		// request's unit of work (ADR-0036).
		s.extractor = &capture.ClaudeExtractor{LLM: llm.InProcess(s.gateway.Transport(), "erp-extraction")}
	}
	if s.cfg.BankOutboxDir != "" {
		s.connector = payments.DirConnector{Dir: s.cfg.BankOutboxDir}
	}
}

// newMetrics registers the cell's metrics, all of them held to the catalog
// of ADR-0043 by its test; analytics adds the analytical store's gauges,
// for a cell with an object store (ADR-0044).
func newMetrics(pool *database.Pool, cfg config.Config, log *slog.Logger, analytics bool) *telemetry.Metrics {
	m := telemetry.NewMetrics()
	m.Counter(rules.Unevaluable, rules.UnevaluableOpts, rules.UnevaluableLabel)
	backlogs(pool, cfg, m)
	businessGauges(pool, cfg, m)
	if analytics {
		analyticsGauges(pool, cfg, m)
	}
	horizonGauges(pool, cfg, m, time.Now)
	atRest(cfg.AtRest, m, log)
	operationClasses(m)
	return m
}

// operationClasses exports erp_operation_info{operation,class}: each public
// operation's latency class, which the latency SLOs join the request
// histogram on, so a class's objective covers its operations (ADR-0043).
func operationClasses(m *telemetry.Metrics) {
	var rows [][]string
	for id, op := range api.MustOperations() {
		rows = append(rows, []string{id, op.Class})
	}
	m.Info("erp_operation_info", "The latency class of each operation of the public API (ADR-0043).", []string{"operation", "class"}, rows)
}

// atRest states how each store encrypts at rest, in the log and as
// erp_at_rest_info, as ERP_AT_REST says (ADR-0033).
func atRest(a config.AtRest, m *telemetry.Metrics, log *slog.Logger) {
	if a == (config.AtRest{}) {
		log.Warn("ERP_AT_REST not set: how the stores encrypt at rest is not stated (ERP_ENV=production refuses this)")
		return
	}
	log.Info("encryption at rest", "postgres", a.Postgres, "objects", a.Objects, "nats", a.NATS)
	var rows [][]string
	for _, r := range [][]string{{"postgres", a.Postgres}, {"objects", a.Objects}, {"nats", a.NATS}} {
		if r[1] != "" {
			rows = append(rows, r)
		}
	}
	m.Info("erp_at_rest_info", "How each store encrypts its data at rest, as ERP_AT_REST states it (ADR-0033).", []string{"store", "encryption"}, rows)
}

// dialEngine connects the workflow engine (ADR-0012): with
// ERP_TEMPORAL_ADDRESS the server hosts a worker and dispatches queued
// operations to it; without, the in-process workers of the transition keep
// running them, and it returns nil.
func (s *server) dialEngine(ctx context.Context) (*workflow.Temporal, error) {
	if s.cfg.Temporal.Address == "" {
		return nil, nil
	}
	var codec *workflow.Codec
	if s.cfg.Temporal.CodecKey != nil {
		codec, _ = workflow.NewCodec(s.cfg.Temporal.CodecKey) // the configuration checked the length
	} else {
		s.log.Warn("ERP_TEMPORAL_CODEC_KEY not set: workflow payloads reach the engine unsealed (ERP_ENV=production refuses this)")
	}
	wc := workflow.Config{Address: s.cfg.Temporal.Address, Namespace: s.cfg.Temporal.Namespace, Codec: codec}
	if s.tls != nil {
		wc.TLS = func() (*tls.Config, error) { return s.tls.ClientConfig(tlsconf.Internal, s.cfg.Temporal.ServerName) }
	}
	engine, err := workflow.Dial(ctx, wc)
	if err != nil {
		return nil, err
	}
	s.engine, s.engineClient = engine, engine
	s.log.Info("workflow engine connected", "address", s.cfg.Temporal.Address, "namespace", s.cfg.Temporal.Namespace, "task_queue", s.cfg.Temporal.TaskQueue)
	return engine, nil
}

// analyticsTimeout bounds a call to erp-analytics; past it the query is
// answered from the database.
const analyticsTimeout = 30 * time.Second

// newAPI builds the public API's handler.
func (s *server) newAPI() error {
	cfg := s.cfg
	s.apiCfg = app.Config{
		Pool: s.pool, AppRole: cfg.AppRole, Verifier: s.verifier, Pages: s.pages, FieldKeys: s.keys, Extractor: s.extractor, Connector: s.connector, ExtractDir: cfg.ExtractDir, Objects: s.store, Retention: cfg.Objects.Retention, Secrets: cfg.Secrets, Log: s.log,
		UploadKey: uploadKey(cfg.PageTokenKey), PublicURL: cfg.PublicURL, Workflows: s.engineClient,
		Limits: ratelimit.New(cfg.TenantQuota, cfg.PrincipalQuota), RequestTimeout: cfg.RequestTimeout,
		Metrics: s.metrics, Tracer: s.tp, Meter: otel.GetMeterProvider(), Now: time.Now,
		RequireBoundAgents: cfg.Environment == config.Production, EvalCell: cfg.EvalCell,
		RefuseEBICS: cfg.Environment == config.Production, RefuseUnsignedPacks: cfg.Environment == config.Production,
	}
	if s.gateway != nil {
		s.apiCfg.Model = s.gateway
	}
	if cfg.AnalyticsURL != "" {
		// erp-analytics over mTLS as its SPIFFE identity (ADR-0044);
		// config.Load requires the trust domain, and so the TLS material.
		s.apiCfg.Analytics = &store.Client{URL: cfg.AnalyticsURL, Peer: tlsconf.ID(cfg.TrustDomain, cfg.Cell, "erp-analytics"),
			HTTP: s.tls.InternalClient(analyticsTimeout)}
	}
	var err error
	if s.apiHandler, err = app.New(s.apiCfg); err != nil {
		return err
	}
	return nil
}

// register records the replica, its version and the configuration keys it
// knows, before it serves: an activation setting a key this version does
// not know is refused while it runs (ADR-0038 rollout, 06 §6.4). A replica
// that cannot record itself does not start.
func (s *server) register(ctx context.Context) error {
	s.registry = &configuration.Replica{Pool: s.pool, WorkerRole: s.cfg.WorkerRole, Build: buildinfo.Get(), Log: s.log, Begin: s.beginCheck}
	if err := s.registry.Register(ctx); err != nil {
		return fmt.Errorf("erp-server: %w", err)
	}
	return nil
}

// startWorkersHook is what run calls to start the workers; tests wrap it to see
// the server whose workers they check.
var startWorkersHook = (*server).startWorkers

// startWorkers starts the background workers on runCtx, which run cancels
// at shutdown.
func (s *server) startWorkers(runCtx context.Context) {
	// Critical: a replica no longer recorded would stop counting while it
	// serves, and a configuration it cannot read could be activated.
	s.spawnCritical("replica record", func() error { return s.registry.Run(runCtx) })
	s.startProjections(runCtx)
	s.startOperations(runCtx)
	s.startMaintenance(runCtx)
	s.startAudit(runCtx)
	s.startWebhooks(runCtx)
	s.startAnalytics(runCtx)
}

// startWebhooks starts the webhook dispatcher (ADR-0041): it delivers what
// the fan-out recorded, so it runs with or without a bus, and prunes
// deliveries past their retention.
func (s *server) startWebhooks(runCtx context.Context) {
	// New fails only for egress.LoopbackForTests outside a test binary; with
	// no option it always makes a client.
	client, _ := egress.New()
	dispatcher := &integration.Dispatcher{Pool: s.pool, AppRole: s.cfg.AppRole, WorkerRole: s.cfg.WorkerRole, Keys: s.keys, Client: client, Log: s.log}
	s.spawn("webhook dispatcher", func() { dispatcher.Run(runCtx) })
}

// startAnalytics starts, with an object store, the analytical store's
// extract scheduler (ADR-0044): the extract reads on the read replica once
// it has replayed past the change, on the primary until then and without
// a replica; it collects superseded files, rebuilds a store the database
// is behind, and sweeps stale tenants hourly.
func (s *server) startAnalytics(runCtx context.Context) {
	if s.store == nil {
		return
	}
	replica := s.pool.Begin
	if s.replica != nil {
		replica = s.replica.Begin
	}
	scheduler := &extract.Scheduler{Extractor: extract.Extractor{Replica: replica, Primary: s.pool.Begin, Role: s.cfg.AppRole, WorkerRole: s.cfg.WorkerRole, Store: s.store},
		Begin: s.pool.Begin, WorkerRole: s.cfg.WorkerRole, Log: s.log}
	s.spawn("analytics extract", func() { scheduler.Run(runCtx) })
}

// startProjections starts the balance projector and, without a workflow
// engine, the configuration simulator.
func (s *server) startProjections(runCtx context.Context) {
	projector := &ledger.Projector{Pool: s.pool, Role: s.cfg.ProjectorRole, Log: s.log}
	s.spawn("projector", func() { projector.Run(runCtx) })
	s.simulator = &configuration.SimulationWorker{Dependencies: app.Configuration(s.apiCfg), Pool: s.pool, AppRole: s.cfg.AppRole, WorkerRole: s.cfg.WorkerRole, Log: s.log}
	if s.engine == nil { // with an engine, simulations run as workflows
		s.spawn("simulator", func() { s.simulator.Run(runCtx) })
	}
}

// startOperations starts what runs queued operations: the bank file
// transmitter, and the in-process payment workers or the workflow worker
// and the dispatcher that hands it the queued operations.
func (s *server) startOperations(runCtx context.Context) {
	cfg, pool, log, apiCfg, engine := s.cfg, s.pool, s.log, s.apiCfg, s.engine
	// Bank files are sent by a worker of every replica, with or without the
	// workflow engine: the outbox of their transmission (T44e, ADR-0038).
	transmitter := &payments.Transmitter{Endpoints: app.Payments(apiCfg), Pool: pool, AppRole: cfg.AppRole, WorkerRole: cfg.WorkerRole, Log: log}
	s.spawn("bank file transmitter", func() { transmitter.Run(runCtx) })
	if engine == nil {
		proposer := &payments.Proposer{Endpoints: app.Payments(apiCfg), Pool: pool, AppRole: cfg.AppRole, WorkerRole: cfg.WorkerRole, Log: log}
		s.spawn("proposer", func() { proposer.Run(runCtx) })
		releaser := &workflows.Releaser{Payments: &workflows.Payments{Pool: pool, AppRole: cfg.AppRole, Endpoints: app.Payments(apiCfg)}, Pool: pool, WorkerRole: cfg.WorkerRole, Log: log}
		s.spawn("releaser", func() { releaser.Run(runCtx) })
		return
	}
	activities := &workflows.Payments{Pool: pool, AppRole: cfg.AppRole, Endpoints: app.Payments(apiCfg)}
	// Without its worker a replica's dispatcher would hand workflows to the
	// engine that this replica never runs, so the worker is critical.
	s.spawnCritical("workflow worker", func() error {
		return engine.Worker(runCtx, cfg.Temporal.TaskQueue, func(r workflow.Registry) {
			workflows.Register(r, activities, &workflows.Simulations{Worker: s.simulator}, &workflows.Migrations{Pool: pool, AppRole: cfg.AppRole, Endpoints: app.Integration(apiCfg), Actors: identity.Actors{}},
				&workflows.Approvals{Pool: pool, AppRole: cfg.AppRole}, &workflows.Statements{Pool: pool, AppRole: cfg.AppRole, Endpoints: app.Payments(apiCfg)})
		})
	})
	dispatcher := &workflows.Dispatcher{Deadline: cfg.Temporal.EscalateAfter, Escalations: cfg.Temporal.Escalations, Pool: pool, WorkerRole: cfg.WorkerRole, Client: engine, Queue: cfg.Temporal.TaskQueue, Interval: dispatchInterval, Statements: cfg.Temporal.StatementFetch, Log: log}
	s.spawn("dispatcher", func() { dispatcher.Run(runCtx) })
}

// startMaintenance starts the partition maintenance, with a global
// directory the directory sync, and with TLS material its re-reading.
func (s *server) startMaintenance(runCtx context.Context) {
	pool, log := s.pool, s.log
	s.spawn("maintenance", func() {
		maintain(runCtx, maintenanceInterval, func(ctx context.Context) error { return partitions(ctx, pool, log) }, log)
	})
	if s.sync != nil {
		s.spawn("directory sync", func() { s.sync.Run(runCtx) })
	}
	if s.tls != nil {
		s.spawn("tls", func() { s.tls.Run(runCtx, tlsconf.RefreshInterval, s.log) })
	}
}

// startAudit starts the workers that keep data within its policies: the
// audit anchorer and offboarding (with an object store), key rotation,
// contract expiry and audit chain verification.
func (s *server) startAudit(runCtx context.Context) {
	cfg, pool, log := s.cfg, s.pool, s.log
	if s.store != nil {
		anchorer := &audit.Anchorer{Pool: pool, Store: s.store, AppRole: cfg.AppRole, WorkerRole: cfg.WorkerRole, Interval: anchorInterval, Retention: cfg.Objects.Retention, Log: log}
		s.spawn("anchorer", func() { anchorer.Run(runCtx) })
		s.spawn("offboarder", func() { s.offboarder.Run(runCtx) })
	}
	// Keys older than the policy's age are rotated by the server (ADR-0028).
	if cfg.KeyRotationAge != 0 {
		rotator := &controlplane.KeyRotator{Pool: pool, Keys: s.keys, AppRole: cfg.AppRole, WorkerRole: cfg.WorkerRole, Age: cfg.KeyRotationAge, Interval: rotationInterval, Log: log}
		s.spawn("rotator", func() { rotator.Run(runCtx) })
	}
	// Active contracts past their validity are expired in rounds, and those
	// ending soon alerted (the contract's ContractExpired and
	// ContractExpiring are the job's), each on its company's date (T72n6).
	expirer := &procurement.ContractExpirer{Pool: pool, AppRole: cfg.AppRole, WorkerRole: cfg.WorkerRole, Interval: expiryInterval, Log: log, LegalEntities: tenancy.Directory{},
		Partners: partner.Payees{}, Settings: configuration.Settings{}}
	s.spawn("expirer", func() { expirer.Run(runCtx) })
	// People's sessions ended or idle for a week are deleted in rounds
	// (ADR-0031).
	sweeper := &identity.SessionSweeper{Pool: pool, AppRole: cfg.AppRole, WorkerRole: cfg.WorkerRole, Interval: sweepInterval, Log: log}
	s.spawn("session sweeper", func() { sweeper.Run(runCtx) })
	// Every tenant's audit chain is verified against its anchors in rounds
	// (ADR-0011); a chain that stops verifying is an alarm.
	if cfg.AuditVerify == 0 {
		log.Warn("ERP_AUDIT_VERIFY_INTERVAL is 0: audit chains are verified on request only (ERP_ENV=production refuses this)")
	} else {
		verifier := &audit.Verifier{Pool: pool, Store: s.store, AppRole: cfg.AppRole, WorkerRole: cfg.WorkerRole, Interval: cfg.AuditVerify, Log: log}
		s.spawn("verifier", func() { verifier.Run(runCtx) })
	}
}

// connectBus connects NATS for the outbox relay, or returns nil when none
// is configured: events then stay in the outbox.
func (s *server) connectBus() (*natsbus.Bus, error) {
	if s.cfg.NATSURL == "" {
		s.log.Warn("ERP_NATS_URL not set: events stay in the outbox until a relay runs")
		return nil, nil
	}
	if s.tls == nil {
		return natsbus.Connect(s.cfg.NATSURL)
	}
	return natsbus.Connect(s.cfg.NATSURL, natsbus.TLS(s.tls)...)
}

// startRelay makes sure the cell's streams exist and starts the outbox
// relay on bus, when there is one.
func (s *server) startRelay(ctx, runCtx context.Context, bus *natsbus.Bus) error {
	if bus == nil {
		return nil
	}
	if err := bus.EnsureStreams(ctx, s.cfg.Cell); err != nil {
		return err
	}
	relay := &outbox.Relay{Pool: s.pool, Role: s.cfg.RelayRole, Publisher: bus, Cell: s.cfg.Cell, Log: s.log, Counter: s.metrics}
	if s.store != nil {
		// A published batch marks its tenants due an extract, once per
		// batch and never in the posting transactions (ADR-0044).
		relay.Relayed = extract.MarkChanged
	}
	s.spawn("relay", func() { relay.Run(runCtx) })
	// Events offered to webhooks become deliveries (ADR-0041), each in its
	// tenant's transaction, so the fan-out needs no role beyond the app's.
	fanout := &integration.Fanout{Pool: s.pool, AppRole: s.cfg.AppRole, Log: s.log}
	consumer, err := bus.Consume(ctx, s.cfg.Cell, integration.FanoutConsumer, integration.WebhookContexts(), fanout.Handle)
	if err != nil {
		return err
	}
	consumer.Log = s.log
	s.spawn("webhook fan-out", func() { consumer.Run(runCtx) })
	return nil
}

// serve opens the listeners and serves until ctx ends, a listener fails or
// a critical worker fails, then shuts the servers down, stops the workers (cancel) and waits for
// them.
func (s *server) serve(ctx context.Context, cancel context.CancelFunc, started func(api, admin net.Listener)) error {
	apiLn, adminLn, err := listen(s.cfg)
	if err != nil {
		return s.stop(cancel, err)
	}
	apiLn, adminLn = s.secure(apiLn, adminLn)
	api, control := s.peers(s.apiHandler, traced(s.control, s.log, s.metrics, s.tp))
	servers := []*http.Server{
		{Handler: api, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second},
		{Handler: httpapi.Admin(ready(s.pool), s.metrics, devHandler(s.dev), control), ReadHeaderTimeout: 5 * time.Second},
	}
	errc := make(chan error, 2)
	for i, ln := range []net.Listener{apiLn, adminLn} {
		go func() { errc <- servers[i].Serve(ln) }()
	}
	build := buildinfo.Get()
	s.log.Info("erp-server started", "api", apiLn.Addr().String(), "admin", adminLn.Addr().String(), "env", s.cfg.Environment, "cell", s.cfg.Cell, "directory", s.cfg.DirectoryURL != "",
		"version", build.Version, "revision", build.Revision)
	if started != nil {
		started(apiLn, adminLn)
	}

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	case <-s.failed: // stop joins the worker's failure into the exit
		s.log.Error("a critical worker failed; the server stops")
	}
	shutdownCtx, done := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ShutdownTimeout)
	defer done()
	for _, srv := range servers {
		// In-flight requests finish or time out; a server that did not stop
		// in time is abandoned with the process, so its error changes nothing.
		_ = srv.Shutdown(shutdownCtx)
	}
	err = s.stop(cancel, serveErr) // nil after a requested shutdown; set when a listener or a critical worker failed
	s.log.Info("erp-server stopped")
	return err
}

// secure serves both listeners over TLS 1.3 as the cell's certificate,
// when it has one (ADR-0033). With a trust domain the API asks every
// client for a certificate, the router being its one caller; the admin
// listener verifies one when given, so probes connect without and
// /control is left to the global plane's.
func (s *server) secure(apiLn, adminLn net.Listener) (net.Listener, net.Listener) {
	if s.tls == nil || !s.tls.HasCertificate() {
		return apiLn, adminLn
	}
	apiAuth := tls.VerifyClientCertIfGiven
	if s.cfg.TrustDomain != "" {
		apiAuth = tls.RequireAndVerifyClientCert
	}
	return tls.NewListener(apiLn, s.tls.ServerConfig(tlsconf.Internal, apiAuth)),
		tls.NewListener(adminLn, s.tls.ServerConfig(tlsconf.Internal, tls.VerifyClientCertIfGiven))
}

// peers restricts, with a trust domain, the API and the control plane to
// the router's identity, which is also the global plane's: the router
// forwards every public request, with the thumbprint of the client's
// certificate and the client's address, which the cell then trusts, the
// global plane calls /control, and the operator's token is still required
// there (ADR-0033). Without a trust domain neither header is trusted: the
// address is the connection's.
func (s *server) peers(api, control http.Handler) (http.Handler, http.Handler) {
	if s.cfg.TrustDomain == "" {
		return httpapi.ClientCertificate(false, httpapi.ClientAddress(false, api)), control
	}
	router := tlsconf.ID(s.cfg.TrustDomain, "global", "erp-router")
	if control != nil {
		control = httpapi.RequirePeer(router, s.log, control)
	}
	return httpapi.RequirePeer(router, s.log, httpapi.ClientCertificate(true, httpapi.ClientAddress(true, api))), control
}

// listen opens the API and admin listeners; when the second fails the
// first is closed again. The listen itself takes no context (it is what
// net.Listen does): a shutdown is noticed once serving.
func listen(cfg config.Config) (apiLn, adminLn net.Listener, err error) {
	var lc net.ListenConfig
	apiLn, err = lc.Listen(context.Background(), "tcp", cfg.HTTPAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("erp-server: listen on %s: %w", cfg.HTTPAddr, err)
	}
	adminLn, err = lc.Listen(context.Background(), "tcp", cfg.AdminAddr)
	if err != nil {
		_ = apiLn.Close() // the admin listener's failure is the one reported
		return nil, nil, fmt.Errorf("erp-server: admin listen on %s: %w", cfg.AdminAddr, err)
	}
	return apiLn, adminLn, nil
}

// spawn runs the worker fn in the background, counted for wait: no worker
// started this way can be left out of a shutdown. A panic in fn is logged
// with its stack and ends that worker only (worker.Recover).
func (s *server) spawn(name string, fn func()) {
	s.start(name, false, func() error { fn(); return nil })
}

// spawnCritical runs fn as a worker the replica cannot serve without: fn
// returns nil once runCtx ends, and an error when it could not run, which
// fails the server, as a panic in it does. The other workers log a failed
// round and try again on the next one, so a transient database error does
// not stop a replica; their Run returns nothing to fail with.
func (s *server) spawnCritical(name string, fn func() error) {
	s.start(name, true, fn)
}

// start is spawn and spawnCritical: it records the worker by name, runs fn
// with its panics contained, and fails the server when a critical worker
// returns an error or panics.
func (s *server) start(name string, critical bool, fn func() error) {
	s.mu.Lock()
	if s.running == nil {
		s.running = map[string]bool{}
	}
	s.running[name] = true
	s.mu.Unlock()
	s.workers.Go(func() {
		err := worker.Recover(fn)
		var p *worker.Panic
		if errors.As(err, &p) {
			// A worker outlives every request, so its panic is logged without one.
			worker.Report(context.Background(), s.log, "worker panicked", err, "worker", name)
		}
		if err != nil && critical {
			s.fail(fmt.Errorf("erp-server: %s: %w", name, err))
		}
		s.mu.Lock()
		s.running[name] = false
		s.mu.Unlock()
	})
}

// fail records the first critical worker's failure and closes failed.
func (s *server) fail(err error) {
	s.failOnce.Do(func() {
		s.failure = err
		close(s.failed)
	})
}

// wait blocks until every worker spawn started stopped.
func (s *server) wait() { s.workers.Wait() }

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
	sweepInterval       = time.Hour
	dispatchInterval    = time.Second
)

// keyring builds the field encryption ring: the KMS master first when there
// is one, then the configured local masters, the legacy key when there is
// one, and the pool for lookups outside requests.
func keyring(cfg config.Config, pool *database.Pool) (*fieldcrypt.Keyring, error) {
	ring := &fieldcrypt.Keyring{Pool: pool, AppRole: cfg.AppRole}
	if cfg.KMS.Enabled() {
		client := cfg.HTTPClient()
		ring.Masters = append(ring.Masters, fieldcrypt.KMS{Name: cfg.KMS.Name, KeyID: cfg.KMS.Key, Endpoint: cfg.KMS.Endpoint, Region: cfg.KMS.Region,
			Credentials: credentials(cfg.AWS, cfg.KMS.AccessKey, cfg.KMS.SecretKey, client), Client: client})
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
// else the deployment's ambient ones (ADR-0024), cached; STS is reached
// with client (nil: its default).
func credentials(aws config.AWS, accessKey, secretKey string, client *http.Client) awscreds.Provider {
	return awscreds.Default(awscreds.Static{AccessKey: accessKey, SecretKey: secretKey}, aws.Getenv, aws.STSEndpoint, aws.IMDSEndpoint, client)
}

// objects opens the configured object store, or nil without one; an S3
// service is reached with the configuration's TLS client (ADR-0033).
func objects(cfg config.Config) blobstore.Store {
	o, aws := cfg.Objects, cfg.AWS
	switch {
	case o.Dir != "":
		return blobstore.Dir{Root: o.Dir}
	case o.Endpoint != "":
		client := cfg.HTTPClient()
		return blobstore.S3{Endpoint: o.Endpoint, Bucket: o.Bucket, Region: o.Region, Credentials: credentials(aws, o.AccessKey, o.SecretKey, client), Client: client}
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
// horizon that got too short meanwhile). A panic in job fails that round
// only (worker.Recover): ending the loop would stop the maintenance on this
// replica for good with nothing but a log line.
func maintain(ctx context.Context, interval time.Duration, job func(context.Context) error, log *slog.Logger) {
	for worker.Wait(ctx, interval) {
		err := worker.Recover(func() error { return job(ctx) })
		if worker.Failed(ctx, err) {
			worker.Report(ctx, log, "partition maintenance failed", err)
		}
	}
}

// analyticsGauges expose the analytical store's freshness (ADR-0044),
// which its alert watches, in a cell that extracts (an object store
// configured): the age of the oldest journal entry of an active tenant not
// yet in its store (the target is p95 of fifteen minutes from posting),
// one index range per tenant above its watermark, or for a tenant not yet
// extracted, how long its first change has waited; and the tenants whose
// store is not found current within the target, answered from the
// database.
func analyticsGauges(pool *database.Pool, cfg config.Config, m *telemetry.Metrics) {
	count := reader(pool)
	m.Gauge("erp_analytics_lag_seconds", "Age of the oldest change of an active tenant not yet in its analytical store.",
		count(cfg.WorkerRole, `SELECT coalesce(extract(epoch FROM now() - least(
			(SELECT min(e.posted_at) FROM analytics.snapshot s JOIN tenancy.tenant t ON t.id = s.tenant_id AND t.status = 'active'
				JOIN ledger.journal_entry e ON e.tenant_id = s.tenant_id AND e.posted_xid >= s.watermark),
			(SELECT min(d.changed_at) FROM analytics.extract_due d JOIN tenancy.tenant t ON t.id = d.tenant_id AND t.status = 'active'
				WHERE d.changed_at > coalesce(d.done_at, '-infinity') AND NOT EXISTS (SELECT FROM analytics.snapshot s WHERE s.tenant_id = d.tenant_id)))), 0)::float8`))
	m.Gauge("erp_analytics_stale_tenants", "Active tenants whose analytical store was not found current within 15 minutes.",
		count(cfg.WorkerRole, `SELECT count(*)::float8 FROM tenancy.tenant t WHERE t.status = 'active'
			AND NOT EXISTS (SELECT FROM analytics.snapshot s WHERE s.tenant_id = t.id AND s.checked_at >= now() - interval '15 minutes')`))
}

// backlogs exposes the queues the workers drain (05-scalability §5.7:
// projector lag is a monitored metric), read on each scrape as the role
// that drains them. The counts read several contexts' tables directly: an
// accepted exception to reading through the owning context (06 §6.7),
// since they are the operators' view of the cell, read-only, and change
// with the queues they watch.
func backlogs(pool *database.Pool, cfg config.Config, m *telemetry.Metrics) {
	count := reader(pool)
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
	m.Gauge("erp_bank_statement_files_refused", "Statement files fetched from bank channels that the import refused, left for a person (ADR-0047).",
		count(cfg.WorkerRole, `SELECT count(*)::float8 FROM payments.statement_fetch WHERE status = 'refused'`))
	m.Gauge("erp_bank_statement_files_unarchived", "Statement files imported from bank channels for over an hour and not yet moved to the archive directory.",
		count(cfg.WorkerRole, `SELECT count(*)::float8 FROM payments.statement_fetch WHERE status = 'imported' AND updated_at < now() - interval '1 hour'`))
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
