package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestInventoryPermissions (S1c, migration 000376, S1d, 000377, and S2c, 000406;
// ADR-0082 §7-§8): the permissions of inventory are seeded and are no
// approval permissions.
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
	if got, want := strings.Join(codes, " "), "inventory.count.write inventory.movement.post inventory.stock.read inventory.valuation.read inventory.valuation.write"; got != want {
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

// valuation is the scene's material valued in its plant at a moving
// price (S1d), which every movement needs.
func (s inventoryScene) valuation() stmt {
	return step(`INSERT INTO inventory.valuation (tenant_id, id, legal_entity_id, material_id, plant_id, valuation_class, price_control, currency,
			price, price_unit, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, '3000', 'moving_average', 'EUR', 100, 1, $1, $1)`,
		s.tenant, s.stock, s.company, s.material, s.plant)
}

// received is a 561 of 10 EA into the scene's stock.
func (s inventoryScene) received() []stmt {
	return []stmt{s.valuation(), s.document(s.doc, "MD1", "561", nil), s.stockRow(), s.movement(s.doc, 1, "10", "10", "EA")}
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
		{"an unknown movement type", func(s inventoryScene) []stmt { return []stmt{s.document(s.doc, "MD1", "601", nil)} }, "material_document_movement_type_check"},
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
		{"a movement of no document", func(s inventoryScene) []stmt {
			return []stmt{s.valuation(), s.stockRow(), s.movement(s.doc, 1, "10", "10", "EA")}
		}, "movement_document_fkey"},
		{"a movement of no valuation", func(s inventoryScene) []stmt {
			return []stmt{s.document(s.doc, "MD1", "561", nil), s.stockRow(), s.movement(s.doc, 1, "10", "10", "EA")}
		}, "a movement is valued by its material's valuation in the plant"},
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

// TestValuationRules (S1d, migration 000377; ADR-0082 §8): a valuation
// is once per material and plant, of a valuation class of 1 to 4 capitals
// or digits, a known price control and currency, a price not below zero
// per a price unit of 1 to 10000; it starts with no stock, its stock's
// quantity and value are not below zero and without stock it has no
// value, a standard-priced one is worth its quantity at its price; its
// material, plant, company, price control, currency and price unit stay,
// the application changes its class alone and deletes none. A movement
// adds its quantity and value, a moving price following them. A price
// change is of the valuation's price and stock as they are, changes the
// price, names its entry exactly when it revalues, and is insert-only;
// a material document names its whole entry, in its currency.
func TestValuationRules(t *testing.T) {
	need(t)
	const valuation = `INSERT INTO inventory.valuation (tenant_id, id, legal_entity_id, material_id, plant_id, valuation_class, price_control, currency,
		price, price_unit, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $1, $1)`
	with := func(s inventoryScene, class, control, currency string, price, unit int) stmt {
		return step(valuation, s.tenant, s.stock, s.company, s.material, s.plant, class, control, currency, price, unit)
	}
	standard := func(s inventoryScene) stmt { return with(s, "3000", "standard", "EUR", 100, 3) }
	valued := func(s inventoryScene, qty string, amount int) stmt {
		return step(`INSERT INTO inventory.movement (tenant_id, material_document_id, line_no, item_no, material_id, plant_id, storage_location_id,
				quantity, entry_quantity, entry_uom_code, amount) VALUES ($1, $2, (SELECT count(*) + 1 FROM inventory.movement WHERE material_document_id = $2),
				1, $3, $4, $5, $6::numeric, abs($6::numeric), 'EA', $7)`, s.tenant, s.doc, s.material, s.plant, s.location, qty, amount)
	}
	doc := func(s inventoryScene) []stmt { return []stmt{s.document(s.doc, "MD1", "561", nil), s.stockRow()} }
	change := func(s inventoryScene, old, price int, qty string, amount int, entry bool) stmt {
		var id any
		if entry {
			id = s.doc
		}
		return step(`INSERT INTO inventory.price_change (tenant_id, id, valuation_id, old_price, new_price, quantity, amount, posting_date,
				journal_entry_id, journal_entry_year, journal_entry_number, created_by) VALUES ($1, $2, $3, $4, $5, $6::numeric, $7, '2026-11-20', $8,
				CASE WHEN $8::uuid IS NULL THEN NULL ELSE 2026 END, CASE WHEN $8::uuid IS NULL THEN NULL ELSE 'PR1' END, $1)`,
			s.tenant, s.doc, s.stock, old, price, qty, amount, id)
	}
	cases := []struct {
		name string
		run  func(s inventoryScene) []stmt
		want string
	}{
		{"a standard valuation with its stock", func(s inventoryScene) []stmt {
			return append([]stmt{standard(s)}, append(doc(s), valued(s, "10", 333))...)
		}, ""},
		{"a standard stock at a tie, rounded away from zero", func(s inventoryScene) []stmt {
			return append([]stmt{with(s, "3000", "standard", "EUR", 1, 3)}, append(doc(s), valued(s, "1.5", 1))...)
		}, ""},
		{"a standard stock at a tie, rounded down", func(s inventoryScene) []stmt {
			return append([]stmt{with(s, "3000", "standard", "EUR", 1, 3)}, append(doc(s), valued(s, "1.5", 0))...)
		}, "valuation_standard_value"},
		{"a moving price at a tie, rounded away from zero", func(s inventoryScene) []stmt {
			return append([]stmt{with(s, "3000", "moving_average", "EUR", 0, 1)}, append(doc(s), valued(s, "2", 3),
				step(`SELECT 1/(price = 2)::int FROM inventory.valuation`))...)
		}, ""},
		{"a standard stock not worth its price", func(s inventoryScene) []stmt {
			return append([]stmt{standard(s)}, append(doc(s), valued(s, "10", 334))...)
		},
			"valuation_standard_value"},
		{"an issue leaving value without stock", func(s inventoryScene) []stmt {
			return append([]stmt{with(s, "3000", "moving_average", "EUR", 100, 1)}, append(doc(s), valued(s, "10", 1000), valued(s, "-10", -999))...)
		}, "valuation_value_without_stock"},
		{"an issue of more value than the stock", func(s inventoryScene) []stmt {
			return append([]stmt{with(s, "3000", "moving_average", "EUR", 100, 1)}, append(doc(s), valued(s, "10", 1000), valued(s, "-5", -1001))...)
		}, "valuation_value_check"},
		{"a class of five", func(s inventoryScene) []stmt { return []stmt{with(s, "30000", "standard", "EUR", 1, 1)} }, "valuation_class_check"},
		{"an unknown price control", func(s inventoryScene) []stmt { return []stmt{with(s, "3000", "fifo", "EUR", 1, 1)} }, "valuation_price_control_check"},
		{"an unknown currency", func(s inventoryScene) []stmt { return []stmt{with(s, "3000", "standard", "XXX", 1, 1)} }, "valuation_currency_fkey"},
		{"a negative price", func(s inventoryScene) []stmt { return []stmt{with(s, "3000", "standard", "EUR", -1, 1)} }, "valuation_price_check"},
		{"a price unit of 0", func(s inventoryScene) []stmt { return []stmt{with(s, "3000", "standard", "EUR", 1, 0)} }, "valuation_price_unit_check"},
		{"a price unit of 10001", func(s inventoryScene) []stmt { return []stmt{with(s, "3000", "standard", "EUR", 1, 10001)} }, "valuation_price_unit_check"},
		{"twice", func(s inventoryScene) []stmt {
			return []stmt{standard(s), step(valuation, s.tenant, id(), s.company, s.material, s.plant, "3000", "standard", "EUR", 1, 1)}
		}, "valuation_once"},
		{"created with stock", func(s inventoryScene) []stmt {
			return []stmt{step(`INSERT INTO inventory.valuation (tenant_id, id, legal_entity_id, material_id, plant_id, valuation_class, price_control,
				currency, price, price_unit, quantity, created_by, updated_by) VALUES ($1, $2, $1, $1, $1, '3000', 'standard', 'EUR', 1, 1, 1, $1, $1)`, s.tenant, s.stock)}
		}, "a valuation starts with no stock"},
		{"its class changed", func(s inventoryScene) []stmt {
			return []stmt{standard(s), step(`UPDATE inventory.valuation SET valuation_class = '3100', version = version + 1`)}
		}, ""},
		{"its price set", func(s inventoryScene) []stmt {
			return []stmt{standard(s), step(`UPDATE inventory.valuation SET price = 5, version = version + 1`)}
		},
			"permission denied"},
		{"deleted", func(s inventoryScene) []stmt { return []stmt{standard(s), step(`DELETE FROM inventory.valuation`)} }, "permission denied"},
		{"a moving price following a receipt", func(s inventoryScene) []stmt {
			return append([]stmt{with(s, "3000", "moving_average", "EUR", 0, 10)}, append(doc(s), valued(s, "4", 1000),
				step(`SELECT 1/(price = 2500)::int FROM inventory.valuation`))...)
		}, ""},
		{"a price change revaluing", func(s inventoryScene) []stmt {
			return append([]stmt{standard(s)}, append(doc(s), valued(s, "9", 300), change(s, 100, 200, "9", 300, true),
				step(`SELECT 1/(price = 200 AND value = 600)::int FROM inventory.valuation`))...)
		}, ""},
		{"a price change of another price", func(s inventoryScene) []stmt { return []stmt{standard(s), change(s, 99, 200, "0", 0, false)} },
			"a price change is of its valuation's price and stock as they are"},
		{"a price change of another stock", func(s inventoryScene) []stmt { return []stmt{standard(s), change(s, 100, 200, "1", 0, false)} },
			"a price change is of its valuation's price and stock as they are"},
		{"a price change to the same price", func(s inventoryScene) []stmt { return []stmt{standard(s), change(s, 100, 100, "0", 0, false)} },
			"price_change_changes"},
		{"a revaluation without its entry", func(s inventoryScene) []stmt { return []stmt{standard(s), change(s, 100, 200, "0", 5, false)} }, "price_change_entry"},
		{"an entry without a revaluation", func(s inventoryScene) []stmt { return []stmt{standard(s), change(s, 100, 200, "0", 0, true)} }, "price_change_entry"},
		{"a price change changed", func(s inventoryScene) []stmt {
			return []stmt{standard(s), change(s, 100, 200, "0", 0, false), step(`UPDATE inventory.price_change SET amount = 1`)}
		}, "permission denied"},
		{"a document's entry without its year", func(s inventoryScene) []stmt {
			return []stmt{step(`INSERT INTO inventory.material_document (tenant_id, id, legal_entity_id, fiscal_year, number, movement_type, posting_date,
				currency, journal_entry_id, journal_entry_number, created_by) VALUES ($1, $2, $1, 2026, 'MD1', '561', '2026-11-20', 'EUR', $2, 'WE1', $1)`,
				s.tenant, s.doc)}
		}, "material_document_entry"},
		{"a document's entry without its currency", func(s inventoryScene) []stmt {
			return []stmt{step(`INSERT INTO inventory.material_document (tenant_id, id, legal_entity_id, fiscal_year, number, movement_type, posting_date,
				journal_entry_id, journal_entry_year, journal_entry_number, created_by) VALUES ($1, $2, $1, 2026, 'MD1', '561', '2026-11-20', $2, 2026, 'WE1', $1)`,
				s.tenant, s.doc)}
		}, "material_document_entry"},
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

// TestValuationTriggersAsOwner (S1d, migration 000377): past the grants,
// as the owner, a valuation's material, plant, company, price control,
// currency and price unit stay, it starts empty, and a price change is
// of its valuation's price, changes it and stays as written. Each case
// runs on a valuation whose price was changed from 1.00 to 1.50.
func TestValuationTriggersAsOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	s := inventoryScene{tenant: id(), company: id(), doc: id(), stock: id(), material: id(), plant: id(), location: id()}
	for name, c := range map[string]struct {
		st   stmt
		want string
	}{
		"its price control changed": {step(`UPDATE inventory.valuation SET price_control = 'standard', price = 100, version = version + 1 WHERE id = $1`, s.stock),
			"valuation_fixed"},
		"its price unit changed": {step(`UPDATE inventory.valuation SET price_unit = 2, version = version + 1 WHERE id = $1`, s.stock), "valuation_fixed"},
		"its plant changed":      {step(`UPDATE inventory.valuation SET plant_id = gen_random_uuid(), version = version + 1 WHERE id = $1`, s.stock), "valuation_fixed"},
		"a second, with stock": {step(`INSERT INTO inventory.valuation (tenant_id, id, legal_entity_id, material_id, plant_id, valuation_class, price_control,
			currency, price, price_unit, value, created_by, updated_by) VALUES ($1, $2, $1, $1, $1, '3000', 'standard', 'EUR', 1, 1, 1, $1, $1)`, s.tenant, id()),
			"valuation_fixed"},
		"a price change changed": {step(`UPDATE inventory.price_change SET amount = 1`), "insert-only"},
		"a price change to its price": {step(`INSERT INTO inventory.price_change (tenant_id, id, valuation_id, old_price, new_price, quantity, amount,
			posting_date, created_by) VALUES ($1, $2, $3, 150, 150, 0, 0, '2026-11-20', $1)`, s.tenant, id(), s.stock), "price_change_changes"},
		"a price change of another price": {step(`INSERT INTO inventory.price_change (tenant_id, id, valuation_id, old_price, new_price, quantity, amount,
			posting_date, created_by) VALUES ($1, $2, $3, 100, 120, 0, 0, '2026-11-20', $1)`, s.tenant, id(), s.stock), "price_change_valuation"},
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
			if err := all(tx, s.valuation(), step(`INSERT INTO inventory.price_change (tenant_id, id, valuation_id, old_price, new_price, quantity, amount,
				posting_date, created_by) VALUES ($1, $2, $3, 100, 150, 0, 0, '2026-11-20', $1)`, s.tenant, id(), s.stock)); err != nil {
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

// TestInventoryKeys (S1d, migrations 000377 and 000378): the account
// determination takes the keys of valuation classes, and the journal the
// source context inventory.
func TestInventoryKeys(t *testing.T) {
	need(t)
	ctx := context.Background()
	for _, c := range []struct{ key, want string }{{"inventory_stock:3000", ""}, {"inventory_revaluation:A1", ""},
		{"inventory_bogus:3000", "account_determination_key_check"}, {"inventory_stock:30000", "account_determination_key_check"}} {
		t.Run(c.key, func(t *testing.T) {
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			// The account's foreign key aside: the key's check is what is tested.
			if _, err := tx.Exec(ctx, "SET LOCAL session_replication_role = replica"); err != nil {
				t.Fatal(err)
			}
			err = exec(tx, `INSERT INTO ledger.account_determination (tenant_id, legal_entity_id, key, account_id, updated_by) VALUES ($1, $1, $2, $1, $1)`, id(), c.key)
			if c.want == "" {
				if err != nil {
					t.Fatalf("got %v, want no error", err)
				}
				return
			}
			failsOn(t, err, c.want)
		})
	}
	var def string
	if err := testDB.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'journal_entry_source_context_check'
		AND conrelid = 'ledger.journal_entry'::regclass`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(def, "'inventory'") {
		t.Errorf("source contexts: got %s, want inventory among them", def)
	}
}
