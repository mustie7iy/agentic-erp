package sales

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// A customer's return received into stock (S2b4; ADR-0083 As built
// S2b4, ADR-0067 As built T85b): a return receipt's items of materials
// kept in stock name the storage location they are received into, and
// the batch and serial numbers of a tracked material, and inventory
// posts them into stock as one material document (653) through the
// ReturnStock port. Each item goes back at the cost its order item's goods
// issues took out (issuedCosts), so the cost of goods sold falls by what
// it was debited; an order item no goods issue delivered (one delivered
// before goods issues, S2b1) goes back as its valuation values a
// receipt. The receipt's other items move no stock, as before.

// StockReturn is what a return receipt puts into stock: its stock items,
// on the receipt's day, with the returns order's number as the material
// document's reference.
type StockReturn struct {
	Reference string
	Day       time.Time
	Items     []StockReturnItem
}

// StockReturnItem is a quantity of a material in the order item's unit,
// received into a storage location of a plant; of a batch-managed
// material, the batch by number, and of a serial-managed one, the serial
// number of each unit.
type StockReturnItem struct {
	MaterialID, PlantID, StorageLocationID uuid.UUID
	Quantity, Unit                         string
	Batch                                  string
	SerialNumbers                          []string
}

// StockReturned is the material document a return posted: its id and
// number, the company's currency it is valued in, and what each item,
// in order, took back from the cost of goods sold.
type StockReturned struct {
	MaterialDocumentID     uuid.UUID
	MaterialDocumentNumber string
	Currency               string
	Costs                  []money.Minor
}

// ReturnStock posts a return receipt's stock items into stock (inventory
// context, through the composition root; S2b4): costs is asked, once the
// valuations and stock are locked, what each item is worth, nil for one
// valued as its valuation values a receipt.
type ReturnStock interface {
	ReceiveReturn(ctx context.Context, in StockReturn, costs func(context.Context) ([]*money.Minor, error)) (StockReturned, error)
}

// stockLine is a receipt item received into stock: its index in the
// request, the returns order's item and the sales order's item it is of,
// and what it puts into stock.
type stockLine struct {
	at          int
	itemNo      int32
	orderItemNo int32
	item        StockReturnItem
}

// stockLines are the receipt's items of materials kept in stock, each
// naming its storage location (422 sales.storage_location_required), one
// of the returns order's company (422 sales.storage_location_invalid),
// and its plant the location's; an item of no material kept in stock
// names no storage location, batch or serial numbers (422
// sales.return_not_stocked). The returns order's items were read under
// its row lock, and the materials of a billed order's items do not
// change.
func (e *Endpoints) stockLines(ctx context.Context, r ReturnsOrder, in []ReceiptItemInput) ([]stockLine, error) {
	returned := make(map[int32]ReturnsItem, len(r.Items))
	var ids []uuid.UUID
	for _, it := range r.Items {
		returned[it.ItemNo] = it
		if it.MaterialID != nil {
			ids = append(ids, *it.MaterialID)
		}
	}
	ms, err := e.Materials.Materials(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("sales: materials of returns order %s: %w", r.DocumentNumber, err)
	}
	var out []stockLine
	var locations []uuid.UUID
	for i, it := range in {
		ri := returned[it.ItemNo] // receivable checked every item is the returns order's
		at := fmt.Sprintf("items[%d].", i)
		if ri.MaterialID == nil || !ms[*ri.MaterialID].Stocked() {
			if err := notStocked(at, it, ri, r.DocumentNumber); err != nil {
				return nil, err
			}
			continue
		}
		if it.StorageLocationID == nil {
			return nil, problem.Invalid("sales.storage_location_required", at+"storage_location_id", fmt.Sprintf("Item %d of returns order %s is material "+
				"%s, kept in stock: its receipt names the storage location it is received into.", it.ItemNo, r.DocumentNumber, ms[*ri.MaterialID].Number))
		}
		locations = append(locations, *it.StorageLocationID)
		out = append(out, stockLine{at: i, itemNo: it.ItemNo, orderItemNo: ri.OrderItemNo, item: StockReturnItem{MaterialID: *ri.MaterialID,
			StorageLocationID: *it.StorageLocationID, Quantity: it.Quantity, Unit: it.Unit, Batch: it.Batch, SerialNumbers: it.SerialNumbers}})
	}
	return out, e.plants(ctx, r, out, locations)
}

// notStocked refuses a storage location, batch or serial numbers on a
// receipt item of no material kept in stock, naming the field.
func notStocked(at string, it ReceiptItemInput, ri ReturnsItem, number string) error {
	var field string
	switch {
	case it.StorageLocationID != nil:
		field = "storage_location_id"
	case it.Batch != "":
		field = "batch_number"
	case len(it.SerialNumbers) > 0:
		field = "serial_numbers"
	default:
		return nil
	}
	return problem.Invalid("sales.return_not_stocked", at+field, fmt.Sprintf("Item %d of returns order %s (%s) is no material kept in stock; its "+
		"receipt names no storage location, batch or serial numbers.", ri.ItemNo, number, ri.Description))
}

