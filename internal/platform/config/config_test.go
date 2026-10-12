package config

import (
	"context"
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/ratelimit"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets/vaulttest"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

var key = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))

func valid() map[string]string {
	return map[string]string{
		"DATABASE_URL":       "postgres://x",
		"ERP_OIDC_ISSUERS":   `[{"issuer":"https://idp.example.com","audience":"erp-api"}]`,
		"ERP_PAGE_TOKEN_KEY": key,
		"ERP_FIELD_KEY":      key,
	}
}

func TestDefaults(t *testing.T) {
	c, err := Load(env(valid()))
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != ":8080" || c.AdminAddr != ":9090" || c.AppRole != "erp_app" || c.RelayRole != "erp_relay" || c.ProjectorRole != "erp_projector" || c.WorkerRole != "erp_worker" || c.ControlRole != "erp_control" || len(c.OperatorIssuers) != 0 || c.BankOutboxDir != "" || len(c.FieldKey) != 32 || c.Cell != "local" ||
		c.MigrateOnStart || c.LogLevel != slog.LevelInfo || c.ShutdownTimeout != 20*time.Second || len(c.Issuers) != 1 || len(c.PageTokenKey) != 32 ||
		c.RequestTimeout != 30*time.Second || c.DBMaxConns != 0 || c.DevIssuer || c.Objects.Enabled() || c.Objects.Region != "us-east-1" || c.Objects.Retention != 87600*time.Hour || c.TenantQuota != (ratelimit.Quota{Rate: 2000, Burst: 10000}) || c.PrincipalQuota != (ratelimit.Quota{Rate: 50, Burst: 100}) {
		t.Fatalf("%+v", c)
	}
}

