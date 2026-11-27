package inventory

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// maxCounts is how often an item is counted at most: its first count and
// 98 recounts (count_item_recounts_check).
const maxCounts = 99

// Recount sends counted items of a count document for a recount at the
// caller's version (ADR-0082 As built S2d2; SAP MI11, from memory): each
// item's count is kept as an earlier count, numbered among the item's
// counts, with who sent it for the recount, and the item is open to be
// counted again (EnterCount), so only its latest count is posted
// (PostDifferences). The document is counting until every item is counted
// again. Only a counting or counted document is recounted: one whose
// differences are posted, wait for approval, or were rejected or
// withdrawn is counted by a new document. The document's row lock
// serializes a recount with the entries and the posting; the posting
// block holds throughout.
func (e *Endpoints) Recount(ctx context.Context, id uuid.UUID, ifMatch func(CountDocument) error, itemNos []int) (CountDocument, error) {
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
	if d.Status != CountCounting && d.Status != CountCounted {
		return CountDocument{}, problem.Conflict("inventory.count_invalid_status", "Count document "+d.Number+" is "+d.Status+"; its items are "+
			"recounted while it is counting or counted, before its differences are posted or sent for approval; a new document counts them now.")
	}
	r, err := e.documentCountRefs(ctx, []CountDocument{d})
	if err != nil {
		return CountDocument{}, err
	}
	set, err := checkRecounts(d, itemNos, r)
	if err != nil {
		return CountDocument{}, err
	}
	name, err := e.personName(ctx, p, "count document "+d.Number)
	if err != nil {
		return CountDocument{}, err
	}
	recounted, err := writeRecounts(ctx, tx, p, name, d, set)
	if err != nil {
		return CountDocument{}, err
	}
	return recounted, recordRecount(ctx, recounted, set, r)
}

// checkRecounts checks the items to recount against the document's: each
// an item of it, once in the request, counted, and counted fewer than
// maxCounts times. It answers them as they are, their counts to set
// aside, in the request's order.
func checkRecounts(d CountDocument, itemNos []int, r refs) ([]CountItem, error) {
	byNo := make(map[int]CountItem, len(d.Items))
	for _, it := range d.Items {
		byNo[int(it.ItemNo)] = it
	}
	seen := map[int]bool{}
	out := make([]CountItem, 0, len(itemNos))
	for i, no := range itemNos {
		field := fmt.Sprintf("items[%d].item_no", i)
		it, ok := byNo[no]
		switch {
		case !ok:
			return nil, problem.Invalid("inventory.unknown_item", field, fmt.Sprintf("Count document %s has no item %d.", d.Number, no))
		case seen[no]:
			return nil, problem.Invalid("inventory.duplicate_item", field, fmt.Sprintf("Item %d is recounted once in a request.", no))
		case it.Counted == nil:
			return nil, problem.Conflict("inventory.item_not_counted", fmt.Sprintf("Item %d (material %s) of count document %s is not counted yet; it "+
				"is recounted once its count is entered.", no, r.materials[it.MaterialID].Number, d.Number))
		case it.CountNo() >= maxCounts:
			return nil, problem.Conflict("inventory.recount_limit", fmt.Sprintf("Item %d (material %s) of count document %s is counted %d times, "+
				"the most an item is; post or have its differences decided and count it by a new document.", no, r.materials[it.MaterialID].Number,
				d.Number, maxCounts))
		}
		seen[no] = true
		out = append(out, it)
	}
	return out, nil
}

