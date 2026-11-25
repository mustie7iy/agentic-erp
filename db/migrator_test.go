package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// asMigrator sets the transaction's role to the one 06 §6.4 guards
// against: a migrating login that does not bypass row-level security
// (neither superuser nor BYPASSRLS), a member of erp_owner, with usage and
// create on every schema, as the schemas' owner has. The role is made in
// the transaction and goes with it; its name is random, since roles are
// the cluster's and other runs make theirs alongside.
func asMigrator(tx pgx.Tx) error {
	role := pgx.Identifier{"erp_test_migrator_" + strings.ReplaceAll(uuid.NewString(), "-", "")}.Sanitize()
	rows, _ := tx.Query(context.Background(), `SELECT nspname FROM pg_namespace
		WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema' ORDER BY nspname`)
	schemas, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for i, s := range schemas {
		schemas[i] = pgx.Identifier{s}.Sanitize()
	}
	return all(tx, step(`CREATE ROLE `+role+` NOLOGIN NOSUPERUSER NOBYPASSRLS IN ROLE erp_owner`),
		step(`GRANT USAGE, CREATE ON SCHEMA `+strings.Join(schemas, ", ")+` TO `+role), step(`SET LOCAL ROLE `+role))
}

// downRefused asserts that a down migration refuses the rows written in
// the tenant (rather than dropping them, 06 §6.4): as a migrating role
// held to row-level security its guard fails on that security, not on
// seeing no rows, and as the owner it refuses naming want. Nothing is
// kept: the transaction is rolled back.
func downRefused(t *testing.T, down string, tenant uuid.UUID, rows []stmt, want string) {
	t.Helper()
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	written := append([]stmt{step(`SELECT set_config('app.tenant_id', $1, true)`, tenant.String())}, rows...)
	if err := all(tx, append(written, step(`SELECT set_config('app.tenant_id', '', true)`), step(`SAVEPOINT migrator`))...); err != nil {
		t.Fatal(err)
	}
	if err := asMigrator(tx); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), "row-level security")
	if err := exec(tx, `ROLLBACK TO SAVEPOINT migrator`); err != nil {
		t.Fatal(err)
	}
	failsWith(t, exec(tx, down), want)
}
