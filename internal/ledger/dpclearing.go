package ledger

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// DownPaymentClearing clears down payments against the supplier's final
// invoice (SAP F-54; T46d, ADR-0037).
type DownPaymentClearing struct {
	LegalEntityID    uuid.UUID
	SupplierID       uuid.UUID
	APAccountID      uuid.UUID // the supplier's ordinary reconciliation account, the invoice's
	DocumentType     string
	PostingDate      time.Time
	Currency         string
	Items            []ItemRef  // the down payments: special G/L items A of the supplier
	Terms            *ItemTerms // the invoice's terms, kept with the transfer's payables debit
	Reference        *string
	HeaderText       *string
	SourceContext    string
	SourceDocumentID uuid.UUID // the invoice
}

// ClearDownPayments posts the transfer that clears whole down payments
// against an invoice: Cr the down payment account (special G/L A) at each
// down payment's original amounts, which clears it; Cr input tax, the tax
// the down payment's entry claimed, reversed at its amounts and on its tax
// code, since the invoice claims the whole tax; Dr the supplier's payables
// account with their total, an open item with the invoice's terms that
// the next run nets against the invoice. Every line is at amounts already
// booked, so the transfer has no exchange difference.
func (p Postings) ClearDownPayments(ctx context.Context, in DownPaymentClearing) (Entry, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return Entry{}, err
	}
	if len(in.Items) == 0 {
		return Entry{}, problem.Unprocessable("ledger.no_items", "A down payment clearing needs at least one down payment.")
	}
	if err := requireDistinct(in.Items); err != nil {
		return Entry{}, err
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
	lines, err := transferLines(ctx, tx, in, items)
	if err != nil {
		return Entry{}, err
	}
	if err := e.checkAccounts(ctx, tx, le.ID, lines, false); err != nil {
		return Entry{}, err
	}
	ledgers, err := itemLedgers(ctx, tx, items)
	if err != nil {
		return Entry{}, err
	}
	taxItems, err := downPaymentTaxItems(ctx, tx, items)
	if err != nil {
		return Entry{}, err
	}
	entry, err := e.post(ctx, tx, actor, draft{entity: le, ledgers: ledgers, documentType: in.DocumentType, documentDate: in.PostingDate,
		postingDate: in.PostingDate, sourcedItems: taxItems, currencyTC: in.Currency, currencyLC: items[0].currencyLC, currencyGC: items[0].currencyGC,
		rateTCLC: items[0].rateTCLC, rateLCGC: items[0].rateLCGC, reference: in.Reference, headerText: in.HeaderText,
		sourceContext: in.SourceContext, sourceDocumentID: &in.SourceDocumentID, lines: lines})
	if err != nil {
		return Entry{}, err
	}
	if err := keepTerms(ctx, tx, entry, in.Terms); err != nil {
		return Entry{}, err
	}
	id, err := clearWithEntry(ctx, tx, actor, le.ID, in.PostingDate, entry, items, leadingLines(len(items)))
	if err != nil {
		return Entry{}, err
	}
	return entry, errors.Join(
		audit.Add(ctx, audit.Entry{Action: "ledger.journal_entry.post", ObjectType: "ledger.journal_entry", ObjectID: entry.ID, Outcome: "succeeded",
			Detail: map[string]any{"document_number": entry.DocumentNumber, "fiscal_year": entry.FiscalYear, "lines": len(lines),
				"source_context": entry.SourceContext}}),
		audit.Add(ctx, audit.Entry{Action: "ledger.clearing.create", ObjectType: "ledger.clearing", ObjectID: id, Outcome: "succeeded",
			Detail: map[string]any{"items": len(items), "clearing_entry_id": entry.ID, "document_number": entry.DocumentNumber}}))
}

// transferLines are the transfer's lines: one offsetting each down payment
// first (clearWithEntry clears item i with line i+1), the tax reversals,
// and the payables debit, which balances them in every currency.
func transferLines(ctx context.Context, tx pgx.Tx, in DownPaymentClearing, items []item) ([]Line, error) {
	special := SpecialDownPayment
	lines := make([]Line, 0, 2*len(items)+1)
	add := func(l Line) { lines = append(lines, l) }
	for i, it := range items {
		if err := checkDownPayment(i, it, in, items[0]); err != nil {
			return nil, err
		}
		add(Line{AccountID: it.account, TC: -it.tc, LC: -it.lc, GC: -it.gc, PartnerID: it.partner, SpecialGL: &special, Text: in.HeaderText,
			TradingPartnerID: it.tradingPartner})
	}
	seen := map[uuid.UUID]bool{}
	for _, it := range items {
		if seen[it.ref.EntryID] {
			continue // an entry's tax is reversed once, with all its down payments
		}
		seen[it.ref.EntryID] = true
		taxes, err := downPaymentTax(ctx, tx, it.ref, in.Items)
		if err != nil {
			return nil, err
		}
		for _, t := range taxes {
			add(Line{AccountID: t.AccountID, TC: -t.TC, LC: -t.LC, GC: -t.GC, TaxCode: t.TaxCode, Text: in.HeaderText})
		}
	}
	debit, err := payablesDebit(lines, in)
	return append(lines, debit), err
}

