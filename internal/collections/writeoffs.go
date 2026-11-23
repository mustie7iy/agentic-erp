package collections

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/receivables"
)

// WriteOffs reads what a dispute case's write-off (T86d) wrote off of a
// customer invoice's entry, for receivables' bad debt relief
// (receivables.WriteOffs; ADR-0049 As built, bad debt relief): the case
// resolved by the write-off whose entry is named, and the accepted amounts
// of its items on the invoice's entry. A resolved case and its items are
// fixed (dispute_case_item_fixed; the ledger refuses the write-off's
// reversal, requireOwnEntry), so no lock is taken.
type WriteOffs struct{}

// WrittenOff implements receivables.WriteOffs; ok is false when the entry
// is no write-off of an item of the invoice's entry.
func (WriteOffs) WrittenOff(ctx context.Context, writeOffEntry uuid.UUID, invoiceYear int16, invoiceEntry uuid.UUID) (receivables.WrittenOff, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return receivables.WrittenOff{}, false, err
	}
	var w receivables.WrittenOff
	err = tx.QueryRow(ctx, `SELECT c.id, c.write_off_entry_year, c.write_off_entry_number, c.posting_date, sum(i.accepted_amount)::bigint,
			sum(i.accepted_amount_lc)::bigint
		FROM collections.dispute_case c
		JOIN collections.dispute_case_item i ON i.tenant_id = c.tenant_id AND i.dispute_case_id = c.id
		WHERE c.write_off_entry_id = $1 AND c.resolution = 'write_off' AND i.fiscal_year = $2 AND i.journal_entry_id = $3 AND i.accepted_amount > 0
		GROUP BY c.id, c.write_off_entry_year, c.write_off_entry_number, c.posting_date`, writeOffEntry, invoiceYear, invoiceEntry).
		Scan(&w.DisputeCaseID, &w.EntryYear, &w.EntryNumber, &w.PostingDate, &w.Amount, &w.AmountLC)
	if errors.Is(err, pgx.ErrNoRows) {
		return receivables.WrittenOff{}, false, nil
	}
	if err != nil {
		return receivables.WrittenOff{}, false, fmt.Errorf("collections: what write-off %s wrote off of entry %s: %w", writeOffEntry, invoiceEntry, err)
	}
	return w, true, nil
}
