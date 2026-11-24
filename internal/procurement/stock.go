package procurement

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/inventory"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Stock items (S1e; ADR-0082 §2-§3 and As built S1e): a purchase order
// item that names a plant and a storage location is a stock item of its
// material. Its goods receipt (101) and the receipt's reversal (102) move
// the material's stock through inventory's published API in the goods
// movement's transaction, and the WE entry posts its value on the stock
// account of the material's valuation class rather than on the item's
// account (SAP's BSX for an item without account assignment, from
// memory). Returns to the supplier (122) of stock items are Later.

// Materials reads materials (the materials context's published API): by
// id, what a stock item's or a material item's material is, where it is
// kept and its units; by number, the materials a catalog's keys are
// mapped onto (S1f2).
type Materials interface {
	Materials(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]materials.Material, error)
	ByNumber(ctx context.Context, numbers []string) (map[string]materials.Material, error)
}

// Movements moves stock (the inventory context's published API): a
// goods receipt of stock items as a material document, and its reversal,
// each posting its value on the entry the callback posts.
type Movements interface {
	ReceiveForOrder(ctx context.Context, in inventory.OrderReceipt, post inventory.PostEntry) (inventory.Document, error)
	ReverseForOrder(ctx context.Context, id uuid.UUID, day time.Time, reference string, post inventory.PostEntry) (inventory.Document, error)
}

// stockItem checks an order item that names a plant or a storage
// location: both named, with its material, assigned to no cost center
// (its receipts post to the stock), ordered in the company's currency,
// which its stock is valued in, and, when the item is added (ordered, or
// added by an order change), at an active plant of the company the active
// material is kept at, at that storage location; in a unit of the
// material whose quantity converts to its base unit exactly. A released
// order's item an order change keeps keeps its material and location
// (keptItems), and is not refused because its material was blocked or its
// extension removed since: a change may still bring it down to what was
// received. An item that names neither is no stock item and is not checked
// here.
func (e *Endpoints) stockItem(ctx context.Context, h orderHeader, field string, it Item, added bool) error {
	switch {
	case it.PlantID == nil && it.StorageLocationID == nil:
		return nil
	case it.PlantID == nil || it.StorageLocationID == nil:
		return problem.Invalid("procurement.invalid_stock_item", field+".storage_location_id", "A stock item names its plant and its storage location.")
	case it.MaterialID == nil:
		return problem.Invalid("procurement.invalid_stock_item", field+".material_id", "A stock item names its material.")
	case it.assignment() != AssignNone:
		return problem.Invalid("procurement.invalid_stock_item", field+".account_assignment",
			"A stock item is assigned to no cost center: its receipts post to the stock.")
	case h.currency != h.le.LocalCurrency:
		return problem.Invalid("procurement.invalid_stock_item", field+".storage_location_id", "Stock items are ordered in company "+h.le.Code+
			"'s currency "+h.le.LocalCurrency+", which their stock is valued in.")
	}
	ms, err := e.Materials.Materials(ctx, []uuid.UUID{*it.MaterialID})
	if err != nil {
		return fmt.Errorf("procurement: material %s of %s: %w", *it.MaterialID, field, err)
	}
	m, ok := ms[*it.MaterialID]
	if !ok {
		return problem.Invalid("procurement.invalid_stock_item", field+".material_id", "No material with this id.")
	}
	if added {
		if err := e.usableAt(ctx, h, field, it, m); err != nil {
			return err
		}
	}
	if _, err := m.Convert(money.ParseRat(it.Quantity), it.Unit); err != nil {
		return problem.Invalid("procurement.invalid_stock_item", field+".quantity.unit", m.UnitRefusal(it.Quantity, it.Unit, err))
	}
	return nil
}

// usableAt checks an added stock item's plant, an active one of the
// company, and that its material is active, kept in stock and kept at its
// storage location there. A batch-managed or serial-managed material's
// goods receipt names its batch or serial numbers (S2f1, S2g1), but an
// intercompany stock transport order's goods issue carries neither yet
// (ADR-0082 As built S2f, S2g), so such an order refuses one.
func (e *Endpoints) usableAt(ctx context.Context, h orderHeader, field string, it Item, m materials.Material) error {
	if err := e.orgUnit(ctx, it.PlantID, h.le.ID, "plant", field+".plant_id"); err != nil {
		return err
	}
	switch {
	case m.Status != materials.Active || !m.Stocked():
		return problem.Invalid("procurement.invalid_stock_item", field+".material_id", "Material "+m.Number+" is "+m.Status+" and of type "+m.Type+
			"; a stock item is of an active material kept in stock.")
	case h.intercompany != nil && m.BatchManaged:
		return problem.Invalid("procurement.invalid_stock_item", field+".material_id", "Material "+m.Number+
			" is managed in batches; an intercompany stock transport order's goods issue carries no batch yet.")
	case h.intercompany != nil && m.SerialManaged:
		return problem.Invalid("procurement.invalid_stock_item", field+".material_id", "Material "+m.Number+
			" has a serial number profile; an intercompany stock transport order's goods issue carries no serial numbers yet.")
	case !m.KeptAt(*it.PlantID, *it.StorageLocationID):
		return problem.Invalid("procurement.invalid_stock_item", field+".storage_location_id", "Material "+m.Number+
			" is not kept at this storage location of the plant.")
	}
	return nil
}

