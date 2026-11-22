package ledger

import (
	"context"
	"fmt"
	"math/big"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// A cash discount granted to a customer (ADR-0063 As built, cash discount
// granted; the product owner's answer of 2026-11-21 for the UAE): an item
// of a receipt application that takes the discount its payment terms grant
// on the day the receipt was received is cleared with it. The discount is
// a debit of the clearing entry, split by the item's entry's tax items as
// a supplier's discount is (T36d, discountTax): its net on the account
// determination's payment_difference_expense account, with each code and
// subdivision of the base it reduces, and its tax share on each output tax
// line's account, naming the emirate the invoice's item named, so the
// output tax items it writes reduce the tax the returns report. Lines and
// items are at the receipt's rate, as the payment difference is.

// receiptDiscounts adds the cash discounts the application's items take
// (in.CashDiscounts, discountsTaken) to its lines, at the receipt's rate,
// and keeps their tax items for the entry. Nothing without one.
func (a *application) receiptDiscounts(ctx context.Context, tx pgx.Tx, in ReceiptApplication, le uuid.UUID, receipt item, items []item) error {
	taken, total, err := discountsTaken(ctx, tx, in, items)
	if err != nil || total == 0 {
		return err
	}
	account, ok, err := Accounts{}.Determination(ctx, le, "payment_difference_expense")
	if err != nil {
		return err
	}
	if !ok {
		return problem.Unprocessable("ledger.no_payment_difference_account",
			"The application grants a cash discount, and the legal entity has no payment_difference_expense account in its account determination.")
	}
	// A customer's invoice is paid by a credit: the discount, in the sign
	// it is paid in, is negative (discountTax).
	paid := make([]money.Minor, len(taken))
	for i, d := range taken {
		paid[i] = -d
	}
	lines, plan, err := discountLines(ctx, tx, account, "customer", items, paid, -total, in.HeaderText)
	if err != nil {
		return err
	}
	for _, l := range lines {
		if l.LC, l.GC, err = receiptRate(receipt, l.TC); err != nil {
			return err
		}
		kind := "The cash discount's account (payment_difference_expense of the account determination)"
		if l.AccountID != account {
			kind = "The output tax account of tax code " + *l.TaxCode // only a tax share is on another account
		}
		a.add(l, kind)
	}
	// What a share of the receipt's transaction amount is in local
	// currency, exactly: the receipt's local amount in proportion, as
	// receiptRate rounds it (prorate). The receipt is a credit, never zero.
	rate := new(big.Rat).SetFrac(big.NewInt(int64(receipt.lc)), big.NewInt(int64(receipt.tc)))
	a.taxItems = plan.taxItemsAt(func(tc money.Minor) *big.Rat { return new(big.Rat).Mul(rate, new(big.Rat).SetInt64(int64(tc))) })
	a.discounts, a.discount = taken, total
	return nil
}

// discountsTaken are the cash discounts the items take, one per item,
// positive (0 for an item that takes none), and their total: of each item
// in.CashDiscounts names, the discount its terms grant on
// in.DiscountDate (ItemTerms.DiscountOn) of its amount in the receipt's
// currency. Only an invoice paid in full takes one (422
// ledger.invalid_discount), and only within its terms' discount period
// (422 ledger.no_cash_discount), each naming items[i].cash_discount.
func discountsTaken(ctx context.Context, tx pgx.Tx, in ReceiptApplication, items []item) ([]money.Minor, money.Minor, error) {
	if !slices.Contains(in.CashDiscounts, true) {
		return nil, 0, nil
	}
	var refs []ItemRef
	for i, take := range in.CashDiscounts {
		if take {
			refs = append(refs, items[i].ref)
		}
	}
	terms, err := itemTerms(ctx, tx, refs)
	if err != nil {
		return nil, 0, err
	}
	taken := make([]money.Minor, len(items))
	for i, take := range in.CashDiscounts {
		if !take {
			continue
		}
		if taken[i], err = discountOf(i, items[i], in, terms[items[i].ref]); err != nil {
			return nil, 0, err
		}
	}
	total, err := money.Sum(taken...)
	if err != nil {
		return nil, 0, errTotalTooLarge("items")
	}
	return taken, total, nil
}

// discountOf is the cash discount item i of the application takes by its
// terms (nil for none) on in.DiscountDate.
func discountOf(i int, it item, in ReceiptApplication, terms *ItemTerms) (money.Minor, error) {
	field := fmt.Sprintf("items[%d].cash_discount", i)
	switch {
	case it.tc <= 0:
		return 0, problem.Invalid("ledger.invalid_discount", field,
			fmt.Sprintf("Item %d is a credit of the customer's: a cash discount is taken on an amount the customer owes.", i+1))
	case len(in.Residuals) != 0 && in.Residuals[i].Amount != 0:
		return 0, problem.Invalid("ledger.invalid_discount", field,
			fmt.Sprintf("Item %d is paid in part: an item takes its cash discount when it is paid in full.", i+1))
	}
	d, err := terms.DiscountOn(it.tc, in.DiscountDate)
	if err != nil {
		return 0, errTotalTooLarge(fmt.Sprintf("items[%d]", i))
	}
	if d == 0 {
		return 0, problem.Invalid("ledger.no_cash_discount", field, fmt.Sprintf("Item %d's payment terms grant no cash discount for a payment received on %s.",
			i+1, in.DiscountDate.Format(time.DateOnly)))
	}
	return d, nil
}

// itemTerms are the payment terms the items were posted with
// (ledger.open_item_terms), by item; an item posted without is left out.
func itemTerms(ctx context.Context, tx pgx.Tx, refs []ItemRef) (map[ItemRef]*ItemTerms, error) {
	years, entries, nos := ItemColumns(refs)
	rows, _ := tx.Query(ctx, `SELECT t.fiscal_year, t.journal_entry_id, t.line_no, `+termsColumns+`
		FROM unnest($1::smallint[], $2::uuid[], $3::int[]) AS x(y, je, n)
		JOIN ledger.open_item_terms t ON t.fiscal_year = x.y AND t.journal_entry_id = x.je AND t.line_no = x.n`, years, entries, nos)
	out := map[ItemRef]*ItemTerms{}
	var ref ItemRef
	if _, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (struct{}, error) {
		terms, err := scanWithTerms(r, "scan item terms", []any{&ref.FiscalYear, &ref.EntryID, &ref.LineNo})
		out[ref] = terms
		return struct{}{}, err
	}); err != nil {
		return nil, fmt.Errorf("ledger: payment terms of %d items: %w", len(refs), err)
	}
	return out, nil
}
