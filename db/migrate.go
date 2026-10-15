package db

import (
	"context"
	"fmt"
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

// CurrentVersion is the highest applied version, or 0 on an empty database.
func CurrentVersion(ctx context.Context, conn *pgx.Conn) (int, error) {
	var v int
	err := conn.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM public.erp_schema_migration`).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("migrate: current version: %w", err)
	}
	return v, nil
}
