package db

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
)

// NoTransaction marks a migration that must run outside a transaction, such
// as CREATE INDEX CONCURRENTLY (06-engineering-process §6.4). Such a
// migration holds exactly one statement.
const NoTransaction = "-- migrate:no-transaction"

// PartitionedIndex marks a migration that builds an index on a partitioned
// table without blocking writes (06-engineering-process §6.4): the index
// ON ONLY the parent, then per partition CREATE INDEX CONCURRENTLY and
// ATTACH PARTITION, the parent's index valid once every partition has its
// own. Such a migration holds exactly one CREATE [UNIQUE] INDEX statement
// naming the parent; it runs outside a transaction and may run again after
// a failure, picking up the partitions still without the index.
const PartitionedIndex = "-- migrate:partitioned-index"

// indexStatement is the statement a PartitionedIndex migration holds:
// unique, index name, table schema and name, and the rest (columns, WHERE).
var indexStatement = regexp.MustCompile(`(?s)^CREATE (UNIQUE )?INDEX (\w+) ON (\w+)\.(\w+) (\([^;]+?);?\s*$`)

// indexLockTimeout bounds the waits for the parent's lock when its index
// is created and attached to: a long transaction on the table fails the
// migration, run again later, rather than queueing every posting behind
// it. A variable so tests can shorten it.
var indexLockTimeout = "10s"

// lockKey serializes migrations across replicas starting at the same time.
const lockKey = 7_428_361

// unlockTimeout bounds the unlock: a server that stopped answering must not
// hold up a migration that is done. A variable so tests can shorten it.
var unlockTimeout = 10 * time.Second

// lockPoll is how long a replica waits before it tries the migration lock
// again. A variable so tests can shorten it.
var lockPoll = 100 * time.Millisecond

// lock takes the migration lock, trying again every lockPoll until it is
// free or ctx ends. It never waits inside pg_advisory_lock: a statement
// blocked there is a transaction that CREATE INDEX CONCURRENTLY, run by the
// replica holding the lock, waits to end, and the two deadlock.
func lock(ctx context.Context, conn *pgx.Conn) error {
	for {
		var ok bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", lockKey).Scan(&ok); err != nil {
			return fmt.Errorf("migrate: lock: %w", err)
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("migrate: lock: %w", ctx.Err())
		case <-time.After(lockPoll):
		}
	}
}

// unlock releases the migration lock, giving up after unlockTimeout. A
// failed unlock needs no handling: the session lock ends with the
// connection, and the unlock fails only on a broken connection or a
// deadline, both of which make pgx close it (a pool discards a closed
// connection rather than reuse it).
func unlock(ctx context.Context, conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unlockTimeout)
	defer cancel()
	_, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockKey)
}

// Version is the migration's number, from its NNNNNN_ name prefix.
func (m Migration) Version() (int, error) {
	prefix, _, ok := strings.Cut(m.Name, "_")
	if !ok {
		return 0, fmt.Errorf("migration %q: name must start with a number and _", m.Name)
	}
	v, err := strconv.Atoi(prefix)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("migration %q: bad version prefix", m.Name)
	}
	return v, nil
}

// LatestVersion is the highest migration this binary knows.
func LatestVersion() int {
	ms := Migrations()
	v, _ := ms[len(ms)-1].Version() // embedded names are checked by TestMigrationFiles
	return v
}

// Migrate applies every pending migration in order and returns the names it
// applied. Versions already applied are skipped, including versions newer
// than this binary knows: during a rolling deploy the previous release runs
// against the expanded schema (expand/migrate/contract).
func Migrate(ctx context.Context, conn *pgx.Conn) ([]string, error) {
	return migrate(ctx, conn, Migrations())
}

func migrate(ctx context.Context, conn *pgx.Conn, ms []Migration) (applied []string, err error) {
	if err := lock(ctx, conn); err != nil {
		return nil, err
	}
	defer unlock(ctx, conn)
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS public.erp_schema_migration (
		version integer PRIMARY KEY, name text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, fmt.Errorf("migrate: version table: %w", err)
	}
	done := map[int]bool{}
	rows, _ := conn.Query(ctx, "SELECT version FROM public.erp_schema_migration") // errors surface in CollectRows
	vs, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return nil, fmt.Errorf("migrate: read versions: %w", err)
	}
	for _, v := range vs {
		done[v] = true
	}
	for _, m := range ms {
		v, err := m.Version()
		if err != nil {
			return applied, err
		}
		if done[v] {
			continue
		}
		if err := apply(ctx, conn, m, v); err != nil {
			return applied, fmt.Errorf("migrate: %s: %w", m.Name, err)
		}
		applied = append(applied, m.Name)
	}
	return applied, nil
}

