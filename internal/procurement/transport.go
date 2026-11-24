package procurement

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/inventory"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Stock transport orders within a company (S2h; ADR-0082 As built S2h):
// a planner orders materials from a storage location of one plant to one
// of another plant of the same company (SAP's purchasing document type
// UB, from memory). Its goods issue (351) puts the stock in transit to the
// receiving plant and its goods receipt (101) takes it out into the
// receiving storage location, both through inventory's published API in
// the request's transaction. The order moves whole: one goods issue of
// every item, then one goods receipt; partial issues and receipts, their
// reversals and a delivery between the plants are Later. An open order is
// cancelled instead of issued (ADR-0082 As built S2h, cancellation;
// review:batch92), so a planning run no longer counts it.

// TransportRange is the document type stock transport orders draw their
// numbers from (configuration number_ranges, ADR-0004).
const TransportRange = "stock_transport_order"

// Statuses of a stock transport order.
const (
	TransportOpen      = "open"
	TransportInTransit = "in_transit"
	TransportReceived  = "received"
	TransportCancelled = "cancelled"
)

func init() {
	problem.RegisterConstraint("stock_transport_order_item_material_once", func() *problem.Problem {
		return problem.Unprocessable("procurement.stock_transport_material_repeated", "A stock transport order orders a material once.")
	})
}

// Transports moves the stock of stock transport orders (the inventory
// context's published API): it checks an order's items as their goods
// issue will, and posts their goods issue and receipt.
type Transports interface {
	TransportQuantities(ctx context.Context, in []inventory.TransportItem) (uuid.UUID, []string, error)
	IssueForTransport(ctx context.Context, reference string, in []inventory.TransportItem) (inventory.Document, error)
	ReceiveForTransport(ctx context.Context, reference string, in []inventory.TransportItem) (inventory.Document, error)
	// IssueForIntercompany posts an intercompany order's goods issue
	// (643, S2i1).
	IssueForIntercompany(ctx context.Context, reference string, in []inventory.DeliveryItem) (inventory.Document, error)
}

// OrgUnits reads tenancy's org units by id: the plants and storage
// locations an order names.
type OrgUnits interface {
	OrgUnits(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]tenancy.Unit, error)
}

// TransportItemInput is an item of a stock transport order as a request
// gives it.
type TransportItemInput struct {
	MaterialID                                             uuid.UUID
	Quantity, Unit                                         string
	SupplyingStorageLocationID, ReceivingStorageLocationID uuid.UUID
}

// TransportInput is a stock transport order as a request creates it.
type TransportInput struct {
	SupplyingPlantID, ReceivingPlantID uuid.UUID
	Items                              []TransportItemInput
}

// TransportItem is an item of a stock transport order: a quantity of a
// material in the unit ordered and in its base unit.
type TransportItem struct {
	ItemNo                                                 int16
	MaterialID                                             uuid.UUID
	Quantity, Unit, BaseQuantity                           string
	SupplyingStorageLocationID, ReceivingStorageLocationID uuid.UUID
}

// TransportOrder is a row of procurement.stock_transport_order with its
// items by item number. Once issued it names its goods issue's material
// document and day, once received its goods receipt's.
type TransportOrder struct {
	ID, LegalEntityID                  uuid.UUID
	FiscalYear                         int16
	Number                             string
	SupplyingPlantID, ReceivingPlantID uuid.UUID
	OrderDate                          time.Time
	Status                             string
	IssueDocumentID                    *uuid.UUID
	IssueDocumentNumber                *string
	IssueDate                          *time.Time
	ReceiptDocumentID                  *uuid.UUID
	ReceiptDocumentNumber              *string
	ReceiptDate                        *time.Time
	Items                              []TransportItem
	Version                            int32
	CreatedAt                          time.Time
	CreatedBy                          uuid.UUID
	UpdatedAt                          time.Time
	UpdatedBy                          uuid.UUID
}

