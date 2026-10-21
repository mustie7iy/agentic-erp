package ledger

import (
	"cmp"
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

// entryKey is an entry by fiscal year and id.
type entryKey struct {
	year  int16
	entry uuid.UUID
}

// sourceItem is a tax item of a discounted item's entry, its base and tax
// in the sign the item is paid in.
type sourceItem struct {
	entry     entryKey
	no        int32
	component string
	key       taxKey
	base, tax money.Minor
}

// taxedItem is a discounted item with what its entry owes the supplier
// (paid, in the sign it is paid in) and its entry's tax items in that
// sign.
type taxedItem struct {
	discount, paid money.Minor
	items          []sourceItem
}

// itemShare is a source item's share of a discount: of its tax and, on
// the first item of each of its code's components, of the code's base;
// codeBase marks the one item per code whose base share is the code's
// (its first component's first item).
type itemShare struct {
	item      int // the discounted item's index among the settlement's discounted items
	src       sourceItem
	tax, base money.Minor
	codeBase  bool
}

// discountSplit is the discounts taken split by their items' tax items
// (T36d): the tax per tax line account and code and the base per code, in
// the sign the items are paid in, their totals, and each source item's
// shares.
type discountSplit struct {
	taxes     map[taxKey]money.Minor
	bases     map[string]money.Minor
	tax, base money.Minor
	shares    []itemShare
}

// discountTax splits the discounts taken by their items' entries' tax
// items (T36d, ADR-0049 §5). A cash discount is taken off the gross
// amount, so it reduces the input tax its items' entries claimed, as SAP's
// tax adjustment of discounts does and §17 UStG requires: each item's
// discount is split by its entry's tax and its codes' bases in proportion
// to the gross its entry owes the supplier (itemTaxShares). The tax items
// name the account each tax line was posted on, a component's former one
// included, so no account history is read.
func discountTax(ctx context.Context, tx pgx.Tx, items []item, discounts []money.Minor) (discountSplit, error) {
	var years []int16
	var entries []uuid.UUID
	for i, it := range items {
		if discounts[i] != 0 {
			years, entries = append(years, it.ref.FiscalYear), append(entries, it.ref.EntryID)
		}
	}
	paid, taxes := map[entryKey]money.Minor{}, map[entryKey][]sourceItem{}
	// One row per entry with what it owes the supplier (item 0), then one
	// per tax item. What the entry withheld at invoice (T46f) counts as owed
	// the supplier: the discount's tax share is of the gross, not of what
	// is left after it.
	rows, _ := tx.Query(ctx, `SELECT l.fiscal_year, l.journal_entry_id, 0, '', '00000000-0000-0000-0000-000000000000'::uuid, '', 0, (-sum(l.amount_tc))::bigint
		FROM ledger.journal_line l JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id
		WHERE (l.fiscal_year, l.journal_entry_id) IN (SELECT * FROM unnest($1::smallint[], $2::uuid[])) AND a.reconciliation_kind = 'supplier'
			AND l.special_gl IS NULL
		GROUP BY 1, 2
		UNION ALL
		SELECT w.fiscal_year, w.journal_entry_id, 0, '', '00000000-0000-0000-0000-000000000000'::uuid, '', 0, sum(w.amount)::bigint FROM ledger.open_item_withholding w
		WHERE (w.fiscal_year, w.journal_entry_id) IN (SELECT * FROM unnest($1::smallint[], $2::uuid[])) AND w.posting = 'invoice'
		GROUP BY 1, 2
		UNION ALL
		SELECT i.fiscal_year, i.journal_entry_id, i.item_no, i.component, i.account_id, i.code, i.base_tc, i.tax_tc FROM ledger.tax_item i
		WHERE (i.fiscal_year, i.journal_entry_id) IN (SELECT * FROM unnest($1::smallint[], $2::uuid[])) AND i.account_id IS NOT NULL`, years, entries)
	var s sourceItem
	var base, sum money.Minor
	if _, err := pgx.ForEachRow(rows, []any{&s.entry.year, &s.entry.entry, &s.no, &s.component, &s.key.account, &s.key.code, &base, &sum}, func() error {
		if s.no == 0 {
			paid[s.entry] += sum // a supplier line is negative where the entry is paid out; what was withheld is owed it too
			return nil
		}
		s.base, s.tax = base, sum
		taxes[s.entry] = append(taxes[s.entry], s)
		return nil
	}); err != nil {
		return discountSplit{}, fmt.Errorf("ledger: read the tax items of %d discounted entries: %w", len(entries), err)
	}
	var taxed []taxedItem
	for i, it := range items {
		if d := discounts[i]; d != 0 {
			k := entryKey{it.ref.FiscalYear, it.ref.EntryID}
			taxed = append(taxed, taxedItem{discount: d, paid: paid[k], items: taxes[k]})
		}
	}
	return splitTax(taxed)
}

// splitTax splits each item's discount by its entry's tax items
// (itemTaxShares) and adds up the shares per tax line and per code.
func splitTax(items []taxedItem) (discountSplit, error) {
	out := discountSplit{taxes: map[taxKey]money.Minor{}, bases: map[string]money.Minor{}}
	for n, it := range items {
		shares := itemTaxShares(it, n)
		for _, sh := range shares {
			var err error
			if out.taxes[sh.src.key], err = money.Sum(out.taxes[sh.src.key], sh.tax); err == nil {
				out.tax, err = money.Sum(out.tax, sh.tax)
			}
			if err == nil && sh.codeBase && sh.base != 0 {
				if out.bases[sh.src.key.code], err = money.Sum(out.bases[sh.src.key.code], sh.base); err == nil {
					out.base, err = money.Sum(out.base, sh.base)
				}
			}
			if err != nil {
				return discountSplit{}, errTotalTooLarge("discounts")
			}
		}
		out.shares = append(out.shares, shares...)
	}
	return out, nil
}

// itemTaxShares is one item's discount split by its entry's tax items. The
// tax share of each item is the discount times its tax over the gross the
// entry owes the supplier, the total rounded once half away from zero and
// shared out by largest remainder (money.Allocate, ties in account, code
// and component order), less than the discount; the net it leaves is
// shared out the same way among the codes' bases and the untaxed rest of
// the gross (baseShares), so net and tax add up to the discount exactly.
// An entry that owes nothing, whose tax does not all run in its payment's
// direction (§13b and intra-EU pairs among them), or whose tax is at least
// what it owes the supplier (its supplier lines offset others), is not
// split: its discount is all net.
func itemTaxShares(it taxedItem, n int) []itemShare {
	if it.paid == 0 || len(it.items) == 0 {
		return nil
	}
	items := slices.SortedFunc(slices.Values(it.items), compareSourceItems)
	var weight money.Minor // the tax, in the sign it is paid in
	exact := make([]*big.Rat, len(items))
	for i, s := range items {
		if s.tax != 0 && (s.tax > 0) != (it.paid > 0) {
			return nil
		}
		if weight += s.tax; weight.Abs() >= it.paid.Abs() { // each step below the gross: in range
			return nil // tax of at least what is owed: the supplier lines offset others, so the gross is not the base
		}
		exact[i] = new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(int64(it.discount)), big.NewInt(int64(s.tax))), big.NewInt(int64(it.paid)))
	}
	// Each share is less than the discount, as the tax is less than the
	// gross, so the allocation is in range and cannot fail.
	taxes, _ := money.Allocate(exact)
	bases := baseShares(it, items, taxes)
	shares := make([]itemShare, len(items))
	seen := map[[2]string]bool{}
	for i, s := range items {
		shares[i] = itemShare{item: n, src: s, tax: taxes[i]}
		if k := [2]string{s.key.code, s.component}; !seen[k] {
			seen[k] = true
			shares[i].base, shares[i].codeBase = bases[s.key.code], s.component == firstComponent(items, s.key.code)
		}
	}
	return shares
}

