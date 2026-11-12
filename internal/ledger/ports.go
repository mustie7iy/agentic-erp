package ledger

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tax"
)

// Postings posts and reverses journal entries for other contexts
// (payables now, payments later) in the request's transaction, with the
// same checks as manual entries: open period, postable accounts, partners
// that may be posted to, balance in every currency.
type Postings struct {
	LegalEntities LegalEntities
	Partners      Partners
	Numbers       Numbers
	// TaxReturns holds a tax date a person chose (Posting.TaxDateChosen)
	// to the periods no tax return reports yet (ADR-0049 P6).
	TaxReturns TaxReturns
	// Settings reads the active configuration's asset clearing accounts
	// (ADR-0057 §6): an invoiced or cleared order item on one of them is
	// an asset's, and what is paid for it beyond the order price belongs
	// to the asset's cost (GRIRItem.ItemAccountID). Read only for a
	// posting whose items name their account.
	Settings Settings
	// CostObjects answers for the internal orders lines name
	// (PostingLine.InternalOrderID, ADR-0064 §2): wired by the controlling
	// context, the one context whose postings name orders (T60e). nil: a
	// line naming an order is an error, never posted unchecked.
	CostObjects CostObjects
}

func (p Postings) endpoints() *Endpoints {
	return &Endpoints{LegalEntities: p.LegalEntities, Partners: p.Partners, Numbers: p.Numbers, TaxReturns: p.TaxReturns, Settings: p.Settings,
		CostObjects: p.CostObjects}
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
	// PaymentMethod is the payment method this line's supplier open item
	// fixes, where the entry's items do not share one: a migration's
	// opening entry holds the open items of many suppliers, each with the
	// method SAP fixed on it or none (ADR-0038 As built, a migrated item's
	// fixed payment method). Only on an ordinary line of a supplier
	// reconciliation account, in an entry without Posting.PaymentMethod
	// and without GR/IR items, whose lines the ledger numbers
	// (keepLineMethods). nil: the entry's, if it fixes one.
	PaymentMethod *string
	// LC is the line's local amount when it is not the posting date's
	// conversion: a tax line's, converted at the rate of the tax date
	// (ADR-0049 §8). Its group amount is the local one converted at the
	// posting date's rate, and what kept amounts leave unbalanced is an
	// exchange difference (fx_gain or fx_loss). nil: converted as usual.
	// A line with an Amount of 0 posts only when it keeps a local amount
	// other than 0: an exchange difference a source system posted in local
	// currency alone (Posting.Rate).
	LC *money.Minor
	// GC is the line's group amount kept as given, beside LC (ADR-0057
	// §4, T57c): a value booked earlier carried over unchanged, as an
	// asset acquisition leaves the clearing line it capitalises at its
	// booked amounts in all three currencies. Only with LC; nil: the
	// local amount converted at the posting date's rate.
	GC *money.Minor
	// The assignment the document gives the line (ADR-0050 §5): an order
	// or invoice item's cost center, a profit center for a P&L line
	// without one; nil: derived by the ledger.
	CostCenterID, ProfitCenterID, SegmentID *uuid.UUID
	// InternalOrderID is the internal order the line's cost is posted to
	// (ADR-0064 §2), instead of a cost center: an order's settlement
	// credits the order (T60e). Checked as a manual entry's is (422
	// ledger.internal_order_invalid), through Postings.CostObjects.
	InternalOrderID *uuid.UUID
	// TradingPartner is the tenant's company on the other side of the line
	// (ADR-0059 §4): a legal entity of the tenant other than the entry's
	// (422 ledger.trading_partner_invalid). nil: Posting.TradingPartner.
	TradingPartner *uuid.UUID
}

// PostingTaxItem is a tax item a context calculated (ADR-0049 §5, T61d):
// a code's component with its base, tax and recoverable part in the
// entry's currency and in local currency. The ledger writes it with the
// component's account, jurisdiction and the rate of the tax date.
type PostingTaxItem struct {
	Code, Component              string
	Base, Tax, Recoverable       money.Minor
	BaseLC, TaxLC, RecoverableLC money.Minor
}