func apply(ctx context.Context, conn *pgx.Conn, m Migration, v int) error {
	record := "INSERT INTO public.erp_schema_migration (version, name) VALUES ($1, $2)"
	if body, ok := strings.CutPrefix(m.Up, PartitionedIndex); ok {
		if err := partitionedIndex(ctx, conn, stripComments(body)); err != nil {
			return fmt.Errorf("build a partitioned index: %w", err)
		}
		_, err := conn.Exec(ctx, record, v, m.Name)
		return errs.Wrapf(err, "record version %d", v)
	}
	if strings.HasPrefix(m.Up, NoTransaction) {
		if _, err := conn.Exec(ctx, m.Up); err != nil {
			return fmt.Errorf("run without a transaction: %w", err)
		}
		_, err := conn.Exec(ctx, record, v, m.Name)
		return errs.Wrapf(err, "record version %d", v)
	}
	return errs.Wrapf(pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Conn().PgConn().Exec(ctx, m.Up).ReadAll(); err != nil {
			return fmt.Errorf("run: %w", err)
		}
		_, err := tx.Exec(ctx, record, v, m.Name)
		return errs.Wrapf(err, "record version %d", v)
	}), "in a transaction")
}

// stripComments drops the -- comment lines of a migration.
func stripComments(sql string) string {
	var kept []string
	for line := range strings.Lines(sql) {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			kept = append(kept, line)
		}
	}
	return strings.TrimSpace(strings.Join(kept, ""))
}

// partitionedIndex builds the index stmt names on its partitioned table,
// one partition at a time (PartitionedIndex). Each partition's index is
// named after the parent's and the partition's suffix (journal_entry_y2026
// gives <index>_y2026). A partition created meanwhile gets the index from
// the parent's; one whose concurrent build failed, leaving an invalid
// index, or whose attach timed out, is built again. The parent's index and
// each attach wait at most indexLockTimeout for the table, holding up new
// postings while they wait.
func partitionedIndex(ctx context.Context, conn *pgx.Conn, stmt string) error {
	m := indexStatement.FindStringSubmatch(stmt)
	if m == nil {
		return fmt.Errorf("want one CREATE [UNIQUE] INDEX name ON schema.table (...) statement, got %q", stmt)
	}
	unique, name, schema, table, rest := m[1], m[2], m[3], m[4], m[5]
	parent := pgx.Identifier{schema, name}.Sanitize()
	// Statements built from the migration's own text, never from input.
	if err := runSQL(ctx, conn, "SET LOCAL lock_timeout = '"+indexLockTimeout+"'; CREATE "+unique+"INDEX IF NOT EXISTS "+pgx.Identifier{name}.Sanitize()+
		" ON ONLY "+pgx.Identifier{schema, table}.Sanitize()+" "+rest); err != nil {
		return err
	}
	rows, _ := conn.Query(ctx, `SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = $1::regclass AND NOT EXISTS (SELECT FROM pg_inherits x JOIN pg_index ix ON ix.indexrelid = x.inhrelid
			WHERE x.inhparent = $2::regclass AND ix.indrelid = i.inhrelid)
		ORDER BY 1`, pgx.Identifier{schema, table}.Sanitize(), parent)
	partitions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("partitions of %s.%s: %w", schema, table, err)
	}
	for _, p := range partitions {
		child := name + "_" + strings.TrimPrefix(p, table+"_")
		qualified := pgx.Identifier{schema, child}.Sanitize()
		if err := runSQL(ctx, conn, "DROP INDEX CONCURRENTLY IF EXISTS "+qualified); err != nil {
			return err
		}
		if err := runSQL(ctx, conn, "CREATE "+unique+"INDEX CONCURRENTLY "+pgx.Identifier{child}.Sanitize()+" ON "+pgx.Identifier{schema, p}.Sanitize()+" "+rest); err != nil {
			return err
		}
		if err := runSQL(ctx, conn, "SET LOCAL lock_timeout = '"+indexLockTimeout+"'; ALTER INDEX "+parent+" ATTACH PARTITION "+qualified); err != nil {
			return err
		}
	}
	return nil
}

// runSQL executes sql, one statement or several in one implicit transaction.
func runSQL(ctx context.Context, conn *pgx.Conn, sql string) error {
	_, err := conn.PgConn().Exec(ctx, sql).ReadAll()
	return errs.Wrapf(err, "%s", sql)
}

// CurrentVersion is the highest applied version, or 0 on an empty database.
func CurrentVersion(ctx context.Context, conn *pgx.Conn) (int, error) {
	var v int
	err := conn.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM public.erp_schema_migration`).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("migrate: current version: %w", err)
	}
	return v, nil
}
