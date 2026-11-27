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

	"github.com/mustie7iy/agentic-erp/internal/inventory"
	"github.com/mustie7iy/agentic-erp/internal/money"
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
	// BatchID and BatchNumber are the batch a stock item of a
	// batch-managed material moved, and SerialNumbers the serial numbers
	// of a serial-managed one's units, sorted (S2f1, S2g1): what its
	// material document's line moved, a reversal's its original's; nil on
	// another item.
	BatchID       *uuid.UUID `json:"batch_id"`
	BatchNumber   *string    `json:"batch_number"`
	SerialNumbers []string   `json:"serial_numbers"`
	// Batch is the batch a receipt's item names as the request gives it,
	// found or created by inventory (ReceiveForOrder).
	Batch *inventory.ReceiptBatch `json:"-"`
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
	// The material document that moved the stock of its stock items
	// (S1e), nil when it has none.
	MaterialDocumentID     *uuid.UUID
	MaterialDocumentNumber *string
	Items                  []ReceiptItem
	PostedAt               time.Time
	PostedBy               uuid.UUID
	// reverses is the material document a reversal reverses, its
	// original's (postStock).
	reverses *uuid.UUID
}

const receiptSelect = `SELECT m.id, m.document_number, m.legal_entity_id, m.plant_id, m.movement_type, m.document_date, m.posting_date, m.reference,
		m.reversal_of_id, (SELECT r.id FROM procurement.goods_movement r WHERE r.tenant_id = m.tenant_id AND r.reversal_of_id = m.id LIMIT 1) AS reversed_by,
		m.posted_at, m.posted_by, m.journal_entry_year, m.journal_entry_id, m.reason, m.material_document_id, m.material_document_number,
		(SELECT json_agg(json_build_object('item_no', i.item_no, 'purchase_order_id', i.purchase_order_id, 'po_item_no', i.po_item_no,
			'quantity', trim_scale(i.quantity)::text, 'unit', i.uom_code, 'batch_id', i.batch_id, 'batch_number', i.batch_number,
			'serial_numbers', i.serial_numbers) ORDER BY i.item_no)
		 FROM procurement.goods_movement_item i WHERE i.tenant_id = m.tenant_id AND i.goods_movement_id = m.id)
	FROM procurement.goods_movement m`

