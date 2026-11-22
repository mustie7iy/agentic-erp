package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Receipts applied (ADR-0063 §6, T75c; ADR-0047): an incoming payment a
// bank statement posted waits as an open credit line on the house bank's
// incoming payments clearing account until accounts receivable applies it
// to the customer's open items. The application is one clearing entry
// that offsets the receipt and every item at their original amounts in
// all three currencies, as a settlement offsets what it pays; what that
// leaves in local or group currency, the items and the receipt having
// been booked at different rates, is a realized exchange difference.

// ReceiptApplication applies a receipt to a customer's open items.
type ReceiptApplication struct {
	LegalEntityID uuid.UUID
	DocumentType  string
	PostingDate   time.Time
	// Currency is the receipt's; an item in another is translated into it
	// at the average rate valid on PostingDate (translateItems, T75n).
	Currency string
	// ReceiptAccountID is the open-item managed account the receipt waits
	// on, a house bank's incoming payments clearing account, and Receipt
	// the entry that posted it (LineNo is unused): its one line there, a
	// credit.
	ReceiptAccountID uuid.UUID
	Receipt          ItemRef
	// CustomerID is the customer whose items Items are, the payer, with
	// OtherCustomers, the other customers the receipt pays for (T75h): open
	// items on customer reconciliation accounts, invoices and credits
	// alike, each of one of them, that together, net of Residuals, are the
	// receipt but for what OnAccount takes or the tolerance writes off.
	CustomerID     uuid.UUID
	OtherCustomers []uuid.UUID
	Items          []ItemRef
	// Residuals, one per item or none, are what stays open of the items
	// paid in part (T75f): a residual's Amount, more than zero and less
	// than its debit item, is posted as a new item of the customer on the
	// item's account, at the item's rate, with its due date; zero: the
	// item is paid in full.
	Residuals []Residual
	// CashDiscounts, one per item or none, say which items take the cash
	// discount their payment terms grant for a payment received on
	// DiscountDate, the receipt's day (receiptdiscount.go): an invoice
	// paid in full, its discount of its amount in the receipt's currency,
	// split by its entry's tax so that it reduces the output tax it levied.
	CashDiscounts []bool
	DiscountDate  time.Time
	// Under and Over bound the payment difference the application writes
	// off (config.PaymentTolerance, T75f): what the items, net of their
	// residuals, exceed or fall short of the receipt by. Zero limits allow
	// none, so the items must total the receipt exactly.
	Under, Over config.ToleranceLimit
	// OnAccount is the customer reconciliation account what the receipt
	// leaves after the items is posted to as the customer's credit on
	// account (T75g); uuid.Nil: none, so nothing may be left beyond the
	// overpayment tolerance.
	OnAccount uuid.UUID
	// Credit says the receipt is a credit of the customer's on account
	// (T75g): Receipt names its line, on ReceiptAccountID, a customer
	// reconciliation account, with the customer as its partner.
	Credit           bool
	Reference        *string
	HeaderText       *string
	SourceContext    string
	SourceDocumentID uuid.UUID
}

// Residual is what stays open of an item paid in part, with the text its
// line carries (nil: the application's header text). WriteOff is the
// account its reason writes it off to instead (T75o), uuid.Nil for a
// residual item.
type Residual struct {
	Amount   money.Minor
	Text     *string
	WriteOff uuid.UUID
}

// AppliedReceipt is a receipt applied: the clearing entry, what the
// receipt credited (positive) and each item's amount in the order of the
// application's Items, debit positive (a credit of the customer's is
// negative), in the receipt's currency, translated for an item in another
// (Translations). The items add up to the receipt.
type AppliedReceipt struct {
	Entry   Entry
	Receipt money.Minor
	Items   []money.Minor
	// Customers are the customer of each item, in the order of Items: the
	// application's CustomerID or one of its OtherCustomers.
	Customers []uuid.UUID
	// ResidualLines are the entry's line of each item's residual item, in
	// the order of Items; 0 for an item paid in full. WriteOffLines are
	// likewise the line of what each item's reason wrote off (T75o).
	ResidualLines []int32
	WriteOffLines []int32
	// Discounts are the cash discount each item took, positive, in the
	// order of Items, nil when none did; Discount is their total.
	Discounts []money.Minor
	Discount  money.Minor
	// Difference is what the items, net of their residuals, exceed the
	// receipt by and was written off: positive an underpayment, negative an
	// overpayment, zero for none.
	Difference money.Minor
	// OnAccount is what the receipt left on the customer's account, a
	// credit, positive; OnAccountLine its line in the entry, 0 for none.
	OnAccount     money.Minor
	OnAccountLine int32
	// Translations say how each item, in the order of Items, was
	// translated into the receipt's currency (T75n): the zero value for an
	// item in it.
	Translations []Translation
}

