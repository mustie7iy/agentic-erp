package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Customer open items as the contexts that work on receivables read them
// (ADR-0063 §1, T75b): the aging (T75e), dunning and the collection
// worklist (T76a, T76c) and the credit exposure (T77a) read one reader of
// the ledger rather than receivables' documents, so a customer item posted
// by a manual or migrated entry counts as an invoice's does.

// ReceivableItem is an open item on a customer reconciliation account of
// the leading ledger: what the customer owes is positive (a debit), a
// credit (a credit memo, a payment on the account) negative.
type ReceivableItem struct {
	ItemRef
	AccountID    uuid.UUID
	PartnerID    uuid.UUID // the customer
	DocumentDate time.Time
	PostingDate  time.Time
	// Amount is in Currency, the entry's transaction currency, whose
	// minor-unit digits are Exponent; AmountLC in LocalCurrency, the
	// company's, with LocalExponent digits, as the item was booked.
	Currency      string
	Exponent      int
	Amount        money.Minor
	LocalCurrency string
	LocalExponent int
	AmountLC      money.Minor
	// Terms are the payment terms the item was posted with (T75b), or nil
	// for an item posted without (a manual or migrated entry): such an
	// item has no due date the ledger keeps (aging.go).
	Terms *ItemTerms
	// SpecialGL is the item's special G/L indicator, or nil for an
	// ordinary item.
	SpecialGL *string
	// Number and Reference are its journal entry's document number and
	// reference; SourceContext and SourceDocumentID the context that
	// posted the entry and its document there (receivables and the
	// customer invoice, for one).
	Number           string
	Reference        *string
	SourceContext    string
	SourceDocumentID *uuid.UUID
}

// NetDueDate is when the item is due by the terms it was posted with; ok
// is false for an item without terms, which has no due date.
func (r ReceivableItem) NetDueDate() (due time.Time, ok bool) {
	if r.Terms == nil {
		return time.Time{}, false
	}
	return r.Terms.NetDueDate(), true
}

// DaysOverdue is how many days past its net due date the item is on the
// date, 0 when it is not yet due (due on the date counts as not due, as
// the open items' aging counts it); ok is false for an item without terms.
// Only on's calendar date counts, whatever its time and zone: a company's
// date (config.CompanyDay) or a key date a request names.
func (r ReceivableItem) DaysOverdue(on time.Time) (days int, ok bool) {
	due, ok := r.NetDueDate()
	if !ok {
		return 0, false
	}
	day := time.Date(on.Year(), on.Month(), on.Day(), 0, 0, 0, 0, time.UTC) // due dates are dates, read at midnight UTC
	return max(int(day.Sub(due).Hours()/24), 0), true
}

// Receivable returns the legal entity's open customer items, of the
// customers named or, without any, of every customer: ordered by customer
// and item, in every currency, with the terms each was posted with.
// Cleared lines and lines of a reversed entry or of a reversal are left
// out, as Payable leaves them out; special G/L items and items an owner
// holds are kept, since the customer owes them all the same. The legal
// entity is not checked against the caller's companies: the caller's
// context does that.
func (OpenItems) Receivable(ctx context.Context, legalEntity uuid.UUID, customers []uuid.UUID) ([]ReceivableItem, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT l.fiscal_year, l.journal_entry_id, l.line_no, l.account_id, l.partner_id, e.document_date, l.posting_date,
			e.currency_tc, ctc.exponent, l.amount_tc, e.currency_lc, clc.exponent, l.amount_lc, l.special_gl, e.document_number, e.reference,
			e.source_context, e.source_document_id, `+termsColumns+`
		FROM ledger.journal_line l
		LEFT JOIN ledger.open_item_terms t ON t.tenant_id = l.tenant_id AND t.fiscal_year = l.fiscal_year AND t.journal_entry_id = l.journal_entry_id
			AND t.line_no = l.line_no
		JOIN ledger.ledger g ON g.tenant_id = l.tenant_id AND g.id = l.ledger_id AND g.is_leading
		JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id AND a.reconciliation_kind = 'customer'
		JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id
		JOIN ref.currency ctc ON ctc.code = e.currency_tc
		JOIN ref.currency clc ON clc.code = e.currency_lc
		WHERE l.legal_entity_id = $1 AND (coalesce(cardinality($2::uuid[]), 0) = 0 OR l.partner_id = ANY($2))
			AND NOT EXISTS (SELECT FROM ledger.clearing_item c WHERE c.tenant_id = l.tenant_id AND c.fiscal_year = l.fiscal_year
				AND c.journal_entry_id = l.journal_entry_id AND c.line_no = l.line_no)
			AND NOT `+reversedLine+`
		ORDER BY l.partner_id, l.fiscal_year, l.journal_entry_id, l.line_no`, legalEntity, customers)
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ReceivableItem, error) {
		var it ReceivableItem
		var t storedTerms
		err := errs.Wrapf(r.Scan(append([]any{&it.FiscalYear, &it.EntryID, &it.LineNo, &it.AccountID, &it.PartnerID, &it.DocumentDate, &it.PostingDate,
			&it.Currency, &it.Exponent, &it.Amount, &it.LocalCurrency, &it.LocalExponent, &it.AmountLC, &it.SpecialGL, &it.Number, &it.Reference,
			&it.SourceContext, &it.SourceDocumentID}, t.fields()...)...), "scan receivable item")
		it.Terms = t.terms()
		return it, err
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: open customer items of %s: %w", legalEntity, err)
	}
	return items, nil
}
