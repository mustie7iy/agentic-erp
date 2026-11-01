package directory_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/directory"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// databaseName is the name of the database u names, unquoted.
func databaseName(t *testing.T, u string) string {
	t.Helper()
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(parsed.Path, "/")
}

// publicHolds is PUBLIC's CONNECT and TEMPORARY on the databases named,
// as "CONNECT on name".
func publicHolds(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, names []string) ([]string, error) {
	rows, _ := q.Query(ctx, `SELECT p || ' on ' || datname FROM pg_database, unnest(ARRAY['CONNECT', 'TEMPORARY']) p
		WHERE datname = ANY($1) AND has_database_privilege('public', oid, p) ORDER BY 1`, names)
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// mustHold is publicHolds on a connection outside any transaction the
// test holds, failing the test on error.
func mustHold(ctx context.Context, t *testing.T, c *pgx.Conn, names []string) []string {
	t.Helper()
	held, err := publicHolds(ctx, c, names)
	if err != nil {
		t.Fatal(err)
	}
	return held
}

// TestSetupTakesPUBLICsRightsOnTheServer: on the server's own postgres
// and template1, which PUBLIC may connect to from initdb (postgres's ACL
// is null, its default; template1's PUBLIC CONNECT is explicit), Setup
// with ServerDatabases leaves PUBLIC neither right; rdsadmin, RDS's, is
// not there and passed over. The test server is every package's, whose
// tests connect to postgres as logins of their own, so the run is rolled
// back once checked.
func TestSetupTakesPUBLICsRightsOnTheServer(t *testing.T) {
	ctx := context.Background()
	u := pgtest.EmptyDatabase(t)
	pool, err := database.Open(ctx, u, database.Settings{MaxConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	server := directory.ServerDatabases()
	errDone := errors.New("rolled back")
	err = directory.SetupIn(ctx, func(ctx context.Context, scope database.Scope, fn func(context.Context, pgx.Tx) error) error {
		return pool.InTx(ctx, scope, func(ctx context.Context, tx pgx.Tx) error {
			// No t.Fatal in here: it would leave the transaction, and its
			// locks on the server's pg_database rows, open; errors roll it
			// back.
			want := []string{"CONNECT on postgres", "CONNECT on template1", "TEMPORARY on postgres"}
			got, err := publicHolds(ctx, tx, server)
			if err != nil {
				return fmt.Errorf("before: %w", err)
			}
			if !slices.Equal(got, want) {
				return fmt.Errorf("before: got %v, want a fresh server's %v", got, want)
			}
			if err := fn(ctx, tx); err != nil {
				return err
			}
			if got, err = publicHolds(ctx, tx, server); err != nil {
				return fmt.Errorf("after: %w", err)
			}
			if len(got) > 0 {
				return fmt.Errorf("after: got PUBLIC holding %v, want nothing", got)
			}
			return errDone
		})
	}, server, nil)
	if !errors.Is(err, errDone) {
		t.Fatalf("got %v, want the run checked and rolled back", err)
	}
}

// TestSetupAgainOnTheServer: Setup takes PUBLIC's rights on databases of
// the test's own standing in for the server's, so a login no longer
// connects to them, passes over one not there and the directory's own,
// writes nothing to pg_database on a second run, and takes a right given
// to PUBLIC again back on the next.
func TestSetupAgainOnTheServer(t *testing.T) {
	ctx := context.Background()
	c := pgtest.NewLogin(t) // before the databases, so dropped after them
	u, plain, template := pgtest.EmptyDatabase(t), pgtest.EmptyDatabase(t), pgtest.EmptyDatabase(t)
	others := []string{databaseName(t, plain), databaseName(t, template)}
	// As initdb leaves template1: PUBLIC's CONNECT alone, explicit.
	pgtest.Exec(t, `REVOKE TEMPORARY ON DATABASE `+dbName(t, template)+` FROM PUBLIC`)
	login := directory.Login{Name: c.Name, Password: c.Password, Role: directory.CellRole}
	all := append([]string{"erp_absent_database", databaseName(t, u)}, others...)
	if err := directory.Setup(ctx, u, all, login); err != nil {
		t.Fatalf("setup: %v", err)
	}
	admin := pgtest.Admin(t)
	if got := mustHold(ctx, t, admin, others); len(got) > 0 {
		t.Errorf("after setup: got PUBLIC holding %v, want nothing", got)
	}
	if got, want := mustHold(ctx, t, admin, []string{databaseName(t, u)}), []string{"CONNECT on " + databaseName(t, u)}; !slices.Equal(got, want) {
		t.Errorf("the directory's own: got PUBLIC holding %v, want %v", got, want)
	}
	for _, db := range []string{plain, template} {
		conn, err := pgx.Connect(ctx, c.As(db))
		if err == nil {
			_ = conn.Close(ctx) // the connection itself is the failure reported
		}
		if pg, ok := errors.AsType[*pgconn.PgError](err); !ok || pg.Code != "42501" {
			t.Errorf("the login connecting to %s: got %v, want it refused (42501)", databaseName(t, db), err)
		}
	}
	versions := func() []string {
		rows, _ := admin.Query(ctx, `SELECT xmin::text FROM pg_database WHERE datname = ANY($1) ORDER BY datname`, others)
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	before := versions()
	if err := directory.Setup(ctx, u, all, login); err != nil {
		t.Fatalf("again: %v", err)
	}
	if got := versions(); !slices.Equal(got, before) {
		t.Errorf("pg_database's rows after a second run: got versions %v, want %v unchanged", got, before)
	}
	pgtest.Exec(t, `GRANT CONNECT ON DATABASE `+dbName(t, plain)+` TO PUBLIC`)
	if err := directory.Setup(ctx, u, all, login); err != nil {
		t.Fatalf("after a grant by hand: %v", err)
	}
	if got := mustHold(ctx, t, admin, others); len(got) > 0 {
		t.Errorf("after a grant by hand: got PUBLIC holding %v, want it taken back", got)
	}
}

// TestSetupRefusesPUBLICsRightsItCannotTake: run as an owner of the
// directory's database who is no superuser and does not own the server's
// other database, as the RDS master user need not, Setup's REVOKE only
// warns, and Setup refuses, naming the database and each right PUBLIC
// keeps.
func TestSetupRefusesPUBLICsRightsItCannotTake(t *testing.T) {
	ctx := context.Background()
	owner := pgtest.NewLogin(t) // before the databases, so dropped after them
	u, other := pgtest.EmptyDatabase(t), pgtest.EmptyDatabase(t)
	if err := directory.Setup(ctx, pgtest.EmptyDatabase(t), nil); err != nil { // the privilege roles exist
		t.Fatal(err)
	}
	// The password is a UUID NewLogin made, no value from outside.
	pgtest.Exec(t, `CREATE ROLE `+pgx.Identifier{owner.Name}.Sanitize()+` LOGIN PASSWORD '`+owner.Password+`'`)
	pgtest.Exec(t, `ALTER DATABASE `+dbName(t, u)+` OWNER TO `+pgx.Identifier{owner.Name}.Sanitize())
	name := databaseName(t, other)
	err := directory.Setup(ctx, owner.As(u), []string{name})
	want := "PUBLIC still holds CONNECT on database " + name + ", TEMPORARY on database " + name
	if !errors.Is(err, directory.ErrServer) || !strings.Contains(err.Error(), want) {
		t.Errorf("got %v, want ErrServer naming %q", err, want)
	}
}
