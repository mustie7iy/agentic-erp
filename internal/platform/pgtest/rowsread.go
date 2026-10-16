package pgtest

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// RowsRead is the number of rows the transaction q belongs to has fetched
// so far, by sequential and index scans, from the tables of schema whose
// name is LIKE table. Tests compare it before and after a query to check
// how much of a table the query reads, which is what the server's actual
// plan does: EXPLAIN plans its query for the parameters given every time,
// so it does not show the generic plan a cached statement ends up with.
func RowsRead(tb testing.TB, ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, schema, table string) int {
	tb.Helper()
	var n int
	if err := q.QueryRow(ctx, `SELECT coalesce(sum(coalesce(seq_tup_read, 0) + coalesce(idx_tup_fetch, 0)), 0)
		FROM pg_stat_xact_user_tables WHERE schemaname = $1 AND relname LIKE $2`, schema, table).Scan(&n); err != nil {
		tb.Fatalf("pgtest: rows read from %s.%s: %v", schema, table, err)
	}
	return n
}
