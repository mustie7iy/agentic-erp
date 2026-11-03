package ledger

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// OpenItems answers the payments context's questions about open items, in
// the request's transaction.
type OpenItems struct{}

// PayableItem is an open item on a supplier reconciliation account of the
// leading ledger, in minor units of its transaction currency (a credit,
// what is owed, is negative).
type PayableItem struct {
	ItemRef
	AccountID    uuid.UUID
	PartnerID    uuid.UUID
	DocumentDate time.Time
	Currency     string // its entry's transaction currency
	Amount       money.Minor
	// Terms are the payment terms the item was posted with (T36b), or nil
	// for an item posted without (before T36, manual or migrated).
	Terms *ItemTerms
	// Withholding are its withholding taxes deducted at payment (T46f):
	// a payment withholds PaymentWithholding of them.
	Withholding []Withholding
	// Lines are its document's offsetting lines where its reader gives
	// them: an unposted invoice's items, as payables reads them for the
	// liquidity forecast (T53e3); a posted item's are read by Offsetting.
	Lines []OffsettingLine
	// Document and Number are the document it is an item of and its
	// number: a posted item's journal entry; an unposted invoice, as
	// payables reads it for the liquidity forecast, or a down payment
	// request, as payments gives it, theirs: what the forecast's
	// drill-down names (T53e4).
	Document uuid.UUID
	Number   string
}

// Payable returns the legal entity's open supplier items in the currency
// with a document date on or before the date, ordered by partner and item.
// Lines of a reversed entry or of a reversal are left out: the reversal
// cancelled them, and Settle refuses them. A reversal clears them, so this
// matters only for entries reversed before reversals cleared the pair.
// Items an owner holds (item_reservation) are left out too: they are
// another run's to pay, and Reserve would refuse them, so a reservation
// that outlived its run (released by a version that did not know of it)
// keeps one item out of later runs instead of failing every proposal.
func (OpenItems) Payable(ctx context.Context, legalEntity uuid.UUID, currency string, documentDateTo time.Time) ([]PayableItem, error) {
	return payable(ctx, fmt.Sprintf("payable items of %s in %s", legalEntity, currency), `e.currency_tc = $2 AND e.document_date <= $3`,
		legalEntity, currency, documentDateTo)
}

// Owed returns the legal entity's open supplier items in every currency,
// whatever their document date, as Payable reads them: what the liquidity
// forecast expects to pay (ADR-0051 §5, T53e).
func (OpenItems) Owed(ctx context.Context, legalEntity uuid.UUID) ([]PayableItem, error) {
	return payable(ctx, "open supplier items of "+legalEntity.String(), `true`, legalEntity)
}

