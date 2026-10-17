package config

import (
	"context"
	"encoding/base64"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/ratelimit"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets/vaulttest"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf"
	"github.com/mustie7iy/agentic-erp/internal/platform/tlsconf/tlstest"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

var key = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))

// field is one value a test expects Load to produce; got and want must be
// comparable and of the same type.
type field struct {
	name      string
	got, want any
}

// checkFields reports every field whose value is not the one expected.
func checkFields(t *testing.T, fields []field) {
	t.Helper()
	for _, f := range fields {
		if f.got != f.want {
			t.Errorf("%s: got %v, want %v", f.name, f.got, f.want)
		}
	}
}

// mustLoad loads the configuration of the settings, failing the test on an error.
func mustLoad(t *testing.T, m map[string]string) Config {
	t.Helper()
	c, err := Load(env(m))
	if err != nil {
		t.Fatalf("load: got %v, want no error", err)
	}
	return c
}

// copyOf is a copy of the settings, for a case that changes some of them.
func copyOf(m map[string]string) map[string]string {
	mm := make(map[string]string, len(m))
	for k, v := range m {
		mm[k] = v
	}
	return mm
}

func valid() map[string]string {
	return map[string]string{
		"DATABASE_URL":       "postgres://x",
		"ERP_ENV":            Development,
		"ERP_OIDC_ISSUERS":   `[{"issuer":"https://idp.example.com","audience":"erp-api"}]`,
		"ERP_PAGE_TOKEN_KEY": key,
		"ERP_FIELD_KEY":      key,
	}
}

func TestDefaults(t *testing.T) {
	c := mustLoad(t, valid())
	checkFields(t, []field{
		{"HTTPAddr", c.HTTPAddr, ":8080"},
		{"AdminAddr", c.AdminAddr, ":9090"},
		{"AppRole", c.AppRole, "erp_app"},
		{"RelayRole", c.RelayRole, "erp_relay"},
		{"ProjectorRole", c.ProjectorRole, "erp_projector"},
		{"WorkerRole", c.WorkerRole, "erp_worker"},
		{"ControlRole", c.ControlRole, "erp_control"},
		{"OperatorIssuers", len(c.OperatorIssuers), 0},
		{"BankOutboxDir", c.BankOutboxDir, ""},
		{"FieldKey length", len(c.FieldKey), 32},
		{"Cell", c.Cell, "local"},
		{"MigrateOnStart", c.MigrateOnStart, false},
		{"LogLevel", c.LogLevel, slog.LevelInfo},
		{"ShutdownTimeout", c.ShutdownTimeout, 20 * time.Second},
		{"Issuers", len(c.Issuers), 1},
		{"PageTokenKey length", len(c.PageTokenKey), 32},
		{"RequestTimeout", c.RequestTimeout, 30 * time.Second},
		{"DBMaxConns", c.DBMaxConns, int32(0)},
		{"DevIssuer", c.DevIssuer, false},
		{"Objects enabled", c.Objects.Enabled(), false},
		{"Objects region", c.Objects.Region, "us-east-1"},
		{"Objects retention", c.Objects.Retention, 87600 * time.Hour},
		{"TenantQuota", c.TenantQuota, ratelimit.Quota{Rate: 2000, Burst: 10000}},
		{"PrincipalQuota", c.PrincipalQuota, ratelimit.Quota{Rate: 50, Burst: 100}},
	})
}