// postedMovement is what a goods movement posted: its WE entry and the
// material document of its stock items, each nil when it has none, and
// that document's lines by the goods movement's item they moved (S2f1,
// S2g1: the batches and serial numbers, tracked).
type postedMovement struct {
	year           *int16
	entry          *uuid.UUID
	document       *uuid.UUID
	documentNumber *string
	lines          map[int]inventory.Line
}

// tracked gives each item the batch and serial numbers its material
// document's line moved (lines, by the item's index), as inventory found
// or created them; an item without a line, no stock item, names none.
func tracked(items []ReceiptItem, lines map[int]inventory.Line) {
	for i, l := range lines {
		items[i].BatchID, items[i].BatchNumber, items[i].SerialNumbers = l.BatchID, l.BatchNumber, l.SerialNumbers
	}
}

// notStockItem refuses a batch or serial numbers on a receipt's item that
// is no stock item: only a stock item's material moves in stock (S1e).
func notStockItem(field string, it ReceiptItem, oi orderItem) error {
	if it.Batch != nil {
		field += ".batch"
	} else {
		field += ".serial_numbers"
	}
	return problem.Invalid("procurement.not_stock_item", field, fmt.Sprintf("Item %d of order %s is no stock item; only a stock item's receipt names "+
		"a batch or serial numbers.", it.POItemNo, oi.documentNumber))
}

// asReceiptItems names a refusal of the material document's item k by the
// goods movement's item it is, stock[k]: inventory numbers the stock items
// alone.
func asReceiptItems(err error, stock []int) error {
	var p *problem.Problem
	if !errors.As(err, &p) {
		return err
	}
	for j := range p.Errors {
		for k, i := range stock {
			if rest, ok := strings.CutPrefix(p.Errors[j].Field, fmt.Sprintf("items[%d].", k)); ok {
				p.Errors[j].Field = fmt.Sprintf("items[%d].%s", i, rest)
				break
			}
		}
	}
	return p
}

func entryOf(entry ledger.Entry, ok bool) postedMovement {
	if !ok {
		return postedMovement{}
	}
	return postedMovement{year: &entry.FiscalYear, entry: &entry.ID}
}

// postStock posts a goods movement's WE entry (postMovement) and, when it
// has stock items, their material document: a receipt's through
// ReceiveForOrder, a receipt's reversal through ReverseForOrder of the
// receipt's document (g.reverses), each posting the WE entry with its
// stock items' bookings in the inventory context's call, so the
// valuations and stock are locked before the ledger's GR/IR lock, as
// every inventory posting takes them.
func (e *Endpoints) postStock(ctx context.Context, id uuid.UUID, number string, g GoodsReceipt, current []orderItem) (postedMovement, error) {
	var stock []int // the goods items that are stock items, in order
	for i := range g.Items {
		if current[i].storageLocation != nil {
			stock = append(stock, i)
		}
	}
	if len(stock) == 0 {
		entry, _, ok, err := e.postMovement(ctx, id, number, g, current, nil)
		return entryOf(entry, ok), err
	}
	if err := e.stockValued(ctx, g, current, stock); err != nil {
		return postedMovement{}, err
	}
	var out postedMovement
	post := func(bookings []ledger.GRIRStock) (ledger.Entry, []ledger.StockPosted, error) {
		per := make([]*ledger.GRIRStock, len(g.Items))
		for k, i := range stock {
			per[i] = &bookings[k]
		}
		entry, index, _, err := e.postMovement(ctx, id, number, g, current, per) // stock items are valued through GR/IR (stockValued)
		if err != nil {
			return ledger.Entry{}, nil, err
		}
		posted := make([]ledger.StockPosted, len(stock))
		for k, i := range stock {
			posted[k] = entry.Stock[index[i]]
		}
		out = entryOf(entry, true)
		return entry, posted, nil
	}
	var doc inventory.Document
	var err error
	switch {
	case g.ReversalOfID != nil && g.reverses == nil: // a receipt of stock items has its document (insertMovement)
		return postedMovement{}, fmt.Errorf("procurement: goods movement %s reverses stock items without a material document", number)
	case g.ReversalOfID != nil:
		doc, err = e.Movements.ReverseForOrder(ctx, *g.reverses, g.PostingDate, number, post)
	default:
		doc, err = e.Movements.ReceiveForOrder(ctx, inventory.OrderReceipt{Company: g.LegalEntityID, Day: g.PostingDate, Reference: number,
			Items: orderItems(g, current, stock)}, post)
	}
	if err != nil {
		return postedMovement{}, asReceiptItems(err, stock)
	}
	out.document, out.documentNumber, out.lines = &doc.ID, &doc.Number, map[int]inventory.Line{}
	for _, l := range doc.Lines { // a receipt's document has one line per item (ItemNo), a stock item's
		out.lines[stock[l.ItemNo-1]] = l
	}
	return out, nil
}

