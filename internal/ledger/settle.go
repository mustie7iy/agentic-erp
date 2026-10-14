package ledger

import (
	"context"
	"errors"
	"fmt"
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
}

// Payable returns the legal entity's open supplier items in the currency
// with a document date on or before the date, ordered by partner and item.
// Lines of a reversed entry or of a reversal are left out: the reversal
// cancelled them, and Settle refuses them. A reversal clears them, so this
// matters only for entries reversed before reversals cleared the pair.
func (OpenItems) Payable(ctx context.Context, legalEntity uuid.UUID, currency string, documentDateTo time.Time) ([]PayableItem, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT l.fiscal_year, l.journal_entry_id, l.line_no, l.account_id, l.partner_id, e.document_date, l.amount_tc
		FROM ledger.journal_line l
		JOIN ledger.ledger g ON g.tenant_id = l.tenant_id AND g.id = l.ledger_id AND g.is_leading
		JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id AND a.reconciliation_kind = 'supplier'
		JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id
		WHERE l.legal_entity_id = $1 AND e.currency_tc = $2 AND e.document_date <= $3
			AND NOT EXISTS (SELECT FROM ledger.clearing_item c WHERE c.tenant_id = l.tenant_id AND c.fiscal_year = l.fiscal_year
				AND c.journal_entry_id = l.journal_entry_id AND c.line_no = l.line_no)
			AND NOT EXISTS (SELECT FROM ledger.reversal r WHERE r.tenant_id = l.tenant_id
				AND ((r.original_year = l.fiscal_year AND r.original_entry_id = l.journal_entry_id)
					OR (r.reversal_year = l.fiscal_year AND r.reversal_entry_id = l.journal_entry_id)))
		ORDER BY l.partner_id, l.fiscal_year, l.journal_entry_id, l.line_no`, legalEntity, currency, documentDateTo)
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (PayableItem, error) {
		var p PayableItem
		err := errs.Wrapf(r.Scan(&p.FiscalYear, &p.EntryID, &p.LineNo, &p.AccountID, &p.PartnerID, &p.DocumentDate, &p.Amount), "scan payable item")
		return p, err
	})
	if err != nil {
		return items, fmt.Errorf("ledger: payable items of %s in %s: %w", legalEntity, currency, err)
	}
	return items, nil
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
// currencies; the bank is credited with their total, converted at the
// rates of the posting date; what that leaves in local or group currency
// is a realized exchange difference on the account determination's
// fx_gain or fx_loss account. The partner lines are cleared with the items.
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
	bank, err := e.bankAmounts(ctx, tx, in, le, tc)
	if err != nil {
		return Entry{}, err
	}
	lines = append(lines, Line{AccountID: in.BankAccountID, TC: tc, LC: bank.lc, GC: bank.gc, Text: in.HeaderText})
	fx, err := exchangeDifferenceLines(ctx, le.ID, lc, gc, bank, in.HeaderText)
	if err != nil {
		return Entry{}, err
	}
	lines = append(lines, fx...)
	if err := e.checkAccounts(ctx, tx, le.ID, lines, false); err != nil {
		return Entry{}, err
	}
	entry, err := e.post(ctx, tx, actor, draft{entity: le, ledgerID: items[0].ledger, documentType: in.DocumentType, documentDate: in.PostingDate,
		postingDate: in.PostingDate, currencyTC: in.Currency, currencyLC: le.LocalCurrency, currencyGC: bank.group, rateTCLC: bank.rateLC, rateLCGC: bank.rateGC,
		reference: in.Reference, headerText: in.HeaderText, sourceContext: in.SourceContext, sourceDocumentID: &in.SourceDocumentID, lines: lines})
	if err != nil {
		return Entry{}, err
	}
	offsets := make([]int32, len(items))
	for i := range items {
		offsets[i] = int32(i + 1)
	}
	id, err := clearWithEntry(ctx, tx, actor, le.ID, in.PostingDate, entry, items, offsets)
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
// rates of the posting date they were converted at.
type settlementBank struct {
	group          string
	rateLC, rateGC string
	lc, gc         money.Minor
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
	if b.rateLC, err = rate(ctx, tx, in.Currency, le.LocalCurrency, in.PostingDate); err != nil {
		return settlementBank{}, err
	}
	if b.rateGC, err = rate(ctx, tx, le.LocalCurrency, b.group, in.PostingDate); err != nil {
		return settlementBank{}, err
	}
	if b.lc, err = convert(tc, b.rateLC, exps[in.Currency], exps[le.LocalCurrency]); err != nil {
		return settlementBank{}, err
	}
	if b.gc, err = convert(b.lc, b.rateGC, exps[le.LocalCurrency], exps[b.group]); err != nil {
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
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.clearing (tenant_id, id, legal_entity_id, clearing_date, clearing_entry_year, clearing_entry_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, actor.TenantID, id, le, date, entry.FiscalYear, entry.ID, actor.ID); err != nil {
		return uuid.Nil, fmt.Errorf("ledger: clear with entry %s: record clearing %s: %w", entry.ID, id, err)
	}
	cleared := make([]ItemRef, 0, 2*len(items))
	for i, it := range items {
		cleared = append(cleared, it.ref, ItemRef{FiscalYear: entry.FiscalYear, EntryID: entry.ID, LineNo: offsets[i]})
	}
	payload := clearedItemsPayload(items, entry)
	if err := markCleared(ctx, tx, actor.TenantID, id, cleared); err != nil {
		return uuid.Nil, err
	}
	if _, err := outbox.Add(ctx, outbox.Event{Context: "ledger", Name: "items_cleared", AggregateType: "ledger.clearing", AggregateID: id,
		Payload: map[string]any{"clearing_id": id, "legal_entity_id": le, "clearing_date": date.Format(time.DateOnly),
			"clearing_entry_id": entry.ID, "clearing_entry_year": entry.FiscalYear, "items": payload}}); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// settledItemLines offsets each item on its account at its original
// amounts and returns the lines with the items' totals per currency. The
// items must be open items on reconciliation accounts of the legal entity,
// in one ledger and in the settlement's currency.
func settledItemLines(items []item, in Settlement, legalEntity uuid.UUID) (lines []Line, tc, lc, gc money.Minor, err error) {
	lines = make([]Line, 0, len(items)+3)
	tcs, lcs, gcs := make([]money.Minor, len(items)), make([]money.Minor, len(items)), make([]money.Minor, len(items))
	for i, it := range items {
		switch {
		case !it.found || !it.recon || it.legalEntity != legalEntity:
			return nil, 0, 0, 0, problem.Unprocessable("ledger.not_an_open_item", "Item "+fmt.Sprint(i+1)+" is not a line on a reconciliation account of this legal entity.")
		case it.reversed:
			return nil, 0, 0, 0, reversedItem(i)
		case !it.open:
			return nil, 0, 0, 0, problem.Conflict("ledger.item_already_cleared", "Item "+fmt.Sprint(i+1)+" is already cleared.")
		case it.reservedBy != nil && it.reservedBy.ID != in.ReservedFor:
			return nil, 0, 0, 0, reservedItem(i, *it.reservedBy)
		case it.currency != in.Currency || it.ledger != items[0].ledger:
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
	var lines []Line
	for _, d := range []Line{{LC: diffLC}, {GC: diffGC}} {
		if d.LC == 0 && d.GC == 0 {
			continue
		}
		key := "fx_gain" // the payment cost less than the items were booked at
		if d.LC+d.GC > 0 {
			key = "fx_loss"
		}
		account, ok, err := Accounts{}.Determination(ctx, legalEntity, key)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, problem.Unprocessable("ledger.no_fx_account", "The payment realizes an exchange difference, and the legal entity has no "+key+
				" account in its account determination.")
		}
		d.AccountID, d.Text = account, text
		lines = append(lines, d)
	}
	return lines, nil
}
