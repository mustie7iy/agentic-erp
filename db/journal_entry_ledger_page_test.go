package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestJournalEntryLedgerPageIndexes (T52; migrations 000470-000472): the
// ledger page indexes are on the journal and every partition, valid, and a
// cached statement's generic plan of one ledger's page follows the index
// of its order, the ledger in its index condition and no sort, as the
// journal list asks it (ListJournalEntries' entryFilter).
func TestJournalEntryLedgerPageIndexes(t *testing.T) {
	ctx := context.Background()
	var partitions int
	if err := testDB.QueryRow(ctx, `SELECT count(*) FROM pg_inherits WHERE inhparent = 'ledger.journal_entry'::regclass`).Scan(&partitions); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ index, order, filter string }{
		{"journal_entry_page_ledger_posting", "posting_date, id", ""},
		{"journal_entry_page_ledger_number", "document_number DESC, id DESC", ""},
		{"journal_entry_page_ledger_period", "posting_date, id", " AND fiscal_period = $5"},
	} {
		t.Run(c.index, func(t *testing.T) {
			var attached, invalid int
			if err := testDB.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE NOT i.indisvalid) FROM pg_inherits h
				JOIN pg_index i ON i.indexrelid = h.inhrelid WHERE h.inhparent = ('ledger.' || $1)::regclass`, c.index).Scan(&attached, &invalid); err != nil {
				t.Fatal(err)
			}
			if attached != partitions || invalid != 0 {
				t.Errorf("partition indexes: got %d attached, %d invalid, want %d attached, none invalid", attached, invalid, partitions)
			}
			plan := genericPlan(t, c.index, `SELECT id FROM ledger.journal_entry WHERE tenant_id = $1 AND legal_entity_id = $2 AND ledger_id = $3
				AND fiscal_year = $4`+c.filter+` ORDER BY `+c.order+` LIMIT 51`, c.filter != "")
			if !strings.Contains(plan, "Index Cond") || !strings.Contains(plan, "ledger_id = $3") || strings.Contains(plan, "Sort") {
				t.Errorf("one ledger's page: got plan\n%s\nwant an index scan on the ledger, unsorted", plan)
			}
		})
	}
}

// genericPlan is the generic plan of query, prepared as name, its parameters a tenant, a
// company, a ledger, a fiscal year and, with period, a period, as a
// cached statement uses it after its first runs; sequential scans are
// off, as the journal is empty here and holds a company's years in
// production.
func genericPlan(t *testing.T, name, query string, period bool) string {
	t.Helper()
	ctx := context.Background()
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The prepared statement is the session's, so it goes before the
	// connection returns to the pool; the rollback undoes the settings.
	defer func() { _, _ = tx.Exec(ctx, `DEALLOCATE `+name); _ = tx.Rollback(ctx) }() // read only: nothing to keep, and a failed DEALLOCATE leaves an aborted transaction the rollback ends
	types, args := "uuid, uuid, uuid, smallint", "'"+uuid.NewString()+"', '"+uuid.NewString()+"', '"+uuid.NewString()+"', 2026"
	if period {
		types, args = types+", smallint", args+", 5"
	}
	for _, s := range []string{`SET LOCAL plan_cache_mode = force_generic_plan`, `SET LOCAL enable_seqscan = off`,
		`PREPARE ` + name + ` (` + types + `) AS ` + query} {
		if _, err := tx.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	rows, err := tx.Query(ctx, `EXPLAIN EXECUTE `+name+` (`+args+`)`) // the arguments are this function's own uuids and numbers
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		out = append(out, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain: %v", err)
	}
	return strings.Join(out, "\n")
}
