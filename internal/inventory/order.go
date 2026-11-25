package inventory

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// A purchase order's goods receipt into stock (S1e; ADR-0082 §2 and As
// built S1e): procurement's goods movement of stock items posts a 101,
// and its reversal a 102, through ReceiveForOrder and ReverseForOrder in
// its own transaction. The material document is valued as any is, but
// its value posts on the entry procurement posts for the goods movement,
// against GR/IR (source context procurement, which moves the order
// items' GR/IR positions), not on an entry of its own: each item's GR/IR
// counter line goes to its valuation class's stock account, the part the
// stock does not take to the class's price difference account
// (ledger.GRIRStock).

// OrderItem is a stock item of a purchase order a goods receipt moves: a
// positive quantity of its material in the order item's unit, into a
// storage location of a plant.
type OrderItem struct {
	MaterialID, PlantID, StorageLocationID uuid.UUID
	Quantity, Unit                         string
}

// OrderReceipt is a purchase order goods receipt's stock items, in the
// goods movement's order, posted on its posting date with its number as
// the reference.
type OrderReceipt struct {
	Company   uuid.UUID
	Day       time.Time
	Reference string
	Items     []OrderItem
}

// PostEntry posts a goods movement's entry, with the stock part of each
// stock item's GR/IR item (bookings, one per item of the material
// document in item order), and answers the entry and what each of them
// put on its stock and price difference accounts, in the same order
// (ledger.Entry.Stock). A refusal posts nothing.
type PostEntry func(bookings []ledger.GRIRStock) (ledger.Entry, []ledger.StockPosted, error)

// ReceiveForOrder posts a purchase order's goods receipt of stock items
// (101) as a material document: each item checked as a receipt's is (an
// active material kept at the storage location of a plant of the
// company), its quantity converted to the base unit exactly, the
// valuations and stock locked, then the entry posted by post with the
// stock's part of each item, a moving-priced receipt taking what its
// GR/IR line moves, a standard-priced one its quantity at the price and
// the rest a price difference. An item without a valuation is refused
// (422 inventory.no_valuation) before anything is posted.
func (e *Endpoints) ReceiveForOrder(ctx context.Context, in OrderReceipt, post PostEntry) (Document, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Document{}, err
	}
	items := make([]ItemInput, len(in.Items))
	for i, it := range in.Items {
		items[i] = ItemInput{MaterialID: it.MaterialID, PlantID: it.PlantID, StorageLocationID: it.StorageLocationID, Quantity: it.Quantity, Unit: it.Unit}
	}
	r, err := e.inputRefs(ctx, items)
	if err != nil {
		return Document{}, err
	}
	company, lines, err := e.items(items, movementTypes["101"], r)
	if err != nil {
		return Document{}, err
	}
	if company != in.Company {
		return Document{}, problem.Invalid("inventory.mixed_companies", "items", "A goods receipt moves the stock of its own company's plants.")
	}
	c, err := e.companyOf(ctx, company)
	if err != nil {
		return Document{}, err
	}
	d, err := e.writeOrder(ctx, tx, p, posting{company: c.le, movementType: "101", day: in.Day, reference: &in.Reference, lines: lines}, r, post)
	if err != nil {
		return Document{}, err
	}
	return d, e.record(ctx, "inventory.material_document.post", d, r, map[string]any{"has_header_text": false})
}

// ReverseForOrder posts the reversal (102) of a purchase order's goods
// receipt's material document, once, on the reversal's posting date with
// its number as the reference: its lines negated, last first, each valued
// out of its stock as any issue is, against what its original was valued
// at, the rest a price difference (Reverse), and posted by post.
func (e *Endpoints) ReverseForOrder(ctx context.Context, id uuid.UUID, day time.Time, reference string, post PostEntry) (Document, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Document{}, err
	}
	// As Reverse (original); procurement's own lock on the goods
	// movement is taken first, so none waits here in practice.
	orig, err := original(ctx, tx, id)
	if err != nil {
		return Document{}, err
	}
	if orig.MovementType != "101" || orig.ReversedByID != nil {
		// procurement reverses a goods movement once, and only its 101.
		return Document{}, fmt.Errorf("inventory: material document %s is a %s, reversed by %v: no goods receipt to reverse", orig.Number,
			orig.MovementType, orig.ReversedByID)
	}
	r, err := e.documentRefs(ctx, []Document{orig})
	if err != nil {
		return Document{}, err
	}
	c, err := e.companyOf(ctx, orig.LegalEntityID)
	if err != nil {
		return Document{}, err
	}
	d, err := e.writeOrder(ctx, tx, p, posting{company: c.le, movementType: "102", day: day, reversalOf: &orig.ID, reference: &reference,
		lines: negated(orig.Lines)}, r, post)
	if err != nil {
		return Document{}, err
	}
	return d, e.record(ctx, "inventory.material_document.reverse", d, r, map[string]any{"reversal_of_id": orig.ID, "reversal_of_number": orig.Number})
}

