package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Resetting a clearing (ADR-0063 §6 As built, T75i; SAP FBRA, reset
// cleared items and reverse, from memory): the lines a clearing entry
// cleared open again, kept in ledger.clearing_reset_item with the reset in
// ledger.clearing_reset, and the clearing entry is reversed, so the lines
// it posted (a residual item, a credit on account, the offsets) are of a
// reversed entry and no longer open items. A line is cleared at most once
// at a time; once its clearing is reset it may be cleared again.

func init() {
	problem.RegisterConstraint("clearing_reset_pkey", func() *problem.Problem {
		return problem.Conflict("ledger.clearing_already_reset", "The clearing was reset by another request in the meantime.")
	})
}

// ClearingReset resets the clearing that the clearing entry Entry (its
// LineNo unused) made, and reverses that entry on its own posting date
// with HeaderText (nil: "Reversal of" its number).
type ClearingReset struct {
	Entry      ItemRef
	HeaderText *string
}

// ResetClearing resets the clearing by the entry and reverses the entry
// in the same transaction, in every ledger it was posted to (reverse), on
// its own posting date: a period closed since refuses it, 409
// ledger.period_not_open, and nothing changes. It returns the reversal;
// ClearingReset names the lines opened again.
//
// The entries of the clearing's lines are read first without a lock, to
// name them, then locked in one sorted call before anything else
// (lockEntries), as a clearing and a reversal of any of them take theirs,
// and read again under the locks: an application of a line the entry
// posted (a residual item, a credit on account) that committed meanwhile
// refuses the reset, 409 ledger.clearing_lines_cleared, and so does a
// second reset of the clearing, 409 ledger.clearing_already_reset.
func (p Postings) ResetClearing(ctx context.Context, in ClearingReset) (Entry, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return Entry{}, err
	}
	clearings, entries, err := entryClearings(ctx, tx, in.Entry)
	if err != nil {
		return Entry{}, err
	}
	if err := lockEntries(ctx, tx, false, entries); err != nil {
		return Entry{}, err
	}
	reopened, err := resettable(ctx, tx, in.Entry, clearings)
	if err != nil {
		return Entry{}, err
	}
	if err := resetClearings(ctx, tx, actor, clearings); err != nil {
		return Entry{}, err
	}
	e := p.endpoints()
	orig, err := reversibleEntry(ctx, tx, in.Entry.EntryID, func(Entry) error { return nil }, uuid.Nil) // its lock is held already
	if err != nil {
		return Entry{}, err
	}
	rev, err := e.reverse(ctx, tx, actor, orig, nil, 0, in.HeaderText)
	if err != nil {
		return Entry{}, err
	}
	return rev, recordClearingReset(ctx, orig, rev, clearings[0], reopened)
}

// entryClearings are the clearings the entry made, the leading ledger's
// first and then its copies' in the other ledgers (clearCopies), and the
// entries whose lines they cleared, the entry's own and its copies'
// among them. An entry that made no clearing is refused, 422
// ledger.not_a_clearing_entry.
func entryClearings(ctx context.Context, tx pgx.Tx, entry ItemRef) ([]uuid.UUID, []uuid.UUID, error) {
	rows, _ := tx.Query(ctx, `SELECT c.id FROM ledger.clearing c
		JOIN ledger.journal_entry ce ON ce.tenant_id = c.tenant_id AND ce.fiscal_year = c.clearing_entry_year AND ce.id = c.clearing_entry_id
		WHERE ce.fiscal_year = $1 AND (ce.id = $2 OR ce.posting_group_id = $2)
		ORDER BY ce.id <> $2, c.id`, entry.FiscalYear, entry.EntryID)
	clearings, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, nil, fmt.Errorf("ledger: clearings by entry %s: %w", entry.EntryID, err)
	}
	if len(clearings) == 0 {
		return nil, nil, problem.Unprocessable("ledger.not_a_clearing_entry", "The journal entry made no clearing: there is nothing to reset.")
	}
	rows, _ = tx.Query(ctx, `SELECT DISTINCT journal_entry_id FROM ledger.clearing_item WHERE clearing_id = ANY($1)
		UNION SELECT $2::uuid ORDER BY 1`, clearings, entry.EntryID)
	entries, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, nil, fmt.Errorf("ledger: entries cleared by entry %s: %w", entry.EntryID, err)
	}
	return clearings, entries, nil
}