func TestOverrides(t *testing.T) {
	m := valid()
	m["ERP_CELL"], m["ERP_MIGRATE_ON_START"], m["ERP_LOG_LEVEL"], m["ERP_SHUTDOWN_TIMEOUT"], m["ERP_NATS_URL"] = "eu1", "true", "debug", "5s", "nats://n"
	m["ANTHROPIC_API_KEY"], m["ERP_BANK_OUTBOX_DIR"], m["ERP_EXTRACT_DIR"] = "key", t.TempDir(), t.TempDir()
	m["ERP_REQUEST_TIMEOUT"], m["ERP_DB_MAX_CONNS"], m["ERP_RATE_LIMIT_TENANT"], m["ERP_RATE_LIMIT_PRINCIPAL"] = "10s", "40", "off", "5"
	c := mustLoad(t, m)
	checkFields(t, []field{
		{"LLMEnabled", c.LLMEnabled, true},
		{"BankOutboxDir", c.BankOutboxDir, m["ERP_BANK_OUTBOX_DIR"]},
		{"ExtractDir", c.ExtractDir, m["ERP_EXTRACT_DIR"]},
		{"Cell", c.Cell, "eu1"},
		{"MigrateOnStart", c.MigrateOnStart, true},
		{"LogLevel", c.LogLevel, slog.LevelDebug},
		{"ShutdownTimeout", c.ShutdownTimeout, 5 * time.Second},
		{"NATSURL", c.NATSURL, "nats://n"},
		{"RequestTimeout", c.RequestTimeout, 10 * time.Second},
		{"DBMaxConns", c.DBMaxConns, int32(40)},
		{"TenantQuota", c.TenantQuota, ratelimit.Quota{}},
		{"PrincipalQuota", c.PrincipalQuota, ratelimit.Quota{Rate: 5, Burst: 5}},
	})
	// With the development issuer the identity providers are optional, and still validated when given.
	m = valid()
	delete(m, "ERP_OIDC_ISSUERS")
	m["ERP_DEV_ISSUER"] = "true"
	c = mustLoad(t, m)
	checkFields(t, []field{{"DevIssuer", c.DevIssuer, true}, {"Issuers", len(c.Issuers), 0}})
	m["ERP_OIDC_ISSUERS"] = "[]"
	if _, err := Load(env(m)); err == nil {
		t.Fatal("empty issuer list accepted")
	}
	// Without either, tenants' tokens come from their registered identity
	// providers alone (ADR-0031).
	m = valid()
	delete(m, "ERP_OIDC_ISSUERS")
	c = mustLoad(t, m)
	checkFields(t, []field{{"DevIssuer", c.DevIssuer, false}, {"Issuers", len(c.Issuers), 0}})
	// Master keys: a ring from ERP_FIELD_KEYS, or one master from the legacy key.
	m = valid()
	c = mustLoad(t, m)
	if len(c.FieldKeys) != 1 {
		t.Fatalf("legacy key ring: got %d keys, want 1", len(c.FieldKeys))
	}
	checkFields(t, []field{{"FieldKeys[0] id", c.FieldKeys[0].ID, "v1"}, {"FieldKeys[0] secret length", len(c.FieldKeys[0].Secret), 32}})
	m["ERP_FIELD_KEYS"] = "k2=" + key + ", k1=" + key
	c = mustLoad(t, m)
	if len(c.FieldKeys) != 2 {
		t.Fatalf("key ring: got %d keys, want 2", len(c.FieldKeys))
	}
	checkFields(t, []field{{"FieldKeys[0] id", c.FieldKeys[0].ID, "k2"}, {"FieldKeys[1] id", c.FieldKeys[1].ID, "k1"}})
	delete(m, "ERP_FIELD_KEY")
	c = mustLoad(t, m)
	checkFields(t, []field{{"ring without a legacy key, FieldKey length", len(c.FieldKey), 0}, {"ring without a legacy key, FieldKeys", len(c.FieldKeys), 2}})
	for _, bad := range []string{"k1", "k1=short", "bad id!=" + key, key} {
		t.Run("field keys "+bad, func(t *testing.T) {
			mm := copyOf(m)
			mm["ERP_FIELD_KEYS"] = bad
			_, err := Load(env(mm))
			pgtest.CheckErr(t, err, "ERP_FIELD_KEYS")
		})
	}
	// Operator issuers are optional and validated when given.
	m = valid()
	m["ERP_OPERATOR_ISSUERS"], m["ERP_DB_CONTROL_ROLE"] = `[{"issuer":"https://ops.example.com","audience":"erp-control"}]`, "ops_role"
	c = mustLoad(t, m)
	if len(c.OperatorIssuers) != 1 {
		t.Fatalf("operator issuers: got %d, want 1", len(c.OperatorIssuers))
	}
	checkFields(t, []field{{"operator audience", c.OperatorIssuers[0].Audience, "erp-control"}, {"ControlRole", c.ControlRole, "ops_role"}})
	for _, bad := range []string{"[]", "nope"} {
		t.Run("operator issuers "+bad, func(t *testing.T) {
			mm := copyOf(m)
			mm["ERP_OPERATOR_ISSUERS"] = bad
			_, err := Load(env(mm))
			pgtest.CheckErr(t, err, "ERP_OPERATOR_ISSUERS")
		})
	}
	// Object storage: a directory or an S3 service, complete, never both.
	m = valid()
	m["ERP_OBJECT_DIR"], m["ERP_OBJECT_RETENTION"] = t.TempDir(), "0"
	c = mustLoad(t, m)
	checkFields(t, []field{
		{"Objects enabled", c.Objects.Enabled(), true},
		{"Objects dir", c.Objects.Dir, m["ERP_OBJECT_DIR"]},
		{"Objects retention", c.Objects.Retention, time.Duration(0)},
	})
	m = valid()
	m["ERP_OBJECT_S3_ENDPOINT"], m["ERP_OBJECT_S3_BUCKET"], m["ERP_OBJECT_S3_REGION"] = "http://localhost:9000", "erp", "eu-west-1"
	m["ERP_OBJECT_S3_ACCESS_KEY"], m["ERP_OBJECT_S3_SECRET_KEY"] = "ak", "sk"
	c = mustLoad(t, m)
	checkFields(t, []field{
		{"Objects enabled", c.Objects.Enabled(), true},
		{"Objects bucket", c.Objects.Bucket, "erp"},
		{"Objects region", c.Objects.Region, "eu-west-1"},
		{"Objects secret key", c.Objects.SecretKey, "sk"},
	})
	for name, change := range map[string]func(){
		"both":         func() { m["ERP_OBJECT_DIR"] = t.TempDir() },
		"no bucket":    func() { delete(m, "ERP_OBJECT_S3_BUCKET") },
		"no secret":    func() { delete(m, "ERP_OBJECT_S3_SECRET_KEY") },
		"retention":    func() { m["ERP_OBJECT_RETENTION"] = "soon" },
		"negative":     func() { m["ERP_OBJECT_RETENTION"] = "-1h" },
		"no directory": func() { delete(m, "ERP_OBJECT_S3_ENDPOINT"); m["ERP_OBJECT_DIR"] = "/no/such/dir" },
	} {
		t.Run(name, func(t *testing.T) {
			saved := copyOf(m)
			defer func() { m = saved }()
			change()
			_, err := Load(env(m))
			pgtest.CheckErr(t, err, "ERP_OBJECT")
		})
	}
}

func TestEveryProblemIsReported(t *testing.T) {
	_, err := Load(env(map[string]string{
		"ERP_CELL": "EU-1", "ERP_OIDC_ISSUERS": "[]", "ERP_PAGE_TOKEN_KEY": "c2hvcnQ=", "ERP_FIELD_KEY": "c2hvcnQ=",
		"ERP_MIGRATE_ON_START": "maybe", "ERP_LOG_LEVEL": "loud", "ERP_SHUTDOWN_TIMEOUT": "-1s", "ERP_BANK_OUTBOX_DIR": "/no/such/dir", "ERP_EXTRACT_DIR": "/no/such/dir",
		"ERP_REQUEST_TIMEOUT": "0", "ERP_DB_MAX_CONNS": "-1", "ERP_RATE_LIMIT_TENANT": "fast", "ERP_RATE_LIMIT_PRINCIPAL": "1/0", "ERP_DEV_ISSUER": "maybe",
	}))
	for _, want := range []string{"DATABASE_URL", "ERP_CELL", "ERP_OIDC_ISSUERS", "ERP_PAGE_TOKEN_KEY", "ERP_FIELD_KEY", "ERP_BANK_OUTBOX_DIR", "ERP_EXTRACT_DIR", "ERP_MIGRATE_ON_START", "ERP_LOG_LEVEL", "ERP_SHUTDOWN_TIMEOUT",
		"ERP_REQUEST_TIMEOUT", "ERP_DB_MAX_CONNS", "ERP_RATE_LIMIT_TENANT", "ERP_RATE_LIMIT_PRINCIPAL", "ERP_DEV_ISSUER"} {
		pgtest.CheckErr(t, err, want)
	}
}

