package db

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestStockItemRule (S1e, migration 000390): a purchase order item names
// a plant exactly when it names a storage location, and an item that does
// is a stock item: it names its material and is assigned to no cost
// center.
func TestStockItemRule(t *testing.T) {
	need(t)
	for _, c := range []struct{ name, set, want string }{
		{"a stock item", `material_id = gen_random_uuid(), plant_id = gen_random_uuid(), storage_location_id = gen_random_uuid()`, ""},
		{"a plant without a storage location", `material_id = gen_random_uuid(), plant_id = gen_random_uuid()`, "purchase_order_item_stock"},
		{"a storage location without a plant", `material_id = gen_random_uuid(), storage_location_id = gen_random_uuid()`, "purchase_order_item_stock"},
		{"a stock item without a material", `material_id = NULL, plant_id = gen_random_uuid(), storage_location_id = gen_random_uuid()`,
			"purchase_order_item_stock"},
		{"a stock item on a cost center", `material_id = gen_random_uuid(), plant_id = gen_random_uuid(), storage_location_id = gen_random_uuid(),
			account_assignment = 'cost_center', cost_center_id = gen_random_uuid()`, "purchase_order_item_stock"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				po, err := f.order(tx, nil)
				if err != nil {
					return err
				}
				return exec(tx, `UPDATE procurement.purchase_order_item SET `+c.set+` WHERE purchase_order_id = $1`, po)
			})
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

// TestGoodsMovementDocument (S1e, migration 000390): a goods movement
// names its material document by id and number together, or neither.
func TestGoodsMovementDocument(t *testing.T) {
	need(t)
	for _, c := range []struct {
		name   string
		doc    any
		number any
		want   string
	}{
		{"both", id(), "MD1", ""},
		{"neither", nil, nil, ""},
		{"the id alone", id(), nil, "goods_movement_material_document"},
		{"the number alone", nil, "MD1", "goods_movement_material_document"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			err := asApp(t, f.tenant, func(tx pgx.Tx) error {
				return exec(tx, `INSERT INTO procurement.goods_movement (tenant_id, id, document_number, legal_entity_id, movement_type, document_date,
					posting_date, posted_by, material_document_id, material_document_number) VALUES ($1, $2, 'GR1', $3, '101', '2026-11-20', '2026-11-20', $4, $5, $6)`,
					f.tenant, id(), f.entity, f.actor, c.doc, c.number)
			})
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

// TestGoodsReceiptDocuments (S1e, migrations 000390 and 000391): a
// material document of a purchase order's goods receipt (101) or its
// reversal (102) names the goods movement's number as its reference, of 1
// to 40 characters, and no other document names one; a 102 reverses a
// 101.
func TestGoodsReceiptDocuments(t *testing.T) {
	need(t)
	const withReference = `INSERT INTO inventory.material_document (tenant_id, id, legal_entity_id, fiscal_year, number, movement_type, posting_date,
		reversal_of_id, reference, created_by) VALUES ($1, $2, $3, 2026, $4, $5, '2026-11-20', $6, $7, $1)`
	for _, c := range []struct {
		name string
		run  func(s inventoryScene) []stmt
		want string
	}{
		{"a goods receipt", func(s inventoryScene) []stmt {
			return []stmt{step(withReference, s.tenant, s.doc, s.company, "MD1", "101", nil, "GR1")}
		}, ""},
		{"its reversal", func(s inventoryScene) []stmt {
			return []stmt{step(withReference, s.tenant, s.doc, s.company, "MD1", "101", nil, "GR1"),
				step(withReference, s.tenant, id(), s.company, "MD2", "102", s.doc, "GR2")}
		}, ""},
		{"a goods receipt without its reference", func(s inventoryScene) []stmt { return []stmt{s.document(s.doc, "MD1", "101", nil)} },
			"material_document_reference"},
		{"another document with one", func(s inventoryScene) []stmt {
			return []stmt{step(withReference, s.tenant, s.doc, s.company, "MD1", "501", nil, "GR1")}
		}, "material_document_reference"},
		{"a reference of 41 characters", func(s inventoryScene) []stmt {
			return []stmt{step(withReference, s.tenant, s.doc, s.company, "MD1", "101", nil, strings.Repeat("G", 41))}
		}, "material_document_reference_check"},
		{"an empty reference", func(s inventoryScene) []stmt {
			return []stmt{step(withReference, s.tenant, s.doc, s.company, "MD1", "101", nil, "")}
		},
			"material_document_reference_check"},
		{"a 102 reversing nothing", func(s inventoryScene) []stmt {
			return []stmt{step(withReference, s.tenant, s.doc, s.company, "MD1", "102", nil, "GR1")}
		}, "material_document_reversal_check"},
		{"a 102 of a 561", func(s inventoryScene) []stmt {
			return []stmt{s.document(s.doc, "MD1", "561", nil), step(withReference, s.tenant, id(), s.company, "MD2", "102", s.doc, "GR2")}
		}, "a reversal is of its original"},
	} {
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

// invoiceDifference inserts an invoice's difference of amount on the
// scene's valuation (s.stock).
func (s inventoryScene) invoiceDifference(amount int64) stmt {
	return step(`INSERT INTO inventory.invoice_difference (tenant_id, id, valuation_id, supplier_invoice_id, purchase_order_id, po_item_no, amount,
		journal_entry_id, journal_entry_year, journal_entry_number, created_by) VALUES ($1, $2, $3, $1, $1, 1, $4, $1, 2026, 'KR1', $1)`,
		s.tenant, id(), s.stock, amount)
}

// valuedReceipt is a 561 of 10 EA worth 1000 into the scene's stock, at
// a moving price (s.valuation).
func (s inventoryScene) valuedReceipt() []stmt {
	return []stmt{s.valuation(), s.document(s.doc, "MD1", "561", nil), s.stockRow(),
		step(`INSERT INTO inventory.movement (tenant_id, material_document_id, line_no, item_no, material_id, plant_id, storage_location_id,
			quantity, entry_quantity, entry_uom_code, amount) VALUES ($1, $2, 1, 1, $3, $4, $5, 10, 10, 'EA', 1000)`,
			s.tenant, s.doc, s.material, s.plant, s.location)}
}

// TestInvoiceDifferenceRules (S1e, migration 000390): an invoice's
// difference is not zero, of a valuation, goes to a moving-priced
// valuation holding stock, whose value it adds to and whose moving price
// follows exactly, never below a value of zero; it is insert-only.
func TestInvoiceDifferenceRules(t *testing.T) {
	need(t)
	for _, c := range []struct {
		name      string
		run       func(s inventoryScene) []stmt
		want      string
		wantPrice int64 // the moving price after, when allowed
	}{
		{"a difference on the stock", func(s inventoryScene) []stmt { return append(s.valuedReceipt(), s.invoiceDifference(55)) }, "", 106},
		{"a difference taking the value to zero", func(s inventoryScene) []stmt { return append(s.valuedReceipt(), s.invoiceDifference(-1000)) }, "", 0},
		{"a difference of nothing", func(s inventoryScene) []stmt { return append(s.valuedReceipt(), s.invoiceDifference(0)) },
			"invoice_difference_amount_check", 0},
		{"of no valuation", func(s inventoryScene) []stmt { return []stmt{s.invoiceDifference(5)} }, "invoice_difference_valuation_fkey", 0},
		{"on no stock", func(s inventoryScene) []stmt { return []stmt{s.valuation(), s.invoiceDifference(5)} },
			"goes to the stock of a moving-priced valuation holding some", 0},
		{"below a value of zero", func(s inventoryScene) []stmt { return append(s.valuedReceipt(), s.invoiceDifference(-1001)) }, "valuation_value_check", 0},
		{"changed", func(s inventoryScene) []stmt {
			return append(s.valuedReceipt(), s.invoiceDifference(5), step(`UPDATE inventory.invoice_difference SET amount = 6`))
		}, "permission denied", 0},
		{"deleted", func(s inventoryScene) []stmt {
			return append(s.valuedReceipt(), s.invoiceDifference(5), step(`DELETE FROM inventory.invoice_difference`))
		}, "permission denied", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := inventoryScene{tenant: id(), company: id(), doc: id(), stock: id(), material: id(), plant: id(), location: id()}
			var price int64
			err := asApp(t, s.tenant, func(tx pgx.Tx) error {
				if err := all(tx, c.run(s)...); err != nil {
					return err
				}
				return tx.QueryRow(context.Background(), `SELECT price FROM inventory.valuation WHERE id = $1`, s.stock).Scan(&price)
			})
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
			if price != c.wantPrice {
				t.Errorf("got moving price %d, want %d", price, c.wantPrice)
			}
		})
	}
}

// TestInvoiceDifferenceAsOwner (S1e, migration 000390): the owner, whom
// grants do not hold, is refused too: an invoice's difference is
// insert-only, and goes to a moving-priced valuation only.
func TestInvoiceDifferenceAsOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	s := inventoryScene{tenant: id(), company: id(), doc: id(), stock: id(), material: id(), plant: id(), location: id()}
	standard := []stmt{step(`INSERT INTO inventory.valuation (tenant_id, id, legal_entity_id, material_id, plant_id, valuation_class, price_control,
		currency, price, price_unit, created_by, updated_by) VALUES ($1, $2, $3, $4, $5, '3000', 'standard', 'EUR', 100, 1, $1, $1)`,
		s.tenant, s.stock, s.company, s.material, s.plant), s.document(s.doc, "MD1", "561", nil), s.stockRow(),
		step(`INSERT INTO inventory.movement (tenant_id, material_document_id, line_no, item_no, material_id, plant_id, storage_location_id,
			quantity, entry_quantity, entry_uom_code, amount) VALUES ($1, $2, 1, 1, $3, $4, $5, 10, 10, 'EA', 1000)`,
			s.tenant, s.doc, s.material, s.plant, s.location)}
	for name, c := range map[string]struct {
		setup []stmt
		st    stmt
		want  string
	}{
		"changed":                        {append(s.valuedReceipt(), s.invoiceDifference(5)), step(`UPDATE inventory.invoice_difference SET amount = 6`), "insert-only"},
		"deleted":                        {append(s.valuedReceipt(), s.invoiceDifference(5)), step(`DELETE FROM inventory.invoice_difference`), "insert-only"},
		"of a standard-priced valuation": {standard, s.invoiceDifference(5), "invoice_difference_valuation"},
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
			if err := all(tx, c.setup...); err != nil {
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
