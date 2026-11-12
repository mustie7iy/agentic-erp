package procurement

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The comparison of an RFQ's quotations (ADR-0069 §7, T82c): computed on
// each read from the quotations and the RFQ's quantities and stored
// nowhere. It is by price alone and deterministic: amounts are compared
// exactly in minor units, and what compares equal stands in the order the
// suppliers were invited.

// Compared is one supplier invited to an RFQ in its comparison, with its
// quotation when it has one.
type Compared struct {
	Supplier  RFQSupplier
	Quotation *Quotation    // nil: the supplier has not answered
	Priced    []Item        // the RFQ's items at the quoted prices, by item number
	Amounts   []money.Minor // each priced item's value, beside Priced
	Unquoted  []int32       // the RFQ items without a price, by item number
	Total     money.Minor   // the sum of Amounts
	Expired   bool          // valid until a day before the comparison's
	Rank      int32         // 1 the lowest total; 0: not ranked
}

// CanWin reports whether the supplier's quotation is one an RFQ can be
// awarded to, which are the quotations the comparison ranks: it prices
// every item and is valid on the comparison's day.
func (c Compared) CanWin() bool {
	return c.Quotation != nil && len(c.Unquoted) == 0 && !c.Expired
}

// Comparison is an RFQ's quotations compared on AsOf, its company's date
// that day: Rows has every supplier invited once, the ranked first.
type Comparison struct {
	RFQ    RFQ // named
	AsOf   time.Time
	Lowest map[int32]money.Minor // by RFQ item number: the lowest amount a quotation not expired prices it at
	Rows   []Compared
}

// compared is supplier s's row in the comparison of RFQ r on day asOf:
// its quotation q's prices beside the RFQ's items, their amounts and
// total. A quotation is expired when it was valid until a day before
// asOf. An amount out of range fails the comparison rather than ranking
// the quotation at a wrong total; recording refuses such a quotation.
func compared(r RFQ, s RFQSupplier, q *Quotation, asOf time.Time) (Compared, error) {
	c := Compared{Supplier: s, Quotation: q}
	if q == nil {
		c.Unquoted = make([]int32, len(r.Items))
		for i, it := range r.Items {
			c.Unquoted[i] = it.No
		}
		return c, nil
	}
	c.Priced, c.Unquoted = q.priced(r)
	c.Expired = q.ValidUntil.Before(asOf)
	var err error
	if c.Amounts, err = itemAmounts(c.Priced); err != nil {
		return Compared{}, fmt.Errorf("procurement: amounts of supplier quotation %s: %w", q.ID, err)
	}
	if c.Total, err = money.Sum(c.Amounts...); err != nil {
		return Compared{}, fmt.Errorf("procurement: total of supplier quotation %s: %w", q.ID, err)
	}
	return c, nil
}

// compare compares the quotations of RFQ r on day asOf. The quotations
// that can win are ranked by total, the lowest first; equal totals share
// a rank (1, 1, 3) and keep the order their suppliers were invited in, as
// do the rows after them: the quotations not ranked, then the suppliers
// without one.
func compare(r RFQ, quotations []Quotation, asOf time.Time) (Comparison, error) {
	out := Comparison{RFQ: r, AsOf: asOf, Lowest: map[int32]money.Minor{}}
	var ranked, unranked, silent []Compared
	for _, s := range r.Suppliers {
		var q *Quotation
		if i := slices.IndexFunc(quotations, func(q Quotation) bool { return q.SupplierID == s.ID }); i >= 0 {
			q = &quotations[i]
		}
		c, err := compared(r, s, q, asOf)
		if err != nil {
			return Comparison{}, err
		}
		out.lower(c)
		switch {
		case c.CanWin():
			ranked = append(ranked, c)
		case q != nil:
			unranked = append(unranked, c)
		default:
			silent = append(silent, c)
		}
	}
	slices.SortStableFunc(ranked, func(a, b Compared) int { return cmp.Compare(a.Total, b.Total) })
	for i := range ranked {
		ranked[i].Rank = int32(i + 1)
		if i > 0 && ranked[i].Total == ranked[i-1].Total {
			ranked[i].Rank = ranked[i-1].Rank
		}
	}
	out.Rows = slices.Concat(ranked, unranked, silent)
	return out, nil
}

// lower takes row c's amounts into the lowest amount per RFQ item, unless
// its quotation is expired: a price that no longer holds is not the lowest.
func (c *Comparison) lower(row Compared) {
	if row.Expired {
		return
	}
	for i, it := range row.Priced {
		if low, ok := c.Lowest[it.No]; !ok || row.Amounts[i] < low {
			c.Lowest[it.No] = row.Amounts[i]
		}
	}
}

// lowest reports whether row's i-th priced item is at the lowest amount
// of its RFQ item; several rows at that amount all are.
func (c Comparison) lowest(row Compared, i int) bool {
	low, ok := c.Lowest[row.Priced[i].No]
	return ok && !row.Expired && row.Amounts[i] == low
}

// counts are the number of suppliers that have a quotation and the number
// whose quotation is ranked.
func (c Comparison) counts() (answered, ranked int32) {
	for _, row := range c.Rows {
		if row.Quotation != nil {
			answered++
		}
		if row.Rank > 0 {
			ranked++
		}
	}
	return answered, ranked
}

// quotationsOf reads every quotation of an RFQ, in the order recorded.
func quotationsOf(ctx context.Context, tx pgx.Tx, rfq uuid.UUID) ([]Quotation, error) {
	rows, _ := tx.Query(ctx, quotationSelect+` WHERE q.rfq_id = $1 ORDER BY q.created_at, q.id`, rfq)
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Quotation, error) { return scanQuotation(row) })
	if err != nil {
		return nil, fmt.Errorf("procurement: read the supplier quotations of RFQ %s: %w", rfq, err)
	}
	return list, nil
}

// comparison compares the quotations of an RFQ the caller may read on its
// legal entity's date today in its company's time zone.
func (e *Endpoints) comparison(ctx context.Context, id uuid.UUID) (Comparison, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Comparison{}, err
	}
	r, err := getRFQ(ctx, tx, id, false)
	if err != nil {
		return Comparison{}, err
	}
	named, err := e.named(ctx, r)
	if err != nil {
		return Comparison{}, err
	}
	today, _, err := e.todayOf(ctx, r.LegalEntityID)
	if err != nil {
		return Comparison{}, err
	}
	quotations, err := quotationsOf(ctx, tx, r.ID)
	if err != nil {
		return Comparison{}, err
	}
	return compare(named[0], quotations, today)
}