// TestSecretReferences: settings that carry secrets may name where the
// secret lives (env:, file:, vault:) instead of carrying it.
func TestSecretReferences(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "field-key"), []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "jwt"), []byte(vaulttest.K8sJWT), 0o600); err != nil {
		t.Fatal(err)
	}
	vault := vaulttest.New(t)
	vault.Secrets["secret/erp/db"] = map[string]any{"url": "postgres://erp:pw@db/erp"}
	vault.Secrets["secret/erp/s3"] = map[string]any{"access": "AK", "secret": "SK"}
	m := valid()
	m["DATABASE_URL"] = "vault:secret/erp/db#url"
	m["ERP_FIELD_KEY"] = "file:" + filepath.Join(dir, "field-key")
	m["ERP_PAGE_TOKEN_KEY"] = "env:PAGE_KEY"
	m["PAGE_KEY"] = key
	m["ERP_OBJECT_S3_ENDPOINT"], m["ERP_OBJECT_S3_BUCKET"] = "http://localhost:9000", "erp"
	m["ERP_OBJECT_S3_ACCESS_KEY"], m["ERP_OBJECT_S3_SECRET_KEY"] = "vault:secret/erp/s3#access", "vault:secret/erp/s3#secret"
	m["ERP_VAULT_ADDR"], m["ERP_VAULT_TOKEN"] = vault.URL, "env:VAULT_TOKEN"
	m["VAULT_TOKEN"] = vaulttest.Token
	c := mustLoad(t, m)
	checkFields(t, []field{
		{"DatabaseURL", c.DatabaseURL, "postgres://erp:pw@db/erp"},
		{"FieldKey length", len(c.FieldKey), 32},
		{"PageTokenKey length", len(c.PageTokenKey), 32},
		{"Objects access key", c.Objects.AccessKey, "AK"},
		{"Objects secret key", c.Objects.SecretKey, "SK"},
		{"Vault token", c.Vault.Token, vaulttest.Token},
	})
	if c.Secrets == nil {
		t.Fatal("Secrets: got nil, want a resolver")
	}
	// The resolver serves connectors too.
	if got, err := c.Secrets.Resolve(context.Background(), "vault:secret/erp/s3#access"); err != nil || got != "AK" {
		t.Fatalf("%q %v", got, err)
	}
	// Kubernetes login with the service account token from a file.
	delete(m, "ERP_VAULT_TOKEN")
	m["ERP_VAULT_K8S_ROLE"], m["ERP_VAULT_K8S_JWT_FILE"] = vaulttest.K8sRole, filepath.Join(dir, "jwt")
	c = mustLoad(t, m)
	checkFields(t, []field{{"kubernetes JWT", c.Vault.K8sJWT, vaulttest.K8sJWT}, {"kubernetes DatabaseURL", c.DatabaseURL, "postgres://erp:pw@db/erp"}})
	for name, change := range map[string]func(){
		"missing env":        func() { m["ERP_PAGE_TOKEN_KEY"] = "env:NOPE" },
		"missing field":      func() { m["DATABASE_URL"] = "vault:secret/erp/db#nope" },
		"vault without auth": func() { delete(m, "ERP_VAULT_K8S_ROLE") },
		"no jwt file":        func() { m["ERP_VAULT_K8S_JWT_FILE"] = filepath.Join(dir, "none") },
		"bad token ref":      func() { m["ERP_VAULT_TOKEN"] = "env:NOPE" },
		"vault unreachable":  func() { m["ERP_VAULT_ADDR"] = "http://127.0.0.1:1" },
	} {
		t.Run(name, func(t *testing.T) {
			saved := copyOf(m)
			defer func() { m = saved }()
			change()
			if _, err := Load(env(m)); err == nil {
				t.Error("got no error, want the settings refused")
			}
		})
	}
	// Without a vault, vault: references cannot be resolved and say so.
	m = valid()
	m["DATABASE_URL"] = "vault:secret/erp/db#url"
	_, err := Load(env(m))
	if err == nil {
		t.Fatal("vault reference without a vault: got no error, want one")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("vault reference without a vault: got %v, want it to name DATABASE_URL", err)
	}
	if !strings.Contains(err.Error(), "no resolver") {
		t.Errorf("vault reference without a vault: got %v, want it to say no resolver", err)
	}
	// The default service account token path is used when none is given.
	if DefaultK8sJWTFile != "/var/run/secrets/kubernetes.io/serviceaccount/token" {
		t.Fatal(DefaultK8sJWTFile)
	}
	m = valid()
	m["ERP_VAULT_ADDR"], m["ERP_VAULT_K8S_ROLE"] = vault.URL, vaulttest.K8sRole
	_, err = Load(env(m))
	pgtest.WantErr(t, err, "ERP_VAULT_K8S_JWT_FILE")
}

func TestKMSSettings(t *testing.T) {
	m := valid()
	if c, err := Load(env(m)); err != nil || c.KMS.Enabled() {
		t.Fatal(c.KMS, err)
	}
	m["ERP_FIELD_KMS_KEY"], m["ERP_FIELD_KMS_ACCESS_KEY"], m["ERP_FIELD_KMS_SECRET_KEY"] = "alias/erp-master", "AKIDEXAMPLE", "secret"
	c, err := Load(env(m))
	if err != nil || c.KMS != (KMS{Name: "kms", Key: "alias/erp-master", Region: "us-east-1", Endpoint: "https://kms.us-east-1.amazonaws.com", AccessKey: "AKIDEXAMPLE", SecretKey: "secret"}) {
		t.Fatalf("%+v %v", c.KMS, err)
	}
	// A name for the master, a region and an endpoint; the legacy key is optional with a KMS.
	m["ERP_FIELD_KMS_KEY"], m["ERP_FIELD_KMS_REGION"], m["ERP_FIELD_KMS_ENDPOINT"] = "eu=arn:aws:kms:eu-west-1:111122223333:key/abc", "eu-west-1", "http://localhost:4566"
	delete(m, "ERP_FIELD_KEY")
	c = mustLoad(t, m)
	checkFields(t, []field{
		{"KMS name", c.KMS.Name, "eu"},
		{"KMS key", c.KMS.Key, "arn:aws:kms:eu-west-1:111122223333:key/abc"},
		{"KMS region", c.KMS.Region, "eu-west-1"},
		{"KMS endpoint", c.KMS.Endpoint, "http://localhost:4566"},
		{"FieldKeys", len(c.FieldKeys), 0},
	})
	for name, bad := range map[string]map[string]string{
		"no credentials": {"ERP_FIELD_KMS_ACCESS_KEY": ""},
		"bad name":       {"ERP_FIELD_KMS_KEY": "no name!=alias/x"},
		"no key":         {"ERP_FIELD_KMS_KEY": "kms="},
	} {
		t.Run(name, func(t *testing.T) {
			mm := copyOf(m)
			for k, v := range bad {
				mm[k] = v
			}
			_, err := Load(env(mm))
			pgtest.CheckErr(t, err, "ERP_FIELD_KMS_")
		})
	}
}

