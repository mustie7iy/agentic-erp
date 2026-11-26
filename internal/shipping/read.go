package shipping

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// deliverySelect reads a delivery with its items by item number.
const deliverySelect = `SELECT d.id, d.legal_entity_id, d.fiscal_year, d.number, d.sales_order_id, d.sales_order_number, d.customer_id,
		d.customer_number, d.plant_id, d.storage_location_id, d.status, d.goods_issue_date, d.material_document_id, d.material_document_number,
		coalesce(d.currency, ''), coalesce((SELECT c.exponent FROM ref.currency c WHERE c.code = d.currency), 0),
		(SELECT json_agg(json_build_object('item_no', i.item_no, 'order_item_no', i.order_item_no, 'material_id', i.material_id,
			'description', i.description, 'quantity', trim_scale(i.quantity)::text, 'unit', i.uom_code,
			'picked_quantity', trim_scale(i.picked_quantity)::text, 'cost_amount', i.cost_amount) ORDER BY i.item_no)
		 FROM shipping.outbound_delivery_item i WHERE i.tenant_id = d.tenant_id AND i.outbound_delivery_id = d.id),
		d.version, d.created_at, d.created_by, d.updated_at, d.updated_by,
		d.shipment_number, d.carrier_code, d.freight_amount, d.freight_currency,
		coalesce((SELECT c.exponent FROM ref.currency c WHERE c.code = d.freight_currency), 0), d.shipment_recorded_at,
		d.trade_compliance_status, d.trade_compliance_reason, d.trade_compliance_screened_at, d.trade_compliance_released_by,
		d.trade_compliance_released_by_name, d.trade_compliance_released_at,
		d.goods_issue_reversal_date, d.reversal_material_document_id, d.reversal_material_document_number
	FROM shipping.outbound_delivery d`

func scanDelivery(row pgx.Row) (Delivery, error) {
	var d Delivery
	var number, carrier, currency *string
	var freight *money.Minor
	var exp int
	var recorded *time.Time
	var tc TradeCompliance
	var tradeStatus *string
	var reversed *time.Time
	var reversal *uuid.UUID
	var reversalNumber *string
	err := row.Scan(&d.ID, &d.LegalEntityID, &d.FiscalYear, &d.Number, &d.OrderID, &d.OrderNumber, &d.CustomerID, &d.CustomerNumber, &d.PlantID,
		&d.StorageLocationID, &d.Status, &d.GoodsIssueDate, &d.MaterialDocumentID, &d.MaterialDocumentNumber, &d.Currency, &d.Exponent, &d.Items,
		&d.Version, &d.CreatedAt,
		&d.CreatedBy, &d.UpdatedAt, &d.UpdatedBy, &number, &carrier, &freight, &currency, &exp, &recorded,
		&tradeStatus, &tc.ReasonCode, &tc.ScreenedAt, &tc.ReleasedBy, &tc.ReleasedByName, &tc.ReleasedAt,
		&reversed, &reversal, &reversalNumber)
	if err == nil && number != nil { // outbound_delivery_shipment: all five or none
		d.Shipment = &Shipment{Number: *number, Carrier: *carrier, Freight: *freight, Currency: *currency, Exponent: exp, RecordedAt: *recorded}
	}
	if err == nil && tradeStatus != nil { // outbound_delivery_trade_compliance: none of it without a status
		tc.Status = *tradeStatus
		d.TradeCompliance = &tc
	}
	if err == nil && reversed != nil { // outbound_delivery_goods_issue_reversal: all three or none
		d.Reversal = &Reversal{Date: *reversed, MaterialDocumentID: *reversal, MaterialDocumentNumber: *reversalNumber}
	}
	return d, errs.Wrapf(err, "scan outbound delivery")
}

