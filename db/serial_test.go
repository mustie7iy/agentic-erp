package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// serialScene is inventoryScene with serial numbers A1 and A2 of its
// material, which is serial-managed at its stock row, and a second
// storage location of the plant.
type serialScene struct {
	inventoryScene
	a1, a2, other, otherStock uuid.UUID
}

func newSerialScene() serialScene {
	return serialScene{inventoryScene: inventoryScene{tenant: id(), company: id(), doc: id(), stock: id(), material: id(), plant: id(), location: id()},
		a1: id(), a2: id(), other: id(), otherStock: id()}
}

func (s serialScene) serial(serial, material uuid.UUID, number string) stmt {
	return step(`INSERT INTO inventory.serial (tenant_id, id, legal_entity_id, material_id, number, created_by) VALUES ($1, $2, $3, $4, $5, $1)`,
		s.tenant, serial, s.company, material, number)
}

func (s serialScene) serialStock(row, location uuid.UUID, managed bool) stmt {
	return step(`INSERT INTO inventory.stock (tenant_id, id, legal_entity_id, material_id, plant_id, storage_location_id, serial_managed)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, s.tenant, row, s.company, s.material, s.plant, location, managed)
}

// moveAt is a line of the scene's document moving qty at a location.
func (s serialScene) moveAt(line int, qty string, location uuid.UUID) stmt {
	return step(`INSERT INTO inventory.movement (tenant_id, material_document_id, line_no, item_no, material_id, plant_id, storage_location_id,
			quantity, entry_quantity, entry_uom_code) VALUES ($1, $2, $3, 1, $4, $5, $6, $7::numeric, 1, 'EA')`,
		s.tenant, s.doc, line, s.material, s.plant, location, qty)
}

// moves is a serial number moved by a line of the scene's document.
func (s serialScene) moves(line int, serial uuid.UUID) stmt {
	return step(`INSERT INTO inventory.movement_serial (tenant_id, material_document_id, line_no, serial_id, material_id) VALUES ($1, $2, $3, $4, $5)`,
		s.tenant, s.doc, line, serial, s.material)
}

// received is a 561 of A1 and A2 into the scene's serial-managed stock.
func (s serialScene) received() []stmt {
	return []stmt{s.valuation(), s.document(s.doc, "MD1", "561", nil), s.serial(s.a1, s.material, "A1"), s.serial(s.a2, s.material, "A2"),
		s.serialStock(s.stock, s.location, true), s.serialStock(s.otherStock, s.other, true), s.moveAt(1, "2", s.location), s.moves(1, s.a1),
		s.moves(1, s.a2)}
}

// TestSerialRules (S2g, migration 000435; ADR-0082 As built S2g): a
// serial number is 1 to 18 capitals, digits, '-' or '_', once per
// material, written out of stock and never deleted; its movements are
// insert-only, of a movement of its material at a location, and move it:
// a receipt only when it is in no stock (so it is in stock at most
// once), an issue only from where it is; a movement of a serial-managed
// stock row names one serial number per unit at commit, and any other
// movement none, also when a serial number is added to a line later; the application changes neither where a serial number
// is nor a stock row's serial number profile.
func TestSerialRules(t *testing.T) {
	need(t)
	cases := []struct {
		name string
		run  func(s serialScene) []stmt
		want string // empty: allowed
	}{
		{"a receipt of two serial numbers", func(s serialScene) []stmt { return s.received() }, ""},
		{"an issue of one", func(s serialScene) []stmt {
			return append(s.received(), s.moveAt(2, "-1", s.location), s.moves(2, s.a2))
		}, ""},
		{"a transfer of one", func(s serialScene) []stmt {
			return append(s.received(), s.moveAt(2, "-1", s.location), s.moves(2, s.a1), s.moveAt(3, "1", s.other), s.moves(3, s.a1))
		}, ""},
		{"a serial number received twice", func(s serialScene) []stmt { return append(s.received(), s.moveAt(2, "1", s.other), s.moves(2, s.a1)) },
			"serial_in_stock_once"},
		{"an issue of one not in stock", func(s serialScene) []stmt {
			return append(s.received(), s.moveAt(2, "-1", s.location), s.moves(2, s.a1), s.moveAt(3, "-1", s.location), s.moves(3, s.a1))
		}, "serial_issued_from_stock"},
		{"an issue from another location", func(s serialScene) []stmt {
			return append(s.received(), s.moveAt(2, "-1", s.location), s.moves(2, s.a1), s.moveAt(3, "1", s.other), s.moves(3, s.a1),
				s.moveAt(4, "-1", s.other), s.moves(4, s.a2))
		}, "serial_issued_from_stock"},
		{"fewer serial numbers than units", func(s serialScene) []stmt {
			return append(s.received(), s.moveAt(2, "-2", s.location), s.moves(2, s.a1))
		},
			"movement_serials"},
		{"a serial-managed movement of none", func(s serialScene) []stmt { return append(s.received(), s.moveAt(2, "-1", s.location)) },
			"movement_serials"},
		{"a serial number of a stock not serial-managed", func(s serialScene) []stmt {
			return []stmt{s.valuation(), s.document(s.doc, "MD1", "561", nil), s.serial(s.a1, s.material, "A1"), s.serialStock(s.stock, s.location, false),
				s.moveAt(1, "1", s.location), s.moves(1, s.a1)}
		}, "movement_serials"},
		{"a serial number of another material", func(s serialScene) []stmt {
			other := id()
			return append(s.received(), s.serial(other, id(), "A9"), s.moveAt(2, "1", s.location),
				step(`INSERT INTO inventory.movement_serial (tenant_id, material_document_id, line_no, serial_id, material_id) VALUES ($1, $2, 2, $3, $4)`,
					s.tenant, s.doc, other, s.material))
		}, "movement_serial_serial_fkey"},
		{"a serial number on another material's movement", func(s serialScene) []stmt {
			other := id()
			return append(s.received(), s.serial(other, other, "A9"), s.moveAt(2, "1", s.location),
				step(`INSERT INTO inventory.movement_serial (tenant_id, material_document_id, line_no, serial_id, material_id) VALUES ($1, $2, 2, $3, $3)`,
					s.tenant, s.doc, other))
		}, "movement_serial_of_movement"},
		{"a serial number in transit", func(s serialScene) []stmt {
			return append(s.received(), step(`INSERT INTO inventory.transit_stock (tenant_id, id, legal_entity_id, material_id, plant_id,
				storage_location_id) VALUES ($1, $2, $3, $4, $5, $6)`, s.tenant, id(), s.company, s.material, s.plant, s.location),
				step(`INSERT INTO inventory.movement (tenant_id, material_document_id, line_no, item_no, material_id, plant_id, storage_location_id,
				quantity, entry_quantity, entry_uom_code, in_transit) VALUES ($1, $2, 2, 1, $3, $4, $5, 1, 1, 'EA', true)`,
					s.tenant, s.doc, s.material, s.plant, s.location), s.moves(2, s.a1))
		}, "movement_serial_of_movement"},
		{"a serial-managed material in transit", func(s serialScene) []stmt {
			return []stmt{s.valuation(), s.document(s.doc, "MD1", "561", nil), step(`INSERT INTO inventory.transit_stock (tenant_id, id, legal_entity_id,
				material_id, plant_id, storage_location_id, serial_managed) VALUES ($1, $2, $3, $4, $5, $6, true)`,
				s.tenant, id(), s.company, s.material, s.plant, s.location),
				step(`INSERT INTO inventory.movement (tenant_id, material_document_id, line_no, item_no, material_id, plant_id, storage_location_id,
				quantity, entry_quantity, entry_uom_code, in_transit) VALUES ($1, $2, 1, 1, $3, $4, $5, 1, 1, 'EA', true)`,
					s.tenant, s.doc, s.material, s.plant, s.location)}
		}, "movement_serials"},
		{"a serial number written in stock", func(s serialScene) []stmt {
			return []stmt{step(`INSERT INTO inventory.serial (tenant_id, id, legal_entity_id, material_id, number, plant_id, storage_location_id, created_by)
				VALUES ($1, $2, $3, $4, 'A1', $5, $6, $1)`, s.tenant, s.a1, s.company, s.material, s.plant, s.location)}
		}, "serial_from_movements"},
		{"a serial number half placed", func(s serialScene) []stmt {
			return []stmt{step(`INSERT INTO inventory.serial (tenant_id, id, legal_entity_id, material_id, number, storage_location_id, created_by)
				VALUES ($1, $2, $3, $4, 'A1', $5, $1)`, s.tenant, s.a1, s.company, s.material, s.location)}
		}, "serial_location"},
		{"a number twice for the material", func(s serialScene) []stmt {
			return []stmt{s.serial(s.a1, s.material, "A1"), s.serial(s.a2, s.material, "A1")}
		}, "serial_number_once"},
		{"a number again for another material", func(s serialScene) []stmt {
			return []stmt{s.serial(s.a1, s.material, "A1"), s.serial(s.a2, id(), "A1")}
		}, ""},
		{"a number of 19 characters", func(s serialScene) []stmt { return []stmt{s.serial(s.a1, s.material, strings.Repeat("A", 19))} },
			"serial_number_check"},
		{"a number in lower case", func(s serialScene) []stmt { return []stmt{s.serial(s.a1, s.material, "a1")} }, "serial_number_check"},
		{"no number", func(s serialScene) []stmt { return []stmt{s.serial(s.a1, s.material, "")} }, "serial_number_check"},
		{"a serial number moved by the application", func(s serialScene) []stmt {
			return append(s.received(), step(`UPDATE inventory.serial SET plant_id = NULL, storage_location_id = NULL WHERE id = $1`, s.a1))
		}, "permission denied"},
		{"a serial number deleted", func(s serialScene) []stmt {
			return []stmt{s.serial(s.a1, s.material, "A1"), step(`DELETE FROM inventory.serial WHERE id = $1`, s.a1)}
		}, "permission denied"},
		{"a serial number's movement deleted", func(s serialScene) []stmt {
			return append(s.received(), step(`DELETE FROM inventory.movement_serial WHERE serial_id = $1`, s.a1))
		}, "permission denied"},
		{"a stock row's serial number profile changed", func(s serialScene) []stmt {
			return []stmt{s.serialStock(s.stock, s.location, true), step(`UPDATE inventory.stock SET serial_managed = false WHERE id = $1`, s.stock)}
		}, "permission denied"},
		{"a transit row's serial number profile changed", func(s serialScene) []stmt {
			return []stmt{step(`INSERT INTO inventory.transit_stock (tenant_id, id, legal_entity_id, material_id, plant_id, storage_location_id)
				VALUES ($1, $2, $3, $4, $5, $6)`, s.tenant, s.stock, s.company, s.material, s.plant, s.location),
				step(`UPDATE inventory.transit_stock SET serial_managed = true WHERE id = $1`, s.stock)}
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newSerialScene()
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
	t.Run("a serial number added to a committed line", func(t *testing.T) {
		s := newSerialScene()
		mustApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, s.received()...) })
		a3 := id()
		failsOn(t, asApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, s.serial(a3, s.material, "A3"), s.moves(1, a3)) }), "movement_serials")
	})
	t.Run("a serial number is where its last movement left it", func(t *testing.T) {
		s := newSerialScene()
		var got string
		err := asApp(t, s.tenant, func(tx pgx.Tx) error {
			if err := all(tx, append(s.received(), s.moveAt(2, "-1", s.location), s.moves(2, s.a1), s.moveAt(3, "1", s.other), s.moves(3, s.a1),
				s.moveAt(4, "-1", s.location), s.moves(4, s.a2))...); err != nil {
				return err
			}
			return tx.QueryRow(context.Background(), `SELECT string_agg(number || ':' || coalesce((storage_location_id = $1)::text, 'none'), ' ' ORDER BY number)
				FROM inventory.serial WHERE material_id = $2`, s.other, s.material).Scan(&got)
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "A1:true A2:none" {
			t.Errorf("the serial numbers' places: got %s, want A1:true A2:none", got)
		}
	})
}

// TestSerialTriggersAsOwner (S2g, migration 000435): past the grants, as
// the owner, a serial number is not deleted and its movements neither
// change nor are deleted.
func TestSerialTriggersAsOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	for name, set := range map[string]string{
		"a serial number deleted":        `DELETE FROM inventory.serial WHERE id = $1`,
		"a serial number's move changed": `UPDATE inventory.movement_serial SET line_no = 2 WHERE serial_id = $1`,
		"a serial number's move deleted": `DELETE FROM inventory.movement_serial WHERE serial_id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			s := newSerialScene()
			if err := all(tx, append([]stmt{step(`SELECT set_config('app.tenant_id', $1, true)`, s.tenant.String())}, s.received()...)...); err != nil {
				t.Fatal(err)
			}
			failsWith(t, exec(tx, set, s.a1), "is insert-only")
		})
	}
}

// TestSerialDownRefused: 000435's down migration refuses to drop serial
// numbers and serial-managed materials (06 §6.4: a down migration loses
// no data).
func TestSerialDownRefused(t *testing.T) {
	need(t)
	ctx := context.Background()
	for name, st := range map[string]func(s serialScene) stmt{
		"a serial number": func(s serialScene) stmt { return s.serial(s.a1, s.material, "A1") },
		"a serial-managed material": func(s serialScene) stmt {
			return step(`INSERT INTO materials.material (tenant_id, id, number, legal_entity_id, description, base_uom_code, material_type, material_group,
				status, serial_managed, created_by, updated_by) VALUES ($1, $2, $3, $1, 'Gauge', 'EA', 'trading_good', 'G', 'active', true, $1, $1)`,
				s.tenant, s.material, "MA-"+s.material.String())
		},
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			s := newSerialScene()
			if err := all(tx, step(`SELECT set_config('app.tenant_id', $1, true)`, s.tenant.String()), st(s)); err != nil {
				t.Fatal(err)
			}
			failsWith(t, exec(tx, migrationNamed(t, "000435_serial_numbers").Down), "serial numbers or serial-managed materials exist")
		})
	}
}