func TestOverrides(t *testing.T) {
	m := valid()
	m["ERP_CELL"], m["ERP_MIGRATE_ON_START"], m["ERP_LOG_LEVEL"], m["ERP_SHUTDOWN_TIMEOUT"], m["ERP_NATS_URL"] = "eu1", "true", "debug", "5s", "nats://n"
	m["ANTHROPIC_API_KEY"], m["ERP_BANK_OUTBOX_DIR"], m["ERP_EXTRACT_DIR"] = "key", t.TempDir(), t.TempDir()
	m["ERP_REQUEST_TIMEOUT"], m["ERP_DB_MAX_CONNS"], m["ERP_RATE_LIMIT_TENANT"], m["ERP_RATE_LIMIT_PRINCIPAL"] = "10s", "40", "off", "5"
	c, err := Load(env(m))
	if err != nil || !c.LLMEnabled || c.BankOutboxDir != m["ERP_BANK_OUTBOX_DIR"] || c.ExtractDir != m["ERP_EXTRACT_DIR"] || c.Cell != "eu1" || !c.MigrateOnStart || c.LogLevel != slog.LevelDebug || c.ShutdownTimeout != 5*time.Second || c.NATSURL != "nats://n" ||
		c.RequestTimeout != 10*time.Second || c.DBMaxConns != 40 || c.TenantQuota != (ratelimit.Quota{}) || c.PrincipalQuota != (ratelimit.Quota{Rate: 5, Burst: 5}) {
		t.Fatal(c, err)
	}
	// With the development issuer the identity providers are optional, and still validated when given.
	m = valid()
	delete(m, "ERP_OIDC_ISSUERS")
	m["ERP_DEV_ISSUER"] = "true"
	if c, err := Load(env(m)); err != nil || !c.DevIssuer || len(c.Issuers) != 0 {
		t.Fatal(c, err)
	}
	m["ERP_OIDC_ISSUERS"] = "[]"
	if _, err := Load(env(m)); err == nil {
		t.Fatal("empty issuer list accepted")
	}
	// Master keys: a ring from ERP_FIELD_KEYS, or one master from the legacy key.
	m = valid()
	if c, err := Load(env(m)); err != nil || len(c.FieldKeys) != 1 || c.FieldKeys[0].ID != "v1" || len(c.FieldKeys[0].Secret) != 32 {
		t.Fatal(c, err)
	}
	m["ERP_FIELD_KEYS"] = "k2=" + key + ", k1=" + key
	if c, err := Load(env(m)); err != nil || len(c.FieldKeys) != 2 || c.FieldKeys[0].ID != "k2" || c.FieldKeys[1].ID != "k1" {
		t.Fatal(c, err)
	}
	delete(m, "ERP_FIELD_KEY")
	if c, err := Load(env(m)); err != nil || len(c.FieldKey) != 0 || len(c.FieldKeys) != 2 {
		t.Fatalf("ring without a legacy key: %+v %v", c, err)
	}
	for _, bad := range []string{"k1", "k1=short", "bad id!=" + key, key} {
		m["ERP_FIELD_KEYS"] = bad
		if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "ERP_FIELD_KEYS") {
			t.Fatalf("%s: %v", bad, err)
		}
	}
	// Operator issuers are optional and validated when given.
	m = valid()
	m["ERP_OPERATOR_ISSUERS"], m["ERP_DB_CONTROL_ROLE"] = `[{"issuer":"https://ops.example.com","audience":"erp-control"}]`, "ops_role"
	if c, err := Load(env(m)); err != nil || len(c.OperatorIssuers) != 1 || c.OperatorIssuers[0].Audience != "erp-control" || c.ControlRole != "ops_role" {
		t.Fatal(c, err)
	}
	for _, bad := range []string{"[]", "nope"} {
		m["ERP_OPERATOR_ISSUERS"] = bad
		if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "ERP_OPERATOR_ISSUERS") {
			t.Fatalf("%s: %v", bad, err)
		}
	}
	// Object storage: a directory or an S3 service, complete, never both.
	m = valid()
	m["ERP_OBJECT_DIR"], m["ERP_OBJECT_RETENTION"] = t.TempDir(), "0"
	if c, err := Load(env(m)); err != nil || !c.Objects.Enabled() || c.Objects.Dir != m["ERP_OBJECT_DIR"] || c.Objects.Retention != 0 {
		t.Fatal(c, err)
	}
	m = valid()
	m["ERP_OBJECT_S3_ENDPOINT"], m["ERP_OBJECT_S3_BUCKET"], m["ERP_OBJECT_S3_REGION"] = "http://localhost:9000", "erp", "eu-west-1"
	m["ERP_OBJECT_S3_ACCESS_KEY"], m["ERP_OBJECT_S3_SECRET_KEY"] = "ak", "sk"
	if c, err := Load(env(m)); err != nil || !c.Objects.Enabled() || c.Objects.Bucket != "erp" || c.Objects.Region != "eu-west-1" || c.Objects.SecretKey != "sk" {
		t.Fatal(c, err)
	}
	for name, change := range map[string]func(){
		"both":         func() { m["ERP_OBJECT_DIR"] = t.TempDir() },
		"no bucket":    func() { delete(m, "ERP_OBJECT_S3_BUCKET") },
		"no secret":    func() { delete(m, "ERP_OBJECT_S3_SECRET_KEY") },
		"retention":    func() { m["ERP_OBJECT_RETENTION"] = "soon" },
		"negative":     func() { m["ERP_OBJECT_RETENTION"] = "-1h" },
		"no directory": func() { delete(m, "ERP_OBJECT_S3_ENDPOINT"); m["ERP_OBJECT_DIR"] = "/no/such/dir" },
	} {
		saved := map[string]string{}
		for k, v := range m {
			saved[k] = v
		}
		change()
		if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "ERP_OBJECT") {
			t.Errorf("%s: %v", name, err)
		}
		m = saved
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
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s not reported: %v", want, err)
		}
	}
}