// movement is the order's items as inventory moves them: their base
// quantities, as converted when the order was created, once it has them.
func (o TransportOrder) movement() []inventory.TransportItem {
	out := make([]inventory.TransportItem, len(o.Items))
	for i, it := range o.Items {
		out[i] = inventory.TransportItem{MaterialID: it.MaterialID, SupplyingPlantID: o.SupplyingPlantID, SupplyingStorageLocation: it.SupplyingStorageLocationID,
			ReceivingPlantID: o.ReceivingPlantID, ReceivingStorageLocation: it.ReceivingStorageLocationID, Quantity: it.Quantity, Unit: it.Unit,
			BaseQuantity: it.BaseQuantity}
	}
	return out
}

// CreateTransport creates a stock transport order: each item checked by
// inventory as its goods issue will be (TransportQuantities: both plants
// active plants of one company the caller may act in, the material active,
// stocked, extended to both and kept at both storage locations, its
// quantity exact in its base unit), a material once (422
// procurement.stock_transport_material_repeated), dated the company's
// date today and numbered from its stock_transport_order range of that
// date's fiscal year, open, and audited.
func (e *Endpoints) CreateTransport(ctx context.Context, in TransportInput) (TransportOrder, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return TransportOrder{}, err
	}
	o := TransportOrder{ID: uuid.Must(uuid.NewV7()), SupplyingPlantID: in.SupplyingPlantID, ReceivingPlantID: in.ReceivingPlantID,
		Items: make([]TransportItem, len(in.Items))}
	seen := map[uuid.UUID]bool{}
	for i, it := range in.Items {
		if seen[it.MaterialID] {
			return TransportOrder{}, problem.Invalid("procurement.stock_transport_material_repeated", fmt.Sprintf("items[%d].material_id", i),
				"A stock transport order orders a material once.")
		}
		seen[it.MaterialID] = true
		o.Items[i] = TransportItem{ItemNo: int16(i + 1), MaterialID: it.MaterialID, Quantity: it.Quantity, Unit: it.Unit, // at most 100 items (the contract's)
			SupplyingStorageLocationID: it.SupplyingStorageLocationID, ReceivingStorageLocationID: it.ReceivingStorageLocationID}
	}
	company, base, err := e.Transports.TransportQuantities(ctx, o.movement())
	if err != nil {
		return TransportOrder{}, err
	}
	for i := range o.Items {
		o.Items[i].BaseQuantity = base[i]
	}
	if err := e.numberTransport(ctx, &o, company); err != nil {
		return TransportOrder{}, err
	}
	if err := insertTransport(ctx, tx, p, o); err != nil {
		return TransportOrder{}, err
	}
	created, err := loadTransport(ctx, tx, o.ID, false)
	if err != nil {
		return TransportOrder{}, err
	}
	return created, e.recordTransport(ctx, "procurement.stock_transport_order.create", created)
}

// numberTransport dates the order the company's date today and numbers it
// from its stock_transport_order range of that date's fiscal year.
func (e *Endpoints) numberTransport(ctx context.Context, o *TransportOrder, company uuid.UUID) error {
	day, _, err := e.todayOf(ctx, company)
	if err != nil {
		return err
	}
	year, number, err := e.numberOf(ctx, company, TransportRange, day)
	if err != nil {
		return fmt.Errorf("procurement: number a stock transport order of %s in company %s: %w", day.Format(time.DateOnly), company, err)
	}
	o.LegalEntityID, o.OrderDate, o.FiscalYear, o.Number = company, day, year, number
	return nil
}

// insertTransport writes an order with its items.
func insertTransport(ctx context.Context, tx pgx.Tx, p reqctx.Principal, o TransportOrder) error {
	if _, err := tx.Exec(ctx, `INSERT INTO procurement.stock_transport_order (tenant_id, id, legal_entity_id, fiscal_year, number, supplying_plant_id,
			receiving_plant_id, order_date, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)`,
		p.TenantID, o.ID, o.LegalEntityID, o.FiscalYear, o.Number, o.SupplyingPlantID, o.ReceivingPlantID, o.OrderDate, p.ID); err != nil {
		return fmt.Errorf("procurement: insert stock transport order %s: %w", o.Number, err)
	}
	n := len(o.Items)
	ms, from, to := make([]uuid.UUID, n), make([]uuid.UUID, n), make([]uuid.UUID, n)
	qty, units, base := make([]string, n), make([]string, n), make([]string, n)
	for i, it := range o.Items {
		ms[i], from[i], to[i], qty[i], units[i], base[i] = it.MaterialID, it.SupplyingStorageLocationID, it.ReceivingStorageLocationID, it.Quantity, it.Unit,
			it.BaseQuantity
	}
	if _, err := tx.Exec(ctx, `INSERT INTO procurement.stock_transport_order_item (tenant_id, order_id, item_no, material_id, quantity, uom_code,
			base_quantity, supplying_storage_location_id, receiving_storage_location_id)
		SELECT $1, $2, x.n, x.m, x.q::numeric, x.u, x.b::numeric, x.f, x.t
		FROM unnest($3::uuid[], $4::text[], $5::text[], $6::text[], $7::uuid[], $8::uuid[]) WITH ORDINALITY AS x (m, q, u, b, f, t, n) ORDER BY x.n`,
		p.TenantID, o.ID, ms, qty, units, base, from, to); err != nil {
		return fmt.Errorf("procurement: items of stock transport order %s: %w", o.Number, err)
	}
	return nil
}

