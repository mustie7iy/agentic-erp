package ledger

// GR/IR clearing (T33, 03-data-model §3.7). Each purchase order item's
// GR/IR position is the sum of its lines on the GR/IR account: their
// quantity, invoiced less received, and their amounts in the three
// currencies. Every goods movement and invoice moves the position so that
// its transaction-currency balance is the order price of its quantity,
// never the sum of rounded postings, and the posting that brings the
// quantity to zero leaves zero in every currency.

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// orderItem identifies a purchase order item.
type orderItem struct {
	order uuid.UUID
	item  int32
}

func compareOrderItems(a, b orderItem) int {
	return cmp.Or(bytes.Compare(a.order[:], b.order[:]), cmp.Compare(a.item, b.item))
}

// position is an order item's GR/IR position.
type position struct {
	qty        *big.Rat
	tc, lc, gc money.Minor
}

// lockOrders takes the tenant's GR/IR lock of each order, in one sorted
// call: postings on one order take turns on its positions, each reading
// them after the last committed. Rows of ledger.gr_ir_position are read
// for a posting and written (project) only under their order's lock. One lock per order rather than per item
// keeps a receipt of many items within the lock table. Taken after any
// entry bucket (a reversal holds its entry's) and before the period and
// number range post takes, so no two transactions wait in a cycle.
func lockOrders(ctx context.Context, tx pgx.Tx, keys []orderItem) error {
	orders := make([]uuid.UUID, len(keys))
	for i, k := range keys {
		orders[i] = k.order
	}
	slices.SortFunc(orders, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	orders = slices.Compact(orders)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('ledger.gr_ir:' || platform.require_tenant() || ':' || o, 0))
		FROM unnest($1::uuid[]) AS x(o)`, orders); err != nil {
		return fmt.Errorf("ledger: lock the GR/IR positions of %d orders: %w", len(orders), err)
	}
	return nil
}

// positions locks and reads the order items' positions in the legal
// entity: the projection of their lines on the GR/IR account
// (ledger.gr_ir_position, T33f), which the lock keeps current. An item
// posted in another currency than tc is refused: its amounts would not
// add up.
func positions(ctx context.Context, tx pgx.Tx, le uuid.UUID, tc string, keys []orderItem) (map[orderItem]*position, error) {
	if err := lockOrders(ctx, tx, keys); err != nil {
		return nil, err
	}
	// Once per item: an item listed twice would join its lines twice and
	// read its position doubled. The caller moves it once per listing.
	keys = slices.Compact(slices.SortedFunc(slices.Values(keys), compareOrderItems))
	orders, items := make([]uuid.UUID, len(keys)), make([]int32, len(keys))
	for i, k := range keys {
		orders[i], items[i] = k.order, k.item
	}
	rows, _ := tx.Query(ctx, `SELECT x.o, x.n, coalesce(trim_scale(p.invoiced + p.cleared + p.migrated - p.received), 0)::text, coalesce(p.amount_tc, 0), coalesce(p.amount_lc, 0),
			coalesce(p.amount_gc, 0), coalesce(p.currency_tc = $4, true)
		FROM unnest($2::uuid[], $3::int[]) AS x(o, n)
		LEFT JOIN ledger.gr_ir_position p ON p.legal_entity_id = $1 AND p.purchase_order_id = x.o AND p.po_item_no = x.n`, le, orders, items, tc)
	out := map[orderItem]*position{}
	var k orderItem
	var qty string
	var p position
	var same bool
	_, err := pgx.ForEachRow(rows, []any{&k.order, &k.item, &qty, &p.tc, &p.lc, &p.gc, &same}, func() error {
		if !same {
			return problem.Unprocessable("ledger.gr_ir_currency_mismatch", fmt.Sprintf("Item %d of purchase order %s has GR/IR postings in another currency than %s.", k.item, k.order, tc))
		}
		p.qty = money.ParseRat(qty)
		out[k] = &position{qty: p.qty, tc: p.tc, lc: p.lc, gc: p.gc}
		return nil
	})
	if err != nil {
		var pr *problem.Problem
		if errors.As(err, &pr) {
			return nil, pr
		}
		return nil, fmt.Errorf("ledger: read the GR/IR positions of %d order items: %w", len(keys), err)
	}
	return out, nil
}

// signedQuantity reads a plain decimal with an optional minus sign and at
// most the six decimals a line's quantity keeps: more would be computed
// exactly but stored rounded, and the position would drift from its lines.
func signedQuantity(s string) (*big.Rat, error) {
	q, err := money.ParsePlain(strings.TrimPrefix(s, "-"))
	if err != nil {
		return nil, err
	}
	if _, frac, _ := strings.Cut(s, "."); len(frac) > money.QuantityDecimals {
		return nil, fmt.Errorf("more than %d decimals", money.QuantityDecimals)
	}
	if strings.HasPrefix(s, "-") {
		q.Neg(q)
	}
	return q, nil
}

// rates converts transaction amounts at a posting's rates.
type rates struct {
	toLC, toGC          exchange
	expTC, expLC, expGC int
}

// local converts a transaction amount to local and group currency.
func (r rates) local(tc money.Minor) (lc, gc money.Minor, err error) {
	if lc, err = convert(tc, r.toLC, r.expTC, r.expLC); err != nil {
		return 0, 0, err
	}
	gc, err = convert(lc, r.toGC, r.expLC, r.expGC)
	return lc, gc, err
}

// share is part of a local or group balance: amount × after / before,
// rounded half away from zero.
func share(amount money.Minor, after, before *big.Rat) (money.Minor, error) {
	v := new(big.Rat).Mul(new(big.Rat).SetInt64(int64(amount)), after)
	return money.Round(v.Quo(v, before), 0)
}

// move moves the position by an item and returns the GR/IR line's amounts.
// In transaction currency the balance becomes the order price of the new
// quantity. In local and group currency a position that grows takes the
// posting's rates; one that shrinks keeps the value it was booked at, in
// the share its transaction balance keeps; one that reaches zero clears
// what is left; one that crosses zero clears what is left and values the
// rest at the posting's rates.
func (r rates) move(p *position, it GRIRItem) (tc, lc, gc money.Minor, err error) {
	q, err := signedQuantity(it.Quantity)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("ledger: GR/IR quantity %q: %w", it.Quantity, err)
	}
	after := new(big.Rat).Add(p.qty, q)
	target, err := money.Scale(it.UnitPrice, after.FloatString(6), int64(it.PriceUnit))
	if err == nil {
		tc, err = money.Diff(target, p.tc)
	}
	if err != nil {
		return 0, 0, 0, convertErr(err)
	}
	if lc, gc, err = r.localMove(p, after, tc, target); err != nil {
		return 0, 0, 0, convertErr(err)
	}
	newLC, err := money.Sum(p.lc, lc)
	newGC, errGC := money.Sum(p.gc, gc)
	if err = errors.Join(err, errGC); err != nil {
		return 0, 0, 0, errTotalTooLarge("grir")
	}
	p.qty, p.tc, p.lc, p.gc = after, target, newLC, newGC
	return tc, lc, gc, nil
}

// localMove is the GR/IR line's local and group amounts when the position
// moves to the quantity after, by tc to the balance target.
func (r rates) localMove(p *position, after *big.Rat, tc, target money.Minor) (lc, gc money.Minor, err error) {
	before, now := p.qty.Sign(), after.Sign()
	var keepLC, keepGC money.Minor // what the position keeps of its local and group balance
	switch {
	case now == 0:
	case before == now && new(big.Rat).Abs(after).Cmp(new(big.Rat).Abs(p.qty)) < 0:
		// The share the transaction balance keeps, so a position in the
		// local currency keeps local equal to transaction amounts; by
		// quantity when the order price is zero.
		kept, of := new(big.Rat).SetInt64(int64(target)), new(big.Rat).SetInt64(int64(p.tc))
		if p.tc == 0 {
			kept, of = after, p.qty
		}
		if keepLC, err = share(p.lc, kept, of); err == nil {
			keepGC, err = share(p.gc, kept, of)
		}
	case before == now:
		return r.local(tc)
	default:
		keepLC, keepGC, err = r.local(target)
	}
	if err != nil {
		return 0, 0, err
	}
	if lc, err = money.Diff(keepLC, p.lc); err == nil {
		gc, err = money.Diff(keepGC, p.gc)
	}
	return lc, gc, err
}

// grirPosting is a posting with GR/IR items, as far as it is established.
type grirPosting struct {
	in         Posting
	le         tenancy.Entity
	group      string
	rates      rates
	grir, diff uuid.UUID
	// diffCosts: the price difference account is a primary cost account,
	// so an invoiced item's price difference carries the item's cost
	// center (GRIRItem.CostCenterID); one that takes none derives its
	// assignment as any line does.
	diffCosts bool
}

// prepareGRIR resolves what a GR/IR posting books with: the legal entity,
// the rates of the posting date and the GR/IR and
// price difference accounts.
func (p Postings) prepareGRIR(ctx context.Context, tx pgx.Tx, in Posting) (grirPosting, error) {
	e := p.endpoints()
	g := grirPosting{in: in}
	var err error
	if g.le, err = e.legalEntity(ctx, in.LegalEntityID, "legal_entity_id"); err != nil {
		return g, err
	}
	if g.group, err = e.LegalEntities.GroupCurrency(ctx); err != nil {
		return g, err
	}
	var exps currency.Exponents
	if exps, err = exponents(ctx, tx, in.Currency, g.le.LocalCurrency, g.group); err != nil {
		return g, err
	}
	g.rates.expTC, g.rates.expLC, g.rates.expGC = exps[in.Currency], exps[g.le.LocalCurrency], exps[g.group]
	if g.rates.toLC, g.rates.toGC, err = postingRates(ctx, tx, in.Currency, g.le.LocalCurrency, g.group, in.PostingDate, nil); err != nil {
		return g, err
	}
	if g.grir, err = determined(ctx, g.le.ID, "gr_ir", "ledger.no_gr_ir_account"); err != nil {
		return g, err
	}
	if !slices.ContainsFunc(in.GRIR, func(it GRIRItem) bool { return it.Invoiced != nil || it.Clears != nil }) {
		return g, nil
	}
	if g.diff, err = determined(ctx, g.le.ID, "price_difference", "ledger.no_price_difference_account"); err != nil {
		return g, err
	}
	var element config.CostElement
	if err := tx.QueryRow(ctx, `SELECT cost_element FROM ledger.gl_account WHERE id = $1`, g.diff).Scan(&element); err != nil {
		return g, fmt.Errorf("ledger: cost element of the price difference account %s: %w", g.diff, err)
	}
	g.diffCosts = element == config.CostElementPrimaryCost
	return g, nil
}

// determined is the legal entity's account for the key, or the problem
// code when it has none.
func determined(ctx context.Context, le uuid.UUID, key, code string) (uuid.UUID, error) {
	account, ok, err := Accounts{}.Determination(ctx, le, key)
	if err == nil && !ok {
		err = problem.Unprocessable(code, "The legal entity has no "+key+" account in its account determination.")
	}
	return account, err
}

// lines builds the entry: the posting's own lines, each item's GR/IR line
// and counter line, converted at the posting's rates, and what the GR/IR
// lines' local and group amounts leave as exchange differences.
func (g grirPosting) lines(ctx context.Context, tx pgx.Tx) ([]Line, error) {
	lines, kept, err := g.ownLines()
	if err != nil {
		return nil, err
	}
	if err := checkTakeOver(g.in); err != nil {
		return nil, err
	}
	keys := make([]orderItem, len(g.in.GRIR))
	for i, it := range g.in.GRIR {
		if kinds := btoi(it.OffsetAccountID != nil) + btoi(it.Invoiced != nil) + btoi(it.Clears != nil) + btoi(it.TakesOver != nil); kinds != 1 {
			return nil, fmt.Errorf("ledger: GR/IR item %d of order %s: a goods movement names the offset account, an invoice its amount, a clearing "+
				"the position it clears, a take-over the balance it takes over; got %d of them", it.POItemNo, it.PurchaseOrderID, kinds)
		}
		keys[i] = orderItem{it.PurchaseOrderID, it.POItemNo}
	}
	pos, err := positions(ctx, tx, g.le.ID, g.in.Currency, keys)
	if err != nil {
		return nil, err
	}
	taken, err := g.takenOver(ctx, tx, keys)
	if err != nil {
		return nil, err
	}
	moved, keep, err := g.itemLines(pos, taken, keys)
	if err != nil {
		return nil, err
	}
	return g.convert(ctx, append(lines, moved...), append(kept, keep...))
}

// ownLines are the posting's own lines and which of them keep the local
// amount they are given (a tax line's at the tax date's rate), its group
// amount converted from it.
func (g grirPosting) ownLines() ([]Line, []bool, error) {
	lines := make([]Line, 0, len(g.in.Lines)+2*len(g.in.GRIR)+2)
	kept := make([]bool, 0, cap(lines))
	for _, l := range g.in.Lines {
		line := Line{AccountID: l.AccountID, TC: l.Amount, PartnerID: l.PartnerID, TaxCode: l.TaxCode, Quantity: l.Quantity, Unit: l.Unit, Text: l.Text,
			SpecialGL: l.SpecialGL, CostCenterID: l.CostCenterID, ProfitCenterID: l.ProfitCenterID, SegmentID: l.SegmentID}
		if l.LC != nil {
			gc, err := convert(*l.LC, g.rates.toGC, g.rates.expLC, g.rates.expGC)
			if err != nil {
				return nil, nil, err
			}
			line.LC, line.GC = *l.LC, gc
		}
		lines, kept = append(lines, line), append(kept, l.LC != nil)
	}
	return lines, kept, nil
}

// itemLines are each item's GR/IR line and its counter line, and which of
// them keep the local and group amounts they are given: the GR/IR line
// and a goods movement's mirror of it, not an invoice's price difference,
// which is converted at the posting's rates. A take-over item moves what
// earlier take-overs took over of its order item (taken) to the source
// system's position.
func (g grirPosting) itemLines(pos map[orderItem]*position, taken map[orderItem]GRIRTakenOver, keys []orderItem) ([]Line, []bool, error) {
	var lines []Line
	var kept []bool
	seen := map[orderItem]bool{}
	for i, it := range g.in.GRIR {
		if it.TakesOver != nil {
			moved, err := g.takeOver(taken[keys[i]], seen[keys[i]], i)
			if err != nil {
				return nil, nil, err
			}
			seen[keys[i]] = true
			lines, kept = append(lines, moved...), append(kept, true, true)
			continue
		}
		if err := g.checkClears(pos[keys[i]], it); err != nil {
			return nil, nil, err
		}
		tc, lc, gc, err := g.rates.move(pos[keys[i]], it)
		if err != nil {
			return nil, nil, err
		}
		// An invoiced item's lines are its tax code's base; a goods
		// movement's carry none.
		var code *string
		if it.Invoiced != nil {
			code = it.TaxCode
		}
		lines = append(lines, Line{AccountID: g.grir, TC: tc, LC: lc, GC: gc, Quantity: &g.in.GRIR[i].Quantity, Unit: &g.in.GRIR[i].Unit,
			Text: it.Text, PurchaseOrderID: &g.in.GRIR[i].PurchaseOrderID, POItemNo: &g.in.GRIR[i].POItemNo, TaxCode: code})
		kept = append(kept, true)
		counter, keep, err := g.counterLine(it, tc, lc, gc, code)
		if err != nil {
			return nil, nil, err
		}
		if counter != nil {
			lines, kept = append(lines, *counter), append(kept, keep)
		}
	}
	return lines, kept, nil
}

// counterLine is an item's line against its GR/IR line, if it has one,
// and whether it keeps the local and group amounts it is given: a goods
// movement's mirror on its offset account and a clearing's on the price
// difference account do; an invoice's price difference, converted at the
// posting's rates, does not. An item that moves nothing has none.
func (g grirPosting) counterLine(it GRIRItem, tc, lc, gc money.Minor, code *string) (*Line, bool, error) {
	moved := tc != 0 || lc != 0 || gc != 0
	switch {
	case it.OffsetAccountID != nil && moved:
		return &Line{AccountID: *it.OffsetAccountID, TC: -tc, LC: -lc, GC: -gc, Text: it.Text, CostCenterID: it.CostCenterID}, true, nil
	case it.Clears != nil && moved:
		return g.priceDifference(Line{AccountID: g.diff, TC: -tc, LC: -lc, GC: -gc, Text: it.Text}, it), true, nil
	case it.Invoiced != nil:
		diff, err := money.Diff(*it.Invoiced, tc)
		if err != nil {
			return nil, false, errTotalTooLarge("grir")
		}
		if diff != 0 {
			return g.priceDifference(Line{AccountID: g.diff, TC: diff, Text: it.Text, TaxCode: code}, it), false, nil
		}
	}
	return nil, false, nil
}

// priceDifference is a line on the price difference account, carrying the
// item's cost center when that account is a primary cost account.
func (g grirPosting) priceDifference(line Line, it GRIRItem) *Line {
	if g.diffCosts {
		line.CostCenterID = it.CostCenterID
	}
	return &line
}

// checkClears refuses a clearing item unless the position still holds
// what was approved: the quantity it clears, the balance in transaction
// currency and the price difference account it posts to. Other items pass.
func (g grirPosting) checkClears(p *position, it GRIRItem) error {
	if it.Clears == nil {
		return nil
	}
	q, err := signedQuantity(it.Quantity)
	if err != nil {
		return fmt.Errorf("ledger: GR/IR quantity %q: %w", it.Quantity, err)
	}
	if new(big.Rat).Add(p.qty, q).Sign() != 0 || p.tc != it.Clears.Balance || g.diff != it.Clears.Account {
		return problem.Conflict("ledger.gr_ir_clearing_outdated", fmt.Sprintf("Item %d of purchase order %s no longer holds the GR/IR position, "+
			"or the price difference account is no longer the one, that the clearing was approved for; propose the clearing again.", it.POItemNo, it.PurchaseOrderID))
	}
	return nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// convert converts the lines at the posting's rates, keeps the local and
// group amounts of the lines marked kept, and books what that leaves in
// local and group currency as exchange differences.
func (g grirPosting) convert(ctx context.Context, lines []Line, kept []bool) ([]Line, error) {
	tcs := make([]money.Minor, len(lines))
	given := slices.Clone(lines)
	for i, l := range lines {
		tcs[i] = l.TC
	}
	if !netsToZero(tcs...) {
		sum, _ := money.Sum(tcs...) // out of range reads as 0: unbalanced either way
		return nil, problem.Unprocessable("ledger.unbalanced", "Debits and credits differ by "+sum.Plain(g.rates.expTC)+" "+g.in.Currency+".")
	}
	if err := convertLines(lines, g.rates.toLC, g.rates.toGC, g.rates.expTC, g.rates.expLC, g.rates.expGC); err != nil {
		return nil, err
	}
	lcs, gcs := make([]money.Minor, len(lines)), make([]money.Minor, len(lines))
	for i := range lines {
		if kept[i] {
			lines[i].LC, lines[i].GC = given[i].LC, given[i].GC
		}
		lcs[i], gcs[i] = lines[i].LC, lines[i].GC
	}
	lc, err := money.Sum(lcs...)
	var gc money.Minor
	if err == nil {
		gc, err = money.Sum(gcs...)
	}
	if err != nil {
		return nil, errTotalTooLarge("lines")
	}
	fx, err := fxLines(ctx, g.le.ID, -lc, -gc, g.in.HeaderText)
	return append(lines, fx...), err
}

// postGRIR posts an entry with GR/IR items (Posting.GRIR).
func (p Postings) postGRIR(ctx context.Context, in Posting) (Entry, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return Entry{}, err
	}
	g, err := p.prepareGRIR(ctx, tx, in)
	if err != nil {
		return Entry{}, err
	}
	lines, err := g.lines(ctx, tx)
	if err != nil {
		return Entry{}, err
	}
	e := p.endpoints()
	if err := e.checkAccounts(ctx, tx, g.le.ID, lines, true); err != nil {
		return Entry{}, err
	}
	taxDate := in.PostingDate
	if in.TaxDate != nil {
		taxDate = *in.TaxDate
	}
	if err := checkTaxCodes(ctx, tx, g.le.ID, taxDate, lines, in.TaxItems != nil); err != nil {
		return Entry{}, err
	}
	if err := holdTaxDate(ctx, e.TaxReturns, g.le.ID, in.TaxDateChosen, taxDate, lines); err != nil {
		return Entry{}, err
	}
	posted, err := e.post(ctx, tx, actor, draft{entity: g.le, documentType: in.DocumentType, documentDate: in.DocumentDate,
		postingDate: in.PostingDate, taxDate: taxDate, currencyTC: in.Currency, currencyLC: g.le.LocalCurrency, currencyGC: g.group, rateTCLC: g.rates.toLC.shown,
		rateLCGC: g.rates.toGC.shown, reference: in.Reference, headerText: in.HeaderText, sourceContext: in.SourceContext,
		sourceDocumentID: &in.SourceDocumentID, lines: lines, givenItems: in.TaxItems})
	if err != nil {
		return Entry{}, err
	}
	if err := recordGRIRClearing(ctx, tx, posted, in); err != nil {
		return Entry{}, err
	}
	if err := project(ctx, tx, posted); err != nil {
		return Entry{}, err
	}
	if err := keepItemData(ctx, tx, posted, in); err != nil {
		return Entry{}, err
	}
	return posted, audit.Add(ctx, audit.Entry{Action: "ledger.journal_entry.post", ObjectType: "ledger.journal_entry", ObjectID: posted.ID,
		Outcome: "succeeded", Detail: map[string]any{"document_number": posted.DocumentNumber, "fiscal_year": posted.FiscalYear, "lines": len(lines),
			"source_context": posted.SourceContext}})
}

// recordGRIRClearing records an entry of clearing items as a GR/IR clearing
// (ledger.gr_ir_clearing_entry), before it is projected: its GR/IR lines'
// quantities are cleared, neither received nor invoiced. Clearing items
// are never mixed with others, whose quantities would then read cleared.
func recordGRIRClearing(ctx context.Context, tx pgx.Tx, e Entry, in Posting) error {
	clears := slices.ContainsFunc(in.GRIR, func(it GRIRItem) bool { return it.Clears != nil })
	if !clears {
		return nil
	}
	if slices.ContainsFunc(in.GRIR, func(it GRIRItem) bool { return it.Clears == nil }) {
		return fmt.Errorf("ledger: entry %s mixes GR/IR clearing items with others", e.ID)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.gr_ir_clearing_entry (tenant_id, fiscal_year, journal_entry_id) VALUES (platform.require_tenant(), $1, $2)`,
		e.FiscalYear, e.ID); err != nil {
		return fmt.Errorf("ledger: record entry %s as a GR/IR clearing: %w", e.ID, err)
	}
	return nil
}

