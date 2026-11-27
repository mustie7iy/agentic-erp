package sales

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// ReceiptInput is a return receipt as a client sends it: the day the
// quantities arrived and the quantities received of the returns order's
// items, each named by the returns order item's number.
type ReceiptInput struct {
	Date  time.Time
	Items []ReceiptItemInput
}

// ReceiptItemInput is a quantity received of a returns order's item and,
// of a material kept in stock, the storage location it is received into,
// with the batch it goes back to by number and the serial number of each
// unit of a tracked material (S2b4).
type ReceiptItemInput struct {
	ItemQuantity
	StorageLocationID *uuid.UUID
	Batch             string
	SerialNumbers     []string
}

// quantities are the quantities the receipt takes of the returns order's
// items.
func (in ReceiptInput) quantities() []ItemQuantity {
	out := make([]ItemQuantity, len(in.Items))
	for i, it := range in.Items {
		out[i] = it.ItemQuantity
	}
	return out
}

// ReceiptItem is an item of a return receipt: the quantity received of
// the returns order's item ItemNo, a decimal string in that item's unit,
// with the billing document's item it returns from and the sales order
// item's key and description. Cost is what an item received into stock
// took back from the cost of goods sold (S2b4), minor units of the
// receipt's CostCurrency, nil for another item.
type ReceiptItem struct {
	ItemNo        int32        `json:"item_no"`
	BillingItemNo int32        `json:"billing_item_no"`
	ItemKey       *string      `json:"item_key"`
	Description   string       `json:"description"`
	Quantity      string       `json:"quantity"`
	Unit          string       `json:"unit"`
	Cost          *money.Minor `json:"cost_amount"`
}

// ReturnReceipt is a row of sales.return_receipt with its items: a
// confirmation that quantities of a returns order's items arrived on a
// date (ADR-0067, As built T85b). No is its number within the returns
// order. Its items of materials kept in stock are received into stock by
// the material document it names (S2b4), its costs in the company's
// currency CostCurrency, whose minor-unit digits are CostExponent; all
// nil for a receipt of other items, which moves no stock and posts
// nothing.
type ReturnReceipt struct {
	ID                     uuid.UUID
	No                     int32
	ReturnsOrderID         uuid.UUID
	ReturnsOrderNumber     string
	Date                   time.Time
	Items                  []ReceiptItem
	MaterialDocumentID     *uuid.UUID
	MaterialDocumentNumber *string
	CostCurrency           *string
	CostExponent           int
	CreatedAt              time.Time
	CreatedBy              uuid.UUID
}

// receiptSelect names the returns order and the stock posting by
// subqueries, not joins, for the reason orderSelect names the quotation
// by one. An item reads what it names and its unit from the sales order's
// item its returns order item's billing item billed, and its cost from
// the receipt's posting (S2b4).
const receiptSelect = `SELECT c.id, c.receipt_no, c.returns_order_id,
		(SELECT r.document_number FROM sales.returns_order r WHERE r.tenant_id = c.tenant_id AND r.id = c.returns_order_id), c.receipt_date,
		(SELECT json_agg(json_build_object('item_no', i.item_no, 'billing_item_no', ri.billing_item_no, 'item_key', t.item_key,
			'description', t.description, 'quantity', trim_scale(i.quantity)::text, 'unit', t.uom_code, 'cost_amount', k.cost_amount) ORDER BY i.item_no)
		 FROM sales.return_receipt_item i
		 JOIN sales.returns_order_item ri ON ri.tenant_id = i.tenant_id AND ri.returns_order_id = i.returns_order_id AND ri.item_no = i.item_no
		 JOIN sales.billing_document_item b ON b.tenant_id = ri.tenant_id AND b.billing_document_id = ri.billing_document_id AND b.item_no = ri.billing_item_no
		 JOIN sales.sales_order_item t ON t.tenant_id = b.tenant_id AND t.sales_order_id = b.sales_order_id AND t.item_no = b.order_item_no
		 LEFT JOIN sales.return_receipt_item_cost k ON k.tenant_id = i.tenant_id AND k.receipt_id = i.receipt_id AND k.item_no = i.item_no
		 WHERE i.tenant_id = c.tenant_id AND i.receipt_id = c.id),
		(SELECT p.material_document_id FROM sales.return_receipt_posting p WHERE p.tenant_id = c.tenant_id AND p.receipt_id = c.id),
		(SELECT p.material_document_number FROM sales.return_receipt_posting p WHERE p.tenant_id = c.tenant_id AND p.receipt_id = c.id),
		(SELECT p.currency FROM sales.return_receipt_posting p WHERE p.tenant_id = c.tenant_id AND p.receipt_id = c.id),
		coalesce((SELECT x.exponent FROM sales.return_receipt_posting p JOIN ref.currency x ON x.code = p.currency
			WHERE p.tenant_id = c.tenant_id AND p.receipt_id = c.id), 0), c.created_at, c.created_by
	FROM sales.return_receipt c`

