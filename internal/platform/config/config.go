// Package config reads the server's configuration from the environment
// (twelve-factor). Every setting is validated at start, so a misconfigured
// replica fails fast instead of serving errors.
package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/ratelimit"
)

// Config is the server configuration.
type Config struct {
	HTTPAddr        string          // ERP_HTTP_ADDR, public API (default :8080)
	AdminAddr       string          // ERP_ADMIN_ADDR, liveness, readiness, metrics (default :9090)
	DatabaseURL     string          // DATABASE_URL (required)
	AppRole         string          // ERP_DB_APP_ROLE (default erp_app)
	RelayRole       string          // ERP_DB_RELAY_ROLE (default erp_relay)
	ProjectorRole   string          // ERP_DB_PROJECTOR_ROLE (default erp_projector)
	WorkerRole      string          // ERP_DB_WORKER_ROLE, finds queued payment proposals (default erp_worker)
	ControlRole     string          // ERP_DB_CONTROL_ROLE, lists tenants for the control plane (default erp_control)
	Cell            string          // ERP_CELL, part of event subjects (default local)
	NATSURL         string          // ERP_NATS_URL; empty disables the event relay
	OTLPEndpoint    string          // OTEL_EXPORTER_OTLP_TRACES_ENDPOINT; empty keeps traces local
	Issuers         []authn.Issuer  // ERP_OIDC_ISSUERS, JSON list of {issuer, audience, tenant_claim} (required)
	OperatorIssuers []authn.Issuer  // ERP_OPERATOR_ISSUERS, the same for the operators' tokens; empty leaves the control plane off (unless ERP_DEV_ISSUER)
	PageTokenKey    []byte          // ERP_PAGE_TOKEN_KEY, base64, at least 32 bytes (required)
	LLMEnabled      bool            // ANTHROPIC_API_KEY is set: invoice capture reads documents with Claude
	BankOutboxDir   string          // ERP_BANK_OUTBOX_DIR, where transmitted bank files are dropped; empty disables transmission
	ExtractDir      string          // ERP_EXTRACT_DIR, where sap_file connectors find SAP extracts; empty disables them
	FieldKey        []byte          // ERP_FIELD_KEY, base64, at least 32 bytes: encrypts bank accounts and tax identifiers (required)
	MigrateOnStart  bool            // ERP_MIGRATE_ON_START
	LogLevel        slog.Level      // ERP_LOG_LEVEL: debug, info, warn, error (default info)
	ShutdownTimeout time.Duration   // ERP_SHUTDOWN_TIMEOUT (default 20s)
	RequestTimeout  time.Duration   // ERP_REQUEST_TIMEOUT, deadline of one API request (default 30s)
	DBMaxConns      int32           // ERP_DB_MAX_CONNS, connections per replica; 0 keeps the driver's default
	TenantQuota     ratelimit.Quota // ERP_RATE_LIMIT_TENANT, requests per second[/burst] per tenant, or off (default 2000/10000, 04 §4.11)
	PrincipalQuota  ratelimit.Quota // ERP_RATE_LIMIT_PRINCIPAL, the same per principal (default 50/100)
	DevIssuer       bool            // ERP_DEV_ISSUER: mint tokens on the admin listener for local development; makes ERP_OIDC_ISSUERS optional
	Objects         Objects         // ERP_OBJECT_DIR or ERP_OBJECT_S3_*: object storage for documents, bank files and audit anchors; neither leaves it off
}

// Objects is the object storage configuration: a directory (development) or
// an S3-compatible service, never both.
type Objects struct {
	Dir       string        // ERP_OBJECT_DIR, an existing directory
	Endpoint  string        // ERP_OBJECT_S3_ENDPOINT, e.g. https://s3.eu-west-1.amazonaws.com or http://localhost:9000
	Bucket    string        // ERP_OBJECT_S3_BUCKET
	Region    string        // ERP_OBJECT_S3_REGION (default us-east-1)
	AccessKey string        // ERP_OBJECT_S3_ACCESS_KEY
	SecretKey string        // ERP_OBJECT_S3_SECRET_KEY (a vault reference later)
	Retention time.Duration // ERP_OBJECT_RETENTION: how long audit anchors are locked against deletion (default 10 years, 01 §1.3; 0 disables the lock)
}

// Enabled reports whether object storage is configured.
func (o Objects) Enabled() bool { return o.Dir != "" || o.Endpoint != "" }

var cellPattern = regexp.MustCompile(`^[a-z0-9]{1,20}$`)