// grirAdjustments are what a reversal adds to its mirrored lines: where
// they bring an order item's quantity to zero but rounding leaves a
// balance (the item moved since the reversed entry), a GR/IR line clears
// it against the price difference account, so a position is zero in every
// currency once its quantities agree. A legal entity without a GR/IR
// account has no positions to adjust.
func grirAdjustments(ctx context.Context, tx pgx.Tx, orig Entry, lines []Line) ([]Line, error) {
	grir, ok, err := Accounts{}.Determination(ctx, orig.LegalEntityID, "gr_ir")
	if err != nil || !ok {
		return lines, err
	}
	moves, err := reversalMoves(lines, grir)
	if err != nil || len(moves) == 0 {
		return lines, err
	}
	keys := slices.SortedFunc(maps.Keys(moves), compareOrderItems)
	pos, err := positions(ctx, tx, orig.LegalEntityID, orig.CurrencyTC, keys)
	if err != nil {
		return nil, err
	}
	return appendAdjustments(ctx, orig.LegalEntityID, lines, keys, moves, pos)
}

// reversalMoves sums a reversal's GR/IR lines per order item.
func reversalMoves(lines []Line, grir uuid.UUID) (map[orderItem]*move, error) {
	moves := map[orderItem]*move{}
	for _, l := range lines {
		if l.PurchaseOrderID == nil || l.AccountID != grir {
			continue
		}
		k := orderItem{*l.PurchaseOrderID, *l.POItemNo}
		if moves[k] == nil {
			moves[k] = &move{qty: new(big.Rat), line: l}
		}
		if err := moves[k].add(l); err != nil {
			return nil, err
		}
	}
	return moves, nil
}

