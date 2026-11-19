package inventory

import (
	"context"
	"fmt"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// CountEntry is the quantity counted of an item, as a request enters it.
type CountEntry struct {
	ItemNo         int
	Quantity, Unit string
}

// counting is an item's count checked: in the base unit and as entered.
type counting struct {
	itemNo               int16
	counted, entry, unit string
}

// EnterCount enters the quantities counted of a counting document's items
// (ADR-0082 As built S2c; SAP MI04) at the caller's version: each item
// counted once, in a unit of its material converted exactly to the base
// unit, zero included. Once every item is counted the document is
// counted. The document's row lock serializes entries; a material blocked
// since is still counted.
func (e *Endpoints) EnterCount(ctx context.Context, id uuid.UUID, ifMatch func(CountDocument) error, entries []CountEntry) (CountDocument, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return CountDocument{}, err
	}
	d, err := loadCount(ctx, tx, id, true)
	if err != nil {
		return CountDocument{}, err
	}
	if err := ifMatch(d); err != nil {
		return CountDocument{}, err
	}
	if d.Status != CountCounting {
		return CountDocument{}, problem.Conflict("inventory.count_invalid_status", "Count document "+d.Number+" is "+d.Status+"; counts are entered "+
			"while it is counting.")
	}
	r, err := e.documentCountRefs(ctx, []CountDocument{d})
	if err != nil {
		return CountDocument{}, err
	}
	counts, err := checkCounts(d, entries, r)
	if err != nil {
		return CountDocument{}, err
	}
	if d, err = writeCounts(ctx, tx, p, d, counts); err != nil {
		return CountDocument{}, err
	}
	return d, recordCount(ctx, "inventory.count_document.count", d, r)
}

// checkCounts checks the entries against the document's items: each an
// item of the document, once in the request and not counted before, its
// quantity zero or more in a unit of its material and exact in the base
// unit.
func checkCounts(d CountDocument, entries []CountEntry, r refs) ([]counting, error) {
	byNo := make(map[int]CountItem, len(d.Items))
	for _, it := range d.Items {
		byNo[int(it.ItemNo)] = it
	}
	seen := map[int]bool{}
	out := make([]counting, 0, len(entries))
	for i, en := range entries {
		at := fmt.Sprintf("items[%d].", i)
		it, ok := byNo[en.ItemNo]
		switch {
		case !ok:
			return nil, problem.Invalid("inventory.unknown_item", at+"item_no", fmt.Sprintf("Count document %s has no item %d.", d.Number, en.ItemNo))
		case seen[en.ItemNo]:
			return nil, problem.Invalid("inventory.duplicate_item", at+"item_no", fmt.Sprintf("Item %d is entered once in a request.", en.ItemNo))
		case it.Counted != nil:
			return nil, problem.Conflict("inventory.item_counted", fmt.Sprintf("Item %d of count document %s is counted already (%s %s); a recount is "+
				"a new document.", en.ItemNo, d.Number, *it.Counted, r.materials[it.MaterialID].BaseUnit))
		}
		seen[en.ItemNo] = true
		base, err := countedQuantity(at+"quantity", r.materials[it.MaterialID], en.Quantity, en.Unit)
		if err != nil {
			return nil, err
		}
		out = append(out, counting{itemNo: it.ItemNo, counted: money.PlainQuantity(base), entry: en.Quantity, unit: en.Unit})
	}
	return out, nil
}

// countedQuantity is a quantity counted in the material's base unit: a
// plain decimal of zero or more (zero is a count too: nothing was found)
// in one of its units, converted exactly.
func countedQuantity(field string, m materials.Material, quantity, unit string) (*big.Rat, error) {
	q, err := money.ParsePlain(quantity)
	if err != nil || (q.Sign() > 0 && money.PositiveQuantity(quantity) != nil) {
		return nil, problem.Invalid("inventory.invalid_quantity", field+".value", fmt.Sprintf("a plain decimal quantity of zero or more, of at most %d "+
			"integer digits and %d decimals", money.QuantityIntDigits, money.QuantityDecimals))
	}
	return converted(field, m, q, quantity, unit)
}

// writeCounts writes the counts, moves the document to counted once every
// item is, and raises its version; it reads the document back.
func writeCounts(ctx context.Context, tx pgx.Tx, p reqctx.Principal, d CountDocument, counts []counting) (CountDocument, error) {
	nos, counted, entry, units := make([]int16, len(counts)), make([]string, len(counts)), make([]string, len(counts)), make([]string, len(counts))
	for i, c := range counts {
		nos[i], counted[i], entry[i], units[i] = c.itemNo, c.counted, c.entry, c.unit
	}
	if _, err := tx.Exec(ctx, `UPDATE inventory.count_item i SET counted_quantity = x.c::numeric, entry_quantity = x.e::numeric, entry_uom_code = x.u,
			counted_at = now(), counted_by = $2
		FROM unnest($3::smallint[], $4::text[], $5::text[], $6::text[]) AS x (n, c, e, u)
		WHERE i.count_document_id = $1 AND i.item_no = x.n AND i.counted_quantity IS NULL`, d.ID, p.ID, nos, counted, entry, units); err != nil {
		return CountDocument{}, fmt.Errorf("inventory: counts of count document %s: %w", d.Number, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE inventory.count_document d SET version = version + 1, updated_by = $2,
			status = CASE WHEN NOT EXISTS (SELECT FROM inventory.count_item i WHERE i.tenant_id = d.tenant_id AND i.count_document_id = d.id
				AND i.counted_quantity IS NULL) THEN 'counted' ELSE status END
		WHERE d.id = $1`, d.ID, p.ID); err != nil {
		return CountDocument{}, fmt.Errorf("inventory: count document %s counted: %w", d.Number, err)
	}
	return loadCount(ctx, tx, d.ID, false)
}

// documentCountRefs reads what the count documents name: their materials,
// plants, storage locations and companies.
func (e *Endpoints) documentCountRefs(ctx context.Context, ds []CountDocument) (refs, error) {
	var ms, units, companies []uuid.UUID
	for _, d := range ds {
		ms, units, companies = append(ms, d.materialIDs()...), append(units, d.PlantID, d.StorageLocationID), append(companies, d.LegalEntityID)
	}
	return e.read(ctx, ms, units, nil, companies)
}