// writeOrder writes a goods receipt's or its reversal's document: it
// locks the valuations and stock, numbers it, values the lines whose
// value does not wait for the entry (a standard price's, and every line
// out of stock), has post post the entry with each item's booking, values
// a moving-priced receipt at what its GR/IR line moved, and writes the
// document naming the entry.
func (e *Endpoints) writeOrder(ctx context.Context, tx pgx.Tx, p reqctx.Principal, in posting, r refs, post PostEntry) (Document, error) {
	vals, err := e.lock(ctx, tx, p, in, r)
	if err != nil {
		return Document{}, err
	}
	n, err := e.numbered(ctx, in)
	if err != nil {
		return Document{}, err
	}
	bookings, err := e.bookings(ctx, in, vals, r)
	if err != nil {
		return Document{}, err
	}
	entry, posted, err := post(bookings)
	if err != nil {
		return Document{}, err
	}
	for i := range in.lines {
		l := &in.lines[i]
		sp := posted[l.ItemNo-1]
		if bookings[l.ItemNo-1].Amount == nil {
			v := vals[valuationKey{l.MaterialID, l.PlantID}]
			if l.Amount, err = v.move(money.ParseRat(l.Quantity), &sp.Stock); err != nil {
				return Document{}, lineErr(err, l, r)
			}
		}
		l.Difference = sp.Difference
	}
	return insertDocument(ctx, tx, p, in, n, entryOf(entry))
}

// bookings are each item's GR/IR booking, in item order (a goods
// receipt's document has one line per item): its valuation class's stock
// and price difference accounts, and what the stock takes when that does
// not wait for the entry, which values the line now.
func (e *Endpoints) bookings(ctx context.Context, in posting, vals map[valuationKey]*Valuation, r refs) ([]ledger.GRIRStock, error) {
	a := e.accounts(in.company.ID, in.company.Code)
	out := make([]ledger.GRIRStock, len(in.lines))
	for i := range in.lines {
		l := &in.lines[i]
		v := vals[valuationKey{l.MaterialID, l.PlantID}]
		b, err := a.stockBooking(ctx, v.ValuationClass)
		if err != nil {
			return nil, err
		}
		if q := money.ParseRat(l.Quantity); v.PriceControl == Standard || q.Sign() < 0 {
			if l.Amount, err = v.move(q, l.target); err != nil {
				return nil, lineErr(err, l, r)
			}
			b.Amount = &l.Amount
		}
		out[l.ItemNo-1] = b
	}
	return out, nil
}

// stockBooking is a booking on the valuation class's stock and price
// difference accounts, before what the stock takes is set.
func (a *accounts) stockBooking(ctx context.Context, class string) (ledger.GRIRStock, error) {
	stock, err := a.of(ctx, config.InventoryStock, class)
	if err != nil {
		return ledger.GRIRStock{}, err
	}
	diff, err := a.of(ctx, config.InventoryPriceDifference, class)
	if err != nil {
		return ledger.GRIRStock{}, err
	}
	return ledger.GRIRStock{AccountID: stock, DifferenceAccountID: diff}, nil
}

// InvoiceItem is a stock item a supplier invoice invoices: its order
// item's material in a plant, and the quantity of the order item's unit
// its price difference is spread over (the invoiced quantity, or what was
// received of the item when the invoice moves no quantity).
type InvoiceItem struct {
	MaterialID, PlantID uuid.UUID
	Quantity, Unit      string
}

// InvoiceSource is what posted an invoice's differences: the invoice, its
// order and the order item of each item in InvoiceDifferences' order.
type InvoiceSource struct {
	InvoiceID, PurchaseOrderID uuid.UUID
	POItemNos                  []int32
}

// InvoiceDifferences posts a supplier invoice of stock items through post
// (bookings one per item, in order) under its items' valuations' locks
// (ADR-0008: taken before the entry's, as a goods receipt takes them), so
// that what each puts on its stock is the share of its price difference
// the stock still holds (SAP's stock coverage, from memory): at a moving
// price the part of its quantity the valuation's stock holds, at most
// all, none at a standard price, where the whole difference goes to the
// class's price difference account. What the stock took is added to the
// valuation's value, its moving price following (inventory.invoice_difference);
// a value below zero is refused (409 inventory.stock_value_below_zero)
// and posts nothing, the request's transaction rolled back.
func (e *Endpoints) InvoiceDifferences(ctx context.Context, company uuid.UUID, items []InvoiceItem, src InvoiceSource, post PostEntry) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	ms, units := make([]uuid.UUID, len(items)), make([]uuid.UUID, len(items))
	lines := make([]Line, len(items))
	for i, it := range items {
		ms[i], units[i] = it.MaterialID, it.PlantID
		lines[i] = Line{MaterialID: it.MaterialID, PlantID: it.PlantID}
	}
	r, err := e.read(ctx, ms, units, nil, nil)
	if err != nil {
		return err
	}
	vals, err := lockValuations(ctx, tx, lines, r)
	if err != nil {
		return err
	}
	c, err := e.companyOf(ctx, company)
	if err != nil {
		return err
	}
	bookings, err := e.invoiceBookings(ctx, c, items, vals, r)
	if err != nil {
		return err
	}
	entry, posted, err := post(bookings)
	if err != nil {
		return err
	}
	return insertDifferences(ctx, tx, p, items, src, vals, entry, posted)
}

