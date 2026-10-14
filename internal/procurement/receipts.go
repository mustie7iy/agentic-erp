package procurement

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
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
	Receipt  = "101"
	Reversal = "102"
)

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
	Items          []ReceiptItem
	PostedAt       time.Time
	PostedBy       uuid.UUID
}

const receiptSelect = `SELECT m.id, m.document_number, m.legal_entity_id, m.plant_id, m.movement_type, m.document_date, m.posting_date, m.reference,
		m.reversal_of_id, (SELECT r.id FROM procurement.goods_movement r WHERE r.tenant_id = m.tenant_id AND r.reversal_of_id = m.id LIMIT 1) AS reversed_by,
		m.posted_at, m.posted_by,
		(SELECT json_agg(json_build_object('item_no', i.item_no, 'purchase_order_id', i.purchase_order_id, 'po_item_no', i.po_item_no,
			'quantity', trim_scale(i.quantity)::text, 'unit', i.uom_code) ORDER BY i.item_no)
		 FROM procurement.goods_movement_item i WHERE i.tenant_id = m.tenant_id AND i.goods_movement_id = m.id)
	FROM procurement.goods_movement m`

func scanReceipt(row pgx.Row) (GoodsReceipt, error) {
	var g GoodsReceipt
	if err := row.Scan(&g.ID, &g.DocumentNumber, &g.LegalEntityID, &g.PlantID, &g.MovementType, &g.DocumentDate, &g.PostingDate, &g.Reference,
		&g.ReversalOfID, &g.ReversedByID, &g.PostedAt, &g.PostedBy, &g.Items); err != nil {
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
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !access.Allows(g.LegalEntityID)) {
		return GoodsReceipt{}, notFound("goods receipt")
	}
	if err != nil {
		return GoodsReceipt{}, fmt.Errorf("procurement: get goods receipt %s: %w", id, err)
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
	found              bool
	legalEntity        uuid.UUID
	status, unit       string
	ordered, tolerance string
	received           string
	documentNumber     string
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
				AND g.purchase_order_id = i.purchase_order_id AND g.po_item_no = i.item_no), 0))::text, coalesce(o.document_number, '')
		FROM unnest($1::uuid[], $2::int[]) WITH ORDINALITY AS x(o, n, ord)
		LEFT JOIN procurement.purchase_order o ON o.id = x.o
		LEFT JOIN procurement.purchase_order_item i ON i.tenant_id = o.tenant_id AND i.purchase_order_id = o.id AND i.item_no = x.n AND NOT i.is_removed
		ORDER BY x.ord`, orders, nos, uuid.Nil)
	var out []orderItem
	var oi orderItem
	_, err := pgx.ForEachRow(rows, []any{&oi.found, &oi.legalEntity, &oi.status, &oi.unit, &oi.ordered, &oi.tolerance, &oi.received, &oi.documentNumber},
		func() error { out = append(out, oi); return nil })
	if err != nil {
		return nil, fmt.Errorf("procurement: read %d purchase order items: %w", len(items), err)
	}
	return out, nil
}

// insertMovement writes a goods movement with its items and returns it.
func (e *Endpoints) insertMovement(ctx context.Context, tx pgx.Tx, p reqctx.Principal, g GoodsReceipt) (GoodsReceipt, error) {
	number, err := e.number(ctx, g.LegalEntityID, "goods_receipt", g.PostingDate)
	if err != nil {
		return GoodsReceipt{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO procurement.goods_movement (tenant_id, id, document_number, legal_entity_id, plant_id, movement_type,
			document_date, posting_date, reference, reversal_of_id, posted_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`, p.TenantID, id, number, g.LegalEntityID, g.PlantID, g.MovementType,
		g.DocumentDate, g.PostingDate, g.Reference, g.ReversalOfID, p.ID); err != nil {
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
	DocumentDate, PostingDate time.Time
	Reference                 *string
	Items                     []ReceiptItem // quantities with the PO items' units
}

