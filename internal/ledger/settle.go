package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
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
	Amount       int64
}

// Payable returns the legal entity's open supplier items in the currency
// with a document date on or before the date, ordered by partner and item.
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
		ORDER BY l.partner_id, l.fiscal_year, l.journal_entry_id, l.line_no`, legalEntity, currency, documentDateTo)
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PayableItem, error) {
		var p PayableItem
		err := r.Scan(&p.FiscalYear, &p.EntryID, &p.LineNo, &p.AccountID, &p.PartnerID, &p.DocumentDate, &p.Amount)
		return p, err
	})
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
	return out, err
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
	lines := make([]Line, 0, len(items)+3)
	var tc, lc, gc int64
	for i, it := range items {
		switch {
		case !it.found || !it.recon || it.legalEntity != le.ID:
			return Entry{}, problem.Unprocessable("ledger.not_an_open_item", "Item "+fmt.Sprint(i+1)+" is not a line on a reconciliation account of this legal entity.")
		case !it.open:
			return Entry{}, problem.Conflict("ledger.item_already_cleared", "Item "+fmt.Sprint(i+1)+" is already cleared.")
		case it.currency != in.Currency || it.ledger != items[0].ledger:
			return Entry{}, problem.Unprocessable("ledger.mixed_items", "The items of one settlement share their ledger and transaction currency "+in.Currency+".")
		}
		lines = append(lines, Line{AccountID: it.account, TC: -it.tc, LC: -it.lc, GC: -it.gc, PartnerID: it.partner, Text: in.HeaderText})
		tc, lc, gc = tc+it.tc, lc+it.lc, gc+it.gc
	}
	group, err := e.LegalEntities.GroupCurrency(ctx)
	if err != nil {
		return Entry{}, err
	}
	exps, err := exponents(ctx, tx, in.Currency, le.LocalCurrency, group)
	if err != nil {
		return Entry{}, err
	}
	rateLC, err := rate(ctx, tx, in.Currency, le.LocalCurrency, in.PostingDate)
	if err != nil {
		return Entry{}, err
	}
	rateGC, err := rate(ctx, tx, le.LocalCurrency, group, in.PostingDate)
	if err != nil {
		return Entry{}, err
	}
	bankLC, err := convert(tc, rateLC, exps[in.Currency], exps[le.LocalCurrency])
	if err != nil {
		return Entry{}, err
	}
	bankGC, err := convert(bankLC, rateGC, exps[le.LocalCurrency], exps[group])
	if err != nil {
		return Entry{}, err
	}
	lines = append(lines, Line{AccountID: in.BankAccountID, TC: tc, LC: bankLC, GC: bankGC, Text: in.HeaderText})
	for _, d := range []Line{{LC: lc - bankLC}, {GC: gc - bankGC}} {
		if d.LC == 0 && d.GC == 0 {
			continue
		}
		key := "fx_gain" // the payment cost less than the items were booked at
		if d.LC+d.GC > 0 {
			key = "fx_loss"
		}
		account, ok, err := Accounts{}.Determination(ctx, le.ID, key)
		if err != nil {
			return Entry{}, err
		}
		if !ok {
			return Entry{}, problem.Unprocessable("ledger.no_fx_account", "The payment realizes an exchange difference, and the legal entity has no "+key+
				" account in its account determination.")
		}
		d.AccountID, d.Text = account, in.HeaderText
		lines = append(lines, d)
	}
	if err := e.checkAccounts(ctx, tx, le.ID, lines, false); err != nil {
		return Entry{}, err
	}
	entry, err := e.post(ctx, tx, actor, draft{entity: le, ledgerID: items[0].ledger, documentType: in.DocumentType, documentDate: in.PostingDate,
		postingDate: in.PostingDate, currencyTC: in.Currency, currencyLC: le.LocalCurrency, currencyGC: group, rateTCLC: rateLC, rateLCGC: rateGC,
		reference: in.Reference, headerText: in.HeaderText, sourceContext: in.SourceContext, sourceDocumentID: &in.SourceDocumentID, lines: lines})
	if err != nil {
		return Entry{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.clearing (tenant_id, id, legal_entity_id, clearing_date, clearing_entry_year, clearing_entry_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, actor.TenantID, id, le.ID, in.PostingDate, entry.FiscalYear, entry.ID, actor.ID); err != nil {
		return Entry{}, err
	}
	cleared := make([]ItemRef, 0, 2*len(items))
	payload := make([]map[string]any, len(items))
	for i, it := range items {
		cleared = append(cleared, it.ref, ItemRef{FiscalYear: entry.FiscalYear, EntryID: entry.ID, LineNo: int32(i + 1)})
		payload[i] = map[string]any{"fiscal_year": it.ref.FiscalYear, "journal_entry_id": it.ref.EntryID, "line_no": it.ref.LineNo,
			"account_id": it.account, "partner_id": it.partner, "amount_tc": amount(it.tc, entry.CurrencyTC, entry.ExpTC)}
	}
	if err := markCleared(ctx, tx, actor.TenantID, id, cleared); err != nil {
		return Entry{}, err
	}
	if _, err := outbox.Add(ctx, outbox.Event{Context: "ledger", Name: "items_cleared", AggregateType: "ledger.clearing", AggregateID: id,
		Payload: map[string]any{"clearing_id": id, "legal_entity_id": le.ID, "clearing_date": in.PostingDate.Format(time.DateOnly),
			"clearing_entry_id": entry.ID, "clearing_entry_year": entry.FiscalYear, "items": payload}}); err != nil {
		return Entry{}, err
	}
	return entry, errors.Join(
		audit.Add(ctx, audit.Entry{Action: "ledger.journal_entry.post", ObjectType: "ledger.journal_entry", ObjectID: entry.ID, Outcome: "succeeded",
			Detail: map[string]any{"document_number": entry.DocumentNumber, "fiscal_year": entry.FiscalYear, "lines": len(lines),
				"source_context": entry.SourceContext}}),
		audit.Add(ctx, audit.Entry{Action: "ledger.clearing.create", ObjectType: "ledger.clearing", ObjectID: id, Outcome: "succeeded",
			Detail: map[string]any{"items": len(items), "clearing_entry_id": entry.ID, "document_number": entry.DocumentNumber}}))
}
