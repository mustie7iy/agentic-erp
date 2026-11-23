package procurement

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Procurement's published API for material requirements planning (S2k;
// ADR-0084 §2): what a plant has on order, the requisitions a planning
// run proposed as they stand now, and the run's proposing and withdrawing
// of its own drafts. The planning context records which requisitions it
// proposed; procurement keeps no mark of it, so a proposal is an ordinary
// draft of the run's requester.

// OnOrder is what a plant has on order of a material in one unit: the
// open quantities of its purchase orders' stock items there.
type OnOrder struct {
	MaterialID uuid.UUID
	Unit       string
	Quantity   string
}

// OpenReceipts is what the purchase orders not closed or cancelled have
// open of the materials ($2) for receipt at the plant ($1), by material
// and unit in that order: each stock item's ordered quantity less what
// its goods movements received (receipts less reversals), an item
// received in full counting nothing. Draft and pending orders count, as
// released ones do (SAP's MRP counts a purchase order item whatever its
// release, from memory). It reads the orders as they are, locking
// nothing. Company access is not applied: the caller reads a plant of a
// company it may act in.
func (Orders) OpenReceipts(ctx context.Context, plant uuid.UUID, materials []uuid.UUID) ([]OnOrder, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT material_id, uom_code, trim_scale(sum(open))::text FROM (
			SELECT i.material_id, i.uom_code, i.quantity - coalesce((SELECT sum(g.quantity) FROM procurement.goods_movement_item g
				WHERE g.tenant_id = i.tenant_id AND g.purchase_order_id = i.purchase_order_id AND g.po_item_no = i.item_no), 0) AS open
			FROM procurement.purchase_order_item i JOIN procurement.purchase_order o ON o.tenant_id = i.tenant_id AND o.id = i.purchase_order_id
			WHERE i.plant_id = $1 AND i.material_id = ANY($2) AND NOT i.is_removed AND o.status NOT IN ('closed', 'cancelled')) items
		WHERE open > 0 GROUP BY material_id, uom_code ORDER BY material_id, uom_code`, plant, materials)
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[OnOrder])
	if err != nil {
		return nil, fmt.Errorf("procurement: open receipts of %d materials at plant %s: %w", len(materials), plant, err)
	}
	return out, nil
}

// TransportReceipts is what stock transport orders within the company
// in transit to the plant ($1) bring of the materials ($2), by material
// and unit in that order (S2h; S2n, ADR-0084 As built S2n): their stock
// left the supplying plant and is no plant's until received. An order not
// yet issued moves stock between the company's plants only, and an
// intercompany order the plant receives is a purchase order
// (OpenReceipts), so neither is here. It reads the orders as they are,
// locking nothing. Company access is not applied: the caller reads a plant
// of a company it may act in.
func (Orders) TransportReceipts(ctx context.Context, plant uuid.UUID, materials []uuid.UUID) ([]OnOrder, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT i.material_id, i.uom_code, trim_scale(sum(i.quantity))::text
		FROM procurement.stock_transport_order_item i JOIN procurement.stock_transport_order o ON o.tenant_id = i.tenant_id AND o.id = i.order_id
		WHERE o.receiving_plant_id = $1 AND o.status = 'in_transit' AND i.material_id = ANY($2)
		GROUP BY i.material_id, i.uom_code ORDER BY i.material_id, i.uom_code`, plant, materials)
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[OnOrder])
	if err != nil {
		return nil, fmt.Errorf("procurement: stock in transit of %d materials to plant %s: %w", len(materials), plant, err)
	}
	return out, nil
}

// DueOnOrder is an OnOrder due on a day.
type DueOnOrder struct {
	MaterialID uuid.UUID
	Unit       string
	Due        time.Time
	Quantity   string
}

