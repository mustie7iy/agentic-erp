package config

import (
	"encoding/base64"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/ratelimit"
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
