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
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Bank statement postings (ADR-0047, T49f): a statement line the payments
// context matched posts to the house bank's main bank account. A line that
// clears payments offsets each payment's line on the bank's clearing
// account, which must be open-item managed in the legal entity
// (ledger.company_account), at its original amounts in all three
// currencies, and clears it with the posting, as a settlement clears
// supplier items: the main bank line is converted at the posting date's
// rates, and what that leaves in local or group currency is a realized
// exchange difference. A line a posting rule takes (charges, interest)
// posts against the rule's account.

// BankPosting is a matched bank statement line to post.
type BankPosting struct {
	LegalEntityID uuid.UUID
	DocumentType  string
	PostingDate   time.Time
	Currency      string
	// BankAccountID is the house bank's main bank account. Its line is
	// Amount, the statement line's amount, debit positive: money the bank
	// booked in debits the account.
	BankAccountID uuid.UUID
	Amount        money.Minor
	// ClearingAccountID and Entries are what a line that clears payments
	// clears: each entry's open lines on the clearing account (Entries'
	// LineNo is unused), which together are Amount.
	ClearingAccountID uuid.UUID
	Entries           []ItemRef
	// CounterAccountID is, without Entries, the account Amount posts
	// against: a posting rule's.
	CounterAccountID uuid.UUID
	Reference        *string
	HeaderText       *string
	SourceContext    string
	SourceDocumentID uuid.UUID
}

// PostBank posts the statement line and clears what it clears. The legal
// entity is not checked against the caller's companies: the payments
// context reads the statement within them, or an approval of the posting
// authorises it (approvals.BankClearing), as for an approved manual entry.
func (p Postings) PostBank(ctx context.Context, in BankPosting) (Entry, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return Entry{}, err
	}
	e := p.endpoints()
	le, err := e.postableEntity(ctx, in.LegalEntityID)
	if err != nil {
		return Entry{}, err
	}
	b, err := e.bankLines(ctx, tx, in, le)
	if err != nil {
		return Entry{}, err
	}
	if err := e.checkAccounts(ctx, tx, le.ID, b.lines, false); err != nil {
		return Entry{}, err
	}
	entry, err := e.post(ctx, tx, actor, draft{entity: le, documentType: in.DocumentType, documentDate: in.PostingDate,
		postingDate: in.PostingDate, currencyTC: in.Currency, currencyLC: le.LocalCurrency, currencyGC: b.bank.group, rateTCLC: b.bank.rateLC.shown,
		rateLCGC: b.bank.rateGC.shown, reference: in.Reference, headerText: in.HeaderText, sourceContext: in.SourceContext,
		sourceDocumentID: &in.SourceDocumentID, lines: b.lines})
	if err != nil {
		return Entry{}, err
	}
	posted := audit.Add(ctx, audit.Entry{Action: "ledger.journal_entry.post", ObjectType: "ledger.journal_entry", ObjectID: entry.ID, Outcome: "succeeded",
		Detail: map[string]any{"document_number": entry.DocumentNumber, "fiscal_year": entry.FiscalYear, "lines": len(b.lines),
			"source_context": entry.SourceContext}})
	if len(b.items) == 0 {
		return entry, posted
	}
	// The entry's line i+1 offsets item i (offsetItems), as a settlement's does.
	id, err := clearWithEntry(ctx, tx, actor, le.ID, in.PostingDate, entry, b.items, leadingLines(len(b.items)))
	if err != nil {
		return Entry{}, err
	}
	return entry, errors.Join(posted, audit.Add(ctx, audit.Entry{Action: "ledger.clearing.create", ObjectType: "ledger.clearing", ObjectID: id,
		Outcome: "succeeded", Detail: map[string]any{"items": len(b.items), "clearing_entry_id": entry.ID, "document_number": entry.DocumentNumber}}))
}

// Postable refuses a posting date of the legal entity outside an open
// period of any ledger the company keeps, as a bank posting, which goes to
// them all, would (409 ledger.period_not_open): a bank statement line that
// waits for approval is checked first, since its approval could not post
// it either.
func (p Postings) Postable(ctx context.Context, legalEntity uuid.UUID, date time.Time) error {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	le, err := p.endpoints().postableEntity(ctx, legalEntity)
	if err != nil {
		return err
	}
	_, _, _, err = postingPeriod(ctx, tx, le, nil, date)
	return err
}

// bankEntry is what a bank posting posts: its lines, at the bank line's
// rates, and the items it clears. It posts in every ledger the company
// keeps, as a settlement does (ADR-0045): the clearing clears the items'
// copies in the parallel ledgers with the leading ledger's.
type bankEntry struct {
	lines []Line
	items []item
	bank  settlementBank
}

