package ledger

import (
	"context"
	"fmt"
	"math/big"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Take-overs of a source system's GR/IR positions (T72f, ADR-0034 As
// built). A migration run takes an order item's position over onto the
// platform's order item: the source system's balance and open quantity on
// the item's GR/IR line, against a line on the GR/IR account without an
// order item. A later run tops it up: what the source system's position
// moved since the earlier runs' take-overs, read from their lines, so the
// take-overs together always hold the source system's position, beside
// whatever the platform itself posted on the item (review:batch21).

// GRIRTakenOverOrder is what migration runs' take-overs took over of an
// order's GR/IR positions: by item, and the source documents of their
// entries (the runs), so a caller can tell which runs it would top up.
type GRIRTakenOverOrder struct {
	Items   map[int32]GRIRTakenOver
	Sources []uuid.UUID
}

// TakenOverGRIR locks the orders' GR/IR positions until the transaction
// ends, as a posting on them does (lockOrders), and reads what migration
// runs' take-overs took over of their items in the legal entity, by order;
// an order they took nothing over of is left out. A later run passes an
// item's as GRIRItem.TakenOver to top its position up. The lock is taken
// before any period or number range, as a posting takes it, and keeps
// what was read current until the caller commits.
func (Postings) TakenOverGRIR(ctx context.Context, legalEntity uuid.UUID, orders []uuid.UUID) (map[uuid.UUID]GRIRTakenOverOrder, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	keys := make([]orderItem, len(orders))
	for i, o := range orders {
		keys[i] = orderItem{order: o}
	}
	if err := lockOrders(ctx, tx, keys); err != nil {
		return nil, err
	}
	taken, sources, err := takenOver(ctx, tx, legalEntity, orders)
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID]GRIRTakenOverOrder{}
	for k, t := range taken {
		o := out[k.order]
		if o.Items == nil {
			o = GRIRTakenOverOrder{Items: map[int32]GRIRTakenOver{}}
		}
		o.Items[k.item] = t
		for _, s := range sources[k] {
			if !slices.Contains(o.Sources, s) {
				o.Sources = append(o.Sources, s)
			}
		}
		out[k.order] = o
	}
	return out, nil
}

// takenOver reads what take-overs took over of the orders' items in the
// legal entity, and the source documents of their entries: the sum of the
// order item lines of integration entries, which only take-overs post
// (journal documents load without their order items), in the leading
// ledger, reversals included (a reversal keeps its entry's source
// document). The caller holds the orders' GR/IR locks, under which
// take-overs post.
func takenOver(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID, orders []uuid.UUID) (map[orderItem]GRIRTakenOver, map[orderItem][]uuid.UUID, error) {
	rows, _ := tx.Query(ctx, `SELECT l.purchase_order_id, l.po_item_no, trim_scale(coalesce(sum(l.quantity), 0))::text, sum(l.amount_tc)::bigint, sum(l.amount_lc)::bigint,
			coalesce(array_agg(DISTINCT e.source_document_id) FILTER (WHERE e.source_document_id IS NOT NULL), '{}')
		FROM ledger.journal_line l
		JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id AND e.source_context = 'integration'
		JOIN ledger.ledger lg ON lg.tenant_id = l.tenant_id AND lg.id = l.ledger_id AND lg.is_leading
		WHERE l.legal_entity_id = $1 AND l.purchase_order_id = ANY($2)
		GROUP BY l.purchase_order_id, l.po_item_no`, legalEntity, orders)
	out, sources := map[orderItem]GRIRTakenOver{}, map[orderItem][]uuid.UUID{}
	var k orderItem
	var t GRIRTakenOver
	var from []uuid.UUID
	if _, err := pgx.ForEachRow(rows, []any{&k.order, &k.item, &t.Quantity, &t.Balance, &t.BalanceLC, &from}, func() error {
		out[k], sources[k] = t, from
		return nil
	}); err != nil {
		return nil, nil, fmt.Errorf("ledger: read what was taken over of the GR/IR positions of %d orders: %w", len(orders), err)
	}
	return out, sources, nil
}

// takenOver is what take-overs took over of the entry's items, when it
// takes over (checkTakeOver: then all its items do); nil otherwise.
func (g grirPosting) takenOver(ctx context.Context, tx pgx.Tx, keys []orderItem) (map[orderItem]GRIRTakenOver, error) {
	if !slices.ContainsFunc(g.in.GRIR, func(it GRIRItem) bool { return it.TakesOver != nil }) {
		return nil, nil
	}
	orders := make([]uuid.UUID, len(keys))
	for i, k := range keys {
		orders[i] = k.order
	}
	taken, _, err := takenOver(ctx, tx, g.le.ID, orders) // the guard compares amounts only
	return taken, err
}

// checkTakeOver refuses an entry that mixes take-over items with others,
// or that is not a migration run's: only an integration entry's line on
// the GR/IR account may go without its order item (ledger.check_gr_ir_line),
// and that line is what a take-over posts against.
func checkTakeOver(in Posting) error {
	if !slices.ContainsFunc(in.GRIR, func(it GRIRItem) bool { return it.TakesOver != nil }) {
		return nil
	}
	if in.SourceContext != "integration" || slices.ContainsFunc(in.GRIR, func(it GRIRItem) bool { return it.TakesOver == nil }) {
		return fmt.Errorf("ledger: a GR/IR take-over is an integration entry of take-over items only; got source context %q", in.SourceContext)
	}
	return nil
}

