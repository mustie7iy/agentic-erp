package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// batchScene is inventoryScene with batches L1 and L2 of its material,
// which is batch-managed at its stock row.
type batchScene struct {
	inventoryScene
	l1, l2, s1, s2 uuid.UUID
}

func newBatchScene() batchScene {
	return batchScene{inventoryScene: inventoryScene{tenant: id(), company: id(), doc: id(), stock: id(), material: id(), plant: id(), location: id()},
		l1: id(), l2: id(), s1: id(), s2: id()}
}

func (s batchScene) batch(batch, material uuid.UUID, number, made string, expiry any) stmt {
	return step(`INSERT INTO inventory.batch (tenant_id, id, material_id, number, manufacture_date, expiry_date, created_by)
		VALUES ($1, $2, $3, $4, $5::date, $6::date, $1)`, s.tenant, batch, material, number, made, expiry)
}

func (s batchScene) managedStock(managed bool) stmt {
	return step(`INSERT INTO inventory.stock (tenant_id, id, legal_entity_id, material_id, plant_id, storage_location_id, batch_managed)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, s.tenant, s.stock, s.company, s.material, s.plant, s.location, managed)
}

func (s batchScene) batchStock(row, batch uuid.UUID) stmt {
	return step(`INSERT INTO inventory.batch_stock (tenant_id, id, legal_entity_id, material_id, plant_id, storage_location_id, batch_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, s.tenant, row, s.company, s.material, s.plant, s.location, batch)
}

// batchMovement is a line of the document moving the batch at the
// scene's location; nil: no batch.
func (s batchScene) batchMovement(line int, qty string, batch *uuid.UUID) stmt {
	return step(`INSERT INTO inventory.movement (tenant_id, material_document_id, line_no, item_no, material_id, plant_id, storage_location_id,
			quantity, entry_quantity, entry_uom_code, batch_id) VALUES ($1, $2, $3, 1, $4, $5, $6, $7::numeric, 1, 'EA', $8)`,
		s.tenant, s.doc, line, s.material, s.plant, s.location, qty, batch)
}

// received is a 561 of 10 EA of L1 and 5 EA of L2 into the scene's
// batch-managed stock.
func (s batchScene) received() []stmt {
	return []stmt{s.valuation(), s.document(s.doc, "MD1", "561", nil), s.batch(s.l1, s.material, "L1", "2026-11-01", "2027-05-01"),
		s.batch(s.l2, s.material, "L2", "2026-11-10", nil), s.managedStock(true), s.batchStock(s.s1, s.l1), s.batchStock(s.s2, s.l2),
		s.batchMovement(1, "10", &s.l1), s.batchMovement(2, "5", &s.l2)}
}

// TestBatchRules (S2f, migrations 000428 and 000429; ADR-0082 As built
// S2f): a batch's number is 1 to 10 capitals, digits, '-' or '_', once
// per material, and it expires on or after the day it was made; a batch
// and a batch stock row are insert-only and unchanged by the application;
// a batch stock row is once per batch and location, of a batch of its
// material, written empty and never below zero; a movement at a location
// names a batch of its material exactly when its stock row is
// batch-managed, and adds to the batch's stock row, which must exist; a
// line in transit names none and is of a material not batch-managed; a
// stock row's batch management is not changed by the application.
func TestBatchRules(t *testing.T) {
	need(t)
	cases := []struct {
		name string
		run  func(s batchScene) []stmt
		want string // empty: allowed
	}{
		{"a receipt of two batches", func(s batchScene) []stmt { return s.received() }, ""},
		{"an issue of a batch", func(s batchScene) []stmt { return append(s.received(), s.batchMovement(3, "-5", &s.l2)) }, ""},
		{"more of a batch than it holds", func(s batchScene) []stmt { return append(s.received(), s.batchMovement(3, "-6", &s.l2)) },
			"batch_stock_not_negative"},
		{"a batch-managed movement of no batch", func(s batchScene) []stmt { return append(s.received(), s.batchMovement(3, "1", nil)) }, "movement_batch"},
		{"a batch of a stock not batch-managed", func(s batchScene) []stmt {
			return []stmt{s.valuation(), s.document(s.doc, "MD1", "561", nil), s.batch(s.l1, s.material, "L1", "2026-11-01", nil), s.managedStock(false),
				s.batchStock(s.s1, s.l1), s.batchMovement(1, "10", &s.l1)}
		}, "movement_batch"},
		{"a batch without its batch stock row", func(s batchScene) []stmt {
			return []stmt{s.valuation(), s.document(s.doc, "MD1", "561", nil), s.batch(s.l1, s.material, "L1", "2026-11-01", nil), s.managedStock(true),
				s.batchMovement(1, "10", &s.l1)}
		}, "movement_batch_stock"},
		{"a batch of another material", func(s batchScene) []stmt {
			other := id()
			return append(s.received(), s.batch(other, id(), "L9", "2026-11-01", nil), s.batchMovement(3, "1", &other))
		}, "movement_batch_fkey"},
		{"a batch in transit", func(s batchScene) []stmt {
			return append(s.received(), step(`INSERT INTO inventory.movement (tenant_id, material_document_id, line_no, item_no, material_id, plant_id,
				storage_location_id, quantity, entry_quantity, entry_uom_code, in_transit, batch_id) VALUES ($1, $2, 3, 1, $3, $4, $5, 1, 1, 'EA', true, $6)`,
				s.tenant, s.doc, s.material, s.plant, s.location, s.l1))
		}, "movement_batch_not_in_transit"},
		{"a batch-managed material in transit", func(s batchScene) []stmt {
			return []stmt{s.valuation(), s.document(s.doc, "MD1", "561", nil), step(`INSERT INTO inventory.transit_stock (tenant_id, id, legal_entity_id,
				material_id, plant_id, storage_location_id, batch_managed) VALUES ($1, $2, $3, $4, $5, $6, true)`,
				s.tenant, id(), s.company, s.material, s.plant, s.location),
				step(`INSERT INTO inventory.movement (tenant_id, material_document_id, line_no, item_no, material_id, plant_id, storage_location_id,
				quantity, entry_quantity, entry_uom_code, in_transit) VALUES ($1, $2, 1, 1, $3, $4, $5, 1, 1, 'EA', true)`,
					s.tenant, s.doc, s.material, s.plant, s.location)}
		}, "movement_batch"},
		{"a number twice for the material", func(s batchScene) []stmt {
			return []stmt{s.batch(s.l1, s.material, "L1", "2026-11-01", nil), s.batch(s.l2, s.material, "L1", "2026-11-02", nil)}
		}, "batch_number_once"},
		{"a number again for another material", func(s batchScene) []stmt {
			return []stmt{s.batch(s.l1, s.material, "L1", "2026-11-01", nil), s.batch(s.l2, id(), "L1", "2026-11-02", nil)}
		}, ""},
		{"a number of 11 characters", func(s batchScene) []stmt {
			return []stmt{s.batch(s.l1, s.material, strings.Repeat("L", 11), "2026-11-01", nil)}
		},
			"batch_number_check"},
		{"a number in lower case", func(s batchScene) []stmt { return []stmt{s.batch(s.l1, s.material, "l1", "2026-11-01", nil)} }, "batch_number_check"},
		{"no number", func(s batchScene) []stmt { return []stmt{s.batch(s.l1, s.material, "", "2026-11-01", nil)} }, "batch_number_check"},
		{"an expiry before it was made", func(s batchScene) []stmt { return []stmt{s.batch(s.l1, s.material, "L1", "2026-11-01", "2026-10-31")} },
			"batch_dates"},
		{"an expiry the day it was made", func(s batchScene) []stmt { return []stmt{s.batch(s.l1, s.material, "L1", "2026-11-01", "2026-11-01")} }, ""},
		{"a batch changed", func(s batchScene) []stmt {
			return []stmt{s.batch(s.l1, s.material, "L1", "2026-11-01", nil), step(`UPDATE inventory.batch SET expiry_date = '2027-01-01' WHERE id = $1`, s.l1)}
		}, "permission denied"},
		{"a batch deleted", func(s batchScene) []stmt {
			return []stmt{s.batch(s.l1, s.material, "L1", "2026-11-01", nil), step(`DELETE FROM inventory.batch WHERE id = $1`, s.l1)}
		}, "permission denied"},
		{"a batch stock row with stock", func(s batchScene) []stmt {
			return []stmt{s.batch(s.l1, s.material, "L1", "2026-11-01", nil), step(`INSERT INTO inventory.batch_stock (tenant_id, id, legal_entity_id,
				material_id, plant_id, storage_location_id, batch_id, quantity) VALUES ($1, $2, $3, $4, $5, $6, $7, 1)`,
				s.tenant, s.s1, s.company, s.material, s.plant, s.location, s.l1)}
		}, "stock_from_movements"},
		{"a batch stock row twice", func(s batchScene) []stmt {
			return []stmt{s.batch(s.l1, s.material, "L1", "2026-11-01", nil), s.batchStock(s.s1, s.l1), s.batchStock(s.s2, s.l1)}
		}, "batch_stock_once"},
		{"a batch stock row of another material's batch", func(s batchScene) []stmt {
			return []stmt{s.batch(s.l1, id(), "L1", "2026-11-01", nil), s.batchStock(s.s1, s.l1)}
		}, "batch_stock_batch_fkey"},
		{"a batch stock's quantity set", func(s batchScene) []stmt {
			return append(s.received(), step(`UPDATE inventory.batch_stock SET quantity = 1 WHERE id = $1`, s.s1))
		}, "permission denied"},
		{"a batch stock row deleted", func(s batchScene) []stmt {
			return append(s.received(), step(`DELETE FROM inventory.batch_stock WHERE id = $1`, s.s1))
		}, "permission denied"},
		{"a stock row's batch management changed", func(s batchScene) []stmt {
			return []stmt{s.managedStock(true), step(`UPDATE inventory.stock SET batch_managed = false WHERE id = $1`, s.stock)}
		}, "permission denied"},
		{"a transit row's batch management changed", func(s batchScene) []stmt {
			return []stmt{step(`INSERT INTO inventory.transit_stock (tenant_id, id, legal_entity_id, material_id, plant_id, storage_location_id)
				VALUES ($1, $2, $3, $4, $5, $6)`, s.tenant, s.s1, s.company, s.material, s.plant, s.location),
				step(`UPDATE inventory.transit_stock SET batch_managed = true WHERE id = $1`, s.s1)}
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newBatchScene()
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, c.run(s)...) })
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("got %v, want no error", err)
			case c.want == "":
			case strings.Contains(c.want, " "):
				failsWith(t, err, c.want)
			default:
				failsOn(t, err, c.want)
			}
		})
	}
	t.Run("the batches add up to the location", func(t *testing.T) {
		s := newBatchScene()
		var got string
		err := asApp(t, s.tenant, func(tx pgx.Tx) error {
			if err := all(tx, append(s.received(), s.batchMovement(3, "-4", &s.l1))...); err != nil {
				return err
			}
			return tx.QueryRow(context.Background(), `SELECT (SELECT trim_scale(quantity)::text FROM inventory.stock WHERE id = $1) || ' ' ||
				(SELECT string_agg(trim_scale(quantity)::text, ' ' ORDER BY batch_id = $2 DESC) FROM inventory.batch_stock WHERE material_id = $3)`,
				s.stock, s.l1, s.material).Scan(&got)
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "11 6 5" {
			t.Errorf("the stock and its batches: got %s, want 11 6 5", got)
		}
	})
}

// TestBatchTriggersAsOwner (S2f, migration 000428): past the grants, as
// the owner, a batch is neither changed nor deleted.
func TestBatchTriggersAsOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	for name, set := range map[string]string{
		"a batch changed": `UPDATE inventory.batch SET expiry_date = '2027-01-01' WHERE id = $1`,
		"a batch deleted": `DELETE FROM inventory.batch WHERE id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			s := newBatchScene()
			if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, s.tenant.String()), s.batch(s.l1, s.material, "L1", "2026-11-01", nil)); err != nil {
				t.Fatal(err)
			}
			failsWith(t, exec(tx, set, s.l1), "is insert-only")
		})
	}
}

// TestBatchDownRefused: 000428's down migration refuses to drop batches
// and batch-managed materials (06 §6.4: a down migration loses no data),
// and a role held to row-level security that would see none of them fails
// rather than drops them.
func TestBatchDownRefused(t *testing.T) {
	need(t)
	ctx := context.Background()
	for name, st := range map[string]func(s batchScene) stmt{
		"a batch": func(s batchScene) stmt { return s.batch(s.l1, s.material, "L1", "2026-11-01", nil) },
		"a batch-managed material": func(s batchScene) stmt {
			return step(`INSERT INTO materials.material (tenant_id, id, number, legal_entity_id, description, base_uom_code, material_type, material_group,
				status, batch_managed, created_by, updated_by) VALUES ($1, $2, $3, $1, 'Sealant', 'EA', 'raw_material', 'S', 'active', true, $1, $1)`,
				s.tenant, s.material, "MA-"+s.material.String())
		},
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			s := newBatchScene()
			if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, s.tenant.String()), st(s),
				step(`SELECT set_config('app.tenant_id', '', true)`), step(`SAVEPOINT migrator`)); err != nil {
				t.Fatal(err)
			}
			if err := asMigrator(tx); err != nil {
				t.Fatal(err)
			}
			down := migrationNamed(t, "000428_batches").Down
			failsWith(t, exec(tx, down), "row-level security")
			if err := exec(tx, `ROLLBACK TO SAVEPOINT migrator`); err != nil {
				t.Fatal(err)
			}
			failsWith(t, exec(tx, down), "batches or batch-managed materials exist")
		})
	}
}