// IntercompanyDue is what intercompany stock transport orders (S2i) the
// plant ($1) supplies, not closed or cancelled and not yet issued, order
// of the materials ($2), by the day each item is due (its delivery date,
// else the order's date), by material, unit and day in that order: the
// plant's demand from the companies it supplies, which a planning run and
// an S&OP review count as they count sales orders (S2n; ADR-0084 As built
// S2n and batch 91). Removed items order nothing. It reads the orders as
// they are, locking nothing. Company access is not applied: the caller
// reads a plant of a company it may act in.
func (Orders) IntercompanyDue(ctx context.Context, plant uuid.UUID, materials []uuid.UUID) ([]DueOnOrder, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT i.material_id, i.uom_code, coalesce(i.delivery_date, o.order_date) AS due, trim_scale(sum(i.quantity))::text
		FROM procurement.purchase_order_item i JOIN procurement.purchase_order o ON o.tenant_id = i.tenant_id AND o.id = i.purchase_order_id
		WHERE o.supplying_plant_id = $1 AND o.issue_document_id IS NULL AND o.status NOT IN ('closed', 'cancelled') AND NOT i.is_removed
			AND i.material_id = ANY($2)
		GROUP BY i.material_id, i.uom_code, due ORDER BY i.material_id, i.uom_code, due`, plant, materials)
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[DueOnOrder])
	if err != nil {
		return nil, fmt.Errorf("procurement: intercompany orders of %d materials from plant %s: %w", len(materials), plant, err)
	}
	return out, nil
}

// RequisitionState is a requisition as a planning run reads it: its
// status and version, and its live items of materials.
type RequisitionState struct {
	Status  string
	Version int32
	Items   []OnOrder
}

// Requisitions reads the requisitions with the ids as they stand,
// keyed by id; an id that is no requisition has no entry. Items without
// a material are left out. Company access is not applied: the caller
// reads the requisitions it proposed.
func (Orders) Requisitions(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]RequisitionState, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT r.id, r.status, r.version,
			coalesce((SELECT json_agg(json_build_object('MaterialID', i.material_id, 'Unit', i.uom_code, 'Quantity', trim_scale(i.quantity)::text)
				ORDER BY i.item_no) FROM procurement.requisition_item i
				WHERE i.tenant_id = r.tenant_id AND i.requisition_id = r.id AND NOT i.is_removed AND i.material_id IS NOT NULL), '[]')
		FROM procurement.requisition r WHERE r.id = ANY($1) ORDER BY r.id`, ids)
	out := map[uuid.UUID]RequisitionState{}
	var id uuid.UUID
	var s RequisitionState
	if _, err := pgx.ForEachRow(rows, []any{&id, &s.Status, &s.Version, &s.Items}, func() error {
		out[id] = s
		return nil
	}); err != nil {
		return nil, fmt.Errorf("procurement: %d requisitions: %w", len(ids), err)
	}
	return out, nil
}

// ProposeRequisition creates a draft requisition of the caller as
// createRequisition does, with its checks, number and audit record: a
// planning run's proposal, raised as the person who started the run.
func (e *Endpoints) ProposeRequisition(ctx context.Context, in NewRequisition) (Requisition, error) {
	return e.createRequisition(ctx, in)
}

// WithdrawProposal cancels a requisition a planning run proposed while it
// is still the run's own: a draft at the version the run created it in,
// which no person changed or submitted since. It takes the requisition's
// row lock, so a person's change either commits first, moving the
// version, and the requisition is left alone (false), or waits and finds
// it cancelled. The cancellation is the requisition's as cancelRequisition
// records it (audited, RequisitionCancelled), by the caller, who need not
// be its requester: the run replacing it.
func (e *Endpoints) WithdrawProposal(ctx context.Context, id uuid.UUID, version int32) (bool, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return false, err
	}
	r, err := getRequisition(ctx, tx, id, true)
	if err != nil {
		return false, err
	}
	if r.Status != Draft || r.Version != version {
		return false, nil
	}
	_, err = e.setStatus(ctx, tx, p, r, Cancelled, nil, "requisition_cancelled", map[string]any{"requester_id": r.RequesterID, "workflow_request_id": nil})
	return err == nil, err
}