func TestOffboardGrace(t *testing.T) {
	m := valid()
	if c, err := Load(env(m)); err != nil || c.OffboardGrace != 720*time.Hour {
		t.Fatal(c.OffboardGrace, err)
	}
	m["ERP_OFFBOARD_GRACE"] = "0"
	if c, err := Load(env(m)); err != nil || c.OffboardGrace != 0 {
		t.Fatal(c.OffboardGrace, err)
	}
	for _, bad := range []string{"-1h", "soon"} {
		t.Run(bad, func(t *testing.T) {
			mm := copyOf(m)
			mm["ERP_OFFBOARD_GRACE"] = bad
			_, err := Load(env(mm))
			pgtest.CheckErr(t, err, "ERP_OFFBOARD_GRACE")
		})
	}
}

func TestDirectorySettings(t *testing.T) {
	m := valid()
	if c, err := Load(env(m)); err != nil || c.DirectoryURL != "" {
		t.Fatal(c.DirectoryURL, err)
	}
	m["ERP_DIRECTORY_URL"] = "postgres://dir@db/directory"
	_, err := Load(env(m))
	pgtest.WantErr(t, err, "ERP_CELL_API_URL")
	m["ERP_CELL_API_URL"] = "eu-1.api.example.com"
	_, err = Load(env(m))
	pgtest.WantErr(t, err, "ERP_CELL_API_URL")
	m["ERP_CELL_API_URL"] = "https://eu-1.api.example.com"
	_, err = Load(env(m))
	if err == nil {
		t.Fatal("directory without the cell's control URL and region: got no error, want one")
	}
	if !strings.Contains(err.Error(), "ERP_CELL_CONTROL_URL") {
		t.Errorf("directory without the cell's control URL: got %v, want it to name ERP_CELL_CONTROL_URL", err)
	}
	if !strings.Contains(err.Error(), "ERP_CELL_REGION") {
		t.Errorf("directory without the cell's region: got %v, want it to name ERP_CELL_REGION", err)
	}
	m["ERP_CELL_CONTROL_URL"], m["ERP_CELL_REGION"] = "http://eu-1-admin.internal:9090", "eu-west"
	c := mustLoad(t, m)
	checkFields(t, []field{
		{"DirectoryURL", c.DirectoryURL, "postgres://dir@db/directory"},
		{"CellAPIURL", c.CellAPIURL, "https://eu-1.api.example.com"},
		{"CellControlURL", c.CellControlURL, "http://eu-1-admin.internal:9090"},
		{"CellRegion", c.CellRegion, "eu-west"},
		{"CellAccepting", c.CellAccepting, true},
	})
	m["ERP_CELL_ACCEPTING"], m["ERP_CELL_REGION"] = "false", "EU"
	_, err = Load(env(m))
	pgtest.WantErr(t, err, "ERP_CELL_REGION")
	m["ERP_CELL_REGION"] = "eu-west"
	if c, err := Load(env(m)); err != nil || c.CellAccepting {
		t.Fatalf("not accepting: %+v %v", c, err)
	}
}

func TestAmbientAWSCredentials(t *testing.T) {
	m := valid()
	delete(m, "ERP_OBJECT_DIR")
	m["ERP_OBJECT_S3_ENDPOINT"], m["ERP_OBJECT_S3_BUCKET"] = "http://localhost:9000", "erp"
	c := mustLoad(t, m) // no access keys: the ambient chain
	checkFields(t, []field{
		{"Objects access key", c.Objects.AccessKey, ""},
		{"STS endpoint", c.AWS.STSEndpoint, "https://sts.amazonaws.com"},
		{"IMDS endpoint", c.AWS.IMDSEndpoint, "http://169.254.169.254"},
	})
	if c.AWS.Getenv == nil {
		t.Error("AWS Getenv: got nil, want the environment")
	}
	m["ERP_OBJECT_S3_ACCESS_KEY"] = "AK"
	_, err := Load(env(m))
	pgtest.WantErr(t, err, "go together")
	delete(m, "ERP_OBJECT_S3_ACCESS_KEY")
	m["ERP_FIELD_KMS_KEY"], m["ERP_FIELD_KMS_SECRET_KEY"] = "alias/erp", "SK"
	_, err = Load(env(m))
	pgtest.WantErr(t, err, "go together")
	delete(m, "ERP_FIELD_KMS_SECRET_KEY")
	m["ERP_AWS_STS_ENDPOINT"], m["ERP_AWS_IMDS_ENDPOINT"], m["AWS_ROLE_ARN"] = "http://localhost:4566", "http://localhost:1338", "arn:aws:iam::1:role/erp"
	c = mustLoad(t, m)
	if c.AWS.Getenv == nil {
		t.Fatal("AWS Getenv: got nil, want the environment")
	}
	checkFields(t, []field{
		{"KMS access key", c.KMS.AccessKey, ""},
		{"STS endpoint", c.AWS.STSEndpoint, "http://localhost:4566"},
		{"IMDS endpoint", c.AWS.IMDSEndpoint, "http://localhost:1338"},
		{"AWS_ROLE_ARN", c.AWS.Getenv("AWS_ROLE_ARN"), "arn:aws:iam::1:role/erp"},
	})
}

func TestAuditVerifyInterval(t *testing.T) {
	m := valid()
	if c, err := Load(env(m)); err != nil || c.AuditVerify != time.Hour {
		t.Fatal(c.AuditVerify, err)
	}
	m["ERP_AUDIT_VERIFY_INTERVAL"] = "0"
	if c, err := Load(env(m)); err != nil || c.AuditVerify != 0 {
		t.Fatal(c.AuditVerify, err)
	}
	for _, bad := range []string{"-1h", "often"} {
		t.Run(bad, func(t *testing.T) {
			mm := copyOf(m)
			mm["ERP_AUDIT_VERIFY_INTERVAL"] = bad
			_, err := Load(env(mm))
			pgtest.CheckErr(t, err, "ERP_AUDIT_VERIFY_INTERVAL")
		})
	}
}

func TestRotationPolicyAndPublicURL(t *testing.T) {
	m := valid()
	c := mustLoad(t, m)
	checkFields(t, []field{{"KeyRotationAge", c.KeyRotationAge, time.Duration(0)}, {"PublicURL", c.PublicURL, ""}})
	m["ERP_KEY_ROTATION_AGE"], m["ERP_PUBLIC_URL"] = "8760h", "https://api.example.com/"
	c = mustLoad(t, m)
	checkFields(t, []field{{"KeyRotationAge", c.KeyRotationAge, 8760 * time.Hour}, {"PublicURL", c.PublicURL, "https://api.example.com"}})
	for name, bad := range map[string][2]string{"negative age": {"ERP_KEY_ROTATION_AGE", "-1h"}, "words": {"ERP_KEY_ROTATION_AGE", "yearly"}, "relative url": {"ERP_PUBLIC_URL", "api.example.com"}} {
		t.Run(name, func(t *testing.T) {
			mm := copyOf(m)
			mm[bad[0]] = bad[1]
			_, err := Load(env(mm))
			pgtest.CheckErr(t, err, bad[0])
		})
	}
}

