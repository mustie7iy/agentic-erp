package ledger

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Archive reads a company's fiscal year for the records context's
// archiving runs (ADR-0079 §6, T99b): whether the year is closed, and its
// journal entries with their lines. It reads in the caller's transaction
// and changes nothing.
type Archive struct{}

// YearClosed tells whether the company's fiscal year is closed: it has
// posting periods, and none of them, regular or special, in any ledger,
// is open. It holds the year's period rows FOR SHARE until the caller's
// transaction ends, in the order the carry-forward locks them (by year,
// period, the leading ledger first, then by code), so no period is
// reopened, and no entry posted into one, between this check and the
// caller's read of the year's entries (ADR-0008): postings go only into
// open periods and hold their period FOR SHARE, reopening takes it FOR
// UPDATE.
func (Archive) YearClosed(ctx context.Context, legalEntity uuid.UUID, year int16) (bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return false, err
	}
	rows, _ := tx.Query(ctx, `SELECT ps.status FROM ledger.period_status ps JOIN ledger.ledger g ON g.tenant_id = ps.tenant_id AND g.id = ps.ledger_id
		WHERE ps.legal_entity_id = $1 AND ps.fiscal_year = $2
		ORDER BY ps.fiscal_year, ps.period, NOT g.is_leading, g.code FOR SHARE OF ps`, legalEntity, year)
	statuses, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return false, fmt.Errorf("ledger: periods of %s in %d: %w", legalEntity, year, err)
	}
	return len(statuses) > 0 && !slices.Contains(statuses, "open"), nil
}

// ArchivedEntry is a journal entry as an archive holds it, with its lines
// in their order.
type ArchivedEntry struct {
	ID                                     uuid.UUID
	Ledger, DocumentType, DocumentNumber   string
	FiscalYear, FiscalPeriod               int16
	DocumentDate, PostingDate              time.Time
	Currency, LocalCurrency, GroupCurrency string
	RateToLocal, RateToGroup               string // decimal strings, as stored
	Reference, HeaderText                  *string
	SourceContext                          string
	SourceDocumentID, ReversalOfID         *uuid.UUID
	ReversalOfYear                         *int16
	PostedAt                               time.Time
	PostedBy                               uuid.UUID
	Lines                                  []ArchivedLine
}

// ArchivedLine is a line of an archived entry: its account by number and
// its signed amounts (a debit positive) in the entry's, the company's and
// the group's currency.
type ArchivedLine struct {
	LineNo                           int32
	Account                          string
	Amount, AmountLocal, AmountGroup money.Minor
	PartnerID                        *uuid.UUID
	TaxCode                          *string
	Quantity, Unit                   *string
	Text                             *string
}

// Entries calls fn with each journal entry of the company's fiscal year,
// of every ledger, with its lines, by posting date and then id. One
// statement reads them, so they are the year's entries at one moment.
// An error from fn stops the read and is returned.
func (Archive) Entries(ctx context.Context, legalEntity uuid.UUID, year int16, fn func(ArchivedEntry) error) error {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	rows, _ := tx.Query(ctx, `SELECT e.id, l.code, e.document_type, e.document_number, e.fiscal_year, e.fiscal_period, e.document_date, e.posting_date,
			e.currency_tc, e.currency_lc, e.currency_gc, e.rate_tc_lc::text, e.rate_lc_gc::text, e.reference, e.header_text, e.source_context,
			e.source_document_id, e.reversal_of_id, e.reversal_of_year, e.posted_at, e.posted_by,
			jl.line_no, a.number, jl.amount_tc, jl.amount_lc, jl.amount_gc, jl.partner_id, jl.tax_code, jl.quantity::text, jl.uom_code, jl.line_text
		FROM ledger.journal_entry e
		JOIN ledger.ledger l ON l.tenant_id = e.tenant_id AND l.id = e.ledger_id
		JOIN ledger.journal_line jl ON jl.tenant_id = e.tenant_id AND jl.fiscal_year = e.fiscal_year AND jl.journal_entry_id = e.id
		JOIN ledger.gl_account a ON a.tenant_id = jl.tenant_id AND a.id = jl.account_id
		WHERE e.legal_entity_id = $1 AND e.fiscal_year = $2
		ORDER BY e.posting_date, e.id, jl.line_no`, legalEntity, year)
	var cur ArchivedEntry
	var e ArchivedEntry
	var l ArchivedLine
	_, err = pgx.ForEachRow(rows, []any{&e.ID, &e.Ledger, &e.DocumentType, &e.DocumentNumber, &e.FiscalYear, &e.FiscalPeriod, &e.DocumentDate, &e.PostingDate,
		&e.Currency, &e.LocalCurrency, &e.GroupCurrency, &e.RateToLocal, &e.RateToGroup, &e.Reference, &e.HeaderText, &e.SourceContext,
		&e.SourceDocumentID, &e.ReversalOfID, &e.ReversalOfYear, &e.PostedAt, &e.PostedBy,
		&l.LineNo, &l.Account, &l.Amount, &l.AmountLocal, &l.AmountGroup, &l.PartnerID, &l.TaxCode, &l.Quantity, &l.Unit, &l.Text}, func() error {
		if e.ID != cur.ID && cur.ID != uuid.Nil {
			if err := fn(cur); err != nil {
				return err
			}
		}
		if e.ID != cur.ID {
			cur = e
			cur.Lines = nil
		}
		cur.Lines = append(cur.Lines, l)
		return nil
	})
	if err != nil {
		return fmt.Errorf("ledger: journal entries of %s in %d: %w", legalEntity, year, err)
	}
	if cur.ID == uuid.Nil {
		return nil
	}
	if err := fn(cur); err != nil {
		return fmt.Errorf("ledger: journal entries of %s in %d: %w", legalEntity, year, err)
	}
	return nil
}
