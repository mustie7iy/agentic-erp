package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestInventoryPermissions (S1c, migration 000376; ADR-0082 §7): the
// permissions of inventory are seeded and are no approval permissions.
func TestInventoryPermissions(t *testing.T) {
	need(t)
	rows, err := testDB.Query(context.Background(), `SELECT code FROM ref.permission WHERE code LIKE 'inventory.%' AND NOT is_approval ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(codes, " "), "inventory.movement.post inventory.stock.read"; got != want {
		t.Errorf("got %v, want %s", codes, want)
	}
}

// inventoryScene is a tenant with a material document of a company and a
// stock row, by id.
type inventoryScene struct{ tenant, company, doc, stock, material, plant, location uuid.UUID }

const inventoryDocument = `INSERT INTO inventory.material_document (tenant_id, id, legal_entity_id, fiscal_year, number, movement_type, posting_date,
		header_text, reversal_of_id, created_by) VALUES ($1, $2, $3, 2026, $4, $5, '2026-11-20', $6, $7, $1)`

func (s inventoryScene) document(id uuid.UUID, number, movementType string, reversalOf *uuid.UUID) stmt {
	return step(inventoryDocument, s.tenant, id, s.company, number, movementType, nil, reversalOf)
}

func (s inventoryScene) stockRow() stmt {
	return step(`INSERT INTO inventory.stock (tenant_id, id, legal_entity_id, material_id, plant_id, storage_location_id) VALUES ($1, $2, $3, $4, $5, $6)`,
		s.tenant, s.stock, s.company, s.material, s.plant, s.location)
}

func (s inventoryScene) movement(doc uuid.UUID, line int, qty, entry, unit string) stmt {
	return step(`INSERT INTO inventory.movement (tenant_id, material_document_id, line_no, item_no, material_id, plant_id, storage_location_id,
			quantity, entry_quantity, entry_uom_code) VALUES ($1, $2, $3, 1, $4, $5, $6, $7::numeric, $8::numeric, $9)`,
		s.tenant, doc, line, s.material, s.plant, s.location, qty, entry, unit)
}

// received is a 561 of 10 EA into the scene's stock.
func (s inventoryScene) received() []stmt {
	return []stmt{s.document(s.doc, "MD1", "561", nil), s.stockRow(), s.movement(s.doc, 1, "10", "10", "EA")}
}

// TestInventoryRules (S1c, migration 000376; ADR-0082 §7): a material
// document has a known movement type, a number of 1 to 40 characters once
// per company and fiscal year and a header text of 1 to 200 characters
// that is not blank; a reversal type names the document it reverses,
// another type none, and a document is reversed once, of its company, by
// its type's reversal; documents and movements are insert-only and none
// is deleted. A movement is of a document, once per line, moves a
// quantity that is not zero entered as a positive quantity of a known
// unit, on lines 1 to 200 of items 1 to 100, into a stock row written
// before it; a stock row is once per material, plant and storage
// location, written empty, changed only by its movements and never below
// zero, and not deleted.
func TestInventoryRules(t *testing.T) {
	need(t)
	cases := []struct {
		name string
		run  func(s inventoryScene) []stmt
		want string // empty: allowed
	}{
		{"a receipt into stock", func(s inventoryScene) []stmt { return s.received() }, ""},
		{"an unknown movement type", func(s inventoryScene) []stmt { return []stmt{s.document(s.doc, "MD1", "101", nil)} }, "material_document_movement_type_check"},
		{"an empty number", func(s inventoryScene) []stmt { return []stmt{s.document(s.doc, "", "561", nil)} }, "material_document_number_check"},
		{"a number of 41 characters", func(s inventoryScene) []stmt {
			return []stmt{s.document(s.doc, strings.Repeat("M", 41), "561", nil)}
		}, "material_document_number_check"},
		{"a number twice in the company and year", func(s inventoryScene) []stmt {
			return []stmt{s.document(s.doc, "MD1", "561", nil), s.document(id(), "MD1", "501", nil)}
		}, "material_document_number_once"},
		{"the number in another company", func(s inventoryScene) []stmt {
			return []stmt{s.document(s.doc, "MD1", "561", nil), step(inventoryDocument, s.tenant, id(), id(), "MD1", "561", nil, nil)}
		}, ""},
		{"a blank header text", func(s inventoryScene) []stmt {
			return []stmt{step(inventoryDocument, s.tenant, s.doc, s.company, "MD1", "561", " ", nil)}
		}, "material_document_header_text_check"},
		{"a header text of 201 characters", func(s inventoryScene) []stmt {
			return []stmt{step(inventoryDocument, s.tenant, s.doc, s.company, "MD1", "561", strings.Repeat("h", 201), nil)}
		}, "material_document_header_text_check"},
		{"a reversal type reversing nothing", func(s inventoryScene) []stmt { return []stmt{s.document(s.doc, "MD1", "562", nil)} }, "material_document_reversal_check"},
		{"a forward type reversing a document", func(s inventoryScene) []stmt {
			return []stmt{s.document(s.doc, "MD1", "561", nil), s.document(id(), "MD2", "501", &s.doc)}
		}, "a reversal is of its original"},
		{"a reversal", func(s inventoryScene) []stmt {
			return []stmt{s.document(s.doc, "MD1", "561", nil), s.document(id(), "MD2", "562", &s.doc)}
		}, ""},
		{"a reversal of another type's", func(s inventoryScene) []stmt {
			return []stmt{s.document(s.doc, "MD1", "561", nil), s.document(id(), "MD2", "502", &s.doc)}
		}, "a reversal is of its original"},
		{"a reversal in another company", func(s inventoryScene) []stmt {
			return []stmt{s.document(s.doc, "MD1", "561", nil), step(inventoryDocument, s.tenant, id(), id(), "MD2", "562", nil, s.doc)}
		}, "a reversal is of its original"},
		{"a reversal of a reversal", func(s inventoryScene) []stmt {
			rev := id()
			return []stmt{s.document(s.doc, "MD1", "561", nil), s.document(rev, "MD2", "562", &s.doc), s.document(id(), "MD3", "562", &rev)}
		}, "a reversal is of its original"},
		{"a reversal of no document", func(s inventoryScene) []stmt { return []stmt{s.document(s.doc, "MD1", "562", &s.stock)} }, "a reversal is of its original"},
		{"a document reversed twice", func(s inventoryScene) []stmt {
			return []stmt{s.document(s.doc, "MD1", "561", nil), s.document(id(), "MD2", "562", &s.doc), s.document(id(), "MD3", "562", &s.doc)}
		}, "material_document_reversed_once"},
		{"a document changed", func(s inventoryScene) []stmt {
			return []stmt{s.document(s.doc, "MD1", "561", nil), step(`UPDATE inventory.material_document SET number = 'MD9'`)}
		}, "permission denied"},
		{"a document deleted", func(s inventoryScene) []stmt {
			return []stmt{s.document(s.doc, "MD1", "561", nil), step(`DELETE FROM inventory.material_document`)}
		}, "permission denied"},
		{"a movement of no document", func(s inventoryScene) []stmt { return []stmt{s.stockRow(), s.movement(s.doc, 1, "10", "10", "EA")} }, "movement_document_fkey"},
		{"a movement into no stock row", func(s inventoryScene) []stmt {
			return []stmt{s.document(s.doc, "MD1", "561", nil), s.movement(s.doc, 1, "10", "10", "EA")}
		}, "a movement adds to a stock row written before it"},
		{"a line twice", func(s inventoryScene) []stmt { return append(s.received(), s.movement(s.doc, 1, "1", "1", "EA")) }, "movement_pkey"},
		{"a line 0", func(s inventoryScene) []stmt {
			return []stmt{s.document(s.doc, "MD1", "561", nil), s.stockRow(), s.movement(s.doc, 0, "1", "1", "EA")}
		}, "movement_line_no_check"},
		{"a line 201", func(s inventoryScene) []stmt {
			return []stmt{s.document(s.doc, "MD1", "561", nil), s.stockRow(), s.movement(s.doc, 201, "1", "1", "EA")}
		}, "movement_line_no_check"},
		{"an item 101", func(s inventoryScene) []stmt {
			return []stmt{s.document(s.doc, "MD1", "561", nil), s.stockRow(), step(`INSERT INTO inventory.movement (tenant_id, material_document_id, line_no,
					item_no, material_id, plant_id, storage_location_id, quantity, entry_quantity, entry_uom_code) VALUES ($1, $2, 1, 101, $3, $4, $5, 1, 1, 'EA')`,
				s.tenant, s.doc, s.material, s.plant, s.location)}
		}, "movement_item_no_check"},
		{"a movement of nothing", func(s inventoryScene) []stmt {
			return []stmt{s.document(s.doc, "MD1", "561", nil), s.stockRow(), s.movement(s.doc, 1, "0", "1", "EA")}
		}, "movement_quantity_check"},
		{"an entry quantity of nothing", func(s inventoryScene) []stmt {
			return []stmt{s.document(s.doc, "MD1", "561", nil), s.stockRow(), s.movement(s.doc, 1, "1", "0", "EA")}
		}, "movement_entry_quantity_check"},
		{"an unknown entry unit", func(s inventoryScene) []stmt {
			return []stmt{s.document(s.doc, "MD1", "561", nil), s.stockRow(), s.movement(s.doc, 1, "1", "1", "XX")}
		}, "movement_entry_uom_code_fkey"},
		{"an issue below zero", func(s inventoryScene) []stmt {
			return append(s.received(), s.movement(s.doc, 2, "-10.000001", "10.000001", "EA"))
		}, "stock_not_negative"},
		{"an issue of the whole stock", func(s inventoryScene) []stmt { return append(s.received(), s.movement(s.doc, 2, "-10", "10", "EA")) }, ""},
		{"a movement changed", func(s inventoryScene) []stmt {
			return append(s.received(), step(`UPDATE inventory.movement SET quantity = 5`))
		}, "permission denied"},
		{"a movement deleted", func(s inventoryScene) []stmt { return append(s.received(), step(`DELETE FROM inventory.movement`)) }, "permission denied"},
		{"a stock row twice", func(s inventoryScene) []stmt {
			return []stmt{s.stockRow(), step(`INSERT INTO inventory.stock (tenant_id, id, legal_entity_id, material_id, plant_id, storage_location_id)
				VALUES ($1, $2, $3, $4, $5, $6)`, s.tenant, id(), s.company, s.material, s.plant, s.location)}
		}, "stock_once"},
		{"a stock row written with a quantity", func(s inventoryScene) []stmt {
			return []stmt{step(`INSERT INTO inventory.stock (tenant_id, id, legal_entity_id, material_id, plant_id, storage_location_id, quantity)
				VALUES ($1, $2, $3, $4, $5, $6, 5)`, s.tenant, s.stock, s.company, s.material, s.plant, s.location)}
		}, "a stock row starts with no quantity"},
		{"a stock's quantity set", func(s inventoryScene) []stmt {
			return append(s.received(), step(`UPDATE inventory.stock SET quantity = 99`))
		}, "permission denied"},
		{"a stock row locked and touched", func(s inventoryScene) []stmt {
			return append(s.received(), step(`SELECT FROM inventory.stock FOR UPDATE`), step(`UPDATE inventory.stock SET updated_at = now()`))
		}, ""},
		{"a stock row deleted", func(s inventoryScene) []stmt { return append(s.received(), step(`DELETE FROM inventory.stock`)) }, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := inventoryScene{tenant: id(), company: id(), doc: id(), stock: id(), material: id(), plant: id(), location: id()}
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, c.run(s)...) })
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
}

// TestInventoryStockIsItsMovements (S1c, migration 000376): a stock row's
// quantity is the sum of its movements, which inventory.movement_stock
// adds as each is written.
func TestInventoryStockIsItsMovements(t *testing.T) {
	need(t)
	s := inventoryScene{tenant: id(), company: id(), doc: id(), stock: id(), material: id(), plant: id(), location: id()}
	issue := id()
	var got string
	err := asApp(t, s.tenant, func(tx pgx.Tx) error {
		if err := all(tx, append(s.received(), s.document(issue, "MD2", "201", nil), s.movement(issue, 1, "-3.25", "3.25", "EA"))...); err != nil {
			return err
		}
		return tx.QueryRow(context.Background(), `SELECT trim_scale(quantity)::text FROM inventory.stock WHERE id = $1`, s.stock).Scan(&got)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "6.75" {
		t.Errorf("stock: got %s, want 6.75 (10 - 3.25)", got)
	}
}

// TestInventoryTriggersAsOwner (S1c, migration 000376): past the grants,
// as the owner, documents and movements stay insert-only, a reversal is
// of its original's company with its type's reversal, a stock row starts
// empty and a movement needs its stock row and never takes it below
// zero. Each case runs in a transaction of the scene's tenant, as
// inventory.movement_stock, a security definer, sees only its rows.
func TestInventoryTriggersAsOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	s := inventoryScene{tenant: id(), company: id(), doc: id(), stock: id(), material: id(), plant: id(), location: id()}
	const movement = `INSERT INTO inventory.movement (tenant_id, material_document_id, line_no, item_no, material_id, plant_id, storage_location_id,
		quantity, entry_quantity, entry_uom_code) VALUES ($1, $2, 2, 1, $3, $4, $5, $6, 1, 'EA')`
	for name, c := range map[string]struct {
		st   stmt
		want string
	}{
		"a document changed":   {step(`UPDATE inventory.material_document SET number = 'MD9' WHERE id = $1`, s.doc), "insert-only"},
		"a document deleted":   {step(`DELETE FROM inventory.material_document WHERE id = $1`, s.doc), "insert-only"},
		"a movement changed":   {step(`UPDATE inventory.movement SET quantity = 1 WHERE material_document_id = $1`, s.doc), "insert-only"},
		"a movement deleted":   {step(`DELETE FROM inventory.movement WHERE material_document_id = $1`, s.doc), "insert-only"},
		"a reversal of a 561s": {step(inventoryDocument, s.tenant, id(), s.company, "MD2", "502", nil, s.doc), "material_document_reversal"},
		"a stock row with a quantity": {step(`INSERT INTO inventory.stock (tenant_id, id, legal_entity_id, material_id, plant_id, storage_location_id,
			quantity) VALUES ($1, $2, $3, $4, $5, $6, 1)`, s.tenant, id(), s.company, id(), s.plant, s.location), "stock_from_movements"},
		"a movement into no stock row": {step(movement, s.tenant, s.doc, id(), s.plant, s.location, "1"), "movement_stock"},
		"an issue below zero":          {step(movement, s.tenant, s.doc, s.material, s.plant, s.location, "-11"), "stock_not_negative"},
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", s.tenant.String()); err != nil {
				t.Fatal(err)
			}
			if err := all(tx, s.received()...); err != nil {
				t.Fatal(err)
			}
			err = exec(tx, c.st.sql, c.st.args...)
			if c.want == "insert-only" {
				failsWith(t, err, c.want)
				return
			}
			failsOn(t, err, c.want)
		})
	}
}

// TestInventoryReversalCheck (S1c, migration 000376): with the triggers
// aside, as material_document_reversal refuses it first, a forward
// movement type naming a document it reverses is refused by
// material_document_reversal_check too.
func TestInventoryReversalCheck(t *testing.T) {
	need(t)
	ctx := context.Background()
	s := inventoryScene{tenant: id(), company: id(), doc: id(), stock: id(), material: id(), plant: id(), location: id()}
	tx, err := testDB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL session_replication_role = replica"); err != nil {
		t.Fatal(err)
	}
	err = all(tx, s.document(s.doc, "MD1", "561", nil), s.document(id(), "MD2", "501", &s.doc))
	failsOn(t, err, "material_document_reversal_check")
}
