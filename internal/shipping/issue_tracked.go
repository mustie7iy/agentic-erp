package shipping

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/inventory"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/sales"
)

// Batches and serial numbers in a delivery's goods issue (S2f2, S2g2;
// ADR-0083 As built S2f2): the person posting it names, for each picked
// item of a batch-managed or serial-managed material, what its picked
// quantity issues: one entry per batch it leaves from (SAP's batch split,
// from memory), each with the serial numbers of its units, or one with the
// serial numbers of a material without batches. Each entry is an item of
// the 601 inventory posts, which checks the batch and the serial numbers
// against the stock at the storage location under its locks; the
// delivery's item keeps what each of its lines issued
// (shipping.outbound_delivery_issue_line), and the reversal (602) gives
// the same back. Every other picked item issues its picked quantity as
// one line, as before.

// IssueInput is what a goods issue issues of the delivery's item ItemNo,
// or part of it: a quantity in the item's unit, from the batch numbered
// Batch ("" for a material without batches), with the serial numbers of
// its units (none for a material without them).
type IssueInput struct {
	ItemNo         int32
	Quantity, Unit string
	Batch          string
	SerialNumbers  []string
}

// IssueLine is what a line of a delivery's goods issue issued of its item
// of a batch-managed or serial-managed material: the 601's line, the
// quantity in the item's unit, the batch by id and number (nil for a
// material without batches) and its units' serial numbers, sorted (nil
// for a material without them).
type IssueLine struct {
	LineNo        int16      `json:"line_no"`
	Quantity      string     `json:"quantity"`
	BatchID       *uuid.UUID `json:"batch_id"`
	BatchNumber   *string    `json:"batch_number"`
	SerialNumbers []string   `json:"serial_numbers"`
}

// issuePlan is a goods issue's items as inventory posts them, in the
// material document's item order, and where each came from: src[k] is
// items[k]'s delivery item (an index of the delivery's items) and the
// request's entry naming it, -1 for an item issued whole, which the
// request does not name.
type issuePlan struct {
	items []inventory.DeliveryItem
	src   []issueSource
}

type issueSource struct{ item, entry int }

// issuePlan checks in against the delivery (issueEntries) and makes the
// goods issue's items: each picked item, in item order, as its entries
// name it, or whole when it has none.
func (e *Endpoints) issuePlan(ctx context.Context, d Delivery, in []IssueInput) (issuePlan, error) {
	ids := make([]uuid.UUID, len(d.Items))
	for i, it := range d.Items {
		ids[i] = it.MaterialID
	}
	ms, err := e.Materials.Materials(ctx, ids)
	if err != nil {
		return issuePlan{}, fmt.Errorf("shipping: materials of outbound delivery %s: %w", d.Number, err)
	}
	byItem, err := issueEntries(d, ms, in)
	if err != nil {
		return issuePlan{}, err
	}
	var plan issuePlan
	for i, it := range d.Items {
		if money.ParseRat(it.Picked).Sign() == 0 { // issueEntries refused an entry naming it
			continue
		}
		item := inventory.DeliveryItem{MaterialID: it.MaterialID, PlantID: d.PlantID, StorageLocationID: d.StorageLocationID, Quantity: it.Picked,
			Unit: it.Unit}
		if len(byItem[it.ItemNo]) == 0 {
			plan.items, plan.src = append(plan.items, item), append(plan.src, issueSource{i, -1})
			continue
		}
		for _, j := range byItem[it.ItemNo] {
			item.Quantity, item.Batch, item.SerialNumbers = in[j].Quantity, in[j].Batch, in[j].SerialNumbers
			plan.items, plan.src = append(plan.items, item), append(plan.src, issueSource{i, j})
		}
	}
	return plan, nil
}

