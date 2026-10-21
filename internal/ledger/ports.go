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
	SpecialGL      *string // A for a down payment on the down payment account (ADR-0037)
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
	// GRIR moves purchase order items' GR/IR positions (T33): the ledger
	// books each item's GR/IR line and its counter line next to Lines.
	GRIR []GRIRItem
	// Terms are the payment terms of the entry's supplier open items, kept
	// with them (ledger.open_item_terms, T36b); nil for none.
	Terms *ItemTerms
	// Withholding are the withholding taxes of the entry's supplier open
	// item (T46f), kept with it (ledger.open_item_withholding): what was
	// withheld at invoice is among Lines already, on the code's account.
	Withholding []Withholding
}

// ItemTerms are the payment terms an open item was posted with, as SAP
// keeps ZTERM, ZFBDT and ZBD1T..ZBD3T in the vendor line: the payment
// term's code, the baseline date the days count from, the net days and
// up to two cash discount tiers, earliest first.
type ItemTerms struct {
	PaymentTermCode string
	BaselineDate    time.Time
	NetDays         int
	Discounts       []Discount
}

// Discount is a cash discount tier: Percent (a plain decimal) off for
// payment within Days of the baseline date.
type Discount struct {
	Days    int
	Percent string
}

// NetDueDate is when the item is due without discount.
func (t ItemTerms) NetDueDate() time.Time { return t.BaselineDate.AddDate(0, 0, t.NetDays) }

// Until is the last day a payment earns the discount.
func (t ItemTerms) Until(d Discount) time.Time { return t.BaselineDate.AddDate(0, 0, d.Days) }

// termsColumns are an open item's terms as storedTerms scans them, from
// ledger.open_item_terms joined as t.
const termsColumns = `t.payment_term_code, t.baseline_date, t.net_days, t.discount_days, trim_scale(t.discount_pct)::text, t.discount2_days,
	trim_scale(t.discount2_pct)::text`

// storedTerms are the terms columns of an item, null when it has none.
type storedTerms struct {
	code         *string
	baseline     *time.Time
	netDays      *int
	days1, days2 *int
	pct1, pct2   *string
}

func (s *storedTerms) fields() []any {
	return []any{&s.code, &s.baseline, &s.netDays, &s.days1, &s.pct1, &s.days2, &s.pct2}
}

// terms are the stored terms, or nil without them.
func (s storedTerms) terms() *ItemTerms {
	if s.code == nil {
		return nil
	}
	t := &ItemTerms{PaymentTermCode: *s.code, BaselineDate: *s.baseline, NetDays: *s.netDays}
	if s.days1 != nil { // a tier's days and percent are null together (open_item_terms_pairs)
		t.Discounts = append(t.Discounts, Discount{Days: *s.days1, Percent: *s.pct1})
	}
	if s.days2 != nil {
		t.Discounts = append(t.Discounts, Discount{Days: *s.days2, Percent: *s.pct2})
	}
	return t
}

// keepTerms keeps the terms with the entry's supplier open items: each
// line on a supplier reconciliation account but special G/L items, which
// no run pays (ADR-0037). Terms for an entry without
// one are refused, rather than kept for nothing.
func keepTerms(ctx context.Context, tx pgx.Tx, entry Entry, terms *ItemTerms) error {
	if terms == nil {
		return nil
	}
	if len(terms.Discounts) > 2 {
		return fmt.Errorf("ledger: terms of entry %s: %d discount tiers, an item holds two", entry.ID, len(terms.Discounts))
	}
	var days [2]*int
	var pct [2]*string
	for i, d := range terms.Discounts {
		days[i], pct[i] = &d.Days, &d.Percent
	}
	tag, err := tx.Exec(ctx, `INSERT INTO ledger.open_item_terms (tenant_id, fiscal_year, journal_entry_id, line_no, payment_term_code, baseline_date,
			net_days, discount_days, discount_pct, discount2_days, discount2_pct)
		SELECT l.tenant_id, l.fiscal_year, l.journal_entry_id, l.line_no, $3, $4, $5, $6, $7::numeric, $8, $9::numeric
		FROM ledger.journal_line l JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id AND a.reconciliation_kind = 'supplier'
		WHERE l.fiscal_year = $1 AND l.journal_entry_id = $2 AND l.special_gl IS NULL`,
		entry.FiscalYear, entry.ID, terms.PaymentTermCode, terms.BaselineDate, terms.NetDays, days[0], pct[0], days[1], pct[1])
	if err != nil {
		return fmt.Errorf("ledger: keep the terms of entry %s: %w", entry.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("ledger: terms of entry %s, which has no supplier open item", entry.ID)
	}
	return nil
}

// GRIRItem moves one purchase order item's GR/IR position (T33). The
// ledger books the GR/IR line, on the legal entity's gr_ir account, so
// that the item's balance afterwards is the order price of its net
// quantity, invoiced less received, and the counter line beside it.
type GRIRItem struct {
	PurchaseOrderID uuid.UUID
	POItemNo        int32
	// Quantity is signed like the GR/IR line, debit positive: the
	// received quantity negated on a goods receipt (101), positive on its
	// reversal (102), the invoiced quantity on an invoice.
	Quantity, Unit string
	// UnitPrice is the order price per PriceUnit, in minor units of the
	// entry's currency (the order's).
	UnitPrice money.Minor
	PriceUnit int32
	// A goods movement names the order item's account (expense or stock),
	// whose line mirrors the GR/IR line in all three currencies. An invoice
	// names its item's net amount instead: what it differs from the GR/IR
	// line goes to the price_difference account.
	OffsetAccountID *uuid.UUID
	Invoiced        *money.Minor
	Text            *string
}

// Post posts the entry in the leading ledger; one with GR/IR items books
// their lines too (postGRIR).
func (p Postings) Post(ctx context.Context, in Posting) (Entry, error) {
	if len(in.GRIR) > 0 {
		return p.postGRIR(ctx, in)
	}
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
			PartnerID: l.PartnerID, TaxCode: l.TaxCode, Quantity: l.Quantity, Unit: l.Unit, Text: l.Text, SpecialGL: l.SpecialGL}
	}
	posted, err := p.endpoints().postJournalEntry(ctx, entry)
	if err != nil {
		return Entry{}, err
	}
	return posted, keepItemData(ctx, tx, posted, in)
}

