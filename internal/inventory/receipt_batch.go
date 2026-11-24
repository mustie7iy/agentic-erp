package inventory

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Batches and serial numbers in a purchase order's goods receipt (S2f1,
// S2g1; ADR-0082 As built S2f1): a stock item of a batch-managed material
// names its batch by number, and the receipt creates it, with the
// manufacture and expiry dates the item gives, when the material has none
// of that number (SAP MIGO creating a batch at goods receipt, from
// memory); one of a serial-managed material names the serial number of
// each unit, which the posting writes as a 501's (takeSerials). Both are
// then checked and moved as a person's goods movement's are (batchOf,
// serialsOf, takeStock): 101 and 102 carry them.

// ReceiptBatch is the batch a goods receipt's stock item names: its
// number, and the dates a new one is created with (a manufacture date
// required) or an existing one has.
type ReceiptBatch struct {
	Number                      string
	ManufactureDate, ExpiryDate *time.Time
}

// batchName is a batch as a receipt names it: by its material and number.
type batchName struct {
	material uuid.UUID
	number   string
}

// compare orders batch names as the receipt writes them: by material as
// PostgreSQL orders uuids (bytewise), then number (the C collation's order
// of the capitals, digits, '-' and '_' a number is made of, bytewise too).
func (k batchName) compare(o batchName) int {
	return cmp.Or(bytes.Compare(k.material[:], o.material[:]), strings.Compare(k.number, o.number))
}

// receiptBatches gives each item of a receipt the batch it names (items[k]
// is in[k]'s), creating the batches the material has none of yet, and
// puts them in r.batches. An item of a batch-managed material names one
// (422 inventory.batch_required), another item none (422
// inventory.batch_not_allowed); a new batch has a manufacture date (422
// inventory.batch_manufacture_date_required) and expires on or after it
// (422 inventory.batch_dates); dates given for a batch are its own (422
// inventory.batch_dates_differ). An item whose material is not found is
// left to items to refuse.
func (e *Endpoints) receiptBatches(ctx context.Context, tx pgx.Tx, p reqctx.Principal, in []OrderItem, items []ItemInput, r *refs) error {
	var names []batchName
	for k, it := range in {
		m, ok := r.materials[it.MaterialID]
		if !ok {
			continue
		}
		if err := receiptBatchOf(k, it, m); err != nil {
			return err
		}
		if n := (batchName{it.MaterialID, batchNumber(it)}); it.Batch != nil && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil
	}
	slices.SortFunc(names, batchName.compare)
	found, err := batchesNamed(ctx, tx, names)
	if err != nil {
		return err
	}
	wrote, err := e.createBatches(ctx, tx, p, in, names, found, r)
	if err != nil {
		return err
	}
	if wrote { // the new ones, as this receipt or another created them
		if found, err = batchesNamed(ctx, tx, names); err != nil {
			return err
		}
	}
	r.batches = map[uuid.UUID]Batch{}
	for k, it := range in {
		if it.Batch == nil {
			continue
		}
		b := found[batchName{it.MaterialID, it.Batch.Number}]
		if err := sameDates(k, it.Batch, b, r.materials[it.MaterialID]); err != nil {
			return err
		}
		items[k].BatchID, r.batches[b.ID] = &b.ID, b
	}
	return nil
}

// batchNumber is the number of the batch an item names, "" when none.
func batchNumber(it OrderItem) string {
	if it.Batch == nil {
		return ""
	}
	return it.Batch.Number
}

// receiptBatchOf checks that receipt item k names a batch exactly when its
// material is batch-managed, and that the dates it gives are in order.
func receiptBatchOf(k int, it OrderItem, m materials.Material) error {
	at := fmt.Sprintf("items[%d].batch", k)
	switch {
	case m.BatchManaged && it.Batch == nil:
		return problem.Invalid("inventory.batch_required", at, "Material "+m.Number+" is managed in batches; the item names its batch.")
	case !m.BatchManaged && it.Batch != nil:
		return problem.Invalid("inventory.batch_not_allowed", at, "Material "+m.Number+" is not managed in batches; the item names no batch.")
	case it.Batch != nil && it.Batch.ManufactureDate != nil && it.Batch.ExpiryDate != nil && it.Batch.ExpiryDate.Before(*it.Batch.ManufactureDate):
		return problem.Invalid("inventory.batch_dates", at+".expiry_date", "A batch expires on or after the day it was made, "+
			it.Batch.ManufactureDate.Format(time.DateOnly)+".")
	}
	return nil
}

// batchesNamed reads the batches of the names that exist, by name. A batch
// does not change, so it is read without a lock.
func batchesNamed(ctx context.Context, tx pgx.Tx, names []batchName) (map[batchName]Batch, error) {
	ms, numbers := make([]uuid.UUID, len(names)), make([]string, len(names))
	for i, n := range names {
		ms[i], numbers[i] = n.material, n.number
	}
	rows, _ := tx.Query(ctx, batchSelect+` JOIN unnest($1::uuid[], $2::text[]) AS x (m, n) ON material_id = x.m AND number = x.n
		ORDER BY material_id, number COLLATE "C"`, ms, numbers)
	bs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Batch, error) { return scanBatch(row) })
	if err != nil {
		return nil, fmt.Errorf("inventory: read %d batches by number: %w", len(names), err)
	}
	out := make(map[batchName]Batch, len(bs))
	for _, b := range bs {
		out[batchName{b.MaterialID, b.Number}] = b
	}
	return out, nil
}

