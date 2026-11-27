package inventory

import (
	"context"
	"fmt"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// A customer's return received into stock (S2b4; ADR-0083 As built
// S2b4): sales' return receipt posts the items it receives of materials
// kept in stock through ReceiveForReturn, as a material document of
// movement type 653 (SAP's return delivery into unrestricted-use stock,
// from memory; there are no stock types yet, so the stock is the
// location's one stock) naming the returns order's number as its
// reference. Each line is valued at what the receipt asks (Costs): the
// cost its goods issue took out, which goes back from the cost of goods
// sold to the stock, the price difference at a standard price taking the
// rest; or, where it asks none, as a receipt is (at a standard price the
// stock after it at the price, at a moving price its quantity at the
// moving price). The 653 is not reversed yet (Later): Reverse refuses it.

// Costs asks, once the valuations of a posting's materials at their
// plants are locked, what each of its items is to be valued at, in item
// order and in the company's currency; nil for an item valued as its
// valuation values a receipt. A customer's return asks its sales
// context, which reads under those locks what the goods issues of the
// returned items took (S2b4).
type Costs func(ctx context.Context) ([]*money.Minor, error)

// ReceiveForReturn posts a customer's return of items into stock (653;
// S2b4, ADR-0083 As built S2b4) as a material document in the request's
// transaction, on the day the receipt names, its reference the returns
// order's number: each item checked as a goods issue's is (an active
// material kept at the storage location of a plant of a company the
// caller may act in, every plant of one company), its quantity converted
// to the base unit exactly, an item of a batch-managed material naming
// its batch by number, one of the material's (deliveryBatches), and one
// of a serial-managed material its units' serial numbers, none in stock
// (takeSerials). The valuations are locked first, as every movement of
// the materials locks them before anything else, and only then is costs
// asked what each item is worth, so what the caller reads for it cannot
// change before the transaction ends; then the stock is locked, an open
// count of a material at its location refused (409
// inventory.posting_blocked), and the document written as any is (write).
// The value posts in one WA entry from the valuation class's cost of
// goods sold account to its stock account (422
// inventory.no_inventory_account when it has none), a price difference
// on the class's price difference account. The audit record and
// MaterialDocumentPosted are written in the request's transaction. What
// each line put into the stock and the price difference together are its
// item's value, the cost of goods sold it took back.
func (e *Endpoints) ReceiveForReturn(ctx context.Context, reference string, day time.Time, in []DeliveryItem, costs Costs) (Document, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Document{}, err
	}
	post, r, err := e.prepare(ctx, tx, "653", reference, in)
	if err != nil {
		return Document{}, err
	}
	post.day = day
	if _, err := lockValuations(ctx, tx, post.lines, r); err != nil {
		return Document{}, err
	}
	asked, err := costs(ctx)
	if err != nil {
		return Document{}, err
	}
	if len(asked) != len(in) {
		return Document{}, fmt.Errorf("inventory: %d costs asked for the %d items of a customer's return", len(asked), len(in))
	}
	for i := range post.lines {
		post.lines[i].target = asked[post.lines[i].ItemNo-1] // items number their lines' ItemNo from 1 (items)
	}
	return e.posted(ctx, tx, p, post, r)
}