// writeRecounts keeps each item's count as its earlier count, copied in
// the database as it is stored, takes the count off the item and raises
// its recounts (count_item_once lets nothing else take a count off), and
// moves the document back to counting at a new version. It reads the
// document back. The caller holds the document's row lock, which every
// writer of its items takes first.
func writeRecounts(ctx context.Context, tx pgx.Tx, p reqctx.Principal, name string, d CountDocument, set []CountItem) (CountDocument, error) {
	nos := make([]int16, len(set))
	for i, it := range set {
		nos[i] = it.ItemNo
	}
	if _, err := tx.Exec(ctx, `INSERT INTO inventory.count_item_recount (tenant_id, count_document_id, item_no, count_no, counted_quantity, entry_quantity,
			entry_uom_code, counted_at, counted_by, counted_by_name, recounted_by, recounted_by_name)
		SELECT i.tenant_id, i.count_document_id, i.item_no, i.recounts + 1, i.counted_quantity, i.entry_quantity, i.entry_uom_code, i.counted_at,
			i.counted_by, i.counted_by_name, $2, $4
		FROM inventory.count_item i WHERE i.count_document_id = $1 AND i.item_no = ANY($3) ORDER BY i.item_no`, d.ID, p.ID, nos, name); err != nil {
		return CountDocument{}, fmt.Errorf("inventory: earlier counts of count document %s: %w", d.Number, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE inventory.count_item SET counted_quantity = NULL, entry_quantity = NULL, entry_uom_code = NULL, counted_at = NULL,
			counted_by = NULL, counted_by_name = NULL, recounts = recounts + 1
		WHERE count_document_id = $1 AND item_no = ANY($2)`, d.ID, nos); err != nil {
		return CountDocument{}, fmt.Errorf("inventory: recount items of count document %s: %w", d.Number, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE inventory.count_document SET status = 'counting', version = version + 1, updated_by = $2 WHERE id = $1`,
		d.ID, p.ID); err != nil {
		return CountDocument{}, fmt.Errorf("inventory: count document %s recounted: %w", d.Number, err)
	}
	return loadCount(ctx, tx, d.ID, false)
}

// recordRecount adds the audit record of a recount and publishes
// PhysicalInventoryRecountRequested in the same transaction (ADR-0007,
// ADR-0011), for whoever counts the items again: the document as it is
// now, and each item recounted with the count it set aside, by number,
// as counted and by whom, in the request's order.
func recordRecount(ctx context.Context, d CountDocument, set []CountItem, r refs) error {
	items := make([]map[string]any, len(set))
	for i, it := range set {
		m := r.materials[it.MaterialID]
		items[i] = map[string]any{"item_no": it.ItemNo, "material_id": it.MaterialID, "material_number": m.Number, "count_no": it.CountNo(),
			"counted_quantity": map[string]any{"value": *it.Counted, "unit": m.BaseUnit}, "counted_at": it.CountedAt, "counted_by": it.CountedBy}
	}
	payload := map[string]any{"count_document_id": d.ID, "number": d.Number, "legal_entity_id": d.LegalEntityID,
		"legal_entity_code": r.companies[d.LegalEntityID].Code, "fiscal_year": d.FiscalYear, "plant_id": d.PlantID, "plant_code": r.units[d.PlantID].Code,
		"storage_location_id": d.StorageLocationID, "storage_location_code": r.units[d.StorageLocationID].Code, "status": d.Status, "items": items,
		"version": d.Version}
	detail := countDetail(d, r)
	detail["recounted"] = items
	return errs.Wrapf(audit.Record(ctx, "inventory.count_document.recount", "inventory.count_document", d.ID, detail,
		outbox.Event{Context: "inventory", Name: "physical_inventory_recount_requested", AggregateType: "inventory.count_document", AggregateID: d.ID,
			Payload: payload}), "inventory: record the recount of count document %s", d.Number)
}

// personName is the caller's name as the identity context has it now,
// which a count and a recount keep ("" when it names none).
func (e *Endpoints) personName(ctx context.Context, p reqctx.Principal, of string) (string, error) {
	names, err := e.People.PersonNames(ctx, []uuid.UUID{p.ID})
	if err != nil {
		return "", fmt.Errorf("inventory: name of the person counting %s: %w", of, err)
	}
	return names[p.ID], nil
}