func TestWorkflowEngineSettings(t *testing.T) {
	m := valid()
	c := mustLoad(t, m)
	checkFields(t, []field{
		{"Temporal address", c.Temporal.Address, ""},
		{"Temporal codec key is nil", c.Temporal.CodecKey == nil, true},
		{"Temporal escalate after", c.Temporal.EscalateAfter, 48 * time.Hour},
		{"Temporal escalations", c.Temporal.Escalations, 3},
		{"Temporal task queue", c.Temporal.TaskQueue, "erp"},
	})
	key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("c", 32)))
	m["ERP_TEMPORAL_ADDRESS"], m["ERP_TEMPORAL_CODEC_KEY"], m["ERP_APPROVAL_ESCALATE_AFTER"], m["ERP_APPROVAL_ESCALATIONS"] = "localhost:7233", key, "0", "1"
	c = mustLoad(t, m)
	checkFields(t, []field{
		{"Temporal codec key length", len(c.Temporal.CodecKey), 32},
		{"Temporal escalate after", c.Temporal.EscalateAfter, time.Duration(0)},
		{"Temporal escalations", c.Temporal.Escalations, 1},
	})
	for name, bad := range map[string][2]string{"short key": {"ERP_TEMPORAL_CODEC_KEY", base64.StdEncoding.EncodeToString([]byte("short"))},
		"not base64": {"ERP_TEMPORAL_CODEC_KEY", "%%%"}, "negative period": {"ERP_APPROVAL_ESCALATE_AFTER", "-1h"}, "words": {"ERP_APPROVAL_ESCALATE_AFTER", "soon"},
		"zero escalations": {"ERP_APPROVAL_ESCALATIONS", "0"}, "not a number": {"ERP_APPROVAL_ESCALATIONS", "many"}} {
		t.Run(name, func(t *testing.T) {
			mm := copyOf(m)
			mm[bad[0]] = bad[1]
			_, err := Load(env(mm))
			pgtest.CheckErr(t, err, bad[0])
		})
	}
}

// TestDirectoryErrorsInFixedOrder: with several directory settings invalid,
// every Load reports them in the same order, so the message is stable.
func TestDirectoryErrorsInFixedOrder(t *testing.T) {
	m := valid()
	missing := filepath.Join(t.TempDir(), "missing")
	m["ERP_DIRECTORY_URL"], m["ERP_CELL_REGION"] = "postgres://dir@db/directory", "eu-west"
	m["ERP_BANK_OUTBOX_DIR"], m["ERP_EXTRACT_DIR"], m["ERP_OBJECT_DIR"] = missing+"-bank", missing+"-extract", missing+"-objects"
	_, first := Load(env(m))
	if first == nil {
		t.Fatal("got no error, want the invalid directory settings reported")
	}
	for i := range 50 {
		if _, err := Load(env(m)); err == nil || err.Error() != first.Error() {
			t.Fatalf("load %d: got %v, want %v", i, err, first)
		}
	}
}

// TestEscalationsFitTheCount: the deadline workflow counts escalations in an
// int16 and the column is a smallint, so a value above 32767 is refused at
// load rather than wrapping later.
func TestEscalationsFitTheCount(t *testing.T) {
	m := valid()
	m["ERP_APPROVAL_ESCALATIONS"] = "32767"
	c, err := Load(env(m))
	if err != nil {
		t.Fatalf("32767: got %v, want no error", err)
	}
	if c.Temporal.Escalations != 32767 {
		t.Errorf("32767: got %d escalations, want 32767", c.Temporal.Escalations)
	}
	m["ERP_APPROVAL_ESCALATIONS"] = "32768"
	_, err = Load(env(m))
	pgtest.CheckErr(t, err, "ERP_APPROVAL_ESCALATIONS")
}