// ApplyReceipt posts the entry that applies the receipt to the customer's
// items and clears them all with it: Dr the receipt's account and Cr (or,
// for a credit of the customer's, Dr) each item's account with the
// customer, each at the amounts it was booked at; then each item's
// residual item, what the receipt leaves on the customer's account
// (OnAccount) or the payment difference within the tolerance (Under,
// Over), and the difference in local or group currency on the account
// determination's fx_gain or fx_loss account (applicationLines). An item
// may take its cash discount (CashDiscounts), whose correction of the
// output tax writes the entry's tax items (receiptDiscounts). The items,
// net of their residuals and discounts, must total the receipt in its
// currency but for those (422 ledger.clearing_not_balanced).
//
// The receipt's line and the items are read in one loadItems, under one
// sorted set of entry locks taken before any number range, as a clearing
// and a settlement take theirs (lockEntries): a reversal or a reservation
// of any of them finishes first or waits. Two applications of one receipt
// or item do not wait for each other; the second to mark its lines
// cleared is refused (clearing_item_pkey, 409 ledger.item_already_cleared).
func (p Postings) ApplyReceipt(ctx context.Context, in ReceiptApplication) (AppliedReceipt, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return AppliedReceipt{}, err
	}
	if err := checkApplication(in); err != nil {
		return AppliedReceipt{}, err
	}
	e := p.endpoints()
	le, err := e.legalEntity(ctx, in.LegalEntityID, "legal_entity_id")
	if err != nil {
		return AppliedReceipt{}, err
	}
	receipt, err := receiptLine(ctx, tx, le.ID, in)
	if err != nil {
		return AppliedReceipt{}, err
	}
	items, err := loadItems(ctx, tx, append([]ItemRef{receipt}, in.Items...))
	if err != nil {
		return AppliedReceipt{}, err
	}
	plan, err := receiptLines(ctx, tx, in, le.ID, items)
	if err != nil {
		return AppliedReceipt{}, err
	}
	lines := plan.lines
	if err := e.checkAccounts(ctx, tx, le.ID, lines, false); err != nil {
		return AppliedReceipt{}, namingLine(err, len(in.Items), plan.kinds)
	}
	ledgers, err := itemLedgers(ctx, tx, items)
	if err != nil {
		return AppliedReceipt{}, err
	}
	// No line is converted: the entry shows the receipt's currencies and
	// rates, as a clearing shows its first item's.
	first := items[0]
	entry, err := e.post(ctx, tx, actor, draft{entity: le, ledgers: ledgers, documentType: in.DocumentType, documentDate: in.PostingDate,
		postingDate: in.PostingDate, currencyTC: in.Currency, currencyLC: first.currencyLC, currencyGC: first.currencyGC, rateTCLC: first.rateTCLC,
		rateLCGC: first.rateLCGC, reference: in.Reference, headerText: in.HeaderText, sourceContext: in.SourceContext,
		sourceDocumentID: &in.SourceDocumentID, lines: lines, sourcedItems: plan.taxItems})
	if err != nil {
		return AppliedReceipt{}, err
	}
	// The entry's line i+1 offsets item i (offsetItems), as a settlement's does.
	id, err := clearWithEntry(ctx, tx, actor, le.ID, in.PostingDate, entry, items, leadingLines(len(items)))
	if err != nil {
		return AppliedReceipt{}, err
	}
	if err := keepResidualTerms(ctx, tx, entry, items[1:], plan.residualLines); err != nil {
		return AppliedReceipt{}, err
	}
	out := AppliedReceipt{Entry: entry, Receipt: -first.tc, Items: make([]money.Minor, len(in.Items)), Customers: make([]uuid.UUID, len(in.Items)),
		ResidualLines: plan.residualLines, WriteOffLines: plan.writeOffLines, Discounts: plan.discounts, Discount: plan.discount, Difference: plan.difference,
		OnAccount: plan.onAccount, OnAccountLine: plan.onAccountLine, Translations: plan.translations}
	for i, it := range items[1:] {
		out.Items[i], out.Customers[i] = it.tc, *it.partner // a customer's item names its customer (checkCustomerItem)
	}
	return out, errors.Join(
		audit.Add(ctx, audit.Entry{Action: "ledger.journal_entry.post", ObjectType: "ledger.journal_entry", ObjectID: entry.ID, Outcome: "succeeded",
			Detail: map[string]any{"document_number": entry.DocumentNumber, "fiscal_year": entry.FiscalYear, "lines": len(lines),
				"source_context": entry.SourceContext}}),
		audit.Add(ctx, audit.Entry{Action: "ledger.clearing.create", ObjectType: "ledger.clearing", ObjectID: id, Outcome: "succeeded",
			Detail: map[string]any{"items": len(items), "clearing_entry_id": entry.ID, "document_number": entry.DocumentNumber}}))
}

