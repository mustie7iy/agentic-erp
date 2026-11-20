package inventory

import (
	"context"
	"fmt"
	"math/big"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Stock transport orders within a company (S2h; ADR-0082 As built S2h):
// procurement posts an order's goods issue (351), which takes the stock
// out of the supplying storage location and puts it in transit to the
// receiving one, valued in the receiving plant as a transfer between
// plants is (SAP's 351 posts the stock accounts of both plants, from
// memory), and its goods receipt (101), which takes the stock out of
// transit into the receiving storage location, within the receiving
// plant's valuation and so posting nothing, as a 311 does. Stock in
// transit is a line in_transit (inventory.transit_stock), neither
// location's stock. Neither document is reversed yet (409
// inventory.reverse_stock_transport).

func init() {
	// takeStock refuses taking more out of transit than is in it first,
	// naming it; this is the database's answer should one pass it.
	problem.RegisterConstraint("transit_stock_not_negative", func() *problem.Problem {
		return problem.Conflict("inventory.insufficient_stock", "A goods movement would take a stock in transit below zero.")
	})
}

// transportReceipt is the goods receipt of a stock transport order: a
// 101 that moves stock from transit into its storage location, as a
// transfer between storage locations of one plant does.
var transportReceipt = movementType{name: "Goods receipt for a stock transport order", sign: -1, transfer: betweenStorageLocations, transport: true}

// TransportItem is an item of a stock transport order's goods issue or
// receipt: a positive quantity of a material in one of its units, from a
// storage location of the supplying plant to one of the receiving plant.
// BaseQuantity is the order's quantity in the material's base unit, as it
// was converted when the order was created: the goods issue and receipt
// move exactly that, whatever an alternative unit's factor is since (a
// material change may change or remove it), so the receipt takes out of
// transit what the issue put in; Quantity and Unit are what each
// document's lines show as entered. TransportQuantities, which converts
// it, takes none.
type TransportItem struct {
	MaterialID                                 uuid.UUID
	SupplyingPlantID, SupplyingStorageLocation uuid.UUID
	ReceivingPlantID, ReceivingStorageLocation uuid.UUID
	Quantity, Unit, BaseQuantity               string
}

// IssueForTransport posts a stock transport order's goods issue (351) as
// a material document in the request's transaction, its reference the
// order's number: each item checked as a transfer between plants is (an
// active material kept at both storage locations, both plants of one
// company the caller may act in and the material extended to both), its
// quantity converted to the base unit exactly, posted on the company's
// date today, the valuations and the stock locked and refused below zero
// or while a count blocks the supplying location, the issue valued as an
// issue is and the stock in transit at what it took, at the receiving
// plant's standard price with the rest a price difference, and posted in
// one WA entry between the classes' stock accounts. The audit record and
// MaterialDocumentPosted are written in the request's transaction.
func (e *Endpoints) IssueForTransport(ctx context.Context, reference string, in []TransportItem) (Document, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Document{}, err
	}
	company, lines, items, r, err := e.issueLines(ctx, in)
	if err != nil {
		return Document{}, err
	}
	return e.postTransport(ctx, tx, p, posting{movementType: "351", reference: &reference, lines: lines, items: items}, company, r)
}

// TransportQuantities checks a stock transport order's items as its goods
// issue checks them (IssueForTransport), moving nothing and reading no
// stock or valuation, and answers the plants' company and each item's
// quantity in its material's base unit, exactly.
func (e *Endpoints) TransportQuantities(ctx context.Context, in []TransportItem) (uuid.UUID, []string, error) {
	company, lines, _, _, err := e.issueLines(ctx, in)
	if err != nil {
		return uuid.Nil, nil, err
	}
	base := make([]string, len(in))
	for _, l := range lines {
		if l.InTransit {
			base[l.ItemNo-1] = l.Quantity
		}
	}
	return company, base, nil
}

// issueLines are the lines of a stock transport order's goods issue, each
// item checked as a transfer between plants (301) is: its issue from the
// supplying storage location, then its receipt in transit to the
// receiving one. An item with its base quantity moves that, in the base
// unit, and its lines show the quantity and unit it was ordered in.
func (e *Endpoints) issueLines(ctx context.Context, in []TransportItem) (uuid.UUID, []Line, []ItemInput, refs, error) {
	items := make([]ItemInput, len(in))
	for i, it := range in {
		items[i] = ItemInput{MaterialID: it.MaterialID, PlantID: it.SupplyingPlantID, StorageLocationID: it.SupplyingStorageLocation, Quantity: it.Quantity,
			Unit: it.Unit, ToPlantID: &in[i].ReceivingPlantID, ToStorageLocationID: &in[i].ReceivingStorageLocation}
	}
	r, err := e.inputRefs(ctx, items)
	if err != nil {
		return uuid.Nil, nil, nil, refs{}, err
	}
	for i, it := range in {
		if m, ok := r.materials[it.MaterialID]; ok && it.BaseQuantity != "" {
			items[i].Quantity, items[i].Unit = it.BaseQuantity, m.BaseUnit
		}
	}
	company, lines, err := e.items(items, movementTypes["351"], r)
	if err != nil {
		return uuid.Nil, nil, nil, refs{}, err
	}
	for i := range lines {
		it := in[lines[i].ItemNo-1]
		lines[i].EntryQuantity, lines[i].EntryUnit = it.Quantity, it.Unit
		lines[i].InTransit = money.ParseRat(lines[i].Quantity).Sign() > 0 // each item's receiving line
	}
	return company, lines, items, r, nil
}

