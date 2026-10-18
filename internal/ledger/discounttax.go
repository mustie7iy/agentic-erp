package ledger

import (
	"context"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

// taxKey is a tax line of an entry: its account and tax code.
type taxKey struct {
	account uuid.UUID
	code    string
}

// taxedItem is a discounted item with what its entry owes the supplier
// (paid, in the sign it is paid in) and its entry's tax lines' totals in
// that sign, per account and tax code.
type taxedItem struct {
	discount, paid money.Minor
	taxes          map[taxKey]money.Minor
}

// discountTax is the tax share of the discounts taken (T36d), per tax line
// account and code, in the sign the items are paid in, and its total. A
// cash discount is taken off the gross amount, so it reduces the input tax
// its items' entries claimed, as SAP's tax adjustment of discounts does
// and §17 UStG requires: each item's discount is split by its entry's tax
// lines in proportion to the gross its entry owes the supplier (splitTax).
func discountTax(ctx context.Context, tx pgx.Tx, items []item, discounts []money.Minor) (map[taxKey]money.Minor, money.Minor, error) {
	var years []int16
	var entries []uuid.UUID
	for i, it := range items {
		if discounts[i] != 0 {
			years, entries = append(years, it.ref.FiscalYear), append(entries, it.ref.EntryID)
		}
	}
	type entryKey struct {
		year  int16
		entry uuid.UUID
	}
	paid, taxes := map[entryKey]money.Minor{}, map[entryKey]map[taxKey]money.Minor{}
	// One row per entry with its supplier total (no tax code), then one per
	// tax line account and code: a line on its tax code's account.
	rows, _ := tx.Query(ctx, `SELECT l.fiscal_year, l.journal_entry_id, l.account_id, coalesce(l.tax_code, ''), (sum(l.amount_tc))::bigint,
			a.reconciliation_kind = 'supplier' AND l.special_gl IS NULL
		FROM ledger.journal_line l JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id
		WHERE (l.fiscal_year, l.journal_entry_id) IN (SELECT * FROM unnest($1::smallint[], $2::uuid[]))
			AND ((a.reconciliation_kind = 'supplier' AND l.special_gl IS NULL) OR (l.tax_code IS NOT NULL AND EXISTS (SELECT FROM ledger.tax_code t WHERE t.tenant_id = l.tenant_id
				AND t.legal_entity_id = l.legal_entity_id AND t.code = l.tax_code AND t.account_id = l.account_id)))
		GROUP BY 1, 2, 3, 4, 6`, years, entries)
	var k entryKey
	var key taxKey
	var sum money.Minor
	var supplier bool
	if _, err := pgx.ForEachRow(rows, []any{&k.year, &k.entry, &key.account, &key.code, &sum, &supplier}, func() error {
		if supplier {
			paid[k] -= sum // a supplier line is negative where the entry is paid out
			return nil
		}
		if taxes[k] == nil {
			taxes[k] = map[taxKey]money.Minor{}
		}
		taxes[k][key] = sum
		return nil
	}); err != nil {
		return nil, 0, fmt.Errorf("ledger: read the tax lines of %d discounted entries: %w", len(entries), err)
	}
	var taxed []taxedItem
	for i, it := range items {
		if d := discounts[i]; d != 0 {
			k := entryKey{it.ref.FiscalYear, it.ref.EntryID}
			taxed = append(taxed, taxedItem{discount: d, paid: paid[k], taxes: taxes[k]})
		}
	}
	return splitTax(taxed)
}

// splitTax splits each item's discount by its entry's tax lines: the tax
// share is the discount times the entry's tax over what it owes the
// supplier, rounded half away from zero, divided among the tax lines by
// largest remainder (ties in account and code order), so net and tax add
// up to the discount exactly, the tax smaller than it. An entry that owes
// nothing, whose tax lines do not all run in its payment's direction
// (§13b and intra-EU pairs among them), or whose tax is at least what it
// owes the supplier (its supplier lines offset others), is not split: its
// discount is all net.
func splitTax(items []taxedItem) (map[taxKey]money.Minor, money.Minor, error) {
	out := map[taxKey]money.Minor{}
	var total money.Minor
	for _, it := range items {
		shares := itemTaxShares(it)
		for key, share := range shares {
			var err error
			if out[key], err = money.Sum(out[key], share); err == nil {
				total, err = money.Sum(total, share)
			}
			if err != nil {
				return nil, 0, errTotalTooLarge("discounts")
			}
		}
	}
	return out, total, nil
}

// itemTaxShares is one item's discount split by its entry's tax lines.
func itemTaxShares(it taxedItem) map[taxKey]money.Minor {
	if it.paid == 0 || len(it.taxes) == 0 {
		return nil
	}
	keys := slices.SortedFunc(maps.Keys(it.taxes), compareTaxKeys)
	gross := big.NewInt(int64(it.paid.Abs()))
	discount, weight := big.NewInt(int64(it.discount.Abs())), new(big.Int)
	for _, k := range keys {
		w := it.taxes[k]
		if w != 0 && (w > 0) != (it.paid > 0) {
			return nil
		}
		weight.Add(weight, big.NewInt(int64(w.Abs())))
	}
	if weight.Cmp(gross) >= 0 { // tax of at least what is owed: the supplier lines offset others, so the gross is not the base
		return nil
	}
	// The tax share, rounded half away from zero: (2·d·W + G) div 2G.
	two := big.NewInt(2)
	whole := new(big.Int).Mul(discount, weight)
	tax := new(big.Int).Quo(new(big.Int).Add(new(big.Int).Mul(whole, two), gross), new(big.Int).Mul(gross, two))
	floors, rems := make([]*big.Int, len(keys)), make([]*big.Int, len(keys))
	left := new(big.Int).Set(tax)
	for i, k := range keys {
		floors[i], rems[i] = new(big.Int).QuoRem(new(big.Int).Mul(discount, big.NewInt(int64(it.taxes[k].Abs()))), gross, new(big.Int))
		left.Sub(left, floors[i])
	}
	order := make([]int, len(keys))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return rems[b].Cmp(rems[a]) })
	for _, i := range order[:left.Int64()] {
		floors[i].Add(floors[i], big.NewInt(1))
	}
	shares := map[taxKey]money.Minor{}
	for i, k := range keys {
		share := money.Minor(floors[i].Int64()) // less than the discount, as the tax is less than the gross: in range
		if it.discount < 0 {
			share = -share
		}
		if share != 0 {
			shares[k] = share
		}
	}
	return shares
}

// compareTaxKeys orders tax lines by account, then tax code.
func compareTaxKeys(a, b taxKey) int {
	if c := slices.Compare(a.account[:], b.account[:]); c != 0 {
		return c
	}
	return strings.Compare(a.code, b.code)
}
