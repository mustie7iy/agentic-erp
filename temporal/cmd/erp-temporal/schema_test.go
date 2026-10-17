package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// adminURL is the test PostgreSQL's superuser (DATABASE_URL, as the
// platform's tests: make db-up).
func adminURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("DATABASE_URL")
	if u == "" {
		t.Fatal("DATABASE_URL is not set: make db-up, then export it")
	}
	return u
}

func exec(t *testing.T, sql string) {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, adminURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }() // the statement's outcome is checked
	if _, err := c.Exec(ctx, sql); err != nil {
		t.Fatal(err)
	}
}

// scratch is a login and two database names of the test's own, dropped
// when it ends: roles are the server's, shared by every test run.
func scratch(t *testing.T, admin string) schemaSettings {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	id := hex.EncodeToString(b)
	s := schemaSettings{adminURL: admin, password: "pw'\\x", user: "erp_temporal_" + id,
		database: "erp_temporal_" + id, visibility: "erp_temporal_vis_" + id}
	t.Cleanup(func() {
		for _, db := range []string{s.database, s.visibility} {
			exec(t, "DROP DATABASE IF EXISTS "+pgx.Identifier{db}.Sanitize()+" WITH (FORCE)")
		}
		exec(t, "DROP ROLE IF EXISTS "+pgx.Identifier{s.user}.Sanitize())
	})
	return s
}

// TestSetupSchema: the login and both databases are made, the schema is
// Temporal's latest, and a second run, with the password changed, changes
// the password and nothing else.
func TestSetupSchema(t *testing.T) {
	s := scratch(t, adminURL(t))
	if err := setupSchema(context.Background(), s); err != nil {
		t.Fatalf("setup: %v", err)
	}
	s.password = "changed"
	if err := setupSchema(context.Background(), s); err != nil {
		t.Fatalf("second setup: %v", err)
	}
	u, err := url.Parse(adminURL(t))
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(s.user, "changed")
	for db, table := range map[string]string{s.database: "executions", s.visibility: "executions_visibility"} {
		u.Path = "/" + db
		ctx := context.Background()
		c, err := pgx.Connect(ctx, u.String())
		if err != nil {
			t.Fatalf("the login with its new password: %v", err)
		}
		var version string
		if err := c.QueryRow(ctx, "SELECT curr_version FROM schema_version WHERE db_name = $1", db).Scan(&version); err != nil {
			t.Errorf("%s: schema version: %v", db, err)
		}
		if version == "" || version == "0.0" {
			t.Errorf("%s: schema version %q, want the embedded latest", db, version)
		}
		var exists bool
		if err := c.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil || !exists {
			t.Errorf("%s: table %s: %v, %v", db, table, exists, err)
		}
		_ = c.Close(ctx) // what the test read is checked
	}
}

// TestSetupSchemaFailures: each step's failure is reported as that step's.
func TestSetupSchemaFailures(t *testing.T) {
	admin := adminURL(t)
	limited := func(t *testing.T, attrs string) string {
		t.Helper()
		s := scratch(t, admin)
		exec(t, "CREATE ROLE "+pgx.Identifier{s.user}.Sanitize()+" LOGIN PASSWORD 'pw' "+attrs)
		u, err := url.Parse(admin)
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword(s.user, "pw")
		return u.String()
	}
	t.Run("connect", func(t *testing.T) {
		s := scratch(t, "postgres://nobody@127.0.0.1:1/x?connect_timeout=1")
		if err := setupSchema(context.Background(), s); err == nil || !strings.Contains(err.Error(), "connect") {
			t.Errorf("error: got %v, want the connection's", err)
		}
	})
	t.Run("login", func(t *testing.T) {
		s := scratch(t, limited(t, ""))
		if err := setupSchema(context.Background(), s); err == nil || !strings.Contains(err.Error(), "login "+s.user) {
			t.Errorf("error: got %v, want the login's", err)
		}
	})
	t.Run("database", func(t *testing.T) {
		s := scratch(t, limited(t, "CREATEROLE"))
		if err := setupSchema(context.Background(), s); err == nil || !strings.Contains(err.Error(), "database "+s.database) {
			t.Errorf("error: got %v, want the database's", err)
		}
	})
	boom := errors.New("boom")
	for name, fail := range map[string]string{"setup": "setup-schema", "update": "update-schema"} {
		t.Run(name, func(t *testing.T) {
			replace(t, &runTool, func(args []string) error {
				if slices.Contains(args, fail) {
					return boom
				}
				return nil
			})
			s := scratch(t, admin)
			if err := setupSchema(context.Background(), s); !errors.Is(err, boom) || !strings.Contains(err.Error(), name) {
				t.Errorf("error: got %v, want the %s step's", err, name)
			}
		})
	}
}