// move is what a reversal's lines do to one order item's position.
type move struct {
	qty    *big.Rat
	amount [3]money.Minor // transaction, local, group
	line   Line           // one of them: the account, unit and order item
}

// add adds a line; a total out of range is too large to post.
func (m *move) add(l Line) (err error) {
	m.qty.Add(m.qty, money.ParseRat(*l.Quantity))
	m.amount, err = add3(m.amount, [3]money.Minor{l.TC, l.LC, l.GC})
	return err
}

// add3 adds amounts in the three currencies; a total out of range is too
// large to post.
func add3(a, b [3]money.Minor) ([3]money.Minor, error) {
	var out [3]money.Minor
	for i := range out {
		var err error
		if out[i], err = money.Sum(a[i], b[i]); err != nil {
			return out, errTotalTooLarge("lines")
		}
	}
	return out, nil
}

// appendAdjustments appends, for each order item the reversal brings to a
// zero quantity with a balance left, the pair that clears it.
func appendAdjustments(ctx context.Context, le uuid.UUID, lines []Line, keys []orderItem, moves map[orderItem]*move, pos map[orderItem]*position) ([]Line, error) {
	var diff uuid.UUID
	for _, k := range keys {
		m, p := moves[k], pos[k]
		if new(big.Rat).Add(p.qty, m.qty).Sign() != 0 {
			continue
		}
		left, err := add3([3]money.Minor{p.tc, p.lc, p.gc}, m.amount)
		if err != nil {
			return nil, err
		}
		if left == ([3]money.Minor{}) {
			continue
		}
		if diff == uuid.Nil {
			if diff, err = determined(ctx, le, "price_difference", "ledger.no_price_difference_account"); err != nil {
				return nil, err
			}
		}
		zero := "0"
		// Residues of rounding and of rates, never near the range: negating
		// them cannot overflow.
		lines = append(lines, Line{AccountID: m.line.AccountID, TC: -left[0], LC: -left[1], GC: -left[2], Quantity: &zero, Unit: m.line.Unit,
			PurchaseOrderID: m.line.PurchaseOrderID, POItemNo: m.line.POItemNo}, Line{AccountID: diff, TC: left[0], LC: left[1], GC: left[2]})
	}
	return lines, nil
}