// orderItems are the stock items of a receipt (stock, the goods items'
// indexes) as inventory receives them, with the batch and serial numbers
// each names (S2f1, S2g1).
func orderItems(g GoodsReceipt, current []orderItem, stock []int) []inventory.OrderItem {
	items := make([]inventory.OrderItem, len(stock))
	for k, i := range stock {
		oi, it := current[i], g.Items[i]
		items[k] = inventory.OrderItem{MaterialID: *oi.material, PlantID: *oi.plant, StorageLocationID: *oi.storageLocation, Quantity: it.Quantity, Unit: it.Unit,
			Batch: it.Batch, SerialNumbers: it.SerialNumbers}
	}
	return items
}

// stockValued refuses stock items that cannot be received into stock: a
// return of them (122, Later), or of an order valued directly, which
// posts no receipt, or in another currency than the company's, which a
// valuation is kept in (an order item's currency is checked as it is
// ordered; this holds for orders the company's currency changed under).
func (e *Endpoints) stockValued(ctx context.Context, g GoodsReceipt, current []orderItem, stock []int) error {
	le, _, err := e.LegalEntities.Entity(ctx, g.LegalEntityID) // the movement's legal entity was checked
	if err != nil {
		return err
	}
	for _, i := range stock {
		oi := current[i]
		field := fmt.Sprintf("items[%d]", i)
		switch {
		case g.MovementType == Return || g.MovementType == ReturnReversal:
			return problem.Invalid("procurement.stock_item_return", field, fmt.Sprintf("Item %d of order %s is a stock item; returning stock items "+
				"to the supplier is not built yet.", g.Items[i].POItemNo, oi.documentNumber))
		case oi.valuation != ValuationGRIR || oi.currency != le.LocalCurrency:
			return problem.Unprocessable("procurement.stock_item_not_valued", fmt.Sprintf("Item %d of order %s is a stock item, received into stock "+
				"against GR/IR in company %s's currency %s; the order is valued %s in %s.", g.Items[i].POItemNo, oi.documentNumber, le.Code,
				le.LocalCurrency, oi.valuation, oi.currency))
		}
	}
	return nil
}

// located gives the stock items among items their plant's and storage
// location's codes and names (tenancy's org units, each read once), which
// a reader needs beside their ids (REVIEW.md, API-consumer classes). An
// org unit is never deleted, so each is found; one not found would leave
// its codes empty.
func (e *Endpoints) located(ctx context.Context, items []gen.PurchaseOrderItem) error {
	units := map[uuid.UUID]tenancy.Unit{}
	unit := func(id uuid.UUID) (tenancy.Unit, error) {
		if u, ok := units[id]; ok {
			return u, nil
		}
		u, _, err := e.LegalEntities.OrgUnit(ctx, id)
		if err != nil {
			return tenancy.Unit{}, fmt.Errorf("procurement: org unit %s of a stock item: %w", id, err)
		}
		units[id] = u
		return u, nil
	}
	for i := range items {
		it := &items[i]
		plant, ok := it.PlantID.Get()
		if !ok {
			continue
		}
		p, err := unit(plant)
		if err != nil {
			return err
		}
		l, err := unit(it.StorageLocationID.Value) // given with the plant (purchase_order_item_stock)
		if err != nil {
			return err
		}
		it.PlantCode, it.PlantName = gen.NewOptString(p.Code), gen.NewOptString(p.Name)
		it.StorageLocationCode, it.StorageLocationName = gen.NewOptString(l.Code), gen.NewOptString(l.Name)
	}
	return nil
}