// TestSecretReferences: settings that carry secrets may name where the
// secret lives (env:, file:, vault:) instead of carrying it.
func TestSecretReferences(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "field-key"), []byte(key+"\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "jwt"), []byte(vaulttest.K8sJWT), 0o600)
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
	c, err := Load(env(m))
	if err != nil || c.DatabaseURL != "postgres://erp:pw@db/erp" || len(c.FieldKey) != 32 || len(c.PageTokenKey) != 32 || c.Objects.AccessKey != "AK" ||
		c.Objects.SecretKey != "SK" || c.Vault.Token != vaulttest.Token || c.Secrets == nil {
		t.Fatalf("%+v %v", c, err)
	}
	// The resolver serves connectors too.
	if got, err := c.Secrets.Resolve(context.Background(), "vault:secret/erp/s3#access"); err != nil || got != "AK" {
		t.Fatalf("%q %v", got, err)
	}
	// Kubernetes login with the service account token from a file.
	delete(m, "ERP_VAULT_TOKEN")
	m["ERP_VAULT_K8S_ROLE"], m["ERP_VAULT_K8S_JWT_FILE"] = vaulttest.K8sRole, filepath.Join(dir, "jwt")
	if c, err := Load(env(m)); err != nil || c.Vault.K8sJWT != vaulttest.K8sJWT || c.DatabaseURL != "postgres://erp:pw@db/erp" {
		t.Fatalf("kubernetes: %+v %v", c, err)
	}
	for name, change := range map[string]func(){
		"missing env":        func() { m["ERP_PAGE_TOKEN_KEY"] = "env:NOPE" },
		"missing field":      func() { m["DATABASE_URL"] = "vault:secret/erp/db#nope" },
		"vault without auth": func() { delete(m, "ERP_VAULT_K8S_ROLE") },
		"no jwt file":        func() { m["ERP_VAULT_K8S_JWT_FILE"] = filepath.Join(dir, "none") },
		"bad token ref":      func() { m["ERP_VAULT_TOKEN"] = "env:NOPE" },
		"vault unreachable":  func() { m["ERP_VAULT_ADDR"] = "http://127.0.0.1:1" },
	} {
		saved := map[string]string{}
		for k, v := range m {
			saved[k] = v
		}
		change()
		if _, err := Load(env(m)); err == nil {
			t.Errorf("%s: accepted", name)
		}
		m = saved
	}
	// Without a vault, vault: references cannot be resolved and say so.
	m = valid()
	m["DATABASE_URL"] = "vault:secret/erp/db#url"
	if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") || !strings.Contains(err.Error(), "no resolver") {
		t.Fatalf("vault reference without a vault: %v", err)
	}
	// The default service account token path is used when none is given.
	if DefaultK8sJWTFile != "/var/run/secrets/kubernetes.io/serviceaccount/token" {
		t.Fatal(DefaultK8sJWTFile)
	}
	m = valid()
	m["ERP_VAULT_ADDR"], m["ERP_VAULT_K8S_ROLE"] = vault.URL, vaulttest.K8sRole
	if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "ERP_VAULT_K8S_JWT_FILE") {
		t.Fatalf("default jwt path on this machine: %v", err)
	}
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
	c, err = Load(env(m))
	if err != nil || c.KMS.Name != "eu" || c.KMS.Key != "arn:aws:kms:eu-west-1:111122223333:key/abc" || c.KMS.Region != "eu-west-1" || c.KMS.Endpoint != "http://localhost:4566" || len(c.FieldKeys) != 0 {
		t.Fatalf("%+v %v", c, err)
	}
	for name, bad := range map[string]map[string]string{
		"no credentials": {"ERP_FIELD_KMS_ACCESS_KEY": ""},
		"bad name":       {"ERP_FIELD_KMS_KEY": "no name!=alias/x"},
		"no key":         {"ERP_FIELD_KMS_KEY": "kms="},
	} {
		mm := map[string]string{}
		for k, v := range m {
			mm[k] = v
		}
		for k, v := range bad {
			mm[k] = v
		}
		if _, err := Load(env(mm)); err == nil || !strings.Contains(err.Error(), "ERP_FIELD_KMS_KEY") {
			t.Errorf("%s: %v", name, err)
		}
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
		m["ERP_OFFBOARD_GRACE"] = bad
		if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "ERP_OFFBOARD_GRACE") {
			t.Fatalf("%s: %v", bad, err)
		}
	}
}

func TestDirectorySettings(t *testing.T) {
	m := valid()
	if c, err := Load(env(m)); err != nil || c.DirectoryURL != "" {
		t.Fatal(c.DirectoryURL, err)
	}
	m["ERP_DIRECTORY_URL"] = "postgres://dir@db/directory"
	if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "ERP_CELL_API_URL") {
		t.Fatalf("directory without the cell's URL: %v", err)
	}
	m["ERP_CELL_API_URL"] = "eu-1.api.example.com"
	if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "ERP_CELL_API_URL") {
		t.Fatalf("cell URL without a scheme: %v", err)
	}
	m["ERP_CELL_API_URL"] = "https://eu-1.api.example.com"
	if c, err := Load(env(m)); err != nil || c.DirectoryURL != "postgres://dir@db/directory" || c.CellAPIURL != "https://eu-1.api.example.com" {
		t.Fatalf("%+v %v", c, err)
	}
}