// baseShares shares out the net of an item's discount, what its tax shares
// leave, among its codes' bases and the untaxed rest of the gross, in
// proportion: a code's base is its first component's, and a base or rest
// against the payment's direction takes no share.
func baseShares(it taxedItem, items []sourceItem, taxes []money.Minor) map[string]money.Minor {
	net := it.discount
	for _, t := range taxes {
		net -= t // the shares add up to less than the discount: in range
	}
	base, rest := map[string]money.Minor{}, it.paid
	for _, s := range items {
		rest -= s.tax // the tax is less than the gross (itemTaxShares)
		if s.component == firstComponent(items, s.key.code) {
			base[s.key.code] += s.base // posted amounts of one entry: in range
		}
	}
	codes := slices.Sorted(maps.Keys(base))
	weights := make([]*big.Rat, len(codes)+1)
	total := new(big.Rat)
	for i := range weights {
		w := rest
		if i < len(codes) {
			w = base[codes[i]]
			rest -= w
		}
		if (w > 0) != (it.paid > 0) {
			w = 0
		}
		weights[i] = new(big.Rat).SetInt64(int64(w))
		total.Add(total, weights[i])
	}
	// Some weight is in the payment's direction: a base, or, all bases
	// against it or none, the rest, which is then more than the gross less
	// the tax, itself more than 0. The total is not 0.
	out := map[string]money.Minor{}
	exact := make([]*big.Rat, len(weights))
	for i, w := range weights {
		exact[i] = new(big.Rat).Quo(new(big.Rat).Mul(new(big.Rat).SetInt64(int64(net)), w), total)
	}
	// Shares of the net, which is in range, each at most it: the
	// allocation cannot fail.
	shares, _ := money.Allocate(exact)
	for i, c := range codes {
		if shares[i] != 0 {
			out[c] = shares[i]
		}
	}
	return out
}

