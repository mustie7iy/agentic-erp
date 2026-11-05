package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// maxRoleRaces bounds MigrateBeside's retries. Each lost race leaves one
// more of the migrations' roles created (seven in 2026-11), so a database
// loses a few at most; the bound only stops a migration that creates an
// existing role unconditionally from being retried for ever.
const maxRoleRaces = 20

// MigrateBeside is Migrate for a database whose server has other databases
// migrated at the same moment, as every test binary's own database is under
// go test ./... on a new server. Roles are the server's, not a database's,
// and the migration lock is a database's (an advisory lock is scoped to
// one): two databases can both find a role missing (IF NOT EXISTS in
// 000001, 000012, 000014, 000016, 000019) and both create it, and the later
// fails, its migration rolled back. The role then exists, so migrating again
// goes on past it. The migrations themselves are immutable, and the server
// migrates one database at a time, so only the test set-up needs this.
func MigrateBeside(ctx context.Context, conn *pgx.Conn) ([]string, error) {
	return retryRoleRaces(func() ([]string, error) { return Migrate(ctx, conn) })
}

func retryRoleRaces(run func() ([]string, error)) ([]string, error) {
	var all []string
	var err error
	for range maxRoleRaces {
		var applied []string
		applied, err = run()
		all = append(all, applied...)
		if !lostRoleRace(err) {
			return all, err
		}
	}
	return all, fmt.Errorf("migrate: lost the race for a role %d times running: %w", maxRoleRaces, err)
}

// lostRoleRace reports whether err is a CREATE ROLE that another database's
// migration beat: on pg_authid's unique index (unique_violation, 23505)
// when the other was still uncommitted at the CREATE, as duplicate_object
// (42710) when it had committed after the IF NOT EXISTS check. Routine and
// constraint name, not the message, so a translated server still matches.
func lostRoleRace(err error) bool {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return false
	}
	return pg.Code == "23505" && pg.ConstraintName == "pg_authid_rolname_index" ||
		pg.Code == "42710" && pg.Routine == "CreateRole"
}
