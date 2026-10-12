// Package config reads the server's configuration from the environment
// (twelve-factor). Every setting is validated at start, so a misconfigured
// replica fails fast instead of serving errors.
package config

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/ratelimit"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
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
	FieldKey        []byte          // ERP_FIELD_KEY, base64, at least 32 bytes: the legacy field encryption key (values sealed before tenant keys); required unless ERP_FIELD_KEYS is set
	FieldKeys       []MasterKey     // ERP_FIELD_KEYS, "id=base64,id=base64,...": the master keys that wrap tenant keys, the first for new ones; default one master "v1" from ERP_FIELD_KEY
	KMS             KMS             // ERP_FIELD_KMS_*: a KMS key in front of the ring, the master that wraps new tenant keys (ADR-0019)
	MigrateOnStart  bool            // ERP_MIGRATE_ON_START
	LogLevel        slog.Level      // ERP_LOG_LEVEL: debug, info, warn, error (default info)
	ShutdownTimeout time.Duration   // ERP_SHUTDOWN_TIMEOUT (default 20s)
	RequestTimeout  time.Duration   // ERP_REQUEST_TIMEOUT, deadline of one API request (default 30s)
	DBMaxConns      int32           // ERP_DB_MAX_CONNS, connections per replica; 0 keeps the driver's default
	TenantQuota     ratelimit.Quota // ERP_RATE_LIMIT_TENANT, requests per second[/burst] per tenant, or off (default 2000/10000, 04 §4.11)
	PrincipalQuota  ratelimit.Quota // ERP_RATE_LIMIT_PRINCIPAL, the same per principal (default 50/100)
	DevIssuer       bool            // ERP_DEV_ISSUER: mint tokens on the admin listener for local development; makes ERP_OIDC_ISSUERS optional
	Objects         Objects         // ERP_OBJECT_DIR or ERP_OBJECT_S3_*: object storage for documents, bank files and audit anchors; neither leaves it off
	OffboardGrace   time.Duration   // ERP_OFFBOARD_GRACE: how long an offboarded tenant's data stays in the cell after its export before it is erased (default 720h; ADR-0021)
	Vault           Vault           // ERP_VAULT_*: where vault: references are resolved
	// Secrets resolves references (env:, file:, vault:) for the settings that
	// carry secrets and for connectors' secret_ref.
	Secrets secrets.Resolver
}

// MasterKey is a key of the ring that wraps tenant keys (ADR-0018).
type MasterKey struct {
	ID     string
	Secret []byte
}

// KMS is the AWS KMS key (or a service speaking its protocol) that wraps
// tenant keys, in front of the ERP_FIELD_KEYS ring.
type KMS struct {
	Name      string // the master's id in tenancy.tenant_key: ERP_FIELD_KMS_KEY as "name=key" (default kms)
	Key       string // ERP_FIELD_KMS_KEY: the key id, ARN or alias/name
	Region    string // ERP_FIELD_KMS_REGION (default us-east-1)
	Endpoint  string // ERP_FIELD_KMS_ENDPOINT (default https://kms.<region>.amazonaws.com)
	AccessKey string // ERP_FIELD_KMS_ACCESS_KEY
	SecretKey string // ERP_FIELD_KMS_SECRET_KEY
}

// Enabled reports whether a KMS master is configured.
func (k KMS) Enabled() bool { return k.Key != "" }

// Vault is the HashiCorp Vault connection for vault: references.
type Vault struct {
	Address   string // ERP_VAULT_ADDR; empty: vault: references are not resolvable
	Token     string // ERP_VAULT_TOKEN (a value or an env:/file: reference)
	Namespace string // ERP_VAULT_NAMESPACE
	K8sRole   string // ERP_VAULT_K8S_ROLE: log in with the Kubernetes method as this role
	K8sJWT    string // ERP_VAULT_K8S_JWT_FILE (default /var/run/secrets/kubernetes.io/serviceaccount/token): the service account token, read at start
}

// SecretSettings are the settings that may carry a secret reference
// instead of a value.
var SecretSettings = []string{"DATABASE_URL", "ERP_PAGE_TOKEN_KEY", "ERP_FIELD_KEY", "ERP_FIELD_KEYS", "ERP_FIELD_KMS_ACCESS_KEY", "ERP_FIELD_KMS_SECRET_KEY",
	"ERP_OBJECT_S3_ACCESS_KEY", "ERP_OBJECT_S3_SECRET_KEY", "ERP_NATS_URL"}

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

