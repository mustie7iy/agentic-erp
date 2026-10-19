package drverify

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// A posted entry balances in all three currencies at commit (the deferred
// trigger ledger.check_entry_balanced), so an unbalanced one or a ledger
// that does not net to zero means the restore lost or tore rows: a
// partition, a page, a half-applied transaction. The sums are bigint, as
// the amounts are (money.Minor, ADR-0005); one that overflows fails the
// query rather than wrapping.

// ledger counts the tenant's journal entries and reports every entry that
// does not balance and every legal entity's ledger whose trial balance
// does not net to zero.
func ledger(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, r *Report) error {
	var n int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger.journal_entry WHERE tenant_id = $1`, tenant).Scan(&n); err != nil {
		return fmt.Errorf("count journal entries: %w", err)
	}
	r.JournalEntries += n
	rows, _ := tx.Query(ctx, `SELECT fiscal_year, journal_entry_id, sum(amount_tc)::bigint, sum(amount_lc)::bigint, sum(amount_gc)::bigint
		FROM ledger.journal_line WHERE tenant_id = $1
		GROUP BY fiscal_year, journal_entry_id
		HAVING sum(amount_tc) <> 0 OR sum(amount_lc) <> 0 OR sum(amount_gc) <> 0
		ORDER BY 1, 2`, tenant)
	var (
		year         int16
		id, ledgerID uuid.UUID
		tc, lc, gc   money.Minor
	)
	if _, err := pgx.ForEachRow(rows, []any{&year, &id, &tc, &lc, &gc}, func() error {
		r.problem("tenant %s: journal entry %s (%d) unbalanced: tc %d, lc %d, gc %d", tenant, id, year, tc, lc, gc)
		return nil
	}); err != nil {
		return fmt.Errorf("balance journal entries: %w", err)
	}
	rows, _ = tx.Query(ctx, `SELECT legal_entity_id, ledger_id, sum(amount_lc)::bigint, sum(amount_gc)::bigint
		FROM ledger.journal_line WHERE tenant_id = $1
		GROUP BY legal_entity_id, ledger_id
		HAVING sum(amount_lc) <> 0 OR sum(amount_gc) <> 0
		ORDER BY 1, 2`, tenant)
	if _, err := pgx.ForEachRow(rows, []any{&id, &ledgerID, &lc, &gc}, func() error {
		r.problem("tenant %s: legal entity %s, ledger %s: trial balance lc %d, gc %d, want 0", tenant, id, ledgerID, lc, gc)
		return nil
	}); err != nil {
		return fmt.Errorf("trial balances: %w", err)
	}
	return nil
}