// TestProductionFailsClosed: in production (ERP_ENV) the server refuses to
// start with the development token issuer, with a workflow engine but no
// codec key, without object storage (no audit anchors, ADR-0011, and no
// offboarding exports, ADR-0021), or with audit verification switched off;
// development allows all of them, as before. An unknown environment is
// refused, and so is none: there is no default, so a production replica
// whose ERP_ENV went missing does not start with development's allowances.
func TestProductionFailsClosed(t *testing.T) {
	// pgx reads the process's PG* settings, not the injected environment.
	t.Setenv("PGSSLMODE", "")
	codec := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("c", 32)))
	const devRefused, codecRefused = "ERP_DEV_ISSUER must not be set with ERP_ENV=production", "ERP_TEMPORAL_CODEC_KEY is required with ERP_ENV=production"
	const envRequired = "ERP_ENV is required: set ERP_ENV=development on a developer's machine or in tests, ERP_ENV=production everywhere else"
	const storeRefused, verifyRefused = "object storage (ERP_OBJECT_DIR or ERP_OBJECT_S3_*) is required with ERP_ENV=production", "ERP_AUDIT_VERIFY_INTERVAL must not be 0 with ERP_ENV=production"
	// prod is a production configuration that starts: object storage and
	// the default audit verification interval.
	// Production trusts tenants' registered identity providers only, and
	// needs operators to register them (ADR-0031).
	const sharedRefused, operatorsRequired = "ERP_OIDC_ISSUERS must not be set with ERP_ENV=production", "ERP_OPERATOR_ISSUERS is required with ERP_ENV=production"
	operators := `[{"issuer":"https://ops.example.com","audience":"erp-control"}]`
	// Production reaches its databases over verified TLS only (ADR-0033).
	const verified = "postgres://u:p@db.example.com/erp?sslmode=verify-full"
	const dbRefused, dirRefused = "DATABASE_URL: sslmode must be verify-full", "ERP_DIRECTORY_URL: sslmode must be verify-full"
	// Production speaks to NATS over TLS only, as the cell's certificate.
	const natsPlain, natsIdentity = "ERP_NATS_URL must name tls:// servers only", "ERP_NATS_URL needs ERP_TLS_CERT and ERP_TLS_KEY"
	identity := tlsFiles(t)
	noIdentity := map[string]string{"ERP_TLS_CERT": "", "ERP_TLS_KEY": ""}
	// The listeners serve TLS only, as the cell's SPIFFE identity in the
	// trust domain (ADR-0033).
	const listenersIdentity = "ERP_TLS_CERT and ERP_TLS_KEY are required with ERP_ENV=production"
	// Production states how each store encrypts at rest and keeps objects
	// in a KMS-encrypted S3 bucket, never a directory (ADR-0033).
	const statement = "postgres=storage-kms,objects=sse-kms"
	const atRestRequired, objectDirRefused = "ERP_AT_REST is required with ERP_ENV=production", "ERP_OBJECT_DIR must not be set with ERP_ENV=production"
	// The engine is reached as the cell's certificate; providers' endpoints
	// over https only, but for the instance metadata service, which is
	// link-local (ADR-0033).
	const engineIdentity = "ERP_TEMPORAL_ADDRESS needs ERP_TLS_CERT and ERP_TLS_KEY"
	const imdsOnly = "ERP_AWS_IMDS_ENDPOINT must be http://169.254.169.254 or http://[fd00:ec2::254]"
	directory := map[string]string{"ERP_DIRECTORY_URL": verified, "ERP_CELL_API_URL": "https://eu1.example.com", "ERP_CELL_REGION": "eu-west", "ERP_CELL_CONTROL_URL": "https://eu1-admin.example.com"}
	with := func(base map[string]string, set map[string]string) map[string]string {
		m := maps.Clone(base)
		maps.Copy(m, set)
		return m
	}
	prod := func(set map[string]string) map[string]string {
		m := map[string]string{"ERP_ENV": "production", "ERP_OBJECT_S3_ENDPOINT": "https://s3.example.com", "ERP_OBJECT_S3_BUCKET": "erp",
			"ERP_OIDC_ISSUERS": "", "ERP_OPERATOR_ISSUERS": operators, "DATABASE_URL": verified, "ERP_AT_REST": statement, "ERP_TRUST_DOMAIN": "erp.example"}
		maps.Copy(m, identity) // listeners serve TLS as the cell's certificate
		for k, v := range set {
			m[k] = v
		}
		return m
	}
	cases := []struct {
		name    string
		set     map[string]string
		wantErr []string // the errors reported; none means the configuration loads
	}{
		{"no environment", map[string]string{"ERP_ENV": "", "ERP_DEV_ISSUER": "true"}, []string{envRequired}},
		{"development dev issuer", map[string]string{"ERP_ENV": "development", "ERP_DEV_ISSUER": "true"}, nil},
		{"development engine without codec", map[string]string{"ERP_ENV": "development", "ERP_TEMPORAL_ADDRESS": "localhost:7233"}, nil},
		{"development without verification", map[string]string{"ERP_ENV": "development", "ERP_AUDIT_VERIFY_INTERVAL": "0"}, nil},
		{"production", prod(with(identity, map[string]string{"ERP_TEMPORAL_ADDRESS": "temporal:7233", "ERP_TEMPORAL_CODEC_KEY": codec})), nil},
		{"production engine without a certificate", prod(with(noIdentity, map[string]string{"ERP_TEMPORAL_ADDRESS": "temporal:7233", "ERP_TEMPORAL_CODEC_KEY": codec})), []string{engineIdentity}},
		{"production plaintext object store", prod(map[string]string{"ERP_OBJECT_S3_ENDPOINT": "http://minio:9000"}), []string{"ERP_OBJECT_S3_ENDPOINT must be an https:// URL"}},
		{"production plaintext KMS", prod(map[string]string{"ERP_FIELD_KMS_KEY": "alias/erp", "ERP_FIELD_KMS_ENDPOINT": "http://kms.internal"}), []string{"ERP_FIELD_KMS_ENDPOINT must be an https:// URL"}},
		{"production KMS by default", prod(map[string]string{"ERP_FIELD_KMS_KEY": "alias/erp"}), nil},
		{"production plaintext vault", prod(map[string]string{"ERP_VAULT_ADDR": "http://vault:8200", "ERP_VAULT_TOKEN": "t"}), []string{"ERP_VAULT_ADDR must be an https:// URL"}},
		{"production vault", prod(map[string]string{"ERP_VAULT_ADDR": "https://vault:8200", "ERP_VAULT_TOKEN": "t"}), nil},
		{"production plaintext STS", prod(map[string]string{"ERP_AWS_STS_ENDPOINT": "http://sts.internal"}), []string{"ERP_AWS_STS_ENDPOINT must be an https:// URL"}},
		{"production instance metadata over IPv6", prod(map[string]string{"ERP_AWS_IMDS_ENDPOINT": "http://[fd00:ec2::254]"}), nil},
		{"production instance metadata elsewhere", prod(map[string]string{"ERP_AWS_IMDS_ENDPOINT": "http://imds.example.com"}), []string{imdsOnly}},
		{"production instance metadata on another port", prod(map[string]string{"ERP_AWS_IMDS_ENDPOINT": "http://169.254.169.254:8080"}), []string{imdsOnly}},
		{"production instance metadata with credentials", prod(map[string]string{"ERP_AWS_IMDS_ENDPOINT": "http://u:p@169.254.169.254"}), []string{imdsOnly}},
		{"production instance metadata with a query", prod(map[string]string{"ERP_AWS_IMDS_ENDPOINT": "http://169.254.169.254?x=1"}), []string{imdsOnly}},
		{"production instance metadata with a fragment", prod(map[string]string{"ERP_AWS_IMDS_ENDPOINT": "http://169.254.169.254#x"}), []string{imdsOnly}},
		{"development plaintext endpoints", map[string]string{"ERP_ENV": "development", "ERP_OBJECT_S3_ENDPOINT": "http://localhost:9000", "ERP_OBJECT_S3_BUCKET": "erp",
			"ERP_AWS_IMDS_ENDPOINT": "http://localhost:1338", "ERP_TEMPORAL_ADDRESS": "localhost:7233"}, nil},
		{"production without an engine", prod(nil), nil},
		{"production with an object directory", prod(map[string]string{"ERP_OBJECT_S3_ENDPOINT": "", "ERP_OBJECT_S3_BUCKET": "", "ERP_OBJECT_DIR": t.TempDir()}), []string{objectDirRefused}},
		{"production without an encryption statement", prod(map[string]string{"ERP_AT_REST": ""}), []string{atRestRequired}},
		{"production without the database's", prod(map[string]string{"ERP_AT_REST": "objects=sse-kms"}), []string{"ERP_AT_REST must state postgres"}},
		{"production without the objects'", prod(map[string]string{"ERP_AT_REST": "postgres=volume"}), []string{"ERP_AT_REST must state objects=sse-kms"}},
		{"production without the bus's", prod(with(identity, map[string]string{"ERP_NATS_URL": "tls://nats:4222"})), []string{"ERP_AT_REST must state nats"}},
		{"production with the bus's", prod(with(identity, map[string]string{"ERP_NATS_URL": "tls://nats:4222", "ERP_AT_REST": statement + ",nats=jetstream-cipher"})), nil},
		{"production issuers for every tenant", prod(map[string]string{"ERP_OIDC_ISSUERS": `[{"issuer":"https://idp.example.com","audience":"erp-api"}]`}), []string{sharedRefused}},
		{"production without operators", prod(map[string]string{"ERP_OPERATOR_ISSUERS": ""}), []string{operatorsRequired}},
		{"production dev issuer", prod(map[string]string{"ERP_DEV_ISSUER": "true"}), []string{devRefused}},
		{"production engine without codec", prod(map[string]string{"ERP_TEMPORAL_ADDRESS": "localhost:7233"}), []string{codecRefused}},
		{"production both", prod(map[string]string{"ERP_DEV_ISSUER": "true", "ERP_TEMPORAL_ADDRESS": "localhost:7233"}), []string{devRefused, codecRefused}},
		{"production without object storage", prod(map[string]string{"ERP_OBJECT_S3_ENDPOINT": "", "ERP_OBJECT_S3_BUCKET": ""}), []string{storeRefused}},
		{"production without verification", prod(map[string]string{"ERP_AUDIT_VERIFY_INTERVAL": "0"}), []string{verifyRefused}},
		{"production without either", prod(map[string]string{"ERP_OBJECT_S3_ENDPOINT": "", "ERP_OBJECT_S3_BUCKET": "", "ERP_AUDIT_VERIFY_INTERVAL": "0s"}), []string{storeRefused, verifyRefused}},
		{"unknown environment", map[string]string{"ERP_ENV": "staging"}, []string{"must be development or production"}},
		{"development database without TLS", map[string]string{"ERP_ENV": "development", "DATABASE_URL": "postgres://u:p@localhost/erp?sslmode=disable"}, nil},
		{"production database without verified TLS", prod(map[string]string{"DATABASE_URL": "postgres://u:p@db.example.com/erp?sslmode=require"}), []string{dbRefused}},
		{"production database by default", prod(map[string]string{"DATABASE_URL": "postgres://u:p@db.example.com/erp"}), []string{dbRefused}},
		{"production directory", prod(directory), nil},
		{"production directory without verified TLS", prod(with(directory, map[string]string{"ERP_DIRECTORY_URL": "postgres://u:p@dir.example.com/erp?sslmode=prefer"})), []string{dirRefused}},
		{"development NATS without TLS", map[string]string{"ERP_ENV": "development", "ERP_NATS_URL": "nats://localhost:4222"}, nil},
		{"production NATS over mutual TLS", prod(with(identity, map[string]string{"ERP_NATS_URL": "tls://nats-1:4222,tls://nats-2:4222", "ERP_AT_REST": statement + ",nats=volume"})), nil},
		{"production NATS without TLS", prod(with(identity, map[string]string{"ERP_NATS_URL": "tls://nats-1:4222,nats://nats-2:4222"})), []string{natsPlain}},
		{"production NATS unparseable", prod(with(identity, map[string]string{"ERP_NATS_URL": "tls://nats-1:4222,%zz"})), []string{natsPlain}},
		{"production NATS without a certificate", prod(with(noIdentity, map[string]string{"ERP_NATS_URL": "tls://nats-1:4222"})), []string{natsIdentity}},
		{"production without a certificate", prod(noIdentity), []string{listenersIdentity}},
		{"production without a trust domain", prod(map[string]string{"ERP_TRUST_DOMAIN": ""}), []string{"ERP_TRUST_DOMAIN is required with ERP_ENV=production"}},
		{"production plaintext cell URLs", prod(with(directory, map[string]string{"ERP_CELL_API_URL": "http://eu1.internal", "ERP_CELL_CONTROL_URL": "http://eu1-admin.internal:9090"})),
			[]string{"ERP_CELL_API_URL must be an https:// URL", "ERP_CELL_CONTROL_URL must be an https:// URL"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := valid()
			for k, v := range c.set {
				m[k] = v
			}
			cfg, err := Load(env(m))
			if len(c.wantErr) == 0 {
				if err != nil {
					t.Fatalf("got %v, want the configuration to load", err)
				}
				if want := c.set["ERP_ENV"]; cfg.Environment != want {
					t.Errorf("environment: got %q, want %q", cfg.Environment, want)
				}
				return
			}
			if err == nil {
				t.Fatalf("got no error, want %q", c.wantErr)
			}
			for _, want := range c.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("got %v, want it to report %q", err, want)
				}
			}
		})
	}
}

