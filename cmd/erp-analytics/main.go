// Command erp-analytics is the analytical store's reader (ADR-0044): it
// answers erp-server's semantic query plans from a tenant's Parquet files
// in DuckDB. It is the one binary built with cgo (ADR-0032, amended), has
// read-only access to the cell bucket's analytics/ prefix and no database
// credentials, and serves erp-server alone, over mTLS as
// spiffe://<domain>/<cell>/erp-analytics (ADR-0033).
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/analytics/reader"
	"github.com/mustie7iy/agentic-erp/internal/platform/awscreds"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/buildinfo"
	"github.com/mustie7iy/agentic-erp/internal/platform/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf"
)

var exit = os.Exit

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli(ctx, os.Getenv, os.Stderr, nil)
	stop()
	exit(code)
}

// settings is the configuration, all from the environment.
type settings struct {
	addr    string // ERP_ANALYTICS_ADDR (default :8443)
	cache   string // ERP_ANALYTICS_CACHE: verified files (default <tmp>/erp-analytics)
	caller  string // erp-server's SPIFFE ID in ERP_TRUST_DOMAIN and ERP_CELL
	tls     *tlsconf.Store
	store   blobstore.Store
	level   slog.Level
	timeout time.Duration
	limits  reader.Limits // ERP_ANALYTICS_THREADS, ERP_ANALYTICS_MEMORY_LIMIT, ERP_ANALYTICS_MAX_QUERIES
}

var cellPattern = regexp.MustCompile(`^[a-z0-9]{1,20}$`)

// load reads the settings. Every security setting is required: the reader
// serves over mTLS or not at all.
func load(ctx context.Context, getenv func(string) string) (settings, error) {
	s := settings{addr: getenv("ERP_ANALYTICS_ADDR"), cache: getenv("ERP_ANALYTICS_CACHE"), timeout: 30 * time.Second}
	if s.addr == "" {
		s.addr = ":8443"
	}
	if s.cache == "" {
		s.cache = filepath.Join(os.TempDir(), "erp-analytics")
	}
	if err := s.level.UnmarshalText([]byte(defaulted(getenv("ERP_LOG_LEVEL"), "info"))); err != nil {
		return s, fmt.Errorf("ERP_LOG_LEVEL: %w", err)
	}
	var err error
	if s.limits, err = limits(getenv); err != nil {
		return s, err
	}
	domain, cell := getenv("ERP_TRUST_DOMAIN"), getenv("ERP_CELL")
	if domain == "" || !config.ValidTrustDomain(domain) || !cellPattern.MatchString(cell) {
		return s, fmt.Errorf("ERP_TRUST_DOMAIN %q and ERP_CELL %q must name the cell: erp-analytics admits its erp-server alone (ADR-0033)", domain, cell)
	}
	s.caller = tlsconf.ID(domain, cell, "erp-server")
	local := secrets.Local{Getenv: getenv}
	s.tls = &tlsconf.Store{Source: tlsconf.Source{CA: getenv("ERP_TLS_CA"), Cert: getenv("ERP_TLS_CERT"), Key: getenv("ERP_TLS_KEY")},
		Resolver: secrets.Multi{secrets.SchemeEnv: local, secrets.SchemeFile: local}}
	if s.tls.Source.CA == "" || s.tls.Source.Cert == "" || s.tls.Source.Key == "" {
		return s, errors.New("ERP_TLS_CA, ERP_TLS_CERT and ERP_TLS_KEY are required: erp-analytics serves over mTLS alone (ADR-0033)")
	}
	if err := s.tls.Refresh(ctx); err != nil {
		return s, fmt.Errorf("ERP_TLS_CA, ERP_TLS_CERT, ERP_TLS_KEY: %w", err)
	}
	s.store, err = objects(getenv, s.tls)
	return s, err
}

