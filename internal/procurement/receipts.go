package procurement

import (
	"cmp"
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func init() {
	problem.RegisterConstraint("goods_movement_item_tenant_id_purchase_order_id_po_item_no_fkey", func() *problem.Problem {
		return problem.Unprocessable("procurement.unknown_order_item", "A purchase order item does not exist.",
			problem.FieldError{Field: "items", Code: "not_found", Message: "an item of the purchase order"})
	})
}

// Movement types (SAP codes).
const (
	Receipt        = "101"
	Reversal       = "102" // of a receipt
	Return         = "122" // to the supplier (T47b)
	ReturnReversal = "123" // of a return
)

// reversals is the movement type that reverses each reversible one.
var reversals = map[string]string{Receipt: Reversal, Return: ReturnReversal}

// ReceiptItem is one item of a goods movement; reversals have negative
// quantities.
type ReceiptItem struct {
	No              int32     `json:"item_no"`
	PurchaseOrderID uuid.UUID `json:"purchase_order_id"`
	POItemNo        int32     `json:"po_item_no"`
	Quantity        string    `json:"quantity"`
	Unit            string    `json:"unit"`
}

// GoodsReceipt is a row of procurement.goods_movement with its items.
type GoodsReceipt struct {
	ID             uuid.UUID
	DocumentNumber string
	LegalEntityID  uuid.UUID
	PlantID        *uuid.UUID
	MovementType   string
	DocumentDate   time.Time
	PostingDate    time.Time
	Reference      *string
	ReversalOfID   *uuid.UUID
	ReversedByID   *uuid.UUID
	Reason         *string // why goods go back (122)
	// The WE entry the movement posted for its items of orders valued
	// through GR/IR (T33), or nil when it has none.
	JournalEntryYear *int16
	JournalEntryID   *uuid.UUID
	Items            []ReceiptItem
	PostedAt         time.Time
	PostedBy         uuid.UUID
}

const receiptSelect = `SELECT m.id, m.document_number, m.legal_entity_id, m.plant_id, m.movement_type, m.document_date, m.posting_date, m.reference,
		m.reversal_of_id, (SELECT r.id FROM procurement.goods_movement r WHERE r.tenant_id = m.tenant_id AND r.reversal_of_id = m.id LIMIT 1) AS reversed_by,
		m.posted_at, m.posted_by, m.journal_entry_year, m.journal_entry_id, m.reason,
		(SELECT json_agg(json_build_object('item_no', i.item_no, 'purchase_order_id', i.purchase_order_id, 'po_item_no', i.po_item_no,
			'quantity', trim_scale(i.quantity)::text, 'unit', i.uom_code) ORDER BY i.item_no)
		 FROM procurement.goods_movement_item i WHERE i.tenant_id = m.tenant_id AND i.goods_movement_id = m.id)
	FROM procurement.goods_movement m`

func scanReceipt(row pgx.Row) (GoodsReceipt, error) {
	var g GoodsReceipt
	if err := row.Scan(&g.ID, &g.DocumentNumber, &g.LegalEntityID, &g.PlantID, &g.MovementType, &g.DocumentDate, &g.PostingDate, &g.Reference,
		&g.ReversalOfID, &g.ReversedByID, &g.PostedAt, &g.PostedBy, &g.JournalEntryYear, &g.JournalEntryID, &g.Reason, &g.Items); err != nil {
		return GoodsReceipt{}, fmt.Errorf("scan goods receipt: %w", err)
	}
	return g, nil
}

func getReceipt(ctx context.Context, tx pgx.Tx, id uuid.UUID) (GoodsReceipt, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return GoodsReceipt{}, err
	}
	g, err := scanReceipt(tx.QueryRow(ctx, receiptSelect+` WHERE m.id = $1`, id))
	if err = authz.Visible(err, access, g.LegalEntityID, problem.NotFoundOf("goods receipt"), "procurement: get goods receipt %s", id); err != nil {
		return GoodsReceipt{}, err
	}
	return g, nil
}

// GetReceipt returns a goods receipt or reversal.
func GetReceipt(ctx context.Context, id uuid.UUID) (GoodsReceipt, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return GoodsReceipt{}, err
	}
	return getReceipt(ctx, tx, id)
}