func scanReceipt(row pgx.Row) (GoodsReceipt, error) {
	var g GoodsReceipt
	if err := row.Scan(&g.ID, &g.DocumentNumber, &g.LegalEntityID, &g.PlantID, &g.MovementType, &g.DocumentDate, &g.PostingDate, &g.Reference,
		&g.ReversalOfID, &g.ReversedByID, &g.PostedAt, &g.PostedBy, &g.JournalEntryYear, &g.JournalEntryID, &g.Reason, &g.MaterialDocumentID,
		&g.MaterialDocumentNumber, &g.Items); err != nil {
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
	// A stock item's material, plant and storage location (S1e), nil on
	// another item.
	material, plant, storageLocation *uuid.UUID
	// awaitingIssue: the item is of an intercompany stock transport order
	// whose goods issue is not posted (S2i1), so nothing is received yet;
	// intercompany: of one, issued or not.
	awaitingIssue, intercompany bool
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
			i.cost_center_id, CASE WHEN i.storage_location_id IS NOT NULL THEN i.material_id END, i.plant_id, i.storage_location_id,
			coalesce(o.supplying_plant_id IS NOT NULL AND o.issue_document_id IS NULL, false), coalesce(o.supplying_plant_id IS NOT NULL, false)
		FROM unnest($1::uuid[], $2::int[]) WITH ORDINALITY AS x(o, n, ord)
		LEFT JOIN procurement.purchase_order o ON o.id = x.o
		LEFT JOIN procurement.purchase_order_item i ON i.tenant_id = o.tenant_id AND i.purchase_order_id = o.id AND i.item_no = x.n AND NOT i.is_removed
		ORDER BY x.ord`, orders, nos, uuid.Nil)
	var out []orderItem
	var oi orderItem
	_, err := pgx.ForEachRow(rows, []any{&oi.found, &oi.legalEntity, &oi.status, &oi.unit, &oi.ordered, &oi.tolerance, &oi.received, &oi.documentNumber,
		&oi.currency, &oi.valuation, &oi.valuationPrice, &oi.valuationPriceUnit, &oi.account, &oi.costCenter, &oi.material, &oi.plant, &oi.storageLocation,
		&oi.awaitingIssue, &oi.intercompany},
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
	var posted postedMovement
	if post {
		if posted, err = e.postStock(ctx, id, number, g, current); err != nil {
			return GoodsReceipt{}, err
		}
		tracked(g.Items, posted.lines)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO procurement.goods_movement (tenant_id, id, document_number, legal_entity_id, plant_id, movement_type,
			document_date, posting_date, reference, reversal_of_id, posted_by, journal_entry_year, journal_entry_id, reason, material_document_id,
			material_document_number)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`, p.TenantID, id, number, g.LegalEntityID, g.PlantID, g.MovementType,
		g.DocumentDate, g.PostingDate, g.Reference, g.ReversalOfID, p.ID, posted.year, posted.entry, g.Reason, posted.document, posted.documentNumber); err != nil {
		return GoodsReceipt{}, fmt.Errorf("procurement: insert goods movement %s: %w", number, err)
	}
	n := len(g.Items)
	nos, orders, poItems, quantities, units := make([]int32, n), make([]uuid.UUID, n), make([]int32, n), make([]string, n), make([]string, n)
	batches, batchNumbers, serials := make([]*uuid.UUID, n), make([]*string, n), make([]string, n)
	for i, it := range g.Items {
		nos[i], orders[i], poItems[i], quantities[i], units[i] = int32(i+1), it.PurchaseOrderID, it.POItemNo, it.Quantity, it.Unit
		// A serial number is capitals, digits, '-' and '_' (inventory's
		// serial_number_check), so a comma joins them unambiguously.
		batches[i], batchNumbers[i], serials[i] = it.BatchID, it.BatchNumber, strings.Join(it.SerialNumbers, ",")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO procurement.goods_movement_item (tenant_id, goods_movement_id, item_no, purchase_order_id, po_item_no, quantity, uom_code,
			batch_id, batch_number, serial_numbers)
		SELECT $1, $2, n, o, pi, q::numeric, u, b, bn, string_to_array(nullif(s, ''), ',')
		FROM unnest($3::int[], $4::uuid[], $5::int[], $6::text[], $7::text[], $8::uuid[], $9::text[], $10::text[]) AS x(n, o, pi, q, u, b, bn, s)`,
		p.TenantID, id, nos, orders, poItems, quantities, units, batches, batchNumbers, serials); err != nil {
		return GoodsReceipt{}, fmt.Errorf("procurement: insert items of goods movement %s: %w", number, err)
	}
	return getReceipt(ctx, tx, id)
}

func movementItems(g GoodsReceipt) []map[string]any {
	items := make([]map[string]any, len(g.Items))
	for i, it := range g.Items {
		items[i] = map[string]any{"item_no": it.No, "purchase_order_id": it.PurchaseOrderID, "po_item_no": it.POItemNo,
			"quantity": map[string]string{"value": it.Quantity, "unit": it.Unit}, "batch_id": it.BatchID, "batch_number": it.BatchNumber,
			"serial_numbers": it.SerialNumbers}
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
	items, left, err := movedItems(in.Items, mt, le.ID, current)
	if err != nil {
		return GoodsReceipt{}, err
	}
	if mt == Return {
		if err := e.returnKeepsInvoiced(ctx, items, current, left); err != nil {
			return GoodsReceipt{}, err
		}
	}
	g, err := e.insertMovement(ctx, tx, p, GoodsReceipt{LegalEntityID: le.ID, PlantID: in.PlantID, MovementType: mt, DocumentDate: in.DocumentDate,
		PostingDate: in.PostingDate, Reference: in.Reference, Reason: in.Reason, Items: items}, current, post)
	if err != nil {
		return GoodsReceipt{}, err
	}
	return g, e.recordMovement(ctx, g)
}

// movementType is the type of the movement asked for: a receipt, or a
// return with its reason.
func movementType(in ReceiptInput) (string, error) {
	switch mt := cmp.Or(in.MovementType, Receipt); {
	case mt != Receipt && mt != Return:
		return "", problem.Invalid("procurement.invalid_movement_type", "movement_type", "101 to receive goods or 122 to return them")
	case mt == Return && (in.Reason == nil || strings.TrimSpace(*in.Reason) == ""):
		return "", problem.Invalid("procurement.reason_required", "reason", "why the goods go back to the supplier")
	case mt == Receipt && in.Reason != nil:
		return "", problem.Invalid("procurement.reason_not_allowed", "reason", "a reason only on a return (122)")
	default:
		return mt, nil
	}
}

// movedItems checks a receipt's or return's items against the order
// items they move and returns them as stored, a return's quantities
// negated, as it sends goods out, with what each order item is then
// received. A return's batch is named by its number alone: the batch
// exists, and its dates are its own.
func movedItems(in []ReceiptItem, mt string, le uuid.UUID, current []orderItem) ([]ReceiptItem, map[[2]any]*big.Rat, error) {
	moved := map[[2]any]*big.Rat{}
	out := make([]ReceiptItem, len(in))
	for i, it := range in {
		field := fmt.Sprintf("items[%d]", i)
		oi := current[i]
		switch {
		case !oi.found || oi.legalEntity != le:
			return nil, nil, problem.Invalid("procurement.unknown_order_item", field, "an item of a purchase order of the legal entity")
		case mt == Receipt && oi.status != Released:
			return nil, nil, problem.Conflict("procurement.order_not_released", "Purchase order "+oi.documentNumber+" is "+oi.status+"; goods are received only on released orders.")
		case mt == Receipt && oi.awaitingIssue:
			return nil, nil, problem.Conflict("procurement.intercompany_not_issued", "Purchase order "+oi.documentNumber+
				" is an intercompany stock transport order whose goods issue is not posted; its goods are received once the supplying company issues them.")
		case mt == Return && oi.status != Released && oi.status != Closed:
			return nil, nil, problem.Conflict("procurement.order_not_released", "Purchase order "+oi.documentNumber+" is "+oi.status+"; goods are returned only on released or closed orders.")
		case it.Unit != oi.unit:
			return nil, nil, problem.Invalid("procurement.unit_mismatch", field+".quantity.unit", "the purchase order item's unit "+oi.unit)
		case oi.storageLocation == nil && (it.Batch != nil || len(it.SerialNumbers) > 0):
			return nil, nil, notStockItem(field, it, oi)
		case mt == Return && it.Batch != nil && (it.Batch.ManufactureDate != nil || it.Batch.ExpiryDate != nil):
			return nil, nil, problem.Invalid("procurement.return_batch_dates", field+".batch", "A return names the batch it gives back by its number alone; "+
				"the batch's dates are its own.")
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
			return nil, nil, err
		}
		out[i] = it
	}
	return out, moved, nil
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
// order item's received quantity within its bounds, and that quantity
// per order item. Order status is not checked: a mistaken receipt or
// return is undone on a closed order too.
func reversedItems(orig []ReceiptItem, reversal string, current []orderItem) ([]ReceiptItem, map[[2]any]*big.Rat, error) {
	left := map[[2]any]*big.Rat{}
	items := make([]ReceiptItem, len(orig))
	for i, it := range orig {
		key := [2]any{it.PurchaseOrderID, it.POItemNo}
		if left[key] == nil {
			left[key] = money.ParseRat(current[i].received)
		}
		left[key].Sub(left[key], money.ParseRat(it.Quantity))
		if err := receivedWithin(current[i], it.POItemNo, left[key], reversal); err != nil {
			return nil, nil, err
		}
		it.Quantity = negated(it.Quantity)
		items[i] = it
	}
	return items, left, nil
}

// keepsInvoiced refuses a receipt's reversal (102) that would leave an
// order item received less than posted documents invoiced of it (left is
// what each keeps received, reversedItems): the match holds an invoice to
// what was received, so the reversal is held to what was invoiced (03
// §3.9). A return (122) of an item that is no stock item is not held to
// it: goods sent back after their invoice are what the supplier's credit
// memo credits (ADR-0034); a stock item's is (returnKeepsInvoiced).
func (e *Endpoints) keepsInvoiced(ctx context.Context, orig GoodsReceipt, current []orderItem, left map[[2]any]*big.Rat) error {
	i, received, billed, err := e.belowInvoiced(ctx, orig.Items, left, func(int) bool { return true })
	if err != nil || i < 0 {
		return err
	}
	return problem.Conflict("procurement.reversal_below_invoiced", fmt.Sprintf(
		"Reversing goods receipt %s would leave item %d of order %s with %s received of %s invoiced; post the supplier's credit memo first, or return the goods (122).",
		orig.DocumentNumber, orig.Items[i].POItemNo, current[i].documentNumber, money.PlainQuantity(received), billed))
}

// returnKeepsInvoiced refuses a return to the supplier (122) of a stock
// item that would leave its order item received less than posted
// documents invoiced of it (left, movedItems; S1e2, decided by default,
// ADR-0082 As built S1e2): the supplier's credit memo for the goods comes
// first, so GR/IR never holds a return's debit waiting for it and the
// stock is not taken out at a value its invoice set while the invoice
// still stands. A return of another item is not held to it, as before.
func (e *Endpoints) returnKeepsInvoiced(ctx context.Context, items []ReceiptItem, current []orderItem, left map[[2]any]*big.Rat) error {
	i, received, billed, err := e.belowInvoiced(ctx, items, left, func(i int) bool { return current[i].storageLocation != nil })
	if err != nil || i < 0 {
		return err
	}
	return problem.Conflict("procurement.return_below_invoiced", fmt.Sprintf(
		"Returning item %d of order %s would leave it with %s received of %s invoiced; it is a stock item: post the supplier's credit memo first, then return the goods.",
		items[i].POItemNo, current[i].documentNumber, money.PlainQuantity(received), billed))
}

// belowInvoiced is the first of the items checked (checked(i)) whose
// order item left keeps received below what posted documents invoiced of
// it, with that received and invoiced quantity, or -1. The order items
// are locked (lockItems), and a document's posting and reversal lock them
// too, so what Invoices reads holds until this commits.
func (e *Endpoints) belowInvoiced(ctx context.Context, items []ReceiptItem, left map[[2]any]*big.Rat, checked func(int) bool) (int, *big.Rat, string, error) {
	invoiced := map[uuid.UUID]map[int32]string{}
	for i, it := range items {
		if !checked(i) {
			continue
		}
		quantities, read := invoiced[it.PurchaseOrderID]
		if !read {
			var err error
			if quantities, err = e.Invoices.Invoiced(ctx, it.PurchaseOrderID); err != nil {
				return -1, nil, "", err
			}
			invoiced[it.PurchaseOrderID] = quantities
		}
		received, billed := left[[2]any{it.PurchaseOrderID, it.POItemNo}], cmp.Or(quantities[it.POItemNo], "0")
		if money.ParseRat(billed).Cmp(received) > 0 {
			return i, received, billed, nil
		}
	}
	return -1, nil, "", nil
}

// recordMovement audits a receipt or return and publishes GoodsReceived or
// GoodsReturned.
func (e *Endpoints) recordMovement(ctx context.Context, g GoodsReceipt) error {
	action, name := "procurement.goods_receipt.post", "goods_received"
	payload := map[string]any{"goods_receipt_id": g.ID, "document_number": g.DocumentNumber, "legal_entity_id": g.LegalEntityID, "plant_id": g.PlantID,
		"movement_type": g.MovementType, "document_date": g.DocumentDate.Format(time.DateOnly), "posting_date": g.PostingDate.Format(time.DateOnly),
		"reference": g.Reference, "journal_entry_id": g.JournalEntryID, "journal_entry_year": g.JournalEntryYear, "items": movementItems(g)}
	if g.MovementType == Return {
		action, name, payload["reason"] = "procurement.goods_return.post", "goods_returned", *g.Reason
	}
	return e.record(ctx, action, "procurement.goods_movement", g.ID, map[string]any{"document_number": g.DocumentNumber, "items": len(g.Items)},
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
// and never so that the received quantity leaves its bounds, nor, for a
// receipt, falls below what was invoiced (keepsInvoiced).
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
	items, left, err := reversedItems(orig.Items, reversal, current)
	if err != nil {
		return GoodsReceipt{}, err
	}
	if reversal == Reversal {
		if err := e.keepsInvoiced(ctx, orig, current, left); err != nil {
			return GoodsReceipt{}, err
		}
	}
	date, err := e.reversalDate(ctx, orig, postingDate)
	if err != nil {
		return GoodsReceipt{}, err
	}
	rev, err := e.insertMovement(ctx, tx, p, GoodsReceipt{LegalEntityID: orig.LegalEntityID, PlantID: orig.PlantID, MovementType: reversal,
		DocumentDate: date, PostingDate: date, Reference: orig.Reference, ReversalOfID: &orig.ID, Items: items, reverses: orig.MaterialDocumentID}, current, true)
	if err != nil {
		return GoodsReceipt{}, err
	}
	return rev, e.record(ctx, "procurement.goods_receipt.reverse", "procurement.goods_movement", orig.ID, map[string]any{"reversal_id": rev.ID},
		event("goods_receipt_reversed", "goods_movement", orig.ID, map[string]any{"goods_receipt_id": orig.ID, "reversal_id": rev.ID,
			"document_number": rev.DocumentNumber, "legal_entity_id": rev.LegalEntityID, "movement_type": reversal,
			"posting_date": rev.PostingDate.Format(time.DateOnly), "journal_entry_id": rev.JournalEntryID, "journal_entry_year": rev.JournalEntryYear,
			"items": movementItems(rev)}))
}
