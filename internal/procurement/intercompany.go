package procurement

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/inventory"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Intercompany stock transport orders (S2i1; ADR-0082 As built S2i):
// between plants of two companies of the tenant the order is the
// receiving company's purchase order naming the plant that supplies it
// (SAP's cross-company stock transport order, a purchasing document with
// a supplying plant, from memory). Its supplier is the business partner
// that is the supplying company (its group company, ADR-0059 §4), its
// prices are the transfer price, and it is released as any order. The
// supplying company posts its goods issue (643) once, for the whole order,
// out of the supplying plant's stock against cost of goods sold; the
// receiving company receives it as any stock order (101 at the order
// price against GR/IR), up to what was issued. In between the goods are
// in neither company's stock. The intercompany billing and the receiving
// company's supplier invoice from it are S2i2 and S2i3.

// intercompanyHeader is what an intercompany order's items are checked
// against: the supplying plant and its company.
type intercompanyHeader struct {
	plant   uuid.UUID
	company uuid.UUID
}

// supplyingPlant checks an order's supplying plant as it is created: an
// active plant of another company of the tenant (422
// procurement.supplying_plant_invalid, procurement.supplying_plant_own_company,
// naming supplying_plant_id), whose company is the order's supplier (422
// procurement.intercompany_supplier naming supplier_id). nil for an order
// supplied by none.
func (e *Endpoints) supplyingPlant(ctx context.Context, le, supplier uuid.UUID, plant *uuid.UUID) (*intercompanyHeader, error) {
	if plant == nil {
		return nil, nil
	}
	units, err := e.Units.OrgUnits(ctx, []uuid.UUID{*plant})
	if err != nil {
		return nil, fmt.Errorf("procurement: supplying plant %s: %w", *plant, err)
	}
	u, ok := units[*plant]
	switch {
	case !ok || u.Kind != "plant" || u.Status != tenancy.Active:
		return nil, problem.Invalid("procurement.supplying_plant_invalid", "supplying_plant_id", "an active plant of a company of the tenant")
	case u.LegalEntityID == le:
		return nil, problem.Invalid("procurement.supplying_plant_own_company", "supplying_plant_id",
			"a plant of another company; within a company, order a stock transport order (createStockTransportOrder)")
	}
	s, _, err := e.Suppliers.Supplier(ctx, supplier, le) // the order's header checked the supplier
	if err != nil {
		return nil, err
	}
	if s.GroupCompanyID == nil || *s.GroupCompanyID != u.LegalEntityID {
		return nil, problem.Invalid("procurement.intercompany_supplier", "supplier_id",
			"the business partner that is the supplying plant's company (its group company)")
	}
	return &intercompanyHeader{plant: *plant, company: u.LegalEntityID}, nil
}

// intercompanyItem refuses an item an intercompany order cannot carry:
// one that is no stock item (422 procurement.intercompany_item_not_stock),
// or one received beyond the ordered quantity (422
// procurement.intercompany_over_delivery), as the receipts are held to
// what the goods issue issued, the ordered quantity.
func intercompanyItem(i int, it Item) error {
	field := fmt.Sprintf("items[%d]", i)
	switch {
	case it.StorageLocationID == nil:
		return problem.Invalid("procurement.intercompany_item_not_stock", field+".storage_location_id",
			"a stock item of a material, received into a storage location of the order's company")
	case money.ParseRat(it.OverDelivery).Sign() != 0:
		return problem.Invalid("procurement.intercompany_over_delivery", field+".over_delivery_pct",
			"0: an intercompany order receives what its goods issue issued")
	}
	return nil
}

// IntercompanyIssueItem names the storage location of the supplying plant
// an item of an intercompany order is issued from.
type IntercompanyIssueItem struct {
	POItemNo          int32
	StorageLocationID uuid.UUID
}

