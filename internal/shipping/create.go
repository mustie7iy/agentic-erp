package shipping

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/sales"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Input is an outbound delivery as a request creates it: the sales
// order, where it ships from and, optionally, its items, each a quantity
// of an order item (ItemNo is the order's item number); nil: what is open
// of every order item of a material kept in stock at the location.
type Input struct {
	OrderID           uuid.UUID
	PlantID           uuid.UUID
	StorageLocationID uuid.UUID
	Items             []sales.ItemQuantity
}

// origin is what a delivery is made from: its order, its company, the
// plant and storage location and the materials of the order's items, and
// whether a trade compliance service screens the plant (S2s).
type origin struct {
	order     sales.Order
	le        tenancy.Entity
	plant     tenancy.Unit
	location  tenancy.Unit
	materials map[uuid.UUID]materials.Material
	screened  bool
}

// Create creates an outbound delivery (ADR-0083 §3): the order one of a
// company the caller may act in, the plant an active one of the order's
// company and the storage location an active one under it. Sales takes the
// quantities under the order's row lock (Ship: confirmed, not blocked for
// credit, at most what is open, or what is open of every eligible item of
// the order as first read), and each item named is then checked, on the
// order as Ship read it, to be an order item of an active material kept
// in stock there (eligible), the delivery is numbered from its
// company's outbound_delivery range of the fiscal year of the company's
// date today, and written created with nothing picked, its trade
// compliance pending when an active gts connector screens the plant (S2s:
// its goods issue waits for the screening); its audit record and
// OutboundDeliveryChanged are written in the request's transaction.
func (e *Endpoints) Create(ctx context.Context, in Input) (Delivery, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Delivery{}, err
	}
	src, err := e.origin(ctx, in)
	if err != nil {
		return Delivery{}, err
	}
	ship := sales.ShipInput{OrderID: in.OrderID, DeliveryID: uuid.Must(uuid.NewV7()), PlantID: in.PlantID, Items: in.Items}
	if in.Items == nil {
		ship.Open = src.eligibleItems()
	}
	day, err := config.DocumentDay(ctx, e.Settings, src.le.Code, clock.Now(e.Now))
	if err != nil {
		return Delivery{}, fmt.Errorf("shipping: today in company %s: %w", src.le.Code, err)
	}
	year, err := e.Calendar.FiscalYear(ctx, src.le.ID, day.Date)
	if err != nil {
		return Delivery{}, fmt.Errorf("shipping: fiscal year of %s in company %s: %w", day.Date.Format(time.DateOnly), src.le.Code, err)
	}
	if ship.DeliveryNumber, err = e.Numbers.Next(ctx, src.le.ID, NumberRange, year); err != nil {
		return Delivery{}, fmt.Errorf("shipping: number an outbound delivery in company %s: %w", src.le.Code, err)
	}
	shipped, err := e.Shipments.Ship(ctx, ship)
	if err != nil {
		return Delivery{}, err
	}
	if in.Items != nil {
		// Judged on the order as Ship read it under its lock: an item added
		// since the read above is checked as any other.
		if src.order, src.materials, err = e.orderMaterials(ctx, shipped.Order); err != nil {
			return Delivery{}, err
		}
		if err := src.eligibleNamed(in.Items); err != nil {
			return Delivery{}, err
		}
	}
	if src.screened, err = e.Screening.Screens(ctx, src.le.Code, src.plant.Code); err != nil {
		return Delivery{}, fmt.Errorf("shipping: trade compliance of plant %s: %w", src.plant.Code, err)
	}
	d, err := insert(ctx, tx, p, src, ship, year, shipped)
	if err != nil {
		return Delivery{}, err
	}
	return d, e.record(ctx, "shipping.outbound_delivery.create", d, nil)
}