// orderItem is a purchase order item as a receipt sees it.
type orderItem struct {
	found               bool
	legalEntity         uuid.UUID
	status, unit        string
	ordered, tolerance  string
	received            string
	documentNumber      string
	currency, valuation string
	valuationPrice      money.Minor // fixed at the item's release (T47a)
	valuationPriceUnit  int32
	account             uuid.UUID
	costCenter          *uuid.UUID // the item's, which its WE entry's counter line carries (T56e)
}

// lockItems locks the purchase order items a movement touches, in a fixed
// order, and returns what each one is.
func lockItems(ctx context.Context, tx pgx.Tx, items []ReceiptItem) ([]orderItem, error) {
	orders, nos := make([]uuid.UUID, len(items)), make([]int32, len(items))
	for i, it := range items {
		orders[i], nos[i] = it.PurchaseOrderID, it.POItemNo
	}
	if _, err := tx.Exec(ctx, `SELECT FROM procurement.purchase_order_item i JOIN unnest($1::uuid[], $2::int[]) AS x(o, n)
		ON i.purchase_order_id = x.o AND i.item_no = x.n ORDER BY i.purchase_order_id, i.item_no FOR UPDATE OF i`, orders, nos); err != nil {
		return nil, fmt.Errorf("procurement: lock %d purchase order items: %w", len(items), err)
	}
	rows, _ := tx.Query(ctx, `SELECT i.item_no IS NOT NULL, coalesce(o.legal_entity_id, $3), coalesce(o.status, ''), coalesce(i.uom_code, ''),
			coalesce(trim_scale(i.quantity)::text, '0'), coalesce(trim_scale(i.over_delivery_pct)::text, '0'),
			trim_scale(coalesce((SELECT sum(g.quantity) FROM procurement.goods_movement_item g WHERE g.tenant_id = i.tenant_id
				AND g.purchase_order_id = i.purchase_order_id AND g.po_item_no = i.item_no), 0))::text, coalesce(o.document_number, ''),
			coalesce(o.currency, ''), coalesce(o.valuation, ''), coalesce(i.valuation_price, 0), coalesce(i.valuation_price_unit, 1), coalesce(i.account_id, $3),
			i.cost_center_id
		FROM unnest($1::uuid[], $2::int[]) WITH ORDINALITY AS x(o, n, ord)
		LEFT JOIN procurement.purchase_order o ON o.id = x.o
		LEFT JOIN procurement.purchase_order_item i ON i.tenant_id = o.tenant_id AND i.purchase_order_id = o.id AND i.item_no = x.n AND NOT i.is_removed
		ORDER BY x.ord`, orders, nos, uuid.Nil)
	var out []orderItem
	var oi orderItem
	_, err := pgx.ForEachRow(rows, []any{&oi.found, &oi.legalEntity, &oi.status, &oi.unit, &oi.ordered, &oi.tolerance, &oi.received, &oi.documentNumber,
		&oi.currency, &oi.valuation, &oi.valuationPrice, &oi.valuationPriceUnit, &oi.account, &oi.costCenter},
		func() error { out = append(out, oi); return nil })
	if err != nil {
		return nil, fmt.Errorf("procurement: read %d purchase order items: %w", len(items), err)
	}
	return out, nil
}

