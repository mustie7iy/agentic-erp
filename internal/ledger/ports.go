package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Postings posts and reverses journal entries for other contexts
// (payables now, payments later) in the request's transaction, with the
// same checks as manual entries: open period, postable accounts, partners
// that may be posted to, balance in every currency.
type Postings struct {
	LegalEntities LegalEntities
	Partners      Partners
	Numbers       Numbers
}

func (p Postings) endpoints() *Endpoints {
	return &Endpoints{LegalEntities: p.LegalEntities, Partners: p.Partners, Numbers: p.Numbers}
}

// PostingLine is a line to post, in minor units of the entry's currency,
// debit positive.
type PostingLine struct {
	AccountID      uuid.UUID
	Amount         money.Minor
	PartnerID      *uuid.UUID
	TaxCode        *string
	Quantity, Unit *string
	Text           *string
}

// Posting is an entry another context posts for one of its documents.
type Posting struct {
	LegalEntityID    uuid.UUID
	DocumentType     string
	DocumentDate     time.Time
	PostingDate      time.Time
	Currency         string
	Reference        *string
	HeaderText       *string
	SourceContext    string
	SourceDocumentID uuid.UUID
	Lines            []PostingLine
}

// Post posts the entry in the leading ledger.
func (p Postings) Post(ctx context.Context, in Posting) (Entry, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Entry{}, err
	}
	exps, err := exponents(ctx, tx, in.Currency)
	if err != nil {
		return Entry{}, err
	}
	entry := NewEntry{LegalEntityID: in.LegalEntityID, DocumentType: in.DocumentType, DocumentDate: in.DocumentDate, PostingDate: in.PostingDate,
		Currency: in.Currency, Reference: in.Reference, HeaderText: in.HeaderText, SourceContext: in.SourceContext,
		SourceDocumentID: &in.SourceDocumentID, Lines: make([]NewLine, len(in.Lines))}
	for i, l := range in.Lines {
		entry.Lines[i] = NewLine{AccountID: l.AccountID, Amount: l.Amount.Plain(exps[in.Currency]), Currency: in.Currency,
			PartnerID: l.PartnerID, TaxCode: l.TaxCode, Quantity: l.Quantity, Unit: l.Unit, Text: l.Text}
	}
	return p.endpoints().postJournalEntry(ctx, entry)
}

// Reverse posts the reversal of an entry, on the given date or the
// original posting date.
func (p Postings) Reverse(ctx context.Context, entryID uuid.UUID, postingDate *time.Time, headerText *string) (Entry, error) {
	return p.endpoints().reverseJournalEntry(ctx, entryID, func(Entry) error { return nil }, postingDate, headerText)
}

// IsCleared reports whether any line of the entry is cleared. Cleared is
// not paid: a reversal clears the reversed entry's open items too.
func (Postings) IsCleared(ctx context.Context, fiscalYear int16, entryID uuid.UUID) (bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return false, err
	}
	var cleared bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM ledger.clearing_item WHERE fiscal_year = $1 AND journal_entry_id = $2)`, fiscalYear, entryID).Scan(&cleared)
	if err != nil {
		return cleared, fmt.Errorf("ledger: is entry %s cleared: %w", entryID, err)
	}
	return cleared, nil
}

// Determination returns the account automatic postings use for the key
// (ap_reconciliation, input_tax, bank, ...) in the legal entity.
func (Accounts) Determination(ctx context.Context, legalEntity uuid.UUID, key string) (uuid.UUID, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return uuid.Nil, false, err
	}
	var id uuid.UUID
	err = tx.QueryRow(ctx, `SELECT account_id FROM ledger.account_determination WHERE legal_entity_id = $1 AND key = $2`, legalEntity, key).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return id, false, fmt.Errorf("ledger: account determination %s in %s: %w", key, legalEntity, err)
	}
	return id, true, nil
}

// IsPostable reports whether the account is active, set up and not blocked
// for posting in the legal entity, and not a reconciliation account (those
// are posted only through their subledger).
func (Accounts) IsPostable(ctx context.Context, legalEntity, account uuid.UUID) (bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return false, err
	}
	var ok bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM ledger.gl_account a
		JOIN ledger.company_account ca ON ca.tenant_id = a.tenant_id AND ca.account_id = a.id AND ca.legal_entity_id = $1
		WHERE a.id = $2 AND a.status = 'active' AND NOT ca.is_posting_blocked AND a.reconciliation_kind = 'none')`, legalEntity, account).Scan(&ok)
	if err != nil {
		return ok, fmt.Errorf("ledger: is account %s postable in %s: %w", account, legalEntity, err)
	}
	return ok, nil
}

// TaxCode is a tax code valid on a date.
type TaxCode struct {
	Code      string
	Direction string
	RatePct   string // decimal, e.g. "19.000000"
	AccountID uuid.UUID
}

// TaxCode returns the legal entity's tax code valid on the date.
func (Accounts) TaxCode(ctx context.Context, legalEntity uuid.UUID, code string, date time.Time) (TaxCode, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return TaxCode{}, false, err
	}
	t := TaxCode{Code: code}
	err = tx.QueryRow(ctx, `SELECT direction, rate_pct::text, account_id FROM ledger.tax_code
		WHERE legal_entity_id = $1 AND code = $2 AND valid_from <= $3 AND (valid_to IS NULL OR valid_to > $3)`,
		legalEntity, code, date).Scan(&t.Direction, &t.RatePct, &t.AccountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return TaxCode{}, false, nil
	}
	if err != nil {
		return t, false, fmt.Errorf("ledger: tax code %s in %s: %w", code, legalEntity, err)
	}
	return t, true, nil
}

// Calendar answers questions about legal entities' fiscal calendars.
type Calendar struct {
	LegalEntities LegalEntities
}

// FiscalYear is the fiscal year of the legal entity the date falls in; a
// date outside every set-up fiscal year is 422 ledger.no_fiscal_period.
func (c Calendar) FiscalYear(ctx context.Context, legalEntity uuid.UUID, date time.Time) (int16, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return 0, err
	}
	le, ok, err := c.LegalEntities.Entity(ctx, legalEntity)
	if err != nil {
		return 0, err
	}
	var year int16
	if ok {
		err = tx.QueryRow(ctx, `SELECT fiscal_year FROM ledger.fiscal_period WHERE calendar_id = $1 AND NOT is_special AND $2 BETWEEN start_date AND end_date`,
			le.FiscalCalendarID, date).Scan(&year)
	}
	if !ok || errors.Is(err, pgx.ErrNoRows) {
		return 0, problem.Unprocessable("ledger.no_fiscal_period", "No fiscal year of the legal entity covers "+date.Format(time.DateOnly)+
			"; a fiscal year is set up by a number range for it in the configuration.")
	}
	if err != nil {
		return year, fmt.Errorf("ledger: fiscal year of %s on %s: %w", legalEntity, date.Format(time.DateOnly), err)
	}
	return year, nil
}