// ReceiveForTransport posts a stock transport order's goods receipt (101)
// as a material document in the request's transaction, its reference the
// order's number: each item's base quantity, what its goods issue put in
// transit, leaves the stock in transit to its receiving storage location
// (409 inventory.insufficient_stock when less is in transit) and enters
// that location's stock, the location active and the material kept
// there; a material blocked since the issue is still received, as its
// stock is the company's already. Within the receiving plant's valuation
// it moves no value and posts nothing.
func (e *Endpoints) ReceiveForTransport(ctx context.Context, reference string, in []TransportItem) (Document, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Document{}, err
	}
	items := make([]ItemInput, len(in))
	for i, it := range in {
		items[i] = ItemInput{MaterialID: it.MaterialID, PlantID: it.ReceivingPlantID, StorageLocationID: it.ReceivingStorageLocation, Quantity: it.Quantity,
			Unit: it.Unit}
	}
	r, err := e.inputRefs(ctx, items)
	if err != nil {
		return Document{}, err
	}
	company, lines, err := receiptLines(in, items, r)
	if err != nil {
		return Document{}, err
	}
	return e.postTransport(ctx, tx, p, posting{movementType: "101", reference: &reference, lines: lines, items: items, kind: &transportReceipt}, company, r)
}

// postTransport posts a stock transport order's document on its
// company's date today and records it.
func (e *Endpoints) postTransport(ctx context.Context, tx pgx.Tx, p reqctx.Principal, in posting, company uuid.UUID, r refs) (Document, error) {
	c, err := e.companyOf(ctx, company)
	if err != nil {
		return Document{}, err
	}
	in.company, in.day = c.le, c.day.Date
	d, err := e.write(ctx, tx, p, in, r)
	if err != nil {
		return Document{}, err
	}
	return d, e.record(ctx, "inventory.material_document.post", d, r, map[string]any{"has_header_text": false})
}

// receiptLines are the lines of a stock transport order's goods receipt:
// for each item, its base quantity in transit to its storage location
// taken out and then put into the location, both at the receiving plant.
func receiptLines(in []TransportItem, items []ItemInput, r refs) (uuid.UUID, []Line, error) {
	var company uuid.UUID
	var lines []Line
	for i, it := range items {
		at := fmt.Sprintf("items[%d].", i)
		m, ok := r.materials[it.MaterialID]
		if !ok {
			return uuid.Nil, nil, problem.Invalid("inventory.material_invalid", at+"material_id", "No material with this id.")
		}
		c, err := location(at+"plant_id", at+"storage_location_id", m, it.PlantID, it.StorageLocationID, r)
		if err != nil {
			return uuid.Nil, nil, err
		}
		if company != uuid.Nil && c != company {
			return uuid.Nil, nil, problem.Invalid("inventory.mixed_companies", at+"plant_id",
				"A material document moves the stock of one company; this plant is of another than the items before it.")
		}
		company = c
		base, err := baseQuantity(at+"quantity", m, in[i].BaseQuantity, m.BaseUnit)
		if err != nil {
			return uuid.Nil, nil, err
		}
		in := Line{ItemNo: int16(i + 1), MaterialID: m.ID, PlantID: it.PlantID, StorageLocationID: it.StorageLocationID, EntryQuantity: it.Quantity,
			EntryUnit: it.Unit, Quantity: money.PlainQuantity(base)} // at most 100 items (the contract's)
		out := in
		out.InTransit, out.Quantity = true, money.PlainQuantity(new(big.Rat).Neg(base))
		out.LineNo, in.LineNo = int16(len(lines)+1), int16(len(lines)+2)
		lines = append(lines, out, in)
	}
	return company, lines, nil
}

// transport says the document moves stock in transit: a stock transport
// order's goods issue or receipt. The in_transit lines are the only mark
// of it a 101 carries (a purchase order's 101 has none), so a document is
// told by them alone; every line a transport document posts with a line in
// transit beside it (issueLines, receiptLines).
func (d Document) transport() bool {
	return slices.ContainsFunc(d.Lines, func(l Line) bool { return l.InTransit })
}

// typeName is the name of the document's movement type: a stock
// transport order's goods receipt is a 101 of its own.
func (d Document) typeName() string {
	if d.MovementType == "101" && d.transport() {
		return transportReceipt.name
	}
	return movementTypes[d.MovementType].name
}