// checkForeignTakeOver refuses local balances given (GRIRItem.TakesOverLC)
// or a rate given (Posting.Rate) on an entry with GR/IR items unless both
// are, for every item, on a take-over in a foreign document currency: a
// position in local currency keeps its local amounts equal to its
// transaction amounts (localMove), and local amounts kept as the source
// system booked them are recorded at its rate, not the average one.
// Neither is taken on a goods movement, an invoice or a clearing, which
// checkTakeOver and the item kinds then refuse with the local balance.
func checkForeignTakeOver(in Posting, local string) error {
	given := slices.ContainsFunc(in.GRIR, func(it GRIRItem) bool { return it.TakesOverLC != nil })
	if !given && in.Rate == nil {
		return nil
	}
	if in.Rate == nil || in.Currency == local || slices.ContainsFunc(in.GRIR, func(it GRIRItem) bool { return it.TakesOverLC == nil }) {
		return fmt.Errorf("ledger: GR/IR local balances taken over and a rate are given together, on every item of a take-over in a foreign "+
			"currency; got an entry in %s, local currency %s, rate given %t, local balances given %t", in.Currency, local, in.Rate != nil, given)
	}
	return nil
}

// takeOver is the pair of lines that moves what earlier take-overs took
// over of item i's order item (had) to the source system's position
// (GRIRItem.TakesOver, TakesOverLC and Quantity): the item's GR/IR line
// with the difference in balance and quantity, and a line on the GR/IR
// account without an order item that offsets it, both kept so they net
// to zero in every currency. Its local amount is the difference of the
// local balances when the item gives one (takeOverLocal), else converted
// at the posting's rates. It is refused unless had is what the caller
// read (GRIRItem.TakenOver), and for an item listed before in the entry
// (seen), so a position is never taken over twice.
func (g grirPosting) takeOver(had GRIRTakenOver, seen bool, i int) ([]Line, error) {
	it := &g.in.GRIR[i]
	read, err := takenRead(it.TakenOver)
	if err != nil {
		return nil, err
	}
	holds := new(big.Rat)
	if had.Quantity != "" { // something was taken over
		holds = money.ParseRat(had.Quantity)
	}
	if seen || holds.Cmp(read.qty) != 0 || had.Balance != read.tc || had.BalanceLC != read.lc {
		return nil, problem.Conflict("ledger.gr_ir_position_exists", fmt.Sprintf("Item %d of purchase order %s does not hold the GR/IR position taken over "+
			"that the take-over was computed from; a source system's position is taken over once, and topped up from what was taken over.",
			it.POItemNo, it.PurchaseOrderID))
	}
	q, err := signedQuantity(it.Quantity)
	if err != nil {
		return nil, fmt.Errorf("ledger: GR/IR quantity %q: %w", it.Quantity, err)
	}
	qty := q.Sub(q, holds).FloatString(money.QuantityDecimals)
	tc, err := money.Diff(*it.TakesOver, had.Balance)
	if err != nil {
		return nil, errTotalTooLarge("grir")
	}
	lc, gc, err := g.takeOverLocal(*it, tc, had.BalanceLC)
	if err != nil {
		return nil, err
	}
	// What a take-over moves, the difference of two balances in range, is
	// never -2^63: negating cannot overflow, and its conversions are
	// bounded by it.
	return []Line{{AccountID: g.grir, TC: tc, LC: lc, GC: gc, Quantity: &qty, Unit: &it.Unit, Text: it.Text, PurchaseOrderID: &it.PurchaseOrderID,
		POItemNo: &it.POItemNo}, {AccountID: g.grir, TC: -tc, LC: -lc, GC: -gc, Text: it.Text}}, nil
}

// takeOverLocal is the local and group amounts of a take-over item's
// GR/IR line that moves its balance by tc: with a local balance given
// (GRIRItem.TakesOverLC), the source system's conversion, what it moves
// from the local balance taken over (hadLC), the group amount converted
// from it; else tc converted at the posting's rates.
func (g grirPosting) takeOverLocal(it GRIRItem, tc, hadLC money.Minor) (lc, gc money.Minor, err error) {
	if it.TakesOverLC == nil {
		return g.rates.local(tc) // convert maps a conversion out of range
	}
	if lc, err = money.Diff(*it.TakesOverLC, hadLC); err != nil {
		return 0, 0, errTotalTooLarge("grir")
	}
	gc, err = convert(lc, g.rates.toGC, g.rates.expLC, g.rates.expGC)
	return lc, gc, err
}

// taken is what a caller read as taken over of an item.
type taken struct {
	qty    *big.Rat
	tc, lc money.Minor
}

// takenRead is what the caller read as taken over, the quantity and the
// balances, zero when it read none.
func takenRead(t *GRIRTakenOver) (taken, error) {
	if t == nil {
		return taken{qty: new(big.Rat)}, nil
	}
	q, err := signedQuantity(t.Quantity)
	if err != nil {
		return taken{}, fmt.Errorf("ledger: GR/IR quantity taken over %q: %w", t.Quantity, err)
	}
	return taken{qty: q, tc: t.Balance, lc: t.BalanceLC}, nil
}
