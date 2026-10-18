// Package pgtest gives platform tests a real, migrated PostgreSQL database
// (06-engineering-process §6.6: no mocks for PostgreSQL) and tenant fixtures.
// It is test support and is excluded from the coverage figure.
//
// Use it from TestMain:
//
//	func TestMain(m *testing.M) { pgtest.Main(m) }
//
// Tests that need the database call URL, Pool or NewTenant; without
// DATABASE_URL they are skipped.
package pgtest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/db"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
)

var packageURL string

// Main creates one migrated database for the test binary and drops it after.
func Main(m *testing.M) {
	admin := os.Getenv("DATABASE_URL")
	if admin == "" {
		os.Exit(m.Run())
	}
	ctx := context.Background()
	// Every package's binary starts at once under go test ./..., so the
	// server may be at its connection limit or restarting for a moment;
	// connecting and creating retry for a while before giving up.
	var conn *pgx.Conn
	name := fmt.Sprintf("erp_pkg_%d", time.Now().UnixNano())
	var err error
	for attempt := 1; ; attempt++ {
		if conn, err = pgx.Connect(ctx, admin); err == nil {
			if _, err = conn.Exec(ctx, "CREATE DATABASE "+name); err == nil {
				break
			}
			_ = conn.Close(ctx) // the CREATE failure is the one reported if every attempt fails
		}
		if attempt == 20 {
			fmt.Fprintln(os.Stderr, "pgtest: setting up the package database:", err)
			os.Exit(1)
		}
		time.Sleep(time.Duration(attempt) * 250 * time.Millisecond)
	}
	u, _ := url.Parse(admin)
	u.Path = "/" + name
	packageURL = u.String()
	if err := migrate(ctx, packageURL); err != nil {
		fmt.Fprintln(os.Stderr, "pgtest: migrate:", err)
		os.Exit(1)
	}
	code := m.Run()
	// A database left behind does not change this run's result, but it
	// piles up on the server unnoticed: say so, with its name.
	if _, err := conn.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
		fmt.Fprintf(os.Stderr, "pgtest: dropping the package database %s (drop it by hand): %v\n", name, err)
	}
	_ = conn.Close(ctx) // the process exits next, which ends the session anyway
	os.Exit(code)
}

func migrate(ctx context.Context, u string) error {
	c, err := pgx.Connect(ctx, u)
	if err != nil {
		return fmt.Errorf("pgtest: connect: %w", err)
	}
	defer func() { _ = c.Close(ctx) }() // the work on it is done or has failed; closing loses nothing
	_, err = db.Migrate(ctx, c)
	return err
}

// URL is the package database's URL; the test is skipped without one.
func URL(tb testing.TB) string {
	tb.Helper()
	if packageURL == "" {
		tb.Skip("DATABASE_URL not set")
	}
	return packageURL
}

// Pool opens a pool on the package database, closed when the test ends.
func Pool(tb testing.TB) *database.Pool {
	tb.Helper()
	p, err := database.Open(context.Background(), URL(tb), database.DefaultSettings)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(p.Close)
	return p
}

// Admin is a superuser connection (bypasses row-level security) for fixtures
// and assertions. Each call opens a new connection, held until the test
// ends: take one before a loop or a polling wait, never inside it, or a slow
// run holds hundreds and the database refuses the next client.
func Admin(tb testing.TB) *pgx.Conn {
	tb.Helper()
	c, err := pgx.Connect(context.Background(), URL(tb))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = c.Close(context.Background()) }) // the test is over; closing loses nothing
	return c
}

// Scan runs a superuser query on a connection of its own, closed right
// after, scans its one row into dest and fails the test on error. Unlike
// Admin it holds no connection afterwards, so a test may call it in a loop
// or a polling wait.
func Scan(tb testing.TB, sql string, args []any, dest ...any) {
	tb.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, URL(tb))
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }() // the read is done or has failed; closing loses nothing
	if err := c.QueryRow(ctx, sql, args...).Scan(dest...); err != nil {
		tb.Fatalf("pgtest: %v\n%s", err, sql)
	}
}

// Exec runs fixture SQL as superuser on a connection of its own, closed
// right after, and fails the test on error.
func Exec(tb testing.TB, sql string, args ...any) {
	tb.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, URL(tb))
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }() // the work on it is done or has failed; closing loses nothing
	if _, err := c.Exec(ctx, sql, args...); err != nil {
		tb.Fatalf("pgtest: %v\n%s", err, sql)
	}
}

// Unguarded runs fixture SQL as superuser with the database's triggers
// off (session_replication_role replica), for a state the rules no longer
// let the application reach: a price changed on a released order, as an
// order change will change it.
func Unguarded(tb testing.TB, sql string, args ...any) {
	tb.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, Admin(tb), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			return fmt.Errorf("turn triggers off: %w", err)
		}
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			return fmt.Errorf("run: %w", err)
		}
		return nil
	}); err != nil {
		tb.Fatalf("pgtest: %v\n%s", err, sql)
	}
}