// plants gives each line the plant of its storage location, one of the
// returns order's company.
func (e *Endpoints) plants(ctx context.Context, r ReturnsOrder, lines []stockLine, locations []uuid.UUID) error {
	if len(lines) == 0 {
		return nil
	}
	units, err := e.LegalEntities.OrgUnits(ctx, locations)
	if err != nil {
		return fmt.Errorf("sales: storage locations of a receipt of returns order %s: %w", r.DocumentNumber, err)
	}
	for k, l := range lines {
		u, ok := units[l.item.StorageLocationID]
		if !ok || u.Kind != "storage_location" || u.LegalEntityID != r.LegalEntityID || u.ParentID == nil {
			return problem.Invalid("sales.storage_location_invalid", fmt.Sprintf("items[%d].storage_location_id", l.at),
				"No storage location of returns order "+r.DocumentNumber+"'s company with this id.")
		}
		lines[k].item.PlantID = *u.ParentID
	}
	return nil
}

// receiveStock posts the receipt's stock lines into stock (ReturnStock),
// each at what issuedCosts finds its goods issues took, and records the
// material document and each item's cost with the receipt. A refusal of
// inventory's names the receipt's own fields (asReceipt).
func (e *Endpoints) receiveStock(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r ReturnsOrder, receipt uuid.UUID, day time.Time,
	lines []stockLine) error {
	if len(lines) == 0 {
		return nil
	}
	// The receipts into stock of one sales order wait for one another,
	// whatever plants they go into, so each reads what the ones before it
	// took back (issuedCosts). Goods issues and their reversals never take
	// this lock; it comes after the returns order's and before
	// inventory's valuations, as nothing takes them the other way round.
	if err := database.LockTenant(ctx, tx, "sales.return_stock", r.OrderID.String()); err != nil {
		return fmt.Errorf("sales: lock the receipts into stock of sales order %s: %w", r.OrderNumber, err)
	}
	in := StockReturn{Reference: r.DocumentNumber, Day: day, Items: make([]StockReturnItem, len(lines))}
	for k, l := range lines {
		in.Items[k] = l.item
	}
	got, err := e.Returns.ReceiveReturn(ctx, in, func(ctx context.Context) ([]*money.Minor, error) { return issuedCosts(ctx, tx, r, lines) })
	if err != nil {
		return asReceipt(err, lines)
	}
	nos, costs := make([]int32, len(lines)), make([]int64, len(lines))
	for k, l := range lines {
		nos[k], costs[k] = l.itemNo, int64(got.Costs[k])
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sales.return_receipt_posting (tenant_id, receipt_id, returns_order_id, material_document_id,
			material_document_number, currency, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		p.TenantID, receipt, r.ID, got.MaterialDocumentID, got.MaterialDocumentNumber, got.Currency, p.ID); err != nil {
		return fmt.Errorf("sales: record the stock posting of receipt %s of returns order %s: %w", receipt, r.DocumentNumber, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sales.return_receipt_item_cost (tenant_id, receipt_id, item_no, cost_amount)
			SELECT $1, $2, n, c FROM unnest($3::int[], $4::bigint[]) AS x(n, c)`, p.TenantID, receipt, nos, costs); err != nil {
		return fmt.Errorf("sales: record the costs of receipt %s of returns order %s: %w", receipt, r.DocumentNumber, err)
	}
	return nil
}

// asReceipt names a refusal of the material document's item k by the
// receipt's item it is, lines[k]: inventory numbers the stock items
// alone, and the receipt names no material or plant (its item's and its
// storage location's) and no amount.
func asReceipt(err error, lines []stockLine) error {
	return problem.Renamed(err, func(field string) (string, bool) {
		for k, l := range lines {
			rest, ok := strings.CutPrefix(field, fmt.Sprintf("items[%d].", k))
			if !ok {
				continue
			}
			switch rest {
			case "quantity":
				rest = "quantity.value"
			case "material_id", "amount":
				rest = "item_no"
			case "plant_id":
				rest = "storage_location_id"
			}
			return fmt.Sprintf("items[%d].%s", l.at, rest), true
		}
		return field, true
	})
}

// issueBasis is what an order item's goods issues took out and are not
// reversed (issued, cost), and what the receipts posted into stock before
// gave back of it (back, costBack), in the order item's unit and the
// company's currency.
type issueBasis struct {
	issued, back   *big.Rat
	cost, costBack money.Minor
}

// issuedCosts is what each line is worth, read under the lock of the
// sales order's receipts into stock (receiveStock) once inventory holds
// the locks of the lines' materials' valuations at their plants, which
// every goods issue and its reversal of them take first, so nothing
// changes what is read here before the transaction ends: the line's
// share, by quantity, of what its order item's goods
// issues took out that the receipts before it did not give back, its
// remainder at the last, rounded half away from zero; nil for an order
// item no goods issue delivered. A line beyond what the goods issues
// delivered and is not back in stock is refused (422
// sales.exceeds_issued_quantity naming its quantity).
func issuedCosts(ctx context.Context, tx pgx.Tx, r ReturnsOrder, lines []stockLine) ([]*money.Minor, error) {
	basis, err := issueBases(ctx, tx, r.OrderID, lines)
	if err != nil {
		return nil, err
	}
	out := make([]*money.Minor, len(lines))
	for k, l := range lines {
		b := basis[l.orderItemNo]
		if b.issued.Sign() == 0 {
			continue
		}
		q, left := money.ParseRat(l.item.Quantity), new(big.Rat).Sub(b.issued, b.back)
		if q.Cmp(left) > 0 {
			// Named by the line's index k, as inventory names its items:
			// asReceipt names the request's item once.
			return nil, problem.Invalid("sales.exceeds_issued_quantity", fmt.Sprintf("items[%d].quantity", k), fmt.Sprintf(
				"Item %d of returns order %s: its goods issues delivered %s %s of sales order %s's item %d that is not back in stock; %s is more.",
				l.itemNo, r.DocumentNumber, money.PlainQuantity(left), l.item.Unit, r.OrderNumber, l.orderItemNo, l.item.Quantity))
		}
		// Both are sums of amounts of at least zero, so the rest is one; a
		// reversal since may have taken back more than it.
		share := new(big.Rat).Mul(new(big.Rat).SetInt64(int64(max(b.cost-b.costBack, 0))), q)
		cost, _ := money.Round(share.Quo(share, left), 0) // q is at most left, so the cost at most the rest, an amount: Round fails only beyond one
		out[k] = &cost
		b.back.Add(b.back, q)
		b.costBack += cost // bound: at most b.cost, what the goods issues took, an amount
	}
	return out, nil
}

// issueBases reads, for each order item the lines are of, what its goods
// issues not reversed took out and what earlier receipts posted into
// stock gave back of it (issueBasis).
func issueBases(ctx context.Context, tx pgx.Tx, order uuid.UUID, lines []stockLine) (map[int32]*issueBasis, error) {
	nos := make([]int32, len(lines))
	for k, l := range lines {
		nos[k] = l.orderItemNo
	}
	rows, _ := tx.Query(ctx, `SELECT n.no, trim_scale(coalesce(g.quantity, 0))::text, coalesce(g.cost, 0)::bigint,
			trim_scale(coalesce(b.quantity, 0))::text, coalesce(b.cost, 0)::bigint
		FROM (SELECT DISTINCT no FROM unnest($2::int[]) AS x(no)) n
		LEFT JOIN LATERAL (SELECT sum(i.quantity) AS quantity, sum(i.cost_amount) AS cost FROM sales.delivery d
			JOIN sales.delivery_item i ON i.tenant_id = d.tenant_id AND i.delivery_id = d.id
			WHERE d.sales_order_id = $1 AND d.outbound_delivery_id IS NOT NULL AND i.order_item_no = n.no
				AND NOT EXISTS (SELECT FROM sales.delivery_reversal x WHERE x.tenant_id = d.tenant_id AND x.delivery_id = d.id)) g ON true
		LEFT JOIN LATERAL (SELECT sum(ci.quantity) AS quantity, sum(k.cost_amount) AS cost FROM sales.return_receipt_item_cost k
			JOIN sales.return_receipt_item ci ON ci.tenant_id = k.tenant_id AND ci.receipt_id = k.receipt_id AND ci.item_no = k.item_no
			JOIN sales.returns_order_item ri ON ri.tenant_id = ci.tenant_id AND ri.returns_order_id = ci.returns_order_id AND ri.item_no = ci.item_no
			JOIN sales.billing_document_item bi ON bi.tenant_id = ri.tenant_id AND bi.billing_document_id = ri.billing_document_id
				AND bi.item_no = ri.billing_item_no
			WHERE bi.sales_order_id = $1 AND bi.order_item_no = n.no) b ON true
		ORDER BY n.no`, order, nos)
	out := map[int32]*issueBasis{}
	var no int32
	var issued, back string
	var cost, costBack money.Minor
	if _, err := pgx.ForEachRow(rows, []any{&no, &issued, &cost, &back, &costBack}, func() error {
		out[no] = &issueBasis{issued: money.ParseRat(issued), back: money.ParseRat(back), cost: cost, costBack: costBack}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("sales: what the goods issues of sales order %s took: %w", order, err)
	}
	return out, nil
}
