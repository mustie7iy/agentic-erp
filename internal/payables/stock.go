package payables

import (
	"context"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/inventory"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

// Stock posts what an invoice of stock items puts on their stock (the
// inventory context's published API, S1e; ADR-0082 §3 and As built S1e):
// the price differences under the valuations' locks, and whether an
// invoice put any there.
type Stock interface {
	InvoiceDifferences(ctx context.Context, company uuid.UUID, items []inventory.InvoiceItem, src inventory.InvoiceSource, post inventory.PostEntry) error
	RevaluedStock(ctx context.Context, invoice uuid.UUID) (bool, error)
}

// post posts the invoice's entry. When items of it are on stock items of
// its order (one valued through GR/IR, order not nil), it posts through
// the inventory context, which locks their valuations first and has each
// item's price difference go to its stock as far as the stock holds the
// quantity it spreads over (the invoiced one, else, for a subsequent debit
// or credit that moves none, what was received of the item), the rest to
// its valuation class's price difference account (ledger.GRIRStock).
func (e *Endpoints) post(ctx context.Context, v Invoice, order *procurement.Order, in ledger.Posting) (ledger.Entry, error) {
	byNo := map[int32]*procurement.OrderItem{}
	if order != nil {
		for i := range order.Items {
			byNo[order.Items[i].ItemNo] = &order.Items[i]
		}
	}
	var stock []int // the GR/IR items on stock items, in order
	var items []inventory.InvoiceItem
	src := inventory.InvoiceSource{InvoiceID: v.ID}
	for j, it := range in.GRIR {
		oi := byNo[it.POItemNo] // every GR/IR item's order item is the order's (grirItem)
		if oi == nil || !oi.Stocked() {
			continue
		}
		q := strings.TrimPrefix(it.Quantity, "-")
		if money.ParseRat(q).Sign() == 0 {
			q = oi.Received
		}
		stock, items = append(stock, j), append(items, inventory.InvoiceItem{MaterialID: *oi.MaterialID, PlantID: *oi.PlantID, Quantity: q, Unit: oi.Unit})
		src.PurchaseOrderID, src.POItemNos = order.ID, append(src.POItemNos, oi.ItemNo)
	}
	if len(stock) == 0 {
		return e.Postings.Post(ctx, in)
	}
	var entry ledger.Entry
	err := e.Stock.InvoiceDifferences(ctx, v.LegalEntityID, items, src, func(bookings []ledger.GRIRStock) (ledger.Entry, []ledger.StockPosted, error) {
		for k, j := range stock {
			in.GRIR[j].Stock = &bookings[k]
		}
		var err error
		if entry, err = e.Postings.Post(ctx, in); err != nil {
			return ledger.Entry{}, nil, err
		}
		posted := make([]ledger.StockPosted, len(stock))
		for k, j := range stock {
			posted[k] = entry.Stock[j]
		}
		return entry, posted, nil
	})
	return entry, err
}

// requireStockUnrevalued refuses to reverse an invoice that put a price
// difference on a stock (InvoiceDifferences): the entry's reversal would
// take it off a stock that may have been issued since. A credit memo
// takes it off as far as the stock still holds the goods. Only an
// invoice on its order's stock items can have.
func (e *Endpoints) requireStockUnrevalued(ctx context.Context, v Invoice) error {
	if v.PurchaseOrderID == nil {
		return nil
	}
	// A posted invoice's order exists; ok false would leave no items.
	order, _, err := e.Orders.Order(ctx, *v.PurchaseOrderID)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(order.Items, func(oi procurement.OrderItem) bool {
		return oi.Stocked() && slices.ContainsFunc(v.Items, func(it Item) bool { return it.POItemNo != nil && *it.POItemNo == oi.ItemNo })
	}) {
		return nil
	}
	revalued, err := e.Stock.RevaluedStock(ctx, v.ID)
	if err != nil {
		return err
	}
	if revalued {
		return problem.Conflict("payables.invoice_revalued_stock", "Invoice "+v.DocumentNumber+" put a price difference on the stock of its "+
			"materials; post a credit memo instead of reversing it.")
	}
	return nil
}
