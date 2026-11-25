package procurement

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Suppliers' delivery performance (S2l1; ADR-0082 As built S2l1): how the
// suppliers of a material delivered it to a plant over a window of goods
// receipt posting dates, measured in SQL from posted documents, never by
// a model: the replenishment agent reads it to suggest a supplier
// (ADR-0072 As built S2l1). A delivery is a goods receipt item (101) that
// no reversal (102) names, on an item of a released or closed order of
// the material whose plant, or else whose receipt's plant, is the plant;
// a return (122) is no delivery. Its observed lead time is the receipt's
// posting date less the order date, its planned one the item's delivery
// date less the order date, and it is on time when posted on or before
// that delivery date.
const (
	// MinReceipts are the receipts a supplier needs in the window for its
	// history to be measured; with fewer it is thin.
	MinReceipts = 3
	// The window's default and longest length, in days, both ends
	// counted: a year, and three.
	defaultWindowDays = 365
	maxWindowDays     = 1096
)

// The history a supplier's receipts in the window make.
const (
	HistoryMeasured = "measured"
	HistoryThin     = "thin"
)

// DeliveryQuery asks for the delivery performance of a material at a
// plant: of one supplier, or of every one when SupplierID is nil, over
// From to To (nil for the defaults).
type DeliveryQuery struct {
	PlantID, MaterialID uuid.UUID
	SupplierID          *uuid.UUID
	From, To            *time.Time
}

// Deliveries are a supplier's deliveries in the window, or every row's
// for the totals (SupplierID zero). OnTimePercent and PlannedLeadDays are
// nil when no receipt's item has a delivery date.
type Deliveries struct {
	SupplierID       uuid.UUID
	Receipts         int32
	Dated, OnTime    int32
	OnTimePercent    *int32
	PlannedLeadDays  *int32
	ObservedLeadDays *int32
	LastReceipt      *time.Time
}

// History is whether the supplier's receipts in the window measure its
// performance or are too few to say much.
func (d Deliveries) History() string {
	if d.Receipts >= MinReceipts {
		return HistoryMeasured
	}
	return HistoryThin
}

// DeliveryPerformance is the answer: the plant, the window, each
// supplier's deliveries in the order of their supplier numbers, and the
// totals.
type DeliveryPerformance struct {
	Plant      tenancy.Unit
	MaterialID uuid.UUID
	From, To   time.Time
	Suppliers  []Deliveries
	Names      map[uuid.UUID]partner.Named
	Totals     Deliveries
}

// deliveries measures the suppliers' receipts of material $2 at plant $1
// posted from $3 to $4, of supplier $5 alone when it is not null; ROLLUP's
// row without a supplier is every row's totals, there even when no
// receipt is. It is told by GROUPING, not by its place: with $5 given the
// planner takes supplier_id for a constant and may drop an ORDER BY on it. Averages of whole days are rounded half away from zero
// (numeric's round), never through a float.
const deliveries = `WITH r AS (
	SELECT o.supplier_id, m.posting_date, o.order_date, i.delivery_date, m.posting_date <= i.delivery_date AS on_time
	FROM procurement.purchase_order_item i
	JOIN procurement.purchase_order o ON o.tenant_id = i.tenant_id AND o.id = i.purchase_order_id
	JOIN procurement.goods_movement_item gi ON gi.tenant_id = i.tenant_id AND gi.purchase_order_id = i.purchase_order_id AND gi.po_item_no = i.item_no
	JOIN procurement.goods_movement m ON m.tenant_id = gi.tenant_id AND m.id = gi.goods_movement_id
	WHERE i.material_id = $2 AND coalesce(i.plant_id, m.plant_id) = $1 AND o.status IN ('released', 'closed') AND m.movement_type = '101'
		AND m.posting_date BETWEEN $3 AND $4 AND ($5::uuid IS NULL OR o.supplier_id = $5)
		AND NOT EXISTS (SELECT FROM procurement.goods_movement v WHERE v.tenant_id = m.tenant_id AND v.reversal_of_id = m.id))
SELECT GROUPING(supplier_id) = 1, supplier_id,
	count(*)::int, count(delivery_date)::int, (count(*) FILTER (WHERE on_time))::int,
	CASE WHEN count(delivery_date) > 0 THEN round(100 * (count(*) FILTER (WHERE on_time))::numeric / count(delivery_date))::int END,
	round(avg(delivery_date - order_date))::int, round(avg(posting_date - order_date))::int, max(posting_date)
FROM r
GROUP BY ROLLUP (supplier_id)
ORDER BY GROUPING(supplier_id) DESC, supplier_id`