// limits reads the per-query DuckDB limits and the cap on concurrent
// queries; unset ones take the reader's defaults.
func limits(getenv func(string) string) (reader.Limits, error) {
	l := reader.Limits{MemoryLimit: getenv("ERP_ANALYTICS_MEMORY_LIMIT")}
	for name, dst := range map[string]*int{"ERP_ANALYTICS_THREADS": &l.Threads, "ERP_ANALYTICS_MAX_QUERIES": &l.MaxQueries} {
		v := getenv(name)
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return l, fmt.Errorf("%s %q: want a positive integer", name, v)
		}
		*dst = n
	}
	return l.Defaults()
}

// objects is the cell bucket, or a directory in development.
func objects(getenv func(string) string, t *tlsconf.Store) (blobstore.Store, error) {
	dir, endpoint, bucket := getenv("ERP_OBJECT_DIR"), getenv("ERP_OBJECT_S3_ENDPOINT"), getenv("ERP_OBJECT_S3_BUCKET")
	switch {
	case dir != "" && endpoint == "":
		return blobstore.Dir{Root: dir}, nil
	case dir == "" && endpoint != "" && bucket != "":
		client := t.HTTPClient(30 * time.Second)
		// The deployment's ambient credentials (ADR-0024): a role allowed to
		// read analytics/ alone, exchanged at the STS endpoint the cell
		// names (its VPC endpoint), as erp-server's are; unset, the defaults.
		creds := awscreds.Default(awscreds.Static{}, getenv, getenv("ERP_AWS_STS_ENDPOINT"), getenv("ERP_AWS_IMDS_ENDPOINT"), client)
		return blobstore.S3{Endpoint: endpoint, Bucket: bucket, Region: defaulted(getenv("ERP_OBJECT_S3_REGION"), "us-east-1"), Credentials: creds, Client: client}, nil
	}
	return nil, errors.New("ERP_OBJECT_DIR or ERP_OBJECT_S3_ENDPOINT with ERP_OBJECT_S3_BUCKET: the store to read")
}

func defaulted(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// cli runs the reader and returns the exit code: 2 for bad configuration,
// 1 for a failure while running, 0 after a clean shutdown.
func cli(ctx context.Context, getenv func(string) string, stderr io.Writer, started func(net.Addr)) int {
	s, err := load(ctx, getenv)
	if err != nil {
		fmt.Fprintln(stderr, "erp-analytics: configuration:", err)
		return 2
	}
	log := telemetry.NewLogger(stderr, s.level)
	if err := run(ctx, s, log, started); err != nil {
		log.Error("erp-analytics stopped", "error", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, s settings, log *slog.Logger, started func(net.Addr)) error {
	if err := os.MkdirAll(s.cache, 0o700); err != nil {
		return fmt.Errorf("erp-analytics: cache: %w", err)
	}
	r, err := reader.New(s.store, s.cache, currency.Seeded(), s.limits)
	if err != nil {
		return err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.addr)
	if err != nil {
		return fmt.Errorf("erp-analytics: listen on %s: %w", s.addr, err)
	}
	go s.tls.Run(ctx, tlsconf.RefreshInterval, log)
	mux := http.NewServeMux()
	mux.Handle(reader.QueryPath, httpapi.RequirePeer(s.caller, log, reader.Handler(r, log)))
	mux.Handle("GET /version", buildinfo.Handler())
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: s.timeout, WriteTimeout: 2 * s.timeout}
	errc := make(chan error, 1)
	go func() {
		errc <- srv.Serve(tls.NewListener(ln, s.tls.ServerConfig(tlsconf.Internal, tls.RequireAndVerifyClientCert)))
	}()
	build := buildinfo.Get()
	log.Info("erp-analytics started", "addr", ln.Addr().String(), "caller", s.caller, "version", build.Version, "revision", build.Revision)
	if started != nil {
		started(ln.Addr())
	}
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}
	shutdownCtx, done := context.WithTimeout(context.WithoutCancel(ctx), s.timeout)
	defer done()
	// A query that does not finish in time is abandoned with the process;
	// it changed nothing, so its error changes nothing either.
	_ = srv.Shutdown(shutdownCtx)
	log.Info("erp-analytics stopped")
	return serveErr
}
