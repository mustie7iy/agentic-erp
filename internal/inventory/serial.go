package inventory

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"math/big"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Serial numbers (S2g; ADR-0082 As built S2g): a material with a serial
// number profile (materials.Material.SerialManaged, fixed once created)
// is moved only in whole units of its base unit by items that name the
// serial number of each unit. A serial number is unique for its material
// and in stock at most once: inventory.serial holds where it is now, and
// only its movements (inventory.movement_serial, its history) move it.
// The first receipt that names a serial number writes it, out of stock; a
// posting then locks the serial numbers its lines move, after the
// locations' and batches' stock rows, in one order, and refuses a receipt
// of one in stock or an issue of one not in stock at the line's location.
// The movement types that carry no serial numbers yet (a purchase order's
// goods receipt, a delivery's goods issue, a stock transport order's, a
// count's) refuse a serial-managed material, as they refuse a
// batch-managed one.

func init() {
	// takeSerials refuses both first, naming the serial number; these are
	// the database's answers should one pass it.
	problem.RegisterConstraint("serial_in_stock_once", func() *problem.Problem {
		return problem.Conflict("inventory.serial_in_stock", "A goods movement would receive a serial number that is in stock already.")
	})
	problem.RegisterConstraint("serial_issued_from_stock", func() *problem.Problem {
		return problem.Conflict("inventory.serial_not_in_stock", "A goods movement would issue a serial number not in stock at its location.")
	})
}

// lowerFirst is a movement type's name inside a sentence.
func lowerFirst(s string) string {
	return strings.ToLower(s[:1]) + s[1:]
}

// serialsOf checks an item's serial numbers: one for each unit it moves
// when the material is serial-managed and the movement type carries
// serial numbers, a whole number of units of the base unit, and none when
// the material is not serial-managed.
func serialsOf(at string, it ItemInput, mt movementType, m materials.Material, base *big.Rat) error {
	n := len(it.SerialNumbers)
	switch {
	case m.SerialManaged && !mt.tracked:
		return problem.Invalid("inventory.serial_not_supported", at+"material_id", "Material "+m.Number+" has a serial number profile; a "+
			lowerFirst(mt.name)+" does not carry serial numbers yet.")
	case !m.SerialManaged && n > 0:
		return problem.Invalid("inventory.serial_not_allowed", at+"serial_numbers", "Material "+m.Number+" has no serial number profile; the item "+
			"names no serial numbers.")
	case !m.SerialManaged:
		return nil
	case !base.IsInt():
		return problem.Invalid("inventory.serial_quantity", at+"quantity", "Material "+m.Number+" has a serial number profile, so an item moves whole "+
			"units of its base unit "+m.BaseUnit+", one serial number each; "+it.Quantity+" "+it.Unit+" is "+money.PlainQuantity(base)+" "+m.BaseUnit+".")
	case big.NewInt(int64(n)).Cmp(base.Num()) != 0:
		return problem.Invalid("inventory.serial_required", at+"serial_numbers", fmt.Sprintf("Material %s has a serial number profile; the item "+
			"moves %s %s and names %d serial numbers, one for each unit.", m.Number, money.PlainQuantity(base), m.BaseUnit, n))
	}
	return nil
}

// serialKey is a serial number of a material.
type serialKey struct {
	material uuid.UUID
	number   string
}

// compare orders serial keys as every posting writes and locks their
// rows: by material as PostgreSQL orders uuids (bytewise), then number
// (the C collation's order of the capitals, digits, '-' and '_' a number
// is made of, which is bytewise too).
func (k serialKey) compare(o serialKey) int {
	return cmp.Or(bytes.Compare(k.material[:], o.material[:]), strings.Compare(k.number, o.number))
}

// repeatedSerials refuses a serial number that a document's items name
// twice: a unit moves once in a document (a transfer's item receives what
// it issues, as two lines of one item).
func repeatedSerials(in []ItemInput) error {
	first := map[serialKey]int{}
	for i, it := range in {
		for j, s := range it.SerialNumbers {
			k := serialKey{it.MaterialID, s}
			if before, ok := first[k]; ok {
				return problem.Invalid("inventory.serial_repeated", fmt.Sprintf("items[%d].serial_numbers[%d]", i, j), fmt.Sprintf("Serial number %s "+
					"is named by item %d already; each unit moves once in a document.", s, before+1))
			}
			first[k] = i
		}
	}
	return nil
}

// place is a storage location of a plant a serial number is in stock at.
type place struct{ plant, location uuid.UUID }

// takeSerials writes the serial numbers the receiving lines name the
// first time, out of stock, and locks every serial number the lines
// move, in serialKey order, after the caller locked the stock rows; then
// it walks the lines in their order, as movement_serial_moves applies
// them, and refuses a receipt of a serial number in stock (409
// inventory.serial_in_stock) or an issue of one not in stock at the
// line's location (409 inventory.serial_not_in_stock), each naming it.
func takeSerials(ctx context.Context, tx pgx.Tx, p reqctx.Principal, company uuid.UUID, lines []Line, r refs) error {
	keys, received := serialKeys(lines)
	if len(keys) == 0 {
		return nil
	}
	at, err := lockSerials(ctx, tx, p, company, received, keys)
	if err != nil {
		return err
	}
	for _, l := range lines {
		if err := moveSerials(l, at, r); err != nil {
			return err
		}
	}
	return nil
}