// PostIntercompanyIssue posts a released intercompany order's goods issue
// (643) through inventory (IssueForIntercompany), once, every item's
// ordered quantity from the storage location given for it, under the
// order's row lock at the version the caller read (ADR-0008; nothing
// locks valuations or stock before an order), and names the material
// document and its day on the order. The caller acts in the receiving
// company (the order's) and, as inventory checks, the supplying one.
func (e *Endpoints) PostIntercompanyIssue(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, from []IntercompanyIssueItem) (PurchaseOrder, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return PurchaseOrder{}, err
	}
	o, err := loadOrder(ctx, tx, id, ifMatch, "the goods issue", Released)
	if err != nil {
		return PurchaseOrder{}, err
	}
	switch {
	case o.SupplyingPlantID == nil:
		return PurchaseOrder{}, problem.Conflict("procurement.not_intercompany", "Purchase order "+o.DocumentNumber+
			" names no supplying plant; only an intercompany stock transport order has a goods issue.")
	case o.IssueDocumentID != nil:
		return PurchaseOrder{}, problem.Conflict("procurement.intercompany_issued", "The goods issue of purchase order "+o.DocumentNumber+
			" is posted already, as material document "+*o.IssueDocumentNumber+".")
	}
	items, err := issueItems(o, from)
	if err != nil {
		return PurchaseOrder{}, err
	}
	d, err := e.Transports.IssueForIntercompany(ctx, o.DocumentNumber, items)
	if err != nil {
		return PurchaseOrder{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE procurement.purchase_order SET issue_document_id = $2, issue_document_number = $3, issue_date = $4,
		version = version + 1, updated_by = $5 WHERE id = $1 AND issue_document_id IS NULL AND version = $6`, o.ID, d.ID, d.Number, d.PostingDate, p.ID,
		o.Version); err != nil {
		return PurchaseOrder{}, fmt.Errorf("procurement: goods issue of purchase order %s: %w", o.DocumentNumber, err)
	}
	issued, err := getOrder(ctx, tx, id, false)
	if err != nil {
		return PurchaseOrder{}, err
	}
	return issued, e.record(ctx, "procurement.purchase_order.goods_issue", "procurement.purchase_order", id, map[string]any{
		"document_number": o.DocumentNumber, "supplying_plant_id": *o.SupplyingPlantID, "issue_document_id": d.ID, "issue_document_number": d.Number,
		"items": len(items)})
}

// issueItems are an order's items as its goods issue moves them: every
// item once, from the storage location the caller names for it (422
// procurement.issue_items naming items otherwise), its ordered quantity
// in its unit, out of the supplying plant.
func issueItems(o PurchaseOrder, from []IntercompanyIssueItem) ([]inventory.DeliveryItem, error) {
	at := make(map[int32]uuid.UUID, len(from))
	for _, f := range from {
		at[f.POItemNo] = f.StorageLocationID
	}
	if len(at) != len(from) || len(at) != len(o.Items) {
		return nil, problem.Invalid("procurement.issue_items", "items", fmt.Sprintf("each of the order's %d items once", len(o.Items)))
	}
	out := make([]inventory.DeliveryItem, len(o.Items))
	for i, it := range o.Items {
		sl, ok := at[it.No]
		if !ok {
			return nil, problem.Invalid("procurement.issue_items", "items", fmt.Sprintf("each of the order's %d items once; item %d is missing",
				len(o.Items), it.No))
		}
		out[i] = inventory.DeliveryItem{MaterialID: *it.MaterialID, PlantID: *o.SupplyingPlantID, StorageLocationID: sl, Quantity: it.Quantity, Unit: it.Unit}
	}
	return out, nil
}

// inTransit refuses cancelling or closing an intercompany order whose
// goods issue is posted and whose items are not all received: its goods
// left the supplying company and are on their way (409
// procurement.order_in_transit).
func inTransit(o PurchaseOrder, action string) error {
	if o.IssueDocumentID == nil {
		return nil
	}
	for _, it := range o.Items {
		if money.ParseRat(it.Received).Cmp(money.ParseRat(it.Quantity)) < 0 {
			return problem.Conflict("procurement.order_in_transit", fmt.Sprintf("The goods of purchase order %s are in transit since its goods issue %s; "+
				"%s it once they are received.", o.DocumentNumber, *o.IssueDocumentNumber, action))
		}
	}
	return nil
}

// orderLocated names what an order as the API gives it names by id: its
// stock items' plants and storage locations (located) and an
// intercompany order's supplying plant and its company.
func (e *Endpoints) orderLocated(ctx context.Context, out *gen.PurchaseOrder) error {
	if err := e.located(ctx, out.Items); err != nil {
		return err
	}
	plant, ok := out.SupplyingPlantID.Get()
	if !ok {
		return nil
	}
	u, _, err := e.LegalEntities.OrgUnit(ctx, plant) // a plant of the tenant when the order was created
	if err != nil {
		return fmt.Errorf("procurement: supplying plant %s: %w", plant, err)
	}
	le, _, err := e.LegalEntities.Entity(ctx, u.LegalEntityID) // a plant's company exists
	if err != nil {
		return fmt.Errorf("procurement: company of supplying plant %s: %w", u.Code, err)
	}
	out.SupplyingPlantCode, out.SupplyingPlantName, out.SupplyingCompanyCode = gen.NewOptString(u.Code), gen.NewOptString(u.Name), gen.NewOptString(le.Code)
	return nil
}