// PostTransportIssue posts an open order's goods issue (351) through
// inventory (IssueForTransport), the order read under its row lock at the
// version the caller read before inventory locks the valuations and stock
// (ADR-0008; nothing locks them the other way round), and the order is in
// transit, naming the material document and its day.
func (e *Endpoints) PostTransportIssue(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (TransportOrder, error) {
	return e.moveTransport(ctx, id, ifMatch, TransportOpen, notOpen, func(o TransportOrder) (inventory.Document, error) {
		return e.Transports.IssueForTransport(ctx, o.Number, o.movement())
	})
}

// PostTransportReceipt posts the goods receipt (101) of an order in
// transit through inventory (ReceiveForTransport), under the order's row
// lock at the version the caller read, and the order is received.
func (e *Endpoints) PostTransportReceipt(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (TransportOrder, error) {
	return e.moveTransport(ctx, id, ifMatch, TransportInTransit, notInTransit, func(o TransportOrder) (inventory.Document, error) {
		return e.Transports.ReceiveForTransport(ctx, o.Number, o.movement())
	})
}

// transportSteps are what each step of an order writes: the status it
// leaves, the columns naming its material document and day, and its audit
// action.
var transportSteps = map[string]struct{ next, document, number, day, action string }{
	TransportOpen: {next: TransportInTransit, document: "issue_document_id", number: "issue_document_number", day: "issue_date",
		action: "procurement.stock_transport_order.goods_issue"},
	TransportInTransit: {next: TransportReceived, document: "receipt_document_id", number: "receipt_document_number", day: "receipt_date",
		action: "procurement.stock_transport_order.goods_receipt"},
}

// notOpen refuses the goods issue or the cancellation of an order no
// longer open: one cancelled, or one whose goods issue is posted.
func notOpen(o TransportOrder) error {
	if o.Status == TransportCancelled {
		return problem.Conflict("procurement.stock_transport_cancelled", "Stock transport order "+o.Number+" is cancelled; it is neither issued nor cancelled again.")
	}
	return problem.Conflict("procurement.stock_transport_issued", "Stock transport order "+o.Number+" is "+o.Status+"; its goods issue is posted already.")
}

// notInTransit refuses the goods receipt of an order not in transit.
func notInTransit(o TransportOrder) error {
	return problem.Conflict("procurement.stock_transport_not_in_transit", "Stock transport order "+o.Number+" is "+o.Status+
		"; only an order in transit is received.")
}

// moveTransport takes an order in status from one step forward: it locks
// the order, checks its version and status, has inventory post the
// step's material document, and writes the step, guarded by the status
// and version it read; another status is refused by refuse.
func (e *Endpoints) moveTransport(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, from string, refuse func(TransportOrder) error,
	post func(TransportOrder) (inventory.Document, error)) (TransportOrder, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return TransportOrder{}, err
	}
	o, err := loadTransport(ctx, tx, id, true)
	if err != nil {
		return TransportOrder{}, err
	}
	if err := ifMatch(o.Version); err != nil {
		return TransportOrder{}, err
	}
	if o.Status != from {
		return TransportOrder{}, refuse(o)
	}
	step := transportSteps[from]
	d, err := post(o)
	if err != nil {
		return TransportOrder{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE procurement.stock_transport_order SET status = $3, `+pgx.Identifier{step.document}.Sanitize()+` = $4, `+
		pgx.Identifier{step.number}.Sanitize()+` = $5, `+pgx.Identifier{step.day}.Sanitize()+` = $6, version = version + 1, updated_by = $7
		WHERE id = $1 AND status = $2 AND version = $8`, o.ID, from, step.next, d.ID, d.Number, d.PostingDate, p.ID, o.Version); err != nil {
		return TransportOrder{}, fmt.Errorf("procurement: %s of stock transport order %s: %w", step.next, o.Number, err)
	}
	after, err := loadTransport(ctx, tx, o.ID, false)
	if err != nil {
		return TransportOrder{}, err
	}
	return after, e.recordTransport(ctx, step.action, after)
}

// CancelTransport cancels an open order (SAP's deletion of a stock
// transport order's items before their goods issue, from memory): read
// under its row lock at the version the caller read, refused once its
// goods issue is posted or once cancelled (notOpen), and written guarded
// by the status and version it read. It moves no stock. Audited, and
// published as StockTransportOrderCancelled, which names the company,
// plants and materials beside their ids.
func (e *Endpoints) CancelTransport(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (TransportOrder, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return TransportOrder{}, err
	}
	o, err := loadTransport(ctx, tx, id, true)
	if err != nil {
		return TransportOrder{}, err
	}
	if err := ifMatch(o.Version); err != nil {
		return TransportOrder{}, err
	}
	if o.Status != TransportOpen {
		return TransportOrder{}, notOpen(o)
	}
	if _, err := tx.Exec(ctx, `UPDATE procurement.stock_transport_order SET status = $3, version = version + 1, updated_by = $4
		WHERE id = $1 AND status = $2 AND version = $5`, o.ID, TransportOpen, TransportCancelled, p.ID, o.Version); err != nil {
		return TransportOrder{}, fmt.Errorf("procurement: cancel stock transport order %s: %w", o.Number, err)
	}
	after, err := loadTransport(ctx, tx, o.ID, false)
	if err != nil {
		return TransportOrder{}, err
	}
	r, err := e.transportRefsOf(ctx, []TransportOrder{after})
	if err != nil {
		return TransportOrder{}, err
	}
	return after, e.recordTransport(ctx, "procurement.stock_transport_order.cancel", after, cancelledEvent(after, o.Status, r))
}

// cancelledEvent is StockTransportOrderCancelled of an order cancelled
// from the status it had: the order, its company, plants and items'
// materials by id and by code or number.
func cancelledEvent(o TransportOrder, from string, r transportRefs) outbox.Event {
	items := make([]map[string]any, len(o.Items))
	for i, it := range o.Items {
		m := r.materials[it.MaterialID]
		items[i] = map[string]any{"item_no": it.ItemNo, "material_id": it.MaterialID, "material_number": m.Number,
			"quantity": map[string]any{"value": it.Quantity, "unit": it.Unit}, "base_quantity": map[string]any{"value": it.BaseQuantity, "unit": m.BaseUnit}}
	}
	return event("stock_transport_order_cancelled", "stock_transport_order", o.ID, map[string]any{"stock_transport_order_id": o.ID, "number": o.Number,
		"legal_entity_id": o.LegalEntityID, "legal_entity_code": r.companies[o.LegalEntityID].Code, "supplying_plant_id": o.SupplyingPlantID,
		"supplying_plant_code": r.units[o.SupplyingPlantID].Code, "receiving_plant_id": o.ReceivingPlantID, "receiving_plant_code": r.units[o.ReceivingPlantID].Code,
		"order_date": o.OrderDate.Format(time.DateOnly), "items": items, "previous_status": from, "status": o.Status})
}

// transportSelect reads stock transport orders with their items in item
// order.
const transportSelect = `SELECT o.id, o.legal_entity_id, o.fiscal_year, o.number, o.supplying_plant_id, o.receiving_plant_id, o.order_date, o.status,
		o.issue_document_id, o.issue_document_number, o.issue_date, o.receipt_document_id, o.receipt_document_number, o.receipt_date, o.version,
		o.created_at, o.created_by, o.updated_at, o.updated_by,
		(SELECT json_agg(json_build_object('item_no', i.item_no, 'material_id', i.material_id, 'quantity', trim_scale(i.quantity)::text,
				'unit', i.uom_code, 'base_quantity', trim_scale(i.base_quantity)::text, 'supplying', i.supplying_storage_location_id,
				'receiving', i.receiving_storage_location_id) ORDER BY i.item_no)
			FROM procurement.stock_transport_order_item i WHERE i.tenant_id = o.tenant_id AND i.order_id = o.id) AS items
	FROM procurement.stock_transport_order o`

// transportItemJSON is an item as transportSelect reads it.
type transportItemJSON struct {
	ItemNo     int16     `json:"item_no"`
	MaterialID uuid.UUID `json:"material_id"`
	Quantity   string    `json:"quantity"`
	Unit       string    `json:"unit"`
	Base       string    `json:"base_quantity"`
	Supplying  uuid.UUID `json:"supplying"`
	Receiving  uuid.UUID `json:"receiving"`
}

func scanTransport(row pgx.Row) (TransportOrder, error) {
	var o TransportOrder
	var items []transportItemJSON
	err := row.Scan(&o.ID, &o.LegalEntityID, &o.FiscalYear, &o.Number, &o.SupplyingPlantID, &o.ReceivingPlantID, &o.OrderDate, &o.Status,
		&o.IssueDocumentID, &o.IssueDocumentNumber, &o.IssueDate, &o.ReceiptDocumentID, &o.ReceiptDocumentNumber, &o.ReceiptDate, &o.Version,
		&o.CreatedAt, &o.CreatedBy, &o.UpdatedAt, &o.UpdatedBy, &items)
	for _, it := range items {
		o.Items = append(o.Items, TransportItem{ItemNo: it.ItemNo, MaterialID: it.MaterialID, Quantity: it.Quantity, Unit: it.Unit, BaseQuantity: it.Base,
			SupplyingStorageLocationID: it.Supplying, ReceivingStorageLocationID: it.Receiving})
	}
	return o, errs.Wrapf(err, "scan stock transport order")
}

// loadTransport reads an order of a company the caller may act in, under
// its row lock when forUpdate; another company's is not found.
func loadTransport(ctx context.Context, tx pgx.Tx, id uuid.UUID, forUpdate bool) (TransportOrder, error) {
	a, err := authz.Companies(ctx)
	if err != nil {
		return TransportOrder{}, fmt.Errorf("procurement: company access: %w", err)
	}
	q := transportSelect + ` WHERE o.id = $1`
	if forUpdate {
		q += ` FOR UPDATE OF o`
	}
	o, err := scanTransport(tx.QueryRow(ctx, q, id))
	switch {
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && !a.Allows(o.LegalEntityID)):
		return TransportOrder{}, problem.NotFoundOf("stock transport order")
	case err != nil:
		return TransportOrder{}, fmt.Errorf("procurement: read stock transport order %s: %w", id, err)
	}
	return o, nil
}

// recordTransport writes an order's audit record: its number, plants,
// status and items' materials and quantities, and the material documents
// of its steps, with the events given. Its goods issue and receipt
// publish no event of the order: its stock moves by the material
// documents, which publish MaterialDocumentPosted; its cancellation,
// which moves nothing, publishes StockTransportOrderCancelled.
func (e *Endpoints) recordTransport(ctx context.Context, action string, o TransportOrder, events ...outbox.Event) error {
	items := make([]map[string]any, len(o.Items))
	for i, it := range o.Items {
		items[i] = map[string]any{"item_no": it.ItemNo, "material_id": it.MaterialID, "quantity": it.Quantity, "unit": it.Unit, "base_quantity": it.BaseQuantity,
			"supplying_storage_location_id": it.SupplyingStorageLocationID, "receiving_storage_location_id": it.ReceivingStorageLocationID}
	}
	return errs.Wrapf(e.record(ctx, action, "procurement.stock_transport_order", o.ID, map[string]any{"number": o.Number,
		"legal_entity_id": o.LegalEntityID, "supplying_plant_id": o.SupplyingPlantID, "receiving_plant_id": o.ReceivingPlantID, "status": o.Status,
		"items": items, "issue_document_number": o.IssueDocumentNumber, "receipt_document_number": o.ReceiptDocumentNumber}, events...),
		"procurement: record %s of stock transport order %s", action, o.Number)
}
