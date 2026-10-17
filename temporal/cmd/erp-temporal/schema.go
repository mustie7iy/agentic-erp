package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	sqltool "go.temporal.io/server/tools/sql"
)

// schemaSettings are setup-schema's: the master credentials' URL, which
// creates the engine's login and databases, and that login's password.
type schemaSettings struct {
	adminURL   string // ERP_TEMPORAL_ADMIN_URL: the writer as the master user, verified TLS
	password   string // ERP_TEMPORAL_DATABASE_PASSWORD
	user       string // ERP_TEMPORAL_DATABASE_USER (default temporal)
	database   string // ERP_TEMPORAL_DATABASE (default temporal)
	visibility string // ERP_TEMPORAL_VISIBILITY_DATABASE (default temporal_visibility)
}

func loadSchemaSettings(getenv func(string) string) (schemaSettings, error) {
	var errs []error
	resolve := func(name string) string {
		v, err := secret(getenv(name))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		} else if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		}
		return v
	}
	or := func(name, def string) string {
		if v := getenv(name); v != "" {
			return v
		}
		return def
	}
	s := schemaSettings{
		adminURL: resolve("ERP_TEMPORAL_ADMIN_URL"), password: resolve("ERP_TEMPORAL_DATABASE_PASSWORD"),
		user: or("ERP_TEMPORAL_DATABASE_USER", "temporal"), database: or("ERP_TEMPORAL_DATABASE", "temporal"),
		visibility: or("ERP_TEMPORAL_VISIBILITY_DATABASE", "temporal_visibility"),
	}
	if s.adminURL != "" {
		if err := requireAdminURL(s.adminURL); err != nil {
			errs = append(errs, fmt.Errorf("ERP_TEMPORAL_ADMIN_URL: %w (ADR-0033)", err))
		}
	}
	return s, errors.Join(errs...)
}

func schemaCmd(ctx context.Context, getenv func(string) string, stderr io.Writer) int {
	s, err := loadSchemaSettings(getenv)
	if err != nil {
		fmt.Fprintln(stderr, "erp-temporal setup-schema: settings:", err)
		return 2
	}
	if err := setupSchema(ctx, s); err != nil {
		fmt.Fprintln(stderr, "erp-temporal setup-schema:", err)
		return 1
	}
	return 0
}

// setupSchema makes the engine's login (its password set each run, so
// that a changed secret takes effect) and databases, then brings each
// database's schema to the version this binary embeds. Every step leaves
// what exists as it is, so running it again changes nothing.
func setupSchema(ctx context.Context, s schemaSettings) error {
	admin, err := url.Parse(s.adminURL)
	if err != nil {
		return errors.New("admin URL: not a URL") // url's error would show the password
	}
	conn, err := pgx.Connect(ctx, s.adminURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }() // closing after the work is best-effort; the work's errors are reported
	if err := ensureLogin(ctx, conn, s.user, s.password); err != nil {
		return err
	}
	for _, db := range []string{s.database, s.visibility} {
		if err := ensureDatabase(ctx, conn, db, s.user); err != nil {
			return err
		}
	}
	for db, schema := range map[string]string{s.database: "postgresql/v12/temporal", s.visibility: "postgresql/v12/visibility"} {
		if err := migrateEngine(admin, s, db, schema); err != nil {
			return err
		}
	}
	return nil
}

// ensureLogin creates the login, or sets its password. Postgres's format
// quotes the name and the password; neither is spliced in by this code.
func ensureLogin(ctx context.Context, conn *pgx.Conn, user, password string) error {
	var stmt string
	err := conn.QueryRow(ctx, `SELECT format(
		CASE WHEN EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1) THEN 'ALTER ROLE %I LOGIN PASSWORD %L' ELSE 'CREATE ROLE %I LOGIN PASSWORD %L' END,
		$1::text, $2::text)`, user, password).Scan(&stmt)
	if err == nil {
		_, err = conn.Exec(ctx, stmt)
	}
	if err == nil {
		// The master user creates databases owned by the login (RDS: a
		// member of the role it hands them to).
		_, err = conn.Exec(ctx, "GRANT "+pgx.Identifier{user}.Sanitize()+" TO CURRENT_USER")
	}
	if err != nil {
		return fmt.Errorf("login %s: %w", user, err)
	}
	return nil
}

func ensureDatabase(ctx context.Context, conn *pgx.Conn, name, owner string) error {
	var exists bool
	err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists)
	if err == nil && !exists {
		_, err = conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" OWNER "+pgx.Identifier{owner}.Sanitize())
	}
	if err != nil {
		return fmt.Errorf("database %s: %w", name, err)
	}
	return nil
}

// runTool is Temporal's schema tool, in process; replaced by tests.
var runTool = sqltool.RunTool

// migrateEngine runs Temporal's own schema tool on one database, as the
// login: version tables first (a no-op once there), then every embedded
// version above the database's.
func migrateEngine(admin *url.URL, s schemaSettings, db, schema string) error {
	conn := toolArgs(admin, s, db)
	if err := runTool(append(conn, "setup-schema", "--version", "0.0")); err != nil {
		return fmt.Errorf("schema %s: setup: %w", db, err)
	}
	if err := runTool(append(conn, "update-schema", "--schema-name", schema)); err != nil {
		return fmt.Errorf("schema %s: update: %w", db, err)
	}
	return nil
}

// toolArgs are the tool's connection to db: the admin URL's host, as the
// login, over TLS verified against the URL's sslrootcert when the URL asks
// for verify-full, as production's must (loadSchemaSettings).
func toolArgs(admin *url.URL, s schemaSettings, db string) []string {
	args := []string{"temporal-sql-tool", "--plugin", "postgres12_pgx", "--ep", admin.Hostname(), "--port", portOf(admin),
		"--user", s.user, "--password", s.password, "--db", db}
	if q := admin.Query(); q.Get("sslmode") == "verify-full" {
		args = append(args, "--tls", "--tls-ca-file", q.Get("sslrootcert"))
	}
	return args
}

func portOf(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	return "5432"
}

// requireAdminURL holds the master URL to what both pgx and Temporal's
// schema tool read alike: a postgres:// URL naming one host, whose own
// sslmode is verify-full, production's rule for the platform's databases
// (ADR-0033). pgx alone would also take a keyword/value string, several
// hosts, and libpq's PGSSLMODE from the environment, none of which reach
// the tool, which is given the URL's host, port and sslmode (toolArgs).
func requireAdminURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return errors.New("must be a postgres:// URL naming one host") // url's error would show the password
	}
	if u.Hostname() == "" || strings.Contains(u.Host, ",") {
		return errors.New("must name one host")
	}
	if u.Query().Get("sslmode") != "verify-full" {
		return errors.New("sslmode must be verify-full, in the URL")
	}
	return nil
}