// issueEntries checks the request's entries, naming items[j]: each of an
// item the delivery has (422 shipping.unknown_item) of a batch-managed or
// serial-managed material (shipping.item_not_tracked), a quantity more
// than zero (shipping.invalid_quantity) in the item's unit
// (shipping.unit_mismatch); an item's entries add up to its picked
// quantity (shipping.issue_quantity_mismatch), and a picked item of such a
// material is named (shipping.tracking_required). It answers each item's
// entries by item number, in the request's order.
func issueEntries(d Delivery, ms map[uuid.UUID]materials.Material, in []IssueInput) (map[int32][]int, error) {
	byItem, sums := map[int32][]int{}, map[int32]*big.Rat{}
	for j, it := range in {
		field := fmt.Sprintf("items[%d]", j)
		if it.ItemNo < 1 || int(it.ItemNo) > len(d.Items) {
			return nil, problem.Invalid("shipping.unknown_item", field+".item_no", fmt.Sprintf("Outbound delivery %s has no item %d.", d.Number, it.ItemNo))
		}
		item := d.Items[it.ItemNo-1] // items are numbered from 1 in order (insert)
		m := ms[item.MaterialID]
		switch {
		case !tracked(m):
			return nil, problem.Invalid("shipping.item_not_tracked", field+".item_no", fmt.Sprintf("Item %d of outbound delivery %s is material %s, "+
				"managed in no batches and without serial numbers; it issues its picked quantity, unnamed.", it.ItemNo, d.Number, m.Number))
		case money.PositiveQuantity(it.Quantity) != nil:
			return nil, problem.Invalid("shipping.invalid_quantity", field+".quantity.value", fmt.Sprintf(
				"A quantity issued is a plain decimal more than zero of at most %d integer digits and %d decimals.", money.QuantityIntDigits,
				money.QuantityDecimals))
		case it.Unit != item.Unit:
			return nil, problem.Invalid("shipping.unit_mismatch", field+".quantity.unit", fmt.Sprintf("Item %d of outbound delivery %s is in %s; "+
				"it is issued in that unit.", it.ItemNo, d.Number, item.Unit))
		}
		if sums[it.ItemNo] == nil {
			sums[it.ItemNo] = new(big.Rat)
		}
		byItem[it.ItemNo] = append(byItem[it.ItemNo], j)
		sums[it.ItemNo].Add(sums[it.ItemNo], money.ParseRat(it.Quantity))
	}
	for _, it := range d.Items {
		picked, entries := money.ParseRat(it.Picked), byItem[it.ItemNo]
		switch m := ms[it.MaterialID]; {
		case len(entries) == 0 && picked.Sign() > 0 && tracked(m):
			return nil, problem.Invalid("shipping.tracking_required", "items", fmt.Sprintf("Item %d of outbound delivery %s is material %s, %s; "+
				"its goods issue names what its %s %s picked issue (items with item_no %d).", it.ItemNo, d.Number, m.Number, tracking(m), it.Picked,
				it.Unit, it.ItemNo))
		case len(entries) > 0 && sums[it.ItemNo].Cmp(picked) != 0:
			return nil, problem.Invalid("shipping.issue_quantity_mismatch", fmt.Sprintf("items[%d].quantity.value", entries[0]), fmt.Sprintf(
				"Item %d of outbound delivery %s has %s %s picked; its entries issue %s %s, which is not that.", it.ItemNo, d.Number, it.Picked, it.Unit,
				money.PlainQuantity(sums[it.ItemNo]), it.Unit))
		}
	}
	return byItem, nil
}

// tracked says whether a material's goods issue names its batches or
// serial numbers.
func tracked(m materials.Material) bool { return m.BatchManaged || m.SerialManaged }

// tracking says what a tracked material's goods issue names.
func tracking(m materials.Material) string {
	switch {
	case m.BatchManaged && m.SerialManaged:
		return "managed in batches and with a serial number profile, so its goods issue names its batches and its units' serial numbers"
	case m.BatchManaged:
		return "managed in batches, so its goods issue names the batches it leaves from"
	}
	return "with a serial number profile, so its goods issue names its units' serial numbers"
}

// asRequest names a refusal of the material document's item k by the
// request's entry it is: items[j] for an entry j, with its quantity's
// value and its item number for inventory's quantity and material; a
// field of an item the request does not name is left out, as the request
// has none.
func (p issuePlan) asRequest(err error) error {
	return problem.Renamed(err, func(field string) (string, bool) {
		for k, s := range p.src {
			rest, ok := strings.CutPrefix(field, fmt.Sprintf("items[%d].", k))
			switch {
			case !ok:
				continue
			case s.entry < 0:
				return "", false
			case rest == "quantity":
				rest = "quantity.value"
			case rest == "material_id" || rest == "plant_id" || rest == "storage_location_id":
				rest = "item_no"
			}
			return fmt.Sprintf("items[%d].%s", s.entry, rest), true
		}
		return field, true
	})
}

// costs gives each issued item its cost, what its lines took from the
// stock: one line per item of the material document (an issue's item
// moves one location).
func (p issuePlan) costs(doc inventory.Document, items []sales.IssuedItem) {
	for _, l := range doc.Lines {
		i := p.src[l.ItemNo-1].item
		items[i].Cost -= l.Amount // the lines' sum was posted in one WA entry, whose amounts the ledger keeps within an amount's range
	}
}

// writeLines writes what each line of the goods issue issued of an item
// the request named, with its batch and serial numbers as inventory
// moved them (a serial number has no comma, so a line's are sent joined).
func (p issuePlan) writeLines(ctx context.Context, tx pgx.Tx, d Delivery, doc inventory.Document) error {
	var lineNo []int16
	var itemNo []int32
	var quantities []string
	var batches []*uuid.UUID
	var numbers, serials []*string
	for _, l := range doc.Lines {
		s := p.src[l.ItemNo-1]
		if s.entry < 0 {
			continue
		}
		var joined *string
		if len(l.SerialNumbers) > 0 {
			j := strings.Join(l.SerialNumbers, ",")
			joined = &j
		}
		lineNo, itemNo, quantities = append(lineNo, l.LineNo), append(itemNo, d.Items[s.item].ItemNo), append(quantities, l.EntryQuantity)
		batches, numbers, serials = append(batches, l.BatchID), append(numbers, l.BatchNumber), append(serials, joined)
	}
	if len(lineNo) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO shipping.outbound_delivery_issue_line (tenant_id, outbound_delivery_id, line_no, item_no, quantity, batch_id,
			batch_number, serial_numbers)
		SELECT d.tenant_id, d.id, x.l, x.i, x.q, x.b, x.n, string_to_array(x.s, ',') FROM shipping.outbound_delivery d,
			unnest($2::smallint[], $3::integer[], $4::numeric[], $5::uuid[], $6::text[], $7::text[]) AS x (l, i, q, b, n, s)
		WHERE d.id = $1 ORDER BY x.l`, d.ID, lineNo, itemNo, quantities, batches, numbers, serials); err != nil {
		return fmt.Errorf("shipping: goods issue lines of outbound delivery %s: %w", d.Number, err)
	}
	return nil
}