// resettable checks, under the entries' locks, that the clearings may be
// reset: the first is not reset already, and no line of the entry is
// cleared by another clearing. It returns the lines the first opens
// again, the entry's own left out, by year, entry and line.
func resettable(ctx context.Context, tx pgx.Tx, entry ItemRef, clearings []uuid.UUID) ([]ItemRef, error) {
	var reset, clearedSince bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM ledger.clearing_reset WHERE clearing_id = $1),
			EXISTS (SELECT FROM ledger.clearing_item WHERE fiscal_year = $2 AND journal_entry_id = $3 AND clearing_id <> ALL($4))`,
		clearings[0], entry.FiscalYear, entry.EntryID, clearings).Scan(&reset, &clearedSince); err != nil {
		return nil, fmt.Errorf("ledger: reset the clearing by entry %s: %w", entry.EntryID, err)
	}
	switch {
	case reset:
		return nil, problem.Conflict("ledger.clearing_already_reset", "The clearing by the journal entry is reset already.")
	case clearedSince:
		return nil, problem.Conflict("ledger.clearing_lines_cleared",
			"A line the clearing entry posted, a residual item or a credit on account, was cleared since; reset that clearing first.")
	}
	rows, _ := tx.Query(ctx, `SELECT fiscal_year, journal_entry_id, line_no FROM ledger.clearing_item WHERE clearing_id = $1 AND journal_entry_id <> $2
		ORDER BY fiscal_year, journal_entry_id, line_no`, clearings[0], entry.EntryID)
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[ItemRef])
	if err != nil {
		return nil, fmt.Errorf("ledger: lines cleared by entry %s: %w", entry.EntryID, err)
	}
	return out, nil
}

// resetClearings records each clearing's reset, which moves its items
// into ledger.clearing_reset_item (the trigger clearing_reset_moves_items,
// migration 000364): the lines are open again.
func resetClearings(ctx context.Context, tx pgx.Tx, actor reqctx.Principal, clearings []uuid.UUID) error {
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.clearing_reset (tenant_id, clearing_id, reset_by) SELECT $1, c, $2 FROM unnest($3::uuid[]) AS x(c)`,
		actor.TenantID, actor.ID, clearings); err != nil {
		return fmt.Errorf("ledger: reset %d clearings: %w", len(clearings), err)
	}
	return nil
}

// recordClearingReset audits the reset and publishes ClearingReset in its
// transaction (ADR-0007, ADR-0011), after the reversal's events, with the
// lines opened again.
func recordClearingReset(ctx context.Context, orig, rev Entry, clearing uuid.UUID, reopened []ItemRef) error {
	items := make([]map[string]any, len(reopened))
	for i, r := range reopened {
		items[i] = map[string]any{"fiscal_year": r.FiscalYear, "journal_entry_id": r.EntryID, "line_no": r.LineNo}
	}
	if _, err := outbox.Add(ctx, outbox.Event{Context: "ledger", Name: "clearing_reset", AggregateType: "ledger.clearing", AggregateID: clearing,
		Payload: map[string]any{"clearing_id": clearing, "legal_entity_id": orig.LegalEntityID, "clearing_entry_id": orig.ID,
			"clearing_entry_year": orig.FiscalYear, "clearing_entry_number": orig.DocumentNumber, "reversal_entry_id": rev.ID,
			"reversal_entry_year": rev.FiscalYear, "reversal_entry_number": rev.DocumentNumber, "reversal_posting_date": rev.PostingDate.Format(time.DateOnly),
			"items": items}}); err != nil {
		return err
	}
	return audit.Add(ctx, audit.Entry{Action: "ledger.clearing.reset", ObjectType: "ledger.clearing", ObjectID: clearing, Outcome: "succeeded",
		Detail: map[string]any{"items": len(reopened), "clearing_entry_id": orig.ID, "reversal_entry_id": rev.ID, "reversal_document_number": rev.DocumentNumber}})
}
