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
	Amount       money.Minor
	// Terms are the payment terms the item was posted with (T36b), or nil
	// for an item posted without (before T36, manual or migrated).
	Terms *ItemTerms
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
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT l.fiscal_year, l.journal_entry_id, l.line_no, l.account_id, l.partner_id, e.document_date, l.amount_tc,
			`+termsColumns+`
		FROM ledger.journal_line l
		LEFT JOIN ledger.open_item_terms t ON t.tenant_id = l.tenant_id AND t.fiscal_year = l.fiscal_year AND t.journal_entry_id = l.journal_entry_id
			AND t.line_no = l.line_no
		JOIN ledger.ledger g ON g.tenant_id = l.tenant_id AND g.id = l.ledger_id AND g.is_leading
		JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id AND a.reconciliation_kind = 'supplier'
		JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id
		WHERE l.legal_entity_id = $1 AND e.currency_tc = $2 AND e.document_date <= $3
			AND NOT EXISTS (SELECT FROM ledger.clearing_item c WHERE c.tenant_id = l.tenant_id AND c.fiscal_year = l.fiscal_year
				AND c.journal_entry_id = l.journal_entry_id AND c.line_no = l.line_no)
			AND NOT `+reversedLine+`
			AND NOT EXISTS (SELECT FROM ledger.item_reservation ir WHERE ir.tenant_id = l.tenant_id AND ir.fiscal_year = l.fiscal_year
				AND ir.journal_entry_id = l.journal_entry_id AND ir.line_no = l.line_no AND ir.released_at IS NULL)
		ORDER BY l.partner_id, l.fiscal_year, l.journal_entry_id, l.line_no`, legalEntity, currency, documentDateTo)
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (PayableItem, error) {
		var p PayableItem
		var t storedTerms
		err := errs.Wrapf(r.Scan(append([]any{&p.FiscalYear, &p.EntryID, &p.LineNo, &p.AccountID, &p.PartnerID, &p.DocumentDate, &p.Amount},
			t.fields()...)...), "scan payable item")
		p.Terms = t.terms()
		return p, err
	})
	if err != nil {
		return items, fmt.Errorf("ledger: payable items of %s in %s: %w", legalEntity, currency, err)
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
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT id, coalesce(reference, document_number) FROM ledger.journal_entry WHERE id = ANY($1)`, entries)
	out := map[uuid.UUID]string{}
	var id uuid.UUID
	var ref string
	_, err = pgx.ForEachRow(rows, []any{&id, &ref}, func() error { out[id] = ref; return nil })
	if err != nil {
		return out, fmt.Errorf("ledger: references of %d entries: %w", len(entries), err)
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
// discounts taken, which go to the cash_discount account, both converted
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
	paid, bank, err := e.paymentLines(ctx, tx, in, le, items, settled{tc: tc, lc: lc, gc: gc})
	if err != nil {
		return Entry{}, err
	}
	lines = append(lines, paid...)
	if err := e.checkAccounts(ctx, tx, le.ID, lines, false); err != nil {
		return Entry{}, err
	}
	entry, err := e.post(ctx, tx, actor, draft{entity: le, ledgerID: items[0].ledger, documentType: in.DocumentType, documentDate: in.PostingDate,
		postingDate: in.PostingDate, currencyTC: in.Currency, currencyLC: le.LocalCurrency, currencyGC: bank.group, rateTCLC: bank.rateLC.shown, rateLCGC: bank.rateGC.shown,
		reference: in.Reference, headerText: in.HeaderText, sourceContext: in.SourceContext, sourceDocumentID: &in.SourceDocumentID, lines: lines})
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
// their total less the discounts taken (T36c); a line crediting the
// discounts to the cash_discount account; both converted at the posting
// date's rates, and what that leaves of the items' local and group totals
// as exchange differences.
func (e *Endpoints) paymentLines(ctx context.Context, tx pgx.Tx, in Settlement, le tenancy.Entity, items []item, total settled) ([]Line, settlementBank, error) {
	discount, bankTC, err := settlementDiscount(in, items, total.tc)
	if err != nil {
		return nil, settlementBank{}, err
	}
	bank, err := e.bankAmounts(ctx, tx, in, le, bankTC)
	if err != nil {
		return nil, settlementBank{}, err
	}
	lines := []Line{{AccountID: in.BankAccountID, TC: bankTC, LC: bank.lc, GC: bank.gc, Text: in.HeaderText}}
	paid := bank
	if discount != 0 {
		dl, err := discountLines(ctx, tx, le.ID, bank, items, in, discount)
		if err != nil {
			return nil, settlementBank{}, err
		}
		for _, d := range dl {
			if paid.lc, err = money.Sum(paid.lc, d.LC); err == nil {
				paid.gc, err = money.Sum(paid.gc, d.GC)
			}
			if err != nil {
				return nil, settlementBank{}, errTotalTooLarge("items")
			}
		}
		lines = append(lines, dl...)
	}
	fx, err := exchangeDifferenceLines(ctx, le.ID, total.lc, total.gc, paid, in.HeaderText)
	return append(lines, fx...), bank, err
}

// settlementDiscount is the total of the discounts taken on the items, in
// the sign they are paid in, and what the bank pays after them (the items'
// total tc, negative when paid out, less the discounts): one discount per
// item (or none at all), each 0 or of the item's sign and smaller than it
// (422 ledger.invalid_discount).
func settlementDiscount(in Settlement, items []item, tc money.Minor) (discount, bank money.Minor, err error) {
	if len(in.Discounts) == 0 {
		return 0, tc, nil
	}
	invalid := func(detail string) error {
		return problem.Unprocessable("ledger.invalid_discount", "A settlement's cash discounts "+detail+".")
	}
	if len(in.Discounts) != len(items) {
		return 0, 0, invalid(fmt.Sprintf("are one per item: %d for %d items", len(in.Discounts), len(items)))
	}
	for i, d := range in.Discounts {
		paid := -items[i].tc
		if d != 0 && (d == math.MinInt64 || (d > 0) != (paid > 0) || d.Abs() >= paid.Abs()) {
			return 0, 0, invalid(fmt.Sprintf("are of their item's sign and smaller than it: %d on an item paid at %d", d, paid))
		}
	}
	if discount, err = money.Sum(in.Discounts...); err == nil {
		bank, err = money.Sum(tc, discount)
	}
	if err != nil {
		return 0, 0, errTotalTooLarge("discounts")
	}
	return discount, bank, nil
}

// discountLines credit the discounts taken: their net part to the legal
// entity's cash_discount account (SAP SKE), and their tax share to each
// tax line's account with its tax code (discountTax), all at the
// settlement's rates.
func discountLines(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID, bank settlementBank, items []item, in Settlement, discount money.Minor) ([]Line, error) {
	account, ok, err := Accounts{}.Determination(ctx, legalEntity, "cash_discount")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, problem.Unprocessable("ledger.no_cash_discount_account", "The payment takes a cash discount, and the legal entity has no cash_discount account in its account determination.")
	}
	taxes, tax, err := discountTax(ctx, tx, items, in.Discounts)
	if err != nil {
		return nil, err
	}
	// The tax share is part of the discount, of its sign and at most it
	// (itemTaxShares), so the net is of the discount's sign or 0, when
	// rounding gives the tax the whole of a small discount: no line then.
	var lines []Line
	if net := discount - tax; net != 0 {
		lines = append(lines, Line{AccountID: account, TC: -net, Text: in.HeaderText})
	}
	for _, k := range slices.SortedFunc(maps.Keys(taxes), compareTaxKeys) {
		code := k.code
		lines = append(lines, Line{AccountID: k.account, TC: -taxes[k], TaxCode: &code, Text: in.HeaderText})
	}
	for i := range lines {
		if lines[i].LC, lines[i].GC, err = bank.convert(lines[i].TC); err != nil {
			return nil, err
		}
	}
	return lines, nil
}

