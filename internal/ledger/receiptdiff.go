package ledger

import (
	"context"
	"fmt"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Partial payments and payments on account (ADR-0063 §6 As built, T75f
// and T75g): a receipt applied to a customer's items may leave a residual
// item of an item it pays in part, write off a difference within the
// company's payment tolerance (SAP OBA3), or leave what the items do not
// take on the customer's account as a credit, which a later application
// applies as it applies a receipt.

// application is an application's entry as receiptLines plans it: its
// lines, the kind of each line after the items' offsets (namingLine), the
// line of each item's residual (0: none), the payment difference written
// off and what was left on account, with its line.
type application struct {
	lines         []Line
	kinds         []string
	residualLines []int32
	difference    money.Minor
	onAccount     money.Minor
	onAccountLine int32
}

// add appends a line of the kind, returning its line number.
func (a *application) add(l Line, kind string) int32 {
	a.lines = append(a.lines, l)
	a.kinds = append(a.kinds, kind)
	return int32(len(a.lines))
}

// checkApplication checks what an application asks before anything is
// read: at least one item unless the receipt goes on account, a residual
// per item or none, each item named once, and the credit applied not
// among the items.
func checkApplication(in ReceiptApplication) error {
	if len(in.Items) == 0 && in.OnAccount == uuid.Nil {
		return problem.Unprocessable("ledger.no_items", "A receipt is applied to at least one open item, or left on the customer's account.")
	}
	if len(in.Residuals) != 0 && len(in.Residuals) != len(in.Items) {
		return fmt.Errorf("ledger: apply a receipt with %d residuals for %d items", len(in.Residuals), len(in.Items))
	}
	if err := requireDistinct(in.Items); err != nil {
		return err
	}
	for i, it := range in.Items {
		if in.Credit && it == in.Receipt {
			return problem.Unprocessable("ledger.duplicate_item", "Item "+fmt.Sprint(i+1)+" is the credit being applied.",
				problem.FieldError{Field: fmt.Sprintf("items[%d]", i), Code: "duplicate", Message: "an item other than the credit applied"})
		}
	}
	return nil
}

// receiptLine is the line the application applies: a credit on account's
// as the application names it, or a receipt's, the one line its entry has
// on the account.
func receiptLine(ctx context.Context, tx pgx.Tx, le uuid.UUID, in ReceiptApplication) (ItemRef, error) {
	if in.Credit {
		return in.Receipt, nil
	}
	receipt, err := accountLines(ctx, tx, le, in.ReceiptAccountID, []ItemRef{in.Receipt})
	if err != nil {
		return ItemRef{}, err
	}
	if len(receipt) != 1 {
		return ItemRef{}, notAReceipt(fmt.Sprintf("has %d lines on the account", len(receipt)))
	}
	return receipt[0], nil
}

// checkCredit checks a credit on account being applied: an open credit
// line of the leading ledger on the customer reconciliation account the
// application names, of the customer and the legal entity, of no reversed
// entry and no special G/L item.
func checkCredit(it item, le uuid.UUID, in ReceiptApplication, customerAccount map[uuid.UUID]bool) error {
	switch {
	case !it.found || it.legalEntity != le || it.account != in.ReceiptAccountID || !customerAccount[it.account] || it.partner == nil ||
		*it.partner != in.CustomerID || it.special != nil:
		return notAReceipt("is not a line of the customer's on its customer reconciliation account")
	case it.reversed:
		return problem.Conflict("ledger.entry_reversed", "The credit's journal entry is reversed, or is a reversal; it cannot be applied.")
	case !it.open:
		return problem.Conflict("ledger.item_already_cleared", "The credit on account is already cleared.")
	case it.tc >= 0:
		return notAReceipt("debits the customer: it is no credit")
	}
	return nil
}

// applicationLines completes the application's lines after the offsets:
// each item's residual, then what is left over the items, net of their
// residuals, on the customer's account when the application asks for it,
// else the payment difference within the tolerance, then the exchange
// differences in local and group currency. A residual keeps its item's
// rate; what is left on account and the difference take the receipt's,
// so a receipt left whole on account realizes no exchange difference.
func applicationLines(ctx context.Context, tx pgx.Tx, in ReceiptApplication, le uuid.UUID, items []item, offsets []Line) (application, error) {
	a := application{lines: offsets, residualLines: make([]int32, len(in.Items))}
	if err := a.residuals(in, items[1:]); err != nil {
		return application{}, err
	}
	tc, _, _, err := lineTotals(a.lines)
	if err != nil {
		return application{}, err
	}
	receipt := items[0]
	d := -tc // what the items, net of their residuals, exceed the receipt by
	lc, gc, err := receiptRate(receipt, d)
	if err != nil {
		return application{}, err
	}
	switch {
	case d < 0 && in.OnAccount != uuid.Nil:
		customer := in.CustomerID
		a.onAccount = -d
		a.onAccountLine = a.add(Line{AccountID: in.OnAccount, PartnerID: &customer, TC: d, LC: lc, GC: gc, Text: in.HeaderText},
			"The customer's reconciliation account the credit is left on")
	case d != 0:
		if err := a.writeOff(ctx, tx, in, le, receipt, d, lc, gc); err != nil {
			return application{}, err
		}
	}
	_, diffLC, diffGC, err := lineTotals(a.lines)
	if err != nil {
		return application{}, err
	}
	fx, err := fxLines(ctx, le, -diffLC, -diffGC, in.HeaderText)
	if err != nil {
		return application{}, err
	}
	for _, l := range fx {
		a.add(l, "The exchange difference's account (fx_gain or fx_loss of the account determination)")
	}
	// A difference line takes the items' trading partner (ADR-0059 §4); the
	// line left on account takes the customer's (deriveTradingPartners).
	tp := sharedTradingPartner(items[1:])
	for i := len(items) + countNonZero(a.residualLines); i < len(a.lines); i++ {
		if int32(i+1) != a.onAccountLine && a.lines[i].TradingPartnerID == nil {
			a.lines[i].TradingPartnerID = tp
		}
	}
	return a, nil
}

// residuals adds each item's residual item: a debit of the customer on
// the item's account, more than zero and less than the item, at the
// item's own rate.
func (a *application) residuals(in ReceiptApplication, items []item) error {
	for i, r := range in.Residuals {
		if r.Amount == 0 {
			continue
		}
		it := items[i]
		if r.Amount < 0 || r.Amount >= it.tc {
			return problem.Unprocessable("ledger.invalid_residual", "Item "+fmt.Sprint(i+1)+"'s residual item is more than zero and less than the item, "+
				"which is an amount the customer owes; what is left of a credit is not a residual.",
				problem.FieldError{Field: fmt.Sprintf("items[%d].residual.amount", i), Code: "invalid", Message: "more than zero and less than the item"})
		}
		// A share of less than the whole of an amount in range is in range:
		// prorate cannot fail here.
		lc, _ := prorate(it.lc, r.Amount, it.tc)
		gc, _ := prorate(it.gc, r.Amount, it.tc)
		text := r.Text
		if text == nil {
			text = in.HeaderText
		}
		a.residualLines[i] = a.add(Line{AccountID: it.account, PartnerID: it.partner, TC: r.Amount, LC: lc, GC: gc, Text: text, TradingPartnerID: it.tradingPartner,
			offsets: true}, "Item "+fmt.Sprint(i+1)+"'s account, which its residual item is posted to")
	}
	return nil
}

// writeOff books the payment difference d, an underpayment when positive,
// on the account determination's payment_difference_expense or, an
// overpayment, payment_difference_revenue, when the tolerance of its
// direction allows it: its local amount at most the limit's amount and d
// at most the limit's percent of the items applied, net of their
// residuals (the lower binds, SAP OBA3). Anything more is refused, 422
// ledger.clearing_not_balanced, as an application that does not balance
// always was.
func (a *application) writeOff(ctx context.Context, tx pgx.Tx, in ReceiptApplication, le uuid.UUID, receipt item, d, lc, gc money.Minor) error {
	limit, key, what := in.Under, "payment_difference_expense", "an underpayment"
	if d < 0 {
		limit, key, what = in.Over, "payment_difference_revenue", "an overpayment"
	}
	base, err := money.Diff(d, receipt.tc) // the items applied, net of their residuals: the receipt and the difference
	if err != nil {
		return errTotalTooLarge("items")
	}
	if !withinTolerance(limit, d, lc, base) {
		exps, err := exponents(ctx, tx, in.Currency)
		if err != nil {
			return err
		}
		exp := exps[in.Currency]
		return problem.Unprocessable("ledger.clearing_not_balanced", fmt.Sprintf("The items applied, net of their residual items, differ from the receipt of %s %s by %s: "+
			"%s beyond the company's payment tolerance (companies[].receivables.payment_tolerance). Leave a residual item, apply other items or leave the rest on the customer's account.",
			(-receipt.tc).Plain(exp), in.Currency, d.Plain(exp), what),
			problem.FieldError{Field: "items", Code: "not_balanced", Message: "items that total the receipt, within the payment tolerance"})
	}
	account, ok, err := Accounts{}.Determination(ctx, le, key)
	if err != nil {
		return err
	}
	if !ok {
		return problem.Unprocessable("ledger.no_payment_difference_account", "The application writes off "+what+", and the legal entity has no "+key+
			" account in its account determination.")
	}
	a.difference = d
	a.add(Line{AccountID: account, TC: d, LC: lc, GC: gc, Text: in.HeaderText}, "The payment difference's account ("+key+" of the account determination)")
	return nil
}

// withinTolerance reports whether a payment difference of d, lc in local currency,
// is within the limit on items applied of base: lc at most its amount and
// d at most its percent of base, both limits above zero.
func withinTolerance(limit config.ToleranceLimit, d, lc, base money.Minor) bool {
	if limit.Amount <= 0 || limit.Percent == nil || limit.Percent.Sign() <= 0 || base <= 0 || lc.Abs() > limit.Amount {
		return false
	}
	allowed := new(big.Rat).Mul(limit.Percent, big.NewRat(int64(base), 100))
	return new(big.Rat).SetInt64(int64(d.Abs())).Cmp(allowed) <= 0
}

// receiptRate is tc of the receipt's currency in local and group currency
// at the receipt's booked rate: its share of the receipt's amounts.
func receiptRate(receipt item, tc money.Minor) (lc, gc money.Minor, err error) {
	if lc, err = prorate(receipt.lc, tc, receipt.tc); err != nil {
		return 0, 0, err
	}
	gc, err = prorate(receipt.gc, tc, receipt.tc)
	return lc, gc, err
}

// prorate is x's share part/whole, rounded half away from zero (share);
// whole is never zero (an open item's amount).
func prorate(x, part, whole money.Minor) (money.Minor, error) {
	v, err := share(x, big.NewRat(int64(part), 1), big.NewRat(int64(whole), 1))
	if err != nil {
		return 0, errTotalTooLarge("items")
	}
	return v, nil
}

// Closed returns which of the lines are no longer open: cleared, or of a
// reversed entry or a reversal; lines still open, or not found, are left
// out. It takes no lock, so a reader calls it without taking the entries'
// locks out of their one sorted call (lockEntries): its answer is advice,
// which a posting checks again under those locks (ApplyReceipt's
// checkCredit, clearing_item_pkey).
func (OpenItems) Closed(ctx context.Context, refs []ItemRef) (map[ItemRef]bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	years, entries, nos := ItemColumns(refs)
	rows, _ := tx.Query(ctx, `SELECT l.fiscal_year, l.journal_entry_id, l.line_no
		FROM unnest($1::smallint[], $2::uuid[], $3::int[]) AS x(y, je, n)
		JOIN ledger.journal_line l ON l.fiscal_year = x.y AND l.journal_entry_id = x.je AND l.line_no = x.n
			AND EXISTS (SELECT FROM ledger.ledger g WHERE g.tenant_id = l.tenant_id AND g.id = l.ledger_id AND g.is_leading)
		WHERE EXISTS (SELECT FROM ledger.clearing_item ci WHERE ci.tenant_id = l.tenant_id AND ci.fiscal_year = l.fiscal_year
				AND ci.journal_entry_id = l.journal_entry_id AND ci.line_no = l.line_no)
			OR `+reversedLine, years, entries, nos)
	out := map[ItemRef]bool{}
	var ref ItemRef
	if _, err := pgx.ForEachRow(rows, []any{&ref.FiscalYear, &ref.EntryID, &ref.LineNo}, func() error { out[ref] = true; return nil }); err != nil {
		return nil, fmt.Errorf("ledger: which of %d lines are closed: %w", len(refs), err)
	}
	return out, nil
}

// lineTotals adds up the lines in each currency.
func lineTotals(lines []Line) (tc, lc, gc money.Minor, err error) {
	tcs, lcs, gcs := make([]money.Minor, len(lines)), make([]money.Minor, len(lines)), make([]money.Minor, len(lines))
	for i, l := range lines {
		tcs[i], lcs[i], gcs[i] = l.TC, l.LC, l.GC
	}
	if tc, err = money.Sum(tcs...); err == nil {
		if lc, err = money.Sum(lcs...); err == nil {
			gc, err = money.Sum(gcs...)
		}
	}
	if err != nil {
		return 0, 0, 0, errTotalTooLarge("items")
	}
	return tc, lc, gc, nil
}

// countNonZero counts the lines that are set.
func countNonZero(lines []int32) int {
	n := 0
	for _, l := range lines {
		if l != 0 {
			n++
		}
	}
	return n
}

// keepResidualTerms gives each residual item the terms of the item it
// stays open of (ledger.open_item_terms), so it falls due when the item
// did; no cash discount tier is kept, the discount period having been the
// item's. An item without terms leaves its residual without, due on the
// application's date as a customer item without terms is
// (ReceivableItem.NetDueDate).
func keepResidualTerms(ctx context.Context, tx pgx.Tx, entry Entry, items []item, residualLines []int32) error {
	var years []int16
	var entries []uuid.UUID
	var lines, residuals []int32
	for i, l := range residualLines {
		if l != 0 {
			years, entries, lines, residuals = append(years, items[i].ref.FiscalYear), append(entries, items[i].ref.EntryID), append(lines, items[i].ref.LineNo),
				append(residuals, l)
		}
	}
	if len(residuals) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.open_item_terms (tenant_id, fiscal_year, journal_entry_id, line_no, payment_term_code, baseline_date, net_days)
		SELECT t.tenant_id, $1, $2, x.n, t.payment_term_code, t.baseline_date, t.net_days
		FROM unnest($3::smallint[], $4::uuid[], $5::int[], $6::int[]) AS x(y, je, l, n)
		JOIN ledger.open_item_terms t ON t.fiscal_year = x.y AND t.journal_entry_id = x.je AND t.line_no = x.l`,
		entry.FiscalYear, entry.ID, years, entries, lines, residuals); err != nil {
		return fmt.Errorf("ledger: keep the terms of %d residual items of entry %s: %w", len(residuals), entry.ID, err)
	}
	return nil
}