// EmptyDatabase creates a database with nothing in it, dropped when the test
// ends, and returns its URL. A test that needs a database to itself uses
// one: the package database holds what every earlier test, and every
// earlier run under -count, left in it.
func EmptyDatabase(tb testing.TB) string {
	tb.Helper()
	ctx := context.Background()
	// Package binaries run in parallel against one server; a random name
	// cannot collide where a clock reading could.
	name := "erp_empty_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	Exec(tb, "CREATE DATABASE "+name)
	tb.Cleanup(func() {
		c, err := pgx.Connect(ctx, URL(tb))
		if err != nil {
			tb.Errorf("drop %s: %v", name, err)
			return
		}
		defer func() { _ = c.Close(ctx) }() // the drop's outcome is checked; closing the admin connection is best-effort
		if _, err := c.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			tb.Errorf("drop %s: %v", name, err)
		}
	})
	u, _ := url.Parse(URL(tb)) // URL is the package database's, parsed and connected to by Main
	u.Path = "/" + name
	return u.String()
}

// Issuer is the issuer the principals of tests are bound to with their
// subject (ADR-0010): the test identity provider, whose tokens the tests
// mint. A test that inserts a principal itself records it.
func Issuer(tb testing.TB) string {
	tb.Helper()
	return authntest.URL(tb)
}

// Actor is a principal with the token subject that authenticates as it.
type Actor struct {
	ID      uuid.UUID
	Subject string
	Kind    string
}

// Tenant is an active tenant with a human, an agent and a service principal.
// The human holds Permissions; the agent and service hold the ones that are
// not approval permissions.
type Tenant struct {
	ID          uuid.UUID
	Human       Actor
	Agent       Actor
	Service     Actor
	Permissions []string
}

// NewTenant creates a tenant whose principals hold the given permissions.
func NewTenant(tb testing.TB, permissions ...string) Tenant {
	tb.Helper()
	ctx := context.Background()
	c := Admin(tb)
	ten := Tenant{ID: uuid.Must(uuid.NewV7()), Permissions: permissions}
	ten.Human = Actor{ID: uuid.Must(uuid.NewV7()), Kind: "human"}
	ten.Agent = Actor{ID: uuid.Must(uuid.NewV7()), Kind: "agent"}
	ten.Service = Actor{ID: uuid.Must(uuid.NewV7()), Kind: "service"}
	for _, a := range []*Actor{&ten.Human, &ten.Agent, &ten.Service} {
		a.Subject = a.Kind + "|" + a.ID.String()
	}
	must := func(sql string, args ...any) {
		if _, err := c.Exec(ctx, sql, args...); err != nil {
			tb.Fatalf("pgtest: %v\n%s", err, sql)
		}
	}
	must(`INSERT INTO tenancy.tenant (id, name, region, group_currency, status) VALUES ($1, 'Tenant', 'eu-west', 'EUR', 'active')`, ten.ID)
	// Bound to the test identity provider, whose tokens the tests mint
	// (ADR-0010: a principal is its issuer's subject).
	issuer := Issuer(tb)
	for _, a := range []Actor{ten.Human, ten.Agent, ten.Service} {
		must(`INSERT INTO identity.principal (tenant_id, id, kind, external_issuer, external_subject, display_name, status, created_by, updated_by)
			VALUES ($1, $2, $3, $4, $5, $3, 'active', $2, $2)`, ten.ID, a.ID, a.Kind, issuer, a.Subject)
	}
	// The agent writes what its permissions allow, as the agents that
	// existed before autonomy levels were enforced do (000077, ADR-0036).
	must(`INSERT INTO identity.agent_profile (tenant_id, principal_id, autonomy_level, model, description, created_by, updated_by)
		VALUES ($1, $2, 2, 'erp-default', 'Test agent', $3, $3)`, ten.ID, ten.Agent.ID, ten.Human.ID)
	human, machine := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	must(`INSERT INTO identity.role (tenant_id, id, code, name, status, created_by, updated_by) VALUES ($1, $2, 'all', 'All', 'active', $3, $3), ($1, $4, 'machine', 'Machine', 'active', $3, $3)`,
		ten.ID, human, ten.Human.ID, machine)
	for _, p := range permissions {
		must(`INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, created_by) VALUES ($1, gen_random_uuid(), $2, $3, current_date - 1, $4)`,
			ten.ID, human, p, ten.Human.ID)
		must(`INSERT INTO identity.role_permission (tenant_id, id, role_id, permission_code, valid_from, created_by)
			SELECT $1, gen_random_uuid(), $2, code, current_date - 1, $4 FROM ref.permission WHERE code = $3 AND NOT is_approval`,
			ten.ID, machine, p, ten.Human.ID)
	}
	must(`INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by) VALUES ($1, gen_random_uuid(), $2, $3, current_date - 1, $2)`,
		ten.ID, ten.Human.ID, human)
	for _, a := range []Actor{ten.Agent, ten.Service} {
		must(`INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by) VALUES ($1, gen_random_uuid(), $2, $3, current_date - 1, $4)`,
			ten.ID, a.ID, machine, ten.Human.ID)
	}
	return ten
}

// AllCompanies lets the actor act in every legal entity.
func (ten Tenant) AllCompanies(tb testing.TB, a Actor) {
	tb.Helper()
	Exec(tb, `UPDATE identity.principal SET all_companies = true, version = version + 1 WHERE tenant_id = $1 AND id = $2`, ten.ID, a.ID)
}
