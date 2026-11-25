package ledger

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// ClearingGuard holds a clearing of open items (createClearing) to the
// rules of the context that owns them: receivables refuses one that
// clears a relieved invoice's items against an item that is not the
// invoice's, since only an incoming payment charges the relieved tax
// again (ADR-0049 As built batch 94). It is given the clearing's company,
// the partners its items name and the items, before the clearing takes
// any lock of its own, so it may take its own first (receivables holds
// the customers), in the order every other writer of those items takes
// them.
type ClearingGuard interface {
	GuardClearing(ctx context.Context, legalEntity uuid.UUID, partners []uuid.UUID, items []ItemRef) error
}

// guardClearing asks the guard about the items, naming the partners of
// those found. A line's partner never changes, so they are read without a
// lock; a line not found names none and is refused later (loadItems).
func (e *Endpoints) guardClearing(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID, refs []ItemRef) error {
	years, entries, nos := ItemColumns(refs)
	rows, _ := tx.Query(ctx, `SELECT DISTINCT l.partner_id FROM unnest($1::smallint[], $2::uuid[], $3::int[]) AS x(y, je, n)
		JOIN ledger.journal_line l ON l.fiscal_year = x.y AND l.journal_entry_id = x.je AND l.line_no = x.n
		WHERE l.partner_id IS NOT NULL ORDER BY l.partner_id`, years, entries, nos)
	partners, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return fmt.Errorf("ledger: the partners of %d items to clear: %w", len(refs), err)
	}
	return e.Clearings.GuardClearing(ctx, legalEntity, partners, refs)
}

// ClearingRef names a clearing by its id and its clearing entry's
// document number.
type ClearingRef struct {
	ID             uuid.UUID
	DocumentNumber string
}

// ClearedApart returns the clearings by createClearing (its entries are
// the ledger's own, document type AB) that cleared a line of the entry
// or one of the lines together with an item that is neither, in the
// order they were made. A reset clearing (T75i) clears nothing any more
// and is left out. It takes no lock: its caller holds what orders it
// with the clearings (receivables' customer hold, which createClearing's
// guard takes too).
func (OpenItems) ClearedApart(ctx context.Context, entryYear int16, entryID uuid.UUID, lines []ItemRef) ([]ClearingRef, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	years, entries, nos := ItemColumns(lines)
	// set: the lines named, by the entry or one by one. c: the clearings
	// of the ledger's own entries that clear one of them.
	rows, _ := tx.Query(ctx, `WITH set AS (SELECT * FROM unnest($3::smallint[], $4::uuid[], $5::int[]) AS x(y, je, n))
		SELECT c.id, e.document_number FROM ledger.clearing c
		JOIN ledger.journal_entry e ON e.tenant_id = c.tenant_id AND e.fiscal_year = c.clearing_entry_year AND e.id = c.clearing_entry_id
		WHERE e.source_context = 'ledger' AND e.document_type = $6
			AND EXISTS (SELECT FROM ledger.clearing_item i WHERE i.tenant_id = c.tenant_id AND i.clearing_id = c.id
				AND ((i.fiscal_year = $1 AND i.journal_entry_id = $2) OR (i.fiscal_year, i.journal_entry_id, i.line_no) IN (SELECT * FROM set)))
			AND EXISTS (SELECT FROM ledger.clearing_item o WHERE o.tenant_id = c.tenant_id AND o.clearing_id = c.id
				AND o.journal_entry_id <> c.clearing_entry_id AND NOT (o.fiscal_year = $1 AND o.journal_entry_id = $2)
				AND (o.fiscal_year, o.journal_entry_id, o.line_no) NOT IN (SELECT * FROM set))
		ORDER BY c.created_at, c.id`, entryYear, entryID, years, entries, nos, ClearingDocumentType)
	refs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[ClearingRef])
	if err != nil {
		return nil, fmt.Errorf("ledger: clearings of entry %s apart from its %d other lines: %w", entryID, len(lines), err)
	}
	return refs, nil
}