// Posting is an entry another context posts for one of its documents.
type Posting struct {
	LegalEntityID uuid.UUID
	DocumentType  string
	DocumentDate  time.Time
	PostingDate   time.Time
	// TaxDate is the date the entry's tax codes' rates apply on and its
	// tax is reported in (ADR-0049 §4): a supplier invoice's invoice date;
	// nil: the posting date.
	TaxDate *time.Time
	// TaxDateChosen says a person chose TaxDate (a supplier invoice's
	// tax date set by hand, P6): an entry with tax is then refused when
	// a tax return already reports the date's period (holdTaxDate).
	TaxDateChosen bool
	// TaxItems are the entry's tax items as the context calculated them
	// (T61d); nil: the ledger derives them from the lines (T61c).
	TaxItems         []PostingTaxItem
	Currency         string
	Reference        *string
	HeaderText       *string
	SourceContext    string
	SourceDocumentID uuid.UUID
	Lines            []PostingLine
	// GRIR moves purchase order items' GR/IR positions (T33): the ledger
	// books each item's GR/IR line and its counter line next to Lines.
	GRIR []GRIRItem
	// Collective says the entry gathers lines of documents that have
	// nothing to do with each other, as a migration's opening entry does
	// (balances of accounts beside open vendor items): each partner line
	// still takes its partner's trading partner, but none is given to the
	// entry's other lines, which would make every balance intercompany
	// when its vendors happen to name one company (ADR-0059 As built).
	Collective bool
	// Terms are the payment terms of the entry's supplier open items, kept
	// with them (ledger.open_item_terms, T36b); nil for none.
	Terms *ItemTerms
	// Withholding are the withholding taxes of the entry's supplier open
	// item (T46f), kept with it (ledger.open_item_withholding): what was
	// withheld at invoice is among Lines already, on the code's account.
	Withholding []Withholding
	// PaymentMethod is the payment method the document fixes for the
	// entry's supplier open items (ADR-0038, SAP BSEG-ZLSCH), kept with
	// them (ledger.open_item_payment_method): a payment run pays them by
	// it alone. nil: none, paid by the company's run order.
	PaymentMethod *string
	// Rate is the rate from the entry's currency into the local one that a
	// source system converted the lines at, recorded on the entry instead
	// of the average rate valid on the posting date: a document migrated
	// in its document currency, each line keeping the local amount the
	// source system posted (PostingLine.LC; ADR-0005 As built). A line
	// that keeps none is converted at it. nil: the average rate. Only an
	// entry in a foreign currency takes one (postingRates, 422 otherwise);
	// one with GR/IR items only when it takes positions over
	// (GRIRItem.TakesOverLC).
	Rate *big.Rat
	// LedgerID posts the entry to that one ledger its company keeps (422
	// ledger.ledger_not_kept otherwise), as a manual entry's ledger_id
	// does: a ledger-specific posting, refused on a supplier or customer
	// reconciliation account (ADR-0057 §4, T57f: a depreciation run posts
	// in its ledger alone). nil: every ledger the company keeps.
	LedgerID *uuid.UUID
	// TradingPartner is the trading partner of each of Lines that names
	// none of its own (PostingLine.TradingPartner): the company a
	// document's intercompany transaction is with. nil: none.
	TradingPartner *uuid.UUID
}