// discountPlan is a settlement's discount split.
type discountPlan struct {
	split discountSplit
}

// taxItems are the settlement's tax items (ADR-0049 §5): one per source
// item with a share, at the source's code, component and rate and the
// settlement's tax date, its amounts the shares in the lines' sign. Their
// local amounts are the shares converted at the settlement's rate, exactly,
// then rounded once per line by largest remainder (money.Allocate): a
// line's local amount is its transaction amount converted, rounded once,
// so the items on it add up to it, and items whose shares cancel (an
// invoice's and a credit memo's) to none. None without a discount split.
func (p discountPlan) taxItems(bank settlementBank) []sourcedTaxItem {
	if len(p.split.shares) == 0 {
		return nil
	}
	taxLC := shareLC(p.split.shares, bank, func(sh itemShare) (any, money.Minor) { return sh.src.key, sh.tax })
	codeLC := shareLC(p.split.shares, bank, func(sh itemShare) (any, money.Minor) {
		if !sh.codeBase {
			return nil, 0
		}
		return sh.src.key.code, sh.base
	})
	// Every component of a discounted item's code has the code's base share.
	baseLC := map[[2]any]money.Minor{}
	for i, sh := range p.split.shares {
		if sh.codeBase {
			baseLC[[2]any{sh.item, sh.src.key.code}] = codeLC[i]
		}
	}
	var out []sourcedTaxItem
	for i, sh := range p.split.shares {
		if sh.tax == 0 && sh.base == 0 {
			continue
		}
		var blc money.Minor
		if sh.base != 0 {
			blc = baseLC[[2]any{sh.item, sh.src.key.code}]
		}
		out = append(out, sourcedTaxItem{year: sh.src.entry.year, entry: sh.src.entry.entry, itemNo: sh.src.no, baseTC: -sh.base, taxTC: -sh.tax,
			baseLC: blc, taxLC: taxLC[i]})
	}
	return out
}

// shareLC converts the shares of each line, those pick gives one key, in
// the lines' sign: each exactly at the settlement's rate, then the line's
// total rounded once and shared out by largest remainder (money.Allocate).
// A share pick gives no key (nil) gets none.
func shareLC(shares []itemShare, bank settlementBank, pick func(itemShare) (any, money.Minor)) []money.Minor {
	out := make([]money.Minor, len(shares))
	on := map[any][]int{} // line key: the shares on it
	var keys []any
	for i, sh := range shares {
		if k, tc := pick(sh); k != nil && tc != 0 {
			if on[k] == nil {
				keys = append(keys, k)
			}
			on[k] = append(on[k], i)
		}
	}
	for _, k := range keys {
		exact := make([]*big.Rat, len(on[k]))
		for j, i := range on[k] {
			_, tc := pick(shares[i])
			// A stored rate is positive (rateOfType), the one thing ExactAt checks.
			x, _ := money.ExactAt(-tc, bank.rateLC.exact, bank.expTC)
			exact[j] = x.Mul(x, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(bank.expLC)), nil)))
		}
		// The rounded total is the line's converted amount, which paymentLines
		// converted in range: the allocation cannot fail.
		parts, _ := money.Allocate(exact)
		for j, i := range on[k] {
			out[i] = parts[j]
		}
	}
	return out
}

// firstComponent is the first component, in name order, of a code's
// items: its base is the code's.
func firstComponent(items []sourceItem, code string) string {
	first := ""
	for _, s := range items {
		if s.key.code == code && (first == "" || s.component < first) {
			first = s.component
		}
	}
	return first
}

// compareSourceItems orders tax items by account, code and component,
// then entry and item number.
func compareSourceItems(a, b sourceItem) int {
	return cmp.Or(compareTaxKeys(a.key, b.key), strings.Compare(a.component, b.component), slices.Compare(a.entry.entry[:], b.entry.entry[:]),
		cmp.Compare(a.no, b.no))
}

// compareTaxKeys orders tax lines by account, then tax code.
func compareTaxKeys(a, b taxKey) int {
	if c := slices.Compare(a.account[:], b.account[:]); c != 0 {
		return c
	}
	return strings.Compare(a.code, b.code)
}