// TestTenantIssuers: the tenant issuers the cell trusts are the development
// issuer and the configured ones.
func TestTenantIssuers(t *testing.T) {
	const one, two = `[{"issuer":"https://idp.example.com","audience":"erp-api"}]`,
		`[{"issuer":"https://a.example.com","audience":"erp-api"},{"issuer":"https://b.example.com","audience":"erp-api"}]`
	for _, c := range []struct {
		name, issuers, dev string
		want               []string
	}{
		{"one issuer", one, "", []string{"https://idp.example.com"}},
		{"development issuer", "", "true", []string{"erp-dev"}},
		{"development and one more", one, "true", []string{"erp-dev", "https://idp.example.com"}},
		{"two issuers", two, "", []string{"https://a.example.com", "https://b.example.com"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := valid()
			m["ERP_OIDC_ISSUERS"], m["ERP_DEV_ISSUER"] = c.issuers, c.dev
			if got := mustLoad(t, m).TenantIssuers(); !slices.Equal(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// tlsFiles are TLS settings naming an authority, a certificate and its
// key written to files of the test.
func tlsFiles(t *testing.T) map[string]string {
	t.Helper()
	ca, dir := tlstest.NewCA(t), t.TempDir()
	leaf := ca.Issue(t, tlstest.Options{})
	return map[string]string{
		"ERP_TLS_CA":   "file:" + tlstest.WriteFile(t, dir, "ca.crt", ca.PEM),
		"ERP_TLS_CERT": "file:" + tlstest.WriteFile(t, dir, "tls.crt", leaf.CertPEM),
		"ERP_TLS_KEY":  "file:" + tlstest.WriteFile(t, dir, "tls.key", leaf.KeyPEM),
	}
}

// TestTLSSettings: the TLS material is read with the configuration, so a
// bundle or certificate that cannot be read stops every command at start,
// and the references are kept for the server to read again while it runs
// (ADR-0033); certificate and key go together. Without material there is
// no store and no client of its own.
func TestTLSSettings(t *testing.T) {
	m := valid()
	files := tlsFiles(t)
	maps.Copy(m, files)
	c := mustLoad(t, m)
	if want := (tlsconf.Source{CA: files["ERP_TLS_CA"], Cert: files["ERP_TLS_CERT"], Key: files["ERP_TLS_KEY"]}); c.TLS != want {
		t.Errorf("TLS: got %+v, want %+v", c.TLS, want)
	}
	if c.TLSMaterial == nil || !c.TLSMaterial.HasCertificate() {
		t.Errorf("TLSMaterial: got %v, want the certificate read", c.TLSMaterial)
	}
	if c.HTTPClient() == nil {
		t.Error("HTTPClient: got nil, want the material's client")
	}
	for _, missing := range []string{"ERP_TLS_CERT", "ERP_TLS_KEY"} {
		t.Run("without "+missing, func(t *testing.T) {
			mm := copyOf(m)
			delete(mm, missing)
			_, err := Load(env(mm))
			pgtest.CheckErr(t, err, "ERP_TLS_CERT and ERP_TLS_KEY go together")
		})
	}
	t.Run("unreadable bundle", func(t *testing.T) {
		mm := copyOf(m)
		mm["ERP_TLS_CA"] = "file:/nonexistent/ca.crt"
		_, err := Load(env(mm))
		pgtest.CheckErr(t, err, "ERP_TLS_CA, ERP_TLS_CERT, ERP_TLS_KEY: tlsconf: CA bundle")
	})
	t.Run("none", func(t *testing.T) {
		c := mustLoad(t, valid())
		if c.TLSMaterial != nil || c.HTTPClient() != nil {
			t.Errorf("without material: got %v, %v, want no store and no client", c.TLSMaterial, c.HTTPClient())
		}
	})
}

// TestAtRest: ERP_AT_REST states how each store encrypts at rest, from a
// fixed vocabulary (ADR-0033); objects=sse-kms needs an S3 store.
func TestAtRest(t *testing.T) {
	for _, c := range []struct {
		name, value string
		s3          bool
		want        AtRest
		wantErr     string
	}{
		{"none", "", false, AtRest{}, ""},
		{"every store", "postgres = storage-kms, objects=sse-kms, nats=volume", true, AtRest{Postgres: "storage-kms", Objects: "sse-kms", NATS: "volume"}, ""},
		{"unknown store", "redis=volume", false, AtRest{}, `ERP_AT_REST: unknown store "redis"`},
		{"unknown encryption", "postgres=rot13", false, AtRest{}, `ERP_AT_REST: postgres=rot13 is not one of storage-kms, volume`},
		{"not a pair", "postgres", false, AtRest{}, `ERP_AT_REST must be "store=encryption,..."`},
		{"stated twice", "postgres=volume,postgres=storage-kms", false, AtRest{}, `ERP_AT_REST states postgres twice`},
		{"KMS objects without S3", "objects=sse-kms", false, AtRest{}, "ERP_AT_REST objects=sse-kms needs ERP_OBJECT_S3_ENDPOINT"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := valid()
			m["ERP_AT_REST"] = c.value
			if c.s3 {
				m["ERP_OBJECT_S3_ENDPOINT"], m["ERP_OBJECT_S3_BUCKET"] = "https://s3.example.com", "erp"
			}
			cfg, err := Load(env(m))
			if c.wantErr != "" {
				pgtest.CheckErr(t, err, c.wantErr)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.AtRest != c.want {
				t.Errorf("AtRest: got %+v, want %+v", cfg.AtRest, c.want)
			}
		})
	}
}

// TestTrustDomain: ERP_TRUST_DOMAIN names the SPIFFE trust domain, a DNS
// name in lower case.
func TestTrustDomain(t *testing.T) {
	for _, c := range []struct {
		name, value, wantErr string
	}{
		{"none", "", ""},
		{"a domain", "erp.example.com", ""}, // with the certificate set below
		{"upper case", "ERP.example", "ERP_TRUST_DOMAIN must be"},
		{"a URL", "spiffe://erp.example", "ERP_TRUST_DOMAIN must be"},
		{"without a certificate", "erp.example", "ERP_TRUST_DOMAIN needs ERP_TLS_CERT"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := valid()
			m["ERP_TRUST_DOMAIN"] = c.value
			if c.name != "without a certificate" {
				maps.Copy(m, tlsFiles(t))
			}
			cfg, err := Load(env(m))
			if c.wantErr != "" {
				pgtest.CheckErr(t, err, c.wantErr)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.TrustDomain != c.value {
				t.Errorf("TrustDomain: got %q, want %q", cfg.TrustDomain, c.value)
			}
		})
	}
}

// TestLoadSchema: the migrations Job's configuration is the database and
// how to log and pool, even in production, with the URL a secret
// reference; anything else it is given is ignored. Production requires
// the database over verified TLS, as the server's configuration does.
func TestLoadSchema(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "url"), []byte("postgres://erp@writer/erp?sslmode=verify-full\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadSchema(env(map[string]string{
		"DATABASE_URL": "file:" + filepath.Join(dir, "url"), "ERP_ENV": Production,
		"ERP_LOG_LEVEL": "warn", "ERP_DB_MAX_CONNS": "2", "ERP_OIDC_ISSUERS": "not json",
	}))
	if err != nil {
		t.Fatalf("load: got %v, want no error", err)
	}
	checkFields(t, []field{
		{"DatabaseURL", c.DatabaseURL, "postgres://erp@writer/erp?sslmode=verify-full"},
		{"Environment", c.Environment, Production},
		{"LogLevel", c.LogLevel, slog.LevelWarn},
		{"DBMaxConns", c.DBMaxConns, int32(2)},
	})
	cases := map[string]struct {
		settings map[string]string
		want     string
	}{
		"no database": {map[string]string{"ERP_ENV": Production}, "DATABASE_URL is required"},
		"unresolved":  {map[string]string{"DATABASE_URL": "file:" + filepath.Join(dir, "missing"), "ERP_ENV": Production}, "DATABASE_URL:"},
		"environment": {map[string]string{"DATABASE_URL": "postgres://x"}, "ERP_ENV"},
		"unverified":  {map[string]string{"DATABASE_URL": "postgres://erp@writer/erp?sslmode=require", "ERP_ENV": Production}, "DATABASE_URL: "},
		"log level":   {map[string]string{"DATABASE_URL": "postgres://x", "ERP_ENV": Development, "ERP_LOG_LEVEL": "loud"}, "ERP_LOG_LEVEL"},
		"pool size":   {map[string]string{"DATABASE_URL": "postgres://x", "ERP_ENV": Development, "ERP_DB_MAX_CONNS": "x"}, "ERP_DB_MAX_CONNS"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadSchema(env(tc.settings))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error: got %v, want one containing %q", err, tc.want)
			}
		})
	}
}