// tradingPartnerOf is the line's trading partner: its own, else the
// posting's.
func (in Posting) tradingPartnerOf(l PostingLine) *uuid.UUID {
	if l.TradingPartner != nil {
		return l.TradingPartner
	}
	return in.TradingPartner
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
	// line goes to the price_difference account, or to the item's own
	// account when the item is an asset's (ItemAccountID).
	OffsetAccountID *uuid.UUID
	Invoiced        *money.Minor
	// ItemAccountID is the order item's account, given with an invoiced
	// or a clearing item. When it is an asset clearing account of the
	// company in the active configuration (companies[].asset_accounts[]
	// .clearing), the item is an asset's (ADR-0057 §6) and the counter
	// line posts to it rather than to the price_difference account: the
	// cost of an asset is what was paid for it, the tax not recovered
	// included (IAS 16.16; product owner 2026-11-10), and the line waits
	// there to be capitalised as the receipt's does. It carries no cost
	// center: a clearing account is a balance sheet account. nil, or any
	// other account: the price_difference account.
	ItemAccountID *uuid.UUID
	// TaxCode is an invoiced item's tax code: its GR/IR and price
	// difference lines are the code's base (ADR-0049 §5).
	TaxCode *string
	// TaxBorne is the input tax an invoiced item bears, which Invoiced
	// includes (a non-deductible component's, a part not recovered;
	// ADR-0049 §8), signed as Invoiced. It moves no amount: the ledger
	// records it beside the item's price difference line with the order
	// item (ledger.price_difference_item; B10, ADR-0057 §6), so that what
	// the line expenses, or adds to an asset's cost on its clearing
	// account, can be told apart as tax and price difference.
	TaxBorne money.Minor
	Text     *string
	// CostCenterID is the order item's cost center (ADR-0050 §6, T56e): a
	// goods movement's counter line and an invoice's price difference line
	// carry it, as SAP charges a non-valuated receipt's price difference to
	// the item's assignment; the GR/IR line, a balance sheet line, none.
	// nil: the ledger derives them (§5).
	CostCenterID *uuid.UUID
	// Clears makes the item a GR/IR clearing's (T72e, SAP MR11), naming
	// neither an offset account nor an invoiced amount: Quantity is the
	// position's quantity negated, and what the GR/IR line clears goes to
	// the price_difference account, with CostCenterID when that is a
	// primary cost account, as an invoice's price difference does, or to
	// an asset's item's own account (ItemAccountID). An entry that clears
	// is all clearing items.
	Clears *GRIRClearing
	// TakesOver makes the item a migration run's take-over of the position
	// the source system holds for the order item (T72f; SAP's GR/IR lines
	// carry the order item, ACDOCA EBELN/EBELP): its balance there, in the
	// entry's currency. Quantity is the position's open quantity, invoiced
	// less received in the source system, and UnitPrice is not read. The
	// quantity counts as migrated, neither received nor invoiced. An entry
	// that takes over is an integration entry of take-over items only.
	//
	// The item's GR/IR line moves what earlier take-overs took over of the
	// item (TakenOver) to that balance and quantity: all of them the first
	// time, what the source system's position moved since on a later run's
	// top-up. A line on the GR/IR account without an order item offsets
	// it, as the run loaded the source system's documents' lines there
	// without theirs. What the platform itself posted on the item is left
	// as it is.
	TakesOver *money.Minor
	// TakesOverLC is the take-over's balance in local currency, kept as the
	// source system booked it rather than converted again (ADR-0005 As
	// built): a position in a foreign document currency migrated in that
	// currency, TakesOver its document amount (SAP WSL) and this its
	// company code amount (HSL). Its group amount is converted from it at
	// the posting date's rate. Given on every item of such an entry, with
	// its Posting.Rate, and on no other (checkForeignTakeOver). nil:
	// TakesOver converted at the posting's rates.
	TakesOverLC *money.Minor
	// TakenOver is what earlier take-overs took over of the item as the
	// caller read it (Postings.TakenOverGRIR), nil when they took over
	// nothing: the item is refused unless that is still what they took
	// over (409 ledger.gr_ir_position_exists), so a source system's
	// position is never taken over twice.
	TakenOver *GRIRTakenOver
}

// GRIRTakenOver is what migration runs' take-overs (GRIRItem.TakesOver)
// took over of an order item's GR/IR position: the quantity, signed as the
// position, and the balance in its transaction currency and in local
// currency.
type GRIRTakenOver struct {
	Quantity  string
	Balance   money.Minor
	BalanceLC money.Minor
}

// GRIRClearing is the position a clearing item was approved to clear: it
// posts only while the position still holds that quantity and balance and
// the price difference account is still the one the approver saw, and is
// refused otherwise (409 ledger.gr_ir_clearing_outdated).
type GRIRClearing struct {
	Balance money.Minor // in the position's transaction currency
	Account uuid.UUID   // the price_difference account
}

// GRIRPosition is an order item's current GR/IR position, as a clearing
// proposes to clear it.
type GRIRPosition struct {
	POItemNo int32
	// Quantity is invoiced + cleared + migrated - received, signed as the GR/IR line
	// (debit positive).
	Quantity, Unit string
	Balance        money.Minor // in Currency, the position's transaction currency
	Currency       string
	Open           bool
}