// bankAmounts converts the settled transaction amount to local currency,
// and that to group currency, at the average rates of the posting date.
func (e *Endpoints) bankAmounts(ctx context.Context, tx pgx.Tx, in Settlement, le tenancy.Entity, tc money.Minor) (settlementBank, error) {
	var b settlementBank
	var err error
	if b.group, err = e.LegalEntities.GroupCurrency(ctx); err != nil {
		return settlementBank{}, err
	}
	exps, err := exponents(ctx, tx, in.Currency, le.LocalCurrency, b.group)
	if err != nil {
		return settlementBank{}, err
	}
	if b.rateLC, b.rateGC, err = postingRates(ctx, tx, in.Currency, le.LocalCurrency, b.group, in.PostingDate); err != nil {
		return settlementBank{}, err
	}
	b.expTC, b.expLC, b.expGC = exps[in.Currency], exps[le.LocalCurrency], exps[b.group]
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
	cleared := make([]ItemRef, 0, 2*len(items))
	for i, it := range items {
		cleared = append(cleared, it.ref, ItemRef{FiscalYear: entry.FiscalYear, EntryID: entry.ID, LineNo: offsets[i]})
	}
	by := ItemRef{FiscalYear: entry.FiscalYear, EntryID: entry.ID}
	return recordClearing(ctx, tx, actor, clearingDoc{le: le, date: date, by: by}, cleared, clearedItemsPayload(items, entry))
}

