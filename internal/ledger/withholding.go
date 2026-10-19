package ledger

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"math/big"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// Withholding is a withholding tax on a supplier open item (T46f,
// ADR-0037), as SAP keeps WITH_ITEM: the code, its rate and account when
// the item posted, and the base in the sign the item is paid in (positive
// on an invoice). A code deducted at invoice has the Amount the invoice
// withheld; one deducted at payment is withheld by the payment (AtPayment).
type Withholding struct {
	Code      string      `json:"code"`
	Posting   string      `json:"posting"`  // config.WithholdingAtInvoice or config.WithholdingAtPayment
	RatePct   string      `json:"rate_pct"` // decimal, e.g. "15.125"
	AccountID uuid.UUID   `json:"account_id"`
	Base      money.Minor `json:"base"`
	Amount    money.Minor `json:"amount"` // withheld at invoice; 0 for a code deducted at payment
}

// AtPayment is what a payment of the item withholds: the rate of the base
// less the cash discount's share of it, that is the base scaled by what the
// item pays after the discount over what it owes, rounded half away from
// zero once. The discount is of the item's sign and smaller than it
// (Settle checks it), so the result is of the base's sign and less than it.
func (w Withholding) AtPayment(owed, discount money.Minor) (money.Minor, error) {
	rate, err := money.ParsePlain(w.RatePct)
	if err != nil {
		return 0, err
	}
	v := new(big.Rat).Mul(new(big.Rat).SetInt64(int64(w.Base)), rate)
	v.Quo(v, big.NewRat(100, 1))
	if discount != 0 && owed != 0 {
		v.Mul(v, new(big.Rat).SetFrac(new(big.Int).Sub(big.NewInt(int64(owed)), big.NewInt(int64(discount))), big.NewInt(int64(owed))))
	}
	return money.Round(v, 0)
}

// PaymentWithholding is what a payment withholds from an item that owes
// owed less discount, by its withholding taxes deducted at payment.
func PaymentWithholding(ws []Withholding, owed, discount money.Minor) (money.Minor, error) {
	var total money.Minor
	for _, w := range ws {
		if w.Posting != config.WithholdingAtPayment {
			continue
		}
		amount, err := w.AtPayment(owed, discount)
		if err == nil {
			total, err = money.Sum(total, amount)
		}
		if err != nil {
			return 0, fmt.Errorf("ledger: withholding tax %s: %w", w.Code, err)
		}
	}
	return total, nil
}

// keepWithholding keeps the withholding taxes with the entry's supplier
// open item, its one line on a supplier reconciliation account that is no
// special G/L item; an entry with none, or with more than one, is refused.
func keepWithholding(ctx context.Context, tx pgx.Tx, entry Entry, ws []Withholding) error {
	if len(ws) == 0 {
		return nil
	}
	codes, postings, rates := make([]string, len(ws)), make([]string, len(ws)), make([]string, len(ws))
	bases, accounts, amounts := make([]money.Minor, len(ws)), make([]uuid.UUID, len(ws)), make([]*money.Minor, len(ws))
	for i, w := range ws {
		codes[i], postings[i], rates[i], bases[i], accounts[i] = w.Code, w.Posting, w.RatePct, w.Base, w.AccountID
		if w.Posting == config.WithholdingAtInvoice {
			amounts[i] = &ws[i].Amount
		}
	}
	tag, err := tx.Exec(ctx, `INSERT INTO ledger.open_item_withholding (tenant_id, fiscal_year, journal_entry_id, line_no, code, posting, rate_pct,
			base_amount, amount, account_id)
		SELECT l.tenant_id, l.fiscal_year, l.journal_entry_id, l.line_no, w.c, w.p, w.r::numeric, w.b, w.a, w.ac
		FROM ledger.journal_line l JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id AND a.reconciliation_kind = 'supplier',
			unnest($3::text[], $4::text[], $5::text[], $6::bigint[], $7::bigint[], $8::uuid[]) AS w(c, p, r, b, a, ac)
		WHERE l.fiscal_year = $1 AND l.journal_entry_id = $2 AND l.special_gl IS NULL`,
		entry.FiscalYear, entry.ID, codes, postings, rates, bases, amounts, accounts)
	if err != nil {
		return fmt.Errorf("ledger: keep the withholding taxes of entry %s: %w", entry.ID, err)
	}
	if tag.RowsAffected() != int64(len(ws)) {
		return fmt.Errorf("ledger: withholding taxes of entry %s, which has no single supplier open item to keep them with", entry.ID)
	}
	return nil
}