var (
	cellPattern     = regexp.MustCompile(`^[a-z0-9]{1,20}$`)
	masterIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// Load reads and validates the configuration. The settings in
// SecretSettings may be references (env:, file:, vault:), resolved here, so
// that credentials live in the vault or in mounted files rather than in
// the environment of every replica.
func Load(getenv func(string) string) (Config, error) {
	var errs []error
	resolver, vault, err := resolver(getenv)
	if err != nil {
		errs = append(errs, err)
	}
	raw := getenv
	resolved := map[string]string{}
	for _, name := range SecretSettings {
		v, err := secrets.Value(context.Background(), resolver, raw(name))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			v = ""
		}
		resolved[name] = v
	}
	getenv = func(key string) string {
		if v, ok := resolved[key]; ok {
			return v
		}
		return raw(key)
	}
	get := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	c := Config{
		Vault:         vault,
		Secrets:       resolver,
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
	if raw := getenv("ERP_FIELD_KEY"); raw != "" || (getenv("ERP_FIELD_KEYS") == "" && getenv("ERP_FIELD_KMS_KEY") == "") {
		if c.FieldKey, err = base64.StdEncoding.DecodeString(raw); err != nil || len(c.FieldKey) < 32 {
			errs = append(errs, errors.New("ERP_FIELD_KEY must be base64 of at least 32 random bytes, the same on every replica and kept secret (optional with ERP_FIELD_KEYS or ERP_FIELD_KMS_KEY)"))
		}
	}
	if raw := getenv("ERP_FIELD_KEYS"); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			id, b64, ok := strings.Cut(strings.TrimSpace(part), "=")
			secret, derr := base64.StdEncoding.DecodeString(b64)
			if !ok || !masterIDPattern.MatchString(id) || derr != nil || len(secret) < 32 {
				errs = append(errs, errors.New(`ERP_FIELD_KEYS must be "id=base64,..." with ids of 1-64 letters, digits, - or _ and secrets of at least 32 bytes`))
				break
			}
			c.FieldKeys = append(c.FieldKeys, MasterKey{ID: id, Secret: secret})
		}
	} else if len(c.FieldKey) >= 32 {
		c.FieldKeys = []MasterKey{{ID: "v1", Secret: c.FieldKey}}
	}
	c.KMS = KMS{Name: "kms", Key: getenv("ERP_FIELD_KMS_KEY"), Region: get("ERP_FIELD_KMS_REGION", "us-east-1"), Endpoint: getenv("ERP_FIELD_KMS_ENDPOINT"),
		AccessKey: getenv("ERP_FIELD_KMS_ACCESS_KEY"), SecretKey: getenv("ERP_FIELD_KMS_SECRET_KEY")}
	if c.KMS.Enabled() {
		if name, key, ok := strings.Cut(c.KMS.Key, "="); ok {
			c.KMS.Name, c.KMS.Key = name, key
		}
		if c.KMS.Key == "" || !masterIDPattern.MatchString(c.KMS.Name) {
			errs = append(errs, errors.New(`ERP_FIELD_KMS_KEY must be a KMS key id, ARN or alias, or "name=key" with a name of 1-64 letters, digits, - or _`))
		}
		if c.KMS.AccessKey == "" || c.KMS.SecretKey == "" {
			errs = append(errs, errors.New("ERP_FIELD_KMS_KEY needs ERP_FIELD_KMS_ACCESS_KEY and ERP_FIELD_KMS_SECRET_KEY"))
		}
		if c.KMS.Endpoint == "" {
			c.KMS.Endpoint = "https://kms." + c.KMS.Region + ".amazonaws.com"
		}
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
	if c.OffboardGrace, err = time.ParseDuration(get("ERP_OFFBOARD_GRACE", "720h")); err != nil || c.OffboardGrace < 0 {
		errs = append(errs, errors.New("ERP_OFFBOARD_GRACE must be a duration such as 720h (30 days); 0 erases right after the export"))
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

// DefaultK8sJWTFile is where Kubernetes mounts the service account token.
const DefaultK8sJWTFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// resolver builds the secret resolver the configuration names: env: and
// file: always, vault: when ERP_VAULT_ADDR is set.
func resolver(getenv func(string) string) (secrets.Resolver, Vault, error) {
	local := secrets.Local{Getenv: getenv}
	m := secrets.Multi{secrets.SchemeEnv: local, secrets.SchemeFile: local}
	v := Vault{Address: getenv("ERP_VAULT_ADDR"), Namespace: getenv("ERP_VAULT_NAMESPACE"), K8sRole: getenv("ERP_VAULT_K8S_ROLE")}
	if v.Address == "" {
		return m, v, nil
	}
	token, err := secrets.Value(context.Background(), local, getenv("ERP_VAULT_TOKEN"))
	if err != nil {
		return m, v, fmt.Errorf("ERP_VAULT_TOKEN: %w", err)
	}
	v.Token = token
	if v.Token == "" {
		if v.K8sRole == "" {
			return m, v, errors.New("ERP_VAULT_ADDR needs ERP_VAULT_TOKEN or ERP_VAULT_K8S_ROLE")
		}
		file := getenv("ERP_VAULT_K8S_JWT_FILE")
		if file == "" {
			file = DefaultK8sJWTFile
		}
		jwt, err := local.Resolve(context.Background(), "file:"+file)
		if err != nil {
			return m, v, fmt.Errorf("ERP_VAULT_K8S_JWT_FILE: %w", err)
		}
		v.K8sJWT = jwt
	}
	m[secrets.SchemeVault] = &secrets.Vault{Address: v.Address, Token: v.Token, Namespace: v.Namespace, K8sRole: v.K8sRole, K8sJWT: v.K8sJWT}
	return m, v, nil
}
