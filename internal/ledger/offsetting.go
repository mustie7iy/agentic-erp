package ledger

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// EntryRef names a journal entry.
type EntryRef struct {
	FiscalYear int16
	EntryID    uuid.UUID
}

// Entry is the entry the item is a line of.
func (r ItemRef) Entry() EntryRef { return EntryRef{FiscalYear: r.FiscalYear, EntryID: r.EntryID} }

// OffsettingLine is a line of a supplier document other than what it owes
// the supplier: an expense, asset or tax line, from which the liquidity
// forecast derives the liquidity item of what paying the document pays
// (ADR-0051 §4, T53e3).
type OffsettingLine struct {
	AccountID uuid.UUID
	// Account is the account's number; "" when the reader knows only its id
	// (an unposted invoice's item, which payables reads).
	Account string
	// Amount is in the document's transaction currency, signed as the
	// journal is: a debit positive.
	Amount  money.Minor
	TaxCode string // the line's tax code, "" for none
	// TaxKey is the account key of the tax component whose account the line
	// posts to (input, output, self_output or self_input, as the entry's tax
	// items record it): a tax line; "" for a base line.
	TaxKey string
}

// Offsetting returns the offsetting lines of the entries, keyed by entry,
// in line order: every line but those on a supplier or customer
// reconciliation account (what the document owes or is owed) and those
// on the account of a withholding tax withheld at invoice (owed to the
// authority, not paid to the supplier). A line on the account of one of
// its tax code's components is a tax line (OffsettingLine.TaxKey, from the
// entry's tax items, ADR-0049 §4). An entry with no such line, or none at
// all, is left out.
func (OpenItems) Offsetting(ctx context.Context, entries []EntryRef) (map[EntryRef][]OffsettingLine, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	years, ids := make([]int16, len(entries)), make([]uuid.UUID, len(entries))
	for i, e := range entries {
		years[i], ids[i] = e.FiscalYear, e.EntryID
	}
	rows, _ := tx.Query(ctx, `SELECT l.fiscal_year, l.journal_entry_id, l.account_id, a.number, l.amount_tc, coalesce(l.tax_code, ''),
			coalesce((SELECT min(i.account_key) FROM ledger.tax_item i WHERE i.tenant_id = l.tenant_id AND i.fiscal_year = l.fiscal_year
				AND i.journal_entry_id = l.journal_entry_id AND i.code = l.tax_code AND i.account_id = l.account_id), '')
		FROM unnest($1::smallint[], $2::uuid[]) AS x(y, id)
		JOIN ledger.journal_line l ON l.fiscal_year = x.y AND l.journal_entry_id = x.id
		JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id
		WHERE a.reconciliation_kind NOT IN ('supplier', 'customer')
			AND NOT EXISTS (SELECT FROM ledger.open_item_withholding w WHERE w.tenant_id = l.tenant_id AND w.fiscal_year = l.fiscal_year
				AND w.journal_entry_id = l.journal_entry_id AND w.posting = 'invoice' AND w.account_id = l.account_id)
		ORDER BY l.fiscal_year, l.journal_entry_id, l.line_no`, years, ids)
	out := map[EntryRef][]OffsettingLine{}
	var ref EntryRef
	var line OffsettingLine
	_, err = pgx.ForEachRow(rows, []any{&ref.FiscalYear, &ref.EntryID, &line.AccountID, &line.Account, &line.Amount, &line.TaxCode, &line.TaxKey}, func() error {
		out[ref] = append(out[ref], line)
		return nil
	})
	if err != nil {
		return nil, errs.Wrapf(err, "ledger: offsetting lines of %d entries", len(entries))
	}
	return out, nil
}
