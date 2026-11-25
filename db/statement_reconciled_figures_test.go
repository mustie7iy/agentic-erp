package db

import (
	"testing"
)

// TestStatementReconciledFiguresBackfill (migration 000476, T49): the
// backfill gives every tenant's reconciled statement the sum of its
// excluded lines and the latest posting date of its posted lines' live
// matches (a rejected match's date left out), null without a posted line;
// a statement not reconciled keeps neither; the touch trigger is back on
// afterwards; the down drops both columns.
func TestStatementReconciledFiguresBackfill(t *testing.T) {
	conn := emptyDatabase(t)
	ctx := t.Context()
	up := migrateBefore(t, conn, "000476_statement_reconciled_figures")
	tenants, actor := []string{id().String(), id().String()}, id()
	// Foreign keys are triggers: replica mode leaves the house banks and
	// companies these statements would name out of the scene.
	if _, err := conn.Exec(ctx, `SET session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	const statement = `INSERT INTO payments.bank_statement (tenant_id, id, house_bank_id, legal_entity_id, format, file_sha256, file_position, file_ref,
			statement_key, statement_ref, sequence, account, currency, opening_balance, opening_date, closing_balance, closing_date, line_count,
			status, reconciled_at, reconciled_by, created_by, updated_by, updated_at)
		VALUES ($1, $2, gen_random_uuid(), gen_random_uuid(), 'mt940', sha256($3::bytea), 1, 'f', $3, $3, '1', 'ACC', 'EUR', 0, '2026-04-16', 0,
			'2026-04-17', 2, $4, CASE WHEN $4 = 'reconciled' THEN now() END, CASE WHEN $4 = 'reconciled' THEN $5::uuid END, $5, $5, '2026-01-01')`
	const line = `INSERT INTO payments.bank_statement_line (tenant_id, statement_id, line_no, amount, booking_date, value_date, transaction_code,
			bank_reference, owner_reference, text, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, '2026-04-17', '2026-04-17', 'NTRF', 'R', 'NONREF', '', $5, $6, $6)`
	const match = `INSERT INTO payments.statement_match (tenant_id, id, statement_id, line_no, kind, reason, status, posting_date, created_by, updated_by)
		VALUES ($1, gen_random_uuid(), $2, $3, 'manual', 'r', $4, $5, $6, $6)`
	statements := map[string]string{}
	for _, s := range []struct{ tenant, key, status string }{
		{tenants[0], "mixed", "reconciled"}, {tenants[0], "excluded", "reconciled"}, {tenants[0], "open", "imported"}, {tenants[1], "other", "reconciled"},
	} {
		sid := id().String()
		statements[s.key] = sid
		if _, err := conn.Exec(ctx, statement, s.tenant, sid, s.key, s.status, actor); err != nil {
			t.Fatalf("statement %s: %v", s.key, err)
		}
	}
	for _, l := range []struct {
		tenant, key string
		no          int
		amount      int64
		status      string
		matches     [][2]string // status, posting date
	}{
		{tenants[0], "mixed", 1, -500, "excluded", nil},
		{tenants[0], "mixed", 2, 300, "posted", [][2]string{{"rejected", "2026-06-30"}, {"matched", "2026-05-04"}}},
		{tenants[0], "mixed", 3, 200, "posted", [][2]string{{"matched", "2026-04-17"}}},
		{tenants[0], "excluded", 1, -70, "excluded", nil},
		{tenants[0], "excluded", 2, -30, "excluded", nil},
		{tenants[0], "open", 1, -10, "excluded", nil},
		{tenants[1], "other", 1, 40, "posted", [][2]string{{"matched", "2026-04-20"}}},
	} {
		if _, err := conn.Exec(ctx, line, l.tenant, statements[l.key], l.no, l.amount, l.status, actor); err != nil {
			t.Fatalf("line %s/%d: %v", l.key, l.no, err)
		}
		for _, m := range l.matches {
			if _, err := conn.Exec(ctx, match, l.tenant, statements[l.key], l.no, m[0], m[1], actor); err != nil {
				t.Fatalf("match %s/%d: %v", l.key, l.no, err)
			}
		}
	}
	if _, err := conn.Exec(ctx, `SET session_replication_role = DEFAULT`); err != nil {
		t.Fatal(err)
	}
	if _, err := migrate(ctx, conn, []Migration{up}); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"mixed": "-500 2026-05-04 2026-01-01", "excluded": "-100 - 2026-01-01", "open": "- - 2026-01-01",
		"other": "0 2026-04-20 2026-01-01"} {
		var got string
		if err := conn.QueryRow(ctx, `SELECT coalesce(reconciled_excluded::text, '-') || ' ' || coalesce(reconciled_posted_until::text, '-') || ' '
			|| updated_at::date FROM payments.bank_statement WHERE id = $1`, statements[key]).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("statement %s's figures and updated day: got %s, want %s", key, got, want)
		}
	}
	var enabled string
	if err := conn.QueryRow(ctx, `SELECT tgenabled FROM pg_trigger WHERE tgname = 'bank_statement_touch'`).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled != "O" {
		t.Errorf("bank_statement_touch after the backfill: got %q, want enabled (O)", enabled)
	}
	if _, err := conn.Exec(ctx, up.Down); err != nil {
		t.Fatalf("down: %v", err)
	}
	var columns int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema = 'payments' AND table_name = 'bank_statement'
		AND column_name IN ('reconciled_excluded', 'reconciled_posted_until')`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 0 {
		t.Errorf("columns after the down: got %d, want none", columns)
	}
}
