package payments

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// StatementPostings names the bank statement lines behind the journal
// entries their postings posted, for a context that reads those entries
// in the ledger: accounts receivable shows a receipt waiting on a house
// bank's incoming payments clearing account with the statement and line
// that brought it (ADR-0063 §6, T75c), where a person finds who paid.
type StatementPostings struct{}

// PostedLine is the statement line an entry was posted for: the
// statement's id and reference and the line's number.
type PostedLine struct {
	StatementID  uuid.UUID
	StatementRef string
	LineNo       int32
}

// Lines returns, for each of the entries (fiscal year and id; LineNo is
// unused) a matched statement line posted, that line, keyed by the
// entry's id; an entry no statement line posted
// is left out. Company access is the caller's to check: it read the
// entries within the companies it may act in.
func (StatementPostings) Lines(ctx context.Context, entries []ledger.ItemRef) (map[uuid.UUID]PostedLine, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	years, ids := make([]int16, len(entries)), make([]uuid.UUID, len(entries))
	for i, e := range entries {
		years[i], ids[i] = e.FiscalYear, e.EntryID
	}
	// An entry is one match's, found by year and id (the index
	// statement_match_journal_entry).
	rows, _ := tx.Query(ctx, `SELECT m.journal_entry_id, s.id, s.statement_ref, m.line_no FROM unnest($1::smallint[], $2::uuid[]) AS x(y, je)
		JOIN payments.statement_match m ON m.journal_entry_year = x.y AND m.journal_entry_id = x.je
		JOIN payments.bank_statement s ON s.tenant_id = m.tenant_id AND s.id = m.statement_id`, years, ids)
	out := map[uuid.UUID]PostedLine{}
	var entry uuid.UUID
	var line PostedLine
	if _, err := pgx.ForEachRow(rows, []any{&entry, &line.StatementID, &line.StatementRef, &line.LineNo}, func() error {
		out[entry] = line
		return nil
	}); err != nil {
		return nil, fmt.Errorf("payments: statement lines of %d journal entries: %w", len(entries), err)
	}
	return out, nil
}
