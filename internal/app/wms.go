package app

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// WMSOrders is procurement as the exchange with warehouse systems reads
// the released purchase orders whose receipts it sends and posts the goods
// receipts they report (ADR-0082 As built S2q2): procurement's published
// order read and goods receipt, with the materials' numbers and the org
// units' codes a WMS knows them by, so integration imports no context.
func WMSOrders(cfg Config) integration.WMSOrders {
	return wmsOrders{e: Procurement(cfg), now: cfg.Now}
}

type wmsOrders struct {
	e   *procurement.Endpoints
	now func() time.Time
}

// Expected implements integration.WMSOrders: the order's stock items with
// a quantity still to receive (ordered less received, the over-delivery
// tolerance aside), by item number.
func (a wmsOrders) Expected(ctx context.Context, id uuid.UUID) (integration.WMSExpectedOrder, bool, error) {
	o, ok, err := procurement.Orders{}.Order(ctx, id)
	if err != nil || !ok || o.Status != procurement.Released {
		return integration.WMSExpectedOrder{}, false, err
	}
	var open []procurement.OrderItem
	var mats, units []uuid.UUID
	quantities := map[int32]*big.Rat{}
	for _, it := range o.Items {
		q := new(big.Rat).Sub(money.ParseRat(it.Quantity), money.ParseRat(it.Received))
		if !it.Stocked() || q.Sign() <= 0 {
			continue
		}
		open, quantities[it.ItemNo] = append(open, it), q
		mats, units = append(mats, *it.MaterialID), append(units, *it.PlantID, *it.StorageLocationID) // a stock item names all three (procurement.OrderItem)
	}
	materialOf, err := materials.Materials{}.Materials(ctx, mats)
	if err != nil {
		return integration.WMSExpectedOrder{}, false, fmt.Errorf("app: the materials of purchase order %s: %w", o.DocumentNumber, err)
	}
	unitOf, err := tenancy.Directory{}.OrgUnits(ctx, units)
	if err != nil {
		return integration.WMSExpectedOrder{}, false, fmt.Errorf("app: the plants of purchase order %s: %w", o.DocumentNumber, err)
	}
	out := integration.WMSExpectedOrder{Number: o.DocumentNumber, Items: make([]integration.WMSExpectedItem, len(open))}
	for i, it := range open {
		out.Items[i] = integration.WMSExpectedItem{ItemNo: it.ItemNo, MaterialNumber: materialOf[*it.MaterialID].Number, PlantCode: unitOf[*it.PlantID].Code,
			StorageLocationCode: unitOf[*it.StorageLocationID].Code, Quantity: money.PlainQuantity(quantities[it.ItemNo]), Unit: it.Unit,
			DeliveryDate: it.DeliveryDate}
	}
	return out, true, nil
}

// maxReference is the longest reference a goods receipt takes
// (GoodsReceiptCreate).
const maxReference = 40

// ReceiptReported implements integration.WMSOrders: a goods receipt
// (101) of the items received, dated the company's date today, its
// reference the WMS's message id when it fits one (at most 40
// characters), else none.
func (a wmsOrders) ReceiptReported(ctx context.Context, le, order uuid.UUID, reference string, received []integration.WMSReceived) (integration.WMSPosted, error) {
	day, err := config.EntityDay(ctx, configuration.Settings{}, tenancy.CompanyCode(tenancy.Directory{}), le, clock.Now(a.now))
	if err != nil {
		return integration.WMSPosted{}, fmt.Errorf("app: the date of a reported goods receipt: %w", err)
	}
	in := procurement.ReceiptInput{LegalEntityID: le, DocumentDate: day.Date, PostingDate: day.Date, Items: make([]procurement.ReceiptItem, len(received))}
	if len([]rune(reference)) <= maxReference {
		in.Reference = &reference
	}
	for i, r := range received {
		in.Items[i] = procurement.ReceiptItem{No: int32(i + 1), PurchaseOrderID: order, POItemNo: r.ItemNo, Quantity: r.Quantity, Unit: r.Unit} // #nosec G115 -- a message's items are far fewer than 2^31
	}
	g, err := a.e.PostReceipt(ctx, in)
	if err != nil {
		return integration.WMSPosted{}, err
	}
	if g.MaterialDocumentID == nil { // an item made a non-stock item by an order change since it was sent
		return integration.WMSPosted{}, problem.Unprocessable("integration.wms_not_stock",
			"The goods receipt "+g.DocumentNumber+" would move no stock: the items it names are no longer stock items of the order.")
	}
	return integration.WMSPosted{MaterialDocumentID: *g.MaterialDocumentID, MaterialDocumentNumber: *g.MaterialDocumentNumber}, nil
}