// payable reads the open supplier items of legal entity $1 that the
// filter, a constant condition on the entry e over the further arguments
// ($2 on), selects; what names them in an error.
func payable(ctx context.Context, what, filter string, args ...any) ([]PayableItem, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT l.fiscal_year, l.journal_entry_id, l.line_no, l.account_id, l.partner_id, e.document_date, e.currency_tc, l.amount_tc,
			e.document_number, `+termsColumns+`, `+withholdingColumn+`
		FROM ledger.journal_line l
		LEFT JOIN ledger.open_item_terms t ON t.tenant_id = l.tenant_id AND t.fiscal_year = l.fiscal_year AND t.journal_entry_id = l.journal_entry_id
			AND t.line_no = l.line_no
		JOIN ledger.ledger g ON g.tenant_id = l.tenant_id AND g.id = l.ledger_id AND g.is_leading
		JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id AND a.reconciliation_kind = 'supplier'
		JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id
		WHERE l.legal_entity_id = $1 AND `+filter+`
			AND NOT EXISTS (SELECT FROM ledger.clearing_item c WHERE c.tenant_id = l.tenant_id AND c.fiscal_year = l.fiscal_year
				AND c.journal_entry_id = l.journal_entry_id AND c.line_no = l.line_no)
			AND l.special_gl IS NULL -- special G/L items (down payments) never net in a run (ADR-0037)
			AND NOT `+reversedLine+`
			AND NOT EXISTS (SELECT FROM ledger.item_reservation ir WHERE ir.tenant_id = l.tenant_id AND ir.fiscal_year = l.fiscal_year
				AND ir.journal_entry_id = l.journal_entry_id AND ir.line_no = l.line_no AND ir.released_at IS NULL)
		ORDER BY l.partner_id, l.fiscal_year, l.journal_entry_id, l.line_no`, args...)
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (PayableItem, error) {
		var p PayableItem
		var t storedTerms
		err := errs.Wrapf(r.Scan(append([]any{&p.FiscalYear, &p.EntryID, &p.LineNo, &p.AccountID, &p.PartnerID, &p.DocumentDate, &p.Currency, &p.Amount,
			&p.Number}, append(t.fields(), &p.Withholding)...)...), "scan payable item")
		p.Terms, p.Document = t.terms(), p.EntryID
		return p, err
	})
	if err != nil {
		return items, fmt.Errorf("ledger: %s: %w", what, err)
	}
	return items, nil
}

// Why an item can no longer be settled (OpenItems.Unpayable).
const (
	ItemReversed = "reversed" // a line of a reversed entry or of a reversal
	ItemNotOpen  = "not_open" // cleared, or not an open item at all
)

// Unpayable returns, for each of the items that can no longer be settled,
// why: ItemReversed or ItemNotOpen. Items still open are left out. These
// are lasting: neither changes back, so a payment of such an item can
// never post.
func (OpenItems) Unpayable(ctx context.Context, refs []ItemRef) (map[ItemRef]string, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	items, err := loadItems(ctx, tx, refs)
	if err != nil {
		return nil, err
	}
	out := map[ItemRef]string{}
	for _, it := range items {
		switch {
		case it.reversed:
			out[it.ref] = ItemReversed
		case !it.found || !it.recon || !it.open:
			out[it.ref] = ItemNotOpen
		}
	}
	return out, nil
}

// References returns the reference of each entry, or its document number
// when it has none: what a payment tells the supplier it settles.
func (OpenItems) References(ctx context.Context, entries []uuid.UUID) (map[uuid.UUID]string, error) {
	return entryTexts(ctx, `SELECT id, coalesce(reference, document_number) FROM ledger.journal_entry WHERE id = ANY($1)`, "references", entries)
}

// Numbers returns the entries' document numbers, keyed by id: what a
// payment's open item shows beside its entry's id. An id that names no
// entry is left out.
func (OpenItems) Numbers(ctx context.Context, entries []uuid.UUID) (map[uuid.UUID]string, error) {
	return entryTexts(ctx, `SELECT id, document_number FROM ledger.journal_entry WHERE id = ANY($1)`, "document numbers", entries)
}

// entryTexts runs query, which selects an entry's id and a text of it for
// the entries in $1, and keys the texts by id; what names the texts in the
// error.
func entryTexts(ctx context.Context, query, what string, entries []uuid.UUID) (map[uuid.UUID]string, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, query, entries)
	out := map[uuid.UUID]string{}
	var id uuid.UUID
	var text string
	_, err = pgx.ForEachRow(rows, []any{&id, &text}, func() error { out[id] = text; return nil })
	if err != nil {
		return nil, fmt.Errorf("ledger: %s of %d entries: %w", what, len(entries), err)
	}
	return out, nil
}

// Settlement pays open items from a bank account.
type Settlement struct {
	LegalEntityID    uuid.UUID
	DocumentType     string
	PostingDate      time.Time
	Currency         string
	BankAccountID    uuid.UUID
	Reference        *string
	HeaderText       *string
	SourceContext    string
	SourceDocumentID uuid.UUID
	Items            []ItemRef
	// Discounts are the cash discounts taken on the items (T36c), in the
	// transaction currency and in the sign the items are paid in (positive
	// on an invoice), one per item and smaller than it; nil for none.
	Discounts []money.Minor
	// Withholding is what each item withholds, as its payment deducted it
	// (T46f), in the items' sign; nil when the caller states none. A
	// recomputed withholding that differs is refused (409
	// ledger.withholding_changed).
	Withholding []money.Minor
	// ReservedFor is the owner whose reservation of the items this
	// settlement pays (a payment run), or uuid.Nil: items another owner
	// holds are refused, and the owner's reservations of the settled items
	// end with the settlement.
	ReservedFor uuid.UUID
}

// Settle posts one entry that settles open items of one legal entity and
// transaction currency against a bank account, and clears the items with
// it (the entry is the clearing document, as a payment is in SAP). Each
// item is offset on its account at its original amounts in all three
// currencies; the bank is credited with their total less any cash
// discounts taken, which go to the cash_discount account, and less the
// withholding taxes deducted at payment (T46f), which go to their codes'
// accounts, all converted
// at the rates of the posting date; what that leaves in local or group
// currency is a realized exchange difference on the account
// determination's fx_gain or fx_loss account. The partner lines are
// cleared with the items.
func (p Postings) Settle(ctx context.Context, in Settlement) (Entry, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return Entry{}, err
	}
	if len(in.Items) == 0 {
		return Entry{}, problem.Unprocessable("ledger.no_items", "A settlement needs at least one open item.")
	}
	e := p.endpoints()
	le, err := e.legalEntity(ctx, in.LegalEntityID, "legal_entity_id")
	if err != nil {
		return Entry{}, err
	}
	items, err := loadItems(ctx, tx, in.Items)
	if err != nil {
		return Entry{}, err
	}
	lines, tc, lc, gc, err := settledItemLines(items, in, le.ID)
	if err != nil {
		return Entry{}, err
	}
	paid, bank, taxItems, err := e.paymentLines(ctx, tx, in, le, items, settled{tc: tc, lc: lc, gc: gc})
	if err != nil {
		return Entry{}, err
	}
	lines = append(lines, paid...)
	if err := e.checkAccounts(ctx, tx, le.ID, lines, false); err != nil {
		return Entry{}, err
	}
	entry, err := e.post(ctx, tx, actor, draft{entity: le, documentType: in.DocumentType, documentDate: in.PostingDate,
		postingDate: in.PostingDate, currencyTC: in.Currency, currencyLC: le.LocalCurrency, currencyGC: bank.group, rateTCLC: bank.rateLC.shown, rateLCGC: bank.rateGC.shown,
		reference: in.Reference, headerText: in.HeaderText, sourceContext: in.SourceContext, sourceDocumentID: &in.SourceDocumentID, lines: lines,
		sourcedItems: taxItems})
	if err != nil {
		return Entry{}, err
	}
	id, err := clearWithEntry(ctx, tx, actor, le.ID, in.PostingDate, entry, items, leadingLines(len(items)))
	if err != nil {
		return Entry{}, err
	}
	if in.ReservedFor != uuid.Nil {
		if err := release(ctx, tx, actor, in.ReservedFor, in.Items); err != nil {
			return Entry{}, err
		}
	}
	return entry, errors.Join(
		audit.Add(ctx, audit.Entry{Action: "ledger.journal_entry.post", ObjectType: "ledger.journal_entry", ObjectID: entry.ID, Outcome: "succeeded",
			Detail: map[string]any{"document_number": entry.DocumentNumber, "fiscal_year": entry.FiscalYear, "lines": len(lines),
				"source_context": entry.SourceContext}}),
		audit.Add(ctx, audit.Entry{Action: "ledger.clearing.create", ObjectType: "ledger.clearing", ObjectID: id, Outcome: "succeeded",
			Detail: map[string]any{"items": len(items), "clearing_entry_id": entry.ID, "document_number": entry.DocumentNumber}}))
}

// settlementBank is the bank line's local and group amounts with the
// rates of the posting date they were converted at, and the currencies'
// exponents to convert other amounts of the settlement at them.
type settlementBank struct {
	group               string
	rateLC, rateGC      exchange
	expTC, expLC, expGC int
	lc, gc              money.Minor
}

// convert is a transaction amount in local and group currency at the
// settlement's rates.
func (b settlementBank) convert(tc money.Minor) (lc, gc money.Minor, err error) {
	if lc, err = convert(tc, b.rateLC, b.expTC, b.expLC); err != nil {
		return 0, 0, err
	}
	gc, err = convert(lc, b.rateGC, b.expLC, b.expGC)
	return lc, gc, err
}

// settled is the items' total in the three currencies.
type settled struct{ tc, lc, gc money.Minor }

// paymentLines are what pays the items: the bank line, credited with
// their total less the discounts taken (T36c) and the withholding taxes
// deducted at payment (T46f); a line crediting the discounts to the
// cash_discount account, and one per withholding tax code and account;
// all converted at the posting date's rates, and what that leaves of the
// items' local and group totals as exchange differences.
func (e *Endpoints) paymentLines(ctx context.Context, tx pgx.Tx, in Settlement, le tenancy.Entity, items []item, total settled) ([]Line, settlementBank,
	[]sourcedTaxItem, error) {
	withheld, wht, err := settlementWithholding(ctx, tx, in, items)
	if err != nil {
		return nil, settlementBank{}, nil, err
	}
	discount, bankTC, err := settlementDiscount(in, items, total.tc, wht)
	if err != nil {
		return nil, settlementBank{}, nil, err
	}
	bank, err := e.bankAmounts(ctx, tx, in.Currency, in.PostingDate, le, bankTC)
	if err != nil {
		return nil, settlementBank{}, nil, err
	}
	lines := []Line{{AccountID: in.BankAccountID, TC: bankTC, LC: bank.lc, GC: bank.gc, Text: in.HeaderText}}
	var more []Line
	var plan discountPlan
	if discount != 0 {
		if more, plan, err = discountLines(ctx, tx, le.ID, items, in, discount); err != nil {
			return nil, settlementBank{}, nil, err
		}
	}
	more = append(more, withholdingLines(withheld, in)...)
	paid := bank
	for i := range more {
		if more[i].LC, more[i].GC, err = bank.convert(more[i].TC); err != nil {
			return nil, settlementBank{}, nil, err
		}
		if paid.lc, err = money.Sum(paid.lc, more[i].LC); err == nil {
			paid.gc, err = money.Sum(paid.gc, more[i].GC)
		}
		if err != nil {
			return nil, settlementBank{}, nil, errTotalTooLarge("items")
		}
	}
	taxItems := plan.taxItems(bank)
	fx, err := exchangeDifferenceLines(ctx, le.ID, total.lc, total.gc, paid, in.HeaderText)
	return append(append(lines, more...), fx...), bank, taxItems, err
}

// settlementDiscount is the total of the discounts taken on the items, in
// the sign they are paid in, and what the bank pays after them (the items'
// total tc, negative when paid out, less the discounts): one discount per
// item (or none at all), each 0 or of the item's sign and smaller than it
// (422 ledger.invalid_discount).
func settlementDiscount(in Settlement, items []item, tc, withheld money.Minor) (discount, bank money.Minor, err error) {
	invalid := func(detail string) error {
		return problem.Unprocessable("ledger.invalid_discount", "A settlement's cash discounts "+detail+".")
	}
	if len(in.Discounts) != 0 && len(in.Discounts) != len(items) {
		return 0, 0, invalid(fmt.Sprintf("are one per item: %d for %d items", len(in.Discounts), len(items)))
	}
	for i, d := range in.Discounts {
		paid := -items[i].tc
		if d != 0 && (d == math.MinInt64 || (d > 0) != (paid > 0) || d.Abs() >= paid.Abs()) {
			return 0, 0, invalid(fmt.Sprintf("are of their item's sign and smaller than it: %d on an item paid at %d", d, paid))
		}
	}
	if discount, err = money.Sum(in.Discounts...); err == nil {
		bank, err = money.Sum(tc, discount, withheld)
	}
	if err != nil {
		return 0, 0, errTotalTooLarge("discounts")
	}
	return discount, bank, nil
}

// discountLines credit the discounts taken (T36d): the net that is a tax
// code's base to the legal entity's cash_discount account (SAP SKE) with
// the code, the untaxed rest of it there without one, and their tax share
// to each tax line's account with its code (discountTax), in transaction
// currency (paymentLines converts them); with the plan of their tax items.
func discountLines(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID, items []item, in Settlement, discount money.Minor) ([]Line, discountPlan, error) {
	account, ok, err := Accounts{}.Determination(ctx, legalEntity, "cash_discount")
	if err != nil {
		return nil, discountPlan{}, err
	}
	if !ok {
		return nil, discountPlan{}, problem.Unprocessable("ledger.no_cash_discount_account",
			"The payment takes a cash discount, and the legal entity has no cash_discount account in its account determination.")
	}
	split, err := discountTax(ctx, tx, items, in.Discounts)
	if err != nil {
		return nil, discountPlan{}, err
	}
	plan := discountPlan{split: split}
	// Tax and bases are shares of the discount, of its sign and adding up
	// to at most it (itemTaxShares), so the untaxed net is of the
	// discount's sign or 0: no line then.
	var lines []Line
	if net := discount - split.tax - split.base; net != 0 {
		lines = append(lines, Line{AccountID: account, TC: -net, Text: in.HeaderText})
	}
	// Shares of an invoice and a credit memo can cancel: no line then.
	for _, c := range slices.Sorted(maps.Keys(split.bases)) {
		if code := c; split.bases[c] != 0 {
			lines = append(lines, Line{AccountID: account, TC: -split.bases[c], TaxCode: &code, Text: in.HeaderText})
		}
	}
	for _, k := range slices.SortedFunc(maps.Keys(split.taxes), compareTaxKeys) {
		if code := k.code; split.taxes[k] != 0 {
			lines = append(lines, Line{AccountID: k.account, TC: -split.taxes[k], TaxCode: &code, Text: in.HeaderText})
		}
	}
	return lines, plan, nil
}

// bankAmounts converts the settled transaction amount to local currency,
// and that to group currency, at the average rates of the posting date.
func (e *Endpoints) bankAmounts(ctx context.Context, tx pgx.Tx, currency string, date time.Time, le tenancy.Entity, tc money.Minor) (settlementBank, error) {
	var b settlementBank
	var err error
	if b.group, err = e.LegalEntities.GroupCurrency(ctx); err != nil {
		return settlementBank{}, err
	}
	exps, err := exponents(ctx, tx, currency, le.LocalCurrency, b.group)
	if err != nil {
		return settlementBank{}, err
	}
	if b.rateLC, b.rateGC, err = postingRates(ctx, tx, currency, le.LocalCurrency, b.group, date, nil); err != nil {
		return settlementBank{}, err
	}
	b.expTC, b.expLC, b.expGC = exps[currency], exps[le.LocalCurrency], exps[b.group]
	if b.lc, b.gc, err = b.convert(tc); err != nil {
		return settlementBank{}, err
	}
	return b, nil
}

// clearWithEntry records a posted entry as the clearing document of the
// items, clears each item with the entry's line that offsets it (line
// offsets[i] for items[i]), and publishes items_cleared. A settlement's
// entry offsets item i with line i+1; a reversal offsets each line with the
// line of the same position.
func clearWithEntry(ctx context.Context, tx pgx.Tx, actor reqctx.Principal, le uuid.UUID, date time.Time, entry Entry, items []item,
	offsets []int32) (uuid.UUID, error) {
	cleared, sets := make([]ItemRef, 0, 2*len(items)), make([]int, 0, 2*len(items))
	for i, it := range items {
		cleared = append(cleared, it.ref, ItemRef{FiscalYear: entry.FiscalYear, EntryID: entry.ID, LineNo: offsets[i]})
		sets = append(sets, i, i)
	}
	by := ItemRef{FiscalYear: entry.FiscalYear, EntryID: entry.ID}
	return recordClearing(ctx, tx, actor, clearingDoc{le: le, date: date, by: by}, cleared, sets, clearedItemsPayload(items, entry))
}

// clearingDoc is a clearing's header: the legal entity, the clearing date
// and the entry that is the clearing document (by.LineNo is unused).
type clearingDoc struct {
	le   uuid.UUID
	date time.Time
	by   ItemRef
}

// recordClearing records a clearing by the entry doc.by, marks the cleared
// lines, applies the clearing to their copies in the other ledgers
// (clearCopies, sets as there) and publishes items_cleared with the items'
// payload: one path for a clearing that posts its entry (clearWithEntry)
// and one whose entry was migrated already posted (Postings.Clear), which
// clears no line of it.
func recordClearing(ctx context.Context, tx pgx.Tx, actor reqctx.Principal, doc clearingDoc, cleared []ItemRef, sets []int,
	payload []map[string]any) (uuid.UUID, error) {
	id, err := insertClearing(ctx, tx, actor, doc)
	if err != nil {
		return uuid.Nil, err
	}
	if err := markCleared(ctx, tx, actor.TenantID, id, cleared); err != nil {
		return uuid.Nil, err
	}
	if err := clearCopies(ctx, tx, actor, doc, cleared, sets); err != nil {
		return uuid.Nil, err
	}
	if _, err := outbox.Add(ctx, outbox.Event{Context: "ledger", Name: "items_cleared", AggregateType: "ledger.clearing", AggregateID: id,
		Payload: map[string]any{"clearing_id": id, "legal_entity_id": doc.le, "clearing_date": doc.date.Format(time.DateOnly),
			"clearing_entry_id": doc.by.EntryID, "clearing_entry_year": doc.by.FiscalYear, "items": payload}}); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// insertClearing inserts the clearing's header row.
func insertClearing(ctx context.Context, tx pgx.Tx, actor reqctx.Principal, doc clearingDoc) (uuid.UUID, error) {
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.clearing (tenant_id, id, legal_entity_id, clearing_date, clearing_entry_year, clearing_entry_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, actor.TenantID, id, doc.le, doc.date, doc.by.FiscalYear, doc.by.EntryID, actor.ID); err != nil {
		return uuid.Nil, fmt.Errorf("ledger: record clearing %s by entry %s: %w", id, doc.by.EntryID, err)
	}
	return id, nil
}

// leadingLines are the line numbers 1..n: the lines of an entry that
// offsets n items in their order, one line per item, as a settlement's and
// a clearing's entry do.
func leadingLines(n int) []int32 {
	lines := make([]int32, n)
	for i := range lines {
		lines[i] = int32(i + 1)
	}
	return lines
}

// settledItemLines offsets each item on its account at its original
// amounts and returns the lines with the items' totals per currency. The
// items must be open items on reconciliation accounts of the legal entity,
// in one ledger and in the settlement's currency.
func settledItemLines(items []item, in Settlement, legalEntity uuid.UUID) (lines []Line, tc, lc, gc money.Minor, err error) {
	return offsetItems(items, in.Currency, in.HeaderText, func(i int, it item) error { return checkItem(i, it, &legalEntity, in.ReservedFor) })
}

// offsetItems offsets each item on its account at its original amounts,
// once check passes it, and returns the lines with the items' totals per
// currency: the items share the first's ledger and the currency. A
// settlement offsets supplier items so, and a bank statement line its
// payments' lines on the bank's clearing account (PostBank).
func offsetItems(items []item, currency string, text *string, check func(int, item) error) (lines []Line, tc, lc, gc money.Minor, err error) {
	lines = make([]Line, 0, len(items)+3)
	tcs, lcs, gcs := make([]money.Minor, len(items)), make([]money.Minor, len(items)), make([]money.Minor, len(items))
	for i, it := range items {
		if err := check(i, it); err != nil {
			return nil, 0, 0, 0, err
		}
		if it.currency != currency || it.ledger != items[0].ledger {
			return nil, 0, 0, 0, problem.Unprocessable("ledger.mixed_items", "The items of one settlement share their ledger and transaction currency "+currency+".")
		}
		lines = append(lines, Line{AccountID: it.account, TC: -it.tc, LC: -it.lc, GC: -it.gc, PartnerID: it.partner, Text: text})
		tcs[i], lcs[i], gcs[i] = it.tc, it.lc, it.gc
	}
	if tc, err = money.Sum(tcs...); err == nil {
		lc, err = money.Sum(lcs...)
	}
	if err == nil {
		gc, err = money.Sum(gcs...)
	}
	if err != nil {
		return nil, 0, 0, 0, errTotalTooLarge("items")
	}
	return lines, tc, lc, gc, nil
}

// exchangeDifferences is what the bank line leaves of the items' local and
// group totals: items booked at other rates than the settlement's differ,
// and the difference of two amounts in range can be out of range.
func exchangeDifferences(lc, gc money.Minor, bank settlementBank) (diffLC, diffGC money.Minor, err error) {
	if diffLC, err = money.Diff(lc, bank.lc); err == nil {
		diffGC, err = money.Diff(gc, bank.gc)
	}
	if err != nil {
		return 0, 0, errTotalTooLarge("items")
	}
	return diffLC, diffGC, nil
}

// exchangeDifferenceLines books what the bank line leaves in local and in
// group currency on the fx_gain or fx_loss account of the account
// determination: one line per currency with a difference.
func exchangeDifferenceLines(ctx context.Context, legalEntity uuid.UUID, lc, gc money.Minor, bank settlementBank, text *string) ([]Line, error) {
	diffLC, diffGC, err := exchangeDifferences(lc, gc, bank)
	if err != nil {
		return nil, err
	}
	return fxLines(ctx, legalEntity, diffLC, diffGC, text)
}

// fxLines books exchange differences in local and in group currency, debit
// positive, on the fx_gain or fx_loss account of the account
// determination: one line per currency with a difference.
func fxLines(ctx context.Context, legalEntity uuid.UUID, diffLC, diffGC money.Minor, text *string) ([]Line, error) {
	var lines []Line
	for _, d := range []Line{{LC: diffLC}, {GC: diffGC}} {
		if d.LC == 0 && d.GC == 0 {
			continue
		}
		key := "fx_gain" // the posting settles for less than the items were booked at
		if d.LC+d.GC > 0 {
			key = "fx_loss"
		}
		account, ok, err := Accounts{}.Determination(ctx, legalEntity, key)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, problem.Unprocessable("ledger.no_fx_account", "The posting realizes an exchange difference, and the legal entity has no "+key+
				" account in its account determination.")
		}
		d.AccountID, d.Text = account, text
		lines = append(lines, d)
	}
	return lines, nil
}
