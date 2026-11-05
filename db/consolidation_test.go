package db

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestConsolidationPermissions (T68a, migration 000220): the
// consolidation read permission is seeded and is no approval permission;
// the group chart, the mapping and the groups are configuration, so the
// slice adds no table.
func TestConsolidationPermissions(t *testing.T) {
	need(t)
	rows, err := testDB.Query(context.Background(), `SELECT code FROM ref.permission WHERE code LIKE 'consolidation.%' AND NOT is_approval ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(codes, " "), "consolidation.read"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}