// DeliveryPerformance measures the suppliers' deliveries of a material
// at a plant of a company the caller may act in (422
// request.reference_not_found otherwise) over a window of at most
// maxWindowDays (400 request.invalid_parameter otherwise), by default the
// defaultWindowDays ending on the plant's company's date.
func (e *Endpoints) DeliveryPerformance(ctx context.Context, q DeliveryQuery) (DeliveryPerformance, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return DeliveryPerformance{}, err
	}
	plant, err := e.performancePlant(ctx, q.PlantID)
	if err != nil {
		return DeliveryPerformance{}, err
	}
	from, to, err := e.window(ctx, plant.LegalEntityID, q.From, q.To)
	if err != nil {
		return DeliveryPerformance{}, err
	}
	out := DeliveryPerformance{Plant: plant, MaterialID: q.MaterialID, From: from, To: to}
	rows, _ := tx.Query(ctx, deliveries, q.PlantID, q.MaterialID, from, to, q.SupplierID)
	var ids []uuid.UUID
	var d Deliveries
	var total bool
	var supplier *uuid.UUID // null on the totals' row
	_, err = pgx.ForEachRow(rows, []any{&total, &supplier, &d.Receipts, &d.Dated, &d.OnTime, &d.OnTimePercent, &d.PlannedLeadDays,
		&d.ObservedLeadDays, &d.LastReceipt}, func() error {
		if total {
			out.Totals = d
		} else {
			d.SupplierID = *supplier
			out.Suppliers, ids = append(out.Suppliers, d), append(ids, d.SupplierID)
		}
		d = Deliveries{} // the next row's pointers are its own
		return nil
	})
	if err != nil {
		return DeliveryPerformance{}, fmt.Errorf("procurement: delivery performance of material %s at plant %s: %w", q.MaterialID, q.PlantID, err)
	}
	if out.Names, err = e.SupplierNames.Names(ctx, ids); err != nil {
		return DeliveryPerformance{}, err
	}
	slices.SortStableFunc(out.Suppliers, func(a, b Deliveries) int { // by id already, so ties keep it
		return cmp.Compare(out.Names[a.SupplierID].Number, out.Names[b.SupplierID].Number)
	})
	return out, nil
}

// performancePlant is the plant asked about, one of a company the caller
// may act in.
func (e *Endpoints) performancePlant(ctx context.Context, id uuid.UUID) (tenancy.Unit, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return tenancy.Unit{}, err
	}
	u, ok, err := e.LegalEntities.OrgUnit(ctx, id)
	if err != nil {
		return tenancy.Unit{}, err
	}
	if !ok || u.Kind != "plant" || !access.Allows(u.LegalEntityID) {
		return tenancy.Unit{}, problem.ReferenceNotFound("plant_id", "plant", "no plant with this id in a company you can act in")
	}
	return u, nil
}

// window is the posting dates asked for: to defaults to the company's
// date, from to the defaultWindowDays ending on to; a window that ends
// before it starts or is longer than maxWindowDays is refused.
func (e *Endpoints) window(ctx context.Context, le uuid.UUID, from, to *time.Time) (time.Time, time.Time, error) {
	end := to
	if end == nil {
		today, _, err := e.todayOf(ctx, le)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		end = &today
	}
	last := clock.Day(*end) // every date at midnight UTC, so a day is 24 hours
	first := last.AddDate(0, 0, 1-defaultWindowDays)
	if from != nil {
		first = clock.Day(*from)
	}
	switch days := int(last.Sub(first).Hours()/24) + 1; {
	case days < 1:
		return time.Time{}, time.Time{}, problem.BadRequest("request.invalid_parameter", "Parameter from must be on or before to.",
			problem.FieldError{Field: "from", Code: "invalid", Message: "on or before to"})
	case days > maxWindowDays:
		return time.Time{}, time.Time{}, problem.BadRequest("request.invalid_parameter", fmt.Sprintf("The window is at most %d days.", maxWindowDays),
			problem.FieldError{Field: "from", Code: "invalid", Message: fmt.Sprintf("at most %d days before to", maxWindowDays-1)})
	}
	return first, last, nil
}

// GetSupplierDeliveryPerformance implements GET
// /v1/supplier-delivery-performance.
func (e *Endpoints) GetSupplierDeliveryPerformance(ctx context.Context, params gen.GetSupplierDeliveryPerformanceParams) (
	gen.GetSupplierDeliveryPerformanceRes, error) {
	d, err := e.DeliveryPerformance(ctx, DeliveryQuery{PlantID: params.PlantID, MaterialID: params.MaterialID,
		SupplierID: apitypes.Ptr[uuid.UUID](params.SupplierID), From: apitypes.Ptr[time.Time](params.From), To: apitypes.Ptr[time.Time](params.To)})
	if err != nil {
		return nil, err
	}
	t := d.Totals
	out := &gen.SupplierDeliveryPerformance{
		PlantID: d.Plant.ID, PlantCode: d.Plant.Code, PlantName: d.Plant.Name, LegalEntityID: d.Plant.LegalEntityID,
		MaterialID: d.MaterialID, SupplierID: params.SupplierID, From: d.From, To: d.To, MinReceipts: MinReceipts,
		Suppliers: make([]gen.SupplierDelivery, len(d.Suppliers)),
		Totals: gen.SupplierDeliveryTotals{
			Receipts: t.Receipts, DatedReceipts: t.Dated, OnTimeReceipts: t.OnTime, OnTimePercent: apitypes.OptInt32(t.OnTimePercent),
			PlannedLeadDays: apitypes.OptInt32(t.PlannedLeadDays), ObservedLeadDays: apitypes.OptInt32(t.ObservedLeadDays),
		},
	}
	for i, s := range d.Suppliers {
		name := d.Names[s.SupplierID]
		out.Suppliers[i] = gen.SupplierDelivery{
			SupplierID: s.SupplierID, SupplierNumber: name.Number, SupplierName: name.Name,
			Receipts: s.Receipts, DatedReceipts: s.Dated, OnTimeReceipts: s.OnTime, OnTimePercent: apitypes.OptInt32(s.OnTimePercent),
			PlannedLeadDays: apitypes.OptInt32(s.PlannedLeadDays), ObservedLeadDays: *s.ObservedLeadDays, // a row has a receipt
			LastReceiptDate: *s.LastReceipt, History: s.History(),
		}
	}
	return out, nil
}
