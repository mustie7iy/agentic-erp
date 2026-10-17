package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"go.temporal.io/server/common/authorization"
	"go.temporal.io/server/common/config"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/rpc/encryption"
	"go.temporal.io/server/temporal"

	"github.com/mustie7iy/agentic-erp/temporal/spiffeauth"
)

// settings are serve's: the engine's configuration (Temporal's YAML, which
// the erp-cell chart writes) and what the platform adds to it.
type settings struct {
	configFile string        // ERP_TEMPORAL_CONFIG
	server     string        // the cell's server's identity, from ERP_TRUST_DOMAIN and ERP_CELL
	nodes      string        // the engine's own identity
	namespace  string        // ERP_TEMPORAL_NAMESPACE (default erp)
	retention  time.Duration // ERP_TEMPORAL_RETENTION (default 720h)
	password   string        // ERP_TEMPORAL_DATABASE_PASSWORD, a value or a file: reference
	address    string        // POD_IP: the address the other nodes reach this one at
}

func loadSettings(getenv func(string) string) (settings, error) {
	var errs []error
	need := func(name string) string {
		v := getenv(name)
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		}
		return v
	}
	s := settings{configFile: need("ERP_TEMPORAL_CONFIG"), namespace: getenv("ERP_TEMPORAL_NAMESPACE"), address: getenv("POD_IP")}
	domain, cell := need("ERP_TRUST_DOMAIN"), need("ERP_CELL")
	s.server, s.nodes = spiffeauth.ID(domain, cell, "erp-server"), spiffeauth.ID(domain, cell, "erp-temporal")
	if s.namespace == "" {
		s.namespace = "erp"
	}
	s.retention = 720 * time.Hour
	if raw := getenv("ERP_TEMPORAL_RETENTION"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < 24*time.Hour {
			errs = append(errs, errors.New("ERP_TEMPORAL_RETENTION must be a duration of a day or more, such as 720h"))
		}
		s.retention = d
	}
	pw, err := secret(need("ERP_TEMPORAL_DATABASE_PASSWORD"))
	if err != nil {
		errs = append(errs, fmt.Errorf("ERP_TEMPORAL_DATABASE_PASSWORD: %w", err))
	}
	s.password = pw
	return s, errors.Join(errs...)
}

// engineConfig reads Temporal's configuration and completes it: the
// databases' password (kept out of the file), this node's address, and
// the refusal of any setting that would let a client in without a
// certificate (ADR-0033).
func engineConfig(s settings) (*config.Config, error) {
	cfg, err := config.Load(config.WithConfigFile(s.configFile))
	if err != nil {
		return nil, fmt.Errorf("configuration %s: %w", s.configFile, err)
	}
	for name, store := range cfg.Persistence.DataStores {
		if store.SQL != nil {
			store.SQL.Password = s.password
			cfg.Persistence.DataStores[name] = store
		}
	}
	if s.address != "" {
		cfg.Global.Membership.BroadcastAddress = s.address
	}
	tls := cfg.Global.TLS
	switch {
	case !tls.Frontend.Server.RequireClientAuth || len(tls.Frontend.Server.ClientCAFiles) == 0:
		return nil, errors.New("global.tls.frontend.server must require client certificates of the environment's CA")
	case !tls.Internode.Server.RequireClientAuth || len(tls.Internode.Server.ClientCAFiles) == 0:
		return nil, errors.New("global.tls.internode.server must require client certificates of the environment's CA")
	case tls.SystemWorker.CertFile == "":
		return nil, errors.New("global.tls.systemWorker must present the engine's certificate")
	}
	return cfg, nil
}

// newServer is Temporal's server with the platform's authorization and TLS
// 1.3; replaced by tests.
var newServer = func(cfg *config.Config, s settings, logger log.Logger) (temporal.Server, error) {
	provider, err := encryption.NewTLSConfigProviderFromConfig(cfg.Global.TLS, metrics.NoopMetricsHandler, logger, nil)
	if err != nil {
		return nil, fmt.Errorf("tls: %w", err)
	}
	mapper := spiffeauth.Mapper{Server: s.server, Nodes: s.nodes, Namespace: s.namespace}
	return temporal.NewServer(
		temporal.WithConfig(cfg),
		temporal.ForServices(temporal.DefaultServices),
		temporal.WithLogger(logger),
		temporal.WithTLSConfigFactory(spiffeauth.TLS13{TLSConfigProvider: provider}),
		temporal.WithAuthorizer(spiffeauth.Authorizer()),
		temporal.WithClaimMapper(func(*config.Config) authorization.ClaimMapper { return mapper }),
	)
}

func serveCmd(ctx context.Context, getenv func(string) string, stderr io.Writer) int {
	s, err := loadSettings(getenv)
	if err != nil {
		fmt.Fprintln(stderr, "erp-temporal: settings:", err)
		return 2
	}
	cfg, err := engineConfig(s)
	if err != nil {
		fmt.Fprintln(stderr, "erp-temporal:", err)
		return 2
	}
	if err := serve(ctx, cfg, s, log.NewZapLogger(log.BuildZapLogger(cfg.Log))); err != nil {
		fmt.Fprintln(stderr, "erp-temporal:", err)
		return 1
	}
	return 0
}

// serve starts the engine, registers the cell's namespace when it is
// missing, and runs until ctx ends.
func serve(ctx context.Context, cfg *config.Config, s settings, logger log.Logger) error {
	srv, err := newServer(cfg, s, logger)
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}
	if err := srv.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	if err := ensureNamespace(ctx, cfg, s); err != nil {
		// The server is stopped either way; the namespace error is the one
		// that explains the failure.
		_ = srv.Stop()
		return err
	}
	logger.Info("erp-temporal ready")
	<-ctx.Done()
	if err := srv.Stop(); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	return nil
}

// secret is a setting's value, or for a file: reference (ADR-0017) the
// file's content without its final line break, as the platform reads its
// own (internal/platform/secrets).
func secret(value string) (string, error) {
	path, ok := strings.CutPrefix(value, "file:")
	if !ok {
		return value, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

// checkCmd loads ERP_TEMPORAL_CONFIG as serve does and reports what serve
// would refuse: Temporal's own validation and the platform's TLS rules.
func checkCmd(getenv func(string) string, stderr io.Writer) int {
	file := getenv("ERP_TEMPORAL_CONFIG")
	if file == "" {
		fmt.Fprintln(stderr, "erp-temporal check-config: ERP_TEMPORAL_CONFIG is required")
		return 2
	}
	if _, err := engineConfig(settings{configFile: file}); err != nil {
		fmt.Fprintln(stderr, "erp-temporal check-config:", err)
		return 1
	}
	fmt.Fprintln(stderr, "erp-temporal check-config: "+file+" is a configuration serve accepts")
	return 0
}