// invoiceBookings are the invoice items' bookings: the stock's share of
// each price difference, of the quantity its valuation holds before the
// invoice, which the items of one valuation cover in turn.
func (e *Endpoints) invoiceBookings(ctx context.Context, c companyOf, items []InvoiceItem, vals map[valuationKey]*Valuation, r refs) ([]ledger.GRIRStock,
	error) {
	a := e.accounts(c.le.ID, c.le.Code)
	held := map[valuationKey]*big.Rat{}
	out := make([]ledger.GRIRStock, len(items))
	for i, it := range items {
		k := valuationKey{it.MaterialID, it.PlantID}
		v := vals[k]
		b, err := a.stockBooking(ctx, v.ValuationClass)
		if err != nil {
			return nil, err
		}
		if held[k] == nil {
			held[k] = v.qty()
		}
		share, err := coverage(r.materials[it.MaterialID], it, v, held[k])
		if err != nil {
			return nil, err
		}
		b.Share = share
		out[i] = b
	}
	return out, nil
}

// coverage is the part of an item's quantity the stock still held covers,
// from 0 to 1, taking it from held: none at a standard price or of no
// quantity.
func coverage(m materials.Material, it InvoiceItem, v *Valuation, held *big.Rat) (*big.Rat, error) {
	q := money.ParseRat(it.Quantity)
	if v.PriceControl != MovingAverage || q.Sign() <= 0 {
		return new(big.Rat), nil
	}
	f, err := m.Factor(it.Unit)
	if err != nil { // the order item's unit, which its material's conversions convert (procurement)
		return nil, fmt.Errorf("inventory: invoiced quantity of material %s: %w", m.Number, err)
	}
	base := new(big.Rat).Mul(q, f)
	covered := new(big.Rat).Set(held)
	if covered.Cmp(base) > 0 {
		covered.Set(base)
	}
	held.Sub(held, covered)
	return covered.Quo(covered, base), nil
}

// insertDifferences records what each item put on its stock, each adding
// it to its valuation (inventory.invoice_difference_valuation), refusing
// one that would take the stock's value below zero (a credit far below
// the order price of a stock valued lower since; valuation_value_check
// refuses it too).
func insertDifferences(ctx context.Context, tx pgx.Tx, p reqctx.Principal, items []InvoiceItem, src InvoiceSource, vals map[valuationKey]*Valuation,
	entry ledger.Entry, posted []ledger.StockPosted) error {
	for i, it := range items {
		if posted[i].Stock == 0 {
			continue
		}
		v := vals[valuationKey{it.MaterialID, it.PlantID}]
		value, err := money.Sum(v.Value, posted[i].Stock)
		if err != nil || value < 0 {
			return problem.Conflict("inventory.stock_value_below_zero", fmt.Sprintf("Invoice item %d's price difference of %s would take the stock value of "+
				"its material in the plant, %s, out of range or below zero; post it as a credit within what the stock is worth.", i+1,
				posted[i].Stock.Plain(v.Exponent), v.Value.Plain(v.Exponent)))
		}
		v.Value = value
		if _, err := tx.Exec(ctx, `INSERT INTO inventory.invoice_difference (tenant_id, id, valuation_id, supplier_invoice_id, purchase_order_id,
				po_item_no, amount, journal_entry_id, journal_entry_year, journal_entry_number, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			p.TenantID, uuid.Must(uuid.NewV7()), v.ID, src.InvoiceID, src.PurchaseOrderID, src.POItemNos[i], posted[i].Stock, entry.ID, entry.FiscalYear,
			entry.DocumentNumber, p.ID); err != nil {
			return fmt.Errorf("inventory: price difference of invoice %s on valuation %s: %w", src.InvoiceID, v.ID, err)
		}
	}
	return nil
}

// RevaluedStock says whether a supplier invoice put a price difference on
// a stock (InvoiceDifferences): its reversal would take it off a stock
// that may have moved since, so payables refuses it.
func (e *Endpoints) RevaluedStock(ctx context.Context, invoice uuid.UUID) (bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return false, err
	}
	var found bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM inventory.invoice_difference WHERE supplier_invoice_id = $1)`, invoice).Scan(&found); err != nil {
		return false, fmt.Errorf("inventory: price differences of invoice %s: %w", invoice, err)
	}
	return found, nil
}
