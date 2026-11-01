package db

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestReplicaRegistry: the cell's running replicas (000177, ADR-0038
// rollout) are read by every tenant's activation and written only by
// erp_worker; a row names a version and at least one key.
func TestReplicaRegistry(t *testing.T) {
	need(t)
	ctx := context.Background()
	r := id()
	if _, err := testDB.Exec(ctx, `INSERT INTO config.replica (id, version, revision, config_keys) VALUES ($1, 'v1.0.0', '', '{version}')`, r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = testDB.Exec(ctx, `DELETE FROM config.replica WHERE id = $1`, r) }) // the next test's rows are its own
	for _, tenant := range []string{"a tenant", "another tenant"} {
		t.Run("read by "+tenant, func(t *testing.T) {
			var n int
			mustApp(t, id(), func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM config.replica WHERE id = $1`, r).Scan(&n)
			})
			if n != 1 {
				t.Errorf("rows the application reads: got %d, want 1", n)
			}
		})
	}
	for name, sql := range map[string]string{
		"insert": `INSERT INTO config.replica (id, version, revision, config_keys) VALUES (gen_random_uuid(), 'v1', '', '{version}')`,
		"update": `UPDATE config.replica SET seen_at = now()`,
		"delete": `DELETE FROM config.replica`,
	} {
		t.Run("application may not "+name, func(t *testing.T) {
			failsWith(t, asApp(t, id(), func(tx pgx.Tx) error { return exec(tx, sql) }), "permission denied")
		})
	}
	for name, c := range map[string]struct{ version, keys, want string }{
		"no version": {"", "{version}", "replica_version_check"},
		"no keys":    {"v1", "{}", "replica_config_keys_check"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := testDB.Exec(ctx, `INSERT INTO config.replica (id, version, revision, config_keys) VALUES ($1, $2, '', $3)`, id(), c.version, c.keys)
			failsWith(t, err, c.want)
		})
	}
	t.Run("written by the worker", func(t *testing.T) {
		tx, err := testDB.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		for _, sql := range []string{"SET LOCAL ROLE erp_worker",
			`INSERT INTO config.replica (id, version, revision, config_keys) VALUES (gen_random_uuid(), 'v2', 'abc', '{version}') ON CONFLICT (id) DO UPDATE SET seen_at = now()`,
			`UPDATE config.replica SET seen_at = now() WHERE id = '` + r.String() + `'`,
			`DELETE FROM config.replica WHERE seen_at < now() - interval '5 minutes'`} {
			if err := exec(tx, sql); err != nil {
				t.Fatalf("%s: got %v, want it done", sql, err)
			}
		}
	})
}