// insertMovement writes a goods movement with its items, posting its WE
// entry first for the items of orders valued through GR/IR unless post is
// false (a migration run's receipt, PostLoadedReceipt; current holds each
// item's order item, as lockItems read it), and returns it. The
// goods_receipt number range is procurement's own: no posting of the
// ledger's that holds a GR/IR lock waits for it, so taking it first adds
// no cycle to the ledger's lock order.
func (e *Endpoints) insertMovement(ctx context.Context, tx pgx.Tx, p reqctx.Principal, g GoodsReceipt, current []orderItem, post bool) (GoodsReceipt, error) {
	number, err := e.number(ctx, g.LegalEntityID, "goods_receipt", g.PostingDate)
	if err != nil {
		return GoodsReceipt{}, err
	}
	id := uuid.Must(uuid.NewV7())
	var year *int16
	var entry *uuid.UUID
	if post {
		if year, entry, err = e.postMovement(ctx, id, number, g, current); err != nil {
			return GoodsReceipt{}, err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO procurement.goods_movement (tenant_id, id, document_number, legal_entity_id, plant_id, movement_type,
			document_date, posting_date, reference, reversal_of_id, posted_by, journal_entry_year, journal_entry_id, reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`, p.TenantID, id, number, g.LegalEntityID, g.PlantID, g.MovementType,
		g.DocumentDate, g.PostingDate, g.Reference, g.ReversalOfID, p.ID, year, entry, g.Reason); err != nil {
		return GoodsReceipt{}, fmt.Errorf("procurement: insert goods movement %s: %w", number, err)
	}
	n := len(g.Items)
	nos, orders, poItems, quantities, units := make([]int32, n), make([]uuid.UUID, n), make([]int32, n), make([]string, n), make([]string, n)
	for i, it := range g.Items {
		nos[i], orders[i], poItems[i], quantities[i], units[i] = int32(i+1), it.PurchaseOrderID, it.POItemNo, it.Quantity, it.Unit
	}
	if _, err := tx.Exec(ctx, `INSERT INTO procurement.goods_movement_item (tenant_id, goods_movement_id, item_no, purchase_order_id, po_item_no, quantity, uom_code)
		SELECT $1, $2, n, o, pi, q::numeric, u FROM unnest($3::int[], $4::uuid[], $5::int[], $6::text[], $7::text[]) AS x(n, o, pi, q, u)`,
		p.TenantID, id, nos, orders, poItems, quantities, units); err != nil {
		return GoodsReceipt{}, fmt.Errorf("procurement: insert items of goods movement %s: %w", number, err)
	}
	return getReceipt(ctx, tx, id)
}

func movementItems(g GoodsReceipt) []map[string]any {
	items := make([]map[string]any, len(g.Items))
	for i, it := range g.Items {
		items[i] = map[string]any{"item_no": it.No, "purchase_order_id": it.PurchaseOrderID, "po_item_no": it.POItemNo,
			"quantity": map[string]string{"value": it.Quantity, "unit": it.Unit}}
	}
	return items
}

// ReceiptInput is the input of postReceipt.
type ReceiptInput struct {
	LegalEntityID             uuid.UUID
	PlantID                   *uuid.UUID
	MovementType              string // Receipt (the default) or Return
	DocumentDate, PostingDate time.Time
	Reference                 *string
	Reason                    *string       // why goods go back, for a Return
	Items                     []ReceiptItem // positive quantities with the PO items' units
}

// postReceipt posts a goods receipt (101) against released purchase order
// items of the legal entity, or a return to the supplier (122, T47b) of
// goods received on released or closed ones. The received quantity of an
// item stays between zero and the ordered quantity plus its over-delivery
// tolerance. post false records it without its WE entry (PostLoadedReceipt).
func (e *Endpoints) postReceipt(ctx context.Context, in ReceiptInput, post bool) (GoodsReceipt, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return GoodsReceipt{}, err
	}
	mt, err := movementType(in)
	if err != nil {
		return GoodsReceipt{}, err
	}
	le, err := e.legalEntity(ctx, in.LegalEntityID)
	if err != nil {
		return GoodsReceipt{}, err
	}
	if err := e.orgUnit(ctx, in.PlantID, le.ID, "plant", "plant_id"); err != nil {
		return GoodsReceipt{}, err
	}
	for i, it := range in.Items {
		if err := quantity(it.Quantity, fmt.Sprintf("items[%d].quantity", i)); err != nil {
			return GoodsReceipt{}, err
		}
	}
	current, err := lockItems(ctx, tx, in.Items)
	if err != nil {
		return GoodsReceipt{}, err
	}
	items, err := movedItems(in.Items, mt, le.ID, current)
	if err != nil {
		return GoodsReceipt{}, err
	}
	g, err := e.insertMovement(ctx, tx, p, GoodsReceipt{LegalEntityID: le.ID, PlantID: in.PlantID, MovementType: mt, DocumentDate: in.DocumentDate,
		PostingDate: in.PostingDate, Reference: in.Reference, Reason: in.Reason, Items: items}, current, post)
	if err != nil {
		return GoodsReceipt{}, err
	}
	return g, recordMovement(ctx, g)
}

// movementType is the type of the movement asked for: a receipt, or a
// return with its reason.
func movementType(in ReceiptInput) (string, error) {
	switch mt := cmp.Or(in.MovementType, Receipt); {
	case mt != Receipt && mt != Return:
		return "", invalid("procurement.invalid_movement_type", "movement_type", "101 to receive goods or 122 to return them")
	case mt == Return && (in.Reason == nil || strings.TrimSpace(*in.Reason) == ""):
		return "", invalid("procurement.reason_required", "reason", "why the goods go back to the supplier")
	case mt == Receipt && in.Reason != nil:
		return "", invalid("procurement.reason_not_allowed", "reason", "a reason only on a return (122)")
	default:
		return mt, nil
	}
}

// movedItems checks a receipt's or return's items against the order
// items they move and returns them as stored: a return's quantities
// negated, as it sends goods out.
func movedItems(in []ReceiptItem, mt string, le uuid.UUID, current []orderItem) ([]ReceiptItem, error) {
	moved := map[[2]any]*big.Rat{}
	out := make([]ReceiptItem, len(in))
	for i, it := range in {
		field := fmt.Sprintf("items[%d]", i)
		oi := current[i]
		switch {
		case !oi.found || oi.legalEntity != le:
			return nil, invalid("procurement.unknown_order_item", field, "an item of a purchase order of the legal entity")
		case mt == Receipt && oi.status != Released:
			return nil, problem.Conflict("procurement.order_not_released", "Purchase order "+oi.documentNumber+" is "+oi.status+"; goods are received only on released orders.")
		case mt == Return && oi.status != Released && oi.status != Closed:
			return nil, problem.Conflict("procurement.order_not_released", "Purchase order "+oi.documentNumber+" is "+oi.status+"; goods are returned only on released or closed orders.")
		case it.Unit != oi.unit:
			return nil, invalid("procurement.unit_mismatch", field+".quantity.unit", "the purchase order item's unit "+oi.unit)
		}
		if mt == Return {
			it.Quantity = negated(it.Quantity)
		}
		key := [2]any{it.PurchaseOrderID, it.POItemNo}
		if moved[key] == nil {
			moved[key] = money.ParseRat(oi.received)
		}
		moved[key].Add(moved[key], money.ParseRat(it.Quantity))
		if err := receivedWithin(oi, it.POItemNo, moved[key], mt); err != nil {
			return nil, err
		}
		out[i] = it
	}
	return out, nil
}

// receivedWithin refuses a movement of type mt that would leave an order
// item's received quantity below zero or above the ordered quantity plus
// its over-delivery tolerance (as check_receipt does in the database).
func receivedWithin(oi orderItem, poItem int32, received *big.Rat, mt string) error {
	if received.Sign() < 0 {
		code, what := "procurement.reversal_exceeds_receipts", "Reversing"
		if mt == Return {
			code, what = "procurement.return_exceeds_receipts", "Returning"
		}
		return problem.Conflict(code, fmt.Sprintf("%s would make the received quantity of item %d of order %s negative.", what, poItem, oi.documentNumber))
	}
	limit := new(big.Rat).Mul(money.ParseRat(oi.ordered), new(big.Rat).Add(big.NewRat(1, 1), new(big.Rat).Quo(money.ParseRat(oi.tolerance), big.NewRat(100, 1))))
	if received.Cmp(limit) > 0 {
		return problem.Conflict("procurement.over_delivery", fmt.Sprintf("Item %d of order %s would receive %s of %s ordered (tolerance %s%%).",
			poItem, oi.documentNumber, received.FloatString(6), oi.ordered, oi.tolerance))
	}
	return nil
}

// reversedItems are a movement's items negated, checked to leave each
// order item's received quantity within its bounds. Order status is not
// checked: a mistaken receipt or return is undone on a closed order too.
func reversedItems(orig []ReceiptItem, reversal string, current []orderItem) ([]ReceiptItem, error) {
	left := map[[2]any]*big.Rat{}
	items := make([]ReceiptItem, len(orig))
	for i, it := range orig {
		key := [2]any{it.PurchaseOrderID, it.POItemNo}
		if left[key] == nil {
			left[key] = money.ParseRat(current[i].received)
		}
		left[key].Sub(left[key], money.ParseRat(it.Quantity))
		if err := receivedWithin(current[i], it.POItemNo, left[key], reversal); err != nil {
			return nil, err
		}
		it.Quantity = new(big.Rat).Neg(money.ParseRat(it.Quantity)).FloatString(6)
		items[i] = it
	}
	return items, nil
}

// recordMovement audits a receipt or return and publishes GoodsReceived or
// GoodsReturned.
func recordMovement(ctx context.Context, g GoodsReceipt) error {
	action, name := "procurement.goods_receipt.post", "goods_received"
	payload := map[string]any{"goods_receipt_id": g.ID, "document_number": g.DocumentNumber, "legal_entity_id": g.LegalEntityID, "plant_id": g.PlantID,
		"movement_type": g.MovementType, "document_date": g.DocumentDate.Format(time.DateOnly), "posting_date": g.PostingDate.Format(time.DateOnly),
		"reference": g.Reference, "journal_entry_id": g.JournalEntryID, "journal_entry_year": g.JournalEntryYear, "items": movementItems(g)}
	if g.MovementType == Return {
		action, name, payload["reason"] = "procurement.goods_return.post", "goods_returned", *g.Reason
	}
	return audit.Record(ctx, action, "procurement.goods_movement", g.ID, map[string]any{"document_number": g.DocumentNumber, "items": len(g.Items)},
		event(name, "goods_movement", g.ID, payload))
}

// reversalDate is the date a reversal of movement orig posts on: the
// given one, else its company's date now in its time zone (product owner
// 2026-10-31, ADR-0051 as built): at 22:00 on 31 March in Toronto, 02:00
// UTC on 1 April, it is the 31st, so the reversal falls in March's period.
func (e *Endpoints) reversalDate(ctx context.Context, orig GoodsReceipt, postingDate *time.Time) (time.Time, error) {
	if postingDate != nil {
		return *postingDate, nil
	}
	day, _, err := e.todayOf(ctx, orig.LegalEntityID)
	return day, err
}

// reverseReceipt posts the movement that reverses a receipt (102) or a
// return (123), on the given date or its company's date (reversalDate). A movement is reversed once,
// and never so that the received quantity leaves its bounds.
func (e *Endpoints) reverseReceipt(ctx context.Context, id uuid.UUID, ifMatch func(GoodsReceipt) error, postingDate *time.Time) (GoodsReceipt, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return GoodsReceipt{}, err
	}
	// Movements are insert-only, so concurrent reversals of one receipt
	// serialize on a lock named after it instead of a row lock.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('procurement.goods_movement:' || $1::text, 0))`, id); err != nil {
		return GoodsReceipt{}, fmt.Errorf("procurement: reverse goods receipt %s: lock: %w", id, err)
	}
	orig, err := getReceipt(ctx, tx, id)
	if err != nil {
		return GoodsReceipt{}, err
	}
	if err := ifMatch(orig); err != nil {
		return GoodsReceipt{}, err
	}
	reversal, reversible := reversals[orig.MovementType]
	switch {
	case !reversible:
		return GoodsReceipt{}, problem.Conflict("procurement.cannot_reverse_reversal", "The movement is itself a reversal.")
	case orig.ReversedByID != nil:
		return GoodsReceipt{}, problem.Conflict("procurement.already_reversed", "The goods movement has already been reversed.")
	}
	current, err := lockItems(ctx, tx, orig.Items)
	if err != nil {
		return GoodsReceipt{}, err
	}
	items, err := reversedItems(orig.Items, reversal, current)
	if err != nil {
		return GoodsReceipt{}, err
	}
	date, err := e.reversalDate(ctx, orig, postingDate)
	if err != nil {
		return GoodsReceipt{}, err
	}
	rev, err := e.insertMovement(ctx, tx, p, GoodsReceipt{LegalEntityID: orig.LegalEntityID, PlantID: orig.PlantID, MovementType: reversal,
		DocumentDate: date, PostingDate: date, Reference: orig.Reference, ReversalOfID: &orig.ID, Items: items}, current, true)
	if err != nil {
		return GoodsReceipt{}, err
	}
	return rev, audit.Record(ctx, "procurement.goods_receipt.reverse", "procurement.goods_movement", orig.ID, map[string]any{"reversal_id": rev.ID},
		event("goods_receipt_reversed", "goods_movement", orig.ID, map[string]any{"goods_receipt_id": orig.ID, "reversal_id": rev.ID,
			"document_number": rev.DocumentNumber, "legal_entity_id": rev.LegalEntityID, "movement_type": reversal,
			"posting_date": rev.PostingDate.Format(time.DateOnly), "journal_entry_id": rev.JournalEntryID, "journal_entry_year": rev.JournalEntryYear,
			"items": movementItems(rev)}))
}