// downPaymentTaxItems are the tax items of the down payments' entries
// negated, each entry's once, on the transfer's tax date: the invoice
// claims the whole tax, so the transfer takes back what the down payment
// claimed, base and tax, at its rate (ADR-0049 §4).
func downPaymentTaxItems(ctx context.Context, tx pgx.Tx, items []item) ([]sourcedTaxItem, error) {
	var years []int16
	var entries []uuid.UUID
	for _, it := range items {
		if !slices.Contains(entries, it.ref.EntryID) {
			years, entries = append(years, it.ref.FiscalYear), append(entries, it.ref.EntryID)
		}
	}
	return negatedTaxItems(ctx, tx, years, entries, false)
}

// payablesDebit is the transfer's payables line: the supplier debited with
// what the other lines credit, in every currency.
func payablesDebit(lines []Line, in DownPaymentClearing) (Line, error) {
	var tcs, lcs, gcs []money.Minor
	for _, l := range lines {
		tcs, lcs, gcs = append(tcs, l.TC), append(lcs, l.LC), append(gcs, l.GC)
	}
	tc, errTC := money.Sum(tcs...)
	lc, errLC := money.Sum(lcs...)
	gc, errGC := money.Sum(gcs...)
	if errors.Join(errTC, errLC, errGC) != nil {
		return Line{}, errTotalTooLarge("down payments")
	}
	return Line{AccountID: in.APAccountID, TC: -tc, LC: -lc, GC: -gc, PartnerID: &in.SupplierID, Text: in.HeaderText}, nil
}

// checkDownPayment requires item i to be a down payment (special G/L A) of
// the supplier that the shared item check passes, in the clearing's
// currency and the first item's ledger.
func checkDownPayment(i int, it item, in DownPaymentClearing, first item) error {
	if it.found && it.recon && (it.special == nil || *it.special != SpecialDownPayment || it.partner == nil || *it.partner != in.SupplierID) {
		return problem.Unprocessable("ledger.not_a_down_payment", fmt.Sprintf("Item %d is not a down payment of the supplier (special G/L A).", i+1),
			problem.FieldError{Field: fmt.Sprintf("items[%d]", i), Code: "not_a_down_payment", Message: "a down payment of the invoice's supplier"})
	}
	plain := it
	plain.special = nil // the shared check refuses special items: here they are the point
	if err := checkItem(i, plain, &in.LegalEntityID, uuid.Nil, problem.FieldError{Field: fmt.Sprintf("items[%d]", i), Code: "not_an_open_item",
		Message: "a down payment of the invoice's supplier"}); err != nil {
		return err
	}
	if it.currency != in.Currency || it.ledger != first.ledger {
		return problem.Unprocessable("ledger.mixed_items", "The down payments of one clearing share the invoice's ledger and transaction currency "+in.Currency+".")
	}
	return nil
}

// downPaymentTax are the tax lines of a down payment's entry, which the
// clearing reverses whole. An entry with tax and a down payment not in this
// clearing is refused: its tax cannot be told apart.
func downPaymentTax(ctx context.Context, tx pgx.Tx, ref ItemRef, clearing []ItemRef) ([]Line, error) {
	rows, _ := tx.Query(ctx, `SELECT line_no, account_id, amount_tc, amount_lc, amount_gc, tax_code, special_gl IS NOT NULL FROM ledger.journal_line
		WHERE fiscal_year = $1 AND journal_entry_id = $2 AND (tax_code IS NOT NULL OR special_gl IS NOT NULL) ORDER BY line_no`, ref.FiscalYear, ref.EntryID)
	var taxes []Line
	left := false
	var l Line
	var isSpecial bool
	if _, err := pgx.ForEachRow(rows, []any{&l.LineNo, &l.AccountID, &l.TC, &l.LC, &l.GC, &l.TaxCode, &isSpecial}, func() error {
		switch {
		case !isSpecial:
			taxes = append(taxes, l)
		case !slices.Contains(clearing, ItemRef{FiscalYear: ref.FiscalYear, EntryID: ref.EntryID, LineNo: l.LineNo}):
			left = true
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("ledger: the tax of down payments of entry %d/%s: %w", ref.FiscalYear, ref.EntryID, err)
	}
	if left && len(taxes) > 0 {
		return nil, problem.Unprocessable("ledger.down_payment_tax_shared", "The down payment's entry has tax and other down payments; clear them together.")
	}
	return taxes, nil
}

// RequireDistinct refuses a list naming an item twice (422
// ledger.duplicate_item), for callers that check items before posting.
func RequireDistinct(refs []ItemRef) error { return requireDistinct(refs) }