// notAReceipt refuses an entry that is no receipt on the account.
func notAReceipt(why string) *problem.Problem {
	return problem.Unprocessable("ledger.not_a_receipt", "The entry is not a receipt on the account: it "+why+".")
}

// receiptLines are the application's lines: one offsetting the receipt
// (items[0]) and one each of the customer's items, in their order
// (clearWithEntry clears item i with line i+1), then the residual items,
// what is left on account or the payment difference, and the exchange
// differences (applicationLines). The receipt is an open credit line; the
// items are open items of the customer on customer reconciliation
// accounts of the legal entity in the receipt's ledger, each in the
// receipt's currency or translated into it (translateItems); the
// application is not dated before any of them was posted (notBeforeItems).
func receiptLines(ctx context.Context, tx pgx.Tx, in ReceiptApplication, le uuid.UUID, items []item) (application, error) {
	customer, err := customerAccounts(ctx, tx, items)
	if err != nil {
		return application{}, err
	}
	payers := map[uuid.UUID]bool{in.CustomerID: true}
	for _, c := range in.OtherCustomers {
		payers[c] = true
	}
	if in.Credit {
		err = checkCredit(items[0], le, in, customer)
	} else {
		err = checkReceipt(items[0])
	}
	if err != nil {
		return application{}, err
	}
	translations, err := translateItems(ctx, tx, in, items, func(i int, it item) error { return checkCustomerItem(i, it, le, payers, customer) })
	if err != nil {
		return application{}, err
	}
	// Every line is checked above and every item is in the receipt's
	// currency now (translateItems).
	lines, _, _, _, err := offsetItems(items, in.Currency, in.HeaderText, func(int, item) error { return nil })
	if err != nil {
		return application{}, err
	}
	plan, err := applicationLines(ctx, tx, in, le, items, lines)
	if err != nil {
		return application{}, err
	}
	plan.translations = translations
	// The receipt's own line counts: it is not applied before the bank posted it.
	if err := notBeforeItems(items, in.PostingDate, "posting_date"); err != nil {
		return application{}, err
	}
	return plan, nil
}

// checkReceipt checks the receipt's line: a line of the leading ledger
// that is a credit, not of a reversed entry or a reversal, and not
// cleared. Reservations hold supplier items only (checkAccountItem).
func checkReceipt(it item) error {
	switch {
	case !it.found: // a parallel ledger's copy: open items are the leading ledger's (ADR-0045)
		return notAReceipt("is not an entry of the leading ledger")
	case it.reversed:
		return problem.Conflict("ledger.entry_reversed", "The receipt's journal entry is reversed, or is a reversal; it cannot be applied.")
	case !it.open:
		return problem.Conflict("ledger.item_already_cleared", "The receipt is already cleared.")
	case it.tc >= 0:
		return notAReceipt("debits the account: nothing was received")
	}
	return nil
}