// createBatches creates the named batches not found, each with the dates
// of the first item naming it that gives a manufacture date, in the
// names' order, so two receipts creating the same batches never wait on
// each other in a cycle; a batch another receipt creates meanwhile is
// left as that one made it (ON CONFLICT DO NOTHING, after waiting for it
// to commit), and its dates are checked as an existing batch's are. Each
// batch this receipt creates is audited as AddBatch's is. It says whether
// it wrote, so that the batches are read again.
func (e *Endpoints) createBatches(ctx context.Context, tx pgx.Tx, p reqctx.Principal, in []OrderItem, names []batchName, found map[batchName]Batch,
	r *refs) (bool, error) {
	var ids, ms []uuid.UUID
	var numbers []string
	var made []time.Time
	var expires []*time.Time
	for _, n := range names {
		if _, ok := found[n]; ok {
			continue
		}
		k, b := newBatch(in, n)
		if b.ManufactureDate == nil {
			return false, problem.Invalid("inventory.batch_manufacture_date_required", fmt.Sprintf("items[%d].batch.manufacture_date", k), "Material "+
				r.materials[n.material].Number+" has no batch "+n.number+"; a new batch is created with the day it was made.")
		}
		ids, ms, numbers = append(ids, uuid.Must(uuid.NewV7())), append(ms, n.material), append(numbers, n.number)
		made, expires = append(made, *b.ManufactureDate), append(expires, b.ExpiryDate)
	}
	if len(ids) == 0 {
		return false, nil
	}
	rows, _ := tx.Query(ctx, `INSERT INTO inventory.batch (tenant_id, id, material_id, number, manufacture_date, expiry_date, created_by)
		SELECT $1, x.id, x.m, x.n, x.d, x.e, $2 FROM unnest($3::uuid[], $4::uuid[], $5::text[], $6::date[], $7::date[]) WITH ORDINALITY AS x (id, m, n, d, e, o)
		ORDER BY x.o ON CONFLICT (tenant_id, material_id, number) DO NOTHING RETURNING `+batchColumns,
		p.TenantID, p.ID, ids, ms, numbers, made, expires)
	created, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Batch, error) { return scanBatch(row) })
	if err != nil {
		return false, fmt.Errorf("inventory: create %d batches at a goods receipt: %w", len(ids), err)
	}
	slices.SortFunc(created, func(a, b Batch) int {
		return batchName{a.MaterialID, a.Number}.compare(batchName{b.MaterialID, b.Number})
	})
	for _, b := range created {
		if err := audit.Record(ctx, "inventory.batch.create", "inventory.batch", b.ID, batchDetail(b, r.materials[b.MaterialID])); err != nil {
			return false, err // its error names the action
		}
	}
	return true, nil
}

// newBatch is the first item naming a batch that gives its manufacture
// date, or the first naming it when none does, and that item's batch.
func newBatch(in []OrderItem, n batchName) (int, *ReceiptBatch) {
	first := -1
	for k, it := range in {
		if it.MaterialID != n.material || batchNumber(it) != n.number {
			continue
		}
		if it.Batch.ManufactureDate != nil {
			return k, it.Batch
		}
		if first < 0 {
			first = k
		}
	}
	return first, in[first].Batch
}

// sameDates refuses dates receipt item k gives that are not its batch's:
// a batch's dates do not change (Later, plan §S2f).
func sameDates(k int, given *ReceiptBatch, b Batch, m materials.Material) error {
	at := fmt.Sprintf("items[%d].batch", k)
	expires := "does not expire"
	if b.ExpiryDate != nil {
		expires = "expires on " + b.ExpiryDate.Format(time.DateOnly)
	}
	refuse := func(field string) error {
		return problem.Invalid("inventory.batch_dates_differ", at+field, "Batch "+b.Number+" of material "+m.Number+" was made on "+
			b.ManufactureDate.Format(time.DateOnly)+" and "+expires+"; a batch's dates do not change.")
	}
	switch {
	case given.ManufactureDate != nil && !sameDay(*given.ManufactureDate, &b.ManufactureDate):
		return refuse(".manufacture_date")
	case given.ExpiryDate != nil && !sameDay(*given.ExpiryDate, b.ExpiryDate):
		return refuse(".expiry_date")
	}
	return nil
}

// sameDay says whether a date given is the day stored, which may be none.
func sameDay(given time.Time, stored *time.Time) bool {
	return stored != nil && given.Format(time.DateOnly) == stored.Format(time.DateOnly)
}

// batchDetail is a batch's creation as its audit record gives it.
func batchDetail(b Batch, m materials.Material) map[string]any {
	detail := map[string]any{"material_id": m.ID, "material_number": m.Number, "number": b.Number,
		"manufacture_date": b.ManufactureDate.Format(time.DateOnly), "expiry_date": nil}
	if b.ExpiryDate != nil {
		detail["expiry_date"] = b.ExpiryDate.Format(time.DateOnly)
	}
	return detail
}