// clearingDoc is a clearing's header: the legal entity, the clearing date
// and the entry that is the clearing document (by.LineNo is unused).
type clearingDoc struct {
	le   uuid.UUID
	date time.Time
	by   ItemRef
}

// recordClearing records a clearing by the entry doc.by, marks the cleared
// lines and publishes items_cleared with the items' payload: one path for a
// clearing that posts its entry (clearWithEntry) and one whose entry was
// migrated already posted (Postings.Clear), which clears no line of it.
func recordClearing(ctx context.Context, tx pgx.Tx, actor reqctx.Principal, doc clearingDoc, cleared []ItemRef,
	payload []map[string]any) (uuid.UUID, error) {
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.clearing (tenant_id, id, legal_entity_id, clearing_date, clearing_entry_year, clearing_entry_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, actor.TenantID, id, doc.le, doc.date, doc.by.FiscalYear, doc.by.EntryID, actor.ID); err != nil {
		return uuid.Nil, fmt.Errorf("ledger: record clearing %s by entry %s: %w", id, doc.by.EntryID, err)
	}
	if err := markCleared(ctx, tx, actor.TenantID, id, cleared); err != nil {
		return uuid.Nil, err
	}
	if _, err := outbox.Add(ctx, outbox.Event{Context: "ledger", Name: "items_cleared", AggregateType: "ledger.clearing", AggregateID: id,
		Payload: map[string]any{"clearing_id": id, "legal_entity_id": doc.le, "clearing_date": doc.date.Format(time.DateOnly),
			"clearing_entry_id": doc.by.EntryID, "clearing_entry_year": doc.by.FiscalYear, "items": payload}}); err != nil {
		return uuid.Nil, err
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
	lines = make([]Line, 0, len(items)+3)
	tcs, lcs, gcs := make([]money.Minor, len(items)), make([]money.Minor, len(items)), make([]money.Minor, len(items))
	for i, it := range items {
		if err := checkItem(i, it, &legalEntity, in.ReservedFor); err != nil {
			return nil, 0, 0, 0, err
		}
		if it.currency != in.Currency || it.ledger != items[0].ledger {
			return nil, 0, 0, 0, problem.Unprocessable("ledger.mixed_items", "The items of one settlement share their ledger and transaction currency "+in.Currency+".")
		}
		lines = append(lines, Line{AccountID: it.account, TC: -it.tc, LC: -it.lc, GC: -it.gc, PartnerID: it.partner, Text: in.HeaderText})
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