func scanReceipt(row pgx.Row) (ReturnReceipt, error) {
	var c ReturnReceipt
	err := row.Scan(&c.ID, &c.No, &c.ReturnsOrderID, &c.ReturnsOrderNumber, &c.Date, &c.Items, &c.MaterialDocumentID, &c.MaterialDocumentNumber,
		&c.CostCurrency, &c.CostExponent, &c.CreatedAt, &c.CreatedBy)
	return c, errs.Wrapf(err, "scan return receipt")
}

// ConfirmReceipt records that quantities of a confirmed returns order's
// items arrived on a date (ADR-0067, As built T85b), as ConfirmDelivery
// records a delivery. The returns order's row is read FOR UPDATE
// (ADR-0008) and its items are read after the lock is held
// (returnsOrder), so two receipts of one returns order wait for one
// another and the second is judged on what the first left open. Only a
// confirmed returns order takes one (409
// sales.returns_order_not_confirmed); the date is not after the
// company's date today (422 sales.receipt_date_in_future); each item is
// one of the returns order's, named once, with a quantity in its unit of
// at most what is open (422, naming the request's field). The database
// adds each quantity to the returns order item's received quantity and
// moves the returns order's version (migration 000307); the sales order
// is not touched. An item of a material kept in stock names the storage
// location it is received into, and inventory posts the receipt's such
// items into stock at what their goods issues took, the cost of goods
// sold taken back (stockLines, receiveStock; S2b4); the receipt's other
// items move no stock and post nothing. The locks are the returns
// order's, then inventory's valuations and stock, after which the costs
// are read (issuedCosts). The audit record and ReturnReceived are written
// in the request's transaction.
func (e *Endpoints) ConfirmReceipt(ctx context.Context, returnsID uuid.UUID, in ReceiptInput) (ReturnReceipt, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return ReturnReceipt{}, err
	}
	r, err := returnsOrder(ctx, tx, returnsID, true)
	if err != nil {
		return ReturnReceipt{}, err
	}
	if r.Status != Confirmed {
		return ReturnReceipt{}, problem.Conflict("sales.returns_order_not_confirmed",
			"Returns order "+r.DocumentNumber+" is "+r.Status+"; a receipt is confirmed against a confirmed returns order.")
	}
	le, _, err := e.LegalEntities.Entity(ctx, r.LegalEntityID) // a stored document's legal entity exists
	if err != nil {
		return ReturnReceipt{}, fmt.Errorf("sales: company of returns order %s: %w", r.ID, err)
	}
	day, after, err := e.afterToday(ctx, le.Code, in.Date)
	if err != nil {
		return ReturnReceipt{}, err
	}
	if after {
		return ReturnReceipt{}, problem.Invalid("sales.receipt_date_in_future", "receipt_date", "A receipt is confirmed on or after the day the quantities arrived: "+
			in.Date.Format(time.DateOnly)+" is after "+day.Date.Format(time.DateOnly)+", the company's date in "+day.Zone.String()+".")
	}
	if err := receivable(r, in.quantities()); err != nil {
		return ReturnReceipt{}, err
	}
	lines, err := e.stockLines(ctx, r, in.Items)
	if err != nil {
		return ReturnReceipt{}, err
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{r.CustomerID})
	if err != nil {
		return ReturnReceipt{}, fmt.Errorf("sales: customer of returns order %s: %w", r.ID, err)
	}
	c, err := e.insertReceipt(ctx, tx, p, r, in, lines)
	if err != nil {
		return ReturnReceipt{}, err
	}
	received, err := scanReturns(tx.QueryRow(ctx, returnsSelect+` WHERE r.id = $1`, r.ID))
	if err != nil {
		return ReturnReceipt{}, fmt.Errorf("sales: read returns order %s: %w", r.ID, err)
	}
	return c, recordReceipt(ctx, c, received, le, names[r.CustomerID].Number)
}