// bankLines are the posting's lines: the items' offsets first (the
// clearing clears item i with line i+1), the main bank line, and the
// exchange differences; or, without items, the main bank line and its
// counter line.
func (e *Endpoints) bankLines(ctx context.Context, tx pgx.Tx, in BankPosting, le tenancy.Entity) (bankEntry, error) {
	bank, err := e.bankAmounts(ctx, tx, in.Currency, in.PostingDate, le, in.Amount)
	if err != nil {
		return bankEntry{}, err
	}
	bankLine := Line{AccountID: in.BankAccountID, TC: in.Amount, LC: bank.lc, GC: bank.gc, Text: in.HeaderText}
	if len(in.Entries) == 0 {
		return bankEntry{lines: []Line{bankLine, {AccountID: in.CounterAccountID, TC: -in.Amount, LC: -bank.lc, GC: -bank.gc, Text: in.HeaderText}},
			bank: bank}, nil
	}
	items, err := accountItems(ctx, tx, le.ID, in.ClearingAccountID, in.Entries)
	if err != nil {
		return bankEntry{}, err
	}
	lines, tc, lc, gc, err := offsetItems(items, in.Currency, in.HeaderText, checkAccountItem)
	if err != nil {
		return bankEntry{}, err
	}
	if tc != in.Amount {
		return bankEntry{}, problem.Unprocessable("ledger.clearing_not_balanced", fmt.Sprintf("The open items on the clearing account total %s, the bank's line %s.",
			tc.Plain(bank.expTC), in.Amount.Plain(bank.expTC)))
	}
	fx, err := exchangeDifferenceLines(ctx, le.ID, lc, gc, bank, in.HeaderText)
	withTradingPartner(fx, sharedTradingPartner(items)) // a difference line takes the items' trading partner (ADR-0059 §4)
	return bankEntry{lines: append(append(lines, bankLine), fx...), items: items, bank: bank}, err
}

// accountItems are the entries' lines on the account, which must be
// open-item managed in the legal entity, in the order of the entries.
func accountItems(ctx context.Context, tx pgx.Tx, le, account uuid.UUID, entries []ItemRef) ([]item, error) {
	var managed bool
	if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT is_open_item_managed FROM ledger.company_account WHERE legal_entity_id = $1 AND account_id = $2), false)`,
		le, account).Scan(&managed); err != nil {
		return nil, fmt.Errorf("ledger: is account %s open-item managed: %w", account, err)
	}
	if !managed {
		return nil, problem.Unprocessable("ledger.not_open_item_managed",
			"The bank's clearing account is not open-item managed in the legal entity (chart_of_accounts[].open_items); a statement line clears its lines only there.")
	}
	years, ids, _ := columns(entries)
	rows, _ := tx.Query(ctx, `SELECT x.y, x.je, l.line_no FROM unnest($1::smallint[], $2::uuid[]) WITH ORDINALITY AS x(y, je, ord)
		LEFT JOIN ledger.journal_line l ON l.fiscal_year = x.y AND l.journal_entry_id = x.je AND l.account_id = $3 AND l.legal_entity_id = $4
		ORDER BY x.ord, l.line_no`, years, ids, account, le)
	var refs []ItemRef
	var ref ItemRef
	var no *int32
	var missing *uuid.UUID
	if _, err := pgx.ForEachRow(rows, []any{&ref.FiscalYear, &ref.EntryID, &no}, func() error {
		if no == nil {
			id := ref.EntryID
			missing = &id
			return nil
		}
		ref.LineNo = *no
		refs = append(refs, ref)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("ledger: lines of %d entries on account %s: %w", len(entries), account, err)
	}
	if missing != nil {
		return nil, problem.Unprocessable("ledger.not_an_open_item", "Entry "+missing.String()+" has no line on the clearing account in the legal entity.")
	}
	return loadItems(ctx, tx, refs)
}

// checkAccountItem checks item i, a line on an open-item managed account:
// not of a reversed entry or a reversal, and not cleared. Payment runs
// reserve supplier items only, so no reservation holds it.
func checkAccountItem(i int, it item) error {
	if !it.found { // a parallel ledger's copy: open items are the leading ledger's (ADR-0045)
		return problem.Unprocessable("ledger.not_an_open_item", "Item "+fmt.Sprint(i+1)+" is not a line of the leading ledger.")
	}
	if it.reversed {
		return reversedItem(i)
	}
	if !it.open {
		return problem.Conflict("ledger.item_already_cleared", "Item "+fmt.Sprint(i+1)+" is already cleared.")
	}
	return nil
}