// AccountRef names a G/L account.
type AccountRef struct {
	ID           uuid.UUID
	Number, Name string
}

// GRIROrder is what a GR/IR clearing of a purchase order is proposed from:
// the current positions of its items in the legal entity, by item number,
// and the legal entity's price_difference account (nil: none). A read
// for a proposal, which the clearing's posting checks again under the
// order's GR/IR lock (GRIRItem.Clears).
type GRIROrder struct {
	Positions       []GRIRPosition
	PriceDifference *AccountRef
}

// GRIROrder reads the positions of the order's items in the legal entity
// (ledger.gr_ir_position) and the price_difference account.
func (Postings) GRIROrder(ctx context.Context, legalEntity, order uuid.UUID) (GRIROrder, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return GRIROrder{}, err
	}
	rows, _ := tx.Query(ctx, `SELECT po_item_no, trim_scale(invoiced + cleared + migrated - received)::text, uom_code, amount_tc, currency_tc, is_open
		FROM ledger.gr_ir_position WHERE legal_entity_id = $1 AND purchase_order_id = $2 ORDER BY po_item_no`, legalEntity, order)
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (GRIRPosition, error) {
		var p GRIRPosition
		return p, r.Scan(&p.POItemNo, &p.Quantity, &p.Unit, &p.Balance, &p.Currency, &p.Open)
	})
	if err != nil {
		return GRIROrder{}, fmt.Errorf("ledger: GR/IR positions of order %s: %w", order, err)
	}
	out := GRIROrder{Positions: list}
	var a AccountRef
	err = tx.QueryRow(ctx, `SELECT a.id, a.number, a.name FROM ledger.account_determination d JOIN ledger.gl_account a ON a.id = d.account_id
		WHERE d.legal_entity_id = $1 AND d.key = 'price_difference'`, legalEntity).Scan(&a.ID, &a.Number, &a.Name)
	switch {
	case err == nil:
		out.PriceDifference = &a
	case !errors.Is(err, pgx.ErrNoRows):
		return GRIROrder{}, fmt.Errorf("ledger: price difference account of %s: %w", legalEntity, err)
	}
	return out, nil
}

// Post posts the entry in the leading ledger; one with GR/IR items books
// their lines too (postGRIR). A given rate is taken only on an entry in
// a foreign currency (postingRates), with GR/IR items only by a take-over
// giving every item's local balance (checkForeignTakeOver).
func (p Postings) Post(ctx context.Context, in Posting) (Entry, error) {
	for i, l := range in.Lines {
		if l.GC != nil && l.LC == nil {
			return Entry{}, fmt.Errorf("ledger: line %d keeps a group amount without its local amount", i+1)
		}
	}
	if len(in.GRIR) > 0 {
		return p.postGRIR(ctx, in)
	}
	tx, principal, err := reqctx.Request(ctx)
	if err != nil {
		return Entry{}, err
	}
	exps, err := exponents(ctx, tx, in.Currency)
	if err != nil {
		return Entry{}, err
	}
	entry := NewEntry{LegalEntityID: in.LegalEntityID, DocumentType: in.DocumentType, DocumentDate: in.DocumentDate, PostingDate: in.PostingDate,
		TaxDate: in.TaxDate, Currency: in.Currency, Reference: in.Reference, HeaderText: in.HeaderText, SourceContext: in.SourceContext,
		SourceDocumentID: &in.SourceDocumentID, Lines: make([]NewLine, len(in.Lines)), taxItems: in.TaxItems, rate: in.Rate,
		taxDateChosen: in.TaxDateChosen, LedgerID: in.LedgerID, collective: in.Collective}
	for i, l := range in.Lines {
		entry.Lines[i] = NewLine{AccountID: l.AccountID, Amount: l.Amount.Plain(exps[in.Currency]), Currency: in.Currency,
			PartnerID: l.PartnerID, TaxCode: l.TaxCode, Quantity: l.Quantity, Unit: l.Unit, Text: l.Text, SpecialGL: l.SpecialGL, lc: l.LC, gc: l.GC,
			CostCenterID: l.CostCenterID, ProfitCenterID: l.ProfitCenterID, SegmentID: l.SegmentID, TradingPartnerID: in.tradingPartnerOf(l),
			InternalOrderID: l.InternalOrderID}
	}
	posted, err := p.endpoints().postJournalEntry(ctx, tx, principal, entry)
	if err != nil {
		return Entry{}, err
	}
	return posted, keepItemData(ctx, tx, posted, in)
}

