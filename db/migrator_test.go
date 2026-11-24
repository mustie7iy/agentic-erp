package db

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// asMigrator sets the transaction's role to one held to row-level
// security as a migrating role may be (06 §6.4: a cell's migrations run as
// its database's master user, no superuser): neither superuser nor
// BYPASSRLS, a member of erp_owner, with usage and create on every schema,
// as the schemas' owner has. The role is made in the transaction and goes
// with it; its name is random, since roles are the cluster's and other
// runs make theirs alongside.
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