// checkCustomerItem checks item i of the application: an open item on a
// reconciliation account of the legal entity that nothing holds
// (checkItem), on a customer reconciliation account and of one of the
// customers the receipt pays (payers); an item in another currency than
// the receipt is translated into it (translateItems, T75n). Every refusal
// names the request's own field, items[i], so a client can tell which item
// to leave out.
func checkCustomerItem(i int, it item, le uuid.UUID, payers, customerAccount map[uuid.UUID]bool) error {
	field := fmt.Sprintf("items[%d]", i)
	if err := checkItem(i, it, &le, uuid.Nil,
		problem.FieldError{Field: field, Code: "not_an_open_item", Message: "an open item of the customer in this legal entity"}); err != nil {
		return namingItem(err, field)
	}
	// A line on a reconciliation account names its partner (checkAccounts).
	if !customerAccount[it.account] || it.partner == nil || !payers[*it.partner] {
		return problem.Unprocessable("ledger.not_the_customers_item", "Item "+fmt.Sprint(i+1)+" is not an item of the customer, or of another customer the payment names, on a customer reconciliation account.",
			problem.FieldError{Field: field, Code: "not_the_customers", Message: "an open item of the customer"})
	}
	return nil
}

// namingItem gives a refusal of an item that names no field the item's
// field in the request, with the refusal's detail.
func namingItem(err error, field string) error {
	var p *problem.Problem
	if errors.As(err, &p) && len(p.Errors) == 0 {
		p.Errors = []problem.FieldError{{Field: field, Code: "not_applicable", Message: p.Detail}}
	}
	return err
}

// namingLine turns a refusal of one of the entry's lines, which names the
// line (lines[k]: the receipt's offset, then one per item, then the
// lines kinds names in their order, k-items-1 for line k; receiptLines),
// into one that names what the caller gave: item k of the application as
// items[k-1], the receipt, or the line's account by its kind (the
// exchange difference's, the payment difference's, the residual's, the
// account the credit is left on), none of which is a field of the items.
// The request has no lines.
func namingLine(err error, items int, kinds []string) error {
	var p *problem.Problem
	if !errors.As(err, &p) || len(p.Errors) == 0 {
		return err
	}
	var k int
	// A field that names no line leaves the refusal as it is.
	if n, _ := fmt.Sscanf(p.Errors[0].Field, "lines[%d]", &k); n == 1 {
		reason := p.Errors[0].Message
		switch {
		case k == 0:
			p.Detail, p.Errors = "The receipt's account cannot be posted: "+reason+".", nil
		case k <= items:
			p.Detail = "Item " + fmt.Sprint(k) + "'s account cannot be posted: " + reason + "."
			p.Errors = []problem.FieldError{{Field: fmt.Sprintf("items[%d]", k-1), Code: p.Errors[0].Code, Message: reason}}
		default:
			p.Detail, p.Errors = kinds[k-items-1]+" cannot be posted: "+reason+".", nil
		}
	}
	return err
}

// customerAccounts are those of the items' accounts that are customer
// reconciliation accounts: an item only says that its account is a
// reconciliation account, a supplier's included.
func customerAccounts(ctx context.Context, tx pgx.Tx, items []item) (map[uuid.UUID]bool, error) {
	ids := make([]uuid.UUID, len(items))
	for i, it := range items {
		ids[i] = it.account
	}
	rows, _ := tx.Query(ctx, `SELECT id FROM ledger.gl_account WHERE id = ANY($1) AND reconciliation_kind = 'customer'`, ids)
	out := map[uuid.UUID]bool{}
	var id uuid.UUID
	if _, err := pgx.ForEachRow(rows, []any{&id}, func() error { out[id] = true; return nil }); err != nil {
		return nil, fmt.Errorf("ledger: customer reconciliation accounts among %d items' accounts: %w", len(items), err)
	}
	return out, nil
}