// Load reads and validates the configuration.
func Load(getenv func(string) string) (Config, error) {
	get := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	c := Config{
		HTTPAddr:      get("ERP_HTTP_ADDR", ":8080"),
		AdminAddr:     get("ERP_ADMIN_ADDR", ":9090"),
		DatabaseURL:   getenv("DATABASE_URL"),
		AppRole:       get("ERP_DB_APP_ROLE", "erp_app"),
		RelayRole:     get("ERP_DB_RELAY_ROLE", "erp_relay"),
		ProjectorRole: get("ERP_DB_PROJECTOR_ROLE", "erp_projector"),
		WorkerRole:    get("ERP_DB_WORKER_ROLE", "erp_worker"),
		ControlRole:   get("ERP_DB_CONTROL_ROLE", "erp_control"),
		Cell:          get("ERP_CELL", "local"),
		NATSURL:       getenv("ERP_NATS_URL"),
		OTLPEndpoint:  getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"),
		LLMEnabled:    getenv("ANTHROPIC_API_KEY") != "",
		BankOutboxDir: getenv("ERP_BANK_OUTBOX_DIR"),
		ExtractDir:    getenv("ERP_EXTRACT_DIR"),
	}
	var errs []error
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if !cellPattern.MatchString(c.Cell) {
		errs = append(errs, fmt.Errorf("ERP_CELL %q must be 1-20 lower-case letters or digits", c.Cell))
	}
	dev, err := strconv.ParseBool(get("ERP_DEV_ISSUER", "false"))
	if err != nil {
		errs = append(errs, errors.New("ERP_DEV_ISSUER must be true or false"))
	}
	c.DevIssuer = dev
	if raw := getenv("ERP_OIDC_ISSUERS"); raw != "" || !c.DevIssuer {
		if err := json.Unmarshal([]byte(raw), &c.Issuers); err != nil || len(c.Issuers) == 0 {
			errs = append(errs, errors.New(`ERP_OIDC_ISSUERS must be a JSON list of {"issuer", "audience", "tenant_claim"} (optional with ERP_DEV_ISSUER=true)`))
		}
	}
	if raw := getenv("ERP_OPERATOR_ISSUERS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &c.OperatorIssuers); err != nil || len(c.OperatorIssuers) == 0 {
			errs = append(errs, errors.New(`ERP_OPERATOR_ISSUERS must be a JSON list of {"issuer", "audience"} when set`))
		}
	}
	key, err := base64.StdEncoding.DecodeString(getenv("ERP_PAGE_TOKEN_KEY"))
	if err != nil || len(key) < 32 {
		errs = append(errs, errors.New("ERP_PAGE_TOKEN_KEY must be base64 of at least 32 random bytes, the same on every replica"))
	}
	c.PageTokenKey = key
	if c.FieldKey, err = base64.StdEncoding.DecodeString(getenv("ERP_FIELD_KEY")); err != nil || len(c.FieldKey) < 32 {
		errs = append(errs, errors.New("ERP_FIELD_KEY must be base64 of at least 32 random bytes, the same on every replica and kept secret"))
	}
	c.Objects = Objects{Dir: getenv("ERP_OBJECT_DIR"), Endpoint: getenv("ERP_OBJECT_S3_ENDPOINT"), Bucket: getenv("ERP_OBJECT_S3_BUCKET"),
		Region: get("ERP_OBJECT_S3_REGION", "us-east-1"), AccessKey: getenv("ERP_OBJECT_S3_ACCESS_KEY"), SecretKey: getenv("ERP_OBJECT_S3_SECRET_KEY")}
	switch {
	case c.Objects.Dir != "" && c.Objects.Endpoint != "":
		errs = append(errs, errors.New("ERP_OBJECT_DIR and ERP_OBJECT_S3_ENDPOINT exclude each other: one object store"))
	case c.Objects.Endpoint != "" && (c.Objects.Bucket == "" || c.Objects.AccessKey == "" || c.Objects.SecretKey == ""):
		errs = append(errs, errors.New("ERP_OBJECT_S3_ENDPOINT needs ERP_OBJECT_S3_BUCKET, ERP_OBJECT_S3_ACCESS_KEY and ERP_OBJECT_S3_SECRET_KEY"))
	}
	if c.Objects.Retention, err = time.ParseDuration(get("ERP_OBJECT_RETENTION", "87600h")); err != nil || c.Objects.Retention < 0 {
		errs = append(errs, errors.New("ERP_OBJECT_RETENTION must be a duration such as 87600h (10 years), or 0 for no lock"))
	}
	for name, dir := range map[string]string{"ERP_BANK_OUTBOX_DIR": c.BankOutboxDir, "ERP_EXTRACT_DIR": c.ExtractDir, "ERP_OBJECT_DIR": c.Objects.Dir} {
		if fi, err := os.Stat(dir); dir != "" && (err != nil || !fi.IsDir()) {
			errs = append(errs, fmt.Errorf("%s %q must be an existing directory", name, dir))
		}
	}
	if c.MigrateOnStart, err = strconv.ParseBool(get("ERP_MIGRATE_ON_START", "false")); err != nil {
		errs = append(errs, errors.New("ERP_MIGRATE_ON_START must be true or false"))
	}
	if err := c.LogLevel.UnmarshalText([]byte(get("ERP_LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, errors.New("ERP_LOG_LEVEL must be debug, info, warn or error"))
	}
	if c.ShutdownTimeout, err = time.ParseDuration(get("ERP_SHUTDOWN_TIMEOUT", "20s")); err != nil || c.ShutdownTimeout <= 0 {
		errs = append(errs, errors.New("ERP_SHUTDOWN_TIMEOUT must be a positive duration such as 20s"))
	}
	if c.RequestTimeout, err = time.ParseDuration(get("ERP_REQUEST_TIMEOUT", "30s")); err != nil || c.RequestTimeout <= 0 {
		errs = append(errs, errors.New("ERP_REQUEST_TIMEOUT must be a positive duration such as 30s"))
	}
	if n, err := strconv.Atoi(get("ERP_DB_MAX_CONNS", "0")); err != nil || n < 0 || n > 10_000 {
		errs = append(errs, errors.New("ERP_DB_MAX_CONNS must be a whole number of connections (0: the driver's default)"))
	} else {
		c.DBMaxConns = int32(n)
	}
	if c.TenantQuota, err = ratelimit.ParseQuota(get("ERP_RATE_LIMIT_TENANT", "2000/10000")); err != nil {
		errs = append(errs, fmt.Errorf("ERP_RATE_LIMIT_TENANT: %w", err))
	}
	if c.PrincipalQuota, err = ratelimit.ParseQuota(get("ERP_RATE_LIMIT_PRINCIPAL", "50/100")); err != nil {
		errs = append(errs, fmt.Errorf("ERP_RATE_LIMIT_PRINCIPAL: %w", err))
	}
	return c, errors.Join(errs...)
}