// load reads a delivery whatever its company, forUpdate under its row
// lock (ADR-0008), taken by a statement of its own so the read that
// follows sees the items as the transaction it waited for left them.
func load(ctx context.Context, tx pgx.Tx, id uuid.UUID, forUpdate bool) (Delivery, error) {
	if forUpdate {
		if _, err := tx.Exec(ctx, `SELECT FROM shipping.outbound_delivery WHERE id = $1 FOR UPDATE`, id); err != nil {
			return Delivery{}, fmt.Errorf("shipping: lock outbound delivery %s: %w", id, err)
		}
	}
	d, err := scanDelivery(tx.QueryRow(ctx, deliverySelect+` WHERE d.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Delivery{}, problem.NotFoundOf("outbound delivery")
	}
	if err != nil {
		return Delivery{}, fmt.Errorf("shipping: read outbound delivery %s: %w", id, err)
	}
	return d, nil
}

// accessible reads a delivery of a company the caller may act in,
// forUpdate under its row lock; another's is not found, so ids cannot be
// probed (04 §4.9).
func accessible(ctx context.Context, tx pgx.Tx, id uuid.UUID, forUpdate bool) (Delivery, error) {
	a, err := authz.Companies(ctx)
	if err != nil {
		return Delivery{}, fmt.Errorf("shipping: company access: %w", err)
	}
	d, err := load(ctx, tx, id, forUpdate)
	if err == nil && !a.Allows(d.LegalEntityID) {
		return Delivery{}, problem.NotFoundOf("outbound delivery")
	}
	return d, err
}

// PickingStatus is the delivery's picking status (SAP KOSTK, from
// memory): not_picked while nothing is picked, picked when every item is
// picked in full, partly_picked between.
func (d Delivery) PickingStatus() string {
	var some, all = false, true
	for _, it := range d.Items {
		picked := money.ParseRat(it.Picked)
		some = some || picked.Sign() > 0
		all = all && picked.Cmp(money.ParseRat(it.Quantity)) == 0
	}
	switch {
	case !some:
		return NotPicked
	case all:
		return Picked
	}
	return PartlyPicked
}

// refs is what deliveries name by id, as their consumer reads them
// (REVIEW.md, API-consumer classes): their companies by code, their
// plants and storage locations by code and name and their materials by
// number and their customers by name.
type refs struct {
	companies map[uuid.UUID]tenancy.Entity
	units     map[uuid.UUID]tenancy.Unit
	materials map[uuid.UUID]materials.Material
	customers map[uuid.UUID]partner.Named
}

// refs reads what the deliveries name, each once.
func (e *Endpoints) refs(ctx context.Context, ds []Delivery) (refs, error) {
	r := refs{companies: map[uuid.UUID]tenancy.Entity{}}
	var units, ms, customers []uuid.UUID
	for _, d := range ds {
		units, customers = append(units, d.PlantID, d.StorageLocationID), append(customers, d.CustomerID)
		for _, it := range d.Items {
			ms = append(ms, it.MaterialID)
		}
		if _, ok := r.companies[d.LegalEntityID]; ok {
			continue
		}
		le, _, err := e.LegalEntities.Entity(ctx, d.LegalEntityID) // a stored delivery's company exists
		if err != nil {
			return refs{}, fmt.Errorf("shipping: company %s: %w", d.LegalEntityID, err)
		}
		r.companies[d.LegalEntityID] = le
	}
	var err error
	if r.units, err = e.LegalEntities.OrgUnits(ctx, units); err != nil {
		return refs{}, fmt.Errorf("shipping: %d plants and storage locations: %w", len(units), err)
	}
	if r.materials, err = e.Materials.Materials(ctx, ms); err != nil {
		return refs{}, fmt.Errorf("shipping: %d materials: %w", len(ms), err)
	}
	if r.customers, err = e.Partners.Names(ctx, customers); err != nil {
		return refs{}, fmt.Errorf("shipping: %d customers: %w", len(customers), err)
	}
	return r, nil
}

// record adds the audit record of a delivery's change and publishes
// OutboundDeliveryChanged in the same transaction (ADR-0007, ADR-0011),
// both naming its company, order, customer, plant, storage location and
// materials by id and code or number, never the customer by name
// (product owner, 2026-11-05). detail adds to the audit record's.
func (e *Endpoints) record(ctx context.Context, action string, d Delivery, detail map[string]any) error {
	r, err := e.refs(ctx, []Delivery{d})
	if err != nil {
		return err
	}
	items := make([]map[string]any, len(d.Items))
	for i, it := range d.Items {
		items[i] = map[string]any{"item_no": it.ItemNo, "order_item_no": it.OrderItemNo, "material_id": it.MaterialID,
			"material_number": r.materials[it.MaterialID].Number, "quantity": map[string]string{"value": it.Quantity, "unit": it.Unit},
			"picked_quantity": map[string]string{"value": it.Picked, "unit": it.Unit}, "cost": nil}
		if it.Cost != nil {
			items[i]["cost"] = money.EventAmount(*it.Cost, d.Currency, d.Exponent)
		}
	}
	var issued *string
	if d.GoodsIssueDate != nil {
		day := d.GoodsIssueDate.Format(time.DateOnly)
		issued = &day
	}
	payload := map[string]any{"outbound_delivery_id": d.ID, "number": d.Number, "legal_entity_id": d.LegalEntityID,
		"legal_entity_code": r.companies[d.LegalEntityID].Code, "sales_order_id": d.OrderID, "sales_order_number": d.OrderNumber,
		"customer_id": d.CustomerID, "customer_number": d.CustomerNumber, "plant_id": d.PlantID, "plant_code": r.units[d.PlantID].Code,
		"storage_location_id": d.StorageLocationID, "storage_location_code": r.units[d.StorageLocationID].Code, "status": d.Status,
		"picking_status": d.PickingStatus(), "goods_issue_date": issued, "material_document_id": d.MaterialDocumentID,
		"material_document_number": d.MaterialDocumentNumber, "items": items, "shipment": shipmentEvent(d.Shipment),
		"trade_compliance": tradeEvent(d.TradeCompliance), "goods_issue_reversal": reversalEvent(d.Reversal), "version": d.Version}
	audited := map[string]any{"number": d.Number, "sales_order_id": d.OrderID, "sales_order_number": d.OrderNumber, "status": d.Status,
		"picking_status": d.PickingStatus(), "items": items}
	for k, v := range detail {
		audited[k] = v
	}
	if err := audit.Record(ctx, action, "shipping.outbound_delivery", d.ID, audited,
		outbox.Event{Context: "shipping", Name: "outbound_delivery_changed", AggregateType: "shipping.outbound_delivery", AggregateID: d.ID,
			Payload: payload}); err != nil {
		return fmt.Errorf("shipping: record %s of outbound delivery %s: %w", action, d.Number, err)
	}
	return nil
}