func TestSchemaCommand(t *testing.T) {
	dir := t.TempDir()
	pw := filepath.Join(dir, "pw")
	if err := os.WriteFile(pw, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	verified := "postgres://master@localhost:1/postgres?sslmode=verify-full&connect_timeout=1"
	env := func(admin, password string) func(string) string {
		return func(k string) string {
			return map[string]string{"ERP_TEMPORAL_ADMIN_URL": admin, "ERP_TEMPORAL_DATABASE_PASSWORD": password}[k]
		}
	}
	cases := map[string]struct {
		getenv func(string) string
		code   int
		want   string
	}{
		"no admin URL": {env("", "file:"+pw), 2, "ERP_TEMPORAL_ADMIN_URL is required"},
		"no password":  {env(verified, ""), 2, "ERP_TEMPORAL_DATABASE_PASSWORD is required"},
		"unreadable":   {env(verified, "file:"+filepath.Join(dir, "missing")), 2, "ERP_TEMPORAL_DATABASE_PASSWORD: read"},
		"plaintext":    {env("postgres://master@localhost/postgres?sslmode=disable", "file:"+pw), 2, "sslmode must be verify-full"},
		"fallback":     {env("postgres://master@localhost/postgres?sslmode=prefer", "file:"+pw), 2, "sslmode must be verify-full"},
		"not a URL":    {env("postgres://master@localhost:port/x", "file:"+pw), 2, "must be a postgres:// URL"},
		"bad escape":   {env("postgres://master:a%zz@localhost/postgres?sslmode=verify-full", "file:"+pw), 2, "must be a postgres:// URL"},
		"keywords":     {env("host=writer sslmode=verify-full", "file:"+pw), 2, "must be a postgres:// URL"},
		"two hosts":    {env("postgres://master@a,b/postgres?sslmode=verify-full", "file:"+pw), 2, "one host"},
		"unreachable":  {env(verified, "file:"+pw), 1, "connect"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer
			if got := cli(context.Background(), []string{"setup-schema"}, c.getenv, &stderr); got != c.code {
				t.Errorf("exit code: got %d, want %d (%s)", got, c.code, stderr.String())
			}
			if !strings.Contains(stderr.String(), c.want) {
				t.Errorf("stderr: got %q, want %q in it", stderr.String(), c.want)
			}
		})
	}
	// The schema tool reads the URL's own sslmode, not libpq's environment,
	// so the URL must say it.
	t.Run("sslmode from the environment", func(t *testing.T) {
		t.Setenv("PGSSLMODE", "verify-full")
		var stderr bytes.Buffer
		if got := cli(context.Background(), []string{"setup-schema"}, env("postgres://master@localhost:1/postgres?connect_timeout=1", "file:"+pw), &stderr); got != 2 {
			t.Errorf("exit code: got %d, want 2 (%s)", got, stderr.String())
		}
		if !strings.Contains(stderr.String(), "sslmode must be verify-full") {
			t.Errorf("stderr: got %q, want the URL's sslmode required", stderr.String())
		}
	})
	t.Run("defaults", func(t *testing.T) {
		s, err := loadSchemaSettings(env(verified, "file:"+pw))
		if err != nil {
			t.Fatal(err)
		}
		if s.user != "temporal" || s.database != "temporal" || s.visibility != "temporal_visibility" || s.password != "secret" {
			t.Errorf("settings: got %+v", s)
		}
	})
}

func TestToolArgs(t *testing.T) {
	s := schemaSettings{user: "temporal", password: "pw"}
	verified, err := url.Parse("postgres://m@writer.example/postgres?sslmode=verify-full&sslrootcert=/etc/rds/ca.pem")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(toolArgs(verified, s, "temporal"), " ")
	if !strings.Contains(got, "--ep writer.example --port 5432") || !strings.Contains(got, "--tls --tls-ca-file /etc/rds/ca.pem") {
		t.Errorf("verified: got %q, want the host, the default port and TLS against the URL's CA", got)
	}
	plain, err := url.Parse("postgres://m@localhost:55432/postgres?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(toolArgs(plain, s, "temporal"), " "); strings.Contains(got, "--tls") || !strings.Contains(got, "--port 55432") {
		t.Errorf("plaintext (tests only): got %q", got)
	}
}

// TestSchemaCommandOverTLS: the Job's command, end to end, as production
// runs it: the writer over verified TLS, the schema tool too.
func TestSchemaCommandOverTLS(t *testing.T) {
	dir := t.TempDir()
	ca := newAuthority(t, dir, "rds")
	cert, key := ca.issue(t, dir, "writer", "")
	s := scratch(t, tlsURL(t, adminURL(t), ca, cert, key))
	pw := filepath.Join(dir, "pw")
	if err := os.WriteFile(pw, []byte(s.password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"ERP_TEMPORAL_ADMIN_URL": s.adminURL, "ERP_TEMPORAL_DATABASE_PASSWORD": "file:" + pw,
		"ERP_TEMPORAL_DATABASE_USER": s.user, "ERP_TEMPORAL_DATABASE": s.database, "ERP_TEMPORAL_VISIBILITY_DATABASE": s.visibility,
	}
	var stderr bytes.Buffer
	if got := cli(context.Background(), []string{"setup-schema"}, func(k string) string { return env[k] }, &stderr); got != 0 {
		t.Fatalf("exit code: got %d, want 0 (%s)", got, stderr.String())
	}
}

// TestSetupSchemaURL: a URL setupSchema cannot parse is refused before
// anything connects, without the URL (and its password) in the error.
func TestSetupSchemaURL(t *testing.T) {
	err := setupSchema(context.Background(), schemaSettings{adminURL: "postgres://master:secret@localhost:port/x"})
	if err == nil {
		t.Fatal("got no error, want the URL refused")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("error: got %q, want no password in it", err)
	}
}