// serialKeys are the serial numbers the lines move and those of them the
// receiving lines name, each once and sorted.
func serialKeys(lines []Line) (keys, received []serialKey) {
	all, in := map[serialKey]bool{}, map[serialKey]bool{}
	for _, l := range lines {
		receives := money.ParseRat(l.Quantity).Sign() > 0
		for _, s := range l.SerialNumbers {
			all[serialKey{l.MaterialID, s}] = true
			in[serialKey{l.MaterialID, s}] = in[serialKey{l.MaterialID, s}] || receives
		}
	}
	for k := range all {
		keys = append(keys, k)
		if in[k] {
			received = append(received, k)
		}
	}
	slices.SortFunc(keys, serialKey.compare)
	slices.SortFunc(received, serialKey.compare)
	return keys, received
}

// moveSerials applies a line to where its serial numbers are, refusing a
// receipt of one in stock and an issue of one not at the line's location.
func moveSerials(l Line, at map[serialKey]*place, r refs) error {
	here, issue := &place{l.PlantID, l.StorageLocationID}, money.ParseRat(l.Quantity).Sign() < 0
	for _, s := range l.SerialNumbers {
		k := serialKey{l.MaterialID, s}
		where := at[k]
		switch {
		case issue && (where == nil || *where != *here):
			return problem.Conflict("inventory.serial_not_in_stock", "Serial number "+s+" of material "+r.materials[l.MaterialID].Number+
				" is not in stock at "+r.where(keyOf(l))+".")
		case !issue && where != nil:
			return problem.Conflict("inventory.serial_in_stock", "Serial number "+s+" of material "+r.materials[l.MaterialID].Number+
				" is in stock already (GET /v1/serial-numbers?material_id="+l.MaterialID.String()+"&number="+s+").")
		case issue:
			at[k] = nil
		default:
			at[k] = here
		}
	}
	return nil
}

// lockSerials writes the received serial numbers not written yet, out of
// stock, and locks every one of keys, both in the keys' order; it answers
// where each is in stock. A serial number of keys an issue names that
// was never written is not in the answer: it is in no stock.
func lockSerials(ctx context.Context, tx pgx.Tx, p reqctx.Principal, company uuid.UUID, received, keys []serialKey) (map[serialKey]*place, error) {
	ids, ms, numbers := make([]uuid.UUID, len(received)), make([]uuid.UUID, len(received)), make([]string, len(received))
	for i, k := range received {
		ids[i], ms[i], numbers[i] = uuid.Must(uuid.NewV7()), k.material, k.number
	}
	if _, err := tx.Exec(ctx, `INSERT INTO inventory.serial (tenant_id, id, legal_entity_id, material_id, number, created_by)
		SELECT $1, x.id, $2, x.m, x.n, $3 FROM unnest($4::uuid[], $5::uuid[], $6::text[]) WITH ORDINALITY AS x (id, m, n, o)
		ORDER BY x.o ON CONFLICT (tenant_id, material_id, number) DO NOTHING`, p.TenantID, company, p.ID, ids, ms, numbers); err != nil {
		return nil, fmt.Errorf("inventory: write %d serial numbers: %w", len(received), err)
	}
	ms, numbers = make([]uuid.UUID, len(keys)), make([]string, len(keys))
	for i, k := range keys {
		ms[i], numbers[i] = k.material, k.number
	}
	rows, _ := tx.Query(ctx, `SELECT s.material_id, s.number, s.plant_id, s.storage_location_id FROM inventory.serial s
		JOIN unnest($1::uuid[], $2::text[]) AS x (m, n) ON s.material_id = x.m AND s.number = x.n
		ORDER BY s.material_id, s.number COLLATE "C" FOR UPDATE OF s`, ms, numbers)
	out := map[serialKey]*place{}
	var k serialKey
	var plant, location *uuid.UUID
	if _, err := pgx.ForEachRow(rows, []any{&k.material, &k.number, &plant, &location}, func() error {
		out[k] = nil
		if plant != nil {
			out[k] = &place{*plant, *location}
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("inventory: lock %d serial numbers: %w", len(keys), err)
	}
	return out, nil
}

// writeSerials writes the serial numbers each line moves, in line order,
// as takeSerials checked them: movement_serial_moves moves each on its
// own, and movement_serials checks at commit that each line names one
// for each unit of a serial-managed material.
func writeSerials(ctx context.Context, tx pgx.Tx, p reqctx.Principal, document uuid.UUID, lines []Line) error {
	var lineNo []int16
	var ms []uuid.UUID
	var numbers []string
	for _, l := range lines {
		for _, s := range l.SerialNumbers {
			lineNo, ms, numbers = append(lineNo, l.LineNo), append(ms, l.MaterialID), append(numbers, s)
		}
	}
	if len(numbers) == 0 {
		return nil
	}
	tag, err := tx.Exec(ctx, `INSERT INTO inventory.movement_serial (tenant_id, material_document_id, line_no, serial_id, material_id)
		SELECT $1, $2, x.l, s.id, s.material_id FROM unnest($3::smallint[], $4::uuid[], $5::text[]) WITH ORDINALITY AS x (l, m, n, o)
		JOIN inventory.serial s ON s.tenant_id = $1 AND s.material_id = x.m AND s.number = x.n ORDER BY x.o`, p.TenantID, document, lineNo, ms, numbers)
	if err != nil {
		return fmt.Errorf("inventory: serial numbers of material document %s: %w", document, err)
	}
	if tag.RowsAffected() != int64(len(numbers)) {
		// takeSerials locked every one, so none is missing.
		return fmt.Errorf("inventory: serial numbers of material document %s: wrote %d of %d", document, tag.RowsAffected(), len(numbers))
	}
	return nil
}