// keepItemData keeps the payment terms and withholding taxes of a posted
// entry's supplier open item.
func keepItemData(ctx context.Context, tx pgx.Tx, entry Entry, in Posting) error {
	if err := keepTerms(ctx, tx, entry, in.Terms); err != nil {
		return err
	}
	return keepWithholding(ctx, tx, entry, in.Withholding)
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

// Balances returns the leading ledger's balance of the account in the
// legal entity's local currency, debit positive, from the entries posted
// on or before each of the dates, in their order: what a house bank's main
// bank account holds on its statements' closing dates (ADR-0047, T49h).
// The ledger posts no year-end carry-forward entries, so every year's
// lines are summed.
func (Accounts) Balances(ctx context.Context, legalEntity, account uuid.UUID, dates []time.Time) ([]money.Minor, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT coalesce((SELECT sum(l.amount_lc) FROM ledger.journal_line l
			WHERE l.legal_entity_id = $1 AND l.account_id = $2 AND l.posting_date <= x.d
				AND l.ledger_id = (SELECT id FROM ledger.ledger WHERE is_leading)), 0)::bigint
		FROM unnest($3::date[]) WITH ORDINALITY AS x(d, n) ORDER BY x.n`, legalEntity, account, dates)
	out, err := pgx.CollectRows(rows, pgx.RowTo[money.Minor])
	if err != nil {
		return nil, fmt.Errorf("ledger: balances of account %s in %s: %w", account, legalEntity, err)
	}
	return out, nil
}

// IsPostable reports whether the account is active, set up and not blocked
// for posting in the legal entity, and neither a reconciliation account
// (those are posted only through their subledger) nor the legal entity's
// GR/IR account (posted only by goods receipts and invoices, T33).
func (Accounts) IsPostable(ctx context.Context, legalEntity, account uuid.UUID) (bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return false, err
	}
	var ok bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM ledger.gl_account a
		JOIN ledger.company_account ca ON ca.tenant_id = a.tenant_id AND ca.account_id = a.id AND ca.legal_entity_id = $1
		WHERE a.id = $2 AND a.status = 'active' AND NOT ca.is_posting_blocked AND a.reconciliation_kind = 'none'
			AND NOT EXISTS (SELECT FROM ledger.account_determination d WHERE d.tenant_id = a.tenant_id AND d.legal_entity_id = $1
				AND d.key = 'gr_ir' AND d.account_id = a.id))`, legalEntity, account).Scan(&ok)
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

// WithholdingTaxCode is a legal entity's active withholding tax code
// (T46e): a rate of the invoice's net or gross, deducted when the invoice
// posts or when it is paid, owed on the account.
type WithholdingTaxCode struct {
	Code      string
	RatePct   string // decimal, e.g. "15.000"
	Base      string // config.WithholdingNet or config.WithholdingGross
	AccountID uuid.UUID
	Posting   string // config.WithholdingAtInvoice or config.WithholdingAtPayment
}

// WithholdingTaxCode returns the legal entity's active withholding tax code.
func (Accounts) WithholdingTaxCode(ctx context.Context, legalEntity uuid.UUID, code string) (WithholdingTaxCode, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return WithholdingTaxCode{}, false, err
	}
	w := WithholdingTaxCode{Code: code}
	err = tx.QueryRow(ctx, `SELECT rate_pct::text, base, account_id, posting FROM ledger.withholding_tax_code
		WHERE legal_entity_id = $1 AND code = $2 AND is_active`, legalEntity, code).Scan(&w.RatePct, &w.Base, &w.AccountID, &w.Posting)
	if errors.Is(err, pgx.ErrNoRows) {
		return WithholdingTaxCode{}, false, nil
	}
	if err != nil {
		return WithholdingTaxCode{}, false, fmt.Errorf("ledger: withholding tax code %s in %s: %w", code, legalEntity, err)
	}
	return w, true, nil
}

// IsWithholdingTaxCode reports whether the code is an active withholding
// tax code of the legal entity (the partner context's check, T46e).
func (a Accounts) IsWithholdingTaxCode(ctx context.Context, legalEntity uuid.UUID, code string) (bool, error) {
	_, ok, err := a.WithholdingTaxCode(ctx, legalEntity, code)
	return ok, err
}