// origin reads and checks what the request names: the order, of a company
// the caller may act in (422 request.reference_not_found naming
// sales_order_id otherwise), the plant, the order's own when it names one
// (422 shipping.plant_mismatch; S2j), and storage location, and the
// materials of the order's items.
func (e *Endpoints) origin(ctx context.Context, in Input) (origin, error) {
	o, ok, err := e.Orders.Order(ctx, in.OrderID)
	if err != nil {
		return origin{}, fmt.Errorf("shipping: sales order %s: %w", in.OrderID, err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return origin{}, fmt.Errorf("shipping: company access: %w", err)
	}
	if !ok || !access.Allows(o.LegalEntityID) {
		return origin{}, problem.ReferenceNotFound("sales_order_id", "sales order", "no sales order with this id that you can access")
	}
	src := origin{order: o}
	if src.le, _, err = e.LegalEntities.Entity(ctx, o.LegalEntityID); err != nil { // a stored order's company exists
		return origin{}, fmt.Errorf("shipping: company of sales order %s: %w", o.DocumentNumber, err)
	}
	units, err := e.LegalEntities.OrgUnits(ctx, []uuid.UUID{in.PlantID, in.StorageLocationID})
	if err != nil {
		return origin{}, fmt.Errorf("shipping: plant and storage location: %w", err)
	}
	if src.plant, src.location, err = shipFrom(units, in, src.le); err != nil {
		return origin{}, err
	}
	if o.PlantID != nil && *o.PlantID != src.plant.ID {
		// The order's items were checked for availability at its plant (S2j).
		return origin{}, problem.Invalid("shipping.plant_mismatch", "plant_id", "Sales order "+o.DocumentNumber+
			" delivers from its own plant, whose stock confirmed its items; a delivery of it ships from that plant.")
	}
	if src.order, src.materials, err = e.orderMaterials(ctx, o); err != nil {
		return origin{}, err
	}
	return src, nil
}

// orderMaterials is the order with the materials its standing items name.
func (e *Endpoints) orderMaterials(ctx context.Context, o sales.Order) (sales.Order, map[uuid.UUID]materials.Material, error) {
	var ids []uuid.UUID
	for _, it := range o.Standing() {
		if it.MaterialID != nil {
			ids = append(ids, *it.MaterialID)
		}
	}
	ms, err := e.Materials.Materials(ctx, ids)
	if err != nil {
		return sales.Order{}, nil, fmt.Errorf("shipping: materials of sales order %s: %w", o.DocumentNumber, err)
	}
	return o, ms, nil
}

// shipFrom checks where a delivery ships from: an active plant of the
// order's company (422 shipping.plant_invalid) and an active storage
// location under it (422 shipping.storage_location_invalid).
func shipFrom(units map[uuid.UUID]tenancy.Unit, in Input, le tenancy.Entity) (tenancy.Unit, tenancy.Unit, error) {
	plant, ok := units[in.PlantID]
	if !ok || plant.Kind != kindPlant || plant.Status != tenancy.Active || plant.LegalEntityID != le.ID {
		return tenancy.Unit{}, tenancy.Unit{}, problem.Invalid("shipping.plant_invalid", "plant_id", "No active plant with this id in company "+le.Code+
			", the sales order's.")
	}
	loc, ok := units[in.StorageLocationID]
	if !ok || loc.Kind != kindStorageLocation || loc.Status != tenancy.Active || loc.ParentID == nil || *loc.ParentID != plant.ID {
		return tenancy.Unit{}, tenancy.Unit{}, problem.Invalid("shipping.storage_location_invalid", "storage_location_id",
			"No active storage location with this id under plant "+plant.Code+".")
	}
	return plant, loc, nil
}

// eligible refuses an order item not delivered from the location, naming
// field: it names an active material kept in stock (422
// shipping.item_not_stocked: an item without one, or of a material kept
// in no stock, is confirmed as delivered by hand, T78c;
// shipping.material_not_usable; one managed in batches or serial-managed
// is delivered as any, its goods issue naming its batches and serial
// numbers, S2f2, S2g2), kept at the storage location of the plant
// (shipping.material_not_at_location).
func (s origin) eligible(it sales.OrderItem, field string) error {
	lead := fmt.Sprintf("Item %d of sales order %s ", it.ItemNo, s.order.DocumentNumber)
	if it.MaterialID == nil {
		return problem.Invalid("shipping.item_not_stocked", field, lead+"names no material; it is confirmed as delivered (confirmSalesDelivery).")
	}
	m := s.materials[*it.MaterialID]
	switch {
	case !m.Stocked():
		return problem.Invalid("shipping.item_not_stocked", field, lead+"is material "+m.Number+" of type "+m.Type+
			", kept in no stock; it is confirmed as delivered (confirmSalesDelivery).")
	case m.Status != materials.Active:
		return problem.Invalid("shipping.material_not_usable", field, lead+"is material "+m.Number+", which is "+m.Status+
			"; only an active material is delivered.")
	case !m.KeptAt(s.plant.ID, s.location.ID):
		return problem.Invalid("shipping.material_not_at_location", field, lead+"is material "+m.Number+", which is not kept at storage location "+
			s.location.Code+" of plant "+s.plant.Code+".")
	}
	return nil
}

// eligibleItems are the numbers of the order's standing items delivered
// from the location.
func (s origin) eligibleItems() []int32 {
	var out []int32
	for _, it := range s.order.Standing() {
		if s.eligible(it, "") == nil {
			out = append(out, it.ItemNo)
		}
	}
	return out
}

// eligibleNamed refuses a named order item not delivered from the
// location, naming items[i].order_item_no; an item the order lacks or
// that is removed is left to sales' refusal.
func (s origin) eligibleNamed(items []sales.ItemQuantity) error {
	byNo := map[int32]sales.OrderItem{}
	for _, it := range s.order.Standing() {
		byNo[it.ItemNo] = it
	}
	for i, in := range items {
		if it, ok := byNo[in.ItemNo]; ok {
			if err := s.eligible(it, fmt.Sprintf("items[%d].order_item_no", i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// insert writes the delivery and its items, numbered from 1 in the order
// sales took them, each with its order item's material and description,
// and reads it back.
func insert(ctx context.Context, tx pgx.Tx, p reqctx.Principal, src origin, ship sales.ShipInput, year int16, shipped sales.Shipped) (Delivery, error) {
	o := shipped.Order
	if _, err := tx.Exec(ctx, `INSERT INTO shipping.outbound_delivery (tenant_id, id, legal_entity_id, fiscal_year, number, sales_order_id,
			sales_order_number, customer_id, customer_number, plant_id, storage_location_id, created_by, updated_by, trade_compliance_status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12, CASE WHEN $13 THEN 'pending' END)`,
		p.TenantID, ship.DeliveryID, o.LegalEntityID, year, ship.DeliveryNumber, o.ID, o.DocumentNumber, o.CustomerID, shipped.CustomerNumber,
		src.plant.ID, src.location.ID, p.ID, src.screened); err != nil {
		return Delivery{}, fmt.Errorf("shipping: insert outbound delivery %s: %w", ship.DeliveryNumber, err)
	}
	for i, q := range shipped.Items {
		it := o.Items[q.ItemNo-1] // items are numbered from 1 in order (sales' insertItems)
		if _, err := tx.Exec(ctx, `INSERT INTO shipping.outbound_delivery_item (tenant_id, outbound_delivery_id, item_no, order_item_no, material_id,
				description, quantity, uom_code) VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8)`,
			p.TenantID, ship.DeliveryID, i+1, q.ItemNo, *it.MaterialID, it.Description, q.Quantity, it.Unit); err != nil {
			return Delivery{}, fmt.Errorf("shipping: insert item %d of outbound delivery %s: %w", i+1, ship.DeliveryNumber, err)
		}
	}
	return load(ctx, tx, ship.DeliveryID, false)
}