// withholdingColumn is an open item's withholding taxes deducted at
// payment, as JSON Withholding decodes, for ledger.journal_line l.
const withholdingColumn = `coalesce((SELECT json_agg(json_build_object('code', w.code, 'posting', w.posting, 'rate_pct', trim_scale(w.rate_pct)::text,
			'account_id', w.account_id, 'base', w.base_amount) ORDER BY w.code)
		FROM ledger.open_item_withholding w WHERE w.tenant_id = l.tenant_id AND w.fiscal_year = l.fiscal_year AND w.journal_entry_id = l.journal_entry_id
			AND w.line_no = l.line_no AND w.posting = 'payment'), '[]')`

// itemWithholding reads the withholding taxes deducted at payment of the
// items, in their order.
func itemWithholding(ctx context.Context, tx pgx.Tx, items []item) ([][]Withholding, error) {
	years, entries, lineNos := make([]int16, len(items)), make([]uuid.UUID, len(items)), make([]int32, len(items))
	for i, it := range items {
		years[i], entries[i], lineNos[i] = it.ref.FiscalYear, it.ref.EntryID, it.ref.LineNo
	}
	rows, _ := tx.Query(ctx, `SELECT `+withholdingColumn+` FROM unnest($1::smallint[], $2::uuid[], $3::int[]) WITH ORDINALITY AS r(y, e, n, o)
		JOIN ledger.journal_line l ON l.fiscal_year = r.y AND l.journal_entry_id = r.e AND l.line_no = r.n ORDER BY r.o`, years, entries, lineNos)
	out, err := pgx.CollectRows(rows, pgx.RowTo[[]Withholding])
	if err != nil {
		return nil, fmt.Errorf("ledger: withholding taxes of %d items: %w", len(items), err)
	}
	return out, nil
}

// withholdingKey is a withholding tax line of a settlement: one per
// account and code.
type withholdingKey struct {
	account uuid.UUID
	code    string
}

// settlementWithholding is what a settlement withholds from its items by
// their taxes deducted at payment (T46f): paymentWithholding of them.
func settlementWithholding(ctx context.Context, tx pgx.Tx, in Settlement, items []item) (map[withholdingKey]money.Minor, money.Minor, error) {
	taxes, err := itemWithholding(ctx, tx, items)
	if err != nil {
		return nil, 0, err
	}
	owed := make([]money.Minor, len(items))
	for i, it := range items {
		owed[i] = -it.tc
	}
	return paymentWithholding(taxes, owed, in.Discounts)
}

// paymentWithholding is what a payment withholds from items owing owed,
// less their discounts (none when nil), by their taxes: per account and
// code, and in all, in the sign the items are paid in.
func paymentWithholding(taxes [][]Withholding, owed, discounts []money.Minor) (map[withholdingKey]money.Minor, money.Minor, error) {
	out := map[withholdingKey]money.Minor{}
	var total money.Minor
	for i, ws := range taxes {
		var discount money.Minor
		if len(discounts) > 0 {
			discount = discounts[i]
		}
		for _, w := range ws {
			amount, err := w.AtPayment(owed[i], discount)
			if err != nil {
				return nil, 0, fmt.Errorf("ledger: withholding tax %s: %w", w.Code, err)
			}
			k := withholdingKey{account: w.AccountID, code: w.Code}
			if out[k], err = money.Sum(out[k], amount); err == nil {
				total, err = money.Sum(total, amount)
			}
			if err != nil {
				return nil, 0, errTotalTooLarge("withholding taxes")
			}
		}
	}
	// Items' amounts of opposite signs can cancel: no line then.
	maps.DeleteFunc(out, func(_ withholdingKey, v money.Minor) bool { return v == 0 })
	return out, total, nil
}

// withholdingLines credit what a settlement withholds to each code's
// account, in transaction currency (paymentLines converts them), in
// account and code order.
func withholdingLines(taxes map[withholdingKey]money.Minor, in Settlement) []Line {
	keys := slices.SortedFunc(maps.Keys(taxes), func(a, b withholdingKey) int {
		if c := slices.Compare(a.account[:], b.account[:]); c != 0 {
			return c
		}
		return cmp.Compare(a.code, b.code)
	})
	lines := make([]Line, len(keys))
	for i, k := range keys {
		text := "Withholding tax " + k.code
		if in.HeaderText != nil {
			text = *in.HeaderText + ", " + text
		}
		lines[i] = Line{AccountID: k.account, TC: -taxes[k], Text: &text}
	}
	return lines
}