// receivable checks a receipt's items against the returns order's, read
// under its lock (taking): each names an item of the returns order,
// once, with a quantity more than zero in the item's unit that is at
// most the item's open quantity, what is returned less what is received.
// Every refusal names the request's own field, and the last the item
// with what is open of it.
func receivable(r ReturnsOrder, in []ItemQuantity) error {
	returned := make(map[int32]ReturnsItem, len(r.Items))
	for _, it := range r.Items {
		returned[it.ItemNo] = it
	}
	short, err := taking{document: "A receipt", noField: "item_no",
		find: func(no int32) (string, *big.Rat, bool) {
			it, ok := returned[no]
			if !ok {
				return "", nil, false
			}
			return it.Unit, money.ParseRat(it.Open), true
		},
		otherUnit: func(no int32, unit string) string {
			return fmt.Sprintf("Item %d of returns order %s is in %s; its receipt is confirmed in that unit.", no, r.DocumentNumber, unit)
		}}.check(in)
	switch {
	case err != nil || short == nil:
		return err
	case short.open == nil:
		return problem.Invalid("sales.unknown_returns_item", short.noField, fmt.Sprintf("Returns order %s has no item %d.", r.DocumentNumber, short.no))
	}
	it := returned[short.no]
	return problem.Invalid("sales.exceeds_open_quantity", short.quantityField, fmt.Sprintf(
		"Item %d of returns order %s has %s %s still expected (%s returned, %s received); %s is more.",
		short.no, r.DocumentNumber, it.Open, it.Unit, it.Quantity, it.Received, short.quantity))
}

// returnReceipts are the tables a return receipt is written to.
var returnReceipts = confirmation{table: "return_receipt", document: "returns_order_id", number: "receipt_no", date: "receipt_date",
	items: "return_receipt_item", parent: "receipt_id", item: "item_no"}

// insertReceipt writes the receipt with its items, receives its stock
// lines into stock (receiveStock) and returns it as it reads. Its number
// is the next within the returns order, which the caller holds locked.
func (e *Endpoints) insertReceipt(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r ReturnsOrder, in ReceiptInput, lines []stockLine) (ReturnReceipt,
	error) {
	id, err := returnReceipts.insert(ctx, tx, p, r.ID, in.Date, in.quantities())
	if err != nil {
		return ReturnReceipt{}, err
	}
	if err := e.receiveStock(ctx, tx, p, r, id, in.Date, lines); err != nil {
		return ReturnReceipt{}, err
	}
	c, err := scanReceipt(tx.QueryRow(ctx, receiptSelect+` WHERE c.id = $1`, id))
	if err != nil {
		return ReturnReceipt{}, fmt.Errorf("sales: read return receipt %s: %w", id, err)
	}
	return c, nil
}

// recordReceipt adds the audit record of a receipt and publishes
// ReturnReceived in the same transaction (ADR-0007, ADR-0011). The event
// is the returns order's (its subject is the returns order's id): it
// carries the receipt's items with what is received of each returns
// order item after it, names the customer by id and number, never by
// name (product owner, 2026-11-05), and gives the returns order's
// version after the receipt, and the material document that received
// its stock items with each such item's cost (S2b4).
func recordReceipt(ctx context.Context, c ReturnReceipt, after ReturnsOrder, le tenancy.Entity, customerNumber string) error {
	received := make(map[int32]string, len(after.Items))
	for _, it := range after.Items {
		received[it.ItemNo] = it.Received
	}
	items := make([]map[string]any, len(c.Items))
	for i, it := range c.Items {
		items[i] = map[string]any{"item_no": it.ItemNo, "billing_item_no": it.BillingItemNo, "quantity": map[string]string{"value": it.Quantity, "unit": it.Unit},
			"received_quantity": map[string]string{"value": received[it.ItemNo], "unit": it.Unit}, "cost": nil}
		if it.Cost != nil { // an item's cost is its receipt's posting's (return_receipt_item_cost_posting_fkey)
			items[i]["cost"] = money.EventAmount(*it.Cost, *c.CostCurrency, c.CostExponent)
		}
	}
	date := c.Date.Format(time.DateOnly)
	payload := map[string]any{"receipt_id": c.ID, "receipt_no": c.No, "returns_order_id": after.ID, "document_number": after.DocumentNumber,
		"legal_entity_id": after.LegalEntityID, "legal_entity_code": le.Code, "customer_id": after.CustomerID, "customer_number": customerNumber,
		"billing_document_id": after.BillingDocumentID, "billing_document_number": after.BillingDocumentNumber, "sales_order_id": after.OrderID,
		"sales_order_number": after.OrderNumber, "receipt_date": date, "items": items,
		"material_document_id": c.MaterialDocumentID, "material_document_number": c.MaterialDocumentNumber, "version": after.Version}
	detail := map[string]any{"returns_order_id": after.ID, "document_number": after.DocumentNumber, "receipt_no": c.No, "receipt_date": date, "items": items,
		"material_document_number": c.MaterialDocumentNumber}
	if err := audit.Record(ctx, "sales.return_receipt.confirm", "sales.return_receipt", c.ID, detail,
		outbox.Event{Context: "sales", Name: "return_received", AggregateType: "sales.returns_order", AggregateID: after.ID, Payload: payload}); err != nil {
		return fmt.Errorf("sales: record receipt %s of returns order %s: %w", c.ID, after.ID, err)
	}
	return nil
}
