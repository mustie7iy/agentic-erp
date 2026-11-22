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
	if err := a.addDiscountLines(receipt, account, lines); err != nil {
		return err
	}
	// What a share of the receipt's transaction amount is in local
	// currency, exactly: the receipt's local amount in proportion, as
	// receiptRate rounds it (prorate). The receipt is a credit, never zero.
	rate := new(big.Rat).SetFrac(big.NewInt(int64(receipt.lc)), big.NewInt(int64(receipt.tc)))
	a.taxItems = plan.taxItemsAt(func(tc money.Minor) *big.Rat { return new(big.Rat).Mul(rate, new(big.Rat).SetInt64(int64(tc))) })
	a.discounts, a.discount = taken, total
	return nil
}

// addDiscountLines adds the discount's lines, its net on the account and
// its tax shares on the output tax accounts, converted at the receipt's
// rate (receiptRate), each with the kind namingLine names it by.
func (a *application) addDiscountLines(receipt item, account uuid.UUID, lines []Line) error {
	for _, l := range lines {
		var err error
		if l.LC, l.GC, err = receiptRate(receipt, l.TC); err != nil {
			return err
		}
		kind := "The cash discount's account (payment_difference_expense of the account determination)"
		if l.AccountID != account {
			kind = "The output tax account of tax code " + *l.TaxCode // only a tax share is on another account
		}
		a.add(l, kind)
	}
	return nil
}

// discountsTaken are the cash discounts the items take, one per item,
// positive (0 for an item that takes none), and their total, as
// discountsOf takes them by the items' terms; nil and 0 when the
// application asks for none.
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
	mixed, err := mixedJurisdictionEntries(ctx, tx, refs)
	if err != nil {
		return nil, 0, err
	}
	return discountsOf(in, items, terms, mixed)
}

// discountsOf are the discounts of discountsTaken: of each item
// in.CashDiscounts names, the discount its terms grant on in.DiscountDate
// (ItemTerms.DiscountOn) of its amount in the receipt's currency. Only an
// invoice paid in full takes one, and only one whose entry the split can
// keep the emirates of (mixed: not, 422 ledger.invalid_discount), within
// its terms' discount period (422 ledger.no_cash_discount), each naming
// items[i].cash_discount.
func discountsOf(in ReceiptApplication, items []item, terms map[ItemRef]*ItemTerms, mixed map[entryKey]bool) ([]money.Minor, money.Minor, error) {
	taken := make([]money.Minor, len(items))
	for i, take := range in.CashDiscounts {
		if !take {
			continue
		}
		field := fmt.Sprintf("items[%d].cash_discount", i)
		it := items[i]
		switch {
		case it.tc <= 0:
			return nil, 0, problem.Invalid("ledger.invalid_discount", field,
				fmt.Sprintf("Item %d is a credit of the customer's: a cash discount is taken on an amount the customer owes.", i+1))
		case len(in.Residuals) != 0 && in.Residuals[i].Amount != 0:
			return nil, 0, problem.Invalid("ledger.invalid_discount", field,
				fmt.Sprintf("Item %d is paid in part: an item takes its cash discount when it is paid in full.", i+1))
		case mixed[entryKey{it.ref.FiscalYear, it.ref.EntryID}]:
			return nil, 0, problem.Invalid("ledger.invalid_discount", field, fmt.Sprintf("Item %d's entry has a tax code of which one component "+
				"levies its lines' subdivision and another does not: the discount's tax could not be kept in each subdivision. Write it off with a reason.", i+1))
		}
		// DiscountOn fails only on overflow, and a tier's percent is below 100
		// (open_item_terms' CHECK), so the discount is less than the item.
		taken[i], _ = terms[it.ref].DiscountOn(it.tc, in.DiscountDate)
		if taken[i] == 0 {
			return nil, 0, problem.Invalid("ledger.no_cash_discount", field, fmt.Sprintf("Item %d's payment terms grant no cash discount for a payment received on %s.",
				i+1, in.DiscountDate.Format(time.DateOnly)))
		}
	}
	total, err := money.Sum(taken...)
	if err != nil {
		return nil, 0, errTotalTooLarge("items")
	}
	return taken, total, nil
}

// mixedJurisdictionEntries are those of the items' entries with a tax item
// of a code whose components do not all levy their lines' subdivision or
// all not (T61f): the line rule is per code (journal_line_tax_jurisdiction_code),
// so a correction line of the component that levies none would still need
// the subdivision, which its item does not name.
func mixedJurisdictionEntries(ctx context.Context, tx pgx.Tx, refs []ItemRef) (map[entryKey]bool, error) {
	years, entries, _ := ItemColumns(refs)
	rows, _ := tx.Query(ctx, `SELECT DISTINCT i.fiscal_year, i.journal_entry_id FROM ledger.tax_item i
		WHERE (i.fiscal_year, i.journal_entry_id) IN (SELECT * FROM unnest($1::smallint[], $2::uuid[]))
			AND EXISTS (SELECT FROM ledger.tax_component c WHERE c.tenant_id = i.tenant_id AND c.legal_entity_id = i.legal_entity_id AND c.code = i.code
				AND c.jurisdiction_on_line)
			AND EXISTS (SELECT FROM ledger.tax_component c WHERE c.tenant_id = i.tenant_id AND c.legal_entity_id = i.legal_entity_id AND c.code = i.code
				AND NOT c.jurisdiction_on_line)`, years, entries)
	out := map[entryKey]bool{}
	var k entryKey
	if _, err := pgx.ForEachRow(rows, []any{&k.year, &k.entry}, func() error { out[k] = true; return nil }); err != nil {
		return nil, fmt.Errorf("ledger: tax codes of %d discounted items: %w", len(refs), err)
	}
	return out, nil
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