// Receipt is an incoming payment waiting to be applied: the open credit
// line of an entry the payments context posted (a bank statement line
// matched as incoming) on an incoming payments clearing account.
type Receipt struct {
	Item        ItemRef
	AccountID   uuid.UUID
	Number      string  // the entry's document number
	HeaderText  *string // its header text: the statement and line that posted it
	PostingDate time.Time
	// Amount is what was received, positive, in Currency, whose minor-unit
	// digits are Exponent.
	Currency string
	Exponent int
	Amount   money.Minor
}

// ReceiptFilter narrows Receipts: to one entry, to a currency ("": any)
// and to posting dates from and to a day, each nil for no limit.
type ReceiptFilter struct {
	Entry    *uuid.UUID
	Currency string
	From, To *time.Time
}

// ReceiptSort is the order Receipts pages in: by posting date, then entry.
var ReceiptSort = pagination.Sort{Default: "posting_date", Fields: []pagination.Field{{Name: "posting_date", Column: "posting_date", Type: "date"}}}

// Receipts returns the legal entity's receipts not yet applied on the
// accounts, its house banks' incoming payments clearing accounts: the
// leading ledger's open credit lines there of entries the payments
// context posted, neither cleared nor of a reversed entry or a reversal,
// as ApplyReceipt would take them. An entry with more than one line on the
// accounts is no receipt (ApplyReceipt refuses it) and is left out, so a
// receipt is one row per entry. page gives one page of those the filter
// passes in ReceiptSort's order with one row more (pagination.Page), or
// nil for all of them by posting date and entry. The legal entity is not
// checked against the caller's companies: the caller's context does that.
func (OpenItems) Receipts(ctx context.Context, page *pagination.Keyset, legalEntity uuid.UUID, accounts []uuid.UUID, f ReceiptFilter) ([]Receipt, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	query, args := `SELECT * FROM (SELECT l.journal_entry_id AS id, l.fiscal_year, l.line_no, l.account_id, e.document_number, e.header_text, l.posting_date,
			e.currency_tc, c.exponent, -l.amount_tc
		FROM ledger.journal_line l
		JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id
		JOIN ref.currency c ON c.code = e.currency_tc
		WHERE l.legal_entity_id = $1 AND l.account_id = ANY($2) AND l.ledger_id = (SELECT id FROM ledger.ledger WHERE is_leading)
			AND e.source_context = 'payments' AND l.amount_tc < 0 AND ($3::uuid IS NULL OR l.journal_entry_id = $3)
			AND ($4 = '' OR e.currency_tc = $4) AND ($5::date IS NULL OR l.posting_date >= $5) AND ($6::date IS NULL OR l.posting_date <= $6)
			AND NOT EXISTS (SELECT FROM ledger.clearing_item ci WHERE ci.tenant_id = l.tenant_id AND ci.fiscal_year = l.fiscal_year
				AND ci.journal_entry_id = l.journal_entry_id AND ci.line_no = l.line_no)
			AND NOT `+reversedLine+`
			AND NOT EXISTS (SELECT FROM ledger.journal_line o WHERE o.tenant_id = l.tenant_id AND o.fiscal_year = l.fiscal_year
				AND o.journal_entry_id = l.journal_entry_id AND o.account_id = ANY($2) AND o.line_no <> l.line_no)) r
		WHERE true`, []any{legalEntity, accounts, f.Entry, f.Currency, f.From, f.To}
	if page != nil {
		query, args = page.SQL(query, args)
	} else {
		query += ` ORDER BY posting_date, id`
	}
	rows, _ := tx.Query(ctx, query, args...)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Receipt, error) {
		var v Receipt
		err := r.Scan(&v.Item.EntryID, &v.Item.FiscalYear, &v.Item.LineNo, &v.AccountID, &v.Number, &v.HeaderText, &v.PostingDate, &v.Currency, &v.Exponent,
			&v.Amount)
		return v, errs.Wrapf(err, "scan receipt")
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: receipts not applied on %d accounts of %s: %w", len(accounts), legalEntity, err)
	}
	return out, nil
}