// postReceipt posts a goods receipt (101) against released purchase order
// items of the legal entity. The received quantity of an item may exceed
// the ordered quantity by its over-delivery tolerance, no more.
func (e *Endpoints) postReceipt(ctx context.Context, in ReceiptInput) (GoodsReceipt, error) {
	tx, p, err := reqctx.Request(ctx)
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
		if _, err := quantity(it.Quantity, fmt.Sprintf("items[%d].quantity", i)); err != nil {
			return GoodsReceipt{}, err
		}
	}
	current, err := lockItems(ctx, tx, in.Items)
	if err != nil {
		return GoodsReceipt{}, err
	}
	added := map[[2]any]*big.Rat{}
	for i, it := range in.Items {
		field := fmt.Sprintf("items[%d]", i)
		oi := current[i]
		switch {
		case !oi.found || oi.legalEntity != le.ID:
			return GoodsReceipt{}, invalid("procurement.unknown_order_item", field, "an item of a purchase order of the legal entity")
		case oi.status != Released:
			return GoodsReceipt{}, problem.Conflict("procurement.order_not_released", "Purchase order "+oi.documentNumber+" is "+oi.status+"; goods are received only on released orders.")
		case it.Unit != oi.unit:
			return GoodsReceipt{}, invalid("procurement.unit_mismatch", field+".quantity.unit", "the purchase order item's unit "+oi.unit)
		}
		key := [2]any{it.PurchaseOrderID, it.POItemNo}
		if added[key] == nil {
			added[key] = rat(oi.received)
		}
		added[key].Add(added[key], rat(it.Quantity))
		limit := new(big.Rat).Mul(rat(oi.ordered), new(big.Rat).Add(big.NewRat(1, 1), new(big.Rat).Quo(rat(oi.tolerance), big.NewRat(100, 1))))
		if added[key].Cmp(limit) > 0 {
			return GoodsReceipt{}, problem.Conflict("procurement.over_delivery", fmt.Sprintf("Item %d of order %s would receive %s of %s ordered (tolerance %s%%).",
				it.POItemNo, oi.documentNumber, added[key].FloatString(6), oi.ordered, oi.tolerance))
		}
	}
	g, err := e.insertMovement(ctx, tx, p, GoodsReceipt{LegalEntityID: le.ID, PlantID: in.PlantID, MovementType: Receipt, DocumentDate: in.DocumentDate,
		PostingDate: in.PostingDate, Reference: in.Reference, Items: in.Items})
	if err != nil {
		return GoodsReceipt{}, err
	}
	return g, audit.Record(ctx, "procurement.goods_receipt.post", "procurement.goods_movement", g.ID, map[string]any{"document_number": g.DocumentNumber,
		"items": len(g.Items)}, event("goods_received", "goods_movement", g.ID, map[string]any{"goods_receipt_id": g.ID,
		"document_number": g.DocumentNumber, "legal_entity_id": g.LegalEntityID, "plant_id": g.PlantID, "movement_type": Receipt,
		"document_date": g.DocumentDate.Format(time.DateOnly), "posting_date": g.PostingDate.Format(time.DateOnly), "reference": g.Reference,
		"items": movementItems(g)}))
}

// reverseReceipt posts a 102 that reverses a receipt, on the given date or
// today. A receipt is reversed once, and never below the quantity received.
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
	switch {
	case orig.MovementType != Receipt:
		return GoodsReceipt{}, problem.Conflict("procurement.cannot_reverse_reversal", "The movement is itself a reversal.")
	case orig.ReversedByID != nil:
		return GoodsReceipt{}, problem.Conflict("procurement.already_reversed", "The goods receipt has already been reversed.")
	}
	current, err := lockItems(ctx, tx, orig.Items)
	if err != nil {
		return GoodsReceipt{}, err
	}
	left := map[[2]any]*big.Rat{}
	items := make([]ReceiptItem, len(orig.Items))
	for i, it := range orig.Items {
		key := [2]any{it.PurchaseOrderID, it.POItemNo}
		if left[key] == nil {
			left[key] = rat(current[i].received)
		}
		left[key].Sub(left[key], rat(it.Quantity))
		if left[key].Sign() < 0 {
			return GoodsReceipt{}, problem.Conflict("procurement.reversal_exceeds_receipts",
				fmt.Sprintf("Reversing would make the received quantity of item %d of order %s negative.", it.POItemNo, current[i].documentNumber))
		}
		it.Quantity = new(big.Rat).Neg(rat(it.Quantity)).FloatString(6)
		items[i] = it
	}
	date, err := database.Today(ctx, tx)
	if err != nil {
		return GoodsReceipt{}, err
	}
	if postingDate != nil {
		date = *postingDate
	}
	rev, err := e.insertMovement(ctx, tx, p, GoodsReceipt{LegalEntityID: orig.LegalEntityID, PlantID: orig.PlantID, MovementType: Reversal,
		DocumentDate: date, PostingDate: date, Reference: orig.Reference, ReversalOfID: &orig.ID, Items: items})
	if err != nil {
		return GoodsReceipt{}, err
	}
	return rev, audit.Record(ctx, "procurement.goods_receipt.reverse", "procurement.goods_movement", orig.ID, map[string]any{"reversal_id": rev.ID},
		event("goods_receipt_reversed", "goods_movement", orig.ID, map[string]any{"goods_receipt_id": orig.ID, "reversal_id": rev.ID,
			"document_number": rev.DocumentNumber, "legal_entity_id": rev.LegalEntityID, "movement_type": Reversal,
			"posting_date": rev.PostingDate.Format(time.DateOnly), "items": movementItems(rev)}))
}