// project adds a posted entry's lines on the legal entity's GR/IR account
// to their order items' positions (ledger.gr_ir_position, T33f), in the
// transaction that posts them and under the orders' GR/IR locks, which
// the caller holds: receipts' quantities to received (negated), invoices'
// to invoiced, a clearing's (ledger.gr_ir_clearing_entry, T72e) to
// cleared, a migration run's take-over's (integration, T72f) to migrated,
// every line's amounts to the balance. An entry without an order
// item on its lines has nothing to add.
func project(ctx context.Context, tx pgx.Tx, e Entry) error {
	if !slices.ContainsFunc(e.Lines, func(l Line) bool { return l.PurchaseOrderID != nil }) {
		return nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.gr_ir_position AS p (tenant_id, legal_entity_id, purchase_order_id, po_item_no, account_id,
			currency_tc, currency_lc, currency_gc, uom_code, received, invoiced, cleared, migrated, amount_tc, amount_lc, amount_gc, last_posting_date)
		SELECT l.tenant_id, l.legal_entity_id, l.purchase_order_id, l.po_item_no, l.account_id, $3, $4, $5, min(l.uom_code),
			CASE WHEN $6 = 'procurement' AND NOT c.clearing THEN -sum(l.quantity) ELSE 0 END, CASE WHEN $6 = 'payables' THEN sum(l.quantity) ELSE 0 END,
			CASE WHEN c.clearing THEN sum(l.quantity) ELSE 0 END, CASE WHEN $6 = 'integration' THEN sum(l.quantity) ELSE 0 END, sum(l.amount_tc), sum(l.amount_lc), sum(l.amount_gc), max(l.posting_date)
		FROM ledger.journal_line l
		JOIN ledger.account_determination d ON d.tenant_id = l.tenant_id AND d.legal_entity_id = l.legal_entity_id AND d.key = 'gr_ir' AND d.account_id = l.account_id
		CROSS JOIN (SELECT EXISTS (SELECT FROM ledger.gr_ir_clearing_entry WHERE fiscal_year = $1 AND journal_entry_id = $2) AS clearing) c
		WHERE l.fiscal_year = $1 AND l.journal_entry_id = $2 AND l.purchase_order_id IS NOT NULL
		GROUP BY l.tenant_id, l.legal_entity_id, l.purchase_order_id, l.po_item_no, l.account_id, c.clearing
		ON CONFLICT (tenant_id, legal_entity_id, purchase_order_id, po_item_no) DO UPDATE SET received = p.received + EXCLUDED.received,
			invoiced = p.invoiced + EXCLUDED.invoiced, cleared = p.cleared + EXCLUDED.cleared, migrated = p.migrated + EXCLUDED.migrated, amount_tc = p.amount_tc + EXCLUDED.amount_tc, amount_lc = p.amount_lc + EXCLUDED.amount_lc,
			amount_gc = p.amount_gc + EXCLUDED.amount_gc, last_posting_date = greatest(p.last_posting_date, EXCLUDED.last_posting_date),
			version = p.version + 1`, e.FiscalYear, e.ID, e.CurrencyTC, e.CurrencyLC, e.CurrencyGC, e.SourceContext); err != nil {
		return fmt.Errorf("ledger: project the GR/IR positions of entry %s: %w", e.ID, err)
	}
	return nil
}