// keepItemData keeps the payment terms, the fixed payment method and the
// withholding taxes of a posted entry's supplier open item.
func keepItemData(ctx context.Context, tx pgx.Tx, entry Entry, in Posting) error {
	if err := keepTerms(ctx, tx, entry, in.Terms); err != nil {
		return err
	}
	if err := keepPaymentMethods(ctx, tx, entry, in); err != nil {
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
// currency, debit positive, from the entries posted on or before each of
// the dates, in their order: what a house bank's main bank account holds
// on its statements' closing dates (ADR-0047, T49h), in the bank's
// currency (ADR-0051 §3, T53a). In the legal entity's local currency that
// is every entry's local amount; in another currency the transaction
// amounts of the entries in it, which a period-end valuation (posted in
// local currency) leaves alone, as the bank does. The ledger posts no
// year-end carry-forward entries, so every year's lines are summed.
func (Accounts) Balances(ctx context.Context, legalEntity, account uuid.UUID, currency string, dates []time.Time) ([]money.Minor, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT coalesce((SELECT sum(CASE WHEN e.currency_lc = $3 THEN l.amount_lc ELSE l.amount_tc END)
			FROM ledger.journal_line l
			JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id
			WHERE l.legal_entity_id = $1 AND l.account_id = $2 AND l.posting_date <= x.d AND (e.currency_lc = $3 OR e.currency_tc = $3)
				AND l.ledger_id = (SELECT id FROM ledger.ledger WHERE is_leading)), 0)::bigint
		FROM unnest($4::date[]) WITH ORDINALITY AS x(d, n) ORDER BY x.n`, legalEntity, account, currency, dates)
	out, err := pgx.CollectRows(rows, pgx.RowTo[money.Minor])
	if err != nil {
		return nil, fmt.Errorf("ledger: balances of account %s in %s in %s: %w", account, legalEntity, currency, err)
	}
	return out, nil
}

// OpenLine is an open line of the leading ledger on an open-item managed
// account, in its entry's transaction currency, with the document the
// entry was posted for.
type OpenLine struct {
	AccountID     uuid.UUID
	Item          ItemRef
	Currency      string
	Amount        money.Minor // in Currency, debit positive
	PostingDate   time.Time
	SourceContext string     // the context that posted the entry: payments for a payment
	SourceID      *uuid.UUID // its document there: the payment
}

// OpenLines are the leading ledger's open lines on the accounts in the
// legal entity: neither cleared nor of a reversed entry or a reversal,
// as an item the clearing would take. A house bank's clearing account
// holds the payments in transit this way (ADR-0051 §4, T53c), beside a
// revaluation's valuation and reset lines, which name the ledger as their
// source and the reader leaves out. Parallel ledgers' copies are never
// open items (ADR-0045).
func (Accounts) OpenLines(ctx context.Context, legalEntity uuid.UUID, accounts []uuid.UUID) ([]OpenLine, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT l.account_id, l.fiscal_year, l.journal_entry_id, l.line_no, e.currency_tc, l.amount_tc, l.posting_date,
			e.source_context, e.source_document_id
		FROM ledger.journal_line l
		JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id
		WHERE l.legal_entity_id = $1 AND l.account_id = ANY($2) AND l.ledger_id = (SELECT id FROM ledger.ledger WHERE is_leading)
			AND NOT EXISTS (SELECT FROM ledger.clearing_item ci WHERE ci.tenant_id = l.tenant_id AND ci.fiscal_year = l.fiscal_year
				AND ci.journal_entry_id = l.journal_entry_id AND ci.line_no = l.line_no)
			AND NOT `+reversedLine+`
		ORDER BY l.posting_date, l.journal_entry_id, l.line_no`, legalEntity, accounts)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (OpenLine, error) {
		var o OpenLine
		err := r.Scan(&o.AccountID, &o.Item.FiscalYear, &o.Item.EntryID, &o.Item.LineNo, &o.Currency, &o.Amount, &o.PostingDate, &o.SourceContext, &o.SourceID)
		return o, errs.Wrapf(err, "scan open line")
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: open lines of %d accounts in %s: %w", len(accounts), legalEntity, err)
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

// TaxCode is a tax code as it is on a date (ADR-0049 §3): its definition
// and the components with a rate in force then.
type TaxCode struct {
	Code       string
	Direction  string // input or output
	Kind       string // standard, zero, exempt, out_of_scope, reverse_charge, import or adjustment
	Category   string // UN/CEFACT 5305, "" when none
	Components []TaxComponent
}

// TaxComponent is a component of a tax code with its rate on the date.
type TaxComponent struct {
	Key            string
	Jurisdiction   string // "" when none
	AccountKey     string // input, output, non_deductible, self_output or self_input
	RecoverablePct string // decimal, e.g. "100"
	AccountID      *uuid.UUID
	RatePct        string // decimal, e.g. "19.000000"
	// JurisdictionOnLine: it levies the subdivision of its Jurisdiction (a
	// country) each line names (T61f).
	JurisdictionOnLine bool
	// Subdivisions are those it accepts on a line, any of its country's
	// when none (the emirate guard, ADR-0049 §7 As built).
	Subdivisions []string
}

// Single is the code's one component when it has exactly one, which posts
// to a tax account of its own and is wholly recoverable: what supplier
// invoices and down payments post until T61d posts every component.
func (t TaxCode) Single() (TaxComponent, bool) {
	if len(t.Components) != 1 {
		return TaxComponent{}, false
	}
	c := t.Components[0]
	ok := (c.AccountKey == "input" || c.AccountKey == "output") && c.AccountID != nil && c.RecoverablePct == "100"
	return c, ok
}

// Calculable is the code as the calculator takes it (tax.Code), with each
// component's rate on the date it was read on.
func (t TaxCode) Calculable() tax.Code {
	c := tax.Code{Code: t.Code, Kind: t.Kind}
	for _, k := range t.Components {
		c.Components = append(c.Components, tax.Component{Key: k.Key, Jurisdiction: k.Jurisdiction, AccountKey: k.AccountKey,
			RecoverablePct: k.RecoverablePct, Rates: []tax.Rate{{Pct: k.RatePct}}})
	}
	return c
}

// TaxCode returns the legal entity's tax code valid on the date: one with a
// rate in force then.
func (Accounts) TaxCode(ctx context.Context, legalEntity uuid.UUID, code string, date time.Time) (TaxCode, bool, error) {
	codes, err := readTaxCodes(ctx, legalEntity, &code, date)
	if err != nil || len(codes) == 0 {
		return TaxCode{}, false, err
	}
	return codes[0], true, nil
}

// TaxCodes returns the legal entity's tax codes valid on the date, by code:
// what the capture agent proposes a code from (T61d).
func (Accounts) TaxCodes(ctx context.Context, legalEntity uuid.UUID, date time.Time) ([]TaxCode, error) {
	return readTaxCodes(ctx, legalEntity, nil, date)
}

// readTaxCodes reads the legal entity's codes valid on the date, the one named
// or all, each with its components rated then.
func readTaxCodes(ctx context.Context, legalEntity uuid.UUID, code *string, date time.Time) ([]TaxCode, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT d.code, d.direction, d.kind, coalesce(d.category, ''), c.component, coalesce(c.jurisdiction, ''), c.account_key,
			trim_scale(c.recoverable_pct)::text, c.account_id, r.rate_pct::text, c.jurisdiction_on_line, c.subdivisions
		FROM ledger.tax_code_definition d
		JOIN ledger.tax_component c ON c.tenant_id = d.tenant_id AND c.legal_entity_id = d.legal_entity_id AND c.code = d.code
		JOIN ledger.tax_rate r ON r.tenant_id = c.tenant_id AND r.legal_entity_id = c.legal_entity_id AND r.code = c.code AND r.component = c.component
		WHERE d.legal_entity_id = $1 AND ($2::text IS NULL OR d.code = $2) AND r.withdrawn_at IS NULL AND r.valid_from <= $3
			AND (r.valid_to IS NULL OR r.valid_to > $3)
		ORDER BY d.code, c.component`, legalEntity, code, date)
	var out []TaxCode
	var t TaxCode
	var c TaxComponent
	if _, err := pgx.ForEachRow(rows, []any{&t.Code, &t.Direction, &t.Kind, &t.Category, &c.Key, &c.Jurisdiction, &c.AccountKey, &c.RecoverablePct,
		&c.AccountID, &c.RatePct, &c.JurisdictionOnLine, &c.Subdivisions}, func() error {
		if n := len(out); n == 0 || out[n-1].Code != t.Code {
			out = append(out, TaxCode{Code: t.Code, Direction: t.Direction, Kind: t.Kind, Category: t.Category})
		}
		out[len(out)-1].Components = append(out[len(out)-1].Components, c)
		c = TaxComponent{} // the next row's account is scanned into a new value, not this one's
		return nil
	}); err != nil {
		return nil, fmt.Errorf("ledger: tax codes of %s: %w", legalEntity, err)
	}
	return out, nil
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
		return 0, noFiscalYear(date)
	}
	if err != nil {
		return year, fmt.Errorf("ledger: fiscal year of %s on %s: %w", legalEntity, date.Format(time.DateOnly), err)
	}
	return year, nil
}

// noFiscalYear refuses a date outside every set-up fiscal year.
func noFiscalYear(date time.Time) error {
	return problem.Unprocessable("ledger.no_fiscal_period", "No fiscal year of the legal entity covers "+date.Format(time.DateOnly)+
		"; a fiscal year is set up by a number range for it in the configuration.")
}

// FiscalPeriod is a regular posting period of a fiscal year with its
// dates.
type FiscalPeriod struct {
	FiscalYear, Number int16
	Start, End         time.Time
}

// Period is the regular posting period of the legal entity the date falls
// in; a date outside every set-up fiscal year is 422
// ledger.no_fiscal_period, as FiscalYear.
func (c Calendar) Period(ctx context.Context, legalEntity uuid.UUID, date time.Time) (FiscalPeriod, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return FiscalPeriod{}, err
	}
	le, ok, err := c.LegalEntities.Entity(ctx, legalEntity)
	if err != nil {
		return FiscalPeriod{}, err
	}
	var p FiscalPeriod
	if ok {
		err = tx.QueryRow(ctx, `SELECT fiscal_year, period, start_date, end_date FROM ledger.fiscal_period
			WHERE calendar_id = $1 AND NOT is_special AND $2 BETWEEN start_date AND end_date`, le.FiscalCalendarID, date).Scan(&p.FiscalYear, &p.Number, &p.Start, &p.End)
	}
	if !ok || errors.Is(err, pgx.ErrNoRows) {
		return FiscalPeriod{}, noFiscalYear(date)
	}
	if err != nil {
		return FiscalPeriod{}, fmt.Errorf("ledger: period of %s on %s: %w", legalEntity, date.Format(time.DateOnly), err)
	}
	return p, nil
}

// Periods are the regular posting periods of the legal entity's fiscal
// year, in order (ADR-0057 §7: a depreciation run and its preview name a
// period by year and number); a year that is not set up is 422
// ledger.no_fiscal_period, as FiscalYear.
func (c Calendar) Periods(ctx context.Context, legalEntity uuid.UUID, year int16) ([]FiscalPeriod, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, ok, err := c.LegalEntities.Entity(ctx, legalEntity)
	if err != nil {
		return nil, err
	}
	var periods []FiscalPeriod
	if ok {
		rows, _ := tx.Query(ctx, `SELECT fiscal_year, period, start_date, end_date FROM ledger.fiscal_period
			WHERE calendar_id = $1 AND fiscal_year = $2 AND NOT is_special ORDER BY period`, le.FiscalCalendarID, year)
		if periods, err = pgx.CollectRows(rows, pgx.RowToStructByPos[FiscalPeriod]); err != nil {
			return nil, fmt.Errorf("ledger: periods of %s in %d: %w", legalEntity, year, err)
		}
	}
	if len(periods) == 0 {
		return nil, problem.Unprocessable("ledger.no_fiscal_period", fmt.Sprintf("No fiscal year %d of the legal entity is set up; "+
			"a fiscal year is set up by a number range for it in the configuration.", year))
	}
	return periods, nil
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
